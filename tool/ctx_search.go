package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	ctxSnippet   = 6  // snippet lines kept per concept hit (the gate's cards)
	ctxBodyLines = 80 // lines of a concept chunk kept for `orch ctx`
	ctxCallLimit = 20 * time.Second
	ctxRgMax     = 2000 // retained visible text excerpts or semantic raw ranks
)

type codeHit struct {
	Path    string   `json:"path"`
	Start   int      `json:"start"`
	End     int      `json:"end"`
	Snippet []string `json:"snippet"`
	Body    []string `json:"body,omitempty"` // the chunk, up to ctxBodyLines lines
}

func jbcontextBin() string {
	if p := os.Getenv("ORCH_JBCONTEXT"); p != "" {
		return p
	}
	return expandHome("~/.jbcontext/bin/jbcontext")
}

// searchCode runs one jbcontext search in dir and returns its hits, from the
// cache under cacheDir when the same search was run before. rev pins it to an
// indexed revision when not empty. Identical searches running at once in this
// process share one jbcontext call, and a rate limited search is retried, then
// skipped for a short cooldown (see jbRun).
func searchCode(bin, dir, cacheDir, rev, pathFilter string, limit int, query string) ([]codeHit, error) {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%d", query, rev, pathFilter, limit))
	cache := filepath.Join(cacheDir, fmt.Sprintf("jb3-%x.json", sum[:8]))
	if b, err := os.ReadFile(cache); err == nil {
		var hits []codeHit
		if json.Unmarshal(b, &hits) == nil && hits != nil && len(hits) < limit {
			return hits, nil
		}
	}
	hits, err := jbFlight.do(bin+"\x00"+dir+"\x00"+cache, func() ([]codeHit, error) {
		return jbSearchOnce(bin, dir, cache, rev, pathFilter, limit, query)
	})
	if hits != nil {
		hits = append([]codeHit{}, hits...) // a copy: callers merge into it
	}
	return hits, err
}

func jbSearchOnce(bin, dir, cache, rev, pathFilter string, limit int, query string) ([]codeHit, error) {
	args := []string{"search", "--limit", fmt.Sprint(limit), "--json-output"}
	if rev != "" {
		args = append(args, "--revision", rev)
	}
	if pathFilter != "" {
		args = append(args, "--path-filter", pathFilter)
	}
	args = append(args, query)
	r, err := jbRun(bin, dir, args, query)
	if err != nil {
		return nil, err
	}
	stdout, runErr, stderr := r.out, r.runErr, r.stderr
	hits, perr := parseJbcontext(stdout.Bytes())
	if r.timedOut {
		return hits, fmt.Errorf("jbcontext timed out after %s", ctxCallLimit)
	}
	if runErr != nil {
		return hits, fmt.Errorf("jbcontext failed: %v: %s", runErr, short(strings.TrimSpace(stderr), 200))
	}
	if stdout.cut {
		return hits, fmt.Errorf("jbcontext output reached the 16 MiB cap; narrow with -p/path= and retry")
	}
	if perr != nil {
		return hits, perr
	}
	// A full ranked page is truncated, not a completed search.
	if len(hits) < limit {
		if hits == nil {
			hits = []codeHit{}
		}
		ctxCacheWrite(cache, hits)
	}
	return hits, nil
}

// ---- rate limit ----

const (
	jbSlots         = 5                // jbcontext searches running at once, machine-wide; ORCH_JB_SLOTS overrides (1 to 32)
	jbSlotsMax      = 32               // largest accepted ORCH_JB_SLOTS
	jbColdAfter     = 10 * time.Minute // no jbcontext answer this long: the token may be stale, so one call goes first
	jbRetries       = 3                // retries after a rate limited answer
	jbBackoff       = 2 * time.Second  // first wait; doubles, plus up to 50% jitter
	jbCooldown      = 20 * time.Second // after retries ran out, skip jbcontext this long
	jbSlotWaitLimit = 3 * time.Minute  // longest wait for a free slot
)

// errJBRateLimited marks a search the JetBrains service refused with HTTP 429.
// Its text is one line and names the query, not the license.
var errJBRateLimited = errors.New("jbcontext rate limited (429)")

