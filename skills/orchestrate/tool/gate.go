package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The gate checks a spec before any lead starts. Deterministic checks cover
// what code can prove (pointers, acceptance commands). Cheap read-only model
// passes (pi, default Flash) then probe scope, open questions and design
// flaws. A locator identifies source, not semantic support. ReduceReview
// validates recorded support and authority before accepting a blocker or pass.
const (
	gateParallel   = 6 // model passes at once, so a large spec does not flood the provider
	gateModelDef   = "openrouter/~deepseek/deepseek-flash-latest"
	gateStatePass  = "pass"
	gateKeepDays   = 7  // saved sessions and pass files of older gate runs are pruned
	gateKeepRuns   = 5  // ... and so are those of all but the newest runs
	gateMaxOwns    = 40 // a ship package owning more entries than this, or spanning more than gateMaxOwnDirs directories, gets a size note
	gateMaxOwnDirs = 6
)

// gateTimeout is the wall cap of one model pass (a group of packages can take
// long); gateStall ends a pass whose session shows no activity for that long,
// checked every gatePoll. They are variables so tests can shorten them.
var (
	gateTimeout = 45 * time.Minute
	gateStall   = 6 * time.Minute
	gatePoll    = 15 * time.Second
)

// gateGroupMax packages go to one pass, and the packages' text in one pass
// stays under gateGroupChars (a variable for tests); a package over it alone
// forms its own group.
var (
	gateGroupMax   = 4
	gateGroupChars = 24000
)

// passLimit is a pass that hit the wall cap or stalled; such a group pass is
// retried once.
type passLimit struct{ msg string }

func (e *passLimit) Error() string { return e.msg }

// gateWarmDelay is how long only the first adversary pass and the first
// package pass run, so each prompt prefix is in the provider cache before the
// other workers send theirs. It is a variable so tests can shorten it.
var gateWarmDelay = 45 * time.Second

type gateFinding struct {
	ID         string        `json:"id,omitempty"`
	Accepted   bool          `json:"accepted,omitempty"`
	Assessment *claimSupport `json:"support_assessment,omitempty"`
	Severity   string        `json:"severity"` // blocker or concern
	Where      string        `json:"where"`
	Claim      string        `json:"claim"`
	Evidence   string        `json:"evidence"`
	Why        string        `json:"why,omitempty"`
	Source     string        `json:"source"`
	Verified   bool          `json:"verified"`
	Support    string        `json:"support,omitempty"` // structural ownership is proven by the spec graph
}

type gateQuestion struct {
	Packet     *gateQuestionPacket `json:"packet,omitempty"`
	Package    string              `json:"package"`
	Question   string              `json:"question"`
	AnsweredBy string              `json:"answered_by,omitempty"`
	// Decisive is true when two reasonable answers would give behaviour a
	// user or a test could tell apart. Only decisive questions block.
	Decisive bool `json:"changes_result"`
}

func (q gateQuestion) blocks() bool { return q.Decisive && q.AnsweredBy == "" }

type gateRecord struct {
	historicalGateResult
	Budget            *RunBudget                   `json:"budget,omitempty"`
	SchemaVersion     int                          `json:"schema_version,omitempty"`
	State             string                       `json:"state,omitempty"`
	Completion        reviewCompletion             `json:"completion,omitempty"`
	NextActors        []string                     `json:"next_actors,omitempty"`
	NextActions       []reviewAction               `json:"next_actions,omitempty"`
	Review            *gateReviewRecords           `json:"review,omitempty"`
	Reduction         *reviewResult                `json:"reduction,omitempty"`
	Subject           *gateSubject                 `json:"subject,omitempty"`
	ReviewPolicy      *gateReviewPolicy            `json:"review_policy,omitempty"`
	SubjectStale      []string                     `json:"subject_stale,omitempty"`
	BenchBuild        benchBuild                   `json:"bench_build,omitempty"`
	BenchInputs       benchInputs                  `json:"bench_inputs,omitempty"`
	BenchConfig       benchConfig                  `json:"bench_config,omitempty"`
	Spec              string                       `json:"spec"`
	Hash              string                       `json:"hash"`
	At                int64                        `json:"at"`
	Reasons           []string                     `json:"reasons,omitempty"` // incomplete review or execution failures
	CtxTool           string                       `json:"ctx_tool"`
	ContextFiles      string                       `json:"context_files"`
	CtxProfile        string                       `json:"ctx_profile,omitempty"` // read from old records only
	CtxShell          string                       `json:"ctx_shell,omitempty"`   // effective shell policy: open or ctx-only
	TreeChanged       []string                     `json:"tree_changed,omitempty"`
	TreeChangedPasses []string                     `json:"tree_changed_passes,omitempty"`
	TreeCheckError    string                       `json:"tree_check_error,omitempty"`
	Deterministic     []string                     `json:"deterministic,omitempty"`
	Findings          []gateFinding                `json:"findings,omitempty"`
	Questions         []gateQuestion               `json:"questions,omitempty"`
	Dropped           int                          `json:"dropped_unsupported_claims"`
	PassErrors        map[string]string            `json:"pass_errors,omitempty"`
	PassAnswerStates  map[string]reviewAnswerState `json:"pass_answer_states,omitempty"`
	PassRunners       map[string]string            `json:"pass_runners,omitempty"`
	PassSeconds       map[string]int               `json:"pass_seconds,omitempty"` // wall time per model pass, timeouts included
	Salvaged          []string                     `json:"salvaged,omitempty"`     // passes whose reply was not valid JSON but could be read
	Groups            map[string][]string          `json:"groups,omitempty"`       // package ids covered by each group pass
	PassKept          map[string]string            `json:"pass_kept,omitempty"`    // files kept for a pass that did not run, to read afterwards
	PassTokens        map[string]passTokens        `json:"pass_tokens,omitempty"`  // model usage per pass, when its session recorded any
	Checklist         string                       `json:"checklist,omitempty"`    // plain: the reuse checklist is the spec text only, no candidates
	Cache             string                       `json:"cache,omitempty"`        // short or long: the provider prompt-cache retention asked for
	Items             []gateItemResult             `json:"items,omitempty"`        // one result per item of a package, with its verdict
	Notes             []string                     `json:"notes,omitempty"`
	Experiment        string                       `json:"experiment,omitempty"`
	Only              []string                     `json:"only,omitempty"`
	SpecRel           string                       `json:"spec_rel,omitempty"`
}

type passTokens struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
	// MaxContext is the largest input (fresh plus cached) of any single
	// request; Requests counts the requests that reported usage.
	MaxContext int `json:"max_context"`
	Requests   int `json:"requests"`
	// Cost is the dollars pi reported (usage.cost.total) and CostParts its
	// four parts; NoCost counts requests that reported usage without a cost.
	Cost      float64   `json:"cost_usd"`
	CostParts costParts `json:"cost_parts"`
	NoCost    int       `json:"cost_missing,omitempty"`
	// Turns counts the assistant messages with at least one toolCall (a turn
	// is one reply; several tool calls at once are one turn); BudgetHit is
	// "soft" or "hard" when the pane run had to tell the agent to stop exploring.
	Turns     int    `json:"turns"`
	BudgetHit string `json:"budget_hits,omitempty"`
}

type costParts struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

func (c costParts) add(o costParts) costParts {
	return costParts{c.Input + o.Input, c.Output + o.Output, c.CacheRead + o.CacheRead, c.CacheWrite + o.CacheWrite}
}

// costNote renders a total cost, flagging it when some requests had none.
func costNote(cost float64, missing int) string {
	if missing > 0 {
		return fmt.Sprintf("cost $%.2f (incomplete: %d request(s) without cost)", cost, missing)
	}
	return fmt.Sprintf("cost $%.2f", cost)
}

