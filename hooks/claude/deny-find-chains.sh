#!/usr/bin/env bash
# PreToolUse hook (matcher: Bash). Blocks two rules from the shared guard:
#   deny-find-chains  find -exec/-execdir/-ok/-okdir/-delete, and a search tool
#                     (find, fd, rg, ag) feeding grep/ag or xargs grep/rg/ag.
#   deny-find         a plain find over a tree, which is a file search.
# Does not touch: a find reading stdin (it formats a pipe), a quoted word or a
# path that merely contains "find", fd feeding rg (it filters file names), and
# any command that only lists a directory.
# Replacement: fd. Without fd a plain find is allowed; a chain names a plain find or rg.
set -u
unset CDPATH
dir=$(cd -- "$(dirname -- "$0")" && pwd) || exit 0
command -v python3 >/dev/null 2>&1 || exit 0
exec python3 "$dir/toolguard.py" find-chains
