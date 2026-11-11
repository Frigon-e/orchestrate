package guard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LSP enrichment: after a search for a symbol name (rg, grep -r, git grep or the
// Grep tool) a PostToolUse hook appends a few language-server facts the search
// output does not already show. Every failure is silent: no error, no block.

const enrichBudgetDefault = 800 * time.Millisecond

var (
	enrichStrip  = strings.NewReplacer(`\b`, "", `\<`, "", `\>`, "")
	enrichPrefix = regexp.MustCompile(`^(?:func|type|const|var|interface|class|def)(?: |\\s[+*]|\\s)+`)
	enrichRecv   = regexp.MustCompile(`^\\?\([^)]*\\?\)(?: |\\s[+*]|\\s)*`)
	enrichTrail  = regexp.MustCompile(`\\?\($`)
	enrichName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)
	testPath     = regexp.MustCompile(`(_test\.[a-z]+$|\.(test|spec)\.[a-z]+$|(^|/)tests?/)`)
	linePrefix   = regexp.MustCompile(`^(.+?):(\d+):`)
	bareLine     = regexp.MustCompile(`^(\d+)[:-]`)
	typeDecl     = regexp.MustCompile(`^type\s+(\w+)`)
)

// searchCall is one search a tool call ran.
type searchCall struct {
	tool                        string // rg, grep, git grep, Grep
	patterns                    []string
	ere, literal, ignoreCase    bool
	filesOnly, count, recursive bool
	bad                         bool // scope or flags this parser cannot judge
	paths                       []string
	globs                       []string // rg style: a leading ! excludes
	types, typesNot             []string
	cwd                         string
	mode                        string // Grep tool output_mode
	stdinFed                    bool
}

var typeExts = map[string][]string{
	"go": {"go"}, "py": {"py", "pyi"}, "python": {"py", "pyi"}, "js": {"js", "jsx", "mjs", "cjs"},
	"ts": {"ts", "tsx", "mts", "cts"}, "rust": {"rs"}, "java": {"java"}, "c": {"c", "h"},
	"cpp": {"cpp", "cc", "cxx", "hpp", "hh", "h"}, "rb": {"rb"}, "ruby": {"rb"}, "php": {"php"},
	"cs": {"cs"}, "kotlin": {"kt", "kts"}, "swift": {"swift"}, "sh": {"sh", "bash"},
}

var longValue = set("--regexp", "--glob", "--iglob", "--type", "--type-not", "--type-add", "--max-count", "--max-depth",
	"--maxdepth", "--max-columns", "--replace", "--file", "--after-context", "--before-context", "--context", "--threads",
	"--encoding", "--sort", "--sortr", "--include", "--exclude", "--exclude-dir", "--exclude-from", "--label",
	"--binary-files", "--devices", "--directories", "--engine", "--pre", "--pre-glob", "--ignore-file",
	"--max-filesize", "--path-separator", "--context-separator", "--field-match-separator", "--field-context-separator")

func shortValue(tool string) string {
	switch tool {
	case "rg":
		return "egtTABCmjfrdME"
	case "grep":
		return "efABCmdD"
	}
	return "efABCm"
}

