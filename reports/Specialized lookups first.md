# Specialized lookups first, in fewer turns

Status: built on worktree/rapid-stone-cbf6 (guard rework, lookup coach and log, orch lsp, docs, Claude gate ctx-only hook), not yet on main; bench A/B and retiring the orch ctx agent surface are still to do, 2026-10-26.

Phase 0 results so far:

- A Bash call that fails does not cancel its sibling calls in the same turn
  (checked in Claude Code 2.1.311), so `rg` exiting 1 on no match is safe in a
  batch; no `updatedInput` rewrite is needed.
- `jbcontext search` answers from the snapshot indexed for the current
  branch, else the default branch, else the newest one. A fresh worktree
  branch gets the default branch's snapshot, so lines can be stale until the
  branch is indexed; the teaching block says to read before citing a line.
- The exec-flag refusal (`rg --pre`, `fd --exec`) lives only in the pi gate
  shell, where it is a security boundary; normal sessions are not blocked.

## Goal

Every worker (Claude Code or pi, main session, subagent or gate pass) finds
code with the most specialized tool that can answer, and sends every
independent lookup of a step in one turn. All lookups run as CLI commands
through the shell, so there is one channel, one set of guards and one log:

| Order | Question | Command |
|---|---|---|
| 1 | Where is a behavior or concept? (location unknown) | `jbcontext search "<one behavior>"` |
| 2 | A name is known: definition, references, callers, implementations | `orch lsp <verb> <Name...>` (new, Phase 3) |
| 3 | An exact string (error text, config key, route, non-code file) | `rg` |
| 4 | A file by name | `fd` |
| 5 | Judge the code you located | native `Read` / `read`, with offset and limit |

`grep`, `find`, `cat`, `head` and `tail` stay blocked. The `orch ctx` agent
surface (`find`, `sym`, `text`, `batch` and its query DSL) is retired once the
bench below shows the ladder does at least as well. Batching comes from
several shell calls in one assistant message, not from a batch DSL.

Why CLIs and not native tools: a CLI works the same in both harnesses, in
subagents and in gate passes, with no per-harness tool registration and no
MCP setup. pi has no LSP of its own, and Claude's `LSP` tool needs a file
position for every question; `orch lsp` takes a symbol name, so a symbol
question is one call instead of two.

## Phase 0: verify before building

Each is one short headless test.

1. **Parallel shell failure.** Does a Bash call that exits non-zero cancel its
   sibling Bash calls in the same turn? `rg` exits 1 on no match. If it does,
   the PreToolUse hook rewrites a plain `rg ...` through `updatedInput` so exit
   1 becomes 0 with a `(no matches)` line, and exit 2 still fails. Same check
   for pi.
2. **jbcontext in worktrees.** Leads work in worktrees. Which checkout does
   `jbcontext search` answer from inside a worktree, and are its line numbers
   right for the worktree's files? (`orch ctx find` currently drops hits
   "not in this checkout"; we need to know why before we stop filtering.)
3. **jbcontext under an isolated HOME.** The keychain popup (memory note) for
   gate passes, and whether the 429 rate limit shows up in normal use.
4. **pi parallelism.** pi-agent-core defaults to parallel tool execution;
   confirm pi-coding-agent does not override it.

## Phase 1: jbcontext, well understood

jbcontext goes first only if workers use it well. One short teaching block,
the same everywhere (Phase 5), based on the `context-search` skill:

- **When:** the location is unknown and you can describe a behavior. Not when
  you already know a name (use `orch lsp`) or an exact string (use `rg`).
- **How to write a query:** one behavior in plain language, "Code that
  validates a session token and refreshes it", never a keyword list. One
  concept per query.
- **Batching:** several independent concepts go out as several
  `jbcontext search` calls in one turn. A retry of the same concept comes only
  after you read the best hit, narrowed with `-p <dir of that hit>`.
- **What an answer means:** ranked candidates, not proof of absence. No hit
  means "try `rg` for a known string", not "it does not exist".
- **Then read:** open the top hits with the read tool, all ranges in one turn.

