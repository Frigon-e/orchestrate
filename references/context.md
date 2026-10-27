# Finding code facts

Agents look code up in this order, most specific tool first:

1. Location unknown: `jbcontext search "<one behavior>"`. One concept per query,
   several queries in one message. Results are candidates, not proof of absence.
   A fresh worktree branch searches the default branch's snapshot, so read
   before citing a line.
2. Known symbol: `orch lsp def|refs|callers|impls <Name>` (`Name`, `pkg.Name` or
   `Type.Method`), `orch lsp symbols <file>` for an outline and `orch lsp hover
   <path:line:col>` for a type. Exit 1 means not found and 2 means no language
   server for that language; `orch lsp --help` lists the flags.
3. Exact text: `rg` (`rg -n -w`, `-F`, `-g 'GLOB'`).
4. File names: `fd`.
5. Read with the native read tool (Pi `read`, Claude `Read`), using offset and
   limit for part of a file.

Independent lookups go as parallel calls in one message. `grep`, a plain `find`
while `fd` is installed, `cat`, `head`, `tail` and `sed -n` are blocked by the
tool guard; plain `rg` is allowed. Never re-read what you already read.

`orch ctx find|sym|text|batch` remain available for scripts and the gate
planner, and `orch ctx brief` writes task briefs, but agents are no longer
taught them for lookups. The rest of this file describes their output, which
scripts and `orch plan` depend on. Here `orch` means
`~/.agents/skills/orchestrate/scripts/orch`, called from the repository root.
`orch ctx` alone shows the live state (index against HEAD, tools ready) and
the commands; `orch ctx <subcommand> --help` shows flags and examples.
`orch version`, `orch --version`, `-v` and `-V` print the bare build fingerprint
without repo/config/backend initialization. Direct Go builds print `devel`.
The launcher uses Go and `shasum` to check source, embedded-asset and toolchain
content on each invocation; unchanged content reuses its worktree-scoped binary.

## The orch ctx commands

Scripts pick the cheapest command that can answer, and move up only when it
cannot:

1. `text` for a string, a name, a route, a message: regex or literal matches in
   the reported scope, with status and exclusions. Narrow it with `--glob`,
   `--word`, `--type` before you widen it.
2. `sym` for a function, type or method name: its definition, callers and
   implementers.
3. `find` (concept) only when no name is known.
4. `batch` for two or more independent lookups.

The tool guard no longer probes `orch` and never names `orch ctx` as a
replacement, so a broken `orch` does not change what a block says.

| You need | Command | Why this one |
|---|---|---|
| String matches within a stated scope | `orch ctx text <pattern> [--glob G] [--word] [--fixed] [--type T] [-l] [-c] [-C n]` | ripgrep: smart-case, skips lockfiles, minified, generated and vendored files; hits grouped by file, each with its enclosing function and configurable context (default 6 lines) |
| Exact or candidate-labelled definitions and available references | `orch ctx sym <Name\|pkg.Name\|Type.Method> [--refs N] [--impl=false] [--kind K] [--in path]` | gopls (Go) or the TypeScript server: doc comment, body preview, callers with their enclosing function, implementers, where it is wired |
| Candidates for a concept ("how are refunds issued") | `orch ctx find "<concept>" [-p path] [--limit N]` | Semantic search (jbcontext): bounded current-checkout excerpts, declarations and package size; not evidence of absence |
| Several of the above at once | `orch ctx batch <plan.json\|-> [-q "kind:query key=value"]` | One call, run in parallel, answered in plan order, a place shown once |
| A starting picture for a piece of work | `orch ctx brief <file\|-> [--owns a,b,c]` | Ranked files, interface sheet and look-alikes outside the owns; not an exhaustive inventory |
| Judging what code does | native `read` / `Read`, with offset and limit | Read the files you located; never use a shell command or ctx read for this |

## Output format

One format for every command. TOON (`name[n]{cols}:` plus rows) is used only for
short uniform tables: status, counts per file, the common files of a batch,
`help[n]:` hints. Hits are plain text because code is full of the delimiters a
table would have to quote. `--json` prints the data, the notes and the sources
for scripts.

```
text: 3 of 3 matches for "createGatewaySchema" in 1 file (glob web/apps/crm/src/**)
web/apps/crm/src/features/admin/sales/gateways.tsx
  60- /** What the connect form checks ... */
  61: export const createGatewaySchema = z
  97: resolver: zodResolver(createGatewaySchema),  [in function CreateGatewayForm]
sources: rg
```

