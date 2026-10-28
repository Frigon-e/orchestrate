---
harness: claude
model: sonnet
effort: medium
base: main
---
# Decision model: shared helper, rules and logging

## Behaviour
Every use of the decision model (the Decider route, about $0.0004 and 1-3 s per
call) goes through one helper that makes the call, records the question, the
raw answers, cost and latency, and later joins the real outcome. `orch decisions
report` shows, per use, how the model decided, what it cost, and how well its
answers predicted what actually happened. Each use starts in shadow mode (record
only) and is promoted or cut by the user from that report; failures always fall
back to today's behaviour.

This spec moves triage onto the helper and fixes its tier, removes the question
classifier, and adds the first new use in shadow mode: judging watcher stall,
looping and quiet alerts before they wake the orchestrator.

Why: a 2026-10-28 evaluation found triage's raw difficulty score predicts hard
tasks well (AUC 0.77 over 384 tasks) while its tier does not (0.54), because a
low-confidence bump moves 45% of tasks up a tier and an open-ended rule fires on
a signal that predicts in reverse (AUC 0.43). The question classifier could not
be evaluated at all: nothing records who really answered its questions, it
abstains on 88% of cards, and 47% of its jobs hit its request cap. Triage drops
the cost the API returns. Each use built its own queue and record format.

## Design rules
- Rules for every use, written in `references/decision-model.md`:
  1. Every use declares its outcome (what really happened) and the code that records it, before it ships. No outcome, no use.
  2. Keep raw probabilities and scores in the record; derive decisions from them with named constants; never discard the raw answer.
  3. Fail open: a timeout, HTTP error, missing key, invalid answer or disabled role leaves today's behaviour and records the reason.
  4. Shadow first, time-boxed: a use stays shadow until `orch decisions report` shows at least 100 outcomes for it; then the user promotes or cuts it. Promotion is a config change the user makes, never automatic.
  5. All uses share the helper and the record format; no per-use queues, workers or record files.
- Answers are validated on each answer's raw JSON: a key or field that is missing never reads as zero.
- Records live in the run root (`site.root`) under `decisions/<use>.jsonl`; outcomes are separate lines in `decisions/<use>.outcomes.jsonl` keyed by decision ID, so a record is never rewritten.
- A decision record holds: ID, time, use, subject (task id, lead id or gate hash), a digest of the state sent, the questions, the raw answers, the model served, cost, tokens, latency, error, and the action taken (`shadow`, or what the promoted use did). State text sent is capped per field at 2000 characters.
- The watcher never waits on the decider: alerts print exactly as today; the decision runs alongside with a 10 s deadline.
- Each use has its own role in `modelRoles` (route Decider by default) so it can be disabled alone.

## Authority
May decide: internal names, record field names beyond those listed, report layout, test layout
Must escalate: any threshold that changes routing or suppresses an alert, adding a new use, sending more task or code text to the provider than listed here
Never: push, merge, edit files another package owns, promote a use out of shadow

## Packages

### decision-core: Helper, records and orch decisions report
kind: ship
model: sonnet
owns: tool/decision_log.go, tool/decision_log_test.go, tool/decisions.go, tool/decisions_test.go, tool/main.go, references/decision-model.md

`tool/decisions.go`: add `postDecisionsRawWithContext`, returning each answer as `json.RawMessage` and the usage; `postDecisionsWithContext` becomes a wrapper with unchanged behaviour. `tool/decision_log.go`: `askDecision(ctx, s site, use string, subject string, state, questions map[string]any) (decisionResult, error)` resolves the use's role through `site.role`, calls the raw function, validates presence, appends the record (locked append with `tool/locks.go#withFlock`), and returns the ID, raw and decoded answers. `recordDecisionOutcome(root, use, id string, outcome map[string]any)`. `orch decisions report [--use U] [--since 7d]`: per use, calls, failures by reason, cost total, latency p50/p90, answer distributions, outcome coverage, and for each declared metric (a noul answer vs a boolean outcome, or a score vs a boolean outcome) the AUC and a calibration table by probability band. Write `references/decision-model.md` with the rules, the record format and how to add a use.

#### Builds
- One helper calls the decider, validates raw answers and records cost and latency.
  id: decision-helper
  reuse: tool/decisions.go#postDecisionsWithContext
- Outcomes are recorded separately and joined in a per-use report with AUC and calibration.
  id: decision-report
- The rules for every decision-model use are written down.
  id: decision-rules

