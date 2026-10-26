# Gate internals

`orch gate <spec>` and `orch plan <spec>` in short are in SKILL.md; the spec
format and the gate verdict summary are in `spec-format.md`. This file holds
what the orchestrator needs only when it reads a gate report or tunes a run.
Model roles, connector health and outages: `model-roles.md`, `outage.md`.
Benchmarks: `bench.md`.

## Passes

The gate checks pointers and acceptance commands, then runs read-only model
passes: one per group of up to 4 related packages for scope, questions and
reuse, plus two adversaries. A pass is capped at 45 minutes, ends early when
its session shows no activity for 6, and a timed-out or stalled group pass is
retried once. Token use per pass is logged in `gate.json` `pass_tokens`. The
gate keeps only claims whose `path:line` evidence exists.

`orch gate <spec> --dry-run` lists the passes and their sizes without calling
a model. A model verdict is input, not truth: Flash can miss a flaw and the
gate can fail a fine spec, so the call is the orchestrator's and the user's.

The passes run in the panes of a `gate-<hash>` tab (at most six, closed when
the gate ends). If the tab cannot be opened no pass runs, the report says so
and the state is `incomplete`. Outside herdr `orch gate` refuses to run (only
`--dry-run` works). Each pass is a real pi or Claude agent in a pane named
after it, which you can watch and attach to. A pass that failed keeps its
session and output under `gate/<hash>/` for 7 days and for the newest 5 gate
runs; a run without `gate.json` is still running and is never pruned until its
directory is 7 days old.

## Question cards

After a run, with no model call, `orch gate` enriches its question cards (also
on demand: `orch gate enrich <spec>`). Only exact duplicates after whitespace
normalization share a card; related questions keep their IDs, routes and
answer states. Original duplicate identities and text remain on the shared
card. Spec lines and code from `jbcontext` are candidate hints, not proof that
an answer exists or is absent. Each card's front matter and index give its
route (`owner`, `lead-may-decide`, `unclear`) and action.

Keep product choices with the owner; settle delegated technical choices with
the lead (`settle-by-lead`), without forwarding them to the user again; never
infer an answer from a hint. Enrichment preserves `questions/answered/` and
does not reopen matching archived cards. For Go and TypeScript symbols a
question names, `gopls` and the TypeScript 7 native server (`tsc --lsp`, found
in the project's own `typescript@7` package under `web/node_modules/.pnpm`;
override with `ORCH_TSLS`) add exact facts (definition, callers outside tests,
test references, implementations).

Read `gate/<hash>/questions/INDEX.md` first: counts, then a table per package
with owner-tagged questions on top. Route delegated cards to the lead before
preparing user decisions. The tag and hints are advice; `orch ask --cards`
still asks every root card and does not filter by route.

## State and output

Gate records and v2 output use only `state`: `error`, `stale`, `fail`,
`incomplete`, `pass`, plus the non-approval `experiment` mode. Precedence is
error > stale > fail > incomplete > pass. Completion, reasons, responsible
actors and next actions remain visible even when a higher-priority state wins.
Only a current, subject-bound `pass` whose recorded inputs still reduce to
pass can authorize `orch plan`. Default `orch gate` stdout is the v2 TOON
selection; `--human` selects readable JSON and `--json` selects machine JSON of
the same facts. This does not change `orch ctx` output or its legacy JSON
contract.

`--runner pi:<model>,claude:haiku:low` overrides gate choices for one run;
`--degraded` permits approved weaker choices.

## Context tools for passes

Gate agents use the plain `orch ctx` CLI, not a server or generated adapter.
`--ctx-tool on|off` defaults to on: a read-only notice is first in the shared
prompt prefix, `ORCH_CTX_ROOT` pins lookups to the code checkout, and a PATH
shim resolves `orch` to this binary. `--ctx-shell ctx-only` is the default for
Pi: native `read` plus a shell limited to one `orch ctx` call. Pi starts with
read alone; the loaded guard enables bash and acknowledges activation. If the
extension cannot be written or loaded, the pass is an execution failure, never
an open-shell fallback. `--ctx-shell open` restores Pi's native read/search
tools and unrestricted bash. Claude ignores this setting and adds Bash with the
`Bash(orch ctx:*)` permission. Prefer one `orch ctx batch` over several greps;
read what it returns before searching again. With ctx off, the original read
and search tools remain and the ctx notice/env are absent.

Repository context files are never loaded: pi gets `--no-context-files`;
Claude uses `--settings '{"claudeMdExcludes":["**"]}'` instead of `--bare`, so
disabling CLAUDE.md discovery does not disable subscription authentication.
The checklist stays in the task text, not in a context file. `ctx_tool` and
the effective `ctx_shell` (`open` with ctx off) are in gate/experiment
records, reports, and compare output. Old records without `ctx_shell` still
display `open`.

Every runner, including ctx-off runs, gets a post-pass unchanged-checkout
check against the pre-pass baseline: Git status, diff and untracked-file
content hashes. A change discards model results, cancels pending/in-flight
passes, sets state to `stale` (or `error` if the subject cannot be checked),
and puts a loud warning in stdout/report. The record names changed files and
passes in flight, not a proven culprit. Files are never reverted. Ignored
files and writes outside the checkout are not covered; edits restored before
the check can also escape detection. Pi's default ctx-only shell blocks
unrelated commands, pipes and redirects. With `--ctx-shell open`, its shell is
unrestricted and can write ignored/outside files or use the network. Native
read can still read outside the repo in either mode. The guard, prompt, pinned
read-only ctx mode and post-pass check are not an operating-system sandbox.

## Options and experiments

The gate puts the plain package text and the reuse checklist in each group
prompt. Each pass is given a turn budget (10 plus 5 per package, 40 for
adversaries): at the budget a pane agent is told once to answer, at 1.4 times
it, or when its context passes 150000 tokens, it is interrupted and asked for
the answer; `gate.json` `pass_tokens` records turns, budget hits, cost and
context. Sessions of passes that succeeded are dropped; experiments keep them.

To compare settings without touching the real record: `orch gate <spec> --only
<group-key> --experiment <name>` runs just those group passes into
`gate/<hash>/experiments/<name>/` (sessions kept), and `orch gate compare
<spec> <expA> <expB>` prints requests, tokens, cost, seconds, findings,
questions and their overlap, with an exploration profile per pass.

## Reuse checklist

Items are only Builds bullets (ids `<package>#<n>`, saved in `items.json` with
`queries` and `reuse` arrays); nothing is guessed from body or acceptance. A
ship package without Builds gets a warning and skips its checklist; scouts are
exempt. More than 8 bullets warns to split the package. The checklist (ids,
bullet text, author pointers, search lines as plain text) is put in the group
prompt and saved in `items.json`, with no lookups and no candidates;
`gate.json` `checklist: plain` marks it. Search lines and `reuse:` pointers
are the spec author's hints for the reviewer, shown as written.

`orch gate compare <spec> <a> <b> --reference <file.json>` adds per-item
columns and precision/recall against an answer key (format:
`[{"package","item" (id or text),"verdict" (duplicate|extend|new|unclear),
"candidate","note"}]`). Per-item display matches by id or 60% word overlap;
reference scoring uses locations, not item text.
