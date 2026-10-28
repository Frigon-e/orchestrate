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
Scout and ship reports get flat front matter with the fields the orchestrator
relays.

A decision model (a new `concern_sorter` role on the Decider route) sorts gate
concerns into likely noise and possibly real. It starts in shadow mode: it only
records its answers, and every concern stays listed. When promoted, it
collapses only concerns it is confident are noise. Any failure leaves the
report fully listed: the worst case is today's behaviour, never a hidden
concern.

Why: a 2026-10-28 audit found gate reports of 38-46 KB, about 70% finding
evidence that orchestrators never relayed, read in 3-5 ranged calls because of
the 30 KB Read limit; 39 of 44 concerns in one run were noise.

## Design rules
- Nothing is deleted: every fact the gate writes today is still written, in `report.md` or `details.md`.
- `gate.json` and its reducer replay do not change. Display IDs are derived at render time by `gateDisplayIDs`, never stored.
- Display ID order: verified blockers, then verified concerns, in `rec.Findings` order, numbered F1..Fn; unverified findings continue the numbering and appear only in `details.md`.
- Blockers are never sent to the sorter and never collapsed.
- The `## Question cards` section and its rewrite in `tool/gate_enrich.go#addReportSection` keep their exact heading.
- No model call runs on the gate publication path. The sorter is queued with a content-addressed job manifest and runs in a detached worker, following `tool/question_classifier.go#classifierQueue`; it is queued from cmdGate for every non-experiment gate with at least one verified concern, independent of question enrichment.
- Sorter fail-safe: a concern is collapsed only when all three answers are present (each key exists in the reply with a non-nil probability map) and meet every threshold. A missing key, a missing probability, a timeout, an HTTP error, a malformed reply, the 80-concern cap, a disabled or unrouted role, or a killed worker each leave the concern listed, and the reason is recorded.
- Report front matter is flat `key: value` lines that `tool/task.go#readTask` parses; lists are comma-separated on one line.
- Old reports (no front matter) are never rewritten; old line-range citations keep working.
- New front matter is checked with a warning only in this spec, never a refusal.

## Authority
May decide: internal names, file layout inside owned files, test layout, exact wording of report lines
Must escalate: front matter field names or meanings, sorter thresholds that collapse more than this spec allows, any change to gate.json or the gate state
Never: push, merge, edit files another package owns, change the gate verdict logic

## Packages

### concern-sorter: Decision-model sorting of gate concerns
kind: ship
model: sonnet
owns: tool/gate_sort.go, tool/gate_sort_test.go, tool/triage.go, tool/paths.go, tool/runners.go

Add role `concern_sorter` to `modelRoles` (tool/runners.go:40) with the same Decider default route and api-harness rules as `question_classifier` (tool/paths.go:393-408). The role's `promote` flag (allow it for this role) switches collapse on; default is shadow (record only). `enabled = false` turns it off.

In `tool/gate_sort.go`:
- `gateDisplayIDs(rec gateRecord) map[int]string`: index in rec.Findings to F-id, per the ID design rule. gate-report and show-command call it.
- `const concernsMarker = "<!-- concerns -->"`: gate-report writes it on its own line right after `## Concerns`; the list below it, up to the next blank line, is what the worker may replace.
- `queueConcernSorter(dir string, rec gateRecord, cfg config) string`: called by gate-report from cmdGate after report.md is written. Returns the publish-time status for front matter: `off` (role disabled), `none` (no verified concerns, or experiment), `unavailable` (no Decider route), `shadow` or `on` (queued). Queues one job manifest per run in `gate/<hash>/concern-sort-jobs/`, content-addressed like classifierQueue, so `orch gate enrich` or a rerun on the same inputs does not reissue it; launches the detached worker like the classifier does. Reuse classifier queue, lock and append helpers where they are generic; do not copy them.
- Worker: for each verified concern (cap 80), call `tool/decisions.go#postDecisionsWithContext` with a 30 s deadline per request, at most 4 concurrent, with state `{where, claim, evidence, why}` (each capped at 2000 characters) and questions:
  - `real_defect` (noul): would a user or a test see wrong behaviour if this is ignored?
  - `covered` (noul): is this already covered by an existing test, the spec text or another finding?
  - `category` (choice): `covered`, `style`, `naming`, `real_risk`, `unclear`.
