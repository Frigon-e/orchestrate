# Tool guards (Claude Code and pi)

One Go binary, `hooks/bin/orch-guard` (source: `tool/guard` and `tool/cmd/orch-guard`), makes every guard and lookup-coach decision for both harnesses. Claude Code runs it as registered hooks in `~/.claude/settings.json`; pi runs it through the thin extension `tool/pi_ctx_reminder.ts`. This folder holds the Claude registration (`settings-fragment.json`, `install_settings.py`), the shared case tables (`toolguard_cases.json`, `coach_cases.json`) and `prefer-read-tool.sh`.

## Install

```sh
scripts/install-guards            # build the binary, register the Claude hooks, link the pi extension
scripts/install-guards --check    # change nothing; say what is missing or stale; exit 1 if anything is
scripts/install-guards --remove   # unregister the Claude hooks, unlink the pi extension
```

- It builds `hooks/bin/orch-guard` with `scripts/build-guard` (needs Go), runs `install_settings.py --apply`, and links `~/.pi/agent/extensions/ctx-reminder.ts` to `tool/pi_ctx_reminder.ts` (pi follows symlinks when it discovers extensions, so a `git pull` is an update). A regular file already at that path is moved to `ctx-reminder.ts.bak-<YYYYMMDD>`, never deleted. Running it twice changes nothing. Everything lands under `$HOME`, so a temporary `HOME` gives a throwaway install.
- `orch doctor` reports a missing or stale binary, missing Claude hooks and a missing or misdirected pi extension, each with the fix `scripts/install-guards`.
- `install_settings.py` (also usable alone: no flag shows what would change, `--apply`, `--check`, `--apply --remove`, `--stdin` for a dotfiles manager) owns only hooks whose command points into `skills/orchestrate/hooks/`. Every other key and hook is left as found, an existing hook is never duplicated, `--apply` keeps a timestamped `settings.json.bak-*`, and a file that is not valid JSON is never touched. It also prunes the retired shell-script hooks (`deny-*.sh`, `lookup-coach.sh`) an older install registered; those scripts are gone, so run `scripts/install-guards` once after pulling.
- Tests: `python3 hooks/claude/install_settings_test.py`, `cd tool && go test -run TestInstallGuards .`.
- Event names, matchers and the JSON shape of Claude hooks: https://docs.claude.com/en/docs/claude-code/hooks

## orch-guard binary

Two modes, each reading one JSON event on stdin:

- `orch-guard pre` is every PreToolUse rule: the Bash rule groups for Bash, `deny-big-read` for Read. The first hit blocks: exit 2 with the reason on stderr. Anything else exits 0.
- `orch-guard coach` is the lookup coach for PostToolUse, PostToolUseFailure and UserPromptSubmit. It never blocks, always exits 0 and fails open.

Add `--harness pi` to either mode for pi (default: claude). pi mode always exits 0 and prints one JSON object; the guard's decision is in the reply, not the exit code.

**pi protocol** (documented in `tool/guard/pi.go` and at the top of `tool/pi_ctx_reminder.ts`). One process per call: `pre --harness pi` for `tool_call`, `coach --harness pi` for the rest.

```
request  { event, session, turn, cwd, tool, input, toolCallId, content?, isError?, durationMs?, resultHook? }
reply    { block?, reason?, appendText?, message? }
```