Update `context-search/SKILL.md` (dotfiles) to drop its "prefer `orch ctx
find`" line and carry this block. The Phase 0 worktree finding decides
whether it also needs a "line numbers may be stale; read to confirm" note.

## Phase 2: guards and reminders

### Blocks only where the replacement is exact

A block costs a turn, and a block that is sometimes wrong teaches the model to
work around the guard. So a block is used only when the blocked command has a
replacement that does the same job, every time:

| Rule | Blocks | Replacement |
|---|---|---|
| `deny-grep` | `grep`, `egrep`, `fgrep`, `ag`, `git grep` over files | `rg` (same job, always) |
| `deny-find`, `deny-find-chains` | `find` over a tree, `find -exec`, search piped into a search | `fd` |
| `deny-cat-head-tail` | one bare `cat`/`head`/`tail`/`sed -n` on a file | the read tool |
| `deny-sed-inplace` | `sed -i`, `perl -pi` | the edit tool |
| `deny-big-read` | a whole-file read over 30 KB | read with offset and limit |
| pi `deny-builtin-grep/find` | pi's own `grep`/`find` tools | `rg`/`fd` in the shell (one channel) |

Removed: the `rg` block while `orch` is healthy, Claude's
`deny-builtin-search` (Grep/Glob are off by default on macOS anyway), and every
"One orch ctx batch replaces it" reason.

### Reminders for judgment calls

Choosing jbcontext over `rg`, `orch lsp` over `rg`, and batching are judgment
calls with legitimate exceptions (an identifier in a Markdown file, one lookup
that really is the only one). Blocking them would be wrong some of the time,
and blocking a single lookup to demand batching adds a turn, which defeats the
point. So these are reminders, injected next to the tool result (Claude
PreToolUse `additionalContext`, pi `tool_result` append), each at most once
per session per kind:

| Reminder | Trigger | Text (short) |
|---|---|---|
| `concept-first` | First search of the session is `rg`/`fd` with no `jbcontext` call before it | "Location unknown? Ask jbcontext first: one behavior per query, several queries in one turn." |
| `symbol-via-lsp` | `rg` whose pattern is a bare identifier (`\bFoo\b`, `func Foo`, `Foo(`) over code files | "For a definition, references or callers, use orch lsp; rg finds text, not uses." |
| `batch-lookups` | Two turns in a row that each held exactly one lookup | "Send independent lookups together in one message; each one alone costs a turn." |
| `read-batch` | Two turns in a row that each read one range | "Read the ranges you already know in one message." |

A reminder becomes a block only when the log shows it is ignored and its
trigger turns out to have no false positives. The log records, for each
reminder, whether the next lookup followed it.

Turn boundaries: Claude hooks get no message id, so the hook reads the last
assistant message from `transcript_path` (the parser in `tool/stall.go`
already does this). pi gets one process per session and tracks it in memory.

### Gate passes

The pi ctx-only shell (`tool/pi_ctx_only.ts`) changes from "one `orch ctx`
call" to "one of `jbcontext search`, `orch lsp`, `rg`, `fd`, `ls`, `wc`", still
with no pipes, redirects or substitutions and with the exec-flag refusal.
Claude gate passes allow `Bash(jbcontext search:*)`, `Bash(orch lsp:*)`,
`Bash(rg:*)`, `Bash(fd:*)` (`tool/ctx_cli.go`, `ctxCLIForExecutable`).

## Phase 3: `orch lsp`

A thin CLI over orch's existing language-server clients (`tool/ctx_lsp.go`,
`ctx_gopls.go`, `ctx_tsls.go`; Go, TypeScript, Python and the registry
languages, with the language-server trust policy). This is `orch ctx sym`'s
engine with LSP-shaped verbs and a smaller output:

```sh
orch lsp def Name [Name...]        # definition: path:line and signature
orch lsp refs Name [--limit N]     # references, grouped by file
orch lsp callers Name              # incoming calls, with the calling function
orch lsp impls Name                # implementations of an interface or method
orch lsp symbols path/to/file.go   # the file's outline
orch lsp hover path:line:col       # type and doc at a position
```

- A name may be `Name`, `pkg.Name` or `Type.Method`; ambiguous names list the
  candidates with their positions.
- Several names in one call are allowed, because it is the natural CLI form
  and lets one language-server pass answer them all.
- Servers stay warm per repository in the existing ctx watch daemon, so a call
  does not pay server startup.

Claude's own `LSP` tool stays available but is not what the instructions
teach, so both harnesses learn one command.

## Phase 4: lookup log

jbcontext already tracks its own searches. This log answers a different
question: did the worker pick the right tool, in the right order, in few
turns? One JSONL line per lookup in `~/.local/state/orchestrate/lookups.jsonl`:

```json
{"ts":"...","harness":"claude","session":"...","repo":"...","turn":12,
 "tool":"jbcontext|lsp:refs|rg|fd|read","query":"...","ok":true,"empty":false,
 "bytes":2140,"duration_ms":38,"reminder":null,"followed":null}
```

- Claude: PostToolUse and PostToolUseFailure on `Bash|Read`, keeping only
  lookup commands.
- pi: `tool_execution_end`.

`orch lookups report [--since 7d] [--repo R]`:

- lookups per turn, and turns spent finding code per task
- share of sessions whose first lookup was jbcontext or `orch lsp`
- `rg` calls with an identifier pattern (missed `orch lsp`)
- empty and error rates per tool
- reminders fired and how often they were followed
- the same file range read twice in one session

`tool/bench_report.go` `benchToolRows` moves to these per-tool counts.

## Phase 5: one teaching block everywhere, measure, retire

The ladder, the jbcontext rules from Phase 1, and "send independent lookups in
one message", in the same words in:

- this repo: `SKILL.md`, `references/context.md` (rewritten), `ctxCLINotice`
  (`tool/ctx_cli.go`), `ctxBudgetHint` (`tool/gate_agent.go`),
  `ctxResultGuide` (`tool/ctx_guide.go`).
- dotfiles: `~/.agents/AGENTS.md` (its search order says `orch ctx` first),
  the `context-search` skill, the route-work skill's subagent sentence, the
  Explore and implementer agent definitions.

Then an `orch bench` A/B on the same tasks: the current `orch ctx batch` group
against a "ladder" group. Compare turns, tokens, lookups per turn and answer
correctness. If the ladder is at least as good, remove the `orch ctx` agent
surface and its guard texts. `orch ctx brief` and the language-server clients
stay.

## Risks

- **jbcontext without the ctx wrapper** loses stale-hit filtering and the `rg`
  fallback on a 429. Phase 0 sizes both; if they matter, `orch` adds a thin
  `jbcontext` shim on PATH that filters and falls back, with the same
  arguments.
- **Reminders can be ignored.** The log measures it per reminder.
- **Opening the gate shell to `rg`/`fd`/`jbcontext`** widens what a read-only
  pass can run. The exec-flag refusal and the no-pipe rule keep it read-only;
  the shared case table gets cases for both.
