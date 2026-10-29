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
answers predicted what actually happened. A new use starts in shadow mode
(record only); failures always fall back to today's behaviour.

This spec moves triage onto the helper and fixes its tier, removes the question
classifier, and adds the first new use in shadow mode: judging watcher stall,
looping and quiet alerts. Nothing in this spec suppresses an alert.

Why: a 2026-10-28 evaluation found triage's raw difficulty score predicts hard
tasks well (AUC 0.77 over 384 tasks) while its tier does not (0.54), because a
low-confidence bump moves 45% of tasks up a tier and an open-ended rule fires on
a signal that predicts in reverse (AUC 0.43). The question classifier could not
be evaluated at all: nothing records who really answered its questions, it
abstains on 88% of cards, and 47% of its jobs hit its request cap. Triage drops
the cost the API returns. Each use built its own queue and record format.

The orchestrator's prompt cache lives 1 hour; a 1-hour cache write costs about
2x input and a warm read about 0.1x, so one cold wake costs about as much as 20
warm ones. The wake use therefore records how long the orchestrator had been
idle at each alert, so a later filter can be judged on cache cost as well as
turns.

## Design rules
- Rules for every use, written in `references/decision-model.md`:
  1. Every use declares its outcome (what really happened) and the code that records it, before it ships. No outcome, no use.
  2. Keep raw probabilities and scores in the record; derive decisions from them with named constants; never discard the raw answer.
  3. Fail open: a timeout, HTTP error, missing key or invalid answer leaves today's behaviour and is recorded with its reason. A disabled role makes no call and writes no record; the report shows the role as disabled.
  4. Shadow first, time-boxed: a new use acts only in shadow until `orch decisions report` shows at least 100 outcomes for it; then the user decides to promote or cut it. Promotion is a later spec that adds the setting; nothing promotes itself.
  5. All uses share the helper and the record format; no per-use queues, workers or record files.
  6. A filter that skips orchestrator wakes must never skip one when the orchestrator's last wake was 50 minutes or more ago (cache lifetime 1 hour).
- Answers are validated on each answer's raw JSON: a key or field that is missing never reads as zero.
- Records live in the run root (`site.root`) under `decisions/<use>.jsonl`; outcomes are separate lines in `decisions/<use>.outcomes.jsonl` keyed by decision ID, so a record is never rewritten. The helper creates `decisions/` before it locks a file there.
- A decision record holds: ID, time, use, subject (task id or lead id), a digest of the state sent, the use's `meta` fields (stored, never sent), the questions, the raw answers, the model served, cost, tokens, latency, error, and the action taken (`shadow`, or for triage the tier it chose). State text sent is capped per field at 2000 characters.
- Each call has a per-use deadline passed as a context: triage keeps its 90 s, wake uses 10 s. The watcher never waits on the decider.
- Outcomes that need time to pass are resolved lazily from durable data (status.log, the worktree) by a resolver the use registers with the helper; the watcher and `orch decisions report` both run the resolvers, so an outcome is written even if the watcher exits first.
- Orchestrator actions on a lead are read from status.log notes authored by `orch` (close, respawn and unfreeze already write one; send, compact and focus gain one). No separate event store.
- Triage stays on the existing `decider` role. The wake use gets its own `wake` role, api harness only, defaulting to the Decider route, so it can be disabled alone.
- `orch auto` and `orch quick` keep calling `triageTask` unchanged; quick keeps its own quick/not-quick verdict from the answers.

## Authority
May decide: internal names, record field names beyond those listed, report layout, test layout
Must escalate: any threshold that changes routing or suppresses an alert, adding a new use, sending more task or code text to the provider than listed here
Never: push, merge, edit files another package owns, promote a use out of shadow

## Packages

### roles: Remove the classifier role, add the wake role
kind: ship
model: sonnet
owns: tool/question_classifier.go, tool/question_classifier_test.go, tool/provider_outage_test.go, tool/runners.go, tool/paths.go, tool/guard.go, tool/gate_enrich.go, tool/gate_enrich_test.go, references/model-roles.md, references/outage.md