func jbRateErr(query string, skipped bool) error {
	how := "retries ran out"
	if skipped {
		how = "skipped during the cooldown"
	}
	return fmt.Errorf("%w: %s, query skipped: %q", errJBRateLimited, how, short(flat(query), 80))
}

func jbRateLimitedOutput(out string) bool {
	return strings.Contains(out, "429") || strings.Contains(strings.ToLower(out), "limit reached")
}

// jbSlotCount is the slot cap: ORCH_JB_SLOTS when it is an integer from 1 to
// jbSlotsMax, else jbSlots.
func jbSlotCount() int {
	if n, err := strconv.Atoi(os.Getenv("ORCH_JB_SLOTS")); err == nil && n >= 1 && n <= jbSlotsMax {
		return n
	}
	return jbSlots
}

func jbBackoffBase() time.Duration {
	if ms, err := strconv.Atoi(os.Getenv("ORCH_JB_BACKOFF_MS")); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return jbBackoff
}

var (
	jbCoolMu    sync.Mutex
	jbCoolUntil = map[string]time.Time{} // jbcontext binary -> no calls before
)

func jbCooling(bin string) bool {
	jbCoolMu.Lock()
	defer jbCoolMu.Unlock()
	return time.Now().Before(jbCoolUntil[bin])
}

type jbResult struct {
	out      *ctxBoundedBuffer
	runErr   error
	timedOut bool
	stderr   string
}

// jbRun runs jbcontext with args under a machine-wide slot, retrying a rate
// limited answer with exponential backoff. The slot is free while it waits. err
// is set only when no process result exists (rate limited, no slot).
func jbRun(bin, dir string, args []string, query string) (jbResult, error) {
	if jbCooling(bin) {
		return jbResult{}, jbRateErr(query, true)
	}
	wait := jbBackoffBase()
	for attempt := 0; ; attempt++ {
		release, serr := jbAcquireSlot()
		if serr != nil {
			return jbResult{}, serr
		}
		ctx, cancel := context.WithTimeout(context.Background(), ctxCallLimit)
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		out := &ctxBoundedBuffer{limit: 16 * 1024 * 1024}
		errBuf := &ctxBoundedBuffer{limit: 64 * 1024}
		cmd.Stdout, cmd.Stderr = out, errBuf
		runErr := cmd.Run()
		timedOut := ctx.Err() != nil
		cancel()
		if runErr == nil && !jbRateLimitedOutput(out.String()+errBuf.String()) {
			jbMarkWarm()
		}
		release()
		if runErr == nil || timedOut || !jbRateLimitedOutput(out.String()+errBuf.String()) {
			return jbResult{out, runErr, timedOut, errBuf.String()}, nil
		}
		if attempt == jbRetries {
			jbCoolMu.Lock()
			jbCoolUntil[bin] = time.Now().Add(jbCooldown)
			jbCoolMu.Unlock()
			return jbResult{}, jbRateErr(query, false)
		}
		time.Sleep(wait + time.Duration(rand.Int63n(int64(wait/2)+1)))
		wait *= 2
	}
}

// jbSlotDir is shared by every orch process of the user. It sits beside the
// per-checkout cache (see ctxCacheDir) so one ORCH_CACHE_DIR moves both.
func jbSlotDir() string {
	root := os.Getenv("ORCH_CACHE_DIR")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".cache", "orchestrate")
	}
	return filepath.Join(root, "jb-slots")
}

// jbWarmStamp is touched after each jbcontext answer that was not rate limited.
func jbWarmStamp() string { return filepath.Join(jbSlotDir(), "warm") }

func jbMarkWarm() {
	p := jbWarmStamp()
	now := time.Now()
	if os.Chtimes(p, now, now) != nil {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Close()
		}
	}
}

// jbCold reports that no jbcontext call answered in the last jbColdAfter.
func jbCold() bool {
	st, err := os.Stat(jbWarmStamp())
	return err != nil || time.Since(st.ModTime()) > jbColdAfter
}

// jbLockFile tries a non-blocking flock on dir/name. The kernel drops a flock
// when its process dies, so a crashed holder never leaves a stale lock.
func jbLockFile(dir, name string) (release func(), ok bool, err error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("jbcontext slots: %w", err)
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, false, nil
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, true, nil
}