`event` is `tool_call`, `tool_result`, `tool_execution_end` or `before_agent_start`. `content` holds one string per result part (`""` for a part that is not text). `resultHook` says the extension could register a `tool_result` handler; without one, the first blocked search carries the reminder itself. `block` and `reason` answer `tool_call`; `appendText` is text parts to add to a tool result; `message` is the hidden ladder message for `before_agent_start`. The extension owns only the wiring: which handlers to register, the 5 s timeout, and failing open (a missing binary, a timeout or an unreadable reply allows the call and prints one warning per session to stderr). The binary is `$ORCH_GUARD_BIN`, which `scripts/orch` sets for pi leads and orch sets for gate panes, else `~/.agents/skills/orchestrate/hooks/bin/orch-guard`. The pi-only rules (`deny-builtin-grep`, `deny-builtin-find`, the inside-repository exemption, the give-way counter after three blocks in a row, the search note, pi's texts) live in the binary, and so does pi's coach state: the per-session state file, keyed by the pi session id.

**Build and staleness.**

- `scripts/build-guard` builds it and is a no-op when `orch-guard --version` already equals the fingerprint of the sources; `build-guard --print-fingerprint` prints that fingerprint. `scripts/orch` runs it when it rebuilds orch or the binary is missing, and `install_settings.py --apply` runs it first. The binary is git-ignored.
- On every call the binary re-hashes its sources under the repository it was built from. If they changed it still answers with its own logic, and starts a detached `scripts/build-guard` (one at a time, a lock in the state dir; output in `guard-build.log` there). A build that fails is not started again for the same sources for 10 minutes (`guard-build-failed` in the state dir holds the fingerprint and time); changed sources start a build at once. `orch doctor` warns when the binary is missing, not executable or stale.
- Tests: `cd tool && go test ./guard/... ./cmd/...`; the pi path runs the real extension in node against the real binary in `TestCtxReminder*` (`tool/ctx_reminder_test.go`).

## Tool guard

The rules live in `tool/guard/rules.go`; each row names a rule group the binary enforces. `deny-grep-rg` keeps its id for compatibility: it blocks `grep`, `egrep`, `fgrep`, `ag` and `git grep`, and never `rg`.

| Rule group | Rule | Blocks | Does not touch |
|---|---|---|---|
| `find-chains` | `deny-find-chains`, `deny-find` | `find` with `-exec/-execdir/-ok/-okdir/-delete`; a search tool (`find`, `fd`, `rg`, `ag`) feeding `grep`/`ag`, or `xargs grep`/`rg`/`ag`; a plain `find` over a tree while `fd` is on PATH | a `find` reading stdin (it formats a pipe), `fd ... \| rg pattern` (it filters file names), a quoted word or a path that merely contains `find`, `fd` alone, and a plain `find` when `fd` is not installed |
| `sed-inplace` | `deny-sed-inplace` | `sed`/`gsed` with `-i`, `-i.bak`, `-i ''` or `--in-place`; `perl` with `-i` or `-pi` | `sed` that prints to stdout, a piped transform (`git diff \| sed 's/^/  /'`), `grep -i` |
| `cat-head-tail` | `deny-cat-head-tail` | a bare `cat` or `sed -n` over exactly one file | `head`, `tail`, several files, a pipe on either side, a redirect, `cat` reading stdin |
| `grep-rg` | `deny-grep-rg` | `grep`, `egrep`, `fgrep`, `ag`, `git grep` over files or a directory, while `rg` is on PATH | `rg` itself, a `grep` filtering another command's output (`go test ./... \| grep FAIL`), `grep -q` as a condition, `grep` reading stdin, a quoted word, and every `grep` when `rg` is not installed (there is no replacement to name) |
| `big-read` | `deny-big-read` | Claude's `Read` of a regular file over 30720 bytes (`bigReadBytes` in `tool/guard/bigread.go`) with neither `offset` nor `limit` | a read with `offset` or `limit` (either one), `pages`, images (png, jpg, jpeg, gif, webp), PDFs, notebooks (`.ipynb`), a file at or under the limit, a missing path, a directory, and any other tool |
| `search-first` | `deny-search-first` | the first `rg` of a user prompt, in any segment of the line (an `rg` filtering a pipe is skipped): a lowercase phrase with a space (`refund before payment`, repo indexed by jbcontext) goes to `jbcontext search`, with any identifiers in it named for `orch lsp def`; identifiers alone (`camelCase`, `PascalCase`, `pkg.Name`, `func.*Name`, `type Name\b`, `A\|B\|C`, ALL_CAPS beside them) go to `orch lsp def NAMES` / `orch lsp refs NAME`. Once per prompt; the retry passes, and the Claude coach drops `concept-first` while it is on. The jbcontext index answer is shared by sessions in `jbindex.json` (24 h, 1 h when not indexed). Off with `ORCH_TOOL_GUARD_SKIP=search-first`. Lives in `tool/guard/searchfirst.go` | `rg -F`, `--files`, a file target, doc-only targets, paths outside the repo, any prompt window with a jbcontext or `orch lsp` lookup already run, and any pattern that may be exact text: an anchor (`^package`), punctuation or digits (`<DataTable`, `@xl`, `429`), sentence case (`Not run`), ALL_CAPS alone (`DEVSTACK_ADMIN_URL`), single words or snake_case (`theme`, `received_at`), or names mixed with such words |

Claude's own `Grep` and `Glob` tools are not guarded: there is no `deny-builtin-search` rule, and `install_settings.py --apply` removes an owned hook that the fragment no longer lists, so an older install loses it on the next run.

### What a block says

A search block is two lines: what is blocked with the replacement, then one ladder line.

```
orch tool guard (deny-grep-rg): grep, egrep, fgrep, ag and git grep over files are blocked; use rg (rg -n PATTERN path, -g 'GLOB', -F literal, -w word, -i).
Lookup order: jbcontext search "<one behavior>" if the location is unknown, orch lsp def|refs|callers <Name> for a known symbol, rg for exact text, fd for file names; send independent lookups in one message.
```

`deny-find` and `deny-find-chains` name `fd` the same way (`fd PATTERN dir`, `fd -e go`, `fd -g '*.ts'`; a chain says "use fd, or rg -g 'GLOB' PATTERN for content"). With no `fd` on PATH a plain `find` is allowed (Claude Code does not load `Glob` by default on macOS, so there is nothing else to name), and a chain names a plain `find` or `rg` and skips the ladder. `deny-sed-inplace` always names `Edit` and `deny-cat-head-tail` always names the exact `Read` call for the blocked line (`Read file_path=FILE offset=N limit=K` for a `sed -n N,Mp`, `Read file_path=FILE` for a `cat`), to send as a parallel call, and says cat and sed output is not a Read so a later Edit fails. pi (`--harness pi`) names its own tool (`read path=FILE ...`) and, since pi accepts relative paths, adds no absolute-path note.

### Big whole-file reads

Whatever a session reads stays in context and is re-read on every later turn, so one whole-file `Read` of a large file costs far more than the read. `deny-big-read` refuses a `Read` of a regular file over 30720 bytes (30 KB, exclusive: exactly 30720 passes) when the call has no `offset` and no `limit`; either one alone is enough, because the model chose a range. Exempt: `pages` (PDF), images, PDFs and notebooks, which Claude Code reads specially, a missing path, a directory, and a payload the script cannot read (it fails open). The block is three lines: the file size in KB, "use Read with offset and limit", and how to get the line numbers (`rg -n PATTERN file`, a doc's headings with `rg -n "^#{1,3} " file`). It stays blocked on retry with the same input, and a retry with `limit` passes. `ORCH_TOOL_GUARD=off` disables it, and `ORCH_TOOL_GUARD_SKIP=big-read` (or `deny-big-read`) disables the rule alone. The limit has no variable of its own.

A blocked command stays blocked on every retry. Two switches are the human escapes, same as pi:

- `ORCH_TOOL_GUARD=off` disables all the rules.
- `ORCH_TOOL_GUARD_SKIP=find-chains,sed-inplace,cat-head-tail,grep-rg` disables rules by short name or full name. Plain `find` has its own id, `find` (or `deny-find`), inside the `find-chains` group.

`prefer-read-tool.sh` stays. It is a non-blocking reminder for the wider reader set (`less`, `more`, `bat`, `nl`, and `sed` without `-n` as well), so it complements `deny-cat-head-tail`; it is not replaced by it. It is a shell script and is not part of the binary. Register both if you want the block and the reminder.

## Lookup coach

A non-blocking coach, in both harnesses, that logs every lookup and reminds the worker when it uses the wrong tool or spends one turn per lookup. It never blocks and always exits 0; any error fails open silently. `orch-guard coach` runs as a PostToolUse and PostToolUseFailure hook (matcher `Bash|Read|LSP|mcp__jbcontext__.*`) and, with no matcher, as a UserPromptSubmit hook. The pi side is `orch-guard coach --harness pi`, driven by `tool/pi_ctx_reminder.ts`: the extension sends `tool_execution_end` (the duration), `tool_result` (the reminder comes back as text to append) and `before_agent_start` (a user prompt; the ladder comes back as a message); it stays silent in the restricted gate shell.

What counts as a lookup is decided by `coach_cases.json`, shared by both harnesses: the first command of a shell line (after `VAR=x` and a leading `cd DIR &&`) is `jbcontext search` (`jbcontext`), `orch lsp <verb>` (`lsp:<verb>`), `rg` or `fd`; anything else, and an `rg` or `grep` later in a pipe, is not. Claude's `Read` and pi's `read` are `read` (query `path:offset-limit`), Claude's `LSP` is `lsp:<operation>`, and `mcp__jbcontext__*` is `jbcontext`.

**Log.** One JSON line per lookup in `$ORCH_TOOL_GUARD_STATE_DIR/lookups.jsonl` (default `~/.local/state/orchestrate/lookups.jsonl`), written with one call: `ts`, `harness`, `session`, `repo`, `turn`, `tool`, `query` (200 characters at most), `ok`, `empty`, `bytes`, `duration_ms`, `reminder` and `followed`. `rg`, `fd` and `orch lsp` exiting 1 with no output are `ok` and `empty`; a `jbcontext search` that prints "No relevant results found" is `empty`. A turn is the id of the assistant message that made the call (Claude: found through `transcript_path` and `tool_use_id`, in the last 512 KB of the transcript; pi: the `turn_start` index). Per-session state lives in `<state dir>/coach/<session_id>.json`, locked with `flock` because parallel calls fire their hooks at once.

**Reminders**, text prefixed `orch lookup coach: `. A prompt window runs from one user prompt to the next (a prompt counter in the session state). `concept-first` and `symbol-via-lsp` fire at most once per prompt window and `lookup-ladder` at most once per prompt; the batch kinds stay at once per session:

| Kind | Fires when |
|---|---|
| `lookup-ladder` | on a user prompt (UserPromptSubmit; pi `before_agent_start`), before any search: the session's first prompt, and afterwards only a prompt that follows a window whose first search (`rg` or `fd`) came before any `jbcontext` or `orch lsp` call. It reads `Before searching: location unknown -> jbcontext search ...; known symbol -> orch lsp ...; exact text -> rg. ... Read known files or ranges with Read, not cat or sed -n.` and writes no lookup row |
| `concept-first` | the window's first `rg` or `fd` comes before any `jbcontext` or `orch lsp` call in that window |
| `symbol-via-lsp` | an `rg` pattern is a symbol (CamelCase, snake_case, `pkg.Name`, or any identifier with `-w`, after stripping `\b`, `\<`, `\>`, a leading `func `/`def `/`class `/`type `/`interface ` and a trailing `(`), unless every `-g`/`-t`/path targets md, txt, json, yaml, yml or toml |
| `batch-lookups` | on the first lookup of a new turn, the previous two consecutive turns each held exactly one non-read lookup |
| `read-batch` | on the first lookup of a new turn, the previous two consecutive turns each held exactly one read and no other lookup |

`followed` is set on the first lookup after a `lookup-ladder` (true for `jbcontext` or `lsp:*`), a `concept-first` (true only for `jbcontext`) or `symbol-via-lsp` (true only for `lsp:*`) reminder; the batch reminders leave it null. A reminder reaches Claude as `additionalContext` on the hook's own event. The ladder is only recorded in the session state, so `orch lookups report` does not count it; its `followed` shows only on the log row.

**Switches.** `ORCH_LOOKUP_COACH=off` disables the reminders (the ladder too), `ORCH_LOOKUP_LOG=off` the log, and `ORCH_TOOL_GUARD=off` both.

**Report.** `orch lookups report [--since 7d] [--repo PATH] [--json]` reads the log: per harness, sessions, lookups, lookups per turn, the share of sessions whose first search was jbcontext or lsp, `rg` calls with a symbol pattern, the empty and error rate per tool, reminders fired and followed, and the same read repeated in a session.

Tests: `cd tool && go test ./guard/...` (the Claude hook, temp state dir, synthetic transcript), `go test -run 'CtxReminder|Lookups' .` (the pi extension in node against the real binary, and the report).

## Tests

`toolguard_cases.json` is one decision table for both harnesses: every case names the command, the PATH situation (`fd-rg` with both installed, `rg` only, `fd` only, or `none`) and the rule that must block it, or no rule when it is allowed. Its `readCases` table covers `deny-big-read` (file name, size in bytes, extra `Read` arguments, block or allow; the test makes the files in a temp directory, and pi's `read` tool runs the same table). Its `builtinCases` table covers pi's own `grep` and `find` tools, which are blocked while `rg` (for grep) or `fd` (for find) is on PATH and allowed for a path outside the session's repository.

`tool/guard/toolguard_test.go` runs every case through the real binary with only stub tools on PATH, plus the two switches, the non-Bash and broken-stdin paths, the reasons byte for byte and the big-read rule with its fail-open paths. `TestCtxReminderSharedToolguardCases` and `TestCtxReminderSharedBuiltinSearchCases` (`tool/ctx_reminder_test.go`) run the same file through the pi extension and the binary, so a disagreement between the two harnesses fails a test.

```sh
cd tool && go test ./guard/... ./cmd/...
cd tool && go test -run CtxReminder .
```
