package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/BurntSushi/toml"
)

// The language registry: the one table every language-specific rule of
// `orch ctx` and the brief reads. A language is its extensions, how a symbol
// is declared (to read the enclosing declaration of a line and to verify that a
// name is a symbol at all), its comment markers, test and generated file
// patterns, entry points that never count as look-alikes, what "exported"
// means, and optionally a language server. A user can add or override entries
// with [[ctx_langs]] in the user config.

type lspCmd struct {
	Bin  string
	Args []string
}

// scanRule reads one declaration from the start of a line. Re has the named
// groups `name` (and `kind` unless Kind is set; `recv` for a method's type).
// Top rules count as an enclosing declaration only at indent 0 (a local
// variable is not what encloses a line).
type scanRule struct {
	Re   string
	Kind string
	Top  bool
	re   *regexp.Regexp
}

type langDef struct {
	ID    string
	Exts  []string
	Scan  []scanRule
	Decl  []string // declaration forms of a symbol {sym}, as regular expressions (rg and Go syntax)
	Doc   []string // comment line prefixes
	Test  []string // test-file path patterns
	Gen   []string // generated-file path patterns
	Entry []string // base names of program entry points
	// Export is "all", "upper" (initial capital), "noscore" (no leading underscore),
	// "nonprivate" (no private keyword) or "keyword:<word>" (the line starts with it).
	Export string
	// BraceEnd means a closing bracket in column 0 ends the declaration above it.
	BraceEnd bool
	// Script marks a language of scripts and tools, which are not look-alikes unless the work is in it.
	Script bool
	// Manifests are the file names (globs) that mark a project root of this language.
	Manifests []string

	LangID    string   // the LSP languageId
	LSP       []lspCmd // servers to look for, first found wins
	LSPEnv    string   // environment variable that overrides the server command
	LocalDirs []string // project directories searched for the server before PATH
	Hint      string   // how to install a server
	Custom    bool     // the user configured this language's server commands: no dedicated adapter (see serverFor)

	tests, gens []*regexp.Regexp
	decls       []string
}

var (
	langMu    sync.RWMutex
	langTable []*langDef
	langByExt map[string]*langDef
)

func init() { setLangs(builtinLangs()) }

func sc(re string, top bool) scanRule { return scanRule{Re: re, Top: top} }