// jbAcquireSlot takes one of jbSlotCount flock files. When the stamp is cold it
// first takes warm.lock, so one call runs alone while the others wait; the
// holder releases it after jbMarkWarm, and then every slot is open.
func jbAcquireSlot() (release func(), err error) {
	dir := jbSlotDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("jbcontext slots: %w", err)
	}
	deadline := time.Now().Add(jbSlotWaitLimit)
	pause := func() error {
		if time.Now().After(deadline) {
			return fmt.Errorf("jbcontext: no free search slot after %s", jbSlotWaitLimit)
		}
		time.Sleep(25*time.Millisecond + time.Duration(rand.Int63n(int64(50*time.Millisecond))))
		return nil
	}
	unwarm := func() {}
	for jbCold() {
		rel, ok, err := jbLockFile(dir, "warm.lock")
		if err != nil {
			return nil, err
		}
		if ok {
			if jbCold() {
				unwarm = rel
			} else {
				rel() // another call warmed the stamp while this one waited
			}
			break
		}
		if err := pause(); err != nil {
			return nil, err
		}
	}
	for {
		for i := 0; i < jbSlotCount(); i++ {
			rel, ok, err := jbLockFile(dir, fmt.Sprintf("slot-%d.lock", i))
			if err != nil {
				unwarm()
				return nil, err
			}
			if ok {
				return func() { rel(); unwarm() }, nil
			}
		}
		if err := pause(); err != nil {
			unwarm()
			return nil, err
		}
	}
}

// ---- single flight ----

type jbCall struct {
	done chan struct{}
	hits []codeHit
	err  error
}

type jbGroup struct {
	mu    sync.Mutex
	calls map[string]*jbCall
}

var jbFlight jbGroup

// do runs fn once per key at a time; callers that arrive meanwhile wait for it
// and share its result.
func (g *jbGroup) do(key string, fn func() ([]codeHit, error)) ([]codeHit, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.hits, c.err
	}
	if g.calls == nil {
		g.calls = map[string]*jbCall{}
	}
	c := &jbCall{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()
	c.hits, c.err = fn()
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	close(c.done)
	return c.hits, c.err
}

// parseJbcontext reads `jbcontext search --json-output`: JSON objects of type
// search_result, each with results holding the file, the chunk text and the
// line the chunk starts on.
func parseJbcontext(out []byte) ([]codeHit, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var hits []codeHit
	seen := false
	for {
		var o struct {
			Type    string `json:"type"`
			Results []struct {
				Result struct {
					Position struct {
						Path string `json:"relativePath"`
					} `json:"sourcePosition"`
				} `json:"result"`
				Content   string `json:"content"`
				StartLine int    `json:"contentStartLine"`
			} `json:"results"`
		}
		if err := dec.Decode(&o); err != nil {
			if err == io.EOF && seen {
				break
			}
			return hits, fmt.Errorf("jbcontext output is incomplete or not JSON: %v", err)
		}
		if o.Type != "search_result" {
			if o.Type == "error" {
				return hits, fmt.Errorf("jbcontext returned an error event")
			}
			continue
		}
		seen = true
		if o.Results == nil {
			return hits, fmt.Errorf("jbcontext search_result is missing results")
		}
		for _, r := range o.Results {
			if len(hits) >= ctxRgMax {
				return hits, fmt.Errorf("jbcontext raw-rank cap %d reached; narrow with -p/path=", ctxRgMax)
			}
			if r.Result.Position.Path == "" || r.StartLine < 1 || r.Content == "" {
				return hits, fmt.Errorf("jbcontext result is missing a path, range or content")
			}
			lines := strings.Split(strings.Trim(r.Content, "\n"), "\n")
			start := max(r.StartLine, 1)
			if strings.HasPrefix(r.Content, "\n") {
				start++
			}
			hits = append(hits, codeHit{Path: r.Result.Position.Path, Start: start, End: start + len(lines) - 1, Snippet: lines[:min(len(lines), ctxSnippet)], Body: lines[:min(len(lines), ctxBodyLines)]})
		}
	}
	return hits, nil
}

// indexDetail describes the index a search used, for the sources line.
func (e *ctxEnv) indexDetail() string {
	st := e.Ready()
	if st.head == "" {
		return "index of unknown revision"
	}
	d := "index " + sha7(orDefault(st.indexd, "none")) + " (HEAD " + sha7(st.head)
	if st.rev == "" {
		d += ", stale"
	}
	if st.dirty {
		d += ", uncommitted changes"
	}
	return d + ")"
}

