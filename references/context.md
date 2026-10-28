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

Independent lookups go as parallel calls in one message. `grep` (also `egrep`,
`fgrep`, `ag`, `git grep`), a plain `find` while `fd` is installed, and a bare
`cat` or `sed -n` over one file are blocked by the tool guard; `head` and
`tail` are not, but their output is not a Read, so use Read. Plain `rg` is
allowed. Never re-read what you already read.

There is no `orch ctx` command. `orch lsp` and the gate's enrichment share one
retrieval engine (jbcontext, language servers, rg); the settings below apply to
both. Here `orch` means `~/.agents/skills/orchestrate/scripts/orch`, called from
the repository root. `orch version`, `orch --version`, `-v` and `-V` print the
bare build fingerprint without repo/config/backend initialization. Direct Go
builds print `devel`. The launcher uses Go and `shasum` to check source,
embedded-asset and toolchain content on each invocation; unchanged content
reuses its worktree-scoped binary.

## Languages and symbol tiers

Languages come from a table (`tool/code_lang.go`): extensions, declaration
forms, doc comment markers, test and generated file patterns, entry-point names,
the export rule and the language servers to try. Built in: Go,
TypeScript/JavaScript, Python, Rust, Java, Kotlin, C/C++, Ruby, PHP and shell.

Project roots come from manifests, never directory names (`go.mod`/`go.work`,
`package.json`/`tsconfig.json`, `pyproject.toml`, `Cargo.toml`, `pom.xml`/
`build.gradle(.kts)`, `Gemfile`, `composer.json`, `CMakeLists.txt`). Nested roots
are separate, and a language server gets one workspace per root.

`orch lsp` answers a symbol with the best tier available:

1. A language server: gopls (Go), the native TypeScript 7 server, Pyrefly for
   Python, rust-analyzer, clangd, jdtls.
2. A declaration scan with rg: a name is a symbol only if some declaration form
   of a language matches it. Callers are word matches outside the definition,
   printed `(by name)`: a pointer, not a call graph.

A missing language server never becomes a successful empty answer: the command
says which server is missing and how to install it. Python servers are tried in
order: Pyrefly, basedpyright, pyright, `ty server`, pylsp. `orch doctor` lists
the detected languages and, for each, the server found or the install hint.

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

## jbcontext index

`ctx_index` in the user config sets the policy: `full` (default) builds a missing
index (15 minutes at most) and refreshes a stale one, `incremental` only
refreshes an existing index, `off` never calls jbcontext; it applies to `orch gate
enrich`. Agents call `jbcontext search` directly, so the index is kept recent
per harness. For Claude, `jbcontext setup-agent` installs SessionStart and
SessionEnd hooks that run `jbcontext index --only-incremental --silent`. It has
no setup for pi, so orch-guard does the same on each pi prompt, in the
background, at most every 10 minutes per checkout (`ORCH_JB_REFRESH=off` turns
it off; read-only retrieval skips it). jbcontext is for finding where code
lives, so an index a few commits behind is fine; the watch daemon is not needed. `orch doctor` prints a
`retrieval:` line with the index and the tools, and the `jbcontext index
--project-path <repo>` to run when a repository has no index. jbcontext
calls are limited to 5 in flight (`ORCH_JB_SLOTS`, 1-32), a 429 is retried 3 times
with backoff, and after that jbcontext is skipped for 20 s.

## Read-only retrieval

Pin retrieval to an absolute git root with `ORCH_CTX_ROOT=/absolute/repo`; it
implies read-only, as does `ORCH_CTX_READONLY=1`, which requires it. The caller's
working directory cannot change the root. Index builds and refreshes are
disabled; an existing stale index can still answer, with a staleness note.
Traversal and symlinks outside the root cannot be read. `orch lsp` honors both
variables.

Gate agent passes get a read-only lookup shell (`jbcontext search`, `orch lsp`,
`rg`, `fd`) through a PATH shim that resolves `orch` to this binary, with both
variables set; see `references/gate.md` (`--lookup-tool`, `--lookup-shell`).

## Tool guard and lookup coach

`tool/pi_ctx_reminder.ts` is a user-level pi extension (`scripts/install-guards`
links it as `~/.pi/agent/extensions/ctx-reminder.ts`). It holds no rules: it turns
pi events into JSON requests for `orch-guard ... --harness pi` (the same binary
the Claude hooks run, `hooks/bin/orch-guard`) and applies the replies; the
protocol is in `hooks/claude/README.md`. `scripts/orch` sets `ORCH_GUARD_BIN` for
the pi leads it launches, and a missing or slow binary fails open.

