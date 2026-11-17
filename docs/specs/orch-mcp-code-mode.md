---
harness: claude
model: sonnet
effort: medium
base: main
---
# orch mcp and Code Mode

## Behaviour
orch's code intelligence becomes reachable as typed MCP tools, so a Claude Code
Code Mode script (`tools.mcp__orch__search(...)`) or a pi codemode script can
chain lookups and return only what it needs. The agent sees two tools with an
obvious split: `search` for a concept whose location is unknown (jbcontext,
through orch's engine) and `lsp` for a known symbol or file (def, refs, callers,
impls, symbols, hover, diagnostics). orch also pushes new type errors after
edits, so the agent gets them without choosing a tool.

The hooks become Code Mode aware: a call made inside a script is enforced and
logged, but guards that exist only to protect context (big whole-file reads,
line-range shell reads) do not block it, because a script's data never enters
context unless the script returns it.

Why: Code Mode (Claude Code 2.1.323+, `CLAUDE_CODE_EXPERIMENTAL_CODE_MODE=1`;
pi 1.5 `codemode`) moves tool calls into scripts. A 2026-11-17 probe in a Code
Mode session showed orch's `deny-big-read` refusing a script's Read that only
counted lines, and that the CLI's text output has to be parsed by scripts.

## Facts this design rests on (probe, Claude Code 2.1.326, 2026-11-17)
- A call made by a script fires `PreToolUse`/`PostToolUse` under its usual name
  (`Read`, `Edit`, `Write`, `Bash`, `mcp__<server>__<tool>`) with
  `tool_use_id` = `repl_<uuid>`. Direct calls have `toolu_...`. The script
  itself is the `REPL` tool (`tool_input.code`); a `Poll` tool call follows it.
- Script globals `edit`, `write`, `file(path).text()/slice()` and `` $`cmd` ``
  go through Edit, Write, Read and Bash. The script VM has no `fs`, no
  `import`, no `fetch`.
- `PostToolUse` `additionalContext` from a script's calls reaches the model as
  a separate message; Claude Code dedupes them and caps them per script run, and
  drops them for background scripts.
- MCP tools are `tools.mcp__<server>__<tool>(input)`; found with
  `searchTools(q)` and `describeTool(name)`. Claude Code builds script types
  from MCP input schemas only (not output schemas), and resolves a call to the
  parsed result body.
- pi: nested calls go through `tool_call`/`tool_result` handlers with
  `parentToolCallId`; their results reach the script, not the model. MCP tools
  with `outputSchema` resolve to `structuredContent`. Registering a server with
  `exposure: "codemode"` turns `codemode` on for the session.

## Design rules

### orch mcp (stdio server)
- `orch mcp` runs an MCP server on stdin/stdout with the official Go SDK
  (`github.com/modelcontextprotocol/go-sdk`, v1.8.x). Logs go to stderr only;
  nothing but protocol frames is written to stdout.
- Server name `orch`; `instructions` (one paragraph): search for concepts,
  lsp for known symbols, rg for exact text; batch lookups in one script and
  return only what is needed.
- Exactly two tools, both annotated `readOnlyHint: true`,
  `openWorldHint: false`, and both with an `outputSchema`:
  - `search` input `{query: string, path?: string, limit?: number (default 10, max 50), dir?: string}`;
    output `{status: "ok"|"no_index"|"rate_limited"|"error", message?: string, hits: {path, start, end, snippet: string[]}[], notes?: string[]}`.
    Backed by `codeEnv.Concept` (`tool/code_search.go:414`) in process.
    `no_index`, disabled index policy, and rate limits are a result with that
    status and `isError: false`; only a bad input is `isError: true`.
  - `lsp` input `{verb: "def"|"refs"|"callers"|"impls"|"symbols"|"hover"|"diagnostics", names?: string[], path?: string, paths?: string[], line?: number, col?: number, limit?: number (default 30), dir?: string}`;
    output `{verb, results: lspAnswer[]}` where `lspAnswer` is the existing
    struct (`tool/lsp_cmd.go:81-100`). `names` is required for def, refs,
    callers and impls; `path` for symbols; `path`+`line`+`col` (1-based) for
    hover; `paths` for diagnostics. A missing or extra-for-verb field is an
    `isError: true` result naming the verb's required fields.
    It runs the same code path as `orch lsp <verb> ... --json` (daemon first,
    in-process fallback), with output captured, never printed.
- Each tool description ends with the output shape as one TypeScript line,
  because Claude Code scripts do not see `outputSchema`.