// Concept is a semantic code search with jbcontext. Hits in files the env
// skips (the spec) are left out.
func (e *ctxEnv) Concept(query, pathFilter string, limit int) ([]codeHit, error) {
	if pathFilter != "" && pathFilter != "." {
		p, ok := repoPath(pathFilter)
		if !ok {
			return nil, fmt.Errorf("path %s is outside the repo", pathFilter)
		}
		pathFilter = p
		if e.ReadOnly && !ctxWithinRoot(e.Code, pathFilter) {
			return nil, fmt.Errorf("read-only ctx: path %s is outside the pinned root or missing", pathFilter)
		}
	}
	limit = min(max(limit, 1), ctxRgMax)
	if why := e.jbMissing(); why != "" {
		return nil, fmt.Errorf("%s", why)
	}
	rev := e.jbRev()
	e.indexNote()
	hits, err := searchCode(e.JB, e.Code, e.CacheDir, rev, pathFilter, limit, query)
	if rev != "" && e.Ready().dirty {
		// A pinned search reads only the committed snapshot. An unpinned one
		// also sees this worktree's edits, so run both and let the check below
		// keep what still matches the files.
		more, merr := searchCode(e.JB, e.Code, e.CacheDir, "", pathFilter, limit, query)
		hits = mergeHits(hits, more)
		if merr != nil {
			err = fmt.Errorf("working-tree semantic search failed: %w", merr)
		}
	}
	e.use("jbcontext", e.indexDetail())
	var paths []string
	for _, h := range hits {
		paths = append(paths, h.Path)
	}
	ignored := e.ignoredPaths(paths)
	var out []codeHit
	stale := 0
	seenCurrent := map[string]bool{}
	for _, h := range hits {
		if e.Skip[h.Path] || ignored[h.Path] || !ctxPathInScope(h.Path, pathFilter) {
			continue
		}
		// Always validate the location and read the body from this checkout.
		current, ok := e.checkoutHit(h)
		if !ok {
			stale++
			continue
		}
		key := fmt.Sprintf("%s:%d:%d", current.Path, current.Start, current.End)
		if !seenCurrent[key] {
			seenCurrent[key] = true
			out = append(out, current)
		}
	}
	if files := e.editedFiles(); rev != "" && len(files) > 0 {
		list := files[:min(len(files), 8)]
		more := ""
		if len(files) > len(list) {
			more = fmt.Sprintf(" (+%d more)", len(files)-len(list))
		}
		e.note("edited in this worktree, so jbcontext may not show their current text: " + strings.Join(list, ", ") + more + "; read them with orch ctx text or read")
	}
	if stale > 0 {
		e.note(fmt.Sprintf("jbcontext: %d hits dropped because their file is not in this checkout or no longer reads as the index saw it", stale))
	}
	return out, err
}

// mergeHits adds the hits of more that are not in base. A hit is the same hit
// only with the same file, start and text: the committed and the edited text of
// one place are both kept, and the check against the file keeps the right one.
func mergeHits(base, more []codeHit) []codeHit {
	key := func(h codeHit) string { return fmt.Sprintf("%s:%d:%s", h.Path, h.Start, strings.Join(h.Body, "\n")) }
	seen := map[string]bool{}
	for _, h := range base {
		seen[key(h)] = true
	}
	for _, h := range more {
		if k := key(h); !seen[k] {
			seen[k] = true
			base = append(base, h)
		}
	}
	return base
}

// ctxPathInScope compares path components, not string prefixes (src is not src2).
func ctxPathInScope(path, scope string) bool {
	if scope == "" || scope == "." {
		return true
	}
	p, ok := repoPath(path)
	s, valid := repoPath(scope)
	return ok && valid && (p == s || strings.HasPrefix(p, s+"/"))
}

