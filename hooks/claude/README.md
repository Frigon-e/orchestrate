# Tool guards (Claude Code and pi)

One Go binary, `hooks/bin/orch-guard` (source: `tool/guard` and `tool/cmd/orch-guard`), makes every guard and lookup-coach decision for both harnesses. Claude Code runs it as registered hooks in `~/.claude/settings.json`; pi runs it through the thin extension `tool/pi_ctx_reminder.ts`. This folder holds the Claude registration (`settings-fragment.json`, `install_settings.py`), the shared case tables (`toolguard_cases.json`, `coach_cases.json`), `prefer-read-tool.sh`, and `orch-hook.sh`, the entry for the `orch hook pretool|stop|session-start` hooks. `orch-hook.sh` starts orch only in a pane orch launched (`ORCH_ROLE`) or one with a role marker from `orch become` (Herdr pane or Orca pane, terminal or session id), so other sessions do not pay orch's start-up on every tool call.

## Install

```sh
scripts/install-guards            # build the binary, register the Claude hooks and the orch MCP server, link the pi extension
scripts/install-guards --check    # change nothing; say what is missing or stale; exit 1 if anything is
scripts/install-guards --remove   # unregister the Claude hooks and the MCP server, unlink the pi extension
```

- It builds `hooks/bin/orch-guard` with `scripts/build-guard` (needs Go), runs `install_settings.py --apply`, and links `~/.pi/agent/extensions/ctx-reminder.ts` to `tool/pi_ctx_reminder.ts` (pi follows symlinks when it discovers extensions, so a `git pull` is an update). A regular file already at that path is moved to `ctx-reminder.ts.bak-<YYYYMMDD>`, never deleted. It also registers the orch MCP server for Claude Code with `claude mcp add --scope user orch -- <this checkout>/scripts/orch mcp` when `~/.claude.json` does not already hold that command (it only reads that file; `--check` reports `orch MCP server not registered` or `points to <x>`, `--remove` runs `claude mcp remove --scope user orch`, and without `claude` on PATH the step is skipped with a warning). pi registers the same server from the extension on load (`exposure: "codemode"`, which turns codemode on for the session), unless pi's own MCP support is off for the session (`-builtin:mcp` in the user or project `settings.json`, or `--no-mcp` / `--no-extensions` without `-e builtin:mcp`), `ORCH_MCP=off` is set, or no `orch` binary is found (`$ORCH_BIN`, else `orch` on PATH). Running it twice changes nothing. Everything lands under `$HOME`, so a temporary `HOME` gives a throwaway install.
- `orch doctor` reports a missing or stale binary, each fragment hook settings.json lacks and a missing or misdirected pi extension and an unregistered or misdirected orch MCP server (read from `~/.claude.json`), each with the fix `scripts/install-guards`.
- `install_settings.py` (also usable alone: no flag shows what would change, `--apply`, `--check`, `--apply --remove`, `--stdin` for a dotfiles manager) owns only hooks whose command points into `skills/orchestrate/hooks/` (and replaces `scripts/orch hook` entries a hand install left). Every other key and hook is left as found, an existing hook is never duplicated, `--apply` keeps a timestamped `settings.json.bak-*`, and a file that is not valid JSON is never touched. It also prunes the retired shell-script hooks (`deny-*.sh`, `lookup-coach.sh`) an older install registered; those scripts are gone, so run `scripts/install-guards` once after pulling.
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

