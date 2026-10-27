#!/usr/bin/env bash
# PreToolUse hook (matcher: Bash). Blocks rule deny-grep-rg from the shared
# guard: grep, egrep, fgrep, ag or git grep over files or a directory, while rg
# is on PATH. With no rg there is no replacement, so nothing is blocked.
# Does not touch: rg itself, a grep filtering another command's output (go
# test ./... | grep FAIL), grep -q as a condition, grep reading stdin and a
# quoted word.
# Replacement: rg.
set -u
unset CDPATH
dir=$(cd -- "$(dirname -- "$0")" && pwd) || exit 0
command -v python3 >/dev/null 2>&1 || exit 0
exec python3 "$dir/toolguard.py" grep-rg