// checkoutHit validates/relocates the semantic anchor and reads every displayed
// line from this checkout. Ambiguous anchors are refused, never guessed.
func (e *ctxEnv) checkoutHit(h codeHit) (codeHit, bool) {
	if !ctxWithinRoot(e.Code, h.Path) {
		return h, false
	}
	ls := e.Lines(h.Path)
	body := h.Body
	if len(body) == 0 {
		body = h.Snippet
	}
	anchor, offset := "", 0
	for i, line := range body {
		if strings.TrimSpace(line) != "" {
			anchor, offset = strings.TrimSpace(line), i
			break
		}
	}
	if anchor == "" {
		return h, false
	}
	start := h.Start
	if start < 1 || start+offset > len(ls) || strings.TrimSpace(ls[start+offset-1]) != anchor {
		found := 0
		for i, line := range ls {
			if strings.TrimSpace(line) == anchor {
				if found != 0 {
					return h, false
				}
				found = i + 1 - offset
			}
		}
		if found < 1 {
			return h, false
		}
		start = found
	}
	n := h.End - h.Start + 1
	if n < 1 {
		n = len(body)
	}
	end := min(start+n-1, len(ls))
	if end < start {
		return h, false
	}
	h.Start, h.End = start, end
	h.Body = append([]string(nil), ls[start-1:min(end, start+ctxBodyLines-1)]...)
	h.Snippet = append([]string(nil), h.Body[:min(len(h.Body), ctxSnippet)]...)
	return h, true
}

// ctxDefaultExcludes are the globs rg skips unless asked: lockfiles, minified
// and generated files, vendored and built directories.
var ctxDefaultExcludes = []string{
	"!*.lock", "!package-lock.json", "!pnpm-lock.yaml", "!yarn.lock", "!go.sum", "!*.min.js", "!*.min.css", "!*.map",
	"!*.pb.go", "!*_gen.go", "!*.gen.go", "!*.generated.*", "!**/vendor/**", "!**/dist/**", "!**/node_modules/**",
}

// ctxGeneratedExcludes are the name-based excludes that --generated lifts.
var ctxGeneratedExcludes = map[string]bool{"!*.min.js": true, "!*.min.css": true, "!*.map": true, "!*.pb.go": true, "!*_gen.go": true, "!*.gen.go": true, "!*.generated.*": true}

type rgHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type rgCount struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// rgQuery is one text search with ripgrep.
type rgQuery struct {
	Pattern       string
	Fixed         bool // -F: the pattern is a literal string, not a regular expression
	Word          bool // -w
	IgnoreCase    bool // -i (the default is smart-case)
	Multiline     bool // -U
	OnlyMatching  bool // -o
	FilesOnly     bool // -l
	Count         bool // -c
	PerFile       int  // -m, matches per file
	Globs         []string
	Type          string // -t
	Path          string // a file or directory to search
	AllFiles      bool   // do not skip lockfiles, generated and vendored files
	CaseSensitive bool   // -s instead of smart-case
	Generated     string // none (default), include or only: the generated-file mode; include and only lift the name-based generated excludes
	Tests         string // applied before retention with ViewFilters
	ViewFilters   bool   // CLI filters, distinct from symbol/declaration consumers
}

// rgResult is what a search found: match and context lines, or the files, or
// the counts per file.
type rgResult struct {
	Hits     []rgHit   `json:"hits,omitempty"`
	Files    []string  `json:"files,omitempty"`
	Counts   []rgCount `json:"counts,omitempty"`
	Total    int       `json:"total,omitempty"`
	Rows     int       `json:"rows,omitempty"` // matching files in count mode, independent of retained rows
	Hidden   int       `json:"hidden,omitempty"`
	HiddenG  int       `json:"hidden_generated,omitempty"`
	Filtered bool      `json:"filtered,omitempty"`
	Limited  bool      `json:"limited,omitempty"`
	Complete bool      `json:"complete,omitempty"`
}

const rgTextCap = 400 // characters kept of a matching line