- `dir` (absolute, or relative to the server's working directory) selects the
  repo; default is the server's working directory. It must resolve to an
  existing directory; the repo root is found the way `codeEnvFor` finds it.
- A tool call never panics the server: errors become `isError: true` results.
  Calls are served concurrently; the LSP daemon already serializes.

### Diagnostics
- The LSP client stops discarding notifications (`tool/code_tsls.go:348`):
  `textDocument/publishDiagnostics` is stored per URI with the document
  version it answers.
- New verb `orch lsp diag <path>... [--json]`, also `lsp` verb `diagnostics`.
  For each path: open it with its current disk text, or send a full-text
  `didChange` when its content hash changed since it was last sent; then pull
  with `textDocument/diagnostic` when the server advertises
  `diagnosticProvider`, else wait for a `publishDiagnostics` for that version,
  up to a settle budget (default 3000 ms, `--wait-ms`). A timeout returns what
  is known with `status: "partial"`.
- One `lspAnswer` per path: `Items` = `{path, line, kind: "error"|"warning"|"info"|"hint", text: message, name: source or code}`,
  errors first, then by line; `--limit` (default 30) caps items, `Total` the
  rest. Works through the daemon (warm servers) and in process.

### Hooks: call origin
- Origin is `script` when Claude's `tool_use_id` starts with `repl_`, or when
  pi's request carries a non-empty `parent` (the extension sends
  `event.parentToolCallId`). Otherwise `direct`.
- `deny-big-read` and `deny-read-range` apply only to `direct` calls.
  `deny-sed-inplace` and every `orch hook pretool` rule apply to both.
- Lookup coach: every lookup is logged with a new field `origin`. For
  `script` calls: no `concept-first`, `symbol-via-lsp`, `batch-lookups` or
  `read-batch` reminder, and no LSP enrichment. pi: never `appendText` to a
  nested call's result (it would corrupt the data the script parses).
- `REPL` and `Poll` (Claude) and `codemode` (pi) are not lookups.
- `mcp__orch__search` is a lookup of tool `search`; `mcp__orch__lsp` is
  `lsp:<verb>`. `mcp__jbcontext__*` stays classified as today.
- Code Mode ladder: when the session is in Code Mode (Claude: the hook's
  environment has `CLAUDE_CODE_EXPERIMENTAL_CODE_MODE=1`, or the session state
  has seen a `REPL` call; pi: the `codemode` tool is active), the
  UserPromptSubmit ladder reads: location unknown ->
  `tools.mcp__orch__search({query})`; known symbol ->
  `tools.mcp__orch__lsp({verb, names})`; exact text -> `` $`rg ...` ``; batch
  lookups in one script and return only what you need; read ranges with
  `file(path).slice(offset, limit)`.

### Hooks: diagnostics after edits
- Claude `PostToolUse` on `Edit|Write|MultiEdit` (and pi `tool_result` on
  `edit`/`write`), files with a language server only:
  - `direct`: run diagnostics for the file within a 2500 ms budget and add
    errors only, at most 8 lines plus `... N more: orch lsp diag <path>`,
    as `orch diagnostics: path:line: message`. Only errors not already
    reported for that file in this session (state keyed by path and message).
  - `script`: record the path in session state; on `PostToolUse` of `REPL`
    (pi: `tool_result` of `codemode`) run diagnostics once for every recorded
    path and add one message, same format and cap, then clear the record.
- Budget exceeded: start `orch lsp warm` detached, add nothing.
  `ORCH_LSP_DIAG=off` turns it off. Every decision is a line in
  `lsp-enrich.jsonl`'s sibling `lsp-diag.jsonl`.

### Registration
- `scripts/install-guards` registers the server for Claude Code with
  `claude mcp add --scope user orch -- <abs path of scripts/orch> mcp` when
  `claude mcp get orch` does not already show that command; `--remove`
  removes it; `--check` reports it missing or pointing elsewhere.
  `orch doctor` reports the same.
- pi: `tool/pi_ctx_reminder.ts` calls
  `pi.registerMcpServer("orch", {command: <orch bin>, args: ["mcp"], exposure: "codemode", description: ...})`
  on load, except in the restricted gate shell and when pi's MCP support is
  off for the session (`-builtin:mcp` in settings, `--no-mcp`); pi reports a
  server registered without it as an extension error in every session. The orch binary is
  `$ORCH_BIN`, else `orch` on PATH; a missing binary registers nothing and
  warns once.
- Gate runs keep `--strict-mcp-config` with no servers (`tool/runners.go:424`).

## Out of scope
- Removing jbcontext's own MCP server from agent configs (a user decision
  after the end-to-end check).
- Claude Code's native `lspServers` plugins.
- An LSP proxy shared with Claude Code's native LSP tool.

## Acceptance
- `cd tool && go test ./...` scoped to changed packages passes; new tests:
  MCP protocol round trip over in-memory transports (tools/list shows two
  tools with input and output schemas; search and lsp calls return structured
  content; bad input is `isError`); diagnostics against live gopls (skips
  without gopls) and against a fake server for pull and push; guard origin
  table cases (`repl_` and pi `parent`) in the shared case files; coach no
  reminders for script calls; diag push aggregation on `REPL`.
- End to end: a Claude Code Code Mode session and a pi codemode session in
  this repo answer three lookup tasks through `tools.mcp__orch__*`, a whole
  big-file Read inside a script is not blocked, a script that edits a Go file
  into a type error gets one `orch diagnostics:` message, and the lookup log
  shows `origin: "script"` rows.