The guard carries five small deny rules over shell commands, each a table entry
with a name, a matcher and a reason, plus two more for pi's own `grep` and `find`
tools: `deny-find-chains` (find `-exec`/`-delete`, and a search tool feeding a
grep), `deny-find` (a plain `find` over a tree while `fd` is installed),
`deny-sed-inplace` (`sed -i`, `perl -pi`), `deny-cat-head-tail` (one bare `cat`
or `sed -n` on a file; `head` and `tail` are not blocked) and `deny-grep-rg` (a
plain `grep`, `egrep`, `fgrep`, `ag` or `git grep` over files or a directory,
while `rg` is installed; `rg` itself is never blocked). `deny-builtin-grep` and
`deny-builtin-find` refuse pi's own `grep` and `find` tools while `rg` or `fd` is
installed; with the replacement absent they are the fallback and are allowed.

A search block is two lines: what is blocked with the replacement (`rg` for
content, `fd` for file names), then the lookup order above. Reading a file is the
exception: `deny-cat-head-tail` always names the built-in `read` with offset and
limit. Allowed: a grep filtering another command's output, a read that feeds a
pipe or a redirect, `tail -f`, several files to `cat`, a `find` reading a pipe,
`rg`, and `fd`. A blocked command line is blocked every time and does not run on
a retry; from the third block in a row the reason adds `Blocked 3 times: use the
replacement above; this guard does not give way.` `ORCH_TOOL_GUARD=off` turns the
guard off and `ORCH_TOOL_GUARD_SKIP=find-chains,find,sed-inplace,cat-head-tail,grep-rg`
disables rules by name; the group name `builtin-search` disables both built-in
search rules. The decision tables run the extension in node against the real
binary: `go test -run 'CtxReminder' .` in `tool`.

The lookup coach is a non-blocking part of the same binary (`orch-guard coach`),
run by the same extension and by the Claude hooks. It logs every lookup
(`jbcontext search`, `orch lsp`, `rg`, `fd`, reads) to
`~/.local/state/orchestrate/lookups.jsonl` and reminds the worker, at most once
per kind per session, when its first `rg` or `fd` comes before any `jbcontext` or
`orch lsp` call, when an `rg` pattern is a symbol that `orch lsp` should answer,
or when it spends one turn per lookup instead of batching. `ORCH_LOOKUP_COACH=off`
disables the reminders and `ORCH_LOOKUP_LOG=off` the log. `orch lookups report
[--since 7d] [--repo PATH] [--json]` summarizes it. The details are in the Lookup
coach section of `hooks/claude/README.md`.

## Language-server trust

Language servers are executable programs, not passive repository data. A server
found inside an inspected checkout is skipped unless that repository is
explicitly trusted. The real binary path is resolved with `EvalSymlinks`, so an
env override or PATH entry that points through a symlink into the checkout is
also subject to the policy.

Read-only mode never honors trust: it skips project `LocalDirs` (such as
`node_modules/.bin` and `.venv/bin`) and uses only env overrides or PATH servers
whose real binary is outside the checkout, otherwise the declaration/rg tier.
TypeScript PATH discovery uses `tsgo`, or a `tsc` whose `--version` reports 7 or
newer; a `tsc` of 5 or 6 is the JavaScript compiler and is ignored.

Normal mode implicitly trusts only the repository containing the installed skill
(`ORCH_SKILL_DIR`). Other git repositories need an explicit user-config opt-in:

```toml
ctx_trust = ["/absolute/repo"]
```

`ctx_deny` wins over every trust source. Git entries cover linked worktrees by
the common Git directory. Non-git directory entries match directly, including
symlink equivalence; trust is not recursive approval for unrelated child
repositories. `orch doctor` reports `ts server found in the repo, not run (...)`
when a server is skipped. This does not sandbox an external server or prevent it
from running project plugins.

## Config (user config)

Path: `$XDG_CONFIG_HOME/orchestrate/config.toml`, default
`~/.config/orchestrate/config.toml`; `ORCH_CONFIG` overrides it.

- `ctx_index`: `full` (default), `incremental` or `off` (see jbcontext index).
- `ctx_deny`: repos (paths) that never use jbcontext and never run repo-local
  language servers (their worktrees included). Tasks with `private: true` never
  use jbcontext either.
- `ctx_trust`: explicit repository/directory opt-ins for repo-local language
  servers in normal mode. Only the installed-skill repository is implicitly
  trusted. `ctx_deny` wins; read-only mode never permits these servers.
- `ctx_langs`: language table additions (see Languages above).
- Overrides for tests and odd installs: `ORCH_JBCONTEXT`, `ORCH_GOPLS`,
  `ORCH_TSLS` (the TypeScript 7 server, `tsc --lsp --stdio`), `ORCH_PYLS` (the
  Python server command), `ORCH_RG`, `ORCH_CACHE_DIR`.
