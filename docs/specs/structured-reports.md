---
harness: claude
model: sonnet
effort: medium
base: main
---
# Structured reports

## Behaviour
Every report starts with a short decision block that is all an orchestrator
needs, read in one call. Gate reports put blockers (in full), decision
questions and next actions first, list concerns one line each, and move
evidence detail and bookkeeping to `details.md`. Findings get IDs (F1, F2, ...)
so briefs cite `orch show gate/<hash> --finding F3` instead of line ranges.
Scout and ship reports get YAML front matter with the fields the orchestrator
relays.

A decision model (the existing `decider` role, OpenRouter) sorts gate concerns
into likely noise and possibly real. It starts in shadow mode: it only records
its answers, and every concern stays listed. When switched on, it collapses
only concerns it is confident are noise. Any failure leaves the report fully
listed: the worst case is today's behaviour, never a hidden concern.

Why: a 2026-10-28 audit found gate reports of 38-46 KB, about 70% finding
evidence that orchestrators never relayed, read in 3-5 ranged calls because of
the 30 KB Read limit; 39 of 44 concerns in one run were noise.

## Design rules
- Nothing is deleted: every fact the gate writes today is still written, in `report.md` or `details.md`.
- `gate.json` and its reducer replay do not change. Display IDs are derived at render time, never stored.
- Display ID order: verified blockers, then verified concerns, in `rec.Findings` order, numbered F1..Fn; unverified findings continue the numbering and appear only in `details.md`.
- Blockers are never sent to the sorter and never collapsed.
- The `## Question cards` section and its rewrite in `tool/gate_enrich.go#addReportSection` keep their exact heading.
- Model calls never run on the gate publication path: the sorter runs in the detached enrichment worker, like question classification.
- Sorter failure of any kind (no key, decider off, timeout, HTTP error, missing answer, malformed reply, budget cap) leaves that concern listed and records the reason. A partial run collapses only concerns that have a complete, confident answer.
- Old reports are never rewritten; old line-range citations keep working.
- New front matter is checked with a warning only in this spec, never a refusal.

## Authority
May decide: internal names, file layout inside owned files, test layout, exact wording of report lines
Must escalate: front matter field names or meanings, sorter thresholds that collapse more than this spec allows, any change to gate.json or the gate state
Never: push, merge, edit files another package owns, change the gate verdict logic

## Packages

### gate-report: Gate report.md and details.md layout
kind: ship
model: sonnet
owns: tool/gate.go, tool/gate_report_test.go, tool/bench_capture.go, tool/gate_reduce_test.go, tool/gate_pane_test.go, tool/gate_checklist_off_test.go, tool/gate_record_compat_test.go, tool/lookup_check_test.go, tool/plan_worktree_test.go, tool/runners_test.go, tool/gate_experiment_test.go, tool/subject_test.go, tool/gate_checkout_test.go, tool/provider_outage_test.go, tool/question_classifier_test.go

Rewrite `tool/gate.go#writeGate` (gate.go:1799-1912) to write two files.

`report.md`, in this order:
1. YAML front matter: `kind: gate`, `spec`, `spec_version`, `state`, `completion: <done>/<required>`, `counts: {blocker, concern, unverified}`, `questions: <n blocking>`, `details: details.md`, `sorter: off|shadow|on` (from config; the sorter package fills real status later).
2. The tree warning, if any, then `# Gate: <spec> (<state>)`.
3. `## Decision`: one or two generated lines (state, blocker count, blocking question count, first next action).
4. `## Blockers`: `F<n> [<source>] <where>: <claim> (<evidence>) <why>` - full line, as today.
5. `## Questions that need a decision`, `## Next actions`, `## Approval reasons`, `## Deterministic issues` (only when non-empty).
6. `## Passes that did not run`: one line per pass with its reason (needed to explain an incomplete state); kept-session paths go to details.
7. `## Concerns`: `F<n> <where>: <claim>` - no evidence or why. Mark the section with the HTML comment `<!-- concerns -->` on its own line before the list so the sorter can replace the list.
8. `## Minor open questions` as today.
9. `## Question cards` (written by gate_enrich, unchanged).