- The first line counts the result ("3 of 3"). A path is printed once, then one
  row per line: `N:` a match, `N-` a context line, `[in func X]` the
  declaration that holds the match (read from the file, column-0 declarations;
  shown when it changes). Overlapping and adjacent ranges of a file are one hit.
- An empty answer is a sentence, not a blank: `text: 0 matches found in the
  whole repo`. Negative text results also state `searched_scope` and `excluded`.
  This is no match within that scope, not proof that behavior is absent.
- Anything left out is named with the flag that shows it, see Pagination.
- A reader sees only what carries information: `sources` names the tools and any
  staleness note, not the binary path; `omissions` and `notes` appear only when
  they have content; the `match` flag appears only when a row is a context line;
  the excerpt byte counters are not printed.
- Evidence ids are `ctx-` plus 8 hex characters. A plain session (not a read-only
  gate agent) gets one per file, not per row; a repeated row says
  `reference: path:line`. A batch whose lookups share `searched_scope` or
  `excluded` prints each once at the top. `--json` keeps every field except
  those two moves on each lookup.
- Per-lookup options (`--glob`, `--word`, `-p`, ...) are flags of `text`, `sym`
  and `find`. A batch takes them inside the query, `-q "text:x glob=dir/**"`; a
  batch-level `--glob` is an error that shows the rewritten form.
- Test files are hidden by default and counted: `(+18 in tests;
  tests=include|only)`. `--tests only` lists only test files, `--tests include`
  both. One matcher everywhere: `_test.go`, `*.test.*`, `*.spec.*`,
  `__tests__`, `testdata`, `fixtures`. Works on `find`, `sym`, `text` and, as
  `tests`, on every lookup of a batch.
- `--compact` is the shortest answer (counts, signatures, first lines, no
  context or caller lists). `--full` raises finite display caps, not backend
  limits: default 1000 text matches/page, 600 columns and about 48000 characters
  per single answer or batch lookup, 280000 per batch. `--fields src` tags each
  file with the tool that found it.
- Each lookup has `status`: `ok` (usable result), `empty` (completed with no
  visible results), `partial` (usable data plus errors/incompleteness), or
  `failed` (no usable result). `count_kind` is `exact` (count completed within
  the stated scope), `lower_bound` (at least the reported number), or `unknown`
  (no authoritative total). An exact count need not mean all excerpts fit.
  Semantic candidates always have unknown counts and never establish absence.
- Errors are on stdout, structured under `--json`, with recovery hints. Usage
  errors exit 2. A failed single lookup exits 1; a batch where all lookups fail
  exits 1. A partial batch exits 0 with aggregate `status: partial` and retains
  successful results and each lookup's errors. A partial single lookup exits 0.
  Read status, count kind, notes, errors and cuts, not only the exit code.

### Pagination

`find`, `text` and `sym` (callers and implementers) take `--limit N` (page size)
and `--page P` (1-based); in a batch they are the per-lookup keys `limit` and
`page` (`-q "text:foo limit=6 page=2"`). `--max` aliases `--limit` for find/text;
`max` is also accepted for symbol batch lookups.
Defaults: find 4, text 5 hits, callers 10 (`--refs N` still sets the callers
page size), implementers 10. Pages are small on purpose: each hit is shown with
more code around it, and the footer names what the page left out. `--limit` or `--page` below 1 is a usage error
(exit 2). Explicit `limit`, `max` and `page` values above 2000 are also
usage errors.

Paging is stateless: page P is items `[(P-1)*N, P*N)` of a list sorted the same
way every time (text: path then line; callers and implementers: file then line;
find: jbcontext rank), so a page never depends on an earlier one. Semantic
pages filter visible candidates before slicing, but retrieval is bounded to a
2000 raw-rank window. Hidden or rejected candidates can consume that window;
use a narrower `-p path` or native checkout reads instead of treating a short
page as an exhaustive search.

Every list with more says where it stands and which files it left out:
`showing 5-8 of 40; next: --page 3; left out: a.go (9), b.go (7), +4 more files`
(up to 5 files with their hit counts; for `find` the 8 ranked hits past the page
are looked up to name them). Inside a batch the hints use lookup keys:
`next: page=3`, `narrow with glob=/word=/tests=/path=` (find: `path=/tests=`, sym:
`in=/def_kind=/tests=`). Above 50 items narrowing comes first: `showing 1-5 of 80;
75 more; narrow with --glob/--word/--tests/-p, or --page 2`; after page 3 only
narrowing is suggested. A page after the end says `page 9 is past the end (40 hits, 8
pages)`. Check `count_kind` before using a total: failures, candidates and
unavailable references do not have exact counts. Callers retain bounded lists
(up to 50 per definition in the existing reference tiers), and symbol display
shows at most four definitions after path/kind filters. Narrow `--in` and
`--kind` for ambiguous symbols. `--full` raises page size (1000 for text, every
kept caller for sym), not the retained backend data.

