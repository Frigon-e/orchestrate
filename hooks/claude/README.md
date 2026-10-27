# Claude Code hooks

Scripts here run only once registered in `~/.claude/settings.json`. `settings-fragment.json` lists exactly which hooks to register, and `install_settings.py` merges it in. The commands point straight at this folder, with no symlink in between. The skill is reached at `~/.agents/skills/orchestrate`, which may itself be a symlink to a checkout (for example `~/Development/orchestrate`).

```sh
python3 ~/.agents/skills/orchestrate/hooks/claude/install_settings.py           # show what would change, write nothing
python3 ~/.agents/skills/orchestrate/hooks/claude/install_settings.py --apply   # merge into ~/.claude/settings.json
python3 ~/.agents/skills/orchestrate/hooks/claude/install_settings.py --check   # exit 1 while a hook is missing
python3 ~/.agents/skills/orchestrate/hooks/claude/install_settings.py --apply --remove   # take them out again
```

- The installer owns only hooks whose command points into this folder. Every other key and hook (other tools' installers, your own) is left as found, and an existing hook is never duplicated, so running it twice changes nothing.
- `--apply` keeps a timestamped `settings.json.bak-*` next to the file and writes through a symlinked `settings.json`. A file that is not valid JSON is never touched.
- `--stdin` reads settings JSON on stdin and prints the merged result, for a dotfiles manager that wants to own the write (for example a chezmoi `modify_settings.json` script). Add `--remove` there to unmerge.
- Tests: `python3 hooks/claude/install_settings_test.py`.

- Make each script executable (`chmod +x`).
- The hook reads its event as JSON on stdin. Exit 0 to allow, exit 2 to block (stderr goes back to Claude).
- Event names, matchers and the JSON shape: https://docs.claude.com/en/docs/claude-code/hooks

## Tool guard

These are the Claude side of the pi guard in `tool/pi_ctx_reminder.ts`, which stays the source of truth. `toolguard.py` is a line for line port of its five shell rules; each shell script below is a thin wrapper that names the rule group it enforces, so a script is one file to read and one block list to change. `deny-grep-rg` keeps its id and script name for compatibility: it now blocks `grep`, `egrep`, `fgrep`, `ag` and `git grep`, and never `rg`.

| Script | Rule | Blocks | Does not touch |
|---|---|---|---|
| `deny-find-chains.sh` | `deny-find-chains`, `deny-find` | `find` with `-exec/-execdir/-ok/-okdir/-delete`; a search tool (`find`, `fd`, `rg`, `ag`) feeding `grep`/`ag`, or `xargs grep`/`rg`/`ag`; a plain `find` over a tree while `fd` is on PATH | a `find` reading stdin (it formats a pipe), `fd ... \| rg pattern` (it filters file names), a quoted word or a path that merely contains `find`, `fd` alone, and a plain `find` when `fd` is not installed |
| `deny-sed-inplace.sh` | `deny-sed-inplace` | `sed`/`gsed` with `-i`, `-i.bak`, `-i ''` or `--in-place`; `perl` with `-i` or `-pi` | `sed` that prints to stdout, a piped transform (`git diff \| sed 's/^/  /'`), `grep -i` |
| `deny-cat-head-tail.sh` | `deny-cat-head-tail` | a bare `cat` or `sed -n` over exactly one file | `head`, `tail`, several files, a pipe on either side, a redirect, `cat` reading stdin |
| `deny-grep-rg.sh` | `deny-grep-rg` | `grep`, `egrep`, `fgrep`, `ag`, `git grep` over files or a directory, while `rg` is on PATH | `rg` itself, a `grep` filtering another command's output (`go test ./... \| grep FAIL`), `grep -q` as a condition, `grep` reading stdin, a quoted word, and every `grep` when `rg` is not installed (there is no replacement to name) |
| `deny-big-read.sh` | `deny-big-read` | Claude's `Read` of a regular file over 30720 bytes (`BIG_READ_BYTES` in `toolguard.py`) with neither `offset` nor `limit` | a read with `offset` or `limit` (either one), `pages`, images (png, jpg, jpeg, gif, webp), PDFs, notebooks (`.ipynb`), a file at or under the limit, a missing path, a directory, and any other tool |

Claude's own `Grep` and `Glob` tools are not guarded: there is no `deny-builtin-search.sh`, and `install_settings.py --apply` removes an owned hook that the fragment no longer lists, so an older install loses it on the next run.

### What a block says

A search block is two lines: what is blocked with the replacement, then one ladder line.

```
orch tool guard (deny-grep-rg): grep, egrep, fgrep, ag and git grep over files are blocked; use rg (rg -n PATTERN path, -g 'GLOB', -F literal, -w word, -i).
Lookup order: jbcontext search "<one behavior>" if the location is unknown, orch lsp def|refs|callers <Name> for a known symbol, rg for exact text, fd for file names; send independent lookups in one message.
```

`deny-find` and `deny-find-chains` name `fd` the same way (`fd PATTERN dir`, `fd -e go`, `fd -g '*.ts'`; a chain says "use fd, or rg -g 'GLOB' PATTERN for content"). With no `fd` on PATH a plain `find` is allowed (Claude Code does not load `Glob` by default on macOS, so there is nothing else to name), and a chain names a plain `find` or `rg` and skips the ladder. `deny-sed-inplace` always names `Edit` and `deny-cat-head-tail` always names `Read` with offset and limit (a section or the whole file).

### Big whole-file reads

Whatever a session reads stays in context and is re-read on every later turn, so one whole-file `Read` of a large file costs far more than the read. `deny-big-read` refuses a `Read` of a regular file over 30720 bytes (30 KB, exclusive: exactly 30720 passes) when the call has no `offset` and no `limit`; either one alone is enough, because the model chose a range. Exempt: `pages` (PDF), images, PDFs and notebooks, which Claude Code reads specially, a missing path, a directory, and a payload the script cannot read (it fails open). The block is three lines: the file size in KB, "use Read with offset and limit", and how to get the line numbers (`rg -n PATTERN file`, a doc's headings with `rg -n "^#{1,3} " file`). It stays blocked on retry with the same input, and a retry with `limit` passes. `ORCH_TOOL_GUARD=off` disables it, and `ORCH_TOOL_GUARD_SKIP=big-read` (or `deny-big-read`) disables the rule alone. The limit has no variable of its own.

A blocked command stays blocked on every retry. Two switches are the human escapes, same as pi:

- `ORCH_TOOL_GUARD=off` disables all the scripts.
- `ORCH_TOOL_GUARD_SKIP=find-chains,sed-inplace,cat-head-tail,grep-rg` disables rules by short name or full name. Plain `find` has its own id, `find` (or `deny-find`), inside `deny-find-chains.sh`.

`prefer-read-tool.sh` stays. It is a non-blocking reminder for the wider reader set (`less`, `more`, `bat`, `nl`, and `sed` without `-n` as well), so it complements `deny-cat-head-tail.sh`; it is not replaced by it. Register both if you want the block and the reminder.

## Lookup coach

A non-blocking coach, in both harnesses, that logs every lookup and reminds the worker when it uses the wrong tool or spends one turn per lookup. It never blocks and always exits 0; any error fails open silently. `lookup-coach.sh` runs `toolguard.py coach` as a PostToolUse and PostToolUseFailure hook (matcher `Bash|Read|LSP|mcp__jbcontext__.*`) and, with no matcher, as a UserPromptSubmit hook. The pi side is the same file as the guard, `tool/pi_ctx_reminder.ts`, which logs on `tool_execution_end`, appends a reminder to the tool result and answers `before_agent_start` (a user prompt) with a message; it stays silent in the ctx-only gate shell.

What counts as a lookup is decided by `coach_cases.json`, shared by both implementations: the first command of a shell line (after `VAR=x` and a leading `cd DIR &&`) is `jbcontext search` (`jbcontext`), `orch lsp <verb>` (`lsp:<verb>`), `orch ctx <sub>` (`ctx:<sub>`), `rg` or `fd`; anything else, and an `rg` or `grep` later in a pipe, is not. Claude's `Read` and pi's `read` are `read` (query `path:offset-limit`), Claude's `LSP` is `lsp:<operation>`, and `mcp__jbcontext__*` is `jbcontext`.

**Log.** One JSON line per lookup in `$ORCH_TOOL_GUARD_STATE_DIR/lookups.jsonl` (default `~/.local/state/orchestrate/lookups.jsonl`), written with one call: `ts`, `harness`, `session`, `repo`, `turn`, `tool`, `query` (200 characters at most), `ok`, `empty`, `bytes`, `duration_ms`, `reminder` and `followed`. `rg`, `fd` and `orch lsp` exiting 1 with no output are `ok` and `empty`; a `jbcontext search` that prints "No relevant results found" is `empty`. A turn is the id of the assistant message that made the call (Claude: found through `transcript_path` and `tool_use_id`, in the last 512 KB of the transcript; pi: the `turn_start` index). Per-session state lives in `<state dir>/coach/<session_id>.json`, locked with `flock` because parallel calls fire their hooks at once.

**Reminders**, text prefixed `orch lookup coach: `. A prompt window runs from one user prompt to the next (a prompt counter in the session state). `concept-first` and `symbol-via-lsp` fire at most once per prompt window and `lookup-ladder` at most once per prompt; the batch kinds stay at once per session:

| Kind | Fires when |
|---|---|
| `lookup-ladder` | on a user prompt (UserPromptSubmit; pi `before_agent_start`), before any search: the session's first prompt, and afterwards only a prompt that follows a window whose first search (`rg` or `fd`) came before any `jbcontext` or `orch lsp` call. It reads `Before searching: location unknown -> jbcontext search ...; known symbol -> orch lsp ...; exact text -> rg.` and writes no lookup row |
| `concept-first` | the window's first `rg` or `fd` comes before any `jbcontext` or `orch lsp` call in that window |
| `symbol-via-lsp` | an `rg` pattern is a symbol (CamelCase, snake_case, `pkg.Name`, or any identifier with `-w`, after stripping `\b`, `\<`, `\>`, a leading `func `/`def `/`class `/`type `/`interface ` and a trailing `(`), unless every `-g`/`-t`/path targets md, txt, json, yaml, yml or toml |
| `batch-lookups` | on the first lookup of a new turn, the previous two consecutive turns each held exactly one non-read lookup |
| `read-batch` | on the first lookup of a new turn, the previous two consecutive turns each held exactly one read and no other lookup |

`followed` is set on the first lookup after a `lookup-ladder` (true for `jbcontext` or `lsp:*`), a `concept-first` (true only for `jbcontext`) or `symbol-via-lsp` (true only for `lsp:*`) reminder; the batch reminders leave it null. A reminder reaches Claude as `additionalContext` on the hook's own event. The ladder is only recorded in the session state, so `orch lookups report` does not count it; its `followed` shows only on the log row.

**Switches.** `ORCH_LOOKUP_COACH=off` disables the reminders (the ladder too), `ORCH_LOOKUP_LOG=off` the log, and `ORCH_TOOL_GUARD=off` both.

**Report.** `orch lookups report [--since 7d] [--repo PATH] [--json]` reads the log: per harness, sessions, lookups, lookups per turn, the share of sessions whose first search was jbcontext or lsp, `rg` calls with a symbol pattern, the empty and error rate per tool, reminders fired and followed, and the same read repeated in a session.

Tests: `python3 hooks/claude/coach_test.py` (real hook, temp state dir, synthetic transcript), `cd tool && go test -run 'CtxReminder|Lookups' .` (the pi extension in node, and the report).

## Tests

`toolguard_cases.json` is one decision table shared with the pi guard: every case names the command, the PATH situation (`fd-rg` with both installed, `rg` only, `fd` only, or `none`) and the rule that must block it, or no rule when it is allowed. Its `readCases` table covers `deny-big-read` (file name, size in bytes, extra `Read` arguments, block or allow; the test makes the files in a temp directory, and pi's `read` tool runs the same table). Its `builtinCases` table covers pi's own `grep` and `find` tools, which are blocked while `rg` (for grep) or `fd` (for find) is on PATH and allowed for a path outside the session's repository. `toolguard_test.py` runs every case through the real shell scripts with only stub tools on PATH, plus `bash -n`/`shellcheck`, the two switches, the non-Bash and broken-stdin paths, the reasons byte for byte, the big-read hook with its switches and fail-open paths, and the task's required examples:

```sh
python3 hooks/claude/toolguard_test.py
```

The pi extension runs the same file in `TestCtxReminderSharedToolguardCases` and `TestCtxReminderSharedBuiltinSearchCases` (`tool/ctx_reminder_test.go`) and compares blocked vs allowed and the rule name, so a disagreement between the two implementations fails a test.