`details.md`: `# Gate details: <spec> (<hash>)`, then one `### F<n>` block per finding (verified and unverified) with severity, source, where, claim, evidence, why, verified, support, support assessment; then the sections that leave report.md: lookup_tool/shell line, Tokens, reuse checklist line, coverage, cache, kept-session paths, Runners, Packages covered by each group pass, Replies that were not valid JSON, Notes, Exploration per pass.

Expose `gateDisplayIDs(rec *gateRecord) map[int]string` (index in rec.Findings to F-id) for the show and sorter packages.

`tool/bench_capture.go`: copy `details.md` next to `report.md` in snapshots when it exists (report-only import at bench_capture.go:554 included). Older snapshots without it stay valid.

Update the listed tests that assert report text; review each changed assertion, do not regenerate blindly. Byte-identical report tests (provider_outage, question_classifier) keep comparing bytes of the new layout.

#### Builds
- Writes report.md with front matter, decision block, full blockers and one-line concerns.
  id: report-layout
  reuse: tool/gate.go#writeGate
- Writes details.md with per-finding evidence and the bookkeeping sections.
  id: details-file
- Derives stable display IDs from rec.Findings order without storing them.
  id: display-ids
- Bench snapshots include details.md.
  id: bench-details
  reuse: tool/bench_capture.go

#### Acceptance
`go test -p 2 -count=1 -run 'Gate|Bench|Lookup|Plan|Runner|Subject|Provider|Classifier' ./...` in tool/ passes. A new test in gate_report_test.go builds a record with 2 blockers, 5 concerns and 1 unverified finding and checks: report.md starts with front matter, blockers carry evidence, concerns do not, F-ids are F1-F7 in rule order with F8 only in details.md, and every evidence string appears in details.md. On a real 46 KB gate record from testdata or a fixture, report.md is under 12 KB.

### concern-sorter: Decision-model sorting of gate concerns
kind: ship
model: sonnet
depends: gate-report
owns: tool/gate_sort.go, tool/gate_sort_test.go, tool/gate_enrich.go, tool/paths.go, tool/triage.go

Add config `concern_sort = "off" | "shadow" | "on"` (default `shadow`) to `tool/paths.go#config`.

In the detached enrichment worker (`tool/gate_enrich.go`, next to question classification; see its comment at gate_enrich.go:700), for each verified concern call the decider through `tool/decisions.go#postDecisionsWith` with state `{where, claim, evidence, why, spec_excerpt}` (each capped at 2000 characters) and these questions:
- `real_defect` (noul): would a user or a test see wrong behaviour if this is ignored?
- `covered` (noul): is this already covered by an existing test, the spec text or another finding?
- `category` (choice): `covered`, `style`, `naming`, `real_risk`, `unclear`.

Collapse rule, thresholds as named constants in gate_sort.go: collapse when P(real_defect) < 0.2, category is `covered`, `style` or `naming` with probability at least 0.8, and confidence at least 0.7. Anything else stays listed.

Limits: at most 4 concurrent requests, 30 s timeout each (as triage), at most 80 concerns per gate run (the rest stay listed); the sorter never uses the gate RunBudget.

Record every answer and every failure to `gate/<hash>/concern-sort.jsonl` (concern F-id, finding hash, answers, decision, model served, error reason).

Modes:
- `off`: no calls. Concerns section unchanged.
- `shadow`: calls and records; report.md unchanged except the front matter `sorter: shadow`.
- `on`: replace the list under `<!-- concerns -->` with the listed concerns, then one line `Collapsed as likely noise: F9, F12, ... (<n>; covered <a>, style <b>, naming <c>). orch show gate/<hash> --concerns lists them.` Set front matter `sorter: on`.

Worst case: if the decider is unavailable (`tool/triage.go#site.deciderOff` returns a reason), or any request fails, the affected concerns stay listed and report.md gets the line `Sorter unavailable (<reason>); all concerns listed.` under `## Concerns`, front matter `sorter: unavailable`. A crash or kill of the worker leaves the report as gate-report published it.

Add `orch triage report --concerns`: joins concern-sort records with outcomes (a concern counts as acted on when its `gate/<hash> F<n>` or its where/claim appears in a later task.md, a lead report or a commit message under the run root) and prints, per category, decisions, acted-on rate and how many collapsed concerns were later acted on. Needs at least 20 outcomes before it prints a rate (like `triageMinOutcomes`).