Text retains at most 2000 visible excerpts while counting independently. A
completed count can still be exact beyond retention; an interrupted count can
be lower-bound or unknown. Narrow `--glob`/`-p` (batch `glob`/`path`), or inspect
the checkout with native Read/read. `--full` does not remove this retention cap.
Backend safety guards also remain: 16 MiB semantic JSON or LSP frames, 8 MiB
per rg record and 64 KiB diagnostics. A cut or failed backend requires a scoped
retry or native inspection, not an inference of absence.

### Text (`orch ctx text`)

Options: `--fixed/-F` literal, `--word/-w`, `--ignore-case/-i` (default is
smart-case), `--glob` (repeatable; `!` excludes), `--type/-t`, `-l` files only,
`-c` counts per file, `-C n` context lines (default 6; `-C 0` none),
`--multiline/-U`, `--only-matching/-o`, `--per-file N`, `--limit N` hits per
page (default 5; `--max` is the same), `--page P`, `--all` also lockfiles, generated, minified and vendored
files, but still honors gitignore and hidden-file rules. `--all` does not
include tests unless `--tests include|only` is also set. Leading whitespace is
trimmed and a line is cut at 300 characters (600 with `--full`).
Lines that read the same in several files print once:
`same line in 5 files: import x (a.ts:1, b.ts:2, c.ts:3, +2 more)`.

```
orch ctx text polar --glob 'internal/**' --word
```

### Symbols (`orch ctx sym`)

One block per definition: a header with the name, kind and place, the doc
comment, a body preview sized by kind, and reference information. Symbol facts
distinguish exact identity from candidates. Candidate headings explicitly say
`candidate (identity not verified)`; the JSON view retains the same heading in
`parts`. Backend-verified definitions use exact identity; declaration scans,
ambiguous bare methods and weaker name refinements remain candidates. Missing servers or failed references produce partial/failed results
with location-qualified errors, not zero callers. By-name matches are observed
name occurrences, not a call graph or proof of absence. Paths are printed once:
callers in the definition's file need none,
callers in other files sit under their path.

```
createGatewaySchema  const  web/apps/crm/src/features/admin/sales/gateways.tsx:61-77
What the connect form checks before the request; the API tests the key and has the last word.
  export const createGatewaySchema = z
    .object({
    ...
  (+8 lines; --full)
used by 2, none in tests:
  85  type CreateGatewayValues = z.infer<typeof createGatewaySchema>
  97  resolver: zodResolver(createGatewaySchema),  in CreateGatewayForm
```

- Body by kind: see "How much of a definition `sym` shows" below. A type,
  interface or struct lists every field or method (a Go struct also gets a
  `methods:` line). A cut says how many lines and `--full`.
- `used by 12 in 3 files, +5 in tests:` lists callers outside tests (up to
  `--refs`, default 10; `(+N more; --refs N or --full)` says what is left), each
  with its line and enclosing function. `-C n` adds n lines above each caller.
  `--refs 0` prints only the line with the counts. Test references stay a count;
  `--tests include|only` lists them under `tests:`.
- `implemented by N:` lists implementers of an interface method (`--impl=false`
  hides it). `wired:` follows the first caller one level up by name with rg (a
  pointer, not a call graph). Both are printed once even when several
  definitions share them.
- `--kind Method` and `--in path` filter definitions. `--compact` is the header,
  first doc line, signature and the counts line.

```
orch ctx sym StartCheckout --refs 3
```

### Concepts (`orch ctx find`)

Each chunk uses current-checkout code (up to 80 lines, 3 in compact mode),
merged per file, followed by
`declares ...` (the declarations in it) and the size of its package.

```
orch ctx find "webhook signature check" -p internal/ --tests include
```

### Batch (`orch ctx batch`)

An agent plan is JSON, `{"lookups":[{"kind":"concept|symbol|text","query":"...","why":"..."}]}`
(from a file or `-`), or inline, repeatable, with options as `key=value`:

```
orch ctx batch -q "symbol:StartCheckout refs=3" -q "text:polar tests=none glob=internal/**"
```