// Text searches the checkout with rg, from the cache when the same search ran
// before. Results come sorted by path; files the env skips are left out.
func (e *ctxEnv) Text(q rgQuery) (rgResult, error) {
	if e.ReadOnly && q.Path != "" && !ctxWithinRoot(e.Code, q.Path) {
		return rgResult{}, fmt.Errorf("read-only ctx: path %s is outside the pinned root or missing", q.Path)
	}
	var skip []string
	for s := range e.Skip {
		skip = append(skip, s)
	}
	sort.Strings(skip)
	key := fmt.Sprintf("%+v\x00%s", q, strings.Join(skip, ","))
	sum := sha256.Sum256([]byte(key))
	cache := filepath.Join(e.CacheDir, fmt.Sprintf("rg3-%x.json", sum[:8]))
	e.use("rg", orDefault(which(e.RG), e.RG))
	if b, err := os.ReadFile(cache); err == nil {
		var res rgResult
		if json.Unmarshal(b, &res) == nil && res.Complete && !res.Limited {
			return res, nil
		}
	}
	args := []string{"--sort", "path", "-S"}
	if q.CaseSensitive {
		args[2] = "-s"
	}
	switch {
	case q.FilesOnly:
		args = append(args, "-l")
	case q.Count:
		args = append(args, "-c", "--with-filename")
	default:
		args = append(args, "--json")
	}
	for flag, on := range map[string]bool{"-F": q.Fixed, "-w": q.Word, "-U": q.Multiline} {
		if on {
			args = append(args, flag)
		}
	}
	if q.IgnoreCase {
		args = append(args, "-i")
	}
	if q.PerFile > 0 {
		args = append(args, "-m", fmt.Sprint(q.PerFile))
	}
	if q.Type != "" {
		args = append(args, "-t", q.Type)
	}
	if !q.AllFiles {
		for _, g := range ctxDefaultExcludes {
			if (q.Generated == "include" || q.Generated == "only") && ctxGeneratedExcludes[g] {
				continue
			}
			args = append(args, "-g", g)
		}
	}
	for _, s := range skip {
		args = append(args, "-g", "!"+s)
	}
	for _, g := range q.Globs {
		args = append(args, "-g", g)
	}
	args = append(args, "-e", q.Pattern, "--")
	if q.Path != "" {
		args = append(args, q.Path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ctxCallLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.RG, args...)
	cmd.Dir = e.Code
	stderr := ctxBoundedBuffer{limit: 64 * 1024}
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return rgResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return rgResult{}, fmt.Errorf("rg failed: %w", err)
	}
	res, parseErr := e.streamRG(pipe, q)
	if parseErr != nil {
		cancel()
	}
	err = cmd.Wait()
	if parseErr != nil {
		return res, parseErr
	}
	if ctx.Err() != nil {
		return res, fmt.Errorf("rg timed out after %s", ctxCallLimit)
	}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		// This specific rg diagnostic means the requested file/glob universe
		// is empty, not that a search of existing files failed. Other exit-2
		// diagnostics (bad regex, permissions, missing path) remain failures.
		noFiles := ok && ee.ExitCode() == 2 && strings.HasPrefix(strings.TrimSpace(stderr.String()), "rg: No files were searched,") && res.Total+res.Hidden+res.HiddenG == 0
		if noFiles {
			res.Complete = true
			e.note("rg: no files matched the requested scope and exclusion rules")
		} else if !ok || ee.ExitCode() != 1 {
			return res, fmt.Errorf("rg failed: %v: %s", err, short(strings.TrimSpace(stderr.String()), 160))
		} else {
			if res.Total+res.Hidden+res.HiddenG > 0 {
				return res, fmt.Errorf("rg returned no-match status after emitting matches")
			}
			res.Complete = true // exit 1 is rg's explicit successful no-match result
		}
	}
	if err == nil && !q.FilesOnly && !q.Count && !res.Complete {
		return res, fmt.Errorf("rg incomplete JSON stream: missing summary; narrow and retry")
	}
	res.Complete = true
	if !res.Limited {
		ctxCacheWrite(cache, res)
	}
	return res, nil
}

// Keep dependency diagnostics and semantic JSON bounded even when a broken
// backend ignores its requested limit. Drain discarded bytes to avoid blocking.
type ctxBoundedBuffer struct {
	buf   bytes.Buffer // do not embed: Buffer.ReadFrom would bypass Write's cap
	limit int
	cut   bool
}

func (b *ctxBoundedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *ctxBoundedBuffer) String() string { return b.buf.String() }
func (b *ctxBoundedBuffer) Len() int       { return b.buf.Len() }

func (b *ctxBoundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, max(b.limit-b.Len(), 0))
	if keep < n {
		b.cut = true
	}
	_, _ = b.buf.Write(p[:keep])
	return n, nil
}

// ctxCacheWrite publishes complete entries atomically.
func ctxCacheWrite(path string, value any) {
	b, err := json.Marshal(value)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ctx-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	closeErr := f.Close()
	if err == nil && closeErr == nil {
		_ = os.Rename(f.Name(), path)
	}
}

