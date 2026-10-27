#!/usr/bin/env bash
# PreToolUse hook (matcher: Read). Blocks one rule from the shared guard:
#   deny-big-read  a Read with no offset and no limit of a file over 30 KB.
# A whole big file stays in context for every later turn, so the reason points
# at a ranged Read and at the rg -n lookup that gives the line numbers.
# Does not touch: a Read with offset or limit, pages (PDF), images, PDFs and
# notebooks, a file at or under 30 KB, a missing path, a directory, and any
# other tool.
# Fail open on a payload this script cannot read.
set -u
unset CDPATH
dir=$(cd -- "$(dirname -- "$0")" && pwd) || exit 0
command -v python3 >/dev/null 2>&1 || exit 0
exec python3 "$dir/toolguard.py" big-read