Options are validated per lookup kind, including explicitly set false/zero
values. Find: `path`, `tests`, `generated`, `max`, `limit`, `page`, `why`;
text: `fixed`, `word`, `ignore_case`, `glob` (repeatable), `type`, `files_only`,
`count`, `context`, `multiline`, `only_matching`, `per_file`, `all`, `path`,
`tests`, `generated`, `max`, `limit`, `page`, `why`; symbol: `refs`, `impl`,
`context`, `def_kind`, `in`, `tests`, `generated`, `max`, `limit`, `page`, `why`.
`orch ctx batch --help` lists the accepted keys. The script-only read kind is
described below. `why` is a label echoed in the lookup's header and is not part of any cache key. Quote a
query word that contains `=`. At most 50 lookups.

The batch removes redundancy across lookups:

- `common[n]{path,sources,lookups}:` lists a file found by several kinds of
  lookup once, with its sources (`jb+rg+sym`) and lookup numbers; it comes first.
- A line already printed by an earlier lookup is not printed again:
  `path also #2: 58-66`.
- One `notes[]` block and one `sources:` line per batch.
- Each answer is held to about 9000 characters and the batch to 70000; what was
  cut is said: `cut at about 9000 characters: 41 more lines (2 more files) not
  shown; --full raises the bounded cap, or narrow the lookup`, or `#6 ...: not
  shown, the batch budget of 70000 characters is spent`. Add `limit=N` or scope
  broad lookups; run a cut lookup alone when needed. `--full` raises per-lookup
  and total budgets to about 48000 and 280000 characters, not unlimited output.

### How much of a definition `sym` shows

A type is shown whole up to 120 lines, a function its signature and first 40
lines. A const or var reads in context: when it sits within 30 lines below the
start of its enclosing function it shows that function's first 30 lines;
otherwise 30 lines around it; the whole declaration is always inside, up to 60
lines.

Put every independent lookup in one batch instead of one turn each.

### Script-only file reads

The `orch ctx read <path> --from N --to N` command and batch `read` kind remain
for scripts and the planner; agents use their native read tool instead. The
batch read options are `path`, `from`, and `to` (at most 60 lines). For scripts:

```sh
orch ctx read pay/pay.go --from 1 --to 40
orch ctx batch -q "read:pay/pay.go from=1 to=40"
```

These are script examples, not agent instructions.

## Languages and symbol tiers

`orch ctx` is not tied to one language or repo layout. Languages come from a
table (`tool/ctx_lang.go`): extensions, declaration forms, doc comment markers,
test and generated file patterns, entry-point names, the export rule and the
language servers to try. Built in: Go, TypeScript/JavaScript, Python, Rust,
Java, Kotlin, C/C++, Ruby, PHP and shell.

Project roots come from manifests, never directory names: `go.mod`/`go.work`,
`package.json`/`tsconfig.json`/`pnpm-workspace.yaml`/`deno.json`, `pyproject.toml`
and friends, `Cargo.toml`, `pom.xml`/`build.gradle(.kts)`, `Gemfile`,
`composer.json`, `CMakeLists.txt`. Nested roots are separate, and a language
server gets one workspace per root. A parent is a single root only when it
declares its children (`go.work`, `package.json` workspaces or
`pnpm-workspace.yaml`, `[tool.uv.workspace]`, Cargo `[workspace]`, Gradle
`include`, Maven `<modules>`). With no manifest, the top directories with the
most code files stand in. Text and concept search always run at the git root.

A symbol is answered by the best tier available, and `sources:` names it:

1. A language server: gopls (Go), the native TypeScript 7 server, Pyrefly for
   Python (`pyrefly lsp --indexing-mode lazy-blocking`), rust-analyzer, clangd,
   jdtls. Definitions come from `workspace/symbol` when the server advertises
   it, else from the declaration scan; references and implementations come from
   the server.
2. A declaration scan with rg: a name is a symbol only if some declaration form
   of a language matches it (`def X`, `class X`, `func X`, `fn X`, ...). Callers
   are word matches outside the definition, printed `(by name)`: a pointer, not
   a call graph. A bare word that no declaration matches is not a symbol.
3. Plain text (`orch ctx text`), as an explicit scoped fallback. A missing
   language server does not become a successful empty symbol search: useful
   declaration/name candidates can survive with partial status and errors;
   without usable data the symbol lookup fails. Install the external server
   named in the hint, narrow the lookup, or inspect source with native Read/read.