// sessionTokens sums the usage of the assistant messages in the pi session
// files (jsonl) under dir. It reports false when none recorded any usage.
func sessionTokens(dir string) (passTokens, bool) {
	var t passTokens
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		r := bufio.NewReaderSize(f, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			var m struct {
				Message struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
					Usage   struct {
						Input      int `json:"input"`
						Output     int `json:"output"`
						CacheRead  int `json:"cacheRead"`
						CacheWrite int `json:"cacheWrite"`
						Cost       *struct {
							Input      float64 `json:"input"`
							Output     float64 `json:"output"`
							CacheRead  float64 `json:"cacheRead"`
							CacheWrite float64 `json:"cacheWrite"`
							Total      float64 `json:"total"`
						} `json:"cost"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &m) == nil && m.Message.Role == "assistant" {
				u := m.Message.Usage
				var parts []struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(m.Message.Content, &parts) == nil {
					for _, p := range parts {
						if p.Type == "toolCall" {
							t.Turns++
							break
						}
					}
				}
				t.Input, t.Output, t.CacheRead, t.CacheWrite = t.Input+u.Input, t.Output+u.Output, t.CacheRead+u.CacheRead, t.CacheWrite+u.CacheWrite
				if u.Input+u.Output+u.CacheRead+u.CacheWrite > 0 {
					t.Requests++
					if u.Cost == nil {
						t.NoCost++
					}
				}
				if c := u.Cost; c != nil {
					t.Cost += c.Total
					t.CostParts = t.CostParts.add(costParts{c.Input, c.Output, c.CacheRead, c.CacheWrite})
				}
				t.MaxContext = max(t.MaxContext, u.Input+u.CacheRead)
			}
			if err != nil {
				return nil
			}
		}
	})
	return t, t != (passTokens{})
}

// totals sums the recorded passes. widest names the pass with the largest
// context, ties going to the smaller name; hits counts budget hits by kind.
func (r *gateRecord) totals() (t passTokens, widest string, hits map[string]int) {
	hits = map[string]int{}
	for name, p := range r.PassTokens {
		t.Input, t.Output, t.CacheRead, t.CacheWrite = t.Input+p.Input, t.Output+p.Output, t.CacheRead+p.CacheRead, t.CacheWrite+p.CacheWrite
		t.Requests += p.Requests
		t.Cost += p.Cost
		t.CostParts = t.CostParts.add(p.CostParts)
		t.NoCost += p.NoCost
		t.Turns += p.Turns
		if p.BudgetHit != "" {
			hits[p.BudgetHit]++
		}
		if p.MaxContext > t.MaxContext || (p.MaxContext == t.MaxContext && name < widest) {
			t.MaxContext, widest = p.MaxContext, name
		}
	}
	return t, widest, hits
}

// tokenSummary totals the recorded passes; the cache share is cache reads
// over all input read (fresh plus cached). It is empty when nothing was recorded.
func (r *gateRecord) tokenSummary() string {
	if len(r.PassTokens) == 0 {
		return ""
	}
	t, widest, hits := r.totals()
	share := 0
	if all := t.Input + t.CacheRead; all > 0 {
		share = t.CacheRead * 100 / all
	}
	out := fmt.Sprintf("input %d, cache read %d (%d%% of input), cache write %d, output %d over %d pass(es), %d requests, max context %d in %s, %s",
		t.Input, t.CacheRead, share, t.CacheWrite, t.Output, len(r.PassTokens), t.Requests, t.MaxContext, widest, costNote(t.Cost, t.NoCost))
	if t.Turns > 0 || len(hits) > 0 {
		out += fmt.Sprintf(", %d turns, budget hits: soft %d, hard %d", t.Turns, hits["soft"], hits["hard"])
	}
	return out
}

func specHash(src []byte) string {
	sum := sha256.Sum256(src)
	return fmt.Sprintf("%x", sum[:8])
}

func gateDir(root, hash string) string { return filepath.Join(root, "gate", hash) }

// gatePassed reports whether approval is bound to this exact current subject.
// The short spec hash is a storage locator only, never approval identity.
func gatePassed(root, code string, src []byte) bool {
	passed, _ := gateApproval(root, code, src)
	return passed
}

var (
	pointerRe  = regexp.MustCompile(`([A-Za-z0-9_./-]+\.[A-Za-z0-9]+)(?::([0-9]+)(?:-([0-9]+))?|#([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*))`)
	backtickRe = regexp.MustCompile("`([^`\n]+)`")
	srcExtRe   = regexp.MustCompile(`\.(go|ts|tsx|js|jsx|py|rs|php|rb|java|kt|swift|c|h|cpp|cs|sql|sh|md|toml|json|ya?ml|html|css|vue|svelte|tmpl)$`)
)

// evidenceHolds checks a line range or symbol pointer in the repo.
func evidenceHolds(repo, ev string) bool {
	m := pointerRe.FindStringSubmatch(strings.TrimSpace(ev))
	if m == nil {
		return false
	}
	_, err := checkPointer(repo, m[0])
	return err == nil
}

// specFiles lists the repo files the spec points at, in first-seen order.
func specFiles(repo, src string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range pointerRe.FindAllStringSubmatch(src, -1) {
		path := m[1]
		if seen[path] || strings.Contains(path, "://") || !evidenceHolds(repo, m[0]) {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// adversaryScope closes an adversary pass to the files the spec names, so it
// has a finish line instead of open-ended exploration.
func adversaryScope(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return " Scope: start from these files, which the spec names: " + strings.Join(files, ", ") +
		". Open other files only when one of these imports or calls them directly. Stop once you have checked these; an empty findings list is a valid answer."
}

var commandWordRe = regexp.MustCompile(`^(\./)?[a-z0-9][a-z0-9._/-]*$`)

// acceptanceCommand returns the program a backticked span invokes, or false
// when the span is prose: a file name, a header, a permission key, an env var
// or any single word that is not a ./script. Acceptance text quotes all of
// these, and only an invocation (a program plus arguments) can be checked.
func acceptanceCommand(span string) (string, bool) {
	f := strings.Fields(span)
	if len(f) == 0 || !commandWordRe.MatchString(f[0]) {
		return "", false
	}
	if srcExtRe.MatchString(f[0]) && !strings.HasPrefix(f[0], "./") {
		return "", false
	}
	if len(f) == 1 && !strings.HasPrefix(f[0], "./") {
		return "", false
	}
	return f[0], true
}

// ownDirs counts the distinct parent directories of the owns entries; an entry
// ending in "/" is a directory and counts as its own parent.
func ownDirs(owns []string) int {
	dirs := map[string]bool{}
	for _, o := range owns {
		if before, ok := strings.CutSuffix(o, "/"); ok {
			dirs[before] = true
		} else {
			dirs[path.Dir(o)] = true
		}
	}
	return len(dirs)
}

// gateDeterministic is every check that needs no model.
func gateDeterministic(repo string, cfg config, sp *spec, src string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range pointerRe.FindAllStringSubmatch(src, -1) {
		path := m[1]
		if seen[m[0]] || !(strings.Contains(path, "/") || srcExtRe.MatchString(path)) || strings.Contains(path, "://") {
			continue
		}
		seen[m[0]] = true
		note, err := checkPointer(repo, m[0])
		if err != nil {
			out = append(out, err.Error())
		} else if note != "" {
			out = append(out, "note: "+note)
		}
	}
	for _, p := range sp.packages {
		for _, m := range backtickRe.FindAllStringSubmatch(p.acceptance, -1) {
			cmd, ok := acceptanceCommand(m[1])
			if !ok {
				continue
			}
			if strings.Contains(cmd, "/") {
				if _, err := os.Stat(filepath.Join(repo, cmd)); err != nil {
					out = append(out, fmt.Sprintf("package %s: acceptance command %q is not in the repo", p.id, cmd))
				}
			} else if _, err := exec.LookPath(cmd); err != nil {
				out = append(out, fmt.Sprintf("package %s: acceptance command %q is not on PATH", p.id, cmd))
			}
		}
	}
	for _, p := range sp.packages {
		if dirs := ownDirs(p.owns); p.kind == "ship" && (len(p.owns) > gateMaxOwns || dirs > gateMaxOwnDirs) {
			out = append(out, fmt.Sprintf("note: package %s owns %d paths across %d directories; consider splitting it, a lead holds all of it in one context", p.id, len(p.owns), dirs))
		}
	}
	for _, p := range sp.packages {
		if p.kind == "ship" && len(p.builds) == 0 {
			out = append(out, fmt.Sprintf("note: package %s has no Builds bullets; reuse checklist is skipped for this package", p.id))
		}
		if len(p.builds) > buildsMaxBullets {
			out = append(out, fmt.Sprintf("note: package %s has %d Builds bullets (more than %d); split the package", p.id, len(p.builds), buildsMaxBullets))
		}
		for n, b := range p.builds {
			for _, ptr := range b.Reuse {
				if seen[ptr] {
					continue
				}
				seen[ptr] = true
				note, err := checkPointer(repo, ptr)
				if err != nil {
					label := fmt.Sprintf("Builds bullet %d", n+1)
					if b.ID != "" {
						label += " (" + itemKey(p.id, b.ID) + ")"
					}
					out = append(out, fmt.Sprintf("package %s: %s: %v", p.id, label, err))
				} else if note != "" {
					out = append(out, "note: "+note)
				}
			}
		}
	}
	if cfg.Check == "" {
		out = append(out, "no `check` command in the repo config, so `orch verify` cannot pass any lead's work")
	}
	return out
}

const gateHeader = `You are a read-only verifier for a software spec. The repository is the current directory. Never edit anything. Open files with your tools before you claim anything about them. Cite evidence as "path:line" for a line you actually read. A claim without real evidence is discarded. Reply with ONLY one JSON object and no other text.

`

// gatePromptParts splits a prompt in what every pass of one class shares and
// the pass's own part: its own text (if any), tag, task and schema come last,
// so the provider can cache the common prefix. Pane passes send the two parts
// as separate files, headless passes the two joined.
func gatePromptParts(shared, own, tag, task, schema string) (string, string) {
	if own != "" {
		own += "\n\n"
	}
	return gateHeader + shared + "\n", own + tag + "\n" + task + "\nJSON schema: " + schema
}

func gatePrompt(shared, own, tag, task, schema string) string {
	a, b := gatePromptParts(shared, own, tag, task, schema)
	return a + b
}

func newGatePass(name, class, shared, own, tag, task, schema string, apply func(string, *gateRecord, string, *spec) error) gatePass {
	sh, tk := gatePromptParts(shared, own, tag, task, schema)
	return gateEvidenceContract(gatePass{name: name, class: class, shared: sh, task: tk, prompt: sh + tk, brief: short(task, 100), apply: apply})
}

// specLines splits a spec the way the parser does, so a line index here is
// the spec file's line number minus one.
func specLines(src string) []string {
	return strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
}

// numbered prints lines[from:to] with their spec file line numbers, so a model
// can cite one as "<spec path>:<n>" and the evidence check can verify it.
func numbered(lines []string, from, to int) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i+1, lines[i])
	}
	return b.String()
}

// packagesSpan is the line range [from, to) of the `## Packages` section,
// heading included; fenced headings do not count.
func packagesSpan(lines []string) (int, int) {
	from, fenced := -1, false
	for i, l := range lines {
		switch {
		case strings.HasPrefix(strings.TrimSpace(l), "```"):
			fenced = !fenced
		case !fenced && strings.HasPrefix(l, "## "):
			if from >= 0 {
				return from, i
			}
			if strings.TrimSpace(l[3:]) == "Packages" {
				from = i
			}
		}
	}
	if from < 0 {
		return 0, 0
	}
	return from, len(lines)
}

// specIntro tells the model how to cite the numbered spec; rel is the spec's
// repo-relative path, empty when no line of it could be cited.
func specIntro(rel string) string {
	if rel == "" {
		return "SPEC (line numbers are not citable):\n"
	}
	return "SPEC, file " + rel + ", every line prefixed with its line number. Cite a spec line as \"" + rel + ":<line>\".\n"
}

// gateSpecContext is the prefix both adversary passes share: the whole spec.
func gateSpecContext(src, rel string) string {
	lines := specLines(src)
	return specIntro(rel) + numbered(lines, 0, len(lines))
}

// gatePackageShared is the prefix every package pass shares: the spec without
// its package bodies (every other section stays verbatim, numbered), then one
// index line per package.
func gatePackageShared(sp *spec, src, rel string) string {
	lines := specLines(src)
	from, to := packagesSpan(lines)
	var b strings.Builder
	b.WriteString(specIntro(rel))
	b.WriteString(numbered(lines, 0, from))
	if to > from {
		b.WriteString("(the package bodies are left out here; the package asked about follows in full)\n")
		b.WriteString(numbered(lines, to, len(lines)))
	}
	b.WriteString("\nPACKAGE INDEX:\n")
	for _, p := range sp.packages {
		b.WriteString("- ")
		b.WriteString(p.id)
		b.WriteString(": ")
		b.WriteString(p.title)
		b.WriteString(" (owns: ")
		b.WriteString(strings.Join(p.owns, ", "))
		b.WriteString(")\n")
	}
	return b.String()
}

// packageBlocks is each package's full text with the spec's real line
// numbers, found by walking the Packages section in order.
func packageBlocks(sp *spec, src string) map[string]string {
	lines := specLines(src)
	pos, to := packagesSpan(lines)
	out := map[string]string{}
	for _, p := range sp.packages {
		first, _, _ := strings.Cut(p.raw, "\n")
		n := strings.Count(p.raw, "\n") + 1
		out[p.id] = p.raw
		for i := pos; i < to; i++ {
			if strings.TrimRight(lines[i], " \t") == strings.TrimRight(first, " \t") && i+n <= len(lines) {
				out[p.id] = strings.TrimRight(numbered(lines, i, i+n), "\n")
				pos = i + n
				break
			}
		}
	}
	return out
}

// pkgAffinity scores how related two packages are: shared owns directories at
// depth 3 or more (deeper counts more), a shared id family (leading `-`
// segments beyond the first, as in crm1-ui-shell and crm1-ui-money), a depends
// edge either way, and the owns tree: web/ against internal/ is the strongest
// signal, for the same tree or against a mix.
func pkgAffinity(p, q *specPackage) int {
	score := 0
	deepest := 0
	for _, a := range p.owns {
		for _, b := range q.owns {
			da, db := strings.Split(ownDir(a), "/"), strings.Split(ownDir(b), "/")
			c := 0
			for c < len(da) && c < len(db) && da[c] == db[c] {
				c++
			}
			deepest = max(deepest, c)
		}
	}
	if deepest >= 3 {
		score += 3 * (deepest - 2)
	}
	ia, ib := strings.Split(p.id, "-"), strings.Split(q.id, "-")
	k := 0
	for k < len(ia) && k < len(ib) && ia[k] == ib[k] {
		k++
	}
	if k >= 2 {
		score += 6 * (k - 1)
	}
	for _, d := range p.deps {
		if d == q.id {
			score += 4
		}
	}
	for _, d := range q.deps {
		if d == p.id {
			score += 4
		}
	}
	switch wa, wb := ownTree(p.owns), ownTree(q.owns); {
	case wa == wb && wa != "":
		score += 2
	case wa != "" && wb != "":
		score -= 8
	}
	return score
}

// ownDir is the directory of an owns entry: the entry itself when it ends in
// "/", else its parent.
func ownDir(o string) string {
	o = strings.TrimLeft(strings.TrimPrefix(o, "./"), "/")
	if before, ok := strings.CutSuffix(o, "/"); ok {
		return before
	}
	return path.Dir(o)
}

// ownTree is "web" or "internal" when the package owns only under that tree
// and "" otherwise (both, neither or nothing).
func ownTree(owns []string) string {
	web, internal := false, false
	for _, o := range owns {
		switch top, _, _ := strings.Cut(ownDir(o), "/"); top {
		case "web":
			web = true
		case "internal":
			internal = true
		}
	}
	switch {
	case web && !internal:
		return "web"
	case internal && !web:
		return "internal"
	}
	return ""
}

// gateGroups packs the packages into groups of at most maxPkgs packages and
// maxChars of package text (a package over maxChars forms its own group). It
// merges agglomeratively: the pair of groups with the highest average
// affinity that still fits goes first, ties to the earliest in spec order. It
// stops when no pair fits, or when there are at most want groups and no pair
// is related (positive affinity). Groups and the packages in them are in
// spec order.
func gateGroups(sp *spec, maxPkgs, maxChars, want int) [][]*specPackage {
	type group struct {
		idx   []int // spec positions, ascending
		chars int
	}
	pkgs := sp.packages
	groups := make([]*group, len(pkgs))
	aff := make([][]int, len(pkgs))
	for i, p := range pkgs {
		groups[i] = &group{idx: []int{i}, chars: len(p.raw)}
		aff[i] = make([]int, len(pkgs))
	}
	for i := range pkgs {
		for j := i + 1; j < len(pkgs); j++ {
			aff[i][j] = pkgAffinity(pkgs[i], pkgs[j])
		}
	}
	avg := func(a, b *group) float64 {
		sum := 0
		for _, i := range a.idx {
			for _, j := range b.idx {
				sum += aff[min(i, j)][max(i, j)]
			}
		}
		return float64(sum) / float64(len(a.idx)*len(b.idx))
	}
	for {
		bi, bj, best := -1, -1, 0.0
		for i := range groups {
			for j := i + 1; j < len(groups); j++ {
				if len(groups[i].idx)+len(groups[j].idx) > maxPkgs || groups[i].chars+groups[j].chars > maxChars {
					continue
				}
				if s := avg(groups[i], groups[j]); bi < 0 || s > best {
					bi, bj, best = i, j, s
				}
			}
		}
		if bi < 0 || (len(groups) <= want && best <= 0) {
			break
		}
		g := groups[bi]
		g.idx = append(g.idx, groups[bj].idx...)
		sort.Ints(g.idx)
		g.chars += groups[bj].chars
		groups = append(groups[:bj], groups[bj+1:]...)
	}
	sort.Slice(groups, func(a, b int) bool { return groups[a].idx[0] < groups[b].idx[0] })
	out := make([][]*specPackage, len(groups))
	for i, g := range groups {
		for _, k := range g.idx {
			out[i] = append(out[i], pkgs[k])
		}
	}
	return out
}

// groupKey names a group pass: the first package id and how many follow it.
func groupKey(g []*specPackage) string {
	if len(g) == 1 {
		return "group:" + g[0].id
	}
	return fmt.Sprintf("group:%s+%d", g[0].id, len(g)-1)
}

func gatePackageOwn(block string) string {
	return "PACKAGES UNDER REVIEW, full text:\n" + block
}

// runPi runs one headless read-only pi pass and returns its text. The session
// is saved under sessionDir so a failed pass can be read afterwards. A pass
// whose session files show no change for gateStall is cancelled.
func runPi(repo, model string, p gatePass, sessionDir string) (string, error) {
	var err error
	p, err = gateAgentPass(repo, "pi", p)
	if err != nil {
		return "", err
	}
	defer os.Remove(p.cli.Ready)
	_ = os.MkdirAll(sessionDir, 0o755)
	ctx, cancel := context.WithTimeout(passContext(p), gateTimeout)
	defer cancel()
	stalled := make(chan struct{})
	poll, stall := gatePoll, gateStall
	go func() {
		last, changed := sessionStamp(sessionDir), time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(poll):
			}
			if now := sessionStamp(sessionDir); now != last {
				last, changed = now, time.Now()
			} else if time.Since(changed) >= stall {
				close(stalled)
				cancel()
				return
			}
		}
	}()
	args := []string{"-p", "--session-dir", sessionDir, "--no-extensions"}
	args = append(args, piGuardArgs(p)...)
	args = append(args, "--no-skills", "--no-context-files")
	args = append(args, piPassTools(p)...)
	if p.effort != "" {
		args = append(args, "--thinking", p.effort)
	}
	cmd := processCommandContext(ctx, envOr("ORCH_PI", "pi"), append(args, "--model", model, p.prompt)...)
	defer cmd.Cancel()
	cmd.Dir = repo
	cmd.Env = agentEnv(nil, p.cli.Env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	select {
	case <-stalled:
		return "", &passLimit{"stalled: no activity for " + stall.String()}
	default:
	}
	if passContext(p).Err() != nil {
		return "", passContext(p).Err()
	}
	if ctx.Err() != nil {
		return "", &passLimit{fmt.Sprintf("timed out after %s", gateTimeout)}
	}
	if err != nil {
		return "", fmt.Errorf("pi failed: %w: %s", err, short(strings.TrimSpace(stderr.String()), 200))
	}
	if err := ctxOnlyReady(p); err != nil {
		return "", err
	}
	if err := sessionFailure(sessionDir); err != nil {
		return "", err
	}
	return string(out), nil
}

// sessionStamp changes whenever a session file under dir is written: the
// newest modification time and the total size.
func sessionStamp(dir string) string {
	var newest time.Time
	var size int64
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			size += info.Size()
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		return nil
	})
	return fmt.Sprintf("%d:%d", newest.UnixNano(), size)
}

// extractJSON decodes the first JSON object in text into v.
func extractJSON(text string, v any) error {
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j <= i {
		return fmt.Errorf("no JSON object in the reply")
	}
	return json.Unmarshal([]byte(text[i:j+1]), v)
}

// gatePass is one model pass. Passes of one class share the text in shared.
type gatePass struct {
	name, class  string
	shared, task string // the prompt in two parts; a pane pass sends them as two files
	prompt       string // shared + task, for a headless pass
	brief        string // the start of the task instruction, for --dry-run
	apply        func(raw string, rec *gateRecord, repo string, sp *spec) error
	effort       string // runner-specific thinking effort
	noTools      bool   // the pass gets no tools at all
	ctxTool      bool
	ctxOnly      bool // pi: read tool plus a shell limited to orch ctx
	cli          CtxCLI
	ctx          context.Context
	sessionDir   string           // invocation-local session; retries retain earlier usage
	admission    *budgetAdmission // shared run ticket for pane continuation requests
	budget       int              // soft tool-call budget; 0 means none
}

// Turn budget of a pass: gateTurnsBase plus gateTurnsPerPackage for each
// package of a group pass (adversaries get gateTurnsAdversary). A pane pass
// that goes over it is told once to answer; at gateBudgetHard times it, or
// when the context of the newest request reaches gateContextSoft tokens
// (treated as the soft limit), it is interrupted and asked for the answer.
var (
	gateTurnsBase       = 10
	gateTurnsPerPackage = 5
	gateTurnsAdversary  = 40
	gateBudgetHard      = 1.4
	gateContextSoft     = 150000
)

// budgetSentenceItems is the budget of a group pass that must decide every
// item of a checklist: it ends in "answer every item" instead of leaving the
// stop to the model.
func budgetSentenceItems(n int, ctxOn ...bool) string {
	if len(ctxOn) > 0 && ctxOn[0] {
		return budgetSentenceItems(n) + " " + ctxBudgetHint
	}
	return fmt.Sprintf(" Budget: about %d turns. A turn is one reply from you; put every independent lookup in the same reply (several tool calls at once are one turn). Never re-read the spec or a file you already read; answer every item.", n)
}

func budgetSentence(n int, ctxOn ...bool) string {
	if len(ctxOn) > 0 && ctxOn[0] {
		return budgetSentence(n) + " " + ctxBudgetHint
	}
	return fmt.Sprintf(" Budget: about %d turns. A turn is one reply from you; put every independent lookup in the same reply (several tool calls at once are one turn). Never re-read the spec or a file you already read; answer as soon as you have enough.", n)
}

// buildGatePasses makes the passes in the order they are queued: the two
// first ones warm the cache for the two prefix classes. groups gets the
// package ids of each group pass.
func buildGatePasses(code, src, rel string, sp *spec, groups map[string][]string, only map[string]bool) []gatePass {
	scope := adversaryScope(specFiles(code, src))
	specCtx := gateSpecContext(src, rel)
	supportInstructions := ` For each semantic blocker provide support_assessment with schema_version 1, state supported|unsupported|conflicting|unknown, rationale, an exact source excerpt, and a concrete failure_scenario. A locator or confidence alone is not support. Use unsupported when the behavior cannot be established; do not label model interpretations structural.`
	advA := newGatePass("adversary-a", "adversary", specCtx, "", "GATE-ADVERSARY-A", `Attack the Design rules and the Authority section. Find a rule that the code contradicts, a failure case no package covers, or an unsafe default. Report only what you can support.`+supportInstructions+scope+budgetSentence(gateTurnsAdversary),
		`{"findings":[{"severity":"blocker|concern","where":"","claim":"","evidence":"path:line","why":"","support_assessment":{"schema_version":1,"state":"supported","rationale":"","excerpt":"","failure_scenario":""}}]}`, applyFindings("adversary-a"))
	advA.budget = gateTurnsAdversary
	advB := newGatePass("adversary-b", "adversary", specCtx, "", "GATE-ADVERSARY-B", `Assume the packages are built exactly as written and the acceptance commands pass. What still breaks for a user, or between two packages? Gaps in a contract that two packages share (one produces, the other consumes) are your job, and so is two packages of this spec building the same thing (duplicate producers, two implementations of one contract). Report only what you can support.`+supportInstructions+scope+budgetSentence(gateTurnsAdversary),
		`{"findings":[{"severity":"blocker|concern","where":"","claim":"","evidence":"path:line","why":"","support_assessment":{"schema_version":1,"state":"supported","rationale":"","excerpt":"","failure_scenario":""}}]}`, applyFindings("adversary-b"))
	advB.budget = gateTurnsAdversary
	// Scope and questions share one pass per group of related packages: it
	// sees the shared sections and the package index but only its own
	// packages in full, so its work stays bounded as the spec grows.
	fromSpec := ""
	if rel != "" {
		fromSpec = " A question that the spec itself already answers (Design rules, Decisions, Behaviour, or any package text) is answered: set answered_by to that spec line as `" + rel + ":<line>`."
	}
	pkgShared := gatePackageShared(sp, src, rel)
	blocks := packageBlocks(sp, src)
	var pkgPasses []gatePass
	for _, g := range gateGroups(sp, gateGroupMax, gateGroupChars, gateParallel) {
		if only != nil && !only[groupKey(g)] {
			continue
		}
		var ids, texts []string
		var gitems []gateItem
		for _, pkg := range g {
			ids = append(ids, pkg.id)
			texts = append(texts, blocks[pkg.id])
			gitems = append(gitems, packageItems(pkg)...)
		}
		key := groupKey(g)
		groups[key] = ids
		joined := strings.Join(texts, "\n\n")
		own := gatePackageOwn(joined) + plainChecklist(gitems)
		reuseTask := ` (3) reuse: for each package, list the things it says it will newly build (files, functions, tables, endpoints, templates, jobs) for which the repo already has something that does the same job. Search with grep and find before you claim it, and cite the existing code as path:line. An empty list is a valid answer. Do not list things the package is told to modify or extend.`
		schema := `{"packages":[{"id":"","will_change":["path"],"claims":[{"claim":"","evidence":"path:line"}],"questions":[{"question":"","answered_by":"","changes_result":false}],"reuse":[{"builds":"","existing":"","evidence":"path:line","why":""}]}]}`
		p := newGatePass(key, "package", pkgShared, own, "GATE-GROUP",
			`Do three things for EACH package under review. (1) scope: say which files this package's work will really have to change, found by reading the code, and give claims about the code with evidence. (2) questions: list the questions a developer would have to answer before building this package. Mark a question answered_by "path:line" when the code or the spec already answers it; leave answered_by empty when it needs a decision. Set changes_result to true ONLY if two reasonable answers would give behaviour that a user or a test could tell apart; style, naming, placement and other details a developer can settle alone are false.`+fromSpec+
				reuseTask+
				` Give one entry in "packages" for each package under review, with its id, and no entry for any other package. Packages in this pass: `+strings.Join(ids, ", ")+`.`+budgetSentence(gateTurnsBase+gateTurnsPerPackage*len(ids)),
			schema, applyGroup(key, ids))
		p.budget = gateTurnsBase + gateTurnsPerPackage*len(ids)
		pkgPasses = append(pkgPasses, p)
	}
	if only != nil {
		return pkgPasses
	}
	passes := []gatePass{advA, pkgPasses[0], advB}
	return append(passes, pkgPasses[1:]...)
}

// gateSpec is a spec resolved the way the gate checks it: the checkout it
// lives in, its text and version, and its repo-relative path.
type gateSpec struct {
	s               site
	code, src, hash string
	rel             string // empty if the evidence check would not accept the path
	sp              *spec
}

func loadGateSpec(a *app, arg string) (*gateSpec, error) {
	s, err := a.site()
	if err != nil {
		return nil, err
	}
	specPath := expandHome(arg)
	if !filepath.IsAbs(specPath) {
		specPath = filepath.Join(a.cwd, specPath)
	}
	real, err := filepath.EvalSymlinks(specPath)
	if err != nil {
		return nil, fail("spec %s: %v", arg, err)
	}
	// Like `orch plan`, check the code in the checkout the spec lives in: a
	// linked worktree may hold code the main checkout does not have yet.
	code, err := git(filepath.Dir(real), "rev-parse", "--show-toplevel")
	if err != nil || code == "" || !sameGitCommonDir(code, s.repo) {
		return nil, fail("spec %s is not inside the repo %s", arg, s.repo)
	}
	srcB, err := os.ReadFile(real)
	if err != nil {
		return nil, fail("spec %s: %v", arg, err)
	}
	src := string(srcB)
	sp := parseSpec(src)
	sp.validate()
	if n := len(sp.problems); n > 0 {
		fmt.Fprintf(a.out, "spec has %d format problem(s); run `orch plan --dry-run` and fix them first\n", n)
		return nil, &exitError{code: 1}
	}
	// The spec's repo-relative path, so the model can cite its lines.
	rel := ""
	if root, err := filepath.EvalSymlinks(code); err == nil {
		if r, err := filepath.Rel(root, real); err == nil && evidenceHolds(code, filepath.ToSlash(r)+":1") {
			rel = filepath.ToSlash(r)
		}
	}
	return &gateSpec{s: s, code: code, src: src, hash: specHash(srcB), rel: rel, sp: sp}, nil
}

// cmdGateEnrich rewrites the question cards of the spec's latest gate run.
func cmdGateEnrich(a *app, args []string) error {
	pos, err := parseArgs(a.flags("gate"), args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "gate enrich <spec>"); err != nil {
		return err
	}
	gs, err := loadGateSpec(a, pos[0])
	if err != nil {
		return err
	}
	dir := gateDir(gs.s.root, gs.hash)
	res, err := enrichGate(dir, gs.code, gs.src, gs.rel, gs.sp)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Fprintf(a.out, "question cards: %s\nindex: %s\n", res.summary(), filepath.Join(dir, "questions", "INDEX.md"))
	for _, n := range res.Notes {
		fmt.Fprintf(a.out, "note: %s\n", agentScalar(n))
	}
	return nil
}

// gateOpts are the options of one gate run.
type gateOpts struct {
	keep     bool // keep the sessions of passes that succeeded (experiments)
	headless bool // tests only: run passes without panes even inside herdr
	runner   *roleRun
	ctxTool  bool
	ctxOnly  bool
	guard    *checkoutGuard
}

var experimentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,40}$`)

// onlyKeys turns --only into group keys; a key may be given without "group:".
func onlyKeys(list string, sp *spec) (map[string]bool, []string, error) {
	valid := map[string]bool{}
	var names []string
	for _, g := range gateGroups(sp, gateGroupMax, gateGroupChars, gateParallel) {
		valid[groupKey(g)] = true
		names = append(names, groupKey(g))
	}
	only := map[string]bool{}
	var keys []string
	for _, k := range strings.Split(list, ",") {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !strings.HasPrefix(k, "group:") {
			k = "group:" + k
		}
		if !valid[k] {
			return nil, nil, fail("--only: no group pass %q; the group passes of this spec are: %s", k, strings.Join(names, ", "))
		}
		only[k] = true
		keys = append(keys, k)
	}
	if len(only) == 0 {
		return nil, nil, fail("--only needs at least one group key; the group passes of this spec are: %s", strings.Join(names, ", "))
	}
	return only, keys, nil
}

func cmdGate(a *app, args []string) (retErr error) {
	if len(args) > 0 && args[0] == "enrich" {
		return cmdGateEnrich(a, args[1:])
	}
	if len(args) > 0 && args[0] == "compare" {
		return cmdGateCompare(a, args[1:])
	}
	runStarted := time.Now()
	fs := a.flags("gate")
	human := fs.Bool("human", false, "readable JSON of the selected gate summary")
	machine := fs.Bool("json", false, "JSON of the selected gate summary")
	schemaVersion := fs.Int("schema-version", 2, "2 (default): state error|stale|fail|incomplete|pass, plus experiment; only a current reducer pass authorizes plan; --human/--json select readable/machine JSON")
	defer func() {
		// Help is successful control flow, not a failed gate result.
		if !errors.Is(retErr, flag.ErrHelp) {
			retErr = outputFailure(a.out, explicitAudience(args), retErr)
		}
	}()
	dry := fs.Bool("dry-run", false, "show the passes, their sizes and the deterministic checks; call no model and write nothing")
	onlyF := fs.String("only", "", "experiment: run only these group passes (comma-separated keys); needs --experiment")
	expF := fs.String("experiment", "", "experiment: write everything under the gate dir's experiments/<name>/ and leave the real gate record alone")
	benchSuiteF := fs.String("bench-suite", "", "durable benchmark suite for experiment runs (default: user bench_suite, or none)")
	ctxTool := fs.String("ctx-tool", "on", "on or off: read-only orch ctx CLI in gate agents (default on)")
	ctxShell := fs.String("ctx-shell", "ctx-only", "open or ctx-only: pi with ctx-tool on defaults to read plus an orch ctx-only shell; open opts out; ignored by Claude and with ctx-tool off")
	runnerF := fs.String("runner", "", "ordered harness:model[:effort],... choices for this run")
	degraded := fs.Bool("degraded", false, "allow degraded role choices approved by the user")
	for _, arg := range args {
		if arg == "--" {
			break
		}
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "dossier" && strings.HasPrefix(arg, "-") {
			return fail("--dossier was removed; the gate runs without a dossier")
		}
		if name == "ctx-profile" && strings.HasPrefix(arg, "-") {
			return fail("--ctx-profile was removed")
		}
		if strings.HasPrefix(arg, "-") {
			switch name {
			case "cache", "keep-sessions", "context-files":
				return fail("--%s was removed", name)
			case "headless":
				return fail("--headless was removed; gate passes run in herdr panes")
			}
		}
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if *schemaVersion == 1 {
		return fail("--schema-version 1 is retired; use --schema-version 2 (the default), --human or --json")
	}
	if *schemaVersion != 2 {
		return fail("--schema-version is 2, not %d; use --schema-version 2 (the default)", *schemaVersion)
	}
	if err := exactArgs(pos, 1, "gate <spec> [--dry-run] | gate <spec> --only <group-key>[,...] --experiment <name> | gate enrich <spec> | gate compare <spec> <experiment-a> <experiment-b>"); err != nil {
		return err
	}
	if *ctxTool != "on" && *ctxTool != "off" {
		return fail("--ctx-tool is on or off, not %q", *ctxTool)
	}
	if *ctxShell != "open" && *ctxShell != "ctx-only" {
		return fail("--ctx-shell is open or ctx-only, not %q", *ctxShell)
	}
	if (*onlyF == "") != (*expF == "") {
		return fail("--only and --experiment go together")
	}
	if *expF != "" && !experimentRe.MatchString(*expF) {
		return fail("--experiment %q: use lowercase letters, digits, '.', '_' and '-'", *expF)
	}
	if !*dry && !gatePaneMode() && !gateOutsideHerdr {
		return fail("gate passes run in herdr panes; start orch gate inside herdr")
	}
	opts := gateOpts{ctxTool: *ctxTool == "on", ctxOnly: *ctxShell == "ctx-only" && *ctxTool == "on", keep: *expF != ""}
	if err := paneOrchOnPath(opts, *dry); err != nil {
		return fail("%v", err)
	}
	gs, err := loadGateSpec(a, pos[0])
	if err != nil {
		return err
	}
	s, code, src, sp, hash, rel := gs.s, gs.code, gs.src, gs.sp, gs.hash, gs.rel
	opts.runner, err = s.newRoleRun("gate", modelOptions{Runner: *runnerF, AllowDegraded: *degraded})
	if err != nil {
		return err
	}
	var only map[string]bool
	var onlyList []string
	if *onlyF != "" {
		if only, onlyList, err = onlyKeys(*onlyF, sp); err != nil {
			return err
		}
	}
	if *dry {
		return gateDryRun(a, pos[0], hash, code, src, rel, s.cfg, sp, only)
	}
	dir := gateDir(s.root, hash)
	runDir, experiment := dir, *expF != ""
	if experiment {
		runDir = filepath.Join(dir, "experiments", *expF)
		_ = os.RemoveAll(runDir)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			return err
		}
	}
	rec := &gateRecord{Spec: pos[0], Hash: hash, At: a.now().Unix(), PassErrors: map[string]string{}, PassSeconds: map[string]int{}, PassKept: map[string]string{}, PassTokens: map[string]passTokens{}, Groups: map[string][]string{},
		SpecRel: rel, CtxTool: onOff(opts.ctxTool)}
	rec.CtxShell = "open"
	if opts.ctxOnly {
		rec.CtxShell = "ctx-only"
	}
	rec.ReviewPolicy = &gateReviewPolicy{Version: gateReviewPolicyVersion, CtxTool: rec.CtxTool, CtxShell: rec.CtxShell}
	rec.BenchBuild = benchBuildNow()
	rec.BenchInputs = benchInputs{Repo: code, SpecPath: rel, SpecHash: hash, SpecSHA256: benchHash([]byte(src))}
	rec.BenchConfig = benchConfig{Roles: s.cfg.Roles, Flags: append([]string(nil), args...), Command: benchCommand(args), Budgets: map[string]int{}}
	if experiment {
		rec.Experiment, rec.Only = *expF, onlyList
	} else {
		pruneGate(s.root, hash, a.now())
		rec.Deterministic = gateDeterministic(code, s.cfg, sp, src)
	}

	opts.guard, err = newCheckoutGuard(code)
	guardErr := err
	var stamp checkoutStamp
	if opts.guard != nil {
		stamp = opts.guard.before
		rec.BenchInputs.Commit = strings.TrimSpace(stamp.head)
		rec.Subject, err = gateSubjectFromStamp(code, []byte(src), stamp)
		if err != nil {
			guardErr = err
		}
	}
	if e := benchWriteGateSubject(runDir, code, filepath.Join(code, rel), src, a.now(), stamp, guardErr); e != nil {
		rec.Notes = append(rec.Notes, "subject capture: "+e.Error())
		b, _ := json.Marshal(map[string]string{"error": e.Error()})
		_ = os.WriteFile(filepath.Join(runDir, "subject.json"), b, 0600)
	}
	model := gateModelDef
	passes := buildGatePasses(code, src, rel, sp, rec.Groups, only)
	initGateReview(rec, sp, src, passes)
	rec.Budget = newRunBudget(gateRunLimits(passes, s.cfg.GateBudget), runStarted)
	for _, p := range passes {
		if rec.BenchConfig.Budgets != nil {
			rec.BenchConfig.Budgets[p.name] = p.budget
		}
	}
	items, order := specItems(sp)
	if err := writeItems(runDir, items, order); err != nil {
		rec.PassErrors["all"] = err.Error()
	}
	for _, its := range items {
		if len(its) > 0 {
			rec.Checklist = "plain"
			break
		}
	}
	if guardErr != nil {
		rec.TreeCheckError = guardErr.Error()
		rec.PassErrors["all"] = guardErr.Error()
	} else {
		defer opts.guard.cancel()
		runGatePasses(runDir, code, model, passes, rec, sp, opts)
	}
	prepareGateReview(rec, code)
	offerGateAdjudication(s, rec, code, runDir, a.now(), modelOptions{AllowDegraded: *degraded})
	if opts.guard != nil {
		opts.guard.finish(rec)
	}
	finishGateSubject(rec, code, filepath.Join(code, rel))
	reduceGateRecord(rec)
	blockers, unanswered := len(rec.Reduction.ActiveBlockers), 0
	for _, q := range rec.Questions {
		if q.blocks() {
			unanswered++
		}
	}
	// State is derived only by ReduceReview. Keep execution diagnostics.
	var failedPasses []string
	for name := range rec.PassErrors {
		failedPasses = append(failedPasses, name)
	}
	sort.Strings(failedPasses)
	for _, name := range failedPasses {
		rec.Reasons = append(rec.Reasons, name+": "+rec.PassErrors[name])
	}
	if err := writeGate(runDir, rec); err != nil {
		return err
	}
	benchID := ""
	if experiment {
		suite := *benchSuiteF
		if suite == "" {
			suite = s.cfg.UserBenchSuite
		}
		if suite != "" {
			br, err := benchRecord(a, runDir, suite, rec.Experiment, rec.Experiment)
			if err != nil {
				return fmt.Errorf("benchmark recording failed (gate artifacts kept): %w", err)
			}
			benchID = suite + "/" + br.ID
		}
	}

	view := gateInformation{Budget: rec.Budget, SchemaVersion: rec.SchemaVersion, State: rec.State, Completion: rec.Completion, Reasons: rec.Reduction.Reasons, NextActors: rec.NextActors, NextActions: rec.NextActions, Failures: rec.Reduction.Failures, Gate: rec.State, Spec: pos[0] + " (version " + hash + ")", Report: filepath.Join(runDir, "report.md"), Bench: benchID, Warning: treeWarning(rec), Tokens: rec.tokenSummary(), Notes: append([]string{}, rec.Notes...), PassErrors: rec.PassErrors}
	if experiment {
		view.Experiment = *expF + " (groups " + strings.Join(onlyList, ", ") + ")"
	}
	concerns, unverified := 0, 0
	for _, f := range rec.Findings {
		switch {
		case !f.Verified:
			unverified++
		case f.Severity == "concern":
			concerns++
		}
	}
	view.Deterministic = fmt.Sprintf("%d issue(s)", len(rec.Deterministic))
	view.Blockers = fmt.Sprintf("%d verified", blockers)
	view.Concerns = fmt.Sprintf("%d verified, %d unverified", concerns, unverified)
	view.Questions = fmt.Sprintf("%d need a decision", unanswered)
	view.Dropped = fmt.Sprintf("%d claim(s) without real evidence", rec.Dropped)
	view.Coverage = rec.coverageLine()
	if !experiment && (unanswered > 0 || len(rec.Questions) > 0) {
		// Best effort: a failure here is a note and never changes the verdict.
		if res, err := enrichGate(dir, code, src, rel, sp); err != nil {
			view.Notes = append(view.Notes, "question cards were not enriched: "+err.Error())
		} else if res.Total > 0 {
			view.QuestionCards = res.summary()
			view.Notes = append(view.Notes, res.Notes...)
		}
	}
	if unanswered > 0 {
		view.Help = []string{"orch ask --cards " + filepath.Join(dir, "questions")}
	}
	view.Details = "Use native read on " + view.Report + " for findings, evidence and coverage details."
	audience := outputAgent
	if *human {
		audience = outputHuman
	}
	if *machine {
		audience = outputJSON
	}
	if err := writeOutput(a.out, audience, view); err != nil {
		return err
	}
	if experiment {
		if len(rec.PassErrors) > 0 {
			return &exitError{code: 1}
		}
		return nil
	}
	if rec.State != gateStatePass {
		return &exitError{code: 1}
	}
	return nil
}

// runGatePasses runs the passes through a pool of workers and applies each
// reply to rec. Inside herdr the workers are panes of one tab; if that tab
// cannot be opened the passes do not run (a pass error), unless opts.headless
// asks for headless passes. Outside herdr they run headless.
func runGatePasses(dir, code, model string, passes []gatePass, rec *gateRecord, sp *spec, opts gateOpts) {
	if len(passes) == 0 {
		return
	}
	if rec.Budget == nil {
		rec.Budget = newRunBudget(gateRunLimits(passes, nil), time.Now())
	}
	defer rec.Budget.refresh()
	if opts.runner == nil {
		opts.runner = &roleRun{s: site{repo: code}, role: "gate", enabled: true, choices: []profile{{Harness: "pi", Model: model}}, down: map[string]string{}}
	}
	if choices, _ := opts.runner.available(); len(choices) == 0 {
		rec.PassErrors["all"] = opts.runner.unavailable().Error()
		return
	}
	if opts.guard == nil {
		var err error
		opts.guard, err = newCheckoutGuard(code)
		if err != nil {
			rec.TreeCheckError = err.Error()
			rec.PassErrors["all"] = err.Error()
			return
		}
		defer opts.guard.cancel()
	}
	defer opts.guard.finish(rec)
	if rec.PassRunners == nil {
		rec.PassRunners = map[string]string{}
	}
	// Each worker takes passes from one queue, one at a time: a pane in herdr
	// mode, otherwise a headless slot. A spec with many passes therefore uses
	// at most gateParallel of either.
	workers := make([]string, min(gateParallel, len(passes)))
	var pool *gatePool
	if gatePaneMode() && !opts.headless {
		pl, err := openGatePool(code, rec.Hash, len(workers))
		if err == nil && (len(pl.panes) == 0 || pl.panes[0] == "") {
			err = fmt.Errorf("herdr created no pane")
		}
		if err != nil {
			rec.PassErrors["all"] = "the gate tab could not be opened in herdr, so no pass ran: " + err.Error()
			return
		}
		pool, workers = pl, pl.panes
		defer pool.close()
	}
	// Pane passes of one class send the same shared file, at the same path
	// and with the same bytes, so the provider can cache that prefix.
	if pool != nil {
		for _, p := range passes {
			if err := writeSharedFile(dir, p); err != nil {
				rec.PassErrors["all"] = err.Error()
			}
		}
	}
	queue := make(chan gatePass, len(passes))
	for _, p := range passes {
		queue <- p
	}
	close(queue)
	var wg, warm sync.WaitGroup
	var mu sync.Mutex
	// Only the first two workers start at once; the rest wait for the warm-up
	// delay, or for the first two to finish, whichever is sooner.
	release := make(chan struct{})
	for i, pane := range workers {
		wg.Add(1)
		if i < 2 {
			warm.Add(1)
		}
		go func(i int, pane string) {
			defer wg.Done()
			if i < 2 {
				defer warm.Done()
			} else {
				<-release
			}
			for p := range queue {
				if opts.guard.ctx.Err() != nil {
					return
				}
				passCtx, cancelPass := context.WithTimeout(opts.guard.ctx, min(gateTimeout, rec.Budget.remaining()))
				invocations := map[string]int{}
				calls := map[string]int{}
				lastRoute, retryAfter := "", ""
				replyError := func(raw string, err error) error {
					if err == nil {
						return nil
					}
					if strings.TrimSpace(raw) == "" {
						return fmt.Errorf("%s: empty reply: %w", lastRoute, err)
					}
					return fmt.Errorf("%s: %w", lastRoute, err)
				}
				run1 := func(q gatePass) (string, string, error) {
					q.ctxTool, q.ctxOnly, q.ctx = opts.ctxTool, opts.ctxOnly, passCtx
					job := strings.TrimSuffix(q.name, "#retry")
					repair := calls[job] > 0
					calls[job]++
					out, hit, used, err := opts.runner.runAfter(retryAfter, func(choice profile) (string, string, error) {
						prepared, prepErr := gateAgentPass(code, choice.Harness, q)
						if prepErr != nil {
							return "", "", prepErr
						}
						fallback := lastRoute != "" && lastRoute != choice.String()
						lastRoute = choice.String()
						invocations[q.name]++
						prepared.sessionDir = filepath.Join(dir, "sessions", passFile(q.name))
						if n := invocations[q.name]; n > 1 {
							prepared.sessionDir = filepath.Join(prepared.sessionDir, fmt.Sprintf("attempt-%d", n))
						}
						return runBudgetedGatePass(rec.Budget, prepared, repair, fallback, func(attempt gatePass) (string, string, error) {
							return opts.guard.run(q.name, func() (string, string, error) {
								if pool != nil {
									return runPaneChoice(&pane, code, choice, dir, attempt)
								}
								out, err := runHeadless(code, choice, attempt, attempt.sessionDir)
								return out, "", err
							})
						})
					})
					// The shared wall cap can expire in any runner operation.
					// Keep its diagnostic stable without hiding checkout cancellation.
					if rec.Budget.remaining() <= 0 {
						err = errAdjudicationBudget
					} else if err != nil && errors.Is(passCtx.Err(), context.DeadlineExceeded) && opts.guard.ctx.Err() == nil {
						err = &passLimit{fmt.Sprintf("timed out after %s", gateTimeout)}
					}
					mu.Lock()
					if used.Harness != "" {
						rec.PassRunners[q.name] = used.String()
					}
					mu.Unlock()
					return out, hit, err
				}
				start := time.Now()
				hit := ""
				run := func() (string, error) {
					out, h, err := run1(p)
					if hit == "" || h == "hard" {
						hit = h
					}
					return out, err
				}
				out, err := run()
				answerState := reviewAnswerUnavailable
				// Stall, schema repair and coverage share one retry slot and
				// the original deadline. A second reply never gets a third try.
				retried := false
				var limit *passLimit
				if errors.As(err, &limit) && strings.HasPrefix(limit.Error(), "stalled:") {
					retried = true
					retryAfter = lastRoute
					first := err
					if out, err = run(); err != nil {
						err = fmt.Errorf("%v; retried once and failed again: %w", first, err)
					}
				}
				if errors.As(err, &limit) && !strings.HasPrefix(limit.Error(), "stalled:") || errors.Is(err, errAdjudicationBudget) {
					answerState = reviewAnswerBudget
				}
				tokens, haveTokens := sessionTokens(filepath.Join(dir, "sessions", passFile(p.name)))
				mu.Lock()
				rec.PassSeconds[p.name] = int(time.Since(start).Seconds())
				if haveTokens || hit != "" {
					tokens.BudgetHit = hit
					rec.PassTokens[p.name] = tokens
				}
				var schemaErr error
				if err == nil {
					schemaErr = replyError(out, p.apply(out, rec, code, sp))
					if schemaErr != nil {
						answerState = reviewAnswerInvalid
					}
					err = schemaErr
				}
				mu.Unlock()
				var repair *gatePass
				if !retried && schemaErr != nil {
					retryAfter = lastRoute
					rp := p
					rp.name = p.name + "#retry"
					rp.task += "\nSchema repair: the earlier reply was incomplete or invalid: " + schemaErr.Error() + ". Return a complete replacement matching the schema above, with every required array explicitly present (use [] when empty)."
					rp.prompt = rp.shared + rp.task
					repair = &rp
				}
				if repair != nil {
					rp := *repair
					rstart := time.Now()
					rout, _, rerr := run1(rp)
					rtokens, rhave := sessionTokens(filepath.Join(dir, "sessions", passFile(rp.name)))
					mu.Lock()
					rec.PassSeconds[rp.name] = int(time.Since(rstart).Seconds())
					if rhave {
						rec.PassTokens[rp.name] = rtokens
					}
					if rerr != nil {
						answerState = reviewAnswerUnavailable
						if errors.As(rerr, &limit) && !strings.HasPrefix(limit.Error(), "stalled:") || errors.Is(rerr, errAdjudicationBudget) {
							answerState = reviewAnswerBudget
						}
					}
					if rerr == nil {
						rerr = replyError(rout, rp.apply(rout, rec, code, sp))
						if rerr != nil {
							answerState = reviewAnswerInvalid
						}
					}
					if rerr != nil {
						if schemaErr != nil {
							rerr = fmt.Errorf("%v; %w", schemaErr, rerr)
						}
						err = fmt.Errorf("repair exhausted: %w", rerr)
						rec.PassKept[rp.name] = keptFiles(dir, rp.name, pool != nil)
					} else {
						err = nil
						dropPassFiles(dir, rp.name, opts.keep)
					}
					mu.Unlock()
				}
				mu.Lock()
				if err != nil {
					if rec.PassAnswerStates == nil {
						rec.PassAnswerStates = map[string]reviewAnswerState{}
					}
					errorKey := p.name
					if errors.Is(err, errAdjudicationBudget) {
						errorKey = recordPartialBudgetFailure(rec, p.name)
					}
					rec.PassAnswerStates[errorKey] = answerState
					rec.PassErrors[errorKey] = err.Error()
					rec.PassKept[p.name] = keptFiles(dir, p.name, pool != nil)
				} else {
					dropPassFiles(dir, p.name, opts.keep)
				}
				mu.Unlock()
				cancelPass()
			}
		}(i, pane)
	}
	go func() {
		warmed := make(chan struct{})
		go func() { warm.Wait(); close(warmed) }()
		select {
		case <-opts.guard.ctx.Done():
		case <-time.After(gateWarmDelay):
		case <-warmed:
		}
		close(release)
	}()
	wg.Wait()
	opts.runner.mu.Lock()
	rec.Notes = append(rec.Notes, opts.runner.notes...)
	opts.runner.mu.Unlock()
	if pool != nil && len(rec.PassErrors) == 0 {
		dropSharedFiles(dir, passes)
	}
	if pool != nil {
		pool.close()
	}
}

// advisory reports a deterministic line that informs but never fails the gate.
func advisory(d string) bool {
	return strings.HasPrefix(d, "no `check` command") || strings.HasPrefix(d, "note: ")
}

func hasError(det []string) bool {
	for _, d := range det {
		if !advisory(d) {
			return true
		}
	}
	return false
}

type scopeReply struct {
	WillChange []string `json:"will_change"`
	Claims     []struct{ Claim, Evidence string }
}

// groupEntry is one package's answer inside a group reply. The fields are
// flat (no nested wrapper to forget to close).
type groupEntry struct {
	ID         string   `json:"id"`
	WillChange []string `json:"will_change"`
	Claims     []struct{ Claim, Evidence string }
	Questions  []gateQuestion `json:"questions"`
	Reuse      []struct{ Builds, Existing, Evidence, Why string }
}

// salvageEntries recovers answers from a reply that is not valid JSON as a
// whole, for example a model that left an object unclosed before its next key
// or a reply cut short. It finds each package's entry by its "id", then
// decodes the value after each known key, which stops after one complete
// value and ignores whatever follows. A group of one package may omit the id.
// ok is false when no value could be decoded at all.
func salvageEntries(raw string, ids []string) (entries []groupEntry, ok bool) {
	type located struct {
		id    string
		start int
	}
	var locs []located
	for _, id := range ids {
		re := regexp.MustCompile(`"id"\s*:\s*` + regexp.QuoteMeta(strconv.Quote(id)))
		if m := re.FindStringIndex(raw); m != nil {
			locs = append(locs, located{id, m[0]})
		}
	}
	if len(locs) == 0 && len(ids) == 1 {
		locs = []located{{ids[0], 0}}
	}
	for _, l := range locs {
		end := len(raw)
		for _, o := range locs {
			if o.start > l.start && o.start < end {
				end = o.start
			}
		}
		part := raw[l.start:end]
		e := groupEntry{ID: l.id}
		decode := func(key string, v any) bool {
			m := regexp.MustCompile(`"` + key + `"\s*:`).FindStringIndex(part)
			return m != nil && json.NewDecoder(strings.NewReader(part[m[1]:])).Decode(v) == nil
		}
		got := decode("will_change", &e.WillChange)
		got = decode("claims", &e.Claims) || got
		got = decode("questions", &e.Questions) || got
		got = decode("reuse", &e.Reuse) || got
		ok = ok || got
		entries = append(entries, e)
	}
	return entries, ok
}

// applyGroup decodes one group pass and feeds each package's entry to the
// scope and question checks only after validating the complete response.
// Salvaged fragments remain diagnostic and cannot establish coverage.
func applyGroup(key string, ids []string) func(string, *gateRecord, string, *spec) error {
	return func(raw string, rec *gateRecord, repo string, sp *spec) error {
		var r struct {
			Packages []groupEntry `json:"packages"`
		}
		decodeErr := extractJSON(raw, &r)
		if decodeErr != nil {
			var ok bool
			if r.Packages, ok = salvageEntries(raw, ids); !ok {
				return decodeErr
			}
			rec.Salvaged = append(rec.Salvaged, key)
		} else if err := validateEntries(r.Packages, ids); err != nil {
			return err
		}
		for _, e := range r.Packages {
			scopeFor(e.ID, scopeReply{WillChange: e.WillChange, Claims: e.Claims}, rec, repo, sp)
			for i := range e.Questions {
				if e.Questions[i].Package == "" {
					e.Questions[i].Package = e.ID
				}
			}
			questionsFor(e.ID, e.Questions, rec, repo)
			for _, u := range e.Reuse {
				if strings.TrimSpace(u.Builds) == "" {
					continue
				}
				f := gateFinding{Severity: "concern", Where: "package " + e.ID, Claim: u.Builds + " may duplicate " + u.Existing,
					Evidence: u.Evidence, Why: u.Why, Source: "reuse", Verified: evidenceHolds(repo, u.Evidence)}
				if !f.Verified {
					rec.Dropped++
				}
				rec.Findings = append(rec.Findings, f)
			}
			if decodeErr == nil {
				recordGateGroup(rec, key, raw, e)
			}
		}
		if decodeErr != nil {
			return fmt.Errorf("incomplete review: salvaged reply: %w", decodeErr)
		}
		return nil
	}
}

func scopeFor(only string, r scopeReply, rec *gateRecord, repo string, sp *spec) {
	var pkg *specPackage
	for _, p := range sp.packages {
		if p.id == only {
			pkg = p
		}
	}
	if pkg == nil {
		return
	}
	for _, c := range r.Claims {
		if !evidenceHolds(repo, c.Evidence) {
			rec.Dropped++
		}
	}
	for _, f := range r.WillChange {
		f = changePath(f)
		if f == "" || ownedPath(pkg.owns, f) {
			continue
		}
		// Another package's file is a blocker unless that package builds on this
		// one: then it wires this package in, and the finding asks it to.
		sev, note, support := "concern", "", ""
		if o := sp.ownerOf(f); o != nil && o.id != pkg.id {
			sev, note, support = "blocker", ", owned by "+o.id, "structural"
			if sp.relation(pkg, o) == relDownstream {
				sev, note = "concern", ", owned by "+o.id+", which uses this package and wires it in: confirm that it does"
			}
		}
		rec.Findings = append(rec.Findings, gateFinding{Severity: sev, Where: "package " + pkg.id,
			Claim: "the work must change " + f + ", which the package does not own" + note, Evidence: f + ":1", Source: "scope",
			Support: support, Verified: support == "structural" || evidenceHolds(repo, f+":1")})
	}
}

func questionsFor(only string, qs []gateQuestion, rec *gateRecord, repo string) {
	for _, q := range qs {
		if strings.TrimSpace(q.Question) == "" || q.Package != only {
			continue
		}
		if q.AnsweredBy != "" && !evidenceHolds(repo, q.AnsweredBy) {
			q.AnsweredBy = "" // an answer the code does not contain is no answer
			rec.Dropped++
		}
		rec.Questions = append(rec.Questions, q)
	}
}

func applyFindings(source string) func(string, *gateRecord, string, *spec) error {
	return func(raw string, rec *gateRecord, repo string, sp *spec) error {
		var r struct {
			Findings []json.RawMessage `json:"findings"`
		}
		decodeErr := extractJSON(raw, &r)
		if decodeErr != nil {
			m := regexp.MustCompile(`"findings"\s*:`).FindStringIndex(raw)
			if m == nil || json.NewDecoder(strings.NewReader(raw[m[1]:])).Decode(&r.Findings) != nil {
				return decodeErr
			}
			rec.Salvaged = append(rec.Salvaged, source)
		}
		if r.Findings == nil {
			return fmt.Errorf("findings must be an explicit array")
		}
		var findings []gateFinding
		for _, entry := range r.Findings {
			if err := checkReplyObject(entry, map[string]string{"severity": "string", "where": "string", "claim": "string", "evidence": "string", "why?": "string"}); err != nil {
				return fmt.Errorf("invalid finding: %w", err)
			}
			var f gateFinding
			if err := json.Unmarshal(entry, &f); err != nil {
				return err
			}
			if f.Severity != "blocker" && f.Severity != "concern" {
				return fmt.Errorf("invalid finding severity %q", f.Severity)
			}
			findings = append(findings, f)
		}
		if decodeErr == nil {
			recordGateAnswer(rec, "@"+source, source, source, raw, reviewJudgmentSatisfied, "Explicit adversarial findings assessment", "", "")
		}
		for _, f := range findings {
			f.Source, f.Support = source, ""
			f.Verified = evidenceHolds(repo, f.Evidence)
			if !f.Verified {
				rec.Dropped++
			}
			rec.Findings = append(rec.Findings, f)
		}
		if decodeErr != nil {
			return fmt.Errorf("incomplete review: salvaged findings: %w", decodeErr)
		}
		return nil
	}
}

// cardKey names the nth question card, for the package it was raised for.
func cardKey(n int, pkg string) string {
	if id := slug(pkg); id != "" {
		return fmt.Sprintf("q%d-%s", n, id)
	}
	return fmt.Sprintf("q%d", n)
}

func writeGate(dir string, rec *gateRecord) error {
	qdir := filepath.Join(dir, "questions")
	_ = os.RemoveAll(qdir)
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		return err
	}
	n := 0
	for _, q := range rec.Questions {
		if !q.blocks() {
			continue
		}
		n++
		key := cardKey(n, q.Package)
		card := gateQuestionCard(q)
		if err := os.WriteFile(filepath.Join(qdir, key+".md"), []byte(card), 0o644); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "gate.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	var r strings.Builder
	if warning := treeWarning(rec); warning != "" {
		fmt.Fprintf(&r, "%s\n\n", warning)
	}
	fmt.Fprintf(&r, "# Gate report: %s (spec version %s)\n\n", rec.Spec, rec.Hash)
	if rec.SchemaVersion == gateStatusSchemaVersion {
		fmt.Fprintf(&r, "Review state: **%s** (schema %d)\n\nCompletion: %d/%d required checks complete; %d incomplete; %d optional.\n\n", rec.State, rec.SchemaVersion, rec.Completion.Completed, rec.Completion.Required, rec.Completion.Incomplete, rec.Completion.Optional)
	} else if label := storedGateResultLabel(rec); label != "" {
		fmt.Fprintf(&r, "Historical result: **%s** (legacy record, not a v2 state)\n\n", label)
	}
	fmt.Fprintf(&r, "ctx_tool: %s; ctx_shell: %s%s%s\n\n", orDefault(rec.CtxTool, "off (legacy)"), orDefault(rec.CtxShell, "open"), legacyContextFiles(rec), legacyCtxProfile(rec))
	if t := rec.tokenSummary(); t != "" {
		fmt.Fprintf(&r, "Tokens: %s\n\n", t)
	}
	if rec.Checklist == "plain" {
		r.WriteString("Reuse checklist: plain (spec text and author pointers; no lookups, no candidates)\n\n")
	}
	if c := rec.coverageLine(); c != "" {
		r.WriteString(c)
		r.WriteString("\n\n")
	}
	if rec.Cache == "long" {
		r.WriteString("Cache: long (PI_CACHE_RETENTION=long)\n\n")
	}
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&r, "## %s\n", title)
		for _, l := range lines {
			fmt.Fprintf(&r, "- %s\n", l)
		}
		r.WriteString("\n")
	}
	section("Approval reasons", rec.Reasons)
	var actions []string
	for _, action := range rec.NextActions {
		actions = append(actions, action.Actor+": "+action.Action)
	}
	section("Next actions", actions)
	section("Deterministic issues", rec.Deterministic)
	var verified, unverified []string
	for _, f := range rec.Findings {
		line := fmt.Sprintf("[%s, %s] %s: %s (%s) %s", f.Severity, f.Source, f.Where, f.Claim, f.Evidence, f.Why)
		if f.Verified {
			verified = append(verified, line)
		} else {
			unverified = append(unverified, line)
		}
	}
	section("Findings with real evidence", verified)
	section("Findings without real evidence (not counted)", unverified)
	var qs, minor []string
	for _, q := range rec.Questions {
		switch {
		case q.blocks():
			qs = append(qs, fmt.Sprintf("%s: %s", q.Package, q.Question))
		case q.AnsweredBy == "":
			minor = append(minor, fmt.Sprintf("%s: %s", q.Package, q.Question))
		}
	}
	section("Questions that need a decision", qs)
	section("Minor open questions (a lead may settle these; not blocking)", minor)
	var errs []string
	for name, msg := range rec.PassErrors {
		errs = append(errs, name+": "+msg)
		if k := rec.PassKept[name]; k != "" {
			errs = append(errs, name+": kept "+k)
		}
	}
	section("Passes that did not run", errs)
	var runners []string
	for name, runner := range rec.PassRunners {
		runners = append(runners, name+": "+runner)
	}
	sort.Strings(runners)
	section("Runners", runners)
	var groups []string
	keys := make([]string, 0, len(rec.Groups))
	for k := range rec.Groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		groups = append(groups, k+": "+strings.Join(rec.Groups[k], ", "))
	}
	section("Packages covered by each group pass", groups)
	section("Replies that were not valid JSON and were read as far as possible (check these passes)", rec.Salvaged)
	section("Notes", rec.Notes)
	section("Exploration per pass (from the kept sessions)", explorationLines(dir, rec))
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(r.String()), 0o644)
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 24 {
		out = strings.Trim(out[:24], "-")
	}
	return out
}

// gateDryRun shows what a gate run would do without calling a model or
// writing anything: the deterministic checks, then each pass with its
// packages and prompt sizes (an estimated token is four characters).
func gateDryRun(a *app, specArg, hash, code, src, rel string, cfg config, sp *spec, only map[string]bool) error {
	det := gateDeterministic(code, cfg, sp, src)
	groups := map[string][]string{}
	passes := buildGatePasses(code, src, rel, sp, groups, only)
	tok := func(n int) int { return (n + 3) / 4 }
	fmt.Fprintf(a.out, "gate dry run: %s (version %s); no model is called and nothing is written\n", agentScalar(specArg), hash)
	fmt.Fprintf(a.out, "deterministic: %d issue(s)\n", len(det))
	for _, d := range det {
		fmt.Fprintf(a.out, "  %s\n", d)
	}
	items, _ := specItems(sp)
	n := 0
	for _, its := range items {
		n += len(its)
	}
	fmt.Fprintf(a.out, "reuse checklist: %d item(s), plain (spec text only; no lookups, no candidates)\n", n)
	total := 0
	show := func(p gatePass, covers, label string) {
		total += tok(len(p.shared) + len(p.task))
		extra := ""
		if p.noTools {
			extra = "  tools: none\n"
		} else if p.budget > 0 {
			extra = fmt.Sprintf("  turn budget: %d soft, %d hard\n", p.budget, int(float64(p.budget)*gateBudgetHard))
		}
		fmt.Fprintf(a.out, "%s: %s\n  packages: %s\n  shared prefix: %d chars (~%d tokens)\n  task: %d chars (~%d tokens)\n%s  starts: %s\n",
			label, p.name, covers, len(p.shared), tok(len(p.shared)), len(p.task), tok(len(p.task)), extra, agentScalar(p.brief))
	}
	for _, p := range passes {
		covers := "the whole spec"
		if ids, ok := groups[p.name]; ok {
			covers = strings.Join(ids, ", ")
		}
		show(p, covers, "pass")
	}
	fmt.Fprintf(a.out, "totals: %d passes, %d workers, ~%d input tokens summed over the passes\n", len(passes), min(gateParallel, len(passes)), total)
	return nil
}

// explorationLines profiles the kept session of each pass of rec under dir.
func explorationLines(dir string, rec *gateRecord) []string {
	var names []string
	for n := range rec.PassSeconds {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		if p, ok := profileSession(filepath.Join(dir, "sessions", passFile(n)), rec.SpecRel); ok {
			out = append(out, n+": "+p.String())
		}
	}
	return out
}

var changeNoteRe = regexp.MustCompile(`\s*\([^)]*\)\s*$`)

// changePath is the path in a model's will_change entry, without a trailing
// remark such as "(new)" or "(new review queue, block list)".
func changePath(f string) string {
	return strings.Trim(changeNoteRe.ReplaceAllString(strings.TrimSpace(f), ""), "` ")
}