- Collapse rule (named constants): all three keys present with probabilities; P(real_defect true) < 0.2; the chosen category is `covered`, `style` or `naming` with probability at least 0.8; and the minimum confidence across the three answers is at least 0.7.
- Records: append to `gate/<hash>/concern-sort.jsonl` one line per concern with run_id (`rec.At` and subject digest, as classifier RunID), F-id, finding digest (where+claim), answers, decision (`listed`, `collapse`), reason, model served, error.
- Report rewrite, only on a report.md whose front matter has `kind: gate` and whose `run_at` equals this job's `rec.At` (otherwise do nothing): in promote mode, replace the list under concernsMarker with the listed concerns and one line `Collapsed as likely noise: F9, F12 (<n>; covered <a>, style <b>, naming <c>). orch show gate/<hash> --concerns lists them.`; on any failure add `Sorter unavailable (<reason>); listed concerns are not sorted.` under the marker and set front matter `sorter: unavailable`. In shadow mode never touch report.md except to set `sorter: unavailable` on total failure. Write atomically (temp file and rename) under the same lock addReportSection uses or an equivalent per-file lock, so a Question cards rewrite and a concerns rewrite in either order both survive.

`orch triage report --concerns` (extend `tool/triage.go#triageReport`, reuse its outcome threshold `triageMinOutcomes`): a sorted concern counts as acted on when a task.md or report.md under the run root, modified after the record's run time and before the next run of the same gate hash, contains `gate/<hash>` and its F-id, or contains its `where` text verbatim. Print per category: decisions, acted-on rate, and how many `collapse` decisions were acted on.

#### Builds
- Derives display IDs and defines the concerns marker contract.
  id: display-ids
- Queues a detached sorter job for every non-experiment gate with verified concerns and returns its publish status.
  id: queue-sorter
  reuse: tool/question_classifier.go#classifierQueue, tool/question_classifier.go#appendClassifier
- Asks the decider with a 30 s deadline and records every answer and failure with its run id.
  id: sort-concerns
  reuse: tool/decisions.go#postDecisionsWithContext
- Collapses only complete, confident noise answers in promote mode and leaves every other concern listed.
  id: collapse-safe
- Reports shadow decisions against outcomes.
  id: sort-report
  reuse: tool/triage.go#triageReport

#### Acceptance
`go test -p 2 -count=1 -run 'Sort|Triage|Config|Runner' ./...` in tool/ passes. Tests with a fake decider cover: a reply missing `real_defect`, one missing its probabilities, a timeout, an HTTP 500, a disabled role and no route each leave every affected concern listed and record the reason; shadow leaves report.md bytes unchanged; promote collapses only answers meeting all four conditions; blockers are never sent; a second queue call with the same inputs issues no job; a rewrite after a Question cards rewrite, and the reverse order, keeps both sections; a report.md with a different `run_at` is not touched.

### gate-report: Gate report.md and details.md layout
kind: ship
model: sonnet
depends: concern-sorter
owns: tool/gate.go, tool/gate_report_test.go, tool/bench.go, tool/bench_capture.go, tool/gate_reduce_test.go, tool/gate_pane_test.go, tool/gate_checklist_off_test.go, tool/gate_record_compat_test.go, tool/lookup_check_test.go, tool/plan_worktree_test.go, tool/runners_test.go, tool/gate_experiment_test.go, tool/subject_test.go, tool/gate_checkout_test.go, tool/provider_outage_test.go, tool/question_classifier_test.go

Rewrite `tool/gate.go#writeGate` (tool/gate.go:1799-1912) to write two files.