func builtinLangs() []*langDef {
	return []*langDef{
		{ID: "go", Exts: []string{".go"}, Manifests: []string{"go.mod", "go.work"}, LangID: "go", Export: "upper", BraceEnd: true, Doc: []string{"//"},
			Scan: []scanRule{
				{Re: `^func\s+\(\s*\w*\s*\*?\s*(?P<recv>[\w.]+)(?:\[[^\]]*\])?\s*\)\s*(?P<name>\w+)`, Kind: "method", Top: true},
				{Re: `^func\s+(?P<name>\w+)`, Kind: "func", Top: true},
				{Re: `^type\s+(?P<name>\w+)`, Kind: "type", Top: true},
				{Re: `^(?P<kind>var|const)\s+(?P<name>\w+)`, Top: true},
			},
			Decl:  []string{`^func\s+{sym}\b`, `^func\s+\([^)]*\)\s*{sym}\s*[\[(]`, `^type\s+{sym}\b`, `^\s*(var|const)\s+{sym}\b`, `^\s*{sym}\s*:=`},
			Test:  []string{`_test\.go$`},
			Gen:   []string{`\.pb\.go$`, `_gen\.go$`, `\.gen\.go$`, `\.sql\.go$`, `_generated\.go$`},
			Entry: []string{"main.go"},
			Hint:  "go install golang.org/x/tools/gopls@latest"},
		{ID: "typescript", Exts: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"}, Manifests: []string{"package.json", "tsconfig.json", "jsconfig.json", "pnpm-workspace.yaml", "deno.json", "deno.jsonc"}, LangID: "typescript", Export: "keyword:export", BraceEnd: true, Doc: []string{"//", "/**", "*"},
			Scan: []scanRule{
				sc(`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?(?:async\s+)?(?P<kind>function|class|interface|type|enum)\*?\s+(?P<name>\w+)`, false),
				sc(`^(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?P<kind>const|let|var)\s+(?P<name>\w+)\s*[:=]`, true),
			},
			Decl: []string{`^\s*(export\s+)?(default\s+)?(declare\s+)?(abstract\s+)?(async\s+)?(function\*?|class|interface|type|enum)\s+{sym}\b`,
				`^\s*(export\s+)?(const|let|var)\s+{sym}\b`, `^\s*export\s+default\s+{sym}\b`,
				`^\s+(public\s+|private\s+|protected\s+|static\s+|async\s+|get\s+|set\s+|readonly\s+)*{sym}\s*\([^)]*\)\s*(:[^{]+)?\{`},
			Test:      []string{`\.(test|spec)\.[cm]?[jt]sx?$`, `(^|/)__tests__/`},
			Gen:       []string{`\.d\.ts$`, `\.min\.js$`, `\.min\.css$`, `\.generated\.`},
			Entry:     []string{"index.ts", "index.tsx", "index.js", "index.jsx", "main.ts", "main.tsx", "main.js"},
			LocalDirs: []string{"node_modules/.bin"}, Hint: "npm i -D typescript@7 (the native server), or set ORCH_TSLS"},
		{ID: "python", Exts: []string{".py", ".pyi"}, Manifests: []string{"pyproject.toml", "pyrefly.toml", "setup.py", "setup.cfg", "requirements*.txt", "Pipfile"}, LangID: "python", Export: "noscore", Doc: []string{"#"},
			Scan: []scanRule{
				sc(`^\s*(?:async\s+)?(?P<kind>def|class)\s+(?P<name>\w+)`, false),
				{Re: `^(?P<name>[A-Za-z_]\w*)\s*(?::[^=]+)?=[^=]`, Kind: "var", Top: true},
			},
			Decl:  []string{`^\s*(async\s+)?def\s+{sym}\s*\(`, `^\s*class\s+{sym}\b`, `^{sym}\s*(:[^=]+)?=[^=]`, `^ {4}{sym}\s*(:[^=]+)?=[^=]`, `^\t{sym}\s*(:[^=]+)?=[^=]`},
			Test:  []string{`(^|/)test_[^/]*\.py$`, `_test\.py$`, `(^|/)conftest\.py$`, `(^|/)tests?/`},
			Gen:   []string{`_pb2(_grpc)?\.pyi?$`, `(^|/)\.venv/`, `(^|/)venv/`, `site-packages/`, `\.egg-info/`, `__pycache__/`},
			Entry: []string{"__main__.py", "manage.py", "wsgi.py", "asgi.py", "setup.py"},
			LSP:   []lspCmd{{"pyrefly", []string{"lsp", "--indexing-mode", "lazy-blocking"}}, {"basedpyright-langserver", []string{"--stdio"}}, {"pyright-langserver", []string{"--stdio"}}, {"ty", []string{"server"}}, {"pylsp", nil}}, LSPEnv: "ORCH_PYLS", LocalDirs: []string{".venv/bin", "venv/bin"},
			Hint: "uv tool install pyrefly  (or: pip install pyrefly, uvx pyrefly; fallbacks: npm i -g basedpyright, npm i -g pyright, uv tool install ty)"},
		{ID: "rust", Exts: []string{".rs"}, Manifests: []string{"Cargo.toml"}, LangID: "rust", Export: "keyword:pub", BraceEnd: true, Doc: []string{"///", "//!", "//"},
			Scan: []scanRule{
				sc(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?(?P<kind>fn|struct|enum|trait|type|const|static|mod|union)\s+(?P<name>\w+)`, false),
				{Re: `^\s*impl(?:<[^>]*>)?\s+(?:[\w:<>]+\s+for\s+)?(?P<name>[\w:]+)`, Kind: "impl"},
			},
			Decl:  []string{`^\s*(pub(\([^)]*\))?\s+)?(async\s+)?(unsafe\s+)?(fn|struct|enum|trait|type|const|static|mod|union)\s+{sym}\b`, `^\s*impl(<[^>]*>)?\s+.*\b{sym}\b`},
			Test:  []string{`(^|/)tests/`, `_test\.rs$`},
			Gen:   []string{`(^|/)target/`},
			Entry: []string{"main.rs"},
			LSP:   []lspCmd{{"rust-analyzer", nil}}, Hint: "rustup component add rust-analyzer"},
		{ID: "java", Exts: []string{".java"}, Manifests: []string{"pom.xml", "build.gradle", "build.gradle.kts"}, LangID: "java", Export: "keyword:public", BraceEnd: true, Doc: []string{"//", "/**", "*"},
			Scan: []scanRule{
				sc(`^\s*(?:(?:public|private|protected|static|final|abstract|sealed)\s+)*(?P<kind>class|interface|enum|record)\s+(?P<name>\w+)`, false),
				{Re: `^\s*(?:(?:public|private|protected|static|final|abstract|synchronized|default)\s+)+[\w<>\[\],.?]+\s+(?P<name>\w+)\s*\(`, Kind: "method"},
			},
			Decl: []string{`^\s*((public|private|protected|static|final|abstract|sealed)\s+)*(class|interface|enum|record)\s+{sym}\b`,
				`^\s*((public|private|protected|static|final|abstract|synchronized|default)\s+)+[\w<>\[\],.?]+\s+{sym}\s*\(`},
			Test:  []string{`Tests?\.java$`, `(^|/)src/test/`},
			Gen:   []string{`(^|/)(build|target)/`, `generated-sources/`},
			Entry: []string{"Main.java", "Application.java"},
			LSP:   []lspCmd{{"jdtls", nil}}, Hint: "install Eclipse jdtls (brew install jdtls)"},
		{ID: "kotlin", Exts: []string{".kt", ".kts"}, Manifests: []string{"build.gradle", "build.gradle.kts", "pom.xml"}, LangID: "kotlin", Export: "nonprivate", BraceEnd: true, Doc: []string{"//", "/**", "*"},
			Scan: []scanRule{
				sc(`^\s*(?:(?:public|private|internal|protected|open|abstract|data|sealed|suspend|override|inline)\s+)*(?P<kind>class|interface|object|fun|val|var|typealias)\s+(?:<[^>]*>\s*)?(?:[\w.<>]+\.)?(?P<name>\w+)`, false),
			},
			Decl:  []string{`^\s*((public|private|internal|protected|open|abstract|data|sealed|suspend|override|inline)\s+)*(class|interface|object|fun|val|var|typealias)\s+(<[^>]*>\s*)?([\w.<>]+\.)?{sym}\b`},
			Test:  []string{`Tests?\.kt$`, `(^|/)src/test/`},
			Gen:   []string{`(^|/)(build|target)/`},
			Entry: []string{"Main.kt"},
			LSP:   []lspCmd{{"kotlin-language-server", nil}}, Hint: "brew install kotlin-language-server"},
		{ID: "cpp", Exts: []string{".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp"}, Manifests: []string{"CMakeLists.txt", "Makefile", "meson.build"}, LangID: "cpp", Export: "all", BraceEnd: true, Doc: []string{"//", "/**", "*"},
			Scan: []scanRule{
				sc(`^\s*(?P<kind>class|struct|enum|union|namespace)\s+(?P<name>\w+)`, false),
				{Re: `^\s*#define\s+(?P<name>\w+)`, Kind: "macro", Top: true},
				{Re: `^[A-Za-z_][\w:<>\s\*&]*?[\s\*&](?P<name>\w+)\s*\([^;]*\)\s*(?:const\s*)?\{?\s*$`, Kind: "func", Top: true},
			},
			Decl:  []string{`^\s*(class|struct|enum|union|namespace)\s+{sym}\b`, `^\s*#define\s+{sym}\b`, `^[A-Za-z_][\w:<>\s\*&]*[\s\*&]{sym}\s*\([^;]*$`},
			Test:  []string{`_test\.(c|cc|cpp|cxx)$`, `(^|/)tests?/`},
			Gen:   []string{`(^|/)(build|cmake-build[^/]*)/`},
			Entry: []string{"main.c", "main.cc", "main.cpp"},
			LSP:   []lspCmd{{"clangd", nil}}, Hint: "brew install llvm (clangd) or apt install clangd"},
		{ID: "ruby", Exts: []string{".rb", ".rake"}, Manifests: []string{"Gemfile"}, LangID: "ruby", Export: "all", Doc: []string{"#"},
			Scan:  []scanRule{sc(`^\s*(?P<kind>def|class|module)\s+(?:self\.)?(?P<name>\w+[?!=]?)`, false)},
			Decl:  []string{`^\s*def\s+(self\.)?{sym}\b`, `^\s*(class|module)\s+{sym}\b`, `^\s*{sym}\s*=[^=]`},
			Test:  []string{`_(spec|test)\.rb$`, `(^|/)(spec|test)/`},
			Entry: []string{"Rakefile", "config.ru"},
			LSP:   []lspCmd{{"solargraph", []string{"stdio"}}}, Hint: "gem install solargraph"},
		{ID: "php", Exts: []string{".php"}, Manifests: []string{"composer.json"}, LangID: "php", Export: "nonprivate", BraceEnd: true, Doc: []string{"//", "#", "/**", "*"},
			Scan:  []scanRule{sc(`^\s*(?:(?:abstract|final|public|private|protected|static)\s+)*(?P<kind>class|interface|trait|enum|function)\s+(?P<name>\w+)`, false)},
			Decl:  []string{`^\s*((abstract|final)\s+)?(class|interface|trait|enum)\s+{sym}\b`, `^\s*((public|private|protected|static)\s+)*function\s+{sym}\s*\(`, `^\s*const\s+{sym}\b`},
			Test:  []string{`Test\.php$`, `(^|/)tests?/`},
			Gen:   []string{`(^|/)vendor/`},
			Entry: []string{"index.php"},
			LSP:   []lspCmd{{"intelephense", []string{"--stdio"}}}, Hint: "npm i -g intelephense"},
		{ID: "shell", Script: true, Exts: []string{".sh", ".bash", ".zsh"}, LangID: "shellscript", Export: "all", Doc: []string{"#"},
			Scan: []scanRule{
				{Re: `^\s*(?:function\s+)?(?P<name>[A-Za-z_][\w-]*)\s*\(\s*\)`, Kind: "func"},
				{Re: `^\s*function\s+(?P<name>[\w-]+)`, Kind: "func"},
			},
			Decl: []string{`^\s*(function\s+)?{sym}\s*\(\s*\)`, `^\s*function\s+{sym}\b`, `^\s*(export\s+)?{sym}=`},
			Test: []string{`\.bats$`, `(^|/)tests?/`},
			LSP:  []lspCmd{{"bash-language-server", []string{"start"}}}, Hint: "npm i -g bash-language-server"},
	}
}

func setLangs(ls []*langDef) {
	langMu.Lock()
	defer langMu.Unlock()
	langTable = ls
	langByExt = map[string]*langDef{}
	for _, l := range ls {
		for _, p := range l.Test {
			l.tests = append(l.tests, regexp.MustCompile(p))
		}
		for _, p := range l.Gen {
			l.gens = append(l.gens, regexp.MustCompile(p))
		}
		for i := range l.Scan {
			l.Scan[i].re = regexp.MustCompile(l.Scan[i].Re)
		}
		for _, e := range l.Exts {
			langByExt[e] = l
		}
	}
}

// langCfg is one [[ctx_langs]] entry of the user config. A known id overrides
// the fields that are set; a new id adds a language.
type langCfg struct {
	ID        string   `toml:"id"`
	Exts      []string `toml:"extensions"`
	Decl      []string `toml:"decl"`
	Doc       []string `toml:"doc"`
	Test      []string `toml:"test"`
	Generated []string `toml:"generated"`
	Entry     []string `toml:"entry"`
	Export    string   `toml:"export"`
	LSPBin    string   `toml:"lsp_bin"`
	LSPArgs   []string `toml:"lsp_args"`
	LSPHint   string   `toml:"lsp_hint"`
	// LSPFallbacks are further servers to try, as "command arg arg" strings.
	LSPFallbacks []string `toml:"lsp_fallbacks"`
	Manifests    []string `toml:"manifests"`
}

// applyCtxLangs rebuilds the table from the built-in languages and the user's
// entries.
func applyCtxLangs(cfg []langCfg) {
	ls := builtinLangs()
	for _, c := range cfg {
		if c.ID == "" {
			continue
		}
		var l *langDef
		for _, x := range ls {
			if x.ID == c.ID {
				l = x
			}
		}
		if l == nil {
			l = &langDef{ID: c.ID, LangID: c.ID, Export: "all"}
			ls = append(ls, l)
		}
		if len(c.Exts) > 0 {
			l.Exts = c.Exts
		}
		if len(c.Decl) > 0 {
			l.Decl = c.Decl
		}
		if len(c.Doc) > 0 {
			l.Doc = c.Doc
		}
		if len(c.Test) > 0 {
			l.Test = c.Test
		}
		if len(c.Generated) > 0 {
			l.Gen = c.Generated
		}
		if len(c.Entry) > 0 {
			l.Entry = c.Entry
		}
		if c.Export != "" {
			l.Export = c.Export
		}
		if c.LSPBin != "" {
			l.LSP = []lspCmd{{c.LSPBin, c.LSPArgs}}
			l.Custom = true
		}
		for _, fb := range c.LSPFallbacks {
			if f := strings.Fields(fb); len(f) > 0 {
				l.LSP = append(l.LSP, lspCmd{f[0], f[1:]})
			}
		}
		if len(c.Manifests) > 0 {
			l.Manifests = c.Manifests
		}
		if c.LSPHint != "" {
			l.Hint = c.LSPHint
		}
	}
	// A bad pattern from the user must not take the tool down: drop it.
	for _, l := range ls {
		l.Test = validPatterns(l.Test)
		l.Gen = validPatterns(l.Gen)
		l.Decl = validDecls(l.Decl)
	}
	setLangs(ls)
}

func validPatterns(ps []string) []string {
	var out []string
	for _, p := range ps {
		if _, err := regexp.Compile(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func validDecls(ps []string) []string {
	var out []string
	for _, p := range ps {
		if _, err := regexp.Compile(strings.ReplaceAll(p, "{sym}", "X")); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// langOf is the language of a path by its extension, nil for none.
func langOf(path string) *langDef {
	langMu.RLock()
	defer langMu.RUnlock()
	return langByExt[strings.ToLower(filepath.Ext(path))]
}

func langByID(id string) *langDef {
	langMu.RLock()
	defer langMu.RUnlock()
	for _, l := range langTable {
		if l.ID == id {
			return l
		}
	}
	return nil
}

func allLangs() []*langDef {
	langMu.RLock()
	defer langMu.RUnlock()
	return append([]*langDef{}, langTable...)
}

// isCodePath is a path in a registered language.
func isCodePath(p string) bool { return langOf(p) != nil }

var testDirs = map[string]bool{"__tests__": true, "testdata": true, "fixtures": true, "__fixtures__": true}

// isTestPath is the one matcher for test files: the language's own patterns,
// *.test.* and *.spec.* files, and anything under __tests__, testdata or
// fixtures.
func isTestPath(p string) bool {
	p = filepath.ToSlash(p)
	base := p[strings.LastIndex(p, "/")+1:]
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if testDirs[seg] {
			return true
		}
	}
	if l := langOf(p); l != nil {
		for _, re := range l.tests {
			if re.MatchString(p) {
				return true
			}
		}
	}
	return false
}

var genUniversal = []string{".generated.", "/generated/", "/gen/", "openapi", "swagger", "/vendor/", "/node_modules/", "/dist/", ".min."}

func isGeneratedPath(p string) bool {
	p = filepath.ToSlash(p)
	for _, s := range genUniversal {
		if strings.Contains("/"+p, s) {
			return true
		}
	}
	if strings.HasPrefix(p, "vendor/") || strings.HasPrefix(p, "dist/") {
		return true
	}
	if l := langOf(p); l != nil {
		for _, re := range l.gens {
			if re.MatchString(p) {
				return true
			}
		}
	}
	return false
}

var docExts = map[string]bool{".md": true, ".mdx": true, ".rst": true, ".txt": true, ".adoc": true}

func isDocPath(p string) bool {
	p = filepath.ToSlash(p)
	return docExts[strings.ToLower(filepath.Ext(p))] || strings.HasPrefix(p, "docs/") || strings.HasPrefix(p, "doc/")
}

// isEntryPoint is a program's entry file: every query matches it, so it is
// never a look-alike.
func isEntryPoint(p string) bool {
	l := langOf(p)
	if l == nil {
		return false
	}
	base := filepath.Base(p)
	return slices.Contains(l.Entry, base)
}

// lookAlikePath is a file a look-alike may come from: code, not a test, not
// generated, not an entry point, and not a script unless the work (ownLangs)
// is in that language.
func lookAlikePath(p string, ownLangs map[string]bool) bool {
	l := langOf(p)
	return l != nil && !isTestPath(p) && !isGeneratedPath(p) && !isEntryPoint(p) && (!l.Script || ownLangs[l.ID])
}

// declOf reads a declaration from the start of a line of a file of language
// ext: its kind (func, method, type, class, def, ...) and name. A method is
// Receiver.Name.
func declOf(line, ext string) (kind, name string) {
	l := langOf("x" + ext)
	if l == nil {
		return "", ""
	}
	k, n, _ := l.declOfLine(line)
	return k, n
}

func (l *langDef) declOfLine(line string) (kind, name string, top bool) {
	for _, r := range l.Scan {
		m := r.re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		get := func(g string) string {
			for i, n := range r.re.SubexpNames() {
				if n == g {
					return m[i]
				}
			}
			return ""
		}
		kind = r.Kind
		if kind == "" {
			kind = get("kind")
		}
		name = get("name")
		if recv := get("recv"); recv != "" {
			name = strings.TrimPrefix(recv, "*") + "." + name
		}
		if name == "" {
			continue
		}
		return kind, name, r.Top
	}
	return "", "", false
}

// isExported applies the language's export rule to a declaration line.
func (l *langDef) isExported(line, name string) bool {
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	switch {
	case l.Export == "upper":
		r := []rune(name)
		return len(r) > 0 && unicode.IsUpper(r[0])
	case l.Export == "noscore":
		return !strings.HasPrefix(name, "_")
	case l.Export == "nonprivate":
		return !strings.Contains(line, "private ")
	case strings.HasPrefix(l.Export, "keyword:"):
		return strings.HasPrefix(strings.TrimSpace(line), strings.TrimPrefix(l.Export, "keyword:"))
	}
	return true
}

func isCommentLine(l *langDef, t string) bool {
	if l == nil {
		return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#")
	}
	for _, d := range l.Doc {
		if strings.HasPrefix(t, d) {
			return true
		}
	}
	return strings.HasSuffix(t, "*/") && l.hasDoc("*")
}

func (l *langDef) hasDoc(m string) bool {
	return slices.Contains(l.Doc, m)
}

// ---- detection from the tracked files ----

var trackedCache sync.Map // repo -> []string

// trackedFiles lists the repo's files: `git ls-files`, or a bounded walk of
// the directory when it is not a git checkout.
func trackedFiles(code string) []string {
	if v, ok := trackedCache.Load(code); ok {
		return v.([]string)
	}
	var files []string
	if out, err := git(code, "ls-files", "--cached", "--others", "--exclude-standard"); err == nil && out != "" {
		files = strings.Split(out, "\n")
	} else {
		_ = filepath.WalkDir(code, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return filepath.SkipDir
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "vendor", "target", "dist", ".venv", "__pycache__":
					return filepath.SkipDir
				}
				return nil
			}
			if rel, err := filepath.Rel(code, p); err == nil && len(files) < 20000 {
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	trackedCache.Store(code, files)
	return files
}

// langCounts counts the code files (not tests, not generated) per language.
func langCounts(files []string) map[string]int {
	out := map[string]int{}
	for _, f := range files {
		if l := langOf(f); l != nil && !isTestPath(f) && !isGeneratedPath(f) {
			out[l.ID]++
		}
	}
	return out
}

// detectedLangs are the languages of the repo with their code-file counts,
// biggest first.
func detectedLangs(code string) []langCount {
	var out []langCount
	for id, n := range langCounts(trackedFiles(code)) {
		out = append(out, langCount{id, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].ID < out[j].ID
	})
	return out
}

type langCount struct {
	ID string
	N  int
}

func repoHasLang(code, id string) bool {
	for _, lc := range detectedLangs(code) {
		if lc.ID == id {
			return true
		}
	}
	return false
}

// ownsLangs are the languages of the files under the owns, in count order.
func ownsLangs(code string, owns []string) []string {
	count := map[string]int{}
	for _, f := range trackedFiles(code) {
		if l := langOf(f); l != nil && inOwns(f, owns) {
			count[l.ID]++
		}
	}
	for _, o := range owns { // an owned file that is not tracked yet
		if l := langOf(o); l != nil && count[l.ID] == 0 {
			count[l.ID]++
		}
	}
	var ids []string
	for id := range count {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if count[ids[i]] != count[ids[j]] {
			return count[ids[i]] > count[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

// ---- project roots ----

// projectRoot is a directory with a project manifest: the root of a Go
// module, an npm/TypeScript project, a Python project, a Cargo crate, and so on.
// Nested roots (a monorepo) are separate roots. It is where a language server
// is started and what a concept search is narrowed to.
type projectRoot struct {
	Dir       string   // "" for the repo root, else "a/b/"
	Langs     []string // languages with code files in the root
	Manifests []string
	Files     int // code files whose deepest root this is
}

func manifestMatches(l *langDef, base string) bool {
	for _, m := range l.Manifests {
		if ok, _ := filepath.Match(m, base); ok {
			return true
		}
	}
	return false
}

var rootsCache sync.Map // repo -> []projectRoot

type rootAcc struct {
	langs     map[string]bool
	manifests []string
}

// projectRoots finds the roots from the tracked manifests. A file belongs to
// the deepest root that has its language.
func projectRoots(code string) []projectRoot {
	if v, ok := rootsCache.Load(code); ok {
		return v.([]projectRoot)
	}
	files := trackedFiles(code)
	by := map[string]*rootAcc{}
	for _, f := range files {
		base := filepath.Base(f)
		for _, l := range allLangs() {
			if !manifestMatches(l, base) {
				continue
			}
			dir := ""
			if d := filepath.ToSlash(filepath.Dir(f)); d != "." {
				dir = d + "/"
			}
			a := by[dir]
			if a == nil {
				a = &rootAcc{langs: map[string]bool{}}
				by[dir] = a
			}
			a.langs[l.ID] = true
			a.manifests = append(a.manifests, base)
		}
	}
	foldWorkspaces(code, files, by)
	var dirs []string
	for d := range by {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) }) // deepest first
	roots := map[string]*projectRoot{}
	for _, d := range dirs {
		roots[d] = &projectRoot{Dir: d, Manifests: by[d].manifests}
	}
	langsIn := map[string]map[string]bool{}
	for _, f := range files {
		l := langOf(f)
		if l == nil || isTestPath(f) || isGeneratedPath(f) || isDocPath(f) {
			continue
		}
		for _, d := range dirs {
			if (d == "" || strings.HasPrefix(f, d)) && by[d].langs[l.ID] {
				roots[d].Files++
				if langsIn[d] == nil {
					langsIn[d] = map[string]bool{}
				}
				langsIn[d][l.ID] = true
				break
			}
		}
	}
	var out []projectRoot
	for _, d := range dirs {
		r := roots[d]
		if r.Files == 0 {
			continue
		}
		for id := range langsIn[d] {
			r.Langs = append(r.Langs, id)
		}
		sort.Strings(r.Langs)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Dir < out[j].Dir
	})
	rootsCache.Store(code, out)
	return out
}

// rootFor is the deepest root that holds path and has language lang ("" for
// any language); false when there is none.
func rootFor(code, path, lang string) (projectRoot, bool) {
	var best projectRoot
	found := false
	for _, r := range projectRoots(code) {
		if r.Dir != "" && !symPathWithin(path, r.Dir) {
			continue
		}
		ok := lang == ""
		for _, id := range r.Langs {
			ok = ok || id == lang
		}
		if ok && (!found || len(r.Dir) > len(best.Dir)) {
			best, found = r, true
		}
	}
	return best, found
}

// rootsOf are the roots with language lang, biggest first.
func rootsOf(code, lang string) []projectRoot {
	var out []projectRoot
	for _, r := range projectRoots(code) {
		for _, id := range r.Langs {
			if id == lang {
				out = append(out, r)
			}
		}
	}
	return out
}

// codeTreesFor are the trees concept searches are narrowed to: the project
// roots, those the package owns files in first and then by size, at most three.
// Code under no manifest falls back to its top directories by file count. "" is
// the repo root (no narrowing).
func codeTreesFor(code string, owns []string) []string {
	type cand struct {
		dir  string
		n    int
		mine bool
	}
	var cands []cand
	seen := map[string]bool{}
	for _, r := range projectRoots(code) {
		mine := false
		for _, o := range owns {
			p := strings.TrimPrefix(strings.TrimSpace(o), "./")
			if strings.HasSuffix(p, "/") || !strings.Contains(filepath.Base(p), ".") {
				p = strings.TrimSuffix(p, "/") + "/_"
			}
			if rr, ok := rootFor(code, p, ""); ok && rr.Dir == r.Dir {
				mine = true
			}
		}
		if !seen[r.Dir] {
			seen[r.Dir] = true
			cands = append(cands, cand{r.Dir, r.Files, mine})
		}
	}
	// Code under no manifest: its top directories.
	top := map[string]int{}
	for _, f := range trackedFiles(code) {
		l := langOf(f)
		if l == nil || isTestPath(f) || isGeneratedPath(f) || isDocPath(f) {
			continue
		}
		if _, ok := rootFor(code, f, l.ID); ok {
			continue
		}
		d, _, ok := strings.Cut(f, "/")
		if !ok {
			top[""]++
		} else {
			top[d+"/"]++
		}
	}
	for d, n := range top {
		if !seen[d] {
			seen[d] = true
			cands = append(cands, cand{d, n, inOwnsDir(owns, d)})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.mine != b.mine {
			return a.mine
		}
		if a.n != b.n {
			return a.n > b.n
		}
		return a.dir < b.dir
	})
	var out []string
	for _, c := range cands[:min(3, len(cands))] {
		out = append(out, c.dir)
	}
	return out
}

func inOwnsDir(owns []string, dir string) bool {
	for _, o := range owns {
		if strings.HasPrefix(strings.TrimPrefix(strings.TrimSpace(o), "./"), dir) {
			return true
		}
	}
	return false
}

// ---- servers ----

const configuredServerNotUsable = "configured language server not usable: "

// langServer finds a language server for l in the checkout: the override
// variable (a command line), then the project's own directories, then PATH.
// The returned bin is the name or path the server was found by, not its
// symlink-resolved target: startLSP resolves and re-checks it before running
// and hands it to the server as argv[0].
func langServer(code string, l *langDef) (bin string, args []string, ok bool) {
	bin, args, ok, _ = langServerReason(code, l)
	return
}

func langServerReason(code string, l *langDef) (bin string, args []string, ok bool, why string) {
	if l.LSPEnv != "" {
		if v := strings.Fields(os.Getenv(l.LSPEnv)); len(v) > 0 {
			if _, err := ctxServerBinary(code, v[0]); err != nil {
				if ctxPathInRoot(code, v[0]) {
					return "", nil, false, ctxServerSkip(l.ID)
				}
				return "", nil, false, configuredServerNotUsable + err.Error()
			}
			return v[0], v[1:], true, ""
		}
	}
	for _, c := range l.LSP {
		for _, d := range l.LocalDirs {
			if p := filepath.Join(code, d, c.Bin); fileExists(p) {
				if !ctxRepoTrusted(code) {
					why = ctxServerSkip(l.ID)
					continue
				}
				if _, err := ctxServerBinary(code, p); err == nil {
					return p, c.Args, true, why
				}
			}
		}
		if p, err := exec.LookPath(c.Bin); err == nil {
			if _, err := ctxServerBinary(code, p); err == nil {
				return p, c.Args, true, why
			}
			if ctxPathInRoot(code, p) {
				why = ctxServerSkip(l.ID)
			}
		}
	}
	return "", nil, false, why
}

// langStatus is one row of the languages table: the language, its code files
// and whether a server was found, with the install hint when not.
type langStatus struct {
	ID     string `json:"id"`
	Files  int    `json:"files"`
	Server string `json:"server"`
	Hint   string `json:"hint,omitempty"`
}

func (e *ctxEnv) langStatuses() []langStatus {
	var out []langStatus
	for _, lc := range detectedLangs(e.Code) {
		l := langByID(lc.ID)
		st := langStatus{ID: lc.ID, Files: lc.N}
		switch {
		case l == nil:
		case l.native() && l.ID == "go":
			if p := which(e.Gopls); p != "" {
				st.Server = p
			}
		case l.native() && l.ID == "typescript":
			p, why := tsServerChoice(e.Code)
			if p != "" {
				// tsServerChoice deliberately retains missing overrides for
				// lookup diagnostics. Status describes availability, not just
				// configuration, without changing lookup execution semantics.
				if _, err := exec.LookPath(p); err != nil {
					why = configuredServerNotUsable + err.Error()
				} else {
					st.Server = p
				}
			}
			if why != "" {
				e.note(why)
				st.Hint = why
			}
		default:
			bin, _, ok, why := langServerReason(e.Code, l)
			if ok {
				st.Server = bin
			}
			if why != "" {
				e.note(why)
				st.Hint = why
			}
		}
		if st.Server == "" && l != nil {
			if st.Hint == "" {
				st.Hint = l.Hint
			} else if l.Hint != "" && !strings.Contains(st.Hint, l.Hint) {
				st.Hint += "; " + l.Hint
			}
		}
		out = append(out, st)
	}
	return out
}

func (s langStatus) String() string {
	if s.Server != "" {
		return fmt.Sprintf("%s %d files, server %s", s.ID, s.Files, homePath(s.Server))
	}
	if s.Hint != "" {
		return fmt.Sprintf("%s %d files, no server (%s)", s.ID, s.Files, s.Hint)
	}
	return fmt.Sprintf("%s %d files, no server", s.ID, s.Files)
}

// ---- workspaces ----

var (
	goWorkUseRe = regexp.MustCompile(`\.{1,2}/[\w./-]*|\buse\s+(\.)`)
	gradleIncRe = regexp.MustCompile(`include\s*\(?\s*((?:['"][:\w./-]+['"]\s*,?\s*)+)`)
	quotedRe    = regexp.MustCompile(`['"]([:\w./*-]+)['"]`)
	mavenModRe  = regexp.MustCompile(`<module>\s*([^<\s]+)\s*</module>`)
)

// workspaceMembers reads what a workspace manifest declares as its children:
// directory globs relative to its directory. ok is false when the manifest
// declares none (a plain manifest), and members empty with ok true means
// "every nested project" (a Cargo [workspace] without members).
func workspaceMembers(path, base string) (members []string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	txt := string(b)
	switch base {
	case "go.work":
		for _, m := range goWorkUseRe.FindAllString(txt, -1) {
			if strings.HasPrefix(m, "use") {
				members = append(members, ".")
				continue
			}
			members = append(members, strings.TrimSuffix(strings.TrimPrefix(m, "./"), "/"))
		}
		return members, len(members) > 0
	case "package.json":
		var p struct {
			Workspaces json.RawMessage `json:"workspaces"`
		}
		if json.Unmarshal(b, &p) != nil || len(p.Workspaces) == 0 {
			return nil, false
		}
		if json.Unmarshal(p.Workspaces, &members) != nil {
			var o struct {
				Packages []string `json:"packages"`
			}
			_ = json.Unmarshal(p.Workspaces, &o)
			members = o.Packages
		}
		return members, len(members) > 0
	case "pnpm-workspace.yaml":
		for _, l := range strings.Split(txt, "\n") {
			if m := quotedRe.FindStringSubmatch(l); m != nil && strings.HasPrefix(strings.TrimSpace(l), "-") {
				members = append(members, m[1])
			} else if t := strings.TrimSpace(l); strings.HasPrefix(t, "- ") {
				members = append(members, strings.TrimSpace(strings.TrimPrefix(t, "- ")))
			}
		}
		return members, len(members) > 0
	case "pyproject.toml", "Cargo.toml":
		var t map[string]any
		if toml.Unmarshal(b, &t) != nil {
			return nil, false
		}
		var ws any
		if base == "Cargo.toml" {
			ws = t["workspace"]
		} else if tool, _ := t["tool"].(map[string]any); tool != nil {
			if uv, _ := tool["uv"].(map[string]any); uv != nil {
				ws = uv["workspace"]
			}
		}
		w, _ := ws.(map[string]any)
		if w == nil {
			return nil, false
		}
		if ms, _ := w["members"].([]any); ms != nil {
			for _, m := range ms {
				if s, _ := m.(string); s != "" {
					members = append(members, s)
				}
			}
		}
		return members, true
	case "settings.gradle", "settings.gradle.kts":
		for _, m := range gradleIncRe.FindAllStringSubmatch(txt, -1) {
			for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
				members = append(members, strings.ReplaceAll(strings.TrimPrefix(q[1], ":"), ":", "/"))
			}
		}
		return members, len(members) > 0
	case "pom.xml":
		for _, m := range mavenModRe.FindAllStringSubmatch(txt, -1) {
			members = append(members, strings.TrimSuffix(m[1], "/"))
		}
		return members, len(members) > 0
	}
	return nil, false
}

func memberMatches(members []string, rel string) bool {
	if len(members) == 0 {
		return true
	}
	rel = strings.TrimSuffix(rel, "/")
	for _, m := range members {
		m = strings.TrimSuffix(strings.TrimPrefix(m, "./"), "/")
		if m == rel {
			return true
		}
		if ok, _ := filepath.Match(m, rel); ok {
			return true
		}
		if strings.HasSuffix(m, "/**") && strings.HasPrefix(rel, strings.TrimSuffix(m, "**")) {
			return true
		}
	}
	return false
}

// foldWorkspaces makes a parent that declares its children (go.work, a
// package.json "workspaces" or pnpm-workspace.yaml, [tool.uv.workspace], Cargo
// [workspace], Gradle include, Maven <modules>) the single root for them: the
// children's roots of that language are removed. A directory that only
// contains manifests but declares none keeps one root per child.
func foldWorkspaces(code string, files []string, by map[string]*rootAcc) {
	var dirs []string
	for d := range by {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	extra := map[string][]string{} // gradle settings are not language manifests
	for _, f := range files {
		if b := filepath.Base(f); b == "settings.gradle" || b == "settings.gradle.kts" {
			d := ""
			if x := filepath.ToSlash(filepath.Dir(f)); x != "." {
				d = x + "/"
			}
			extra[d] = append(extra[d], b)
		}
	}
	for _, d := range dirs {
		a := by[d]
		if a == nil {
			continue
		}
		for _, mf := range append(append([]string{}, a.manifests...), extra[d]...) {
			members, ok := workspaceMembers(filepath.Join(code, d, mf), mf)
			if !ok {
				continue
			}
			var fold []*langDef
			for _, l := range allLangs() {
				if manifestMatches(l, mf) || ((mf == "settings.gradle" || mf == "settings.gradle.kts") && (l.ID == "java" || l.ID == "kotlin")) {
					fold = append(fold, l)
				}
			}
			for _, cd := range dirs {
				c := by[cd]
				if c == nil || cd == d || !strings.HasPrefix(cd, d) {
					continue
				}
				if !memberMatches(members, strings.TrimPrefix(cd, d)) {
					continue
				}
				for _, l := range fold {
					if c.langs[l.ID] {
						a.langs[l.ID] = true
						delete(c.langs, l.ID)
					}
				}
				if len(c.langs) == 0 {
					delete(by, cd)
				}
			}
		}
	}
}

// langIDOf is the language id of a path, "" for none.
func langIDOf(path string) string {
	if l := langOf(path); l != nil {
		return l.ID
	}
	return ""
}
