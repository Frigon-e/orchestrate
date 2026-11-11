# lsp-enrich

After a Bash `rg`, `grep -r` or `git grep`, or the Grep tool, searches for a symbol name, this plugin's PostToolUse hook runs `orch-guard enrich` and appends a few `orch lsp` facts the search output did not already show (the definition, callers, references in the searched scope). It stays silent for plain-text searches, names that are not symbols, a language server that is not warm, and anything the agent already saw. The binary is `$ORCH_GUARD_BIN`, else `~/.agents/skills/orchestrate/hooks/bin/orch-guard`; `ORCH_LSP_ENRICH=off` disables it, and every decision is logged to `lsp-enrich.jsonl` in the orchestrate cache directory.

A SessionStart hook runs `orch lsp warm` detached so the first search finds a running server; its failure is ignored. Install with `claude --plugin-dir plugins/lsp-enrich` or through a local marketplace.
