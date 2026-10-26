# Claude Code hooks

Scripts here run only once registered in `~/.claude/settings.json`. `settings-fragment.json` lists exactly which hooks to register, and `install_settings.py` merges it in. The commands point straight at this folder, no symlink, so the skill must be installed at `~/.agents/skills/orchestrate`.

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

These are the Claude side of the pi guard in `tool/pi_ctx_reminder.ts`, which stays the source of truth. `toolguard.py` is a line for line port of its five shell rules and its two built-in rules; each shell script below is a thin wrapper that names the rule group it enforces, so a script is one file to read and one block list to change.

| Script | Rule | Blocks | Does not touch |
|---|---|---|---|
| `deny-find-chains.sh` | `deny-find-chains`, `deny-find` | `find` with `-exec/-execdir/-ok/-okdir/-delete`; a search tool (`find`, `fd`, `rg`, `ag`) feeding `grep`/`rg`/`ag` or `xargs grep`; a plain `find` over a tree | a `find` reading stdin (it formats a pipe), a quoted word or a path that merely contains `find`, `fd` alone |
| `deny-sed-inplace.sh` | `deny-sed-inplace` | `sed`/`gsed` with `-i`, `-i.bak`, `-i ''` or `--in-place`; `perl` with `-i` or `-pi` | `sed` that prints to stdout, a piped transform (`git diff \| sed 's/^/  /'`), `grep -i` |
| `deny-cat-head-tail.sh` | `deny-cat-head-tail` | a bare `cat`, `head`, `tail` or `sed -n` over exactly one file | `tail -f`/`--follow`, several files, a pipe on either side, a redirect, `cat` reading stdin |
| `deny-grep-rg.sh` | `deny-grep-rg` | `grep`, `egrep`, `fgrep`, `ag`, `git grep` over files or a directory, and `rg` too while `orch` is healthy | a `grep` filtering another command's output (`go test ./... \| grep FAIL`), `grep -q` as a condition, `grep` reading stdin, a quoted word, `orch ctx` itself, and `rg` when `orch` is not usable |
| `deny-builtin-search.sh` | `deny-builtin-grep`, `deny-builtin-glob` | Claude's own `Grep` and `Glob` tools while `orch` is healthy, because they reach the tree without a shell | an `ls` or a `Read`, a search whose path is absolute and outside the session's git repository, a payload with no pattern, and any other tool |
| `deny-big-read.sh` | `deny-big-read` | Claude's `Read` of a regular file over 30720 bytes (`BIG_READ_BYTES` in `toolguard.py`) with neither `offset` nor `limit` | a read with `offset` or `limit` (either one), `pages`, images (png, jpg, jpeg, gif, webp), PDFs, notebooks (`.ipynb`), a file at or under the limit, a missing path, a directory, and any other tool |

The replacement is the same order as pi: one `orch ctx` call first when `orch` is healthy, then `fd` for a file search and `rg` for a content search, then the built-in Claude tools (`Glob`, `Grep`, `Edit`). Two rules name one fixed tool whichever tools are installed: `sed -i` always names `Edit`, because `orch ctx` is read-only, and `deny-cat-head-tail` always names `Read` with offset and limit (a section or the whole file), never `orch ctx`.

### The built-in tools and the repository boundary

`Grep` and `Glob` are how a session escapes the shell rules without touching a shell, so while `orch` is healthy they are blocked too, and both name one `orch ctx batch` first. With no usable `orch` they are the fallback pi's rules point at, so the hook allows them, and `Read`, `Bash` and every other tool are untouched.

`orch ctx` is pinned to a git repository root, so a search outside the session's repository has no replacement. The hook walks up from its working directory to the nearest `.git` entry (a worktree root has a file, not a directory) and blocks a built-in search only when its path is inside that root. A relative path, or none at all, counts as inside; an absolute path outside it (`/private/var/folders/...`, `/tmp/...`, `~/.claude/...`) is allowed, and so is any absolute path when the working directory is in no repository at all. A payload without a tool name, a tool input or a pattern is allowed as well.

### Big whole-file reads