`report.md`, in this order:
1. Flat front matter: `kind: gate`, `spec`, `spec_version`, `run_at` (rec.At), `state` (rec.State, or `legacy` for a record that is not gateStatusSchemaVersion), `completion: <done>/<required>`, `incomplete`, `optional` (v2 only), `blockers`, `concerns`, `unverified`, `questions` (blocking count), `details: details.md`, `sorter` (the status queueConcernSorter returns; `none` when it is not called).
2. The tree warning, if any, then `# Gate: <spec> (<state>)`; for a legacy record, the `Historical result: ...` line from tool/gate.go:1829 directly under the title.
3. `## Decision`: one or two generated lines (state, blocker count, blocking question count, first next action).
4. `## Blockers`: `F<n> [<source>] <where>: <claim> (<evidence>) <why>` - full line, as today.
5. `## Questions that need a decision`, `## Next actions`, `## Approval reasons`, `## Deterministic issues` (each only when non-empty).
6. `## Passes that did not run`: one line per pass with its reason; kept-session paths go to details.
7. `## Concerns`, then `concernsMarker` on its own line, then `F<n> <where>: <claim>` per verified concern (no evidence or why), then a blank line.
8. `## Minor open questions` as today.
9. `## Question cards` (written by gate_enrich, unchanged).

`details.md`: `# Gate details: <spec> (<hash>)`, then `## Findings` with one `### F<n>` block per finding (verified and unverified) whose first line is `F<n> [<severity>, <source>] <where>: <claim>` and then evidence, why, verified, support and support assessment; then the sections that leave report.md: schema and completion line, lookup_tool/shell line, Tokens, reuse checklist line, coverage, cache, kept-session paths, Runners, Packages covered by each group pass, Replies that were not valid JSON, Notes, Exploration per pass.

In cmdGate (tool/gate.go:1273-1285): call `queueConcernSorter` after report.md is written, for every non-experiment run, whether or not enrichment runs; set `view.Details` to `Run orch show gate/<hash> for the decision part; evidence is in <dir>/details.md.`

Bench: `tool/bench.go` and `tool/bench_capture.go` (including the report-only import at tool/bench_capture.go:554) copy `details.md` next to `report.md` when it exists. Older snapshots without it stay valid.

Update the listed tests that assert report text; review each changed assertion, do not regenerate blindly. Byte-identical report tests (provider_outage, question_classifier) keep comparing bytes of the new layout. `TestGateRecordWithRemovedSwitchesPrintsInReport` keeps finding the legacy line in report.md.

#### Builds
- Writes report.md with flat front matter, decision block, full blockers and one-line concerns under the marker.
  id: report-layout
  reuse: tool/gate.go#writeGate
- Writes details.md with per-finding evidence and the bookkeeping sections, losing no fact for v2 or legacy records.
  id: details-file
- Queues the concern sorter from cmdGate and points the gate output at orch show.
  id: gate-wiring
  reuse: tool/gate_sort.go#queueConcernSorter
- Bench snapshots include details.md.
  id: bench-details

#### Acceptance
`go test -p 2 -count=1 -run 'Gate|Bench|Lookup|Plan|Runner|Subject|Provider|Classifier' ./...` in tool/ passes. A new test in gate_report_test.go builds a record with 2 blockers, 5 concerns and 1 unverified finding and checks: readTask parses the front matter; blockers carry evidence, concerns do not; F-ids are F1-F7 in rule order with F8 only in details.md; every evidence string appears in details.md; a legacy record keeps its Historical result line; a gate with concerns and no questions still calls queueConcernSorter. A record with 60 findings renders report.md under 12 KB.

### show-command: orch show
kind: ship
model: sonnet
depends: gate-report
owns: tool/show.go, tool/show_test.go, tool/main.go

`orch show <task-id | gate/<hash>> [--finding F1,F3-F5] [--concerns] [--section <heading>] [--all]`, registered in `tool/main.go` next to `report`. Parse front matter with `tool/task.go#readTask`.