#### Acceptance
`go test -p 2 -count=1 -run 'Decision' ./...` in tool/ passes. Tests with a fake decider: a missing `noul` key is invalid, not 0; a timeout, HTTP 500 and a disabled role each record the reason and return an error the caller can fall back on; cost and latency are recorded; two concurrent appends both land; the report computes AUC and calibration bands on a fixed fixture and says when a use has fewer than 100 outcomes.

### triage-on-core: Triage through the helper, with a fixed tier
kind: ship
model: sonnet
depends: decision-core
owns: tool/triage.go, tool/triage_test.go

Triage calls `askDecision` with use `triage`. Tier rule (named constants): `planning` when P(difficulty=3) >= 0.3 or P(cause known) < 0.2; `settled` when expected difficulty < 1.5 and P(verifiable) >= 0.8; otherwise `standard`. Remove the low-confidence bump and the open-ended rule. When a task closes, record its outcome through `recordDecisionOutcome` using `triageOutcome`, with the hard signals split into `lead_signals` (escalate, failed) and `run_signals` (stalled, looping, verify fail, respawn, frozen). `orch triage report` keeps working on the old `triage/triage.jsonl` history and points to `orch decisions report --use triage` for new records.

#### Builds
- Triage records through the helper with cost and its outcome on close.
  id: triage-records
- The tier follows the raw difficulty score.
  id: triage-tier

#### Acceptance
`go test -p 2 -count=1 -run 'Triage' ./...` in tool/ passes, with tier tests at each boundary, a low-confidence answer that keeps its tier, an open-ended answer that no longer forces planning, and an outcome line written on close with both signal groups.

### classifier-removal: Remove the question classifier
kind: ship
model: haiku
owns: tool/question_classifier.go, tool/question_classifier_test.go, tool/runners.go, tool/paths.go, tool/gate_enrich.go, tool/gate_enrich_test.go, references/model-roles.md

Delete the classifier, its detached worker command and its role (`question_classifier` in tool/runners.go:40 and the special cases at tool/paths.go:397-404 and tool/runners.go:72). Gate question cards keep their deterministic route. A config that still names the role loads with a warning, not an error. Existing `question-classifier.jsonl` files are left on disk.

#### Builds
- The classifier, its worker and its role are gone; gates queue no classifier job.
  id: classifier-gone

#### Acceptance
`go test -p 2 -count=1 -run 'Gate|Enrich|Config|Runner' ./...` in tool/ passes; `rg -n 'classifier' tool/*.go` finds no live reference outside the config warning; a config naming `question_classifier` loads with a warning.

### wake-shadow: Judge watcher alerts in shadow
kind: ship
model: sonnet
depends: decision-core
owns: tool/stall.go, tool/stall_test.go, tool/wake_decision.go, tool/wake_decision_test.go, tool/lead.go, tool/orchestrator.go

When the watcher emits `stalled`, `looping` or `quiet` (tool/stall.go:211-236, tool/orchestrator.go watchLead), it also starts, without waiting, `askDecision` with use `wake` and state: alert kind, minutes since progress, the lead's last 40 screen lines, its last 5 status lines, whether a tool call is in flight. Questions: `recovers` (noul): will the lead make progress within stall_minutes without help? `needs_orchestrator` (noul): does this need the orchestrator to act? The alert prints unchanged. The record also stores the seconds since the watcher's previous printed batch, so the report can show how often a filtered wake would have kept the orchestrator's prompt cache warm.

Outcome, recorded stall_minutes after the alert: `progressed_alone` (the worktree fingerprint changed or the lead reported, with no orchestrator action on that lead), and `orchestrator_acted` (one of `orch send`, `close`, `respawn`, `compact`, `focus`, `unfreeze` ran for that lead in the window; `peek` alone counts as looked, not acted). Those commands record an action event for the lead so the outcome can be computed.

#### Builds
- Stall, looping and quiet alerts get a shadow decision without delaying the alert.
  id: wake-decide
- Each alert's outcome and the gap since the previous wake are recorded.
  id: wake-outcome

#### Acceptance
`go test -p 2 -count=1 -run 'Stall|Watch|Wake|Send|Close' ./...` in tool/ passes. With a fake decider that hangs, the alert still prints at once; with a working one, a record and, after the window, one outcome line exist; an `orch send` to the lead inside the window gives `orchestrator_acted: true`; a worktree change with no action gives `progressed_alone: true`; a disabled `wake` role records nothing and changes nothing.

## Open questions
None.
