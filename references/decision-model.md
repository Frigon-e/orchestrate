# Decision model

The decision model is the Decider route: about $0.0004 and 1-3 s per call. Every
use goes through one helper, `askDecision` (`tool/decision_log.go`). It makes
the call, validates the raw answers, and records the question, the answers, the
cost and the latency. Later, the real outcome is joined to the record.
`orch decisions report [--use U] [--since 7d]` shows, per use, what it cost and
how well its answers predicted what happened.

## Rules for every use

1. **Declare the outcome first.** Before a use ships, it names what really
   happened (a boolean field in the outcome) and the code that records it. No
   outcome, no use.
2. **Keep raw numbers.** Store raw probabilities and scores in the record.
   Derive decisions from them with named constants. Never discard the raw answer.
3. **Fail open.** A timeout, HTTP error, missing key or invalid answer leaves
   today's behaviour and is recorded with its reason. A disabled role makes no
   call and writes no record; the report shows the role as disabled.
4. **Shadow first, time-boxed.** A new use only records until the report shows
   at least 100 outcomes for it. Then the user decides to promote or cut it.
   Promotion is a later spec that adds the setting. Nothing promotes itself.
5. **One helper, one format.** No per-use queues, workers or record files.
6. **Never skip a cold wake.** A filter that skips orchestrator wakes must not
   skip one when the last wake was 50 minutes or more ago. The prompt cache
   lives 1 hour.
7. **Validate raw JSON.** A missing key or field is invalid; it never reads as 0.

## Files

Under the run root (`site.root`):

- `decisions/<use>.jsonl`: one decision record per line.
- `decisions/<use>.outcomes.jsonl`: one outcome per line, keyed by decision ID.
  A record is never rewritten. A second outcome for the same ID is ignored.

`decisions/` is created before the first locked append (`withFlock`).

### Decision record

| Field | Meaning |
|---|---|
| `id`, `time` | unique ID and UTC time |
| `use`, `role`, `subject` | the use, its model role, the task or lead ID |
| `state_digest` | short hash of the state sent (each text field capped at 2000 characters) |
| `meta` | the use's own fields; stored, never sent |
| `questions`, `answers` | what was asked; the raw JSON answer per key |
| `model`, `cost`, `tokens`, `latency_ms` | what served it and what it cost |
| `reason`, `error` | failure class and message; empty on success |
| `action` | `shadow`, or what the use did (for triage, the tier) |

Failure reasons: `timeout`, `http_<code>`, `no_key`, `no_endpoint`,
`no_connector`, `bad_reply`, `invalid_answer`, `error`.

### Outcome record

`{"id": ..., "time": ..., "outcome": {...}}`. The outcome fields are the use's
own; boolean fields can be scored.

## Adding a use

1. Pick a use name and a role. A new role needs a `modelRoles` entry; the
   `decider` role serves existing uses.
2. Write the questions and say which keys and fields must be present
   (`decisionRequest.Keys` and `.Fields`).
3. Call `askDecision(ctx, s, decisionRequest{...})` with a per-use deadline in
   `ctx`. Handle `errDecisionDisabled` and every other error by keeping today's
   behaviour. Leave `Action` nil for shadow mode.
4. Record the outcome with `recordDecisionOutcome(root, use, id, outcome)`
   when it is known. If it needs time to pass, register
   `registerOutcomeResolver(use, fn)` that reads durable data (status.log, the
   worktree) and records it. The watcher and the report both run resolvers, so
   `fn` must be safe to run repeatedly.
5. Declare each score to judge with `registerDecisionMetric(use,
   decisionMetric{Name, Key, Field, Outcome})`: `Field` is `noul` or `score`
   of answer `Key`, and `Outcome` is a boolean outcome field. The report then
   shows AUC and a calibration table in 10 probability bands over 0-1, and says
   "fewer than 100 outcomes" until the rule 4 threshold is met.
6. Adding a use is a spec decision: escalate it, do not slip it into another change.