Python servers are tried in order: Pyrefly, basedpyright, pyright, `ty server`,
pylsp, then the declaration scan. Override with `ORCH_PYLS` (a command line).
Discovery order: the env override, the project's `.venv`/`venv` in normal mode
(trusted repos only), then PATH. Every resolved server must pass
the language-server trust policy below. Install
Pyrefly with `uv tool install pyrefly`, `pip install pyrefly` or run it with
`uvx pyrefly`. `orch ctx status` and `orch doctor` list the detected languages
and, for each, the server found or the install hint; a missing server never
fails them.

Add or change a language in the user config with `[[ctx_langs]]`; a known `id`
overrides only the fields set:

```toml
[[ctx_langs]]
id = "zig"
extensions = [".zig"]
decl = ['^\s*(?:pub\s+)?fn\s+{sym}\b']   # {sym} is the escaped name
test = ['_test\.zig$']
lsp_bin = "zls"
lsp_hint = "install zls"
```

Other keys: `doc` (comment markers), `generated`, `entry`, `export`, `lsp_args`,
`lsp_fallbacks` (`"command args"` strings), `manifests`.

## Writing a concept query

When you do not know where code lives, search one behavior at a time in plain
language: `Code that <action> on <entity>, when <trigger>, producing <result>`.
Include only what you know. Do not invent symbols, frameworks or paths. Read the
best hits, adopt the repo's own names, then use `sym` / `text` for exact lookups.

Drop any slot that does not apply; do not force the whole template.
- Good (full): `Code that saves a payment method for a customer, when a checkout completes, producing a stored method row`.
- Good (short): `Code that retries failed payments`.
- Bad (noun list): `payment checkout method customer store`.

Briefs build their own concept queries from the task text, in the same plain
language and with no model: each of the first three acceptance bullets that
describes behaviour (command lines and bare file names are skipped), then the
title's first sentence, else the first sentence of the body. A query of fewer
than 4 words is never sent. Owned directories narrow the search to their code
tree; they are not queries.

`orch ctx find --help` and the home view use the same form. A short noun-list query, or an answer with at most one hit, gets one line:
`tip: describe the behavior: "Code that <action> <entity> ..." (one behavior per
query)`. It is shown once per batch and never for a query that starts with `Code
that` or has six or more words. Queries you type are never rewritten.

## Generated files and ignored files

A file is generated when its path matches the language table or the universal
list (`.generated.`, `/generated/`, `/gen/`, `openapi`, `swagger`, `/vendor/`,
`/node_modules/`, `/dist/`, `.min.`), or one of its first five lines has a marker
(`Code generated ... DO NOT EDIT`, `auto-generated`, `autogenerated`,
`@generated`, `This file was generated`, `DO NOT EDIT`), or `.gitattributes` sets
`linguist-generated`. One function decides this for `find`, `text`, `batch`, `sym`
callers and the brief's look-alikes; header checks
read only the first lines and are cached per run.

Generated files are hidden and counted like tests: `(+3 in generated; --generated
include|only)`; the flag is `--generated` on `find`, `text` and `sym` and
`generated=` in a batch lookup; `text --all` includes them. Totals and pages
exclude what is hidden. A symbol's definition may live in a generated file (sqlc
store methods): it is shown, labelled `generated`, because it is the real
definition; generated files are still not listed as callers, look-alikes or reuse
candidates.

Ignored files: no `orch ctx` path passes `-u`, `--no-ignore` or `--hidden` to
rg, so `.gitignore` is honoured; the file lists come from `git ls-files --cached
--others --exclude-standard` (tracked files plus untracked ones git does not
ignore). jbcontext indexes a git revision (`index --revision`, default HEAD), so an ignored untracked file is not in the index. Every `find` hit is checked with `git check-ignore`, and an ignored path is dropped (this covers the `watch` overlay of uncommitted changes).

## When not to use it

- To understand a function's behaviour, to decide anything, or to check a claim
  that depends on a condition the tools cannot see: read the code.
- `find` returns ranked semantic candidates, never proof of absence. A missing
  hit can reflect ranking, filtering, a cap or stale/missing index data.
- A successful empty `text` result means no pattern match in the stated scope
  and exclusions. Check status and count kind, then broaden scope or inspect
  source if the claim needs more evidence. Symbols can be candidates; missing
  servers and reference failures are not evidence of zero callers.
- No retrieval result alone establishes that a behavior does not exist.
- A brief leads with the scored picture: ranked files, the interface sheet and
  the existing look-alikes. It ends with a short "Search notes" block (at most 8
  lines): lookup counts by state, one line per distinct failure reason and the
  names found nowhere. Lookup ids, states, counts and scopes are not in the
  markdown. It says once that absence of a hit is not proof of absence. When the
  byte budget cuts something it cuts the notes, then the look-alikes, and says so
  in one line; a section with nothing to show is left out.