`event` is `tool_call`, `tool_result`, `tool_execution_end` or `before_agent_start`. `content` holds one string per result part (`""` for a part that is not text). `resultHook` says the extension could register a `tool_result` handler; without one, the first blocked search carries the reminder itself. `block` and `reason` answer `tool_call`; `appendText` is text parts to add to a tool result; `message` is the hidden ladder message for `before_agent_start`. The extension owns only the wiring: which handlers to register, the 5 s timeout, and failing open (a missing binary, a timeout or an unreadable reply allows the call and prints one warning per session to stderr). The binary is `$ORCH_GUARD_BIN`, which `scripts/orch` sets for pi leads and orch sets for gate panes, else `~/.agents/skills/orchestrate/hooks/bin/orch-guard`. The pi-only parts (the give-way counter after three blocks in a row, the search note, pi's texts) live in the binary, and so does pi's coach state: the per-session state file, keyed by the pi session id.

**Build and staleness.**

- `scripts/build-guard` builds it and is a no-op when `orch-guard --version` already equals the fingerprint of the sources; `build-guard --print-fingerprint` prints that fingerprint. `scripts/orch` runs it when it rebuilds orch or the binary is missing, and `install_settings.py --apply` runs it first. The binary is git-ignored.
- On every call the binary re-hashes its sources under the repository it was built from. If they changed it still answers with its own logic, and starts a detached `scripts/build-guard` (one at a time, a lock in the state dir; output in `guard-build.log` there). A build that fails is not started again for the same sources for 10 minutes (`guard-build-failed` in the state dir holds the fingerprint and time); changed sources start a build at once. `orch doctor` warns when the binary is missing, not executable or stale.
- Tests: `cd tool && go test ./guard/... ./cmd/...`; the pi path runs the real extension in node against the real binary in `TestCtxReminder*` (`tool/ctx_reminder_test.go`).

## Tool guard

The rules live in `tool/guard/rules.go`; each row names a rule the binary enforces. Three block: `deny-sed-inplace`, `deny-read-range` and `deny-big-read`. The lookup blocks (`deny-search-first`, `deny-grep-rg`, `deny-find`, `deny-find-chains`, `deny-cat-head-tail`, and pi's `deny-builtin-grep` and `deny-builtin-find`) are removed: a search just runs, and `orch-guard enrich` (below) adds language-server facts after it.

| Rule group | Rule | Blocks | Does not touch |
|---|---|---|---|
| `sed-inplace` | `deny-sed-inplace` | `sed`/`gsed` with `-i`, `-i.bak`, `-i ''` or `--in-place`; `perl` with `-i` or `-pi`; `gawk`/`awk -i inplace` with a file; a `sed`, `awk` or `perl` whose only output is a `>` redirect to a temp file that a later `mv`, `cp` or `cat T >` puts back over one of its files, or whose piped output a later `sponge` writes over one of its files | `sed` that prints to stdout, a piped transform (`git diff \| sed 's/^/  /'`), `grep -i`; bulk edits through `find -exec sed -i` or `xargs sed -i`, on purpose, since one `Edit` per file is too costly until there is an LSP rename |
| `read-range` | `deny-read-range` | a line-range read of one plain file whose output goes nowhere else: `sed -n '10,20p' F` (also `7p`, `10,+5p`, `10,$p`, `10,20!d`, `20q`), `head [-n N] F`, `tail -n +N F`, `awk 'NR>=10 && NR<=20' F` (also `NR==5`, `NR<=20`, `NR==10,NR==20`, with `{print}`), and the two-segment lines `head -n B F \| tail -n K`, `tail -n +A F \| head -n N`, `cat [-n] F \| sed\|awk\|head ...` | a whole-file `cat`, `tail -n N` (last N lines), `head -c`, `tail -f`, any other pipe or a redirect of stdout (`>`, `>>`, `&>`; a `2>` of stderr alone does not count), two files, a regex address (`/x/p`), a bare `$p`, an end before the start, zero counts |
| `big-read` | `deny-big-read` | Claude's `Read` of a regular file over 30720 bytes (`bigReadBytes` in `tool/guard/bigread.go`; a code file also needs over 2000 lines or 102400 bytes) with neither `offset` nor `limit` | a repeat of the same whole `Read` in the same session (the first is blocked and remembered), a read with `offset` or `limit` (either one), `pages`, images (png, jpg, jpeg, gif, webp), PDFs, notebooks (`.ipynb`), a file at or under the limit, a missing path, a directory, and any other tool |

Claude's own `Grep` and `Glob` tools are not guarded, and neither are pi's `grep` and `find`.

**Call origin.** A call a Code Mode script makes is `script`, any other call is `direct` (`callOrigin` in `tool/guard/origin.go`). Claude: the `PreToolUse`/`PostToolUse` `tool_use_id` starts with `repl_` (a direct call's starts with `toolu_`); the script itself is the `REPL` tool. pi: the extension sends `parent` (the event's `parentToolCallId`) on `tool_call` and `tool_result`, and a non-empty one means `script`. A script filters what it reads before anything reaches the model, so `deny-big-read` and `deny-read-range` apply to `direct` calls only; `deny-sed-inplace` applies to both. For pi, the first-search fallback block (no result hook) is also skipped for a script's call.

### What a block says

A block names its replacement: `deny-sed-inplace` always names `Edit`, and when the command has a clear translation it adds a third line `Here: ...` with the exact call (`Edit file_path=a.go old_string="x" new_string="y" replace_all=true` for a literal `s/x/y/g`, or `Read file_path=a.go offset=10 limit=11, then Edit those lines out` for `10,20d`); a regex, several commands or `-e` scripts on one line, more than three files, or an `s` flag other than `g` get no third line. `deny-read-range` names the exact `Read file_path=F offset=A limit=N` (`limit` is left out for an open range) to send as a parallel call. pi (`--harness pi`) gets the same calls with its own tools and parameters: `read path=F offset=A limit=N` and `edit path=F edits=[{"oldText":"x","newText":"y"}]`. pi's edit has no replace-all (each `oldText` must match one place), so a `g` replacement asks for one `edits` entry per occurrence, and pi gets no absolute-path note because its tools take relative paths. Both agents' tool names and parameters live in one `harness` value each (`tool/guard/rules.go`).

### Big whole-file reads

Whatever a session reads stays in context and is re-read on every later turn, so one whole-file `Read` of a large file costs far more than the read. `deny-big-read` refuses a `Read` of a regular file over 30720 bytes (30 KB, exclusive: exactly 30720 passes) when the call has no `offset` and no `limit`; either one alone is enough, because the model chose a range. Exempt: `pages` (PDF), images, PDFs and notebooks, which Claude Code reads specially, a missing path, a directory, and a payload the script cannot read (it fails open). A code file (extensions in `codeExts`) passes up to 2000 lines and 102400 bytes, both inclusive; the byte cap catches generated and minified files. The block names the size (lines and KB for code and docs), says to use Read with offset and limit, and lists an outline as `  line: text` (at most 50 entries, then `... N more; rg -n PATTERN file for the rest`): top-level declarations at column 0 for code, `#` to `###` headings (outside fenced blocks) for `.md`, `.mdx` and `.markdown`. Any other file, or an empty outline, gets the `rg -n PATTERN file` hint. The first whole Read of a big file in a session is blocked and its absolute path is kept in the session state (`big_reads_allowed`, newest 50); the same Read sent again passes, and the path stays allowed for the session. With no `session_id` it blocks every time and omits the "send again" line. A retry with `limit` passes. `ORCH_TOOL_GUARD=off` disables it, and `ORCH_TOOL_GUARD_SKIP=big-read` (or `deny-big-read`) disables the rule alone. The limit has no variable of its own.

A blocked command stays blocked on every retry. Two switches are the human escapes, same as pi:

- `ORCH_TOOL_GUARD=off` disables all the rules.
- `ORCH_TOOL_GUARD_SKIP=sed-inplace,big-read` disables rules by short name or full name.

`prefer-read-tool.sh` stays. It is a non-blocking reminder for the wider reader set (`less`, `more`, `bat`, `nl`, and `sed` without `-n` as well), so it is not replaced by anything in the binary. It is a shell script and is not part of the binary.

## LSP enrichment

`orch-guard enrich` reads one PostToolUse event on stdin and prints only the `hookSpecificOutput` JSON, or nothing. The Claude coach calls the same code (`Enrich` in `tool/guard/enrich.go`) on every PostToolUse, so a registered coach hook enriches too; the `plugins/lsp-enrich` plugin registers `enrich` alone (Bash and Grep) for installs without the coach. It needs no `ORCH_TOOL_GUARD` or `ORCH_LOOKUP_COACH` setting.

When a Bash `rg`, `grep -r`/`-R` or `git grep`, or the Grep tool, searches for symbol names, it appends `lsp <Name> (<kind>): <signature>` plus a few indented lines (definition, callers, references, implementations) limited to the search's own scope, and drops what the search output already showed. The facts come from `orch lsp def|refs|callers|impls --json` (binary `$ORCH_BIN`, else `orch` on PATH, run from the event cwd). It is silent for plain-text patterns (`-i`, `-F` with a space, doc-only targets, names under 4 characters or not identifiers), a name with no definition or more than 10, a scope it cannot parse, a name already enriched in the session while the dirty files are unchanged, and any failure. The total budget is 1500 ms (`ORCH_LSP_ENRICH_BUDGET_MS`); when the first query times out it starts `orch lsp warm <cwd>` detached and stays silent. Output shrinks with the number of names (1 name up to 12 lines, 2 up to 5, 3-4 two each, 5 or more the first 4 plus `+K names: orch lsp def A B C`), ending with `more: orch lsp <verb> <name>` when something was cut. When it fires, the `symbol-via-lsp` reminder is dropped.

`ORCH_LSP_ENRICH=off` disables it. Every decision is logged as one JSON line in `<ORCH_CACHE_DIR, else the user cache dir>/orchestrate/lsp-enrich.jsonl` (not gated by `ORCH_LOOKUP_LOG`; `ORCH_CACHE_DIR` is used as given).

## Diagnostics after edits

After a file edit the guard pushes the file's new type errors to the model without being asked. Claude Code: a PostToolUse hook on `Edit|Write|MultiEdit` runs `orch-guard coach`; pi: the `tool_result` of `edit` and `write`. Code is `tool/guard/diag.go`.

- **Direct edit** (the model's own call): `orch lsp diag <abs path> --json --wait-ms <budget-200> --limit 50` runs from the event cwd (binary `$ORCH_BIN`, else `orch` on PATH). Only `error` items count. Errors already reported for that file in this session are dropped (state keyed by path and message, so an error whose line moved is not repeated); a file that comes back with no errors forgets its keys, so a reintroduced error is reported again. The output is `additionalContext` (pi: an appended text part) that starts with `orch diagnostics:`, then up to 8 lines `path:line: message` (path relative to the repo root) and `... N more: orch lsp diag <path>` when there are more; the errors past the cap stay unreported and show up on the next check. Nothing new means no output.
- **Script edit** (an edit inside a Code Mode script: Claude `tool_use_id` starting with `repl_`, pi `parent` set): the path is recorded in the session state (`diag_pending`) and nothing is output. When the script's own call ends (Claude PostToolUse of `REPL`, pi `tool_result` of `codemode`) one `orch lsp diag p1 p2 ...` runs for every recorded path and one message with the same format and cap is output; the record is cleared.
- A file with no language server (`no_server`) or that does not exist is silent. When the budget runs out the guard starts `orch lsp warm <repo>` detached, outputs nothing and keeps the recorded paths for the next `REPL`.
- The budget is 2500 ms (`ORCH_LSP_DIAG_BUDGET_MS`). `ORCH_LSP_DIAG=off`, or `ORCH_TOOL_GUARD=off`, turns it off.
- Every decision is one JSON line in `<ORCH_CACHE_DIR, else the user cache dir>/orchestrate/lsp-diag.jsonl`, beside `lsp-enrich.jsonl`: `ts`, `harness`, `session`, `origin`, `paths`, `outcome` (`reported`, `none`, `no_server`, `timeout`, `off`, `error`, or `pending` for a recorded script edit), `errors` (all errors the files had), `reported` (lines shown) and `ms`.

## Lookup coach

A non-blocking coach, in both harnesses, that logs every lookup and reminds the worker when it uses the wrong tool or spends one turn per lookup. It never blocks and always exits 0; any error fails open silently. `orch-guard coach` runs as a PostToolUse and PostToolUseFailure hook (matcher `Bash|Read|LSP|REPL|mcp__jbcontext__.*|mcp__orch__.*`) and, with no matcher, as a UserPromptSubmit hook. The pi side is `orch-guard coach --harness pi`, driven by `tool/pi_ctx_reminder.ts`: the extension sends `tool_execution_end` (the duration), `tool_result` (the reminder comes back as text to append) and `before_agent_start` (a user prompt; the ladder comes back as a message); it stays silent in the restricted gate shell.

What counts as a lookup is decided by `coach_cases.json`, shared by both harnesses: the first command of a shell line (after `VAR=x` and a leading `cd DIR &&`) is `jbcontext search` (`jbcontext`), `orch lsp <verb>` (`lsp:<verb>`), `rg` or `fd`; anything else, and an `rg` or `grep` later in a pipe, is not. Claude's `Read` and pi's `read` are `read` (query `path:offset-limit`), Claude's `LSP` is `lsp:<operation>`, `mcp__jbcontext__*` is `jbcontext`, `mcp__orch__search` is `search` (it counts like `jbcontext` for `concept-first` and for what `followed` means), and `mcp__orch__lsp` is `lsp:<verb>` from `tool_input.verb`. The `REPL` and `Poll` tools (Claude) and `codemode` (pi) are not lookups; a `REPL` event only records in the session state that the session is in Code Mode.

**Log.** One JSON line per lookup in `$ORCH_TOOL_GUARD_STATE_DIR/lookups.jsonl` (default `~/.local/state/orchestrate/lookups.jsonl`), written with one call: `ts`, `harness`, `session`, `repo`, `turn`, `tool`, `query` (200 characters at most), `ok`, `empty`, `bytes`, `duration_ms`, `reminder`, `followed` and `origin` (`direct` or `script`; absent on rows from before it existed, which are direct). `rg`, `fd` and `orch lsp` exiting 1 with no output are `ok` and `empty`; a `jbcontext search` that prints "No relevant results found" is `empty`. A turn is the id of the assistant message that made the call (Claude: found through `transcript_path` and `tool_use_id`, in the last 512 KB of the transcript; pi: the `turn_start` index). Per-session state lives in `<state dir>/coach/<session_id>.json`, locked with `flock` because parallel calls fire their hooks at once.

**Reminders**, text prefixed `orch lookup coach: `. A prompt window runs from one user prompt to the next (a prompt counter in the session state). `concept-first` and `symbol-via-lsp` fire at most once per prompt window and `lookup-ladder` at most once per prompt; the batch kinds stay at once per session:

| Kind | Fires when |
|---|---|
| `lookup-ladder` | on a user prompt (UserPromptSubmit; pi `before_agent_start`), before any search: the session's first prompt, and afterwards only a prompt that follows a window whose first search (`rg` or `fd`) came before any `jbcontext` or `orch lsp` call. It reads `Before searching: location unknown -> jbcontext search ...; known symbol -> orch lsp ...; exact text -> rg. ... Read known files or ranges with Read, not cat or sed -n.` and writes no lookup row |
| `concept-first` | the window's first `rg` or `fd` comes before any `jbcontext` or `orch lsp` call in that window |
| `symbol-via-lsp` | an `rg` pattern is a symbol (CamelCase, snake_case, `pkg.Name`, or any identifier with `-w`, after stripping `\b`, `\<`, `\>`, a leading `func `/`def `/`class `/`type `/`interface ` and a trailing `(`), unless every `-g`/`-t`/path targets md, txt, json, yaml, yml or toml |
| `batch-lookups` | on the first lookup of a new turn, the previous two consecutive turns each held exactly one non-read lookup |
| `read-batch` | on the first lookup of a new turn, the previous two consecutive turns each held exactly one read and no other lookup |

`followed` is set on the first lookup after a `lookup-ladder` (true for `jbcontext` or `lsp:*`), a `concept-first` (true only for `jbcontext`) or `symbol-via-lsp` (true only for `lsp:*`) reminder; the batch reminders leave it null. A reminder reaches Claude as `additionalContext` on the hook's own event. The ladder is only recorded in the session state, so `orch lookups report` does not count it; its `followed` shows only on the log row.

**Script calls.** A lookup with origin `script` is logged like any other, and it still counts as seen (a `search` or `lsp:*` call satisfies `concept-first`), but it fires no `concept-first`, `symbol-via-lsp`, `batch-lookups` or `read-batch` reminder, is not counted in a turn, and gets no LSP enrichment. pi never gets `appendText` for a nested call (the result goes to the script, and a note would corrupt the data it parses), and the extension drops any text the binary returns for one.

**Code Mode ladder.** When the session is in Code Mode, the `lookup-ladder` (same once-per-prompt rules) reads `Before searching: location unknown -> tools.mcp__orch__search({query}); known symbol -> tools.mcp__orch__lsp({verb, names}); exact text -> $`rg ...`. Batch independent lookups in one script and return only what you need. Read ranges with file(path).slice(offset, limit).` Claude: the hook environment has `CLAUDE_CODE_EXPERIMENTAL_CODE_MODE=1`, or the session state (`code_mode`) has seen a `REPL` call. pi: the extension sends `codemode: true` on `before_agent_start` when `pi.getActiveTools()` includes `codemode`.

**Switches.** `ORCH_LOOKUP_COACH=off` disables the reminders (the ladder too), `ORCH_LOOKUP_LOG=off` the log, and `ORCH_TOOL_GUARD=off` both.

**Report.** `orch lookups report [--since 7d] [--repo PATH] [--json]` reads the log: per harness, sessions, lookups, lookups per turn, the share of sessions whose first search was jbcontext or lsp, `rg` calls with a symbol pattern, the empty and error rate per tool, reminders fired and followed, and the same read repeated in a session.

Tests: `cd tool && go test ./guard/...` (the Claude hook, temp state dir, synthetic transcript), `go test -run 'CtxReminder|Lookups' .` (the pi extension in node against the real binary, and the report).

## Tests

`toolguard_cases.json` is one decision table for both harnesses: every case names the command, the PATH situation (`fd-rg` with both installed, `rg` only, `fd` only, or `none`) and the rule that must block it, or no rule when it is allowed. Its `readCases` table covers `deny-big-read` (file name, size in bytes, extra `Read` arguments, block or allow; the test makes the files in a temp directory, and pi's `read` tool runs the same table). Its `builtinCases` table pins that pi's own `grep` and `find` tools are allowed. The cases of the removed lookup rules are kept with no rule, so a removed block cannot come back unnoticed.

`tool/guard/toolguard_test.go` runs every case through the real binary with only stub tools on PATH, plus the two switches, the non-Bash and broken-stdin paths, the reasons byte for byte and the big-read rule with its fail-open paths. `TestCtxReminderSharedToolguardCases` and `TestCtxReminderSharedBuiltinSearchCases` (`tool/ctx_reminder_test.go`) run the same file through the pi extension and the binary, so a disagreement between the two harnesses fails a test.

```sh
cd tool && go test ./guard/... ./cmd/...
cd tool && go test -run CtxReminder .
```