// parseSearch is the search a command runs, or false when it is not one.
func parseSearch(tool string, args []string, stdin bool, cwd string) (*searchCall, bool) {
	c := &searchCall{tool: tool, cwd: cwd, stdinFed: stdin}
	var rest []string
	dd := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			dd = true
			rest = append(rest, args[i+1:]...)
			break
		}
		switch {
		case strings.HasPrefix(a, "--"):
			k, v, hasV := strings.Cut(a, "=")
			if (longValue[k] || (tool == "rg" && k == "--color")) && !hasV && i+1 < len(args) {
				i++
				v = args[i]
			}
			switch k {
			case "--ignore-case":
				c.ignoreCase = true
			case "--fixed-strings":
				c.literal = true
			case "--files-with-matches":
				c.filesOnly = true
			case "--count":
				c.count = true
			case "--recursive", "--dereference-recursive":
				c.recursive = true
			case "--extended-regexp", "--perl-regexp":
				c.ere = true
			case "--regexp":
				c.patterns = append(c.patterns, v)
			case "--glob", "--iglob":
				c.globs = append(c.globs, v)
			case "--include":
				c.globs = append(c.globs, v)
			case "--exclude", "--exclude-dir":
				c.globs = append(c.globs, "!"+v)
			case "--type":
				c.types = append(c.types, v)
			case "--type-not":
				c.typesNot = append(c.typesNot, v)
			case "--files", "--type-list", "--invert-match", "--files-without-match", "--quiet", "--file", "--type-add", "--pre":
				c.bad = true
				if k == "--files" || k == "--type-list" {
					return nil, false
				}
			}
		case len(a) > 1 && a[0] == '-':
			sv := shortValue(tool)
			for j := 1; j < len(a); j++ {
				ch := a[j]
				if strings.IndexByte(sv, ch) >= 0 {
					val := a[j+1:]
					if val == "" && i+1 < len(args) {
						i++
						val = args[i]
					}
					switch ch {
					case 'e':
						c.patterns = append(c.patterns, val)
					case 'g':
						if tool == "rg" {
							c.globs = append(c.globs, val)
						}
					case 't':
						if tool == "rg" {
							c.types = append(c.types, val)
						}
					case 'T':
						if tool == "rg" {
							c.typesNot = append(c.typesNot, val)
						}
					case 'f':
						c.bad = true
					}
					break
				}
				switch ch {
				case 'i':
					c.ignoreCase = true
				case 'F':
					c.literal = true
				case 'l':
					c.filesOnly = true
				case 'c':
					c.count = true
				case 'r', 'R':
					c.recursive = true
				case 'E', 'P':
					c.ere = true
				case 'v', 'L', 'q':
					c.bad = true
				}
			}
		default:
			rest = append(rest, a)
		}
	}
	if len(c.patterns) == 0 {
		if len(rest) == 0 {
			return nil, false
		}
		c.patterns, rest = []string{rest[0]}, rest[1:]
	}
	switch tool {
	case "grep":
		if (!c.recursive && len(rest) == 0) || (stdin && len(rest) == 0) {
			return nil, false
		}
		c.paths = rest
	case "rg":
		if stdin && len(rest) == 0 {
			return nil, false
		}
		c.paths = rest
	case "git grep":
		if dd {
			// before -- come revisions: not paths. rest holds the pattern's leftovers and the pathspecs.
			c.paths = pathspecsAfter(args)
		} else {
			c.paths = rest
		}
	}
	return c, true
}