Whatever a session reads stays in context and is re-read on every later turn, so one whole-file `Read` of a large file costs far more than the read. `deny-big-read` refuses a `Read` of a regular file over 30720 bytes (30 KB, exclusive: exactly 30720 passes) when the call has no `offset` and no `limit`; either one alone is enough, because the model chose a range. Exempt: `pages` (PDF), images, PDFs and notebooks, which Claude Code reads specially, a missing path, a directory, and a payload the script cannot read (it fails open). The block is three lines: the file size in KB, "use Read with offset and limit", and how to get the line numbers (`orch ctx text` or `orch ctx sym`, a doc's headings with `orch ctx text "^#{1,3} "`; `rg -n` instead while `orch` is not healthy). It stays blocked on retry with the same input, and a retry with `limit` passes. `ORCH_TOOL_GUARD=off` disables it, and `ORCH_TOOL_GUARD_SKIP=big-read` (or `deny-big-read`) disables the rule alone. The limit has no variable of its own.

### A broken `orch` is not a replacement

`orch` counts as available only when `orch --version` exits 0 within three seconds. A wrapper that is on PATH and cannot find its tool directory fails that probe, so the guard treats it exactly as if it were absent: the three search rules name `fd`, `rg` or the built-in tools instead, a plain `rg` is not blocked, and each of those blocks ends with one more line:

```
orch ctx is installed but broken here (cd: /path/tool: No such file or directory); use the tools above and tell the user.
```

The text in parentheses is the first line of the probe's error, cut at 120 characters. `deny-sed-inplace` and `deny-cat-head-tail` never add that line: they always name `Edit` and `Read`, which `orch` does not back. A block that points at a tool that cannot answer only teaches the model to work around the guard.

Every hook call is a fresh process, so the probe result is cached for 60 seconds in `~/.local/state/orchestrate/toolguard-health.json` as `{ts, ok, detail}`. `ORCH_TOOL_GUARD_STATE_DIR` points the cache somewhere else (the tests use it), and a stale or unreadable cache is simply re-probed. Pi probes once at extension startup instead, because one session is one process.

A blocked command stays blocked on every retry. Two switches are the human escapes, same as pi:

- `ORCH_TOOL_GUARD=off` disables all five scripts.
- `ORCH_TOOL_GUARD_SKIP=find-chains,sed-inplace,cat-head-tail,grep-rg` disables rules by short name or full name. Plain `find` has its own id, `find` (or `deny-find`), inside `deny-find-chains.sh`. Built-in searches answer to the group name `builtin-search`, or to one rule's own name (`builtin-grep`, `builtin-glob`, `deny-builtin-grep`, `deny-builtin-glob`).

`prefer-read-tool.sh` stays. It is a non-blocking reminder for the wider reader set (`less`, `more`, `bat`, `nl`, and `sed` without `-n` as well), so it complements `deny-cat-head-tail.sh`; it is not replaced by it. Register both if you want the block and the reminder.

## Tests

`toolguard_cases.json` is one decision table shared with the pi guard: every case names the command, the PATH situation (`orch` on PATH, `orch` on PATH but a stub that exits 1, `fd` and `rg` but no `orch`, or neither; a situation's optional `broken` list names the tools whose stub exits 1) and the rule that must block it, or no rule when it is allowed. Its `readCases` table covers `deny-big-read` (file name, size in bytes, extra `Read` arguments, block or allow; the test makes the files in a temp directory, and pi's `read` tool runs the same table). Its `builtinCases` table covers the built-in search tools, where the tool name and the rule id differ by harness: each case names a harness-neutral `tool` (`content-search` for pi `grep` and Claude `Grep`, `file-search` for pi `find` and Claude `Glob`), a pattern, a target path and the rule each harness must block with, so the repository boundary is tested the same way on both sides. `toolguard_test.py` runs every case through the real shell scripts with only stub tools on PATH, plus `bash -n`/`shellcheck`, the two switches, the non-Bash and broken-stdin paths, the broken-`orch` block, the rg exception, the 60-second cache (one probe for two calls, a stale cache re-probed, a fresh one trusted), the 120-character detail, the reasons byte for byte, the built-in table with its own switches, repository boundary and fail-open paths, and the task's required examples:

```sh
python3 hooks/claude/toolguard_test.py
```

The pi extension runs the same file in `TestCtxReminderSharedToolguardCases` and `TestCtxReminderSharedBuiltinSearchCases` (`tool/ctx_reminder_test.go`) and compares blocked vs allowed and the rule name, so a disagreement between the two implementations fails a test.
