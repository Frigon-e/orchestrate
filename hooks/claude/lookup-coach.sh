#!/usr/bin/env bash
# PostToolUse and PostToolUseFailure hook (matcher: Bash|Read|LSP|mcp__jbcontext__.*).
# Never blocks. Logs each lookup (jbcontext, orch lsp, rg, fd, Read) to
# lookups.jsonl and, at most once per session per kind, adds a short reminder
# as additionalContext: concept-first, symbol-via-lsp, batch-lookups, read-batch.
# ORCH_LOOKUP_COACH=off silences the reminders, ORCH_LOOKUP_LOG=off the log,
# ORCH_TOOL_GUARD=off both.
# Fail open on anything this script cannot read.
set -u
unset CDPATH
dir=$(cd -- "$(dirname -- "$0")" && pwd) || exit 0
command -v python3 >/dev/null 2>&1 || exit 0
python3 "$dir/toolguard.py" coach
exit 0