#### Builds
- Sorts verified gate concerns with the decider in the detached worker and records each answer.
  id: sort-concerns
  reuse: tool/decisions.go#postDecisionsWith, tool/gate_enrich.go
- Collapses only confident noise when concern_sort is on; leaves every failed or unsure concern listed.
  id: collapse-safe
- Reports shadow decisions against outcomes.
  id: sort-report
  reuse: tool/triage.go#cmdTriage

#### Acceptance
`go test -p 2 -count=1 -run 'Sort|Enrich|Triage' ./...` in tool/ passes. Tests with a fake decider cover: shadow leaves report.md bytes unchanged apart from front matter; on collapses only answers that meet all three thresholds; a timeout, an HTTP 500, a missing answer and decider-off each leave every affected concern listed and write the unavailable line; blockers are never sent; the Question cards rewrite still works after a concerns rewrite and in the reverse order.

### show-command: orch show
kind: ship
model: sonnet
depends: gate-report
owns: tool/show.go, tool/show_test.go, tool/main.go

`orch show <task-id | gate/<hash>> [--finding F1,F3-F5] [--concerns] [--section <heading>] [--all]`, registered in `tool/main.go` next to `report`.

- No flags: front matter as one summary line, then the decision part: for a gate, Decision, Blockers, Questions, Next actions; for a lead report, everything above `## Findings` or the first 60 lines.
- `--finding`: the matching `### F<n>` blocks from details.md, one call for any number of IDs.
- `--concerns`: every concern line, including collapsed ones, from details.md.
- `--section`: one `##` section by heading (case-insensitive prefix match).
- `--all`: report.md then details.md, no limit (for a lead that needs everything).
- Output limit without `--all`: 10,000 characters, cut at a line boundary, then `<n> more characters; use --finding, --section or --all`.
- A report without front matter (older reports): print its `##` headings with line numbers, then the first 150 lines, and say it is an old-format report.
- Unknown task or gate hash, or missing report: exit 1 with the path it looked for.

#### Builds
- Prints a report's decision part in one call.
  id: show-default
- Prints findings, concerns or one section by ID or heading.
  id: show-select
- Handles old reports without front matter.
  id: show-legacy

#### Acceptance
`go test -p 2 -count=1 -run 'Show' ./...` in tool/ passes, with tests for each flag, the 10,000-character cut, an old-format report and a missing report.

### report-docs: Report templates and reading rules
kind: ship
model: sonnet
depends: show-command
owns: SKILL.md, references/lead.md, references/task-template.md, references/gate.md, tool/lead.go, tool/lead_report_test.go

Docs:
- `SKILL.md` around line 90: the orchestrator runs `orch show gate/<hash>` and `orch show <task>`, judges blockers from that output, does not open finding detail, and cites F-ids and section names in briefs. Line 149: same for scout reports.
- `references/lead.md` around lines 42-44 and 141: report templates. Scout front matter: `kind: scout`, `task`, `verdict` (one line), `decisions` (ids), `findings` (count), `carry_forward` (ids); sections Verdict, Decisions needed, Carry forward, Findings (`F<n> path:line - claim`), then free text. Ship front matter: `kind: ship`, `task`, `branch`, `head`, `check: {ok, failed}`, `conflicts`, `unverified` (list), `carry_forward` (ids); sections only when there is something to carry forward. Fields summarise and never replace the prose.
- `references/task-template.md` line 23: Pointers cite `orch show <id> --finding F<n>` or `--section <heading>`, not line ranges.
- `references/gate.md`: describe report.md, details.md, F-ids and the sorter modes and worst case.

Code: in `tool/lead.go#cmdReport`, on state `done`, warn (exit 0) when report.md is missing, has no front matter, or lacks the required fields for its `kind`. Text: `report.md: missing <fields> (see references/lead.md "Report")`.

#### Builds
- Orchestrator and lead docs use orch show and F-ids.
  id: docs-reading
- Lead report templates with front matter.
  id: docs-templates
- orch report done warns on missing report front matter.
  id: done-warning
  reuse: tool/lead.go#cmdReport

#### Acceptance
`go test -p 2 -count=1 -run 'Report' ./...` in tool/ passes, with tests for done with a valid scout report, a valid ship report, a missing report and missing fields (all exit 0, warnings only where expected). `rg -n 'lines? [0-9]+' references/task-template.md` finds no line-range pointer advice.

## Open questions
None.