- A brief plans every search first, then runs the symbol lookups, the text
  searches and the concept searches at the same time (one concept pool, 5 wide
  at most and never above the slot cap) and puts the answers together in the
  order the searches were planned, so the text does not depend on which search
  finished first. A concept search the service refused (429) is asked again after
  the cooldown, up to 2 rounds. A refused search is a lost lookup, so the width
  follows the service, not the slots.

## Staleness

Every answer ends with a `sources:` line naming what answered: the jbcontext
index revision against HEAD, the gopls or TypeScript server used, rg. A note
`jbcontext index is at <sha7>, working tree is at <sha7>[ with uncommitted
changes]: hits may be stale` means `find` may miss new code. Displayed semantic
spans are validated or uniquely relocated against the checkout; ambiguous or
invalid spans are rejected with partial/error status. Text reads the checkout;
symbol coverage still depends on server readiness and the selected tier.
Results are cached per HEAD and working-tree fingerprint under
`~/.cache/orchestrate/ctx/`; incomplete/error results are not cached as
successful search facts. This is not a guarantee of exhaustive server coverage.
The JetBrains service rate limits (HTTP 429), so jbcontext searches are
throttled: identical searches in one process share one call, at most 5 run at
once machine-wide (flock slots under `~/.cache/orchestrate/jb-slots/`; set
`ORCH_JB_SLOTS` to 1-32 to raise it for light workloads, invalid values give 5; the
service limit depends on the request kind), the first call after
10 minutes without one runs alone so a stale token is exchanged once, and a
429 is retried 3 times with backoff. After that the lookup fails with
`jbcontext rate limited (429)` and the query it skipped, jbcontext is skipped for
20 s, and a `find` answers from rg words with `jbcontext skipped: rate limited
(429)` in its first line.
`orch ctx status` shows the index, the watch daemon and
the tools; `orch ctx refresh` makes the index current. `orch ctx watch add`
keeps an overlay for uncommitted changes (the daemon is never started by orch).

## Read-only tools for agents

Pin retrieval with an absolute git root:

```sh
ORCH_CTX_ROOT=/absolute/repo ORCH_CTX_READONLY=1 orch ctx find "Code that retries failed payments"
ORCH_CTX_ROOT=/absolute/repo orch ctx --readonly sym StartCheckout
```

`ORCH_CTX_ROOT` itself implies read-only. The flag and `ORCH_CTX_READONLY=1`
require it. The caller's working directory cannot change the root. Scripts and
the gate planner use `find`, `sym`, `text`, `refs` (an alias of `sym`) and `batch`;
agents are taught `jbcontext search` and `orch lsp` instead.
The CLI also permits `read` for scripts; agents use their native read tool.
`refresh`, `status`, `watch` and `brief` are refused. Index builds
and refreshes are disabled, including automatic ones on `find`. An existing
stale index can still answer, with a staleness note. Traversal and symlinks
outside the root cannot be read, and a batch-plan file must be inside the root.
Result caches and CLI shims stay under the ctx cache.

Agents use the plain CLI through the harness shell tool. No server, schema or
adapter files are generated. `CtxCLIFor(harness, code)` returns:

```go
type CtxCLI struct {
    Env    []string
    Tools  []string
    Notice string
    Extension string // pi guard path
    Ready string     // gate invocation's activation acknowledgement
}
```

Append `Env` to the agent process environment, add `Tools` to its allowlist,
and put `Notice` at the very top of the shared read-only prompt. `Env` pins
`ORCH_CTX_ROOT`, sets `ORCH_CTX_READONLY=1`, and prepends a cache directory to
PATH. Its `orch` symlink targets the current binary's path (`os.Executable()`),
not its inode: an atomic rebuild at that path still works without regenerating
the shim. Shim creation is idempotent; a conflicting entry is an error.
A gate pane pass does not use that PATH: it
finds `orch` on the pane shell's own PATH (link `scripts/orch` into a PATH
directory such as `~/.local/bin`; the launcher follows symlinks), and a pane
run with the ctx tool fails at once when `orch` is not on PATH. The pane types
one short `sh <launcher>` line. The launcher, in the gate dir's `passes/`,
exports the rest of `Env` and execs the agent, so typed input stays well
under the terminal's input limit (about 1 KB on macOS) even while a fresh
pane's shell is still starting. The pass then waits for the agent's turn to
begin (Herdr reports it working, or its session holds an answer): pi is idle
at its prompt while its extensions load, and that idle is not the end of the
pass.