func pathspecsAfter(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// grepToolCall is the search the Grep tool ran.
func grepToolCall(input map[string]any, cwd string) *searchCall {
	pat, _ := input["pattern"].(string)
	if pat == "" {
		return nil
	}
	c := &searchCall{tool: "Grep", patterns: []string{pat}, cwd: cwd, ere: true}
	if p, _ := input["path"].(string); p != "" {
		c.paths = []string{p}
	}
	if g, _ := input["glob"].(string); g != "" {
		c.globs = []string{g}
	}
	if t, _ := input["type"].(string); t != "" {
		c.types = []string{t}
	}
	c.ignoreCase, _ = input["-i"].(bool)
	c.mode, _ = input["output_mode"].(string)
	if c.mode == "" {
		c.mode = "files_with_matches"
	}
	switch c.mode {
	case "files_with_matches":
		c.filesOnly = true
	case "count":
		c.count = true
	}
	if m, _ := input["multiline"].(bool); m {
		c.bad = true
	}
	return c
}

// searchCallsOf lists the searches of a Claude PostToolUse event.
func searchCallsOf(ev *Event, cwd string) []*searchCall {
	input, ok := ev.Object("tool_input")
	if !ok {
		return nil
	}
	switch ev.Str("tool_name") {
	case "Grep":
		if c := grepToolCall(input, cwd); c != nil {
			return []*searchCall{c}
		}
	case "Bash":
		line, _ := input["command"].(string)
		var out []*searchCall
		cur, unknown := cwd, false
		for _, s := range segments(line, true) {
			name, args := command(s)
			var c *searchCall
			var ok bool
			switch {
			case name == "cd":
				dir := ""
				for _, a := range args {
					if !strings.HasPrefix(a, "-") {
						dir = a
						break
					}
				}
				switch {
				case dir == "":
					unknown = true
				case strings.ContainsAny(dir, "$`"):
					unknown = true
				default:
					if strings.HasPrefix(dir, "~") {
						home, _ := os.UserHomeDir()
						dir = filepath.Join(home, dir[1:])
					}
					if !filepath.IsAbs(dir) {
						dir = filepath.Join(cur, dir)
					}
					cur = dir
				}
				continue
			case name == "rg":
				c, ok = parseSearch("rg", args, s.stdin, cur)
			case name == "grep" || name == "egrep" || name == "fgrep":
				c, ok = parseSearch("grep", args, s.stdin, cur)
				if ok && name == "egrep" {
					c.ere = true
				}
				if ok && name == "fgrep" {
					c.literal = true
				}
			case name == "git" && len(args) > 0 && args[0] == "grep":
				c, ok = parseSearch("git grep", args[1:], s.stdin, cur)
			}
			if ok {
				c.bad = c.bad || unknown
				out = append(out, c)
			}
		}
		return out
	}
	return nil
}

// patternNames are the symbol names a search pattern looks for.
func patternNames(c *searchCall) []string {
	var names []string
	for _, p := range c.patterns {
		if c.literal && strings.Contains(p, " ") {
			return nil
		}
		var alts []string
		switch {
		case c.literal:
			alts = []string{p}
		case (c.tool == "grep" || c.tool == "git grep") && !c.ere:
			alts = strings.Split(p, `\|`)
		default:
			alts = splitAlternatives(p)
		}
		for _, alt := range alts {
			alt = strings.TrimSpace(enrichStrip.Replace(alt))
			alt = strings.TrimSuffix(strings.TrimPrefix(alt, "^"), "$")
			alt = enrichPrefix.ReplaceAllString(alt, "")
			alt = enrichRecv.ReplaceAllString(alt, "")
			alt = enrichTrail.ReplaceAllString(alt, "")
			alt = strings.TrimSpace(strings.ReplaceAll(alt, `\.`, "."))
			if len(alt) >= 4 && enrichName.MatchString(alt) && !contains(names, alt) {
				names = append(names, alt)
			}
		}
	}
	return names
}

// scope is where a search looked.
type scope struct {
	cwd      string
	roots    []string
	inc, exc []*regexp.Regexp
	exts     map[string]bool
	extsNot  map[string]bool
	label    string
	bad      bool
}

func globRegex(g string) *regexp.Regexp {
	g = strings.TrimSuffix(g, "/")
	anchored := strings.Contains(strings.TrimPrefix(g, "/"), "/")
	g = strings.TrimPrefix(g, "/")
	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?")
	}
	brace := 0
	for i := 0; i < len(g); i++ {
		switch ch := g[i]; ch {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '{':
			brace++
			b.WriteString("(?:")
		case '}':
			if brace > 0 {
				brace--
				b.WriteString(")")
			} else {
				b.WriteString(`\}`)
			}
		case ',':
			if brace > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		case '[':
			end := strings.IndexByte(g[i:], ']')
			if end < 0 {
				return nil
			}
			b.WriteString(g[i : i+end+1])
			i += end
		case '\\':
			if i+1 < len(g) {
				i++
				b.WriteString(regexp.QuoteMeta(string(g[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	if brace != 0 {
		return nil
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

func buildScope(c *searchCall) *scope {
	s := &scope{cwd: c.cwd}
	if c.bad {
		s.bad = true
		return s
	}
	var labels []string
	add := func(g string) {
		neg := strings.HasPrefix(g, "!")
		re := globRegex(strings.TrimPrefix(g, "!"))
		if re == nil {
			s.bad = true
			return
		}
		labels = append(labels, "-g "+g)
		if neg {
			s.exc = append(s.exc, re)
		} else {
			s.inc = append(s.inc, re)
		}
	}
	for _, p := range c.paths {
		if c.tool == "git grep" {
			switch {
			case strings.HasPrefix(p, ":!") || strings.HasPrefix(p, ":^"):
				add("!" + p[2:])
			case strings.HasPrefix(p, ":(exclude)"):
				add("!" + strings.TrimPrefix(p, ":(exclude)"))
			case strings.HasPrefix(p, ":"):
				s.bad = true
			case strings.ContainsAny(p, "*?["):
				add(p)
			default:
				s.addRoot(p, &labels)
			}
			continue
		}
		s.addRoot(p, &labels)
	}
	for _, g := range c.globs {
		add(g)
	}
	set := func(list []string) map[string]bool {
		out := map[string]bool{}
		for _, t := range list {
			exts, ok := typeExts[t]
			if !ok {
				s.bad = true
			}
			for _, e := range exts {
				out[e] = true
			}
		}
		return out
	}
	if len(c.types) > 0 {
		s.exts = set(c.types)
		labels = append(labels, "-t "+strings.Join(c.types, ","))
	}
	if len(c.typesNot) > 0 {
		s.extsNot = set(c.typesNot)
	}
	s.label = strings.Join(labels, " ")
	if s.label == "" {
		s.label = "the searched scope"
	}
	return s
}

func (s *scope) addRoot(p string, labels *[]string) {
	if strings.ContainsAny(p, "$`*?[") {
		s.bad = true
		return
	}
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, p[1:])
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.cwd, abs)
	}
	s.roots = append(s.roots, filepath.Clean(abs))
	*labels = append(*labels, strings.TrimSuffix(p, "/"))
}

func under(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func (s *scope) contains(abs string) bool {
	roots := s.roots
	if len(roots) == 0 {
		roots = []string{s.cwd}
	}
	ok := false
	for _, r := range roots {
		if under(r, abs) {
			ok = true
		}
	}
	if !ok {
		return false
	}
	rel, err := filepath.Rel(s.cwd, abs)
	if err != nil || strings.HasPrefix(rel, "../") {
		rel = strings.TrimPrefix(abs, "/")
	}
	parts := strings.Split(rel, "/")
	hit := func(res []*regexp.Regexp) bool {
		for i := 1; i <= len(parts); i++ {
			prefix := strings.Join(parts[:i], "/")
			for _, re := range res {
				if re.MatchString(prefix) {
					return true
				}
			}
		}
		return false
	}
	if hit(s.exc) || (len(s.inc) > 0 && !hit(s.inc)) {
		return false
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(abs), "."))
	if s.exts != nil && !s.exts[ext] {
		return false
	}
	return !s.extsNot[ext]
}

// codeSearch: the search may look for code (not docs only, not case-insensitive).
func codeSearch(c *searchCall) bool {
	if c.ignoreCase {
		return false
	}
	var globs []string
	for _, g := range c.globs {
		if !strings.HasPrefix(g, "!") {
			globs = append(globs, g)
		}
	}
	return !docOnly(globs, c.types, c.paths)
}

// shown is what the agent saw of a search's output.
type shown struct {
	files map[string]bool
	lines map[string]map[int]bool
}

func (s shown) has(abs string, line int) bool {
	return s.files[abs] || s.lines[abs][line]
}

func parseShown(c *searchCall, text string) shown {
	sh := shown{files: map[string]bool{}, lines: map[string]map[int]bool{}}
	toAbs := func(p string) string {
		p = strings.TrimPrefix(p, "./")
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.cwd, p)
		}
		return filepath.Clean(p)
	}
	switch {
	case c.count:
		return sh
	case c.filesOnly:
		for _, l := range strings.Split(text, "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "Found ") || strings.HasPrefix(l, "No ") || strings.HasPrefix(l, "[") {
				continue
			}
			sh.files[toAbs(l)] = true
		}
		return sh
	}
	single := ""
	if len(c.paths) == 1 {
		if info, err := os.Stat(toAbs(c.paths[0])); err == nil && info.Mode().IsRegular() {
			single = toAbs(c.paths[0])
		}
	}
	for _, l := range strings.Split(text, "\n") {
		path, n := "", 0
		if m := linePrefix.FindStringSubmatch(l); m != nil && single == "" {
			path = toAbs(m[1])
			n, _ = strconv.Atoi(m[2])
		} else if m := bareLine.FindStringSubmatch(l); m != nil && single != "" {
			path = single
			n, _ = strconv.Atoi(m[1])
		}
		if path == "" {
			continue
		}
		if sh.lines[path] == nil {
			sh.lines[path] = map[int]bool{}
		}
		sh.lines[path][n] = true
	}
	return sh
}

// lspItem and lspResult are the shapes of `orch lsp <verb> --json`.
type lspItem struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	Doc  string `json:"doc"`
	In   string `json:"in"`
}

type lspResult struct {
	Status   string    `json:"status"`
	Total    int       `json:"total"`
	Items    []lspItem `json:"items"`
	exit     int
	timedOut bool
	failed   bool
}

func orchBin() string {
	if b := os.Getenv("ORCH_BIN"); b != "" {
		return b
	}
	if p, err := exec.LookPath("orch"); err == nil {
		return p
	}
	return ""
}

func runLSP(ctx context.Context, bin, cwd string, args ...string) lspResult {
	cmd := exec.CommandContext(ctx, bin, append([]string{"lsp"}, args...)...)
	cmd.Dir = cwd
	cmd.WaitDelay = 50 * time.Millisecond
	out, err := cmd.Output()
	res := lspResult{}
	if ctx.Err() != nil {
		res.timedOut = true
		return res
	}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			res.failed = true
			return res
		}
		res.exit = ee.ExitCode()
	}
	var doc struct {
		Results []lspResult `json:"results"`
	}
	if json.Unmarshal(out, &doc) != nil || len(doc.Results) == 0 {
		res.failed = true
		return res
	}
	r := doc.Results[0]
	r.exit = res.exit
	return r
}

// startWarm starts `orch lsp warm` detached; a missing verb (exit 2) is fine.
func startWarm(bin, cwd string) {
	cmd := exec.Command(bin, "lsp", "warm", cwd)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

func kindLabel(k string) string {
	switch k {
	case "Function":
		return "func"
	case "Constant":
		return "const"
	case "Variable":
		return "var"
	case "Property":
		return "field"
	}
	return strings.ToLower(k)
}

func verbFor(label string) string {
	switch label {
	case "func", "method":
		return "callers"
	case "interface":
		return "impls"
	}
	return "refs"
}

type pkgCount struct {
	pkg string
	n   int
}

type enrichEntry struct {
	name, kind, sig, doc, verb string
	def                        *lspItem
	defShown                   bool
	defRel                     string
	listed                     []string
	pkgs                       []pkgCount
	hidden, tests              int
	outside                    map[string]int
	others                     []string
	lspCut                     bool
	label                      string
}

func (e *enrichEntry) hasContent() bool {
	return (e.def != nil && !e.defShown) || e.hidden > 0 || e.tests > 0 || len(e.outside) > 0 || len(e.others) > 0
}

func pkgOf(rel string) string {
	d := filepath.Dir(rel)
	if d == "." {
		return "."
	}
	return d
}

func topCounts(m map[string]int, limit int) ([]pkgCount, int) {
	var list []pkgCount
	for k, v := range m {
		list = append(list, pkgCount{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].pkg < list[j].pkg
	})
	extra := 0
	if len(list) > limit {
		extra = len(list) - limit
		list = list[:limit]
	}
	return list, extra
}

func countsText(list []pkgCount) string {
	var parts []string
	for _, p := range list {
		parts = append(parts, fmt.Sprintf("%s %d", p.pkg, p.n))
	}
	return strings.Join(parts, ", ")
}

type enrichCtx struct {
	root string
	sh   shown
	sc   *scope
}

func (x enrichCtx) abs(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(x.root, p)
	}
	return filepath.Clean(p)
}

func (x enrichCtx) rel(p string) string {
	if filepath.IsAbs(p) {
		if r, err := filepath.Rel(x.root, p); err == nil {
			return r
		}
	}
	return p
}

// noun is what the verb's answers are called, in the singular for one.
func noun(verb string, n int) string {
	word := "ref"
	switch verb {
	case "callers":
		word = "caller"
	case "impls":
		word = "implementation"
	}
	if n != 1 {
		word += "s"
	}
	return word
}

// fill sorts the verb answer into not-shown, tests and outside-scope.
func (e *enrichEntry) fill(x enrichCtx, res lspResult) {
	if res.timedOut || res.failed || res.exit != 0 {
		return
	}
	e.lspCut = res.Total > len(res.Items)
	byPkg := map[string]int{}
	for _, it := range res.Items {
		abs := x.abs(it.Path)
		if e.def != nil && abs == x.abs(e.def.Path) && it.Line == e.def.Line {
			continue
		}
		rel := x.rel(abs)
		if !x.sc.contains(abs) {
			e.outside[pkgOf(rel)]++
			continue
		}
		if x.sh.has(abs, it.Line) {
			continue
		}
		e.hidden++
		switch e.verb {
		case "callers":
			if testPath.MatchString(rel) {
				e.tests++
				continue
			}
			s := fmt.Sprintf("%s:%d", rel, it.Line)
			if it.In != "" {
				s += " in " + it.In
			}
			e.listed = append(e.listed, s)
		case "impls":
			name := strings.TrimSpace(it.Text)
			if m := typeDecl.FindStringSubmatch(name); m != nil {
				name = m[1]
			}
			e.listed = append(e.listed, fmt.Sprintf("%s:%d %s", rel, it.Line, name))
		default:
			byPkg[pkgOf(rel)]++
		}
	}
	for k, v := range byPkg {
		e.pkgs = append(e.pkgs, pkgCount{k, v})
	}
	sort.Slice(e.pkgs, func(i, j int) bool {
		if e.pkgs[i].n != e.pkgs[j].n {
			return e.pkgs[i].n > e.pkgs[j].n
		}
		return e.pkgs[i].pkg < e.pkgs[j].pkg
	})
}

func (e *enrichEntry) header() string {
	return fmt.Sprintf("lsp %s (%s): %s", e.name, e.kind, e.sig)
}

func (e *enrichEntry) moreCmd() string {
	verb := e.verb
	if verb == "" {
		verb = "def"
	}
	return fmt.Sprintf("orch lsp %s %s", verb, e.name)
}

// render is the entry's lines under a budget; cut says something was left out.
func (e *enrichEntry) render(maxLines, n, packages int, compact bool) ([]string, bool) {
	cut := e.lspCut
	var lines []string
	lines = append(lines, e.header())
	var body []string
	if e.def != nil && !e.defShown {
		body = append(body, fmt.Sprintf("defined: %s:%d", e.defRel, e.def.Line))
	}
	if compact {
		if e.hidden > 0 {
			body = append(body, fmt.Sprintf("%d %s not shown", e.hidden, noun(e.verb, e.hidden)))
		}
		if e.tests > 0 {
			body = append(body, fmt.Sprintf("%d in tests", e.tests))
		}
		if k := sumCounts(e.outside); k > 0 {
			body = append(body, fmt.Sprintf("+%d outside %s", k, e.label))
		}
		if len(e.others) > 0 {
			body = append(body, fmt.Sprintf("%d same-name", len(e.others)))
		}
		if len(body) > 0 {
			lines = append(lines, "  "+strings.Join(body, "; "))
		}
		return lines, cut
	}
	lines = lines[:1]
	if len(body) > 0 {
		lines = append(lines, "  "+body[0])
	}
	if e.doc != "" {
		lines = append(lines, "  "+e.doc)
	}
	switch e.verb {
	case "callers", "impls":
		if len(e.listed) > 0 {
			shownN := e.listed
			if len(shownN) > n {
				shownN = shownN[:n]
				cut = true
			}
			word := "callers"
			if e.verb == "impls" {
				word = "implementations"
			}
			lines = append(lines, fmt.Sprintf("  %s in scope, not shown above: %s", word, strings.Join(shownN, "; ")))
		}
		if e.tests > 0 {
			lines = append(lines, fmt.Sprintf("  tests: %d %s", e.tests, noun("callers", e.tests)))
		}
	default:
		if len(e.pkgs) > 0 {
			list := e.pkgs
			extra := 0
			if len(list) > packages {
				extra = len(list) - packages
				list = list[:packages]
				cut = true
			}
			text := countsText(list)
			if extra > 0 {
				text += fmt.Sprintf(", +%d packages", extra)
			}
			lines = append(lines, "  refs in scope, not shown above, by package: "+text)
		}
	}
	if k := sumCounts(e.outside); k > 0 {
		top, extra := topCounts(e.outside, 3)
		text := countsText(top)
		if extra > 0 {
			text += fmt.Sprintf(", +%d packages", extra)
			cut = true
		}
		lines = append(lines, fmt.Sprintf("  +%d %s outside %s: %s", k, noun(e.verb, k), e.label, text))
	}
	for i, o := range e.others {
		if i == 3 {
			cut = true
			break
		}
		lines = append(lines, "  same name: "+o)
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		cut = true
	}
	return lines, cut
}

func sumCounts(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}

type enrichLog struct {
	TS       string   `json:"ts"`
	Session  string   `json:"session"`
	Cwd      string   `json:"cwd"`
	Tool     string   `json:"tool"`
	Decision string   `json:"decision"`
	Names    []string `json:"names,omitempty"`
	Lines    int      `json:"lines"`
	Elapsed  int64    `json:"elapsed_ms"`
}

func cacheDir() string {
	if d := os.Getenv("ORCH_CACHE_DIR"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "orchestrate")
}

func (l enrichLog) write() {
	dir := cacheDir()
	if dir == "" || os.MkdirAll(dir, 0o777) != nil {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(l) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "lsp-enrich.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(buf.Bytes())
}

// dirtyFingerprint identifies the checkout's HEAD and uncommitted files; "" when unknown.
var dirtyFingerprint = func(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	git := func(args ...string) []byte {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
		cmd.WaitDelay = 50 * time.Millisecond
		out, _ := cmd.Output()
		return out
	}
	h := sha256.New()
	h.Write(git("rev-parse", "HEAD"))
	status := git("status", "--porcelain", "-z", "--no-renames")
	h.Write(status)
	root := strings.TrimSpace(string(git("rev-parse", "--show-toplevel")))
	for _, ent := range bytes.Split(status, []byte{0}) {
		if len(ent) > 3 && root != "" {
			if info, err := os.Stat(filepath.Join(root, string(ent[3:]))); err == nil {
				fmt.Fprintf(h, "%s %d %d\n", ent[3:], info.Size(), info.ModTime().UnixNano())
			}
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Enrich is the language-server facts to append to a search result, or "".
func Enrich(ev *Event) string {
	if os.Getenv("ORCH_LSP_ENRICH") == "off" {
		return ""
	}
	if name := ev.Str("hook_event_name"); name != "" && name != "PostToolUse" {
		return ""
	}
	start := time.Now()
	cwd := ev.Str("cwd")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	calls := searchCallsOf(ev, cwd)
	if len(calls) == 0 {
		return ""
	}
	session, _ := sessionName(ev)
	var names []string
	logIt := func(decision string, lines int) {
		enrichLog{TS: time.Now().UTC().Format(time.RFC3339Nano), Session: session, Cwd: cwd, Tool: ev.Str("tool_name"),
			Decision: decision, Names: names, Lines: lines, Elapsed: time.Since(start).Milliseconds()}.write()
	}
	out, decision := enrichRun(ev, calls, cwd, start, &names)
	n := 0
	if out != "" {
		n = strings.Count(out, "\n") + 1
	}
	logIt(decision, n)
	return out
}

type enrichTarget struct {
	name string
	call *searchCall
	sc   *scope
	sh   shown
}

func enrichRun(ev *Event, calls []*searchCall, cwd string, start time.Time, namesOut *[]string) (string, string) {
	text := searchResultText(ev)
	var targets []enrichTarget
	reason := "not-code-search"
	for _, c := range calls {
		if !codeSearch(c) {
			continue
		}
		reason = "no-names"
		found := patternNames(c)
		if len(found) == 0 {
			continue
		}
		sc := buildScope(c)
		if sc.bad {
			reason = "unparseable-scope"
			continue
		}
		sh := parseShown(c, text)
		for _, nm := range found {
			dup := false
			for _, t := range targets {
				dup = dup || t.name == nm
			}
			if !dup && len(targets) < 8 {
				targets = append(targets, enrichTarget{nm, c, sc, sh})
			}
		}
	}
	if len(targets) == 0 {
		return "", reason
	}
	for _, t := range targets {
		*namesOut = append(*namesOut, t.name)
	}
	bin := orchBin()
	if bin == "" {
		return "", "no-orch"
	}
	budget := enrichBudgetDefault
	if v, err := strconv.Atoi(os.Getenv("ORCH_LSP_ENRICH_BUDGET_MS")); err == nil && v > 0 {
		budget = time.Duration(v) * time.Millisecond
	}
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(budget))
	defer cancel()

	// Stage 1: is the name a symbol, and which?
	defs := make([]lspResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defs[i] = runLSP(ctx, bin, cwd, "def", t.name, "--json")
		}()
	}
	wg.Wait()
	for _, d := range defs {
		if d.timedOut {
			startWarm(bin, cwd)
			return "", "timeout-warm"
		}
	}
	root := repoOf(cwd)
	type plan struct {
		t         enrichTarget
		entry     *enrichEntry
		qname     string
		wantSig   bool
		verbRes   lspResult
		qdef      lspResult
		hasPrimay bool
	}
	var plans []*plan
	for i, t := range targets {
		d := defs[i]
		if d.failed || d.exit != 0 || len(d.Items) == 0 || (d.Status != "ok" && d.Status != "ambiguous") {
			continue
		}
		x := enrichCtx{root, t.sh, t.sc}
		e := &enrichEntry{name: t.name, outside: map[string]int{}, label: t.sc.label}
		p := &plan{t: t, entry: e}
		switch {
		case d.Status == "ok" || len(d.Items) == 1:
			it := d.Items[0]
			e.def, p.qname = &it, t.name
			e.kind, e.sig, e.doc = kindLabel(it.Kind), strings.TrimSuffix(strings.TrimSpace(it.Text), " {"), it.Doc
			p.hasPrimay = true
		case len(d.Items) > 10:
			continue
		default:
			var inScope []lspItem
			for _, it := range d.Items {
				if t.sc.contains(x.abs(it.Path)) {
					inScope = append(inScope, it)
				}
			}
			e.kind, e.sig = "ambiguous", fmt.Sprintf("%d symbols named %s", len(d.Items), t.name)
			if len(inScope) == 1 && !strings.Contains(t.name, ".") {
				it := inScope[0]
				e.def = &it
				e.kind = kindLabel(it.Kind)
				p.qname = filepath.Base(filepath.Dir(it.Path)) + "." + t.name
				p.wantSig, p.hasPrimay = true, true
			}
			for _, it := range d.Items {
				abs := x.abs(it.Path)
				if e.def != nil && it.Path == e.def.Path && it.Line == e.def.Line {
					continue
				}
				if t.sh.has(abs, it.Line) {
					continue
				}
				pkg := filepath.Base(filepath.Dir(it.Path))
				e.others = append(e.others, fmt.Sprintf("%s.%s (%s) %s:%d", pkg, t.name, kindLabel(it.Kind), x.rel(abs), it.Line))
			}
		}
		if e.def != nil {
			e.defRel = x.rel(x.abs(e.def.Path))
			e.defShown = t.sh.has(x.abs(e.def.Path), e.def.Line)
			e.verb = verbFor(e.kind)
		}
		plans = append(plans, p)
	}
	// Stage 2: the facts of the verb, and the signature of a name picked out of several.
	for _, p := range plans {
		if !p.hasPrimay {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			args := []string{p.entry.verb, p.qname, "--json", "--limit", "100"}
			p.verbRes = runLSP(ctx, bin, cwd, args...)
		}()
		if p.wantSig {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.qdef = runLSP(ctx, bin, cwd, "def", p.qname, "--json")
			}()
		}
	}
	wg.Wait()
	var entries []*enrichEntry
	for _, p := range plans {
		e := p.entry
		x := enrichCtx{root, p.t.sh, p.t.sc}
		if p.wantSig && !p.qdef.timedOut && !p.qdef.failed && p.qdef.Status == "ok" && len(p.qdef.Items) > 0 {
			e.sig = strings.TrimSuffix(strings.TrimSpace(p.qdef.Items[0].Text), " {")
			e.doc = p.qdef.Items[0].Doc
		}
		if p.hasPrimay {
			e.fill(x, p.verbRes)
		}
		if e.sig == "" && e.def != nil {
			e.sig = fmt.Sprintf("%s at %s:%d", e.kind, e.defRel, e.def.Line)
		}
		if e.hasContent() {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return "", "nothing-new"
	}
	entries = dedupeEntries(ev, cwd, entries)
	if len(entries) == 0 {
		return "", "deduped"
	}
	return renderEntries(entries), "fired"
}

// dedupeEntries drops names already enriched in this session while the dirty files are unchanged.
func dedupeEntries(ev *Event, cwd string, entries []*enrichEntry) []*enrichEntry {
	fp := dirtyFingerprint(cwd)
	_, name := sessionName(ev)
	dir := StateDir()
	if os.MkdirAll(filepath.Join(dir, "coach"), 0o777) != nil {
		return entries
	}
	f, st, err := lockState(dir, name)
	if err != nil {
		return entries
	}
	defer f.Close()
	if st.Enriched == nil {
		st.Enriched = map[string]string{}
	}
	var out []*enrichEntry
	for _, e := range entries {
		if prev, ok := st.Enriched[e.name]; ok && prev == fp {
			continue
		}
		st.Enriched[e.name] = fp
		out = append(out, e)
	}
	_ = saveState(f, st)
	return out
}

func renderEntries(entries []*enrichEntry) string {
	maxLines, n, p, compact := 12, 8, 5, false
	switch {
	case len(entries) == 2:
		maxLines, n, p = 5, 3, 3
	case len(entries) > 2:
		maxLines, n, p, compact = 2, 3, 3, true
	}
	var rest []string
	if len(entries) > 4 {
		for _, e := range entries[4:] {
			rest = append(rest, e.name)
		}
		entries = entries[:4]
	}
	var lines, more []string
	for _, e := range entries {
		ls, cut := e.render(maxLines, n, p, compact)
		lines = append(lines, ls...)
		if cut {
			more = append(more, e.moreCmd())
		}
	}
	if len(rest) > 0 {
		lines = append(lines, fmt.Sprintf("+%d names: orch lsp def %s", len(rest), strings.Join(rest, " ")))
	}
	if len(more) > 0 {
		lines = append(lines, "more: "+strings.Join(more, "; "))
	}
	return strings.Join(lines, "\n")
}

// writeContext writes additionalContext as PostToolUse hook JSON.
func writeContext(stdout io.Writer, eventName, text string) {
	fmt.Fprintf(stdout, `{"hookSpecificOutput": {"hookEventName": %s, "additionalContext": %s}}`, pyQuote(eventName), pyQuote(text))
}

// EnrichOnly handles `orch-guard enrich`: one PostToolUse event in, the enrichment (if any) out.
func EnrichOnly(ev *Event, stdout io.Writer) {
	if text := Enrich(ev); text != "" {
		name := ev.Str("hook_event_name")
		if name == "" {
			name = "PostToolUse"
		}
		writeContext(stdout, name, text)
	}
}

// searchResultText is the output the agent saw. The Grep tool answers with an
// object: its file names, or its content.
func searchResultText(ev *Event) string {
	if ev.Str("tool_name") == "Grep" {
		if resp, ok := ev.Object("tool_response"); ok {
			if names, ok := resp["filenames"].([]any); ok {
				var lines []string
				for _, n := range names {
					if s, ok := n.(string); ok {
						lines = append(lines, s)
					}
				}
				return strings.Join(lines, "\n")
			}
			if content, ok := resp["content"].(string); ok {
				return content
			}
		}
	}
	return resultText(ev)
}