Delete the classifier, its detached worker command and its role (`question_classifier` in tool/runners.go:40, its defaults at tool/runners.go:72 and the special cases at tool/paths.go:397-411). Gate question cards keep their deterministic route. Remove the classifier subtests and helper calls from tool/provider_outage_test.go and keep its other outage tests. A config that still names `question_classifier` loads: drop it from `cfg.Roles` before validation the way `shadow_flash` is dropped (tool/paths.go:389), set a flag, and surface a `warn` finding the way the ignored `bench_suite` key does (tool/guard.go:201). Existing `question-classifier.jsonl` files are left on disk. Add the `wake` role to `modelRoles`, allow the `api` harness for it, and default it to the Decider route like `decider`. Update references/model-roles.md and references/outage.md.

#### Builds
- The classifier, its worker and its role are gone; gates queue no classifier job.
  id: classifier-gone
- A `wake` role routes to the Decider by default and can be disabled in config.
  id: wake-role

#### Acceptance
`go test -p 2 -count=1 -run 'Gate|Enrich|Config|Runner|Outage|Role' ./...` in tool/ passes; `rg -n -i 'classifier' tool` finds only the removed-key entry and its warning; a config naming `question_classifier` loads with a `warn` finding; an unconfigured site resolves `wake` to the Decider api route; `wake` with `enabled: false` reports disabled.

### decision-core: Helper, records and orch decisions report
kind: ship
model: sonnet
owns: tool/decision_log.go, tool/decision_log_test.go, tool/decisions.go, tool/decisions_test.go, tool/main.go, references/decision-model.md

`tool/decisions.go`: add `postDecisionsRawWithContext`, returning each answer as `json.RawMessage` and the usage; `postDecisionsWithContext` becomes a wrapper with unchanged behaviour. `tool/decision_log.go`: `askDecision(ctx, s site, req decisionRequest) (decisionResult, error)`, where `decisionRequest` holds use, role, subject, state, questions, required keys and fields, `meta map[string]any` (stored in the record, never sent) and `action func(decisionResult) string` (computed before the record is appended; nil means `shadow`), resolves the role through `site.role`; when the role is disabled it returns `errDecisionDisabled` and writes nothing. Otherwise it calls the raw function, checks every required key and field is present, appends the record (MkdirAll `decisions/`, then a locked append with `tool/locks.go#withFlock`), and returns the ID, raw and decoded answers. `recordDecisionOutcome(root, use, id string, outcome map[string]any)` appends one outcome line and ignores a second one for the same ID. `registerOutcomeResolver(use string, fn func(s site) error)` and `resolveOutcomes(s site)`, which runs all registered resolvers and logs, not returns, their errors. `orch decisions report [--use U] [--since 7d]` (`--since` filters by decision time; outcomes join their decision whenever written) runs `resolveOutcomes` first, then shows per use: calls, failures by reason, whether the role is enabled, cost total, latency p50/p90, answer distributions, outcome coverage, and for each metric a use declares (a noul answer or a score against a boolean outcome field) the AUC and a calibration table by probability band, with "fewer than 100 outcomes" when that holds. Reuse `parseSince` (tool/cmd_lookups.go:76). Write `references/decision-model.md` with the rules, the record format and how to add a use.

#### Builds
- One helper calls the decider, validates raw answers and records cost and latency.
  id: decision-helper
  reuse: tool/decisions.go#postDecisionsWithContext
- Outcomes are recorded separately, resolved lazily, and joined in a per-use report with AUC and calibration.
  id: decision-report
- The rules for every decision-model use are written down.
  id: decision-rules

#### Acceptance
`go test -p 2 -count=1 -run 'Decision' ./...` in tool/ passes. Tests with a fake decider: a missing `noul` key is invalid, not 0; a timeout and an HTTP 500 each record the reason and return an error; a disabled role writes nothing and returns `errDecisionDisabled`; the first record creates `decisions/`; cost and latency are recorded; two concurrent appends both land; a second outcome for one ID is ignored; a registered resolver runs from the report; the report computes AUC and calibration bands on a fixed fixture and says when a use has fewer than 100 outcomes.

### triage-on-core: Triage through the helper, with a fixed tier
kind: ship
model: sonnet
depends: decision-core
owns: tool/triage.go, tool/triage_test.go