Keep Claude's native `Read,Grep,Glob` in both the tool and allowed-tool lists,
add `Bash` to the tool list, and add `Bash(jbcontext search:*)`,
`Bash(orch lsp:*)`, `Bash(rg:*)` and `Bash(fd:*)` to `--allowedTools` with
`dontAsk` permission handling. Pi defaults to `--ctx-shell ctx-only` with ctx on:
only native `read` and guarded bash. It starts with
`--no-builtin-tools --tools +read`: read is active, while bash stays registered
but inactive. After its command guard is installed, the extension enables bash
and acknowledges startup. A hard `--tools read` allowlist would exclude bash
from the registry and prevent activation.
A missing acknowledgement or extension-write error fails the pass, without an
open-shell fallback. `--ctx-shell open` keeps Pi's native `read,grep,find,ls`
and appends unrestricted `bash`. Claude ignores `--ctx-shell`; ctx off keeps the
original native tools. `CtxCLI.Tools` contains only the extra shell entries.
The Claude `Bash(...)` permissions are not an exclusive shell-command allowlist, and a Claude
pass can read outside the checkout through native Read. `ORCH_CTX_ROOT`
constrains orch, not native Read; it is not a file-confidentiality boundary.

Pi's default ctx-only shell accepts only one `jbcontext search`, `orch lsp`,
`rg`, `fd`, `ls` or `wc` call, without pipes, redirects, command substitution,
unquoted globs or other commands; `rg` and `fd` flags that run another program
are refused. `--ctx-shell open` opts
out and permits arbitrary shell programs, including writes and network access.
Native read is not root-confined in either mode. The prompt and post-pass
unchanged check remain separate layers, not an operating-system sandbox;
ignored files and outside paths are not covered by that check. Gate records,
reports and compare show `ctx_shell`; missing historical fields display `open`.

`tool/pi_ctx_reminder.ts` is a user-level pi extension (the
orchestrator installs it under `~/.pi/agent/extensions/`). Gate passes do not
rely on that copy: a pi pass with `--ctx-tool on --ctx-shell open` starts with `--no-extensions -e <cli-dir>/orch-ctx-reminder.ts`, the copy
embedded in the binary and written next to the ctx shim, so a guarded gate run
tests the code in the checkout. Ctx-only and ctx off do not load it. The
`ORCH_TOOL_GUARD`, `ORCH_TOOL_GUARD_SKIP` and `ORCH_TOOL_GUARD_STATE_DIR` settings in orch's environment reach the pane's pi through the pane shell exports. `ORCH_TSLS`, when
set in orch's environment, reaches the pane's `orch ctx` and `orch lsp` the same way in any
ctx-tool pass. The Claude hooks in `hooks/claude/` port the same rules, and
`hooks/claude/README.md` is the reference for them. The guard carries five
small deny rules over shell commands, each a table entry with a name, a matcher
and a reason, plus two more for pi's own `grep` and `find` tools:
`deny-find-chains` (find `-exec`/`-delete`, and a search tool feeding a grep),
`deny-find` (a plain `find` over a tree while `fd` is installed),
`deny-sed-inplace` (`sed -i`, `perl -pi`), `deny-cat-head-tail` (one bare
`cat`, `head`, `tail` or `sed -n` on a file) and `deny-grep-rg` (a plain
`grep`, `egrep`, `fgrep`, `ag` or `git grep` over files or a directory, while
`rg` is installed; `rg` itself is never blocked). `deny-builtin-grep` and
`deny-builtin-find` refuse pi's own `grep` and `find` tools while `rg` (for
grep) or `fd` (for find) is installed, because they reach the tree without a
shell; with the replacement absent they are the fallback and are allowed. On
Claude there are no rules for the built-in `Grep` and `Glob` tools, and no
health probe of `orch`: a block does not depend on it. `ls` only lists a
directory, so it never blocks.
A search block is two lines: what is blocked with the replacement (`rg` for
content, `fd` for file names), then the lookup order: `jbcontext search "<one
behavior>"` if the location is unknown, `orch lsp def|refs|callers <Name>` for a
known symbol, `rg` for exact text, `fd` for file names, with independent lookups
sent in one message. A plain `find` is allowed when `fd` is not installed and a
`grep` when `rg` is not installed, because there is no replacement to name.
Reading a file is the exception: `deny-cat-head-tail` always names the
built-in `read` with offset and limit (a section or the whole file).
`orch ctx` is never the named replacement. Allowed: a grep filtering another
command's output, a read that feeds a pipe or a redirect, `tail -f`, several
files to `cat`, a `find` reading a pipe, `rg`, and `fd`. A built-in search
that names an absolute path outside the session's repository has no
replacement and is allowed. A blocked command line is blocked every time and
does not run on a retry; from the third block in a row the reason adds `Blocked
3 times: use the replacement above; this guard does not give way.`
`ORCH_TOOL_GUARD=off` turns the guard off and
`ORCH_TOOL_GUARD_SKIP=find-chains,find,sed-inplace,cat-head-tail,grep-rg`
disables rules by name; the group name `builtin-search` disables both built-in
search rules. The guard stays silent inside the ctx-only gate shell. Its
decision tables run in node: `go test -run 'CtxReminder' .` in `tool`.