- No flags: front matter as one summary line, then for a gate Decision, Blockers, Questions and Next actions; for a lead report everything above `## Findings`, or the first 60 lines if there is no such heading.
- `--finding`: for a gate, the `### F<n>` blocks from details.md; for a lead report, the lines in report.md's `## Findings` section that start with `F<n>` and their indented continuation lines. One call for any number of IDs; unknown IDs are named in one error line.
- `--concerns`: for a gate, the first line of every concern's `### F<n>` block in details.md (collapsed ones included).
- `--section`: one `##` section by heading, case-insensitive prefix match, searched in report.md first, then details.md; the first match wins and the output names its file.
- `--all`: report.md then details.md.
- Output limit: 10,000 characters for the default view and `--section`, cut at a line boundary, then `<n> more characters; use --finding, --section or --all`. `--finding`, `--concerns` and `--all` are not cut.
- Old reports (no front matter): the default view prints the `##` headings with line numbers and the first 150 lines, labelled old-format; `--finding` and `--concerns` exit 1 with `old-format report: no finding IDs; use orch show <id> or --section`.
- Unknown task or gate hash, or missing report: exit 1 with the path it looked for.

#### Builds
- Prints a report's decision part in one call.
  id: show-default
- Prints findings, concerns or one section by ID or heading for gate and lead reports.
  id: show-select
- Handles old reports without front matter.
  id: show-legacy

#### Acceptance
`go test -p 2 -count=1 -run 'Show' ./...` in tool/ passes, with tests for each flag on a gate and on a scout report, the 10,000-character cut and the uncut flags, `--section` found only in details.md, an old-format report with and without `--finding`, and a missing report.

### report-docs: Report templates and reading rules
kind: ship
model: sonnet
depends: show-command
owns: SKILL.md, references/lead.md, references/task-template.md, references/gate.md, tool/lead.go, tool/lead_report_test.go

Docs:
- `SKILL.md` around line 90: the orchestrator runs `orch show gate/<hash>` and `orch show <task>`, judges blockers from that output, does not open finding detail, and cites F-ids and section names in briefs. Line 149: same for scout reports.
- `references/lead.md` around lines 42-44 and 141: report templates with flat front matter. Scout: `kind: scout`, `task`, `verdict` (one line), `decisions` (comma-separated ids), `findings` (count), `carry_forward` (comma-separated ids); sections Verdict, Decisions needed, Carry forward, Findings (`F<n> path:line - claim`), then free text. Ship: `kind: ship`, `task`, `branch`, `head`, `check_ok`, `check_failed`, `conflicts`, `unverified` (semicolon-separated), `carry_forward`; body sections only when there is something to carry forward. Fields summarise and never replace the prose.
- `references/task-template.md` line 23: Pointers cite another report with `orch show <id> --finding F<n>` or `--section <heading>`, never by line range. Code pointers keep `path#Symbol` and `path:line`.
- `references/gate.md`: describe report.md, details.md, F-ids, the concern_sorter role, shadow and promote, and the worst case.

Code: in `tool/lead.go#cmdReport`, on state `done`, warn (exit 0) when report.md is missing, has no front matter, has a `kind` other than `scout` or `ship` (or none), or lacks the required fields for its kind. Text: `report.md: <problem> (see references/lead.md "Report")`.

#### Builds
- Orchestrator and lead docs use orch show and F-ids.
  id: docs-reading
- Lead report templates with flat front matter.
  id: docs-templates
- orch report done warns on missing or invalid report front matter.
  id: done-warning
  reuse: tool/lead.go#cmdReport, tool/task.go#readTask

#### Acceptance
`go test -p 2 -count=1 -run 'Report' ./...` in tool/ passes, with tests for done with a valid scout report, a valid ship report, a missing report, a missing kind, an unknown kind and missing fields (all exit 0, warnings only where expected). In references/task-template.md, the Pointers guidance names `orch show` and no longer advises citing a report by line range (`rg -n 'orch show' references/task-template.md` matches).

## Open questions
None.