Triage calls `askDecision` with use `triage` and role `decider`, keeping its 90 s deadline and the private-task refusal; its `action` returns the tier. It keeps appending `triage/triage.jsonl` for its hash cache and the old report. `triageRecord` gains the decision ID. Tier rule (named constants): `planning` when P(difficulty=3) >= 0.3 or P(cause known) < 0.2; `settled` when expected difficulty < 1.5 and P(verifiable) >= 0.8; otherwise `standard`. Remove the low-confidence bump and the open-ended rule. Add `recordTriageOutcome(s site, id string) error`: finds the task's latest triage record, computes `triageOutcome`, and writes it through `recordDecisionOutcome` with the hard signals split into `lead_signals` (escalate, failed) and `run_signals` (stalled, looping, verify fail, respawn, frozen). Register a resolver that does this for every task closed by `orch close` (not a close for respawn) with a triage decision and no outcome yet. `orch triage report` keeps working on the old `triage/triage.jsonl` history and points to `orch decisions report --use triage` for new records. `triageTask`'s signature does not change.

#### Builds
- Triage records through the helper with cost, and its outcome once the task closes.
  id: triage-records
- The tier follows the raw difficulty score.
  id: triage-tier

#### Acceptance
`go test -p 2 -count=1 -run 'Triage|Quick|Auto' ./...` in tool/ passes, with tier tests at each boundary, a low-confidence answer that keeps its tier, an open-ended answer that no longer forces planning, a closed task whose outcome the resolver writes with both signal groups, a respawned task that gets no outcome until `orch close`, a record whose action is the tier, and a task triaged twice whose outcome joins its latest decision.

### wake-shadow: Judge watcher alerts in shadow
kind: ship
model: sonnet
depends: roles, triage-on-core
owns: tool/stall.go, tool/stall_test.go, tool/wake_decision.go, tool/wake_decision_test.go, tool/lead.go, tool/orchestrator.go

When the watcher emits `stalled`, `looping` or `quiet` (tool/stall.go:211-236, tool/orchestrator.go watchLead), it starts, without waiting, `askDecision` with use `wake`, role `wake`, a 10 s deadline, and state: alert kind, minutes since progress, the lead's last 40 screen lines (reuse the read in `cmdPeek`, tool/orchestrator.go:1338), its last 5 status lines, and whether a tool call is in flight (`toolInFlight`, tool/stall.go:203). Questions: `recovers` (noul): will the lead make progress within stall_minutes without help? `needs_orchestrator` (noul): does this need the orchestrator to act? The alert prints unchanged. Its `meta` holds `idle_seconds`, the time since the watcher last flushed any line to the orchestrator, `window_minutes` (stall_minutes for stalled and looping, quiet_minutes for quiet), and the worktree fingerprint at the alert. The watcher writes its last flush time to `last-flush-<watch-name>`, a file the flush does not delete; before any flush `idle_seconds` is null. In `--once` mode and on exit the watcher waits up to the 10 s deadline for in-flight calls so their records are written.

Outcome, for alerts whose `window_minutes` has ended: `orchestrator_acted` is true when status.log has an `orch`-authored action note for the lead inside the window (close, respawn, unfreeze, send, compact, focus; `orch send`, `compact` and `focus` gain such a note here). `progressed_alone` is true when there was no action and either the lead wrote a status line in the window or its worktree fingerprint differs from the one recorded. Register this as the `wake` resolver; the watcher runs `resolveOutcomes` each tick, and `orch decisions report` runs it too, so a closed lead or a stopped watcher still gets its outcome. `cmdClose` calls `recordTriageOutcome` for the task after `closeLead`; `cmdRespawn` does not, so a respawn never fixes an early outcome.

#### Builds
- Stall, looping and quiet alerts get a shadow decision without delaying the alert.
  id: wake-decide
- Each alert's outcome and the orchestrator's idle time are recorded.
  id: wake-outcome
- Closing a task records its triage outcome.
  id: close-outcome

#### Acceptance
`go test -p 2 -count=1 -run 'Stall|Watch|Wake|Send|Close|Compact|Focus' ./...` in tool/ passes. With a fake decider that hangs, the alert still prints at once; with a working one, a record exists with `idle_seconds`, `window_minutes` and the fingerprint in `meta`; a quiet alert uses quiet_minutes; after the window, the resolver writes one outcome line; an `orch send` to the lead inside the window gives `orchestrator_acted: true`; a lead closed inside the window still gets its outcome from `orch decisions report`; a worktree change with no action gives `progressed_alone: true`; a disabled `wake` role records nothing and changes nothing; `orch close` on a triaged task writes its triage outcome.

## Open questions
None.