The lookup coach is a non-blocking part of the same extension and of the Claude
`lookup-coach.sh` hook. It logs every lookup (`jbcontext search`, `orch lsp`,
`orch ctx`, `rg`, `fd`, reads) to `~/.local/state/orchestrate/lookups.jsonl`
and reminds the worker, at most once per kind per session, when its first `rg`
or `fd` comes before any `jbcontext` or `orch lsp` call, when an `rg` pattern is
a symbol that `orch lsp` should answer, or when it spends one turn per lookup
instead of batching. `ORCH_LOOKUP_COACH=off` disables the reminders and
`ORCH_LOOKUP_LOG=off` the log. `orch lookups report [--since 7d] [--repo PATH]
[--json]` summarizes it: lookups per turn, how often the first search was
`jbcontext` or `orch lsp`, empty and error rates, reminders fired and followed.
The details are in the Lookup coach section of `hooks/claude/README.md`.

## Language-server trust

Language servers are executable programs, not passive repository data. A
server found inside an inspected checkout is skipped unless that repository is
explicitly trusted. The real binary path is resolved with `EvalSymlinks`, so an
env override or PATH entry that points through a symlink into the checkout is
also subject to the policy. These checks also guard the execution boundary.

Read-only mode never honors trust: it skips project `LocalDirs` (such as
`node_modules/.bin` and `.venv/bin`) and does not select a TypeScript server
from the checkout's pnpm store. It uses only env overrides or PATH servers
whose real binary is outside the checkout, otherwise the declaration/rg tier.
TypeScript PATH discovery uses `tsgo`, or a `tsc` whose `--version` reports 7
or newer (checked once per process); a `tsc` of 5 or 6 is the JavaScript
compiler and is ignored.
A skipped TypeScript server adds the note `ts server in the repo not run in
read-only mode`, once per response.

Normal mode implicitly trusts only the repository containing the installed
skill (`ORCH_SKILL_DIR`). Other git repositories need an explicit user-config
opt-in:

```toml
ctx_trust = ["/absolute/repo"]
```

`ctx_deny` wins over every trust source. Git entries cover linked worktrees by
the common Git directory. Non-git directory entries match directly, including
symlink equivalence; trust is not recursive approval for unrelated child
repositories. Read-only mode never runs repo-local servers, even in a trusted
or installed-skill repository. In trusted normal mode, TypeScript auto-pick
looks for the pnpm store in the checkout and other worktrees of its repository,
so a fresh worktree can use that server without `ORCH_TSLS`. `orch doctor`
reports `ts server found in the repo, not run (...)` when a server is skipped.
This does not sandbox an external server or prevent it from running project
plugins.

## Config (user config)

Path: `$XDG_CONFIG_HOME/orchestrate/config.toml`, default
`~/.config/orchestrate/config.toml`; `ORCH_CONFIG` overrides it.

- `ctx_index`: `full` (default) builds a missing index (15 minutes at most; a
  one-line notice says so) and refreshes a stale one; `incremental` only
  refreshes an existing index; `off` never calls jbcontext.
- `ctx_deny`: repos (paths) that never use jbcontext and never run repo-local
  language servers (their worktrees included). Tasks with `private: true` never
  use jbcontext either.
- `ctx_trust`: explicit repository/directory opt-ins for repo-local language
  servers in normal mode, for example `ctx_trust = ["/absolute/repo"]`. Only
  the installed-skill repository is implicitly trusted. `ctx_deny` wins;
  read-only mode never permits these servers.
- Overrides for tests and odd installs: `ORCH_JBCONTEXT`, `ORCH_GOPLS`,
  `ORCH_TSLS` (the TypeScript 7 server, `tsc --lsp --stdio`), `ORCH_PYLS` (the
  Python server command), `ORCH_RG`,
  `ORCH_CACHE_DIR`.