// streamRG counts independently of bounded excerpts, filtering first.
func (e *ctxEnv) streamRG(r io.Reader, q rgQuery) (rgResult, error) {
	res := rgResult{Filtered: q.ViewFilters}
	retain := func(path string, n int) bool {
		if e.Skip[path] {
			return false
		}
		if q.ViewFilters && !passTests(q.Tests, path) {
			res.Hidden += n
			return false
		}
		if q.ViewFilters && !e.passGen(q.Generated, path) {
			res.HiddenG += n
			return false
		}
		res.Total += n
		return true
	}
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		if q.FilesOnly || q.Count {
			path, n := string(line), 1
			if q.Count {
				i := strings.LastIndex(path, ":")
				if i < 1 {
					return res, fmt.Errorf("rg malformed count record")
				}
				var err error
				n, err = strconv.Atoi(path[i+1:])
				if err != nil || n < 0 {
					return res, fmt.Errorf("rg malformed count record")
				}
				path = path[:i]
			}
			path = filepath.ToSlash(strings.TrimPrefix(path, "./"))
			if !retain(path, n) {
				continue
			}
			res.Rows++
			if q.FilesOnly {
				if len(res.Files) < ctxRgMax {
					res.Files = append(res.Files, path)
				} else {
					res.Limited = true
				}
			} else {
				if len(res.Counts) < ctxRgMax {
					res.Counts = append(res.Counts, rgCount{path, n})
				} else {
					res.Limited = true
				}
			}
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Path  struct{ Text string } `json:"path"`
				Lines struct{ Text string } `json:"lines"`
				Line  int                   `json:"line_number"`
				Subs  []struct {
					Match struct{ Text string } `json:"match"`
				} `json:"submatches"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type == "" {
			return res, fmt.Errorf("rg malformed JSON stream")
		}
		if ev.Type != "match" {
			switch ev.Type {
			case "summary":
				res.Complete = true
				continue
			case "begin", "end", "context":
				continue
			}
			return res, fmt.Errorf("rg unexpected event %q", ev.Type)
		}
		if ev.Data.Path.Text == "" || ev.Data.Line < 1 {
			return res, fmt.Errorf("rg match is missing path or line")
		}
		path := filepath.ToSlash(strings.TrimPrefix(ev.Data.Path.Text, "./"))
		texts := []string{strings.SplitN(strings.TrimRight(ev.Data.Lines.Text, "\r\n"), "\n", 2)[0]}
		if q.OnlyMatching {
			texts = nil
			if len(ev.Data.Subs) == 0 {
				return res, fmt.Errorf("rg match is missing submatches")
			}
			for _, sub := range ev.Data.Subs {
				texts = append(texts, sub.Match.Text)
			}
		} else if q.Multiline {
			texts = strings.Split(strings.TrimRight(ev.Data.Lines.Text, "\r\n"), "\n")
		}
		if !retain(path, len(texts)) {
			continue
		}
		for i, text := range texts {
			if len(res.Hits) >= ctxRgMax {
				res.Limited = true
				break
			}
			n := ev.Data.Line
			if q.Multiline && !q.OnlyMatching {
				n += i
			}
			res.Hits = append(res.Hits, rgHit{path, n, short(strings.TrimSpace(text), rgTextCap)})
		}
	}
	if err := s.Err(); err != nil {
		return res, fmt.Errorf("rg incomplete stream (8 MiB record cap): %w; narrow with --glob or -p", err)
	}
	return res, nil
}

// Lines reads a file of the checkout into lines; nil if it cannot be read.
func (e *ctxEnv) Lines(path string) []string {
	if e.ReadOnly && !ctxWithinRoot(e.Code, path) {
		return nil
	}
	e.mu.Lock()
	ls, ok := e.lineCache[path]
	e.mu.Unlock()
	if ok {
		return ls
	}
	if b, err := os.ReadFile(filepath.Join(e.Code, path)); err == nil {
		ls = strings.Split(string(b), "\n")
	}
	e.mu.Lock()
	if e.lineCache == nil {
		e.lineCache = map[string][]string{}
	}
	e.lineCache[path] = ls
	e.mu.Unlock()
	return ls
}
