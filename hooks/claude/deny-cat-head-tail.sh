#!/usr/bin/env bash
# PreToolUse hook (matcher: Bash). Blocks rule deny-cat-head-tail from the
# shared guard: a bare cat or sed -n over exactly one file, which
# floods the context with raw text.
# head and tail are allowed. Does not touch: several files at once, a pipe on either
# side (cat f | grep TODO), a redirect (cat f > copy) and
# cat reading stdin.
# Replacement: the block text names the exact Read call (file_path, and offset and
# limit for a sed -n range) to send as a parallel call, whether or not orch is
# installed.
set -u
unset CDPATH
dir=$(cd -- "$(dirname -- "$0")" && pwd) || exit 0
command -v python3 >/dev/null 2>&1 || exit 0
exec python3 "$dir/toolguard.py" cat-head-tail
