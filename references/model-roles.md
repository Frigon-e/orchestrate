# Model roles and connector health

Machine settings go in `~/.config/orchestrate/config.toml` (or `ORCH_CONFIG`),
never repository config. Each role tries its ordered `use` list. An omitted
`enabled` means true for an explicitly configured role. An explicitly
configured empty list has no choices; `enabled = false` also wins over command
overrides. The optional `gate_adjudicator` role is disabled when absent, with
no inherited model choices, even when a command supplies a runner override.

```toml
[roles.gate]
# Gate passes get no lookup CLI or shell; the gate gathers its own facts.
use = [
  { harness = "pi", model = "openrouter/~deepseek/deepseek-flash-latest" },
  { harness = "claude", model = "haiku", effort = "low" },
]

[roles.spec_queries]
use = [{ harness = "claude", model = "haiku", effort = "low" }]

[roles.decider]
enabled = true
use = [{ harness = "api", provider = "openrouter", model = "<decisions-model-id>" }]
```

The Decider is an HTTP API role, not a pi profile. It checks that connector's
credential, provider health, and available credit information. It does not
use pi's model allowlist or need the pi binary when an environment credential
exists. `triage_model`, if set, overrides the role's model for legacy configs.

OpenRouter credentials come from `ORCH_TRIAGE_KEY`, `OPENROUTER_API_KEY`, or
`pi auth print-bearer-token --provider openrouter`. Other API connectors use
`<PROVIDER>_API_KEY` or pi auth. An alternative decisions-compatible connector
uses `ORCH_<PROVIDER>_DECISIONS_URL`; once its endpoint and credential exist,
only the role entry's `provider` and `model` change. OpenRouter keeps the
legacy `ORCH_TRIAGE_URL` endpoint override. A connector must implement the
same typed questions/answers contract; this is not a chat-completions adapter.

## Defaults and overrides

On a Claude-only machine, gate and spec-query roles use `claude haiku low`.
The Decider is off unless an API key is independently
available. With both harnesses installed, gate/spec-query roles offer pi
DeepSeek Flash followed by Claude Haiku.
Choices are filtered by their connector health and credential readiness.
A missing role gets its defaults even when other roles are explicitly set.

`orch gate spec.md --runner pi:openrouter/~deepseek/deepseek-flash-latest,claude:haiku:low`
sets the run's ordered list. `ORCH_PI` and `ORCH_CLAUDE` override the pi and
Claude binaries.
A profile marked `degraded = true` requires user approval and `--degraded`.
`site.runModel(role, prompt, ...modelOptions)` is the tool-less conversational
entry point for generation commands. API Decider calls use the API wrapper.

## When a provider is down

Provider failures are local to the run. They do not write health state. Mark a
persistent limit explicitly with `orch health set <lane> limited --for 3h` and
clear it with `orch health set <lane> ok` after recovery. The lanes are
`claude`, `pi:openrouter`, `pi:anthropic`, and `pi:openai`. A `pi` limit covers
all pi children; an OpenAI limit does not disable Claude or OpenRouter.

| Component | Provider unavailable |
|---|---|
| Gate reviewers and adversaries | Skip a failed lane for this run and try one configured fallback. With no completed review, the v2 result is incomplete, never pass. Deterministic checks and items remain. |
| Triage, auto dispatch and quick | Record a sanitized skipped triage result, never invent a tier. Shadow proceeds; enforce refuses. Auto gives manual-profile/`--tier` guidance and starts no lead. |
| Question classifier | Keep deterministic cards and gate outputs unchanged. Record unavailability; bounded retries and late-result rejection remain in force. |
| Optional gate adjudicator | Leave the check incomplete. Charge attempts to its local and shared gate budget; no indefinite retries. |
| Spec queries | Report the unavailable optional work. No independent authority to pass a gate. |
| Offline sweep | Missing, unreadable or insufficient independent passes cannot settle a card. Human-only questions stay human-only. |
| Lead dispatch | Refuse limited lanes. Use an allowed healthy profile; a degraded profile requires consent and `--degraded`. An already-running lead can be explicitly respawned on another harness. |

See [outage.md](outage.md) for the deterministic injection matrix and precise
limits of what these tests establish.

## Background question classification

`question_classifier` is a separate typed decisions API role. When absent, it
inherits a copy of the Decider's ordered connector list, including the legacy
`triage_model` override. It is enabled by default when a usable connector exists.
It does not inherit triage's enabled switch or `triage_mode`. An explicit role
replaces that default. Changing or disabling this role never changes triage or
`gate_adjudicator`. Health, credentials and degraded-choice filtering still
apply. Only `api` profiles are accepted; this is not a conversational agent.

```toml
[roles.question_classifier]
enabled = true
use = [{ harness = "api", provider = "openrouter", model = "<decisions-model-id>" }]
promote = false # Keep OFF until a measured trial approves promotion.

[roles.question_classifier.classifier_limits]
batch_requests = 2
run_requests = 8
seconds = 60
retries = 1
```

These are the defaults. Each package's related cards share batches of at most
four cards. Each batch makes one request, with at most one retry. All pairs
within that bounded batch receive a relation question. Requests have a 96000
UTF-8 byte input cap. One worker serializes the run's calls through a process
lock. The run request count and wall deadline survive worker restarts; admission
is persisted and directory-synced before a call. Ledgers are scoped to the gate
invocation, not the lifetime of a reused spec-hash directory. Accounting run IDs
are excluded from both the model state and the decision cache key. Limits are independent of the gate's `RunBudget`.
Classifier failures and exhaustion never consume or reset the gate budget.
The configured hard ranges are 1..2 requests per batch, 1..32 per run, 1..300
seconds, and 0..1 retries. Exhaustion is an explicit unresolved outcome.

Card production queues immutable jobs, starts a detached local worker, and
returns without connector probes or model calls. No usable connector, a disabled
role, or an unbound legacy subject records an unavailable note, without changing
cards, packets, routes, readiness, gate status, report or command output.
Connector resolution happens in the worker. Pending jobs do not cause polling
or repeated requests. A completed or exhausted manifest is never resubmitted by
re-enrichment. `orch question-classifier-worker <gate-dir>` explicitly drains
pending manifests after an interrupted worker. This internal command never
retries terminal jobs. Queued jobs remain visible if a process is killed.

The append-only `question-classifications.jsonl` beside `gate.json` records:
`input_hash` (all decision inputs, including card text/options/revisions, evidence
IDs/hashes, subject, authority, rubric, model choices and grouping), `job_key`
(the immutable submitted manifest), `card_hashes`, `subject_id`, `evidence`,
`evidence_scope`, configured/served model IDs, `rubric_version`, `grouping`,
`status`, `note`, `run_id`, attempt, latency, limits/spent requests, usage and the empty
`subsequent_outcome` field for a later human/lead outcome. JSONL readers tolerate
missing additive fields and incomplete lines. Positive aggregate token usage
is recorded separately; it is never guessed into input/output splits. The
legacy connector cannot distinguish omitted usage from zero, so zero stays
unknown rather than becoming a fabricated measurement.

`cards[].question_kind` and `cards[].card_readiness` are separate typed guesses;
`relations[].question_relation` describes a card pair. Each guess records its
probability distribution and a selected/unresolved outcome. Labels below 0.8
remain unresolved. No probability creates explanation text: the reviewer's
rationale stays in the original packet. Classifier evidence scope is limited
to the supplied review evidence and card hints, never an exhaustive search.
Before accepting a result, the worker rechecks card bytes, the current code/spec
subject, and the gate's evidence records. Late results become `stale` records,
not updates over newer cards.

The worker is RECORD-ONLY. It never rewrites cards or grants authority. The
`promote` flag defaults off. Its policy projection records `effective_route`
beside `deterministic_route`: only an `unclear` implementation choice may project
to `lead-may-decide`, only with an exact supporting Authority `May decide`
phrase and ready deterministic checks. Owner routes, owner-sensitive words,
missing readiness, and any escalation/prohibition text prevent promotion.
Even an enabled projection cannot resolve questions, merge cards, waive checks
or change triage. The production route stays deterministic pending trial.

## Optional gate adjudication infrastructure

`gate_adjudicator` is separate from the main `gate` reviewer and the triage
`decider`. It may use the same model without sharing a conversation. `triage_model`
does not change this role. Existing runner override,
health, allowlist and approved degraded-profile rules still apply through
`newRoleRun`. Gate reviewer command overrides do not reroute adjudication.

The role stays off until explicitly configured. This example is deliberately
disabled; enabling it is a separate calibration decision:

```toml
[roles.gate_adjudicator]
enabled = false
use = [{ harness = "claude", model = "haiku", effort = "low" }]

[roles.gate_adjudicator.budget]
requests = 24
input_tokens = 256000
output_tokens = 32768
seconds = 3600
repairs = 1
fallbacks = 1
output_reserve = 4096
# Optional dollar cap requires a reliable configured per-request estimate:
# max_usd = 1.0
# estimated_request_usd = 0.05
# reliable_estimate = true
```

Without a role budget table, adjudication has a local cap of 2 requests,
128000 input tokens, 8192 output tokens, 180 seconds, 1 repair, 1 fallback and
a 4096 output allowance per invocation. A supplied role table replaces these
local limits; required positive fields cannot be omitted. This local cap lives
inside the live whole-gate `RunBudget`, not a fresh run entitlement.

The user configuration, `~/.config/orchestrate/config.toml`, also accepts an
optional `[gate_budget]` table. Repository files cannot set it. Each omitted
key keeps its default; an explicit zero disables repairs or fallbacks:

```toml
[gate_budget]
# Example values; the defaults derive from the pass count (below).
requests = 2000
input_tokens = 300000000
output_tokens = 64000000
seconds = 14400
repairs = 12
fallbacks = 24
output_reserve = 4096
# Optional, only with a reliable configured estimate:
# max_usd = 10.0
# estimated_request_usd = 0.01
# reliable_estimate = true
```

Defaults preserve the existing normal path: for each main pass, four route
invocations times its existing hard turn allowance (`ceil(1.4 * soft_turns)`)
plus two answer turns, and two for optional adjudication. The total input allowance is that request count
times 600000 (four UTF-8 bytes per 150000-token context allowance); output is
that count times 32768. The total wall cap is 45 minutes times the number of
main passes plus 180 seconds. Repairs default to twice the number of main passes plus one, fallbacks to
three times that number plus one, and the per-invocation
output reserve to 4096. These are generous safety limits, not a guarantee that
an opaque harness uses exactly the configured turn count. Existing per-job
45-minute deadlines, stall rules, soft/hard turns, prefixes and warm-up order
remain unchanged.

First attempts, schema repairs, coverage/stall retries and provider fallbacks
admit against this same object. Pane stop/answer prompts also require admission.
Concurrent invocations reserve input/output allowance atomically. Admission
uses the complete prepared prompt, including the shared prefix and the
checklist, not only the changing suffix. Reported usage includes cached input,
tool-loop requests and output before a reply can be accepted. Retry sessions
retain earlier usage; a fresh conversation or compaction cannot reset spend.
An opaque invocation without request telemetry costs at least one request and
leaves the request counter explicitly incomplete. Missing token/cost reports
stay unknown, never measured zero. Unknown input/output consumes its admitted
allowance as an estimate; estimated allowance and reported tokens are separate.
Token enforcement remains labeled `reported_usage_and_reserved_allowance`:
unreported provider usage cannot be claimed as an exact hard measurement.
Dollar enforcement requires the reliable configured per-request estimate;
estimated and reported cost remain separate. Request, token and wall limits do
not need a dollar estimate. `gate.json` and v2 status expose the same spent,
remaining and unknown budget state.

An unresolved batch gets one initial invocation and at most one schema repair,
from that same run budget. Provider errors and valid uncertainty are not
resubmitted until another model agrees. A later explicitly requested batch or
route switch retains the counters and charges a fallback when its route
changes. Exhaustion records an incomplete answer with `budget_exhausted` and
an owner/reviewer next action. Only the unfinished required checks become
incomplete; completed groups are retained and are not restarted. Raise the
user-level budget, resume the named review, or ask its recorded owner.

Only unanswered technical questions with recorded delegation to
`gate_adjudicator`, an allowed assessor, and targeted evidence retrieval are
eligible. No source text or model reply can grant that authority. The current
legacy consequential-question adapter grants no delegation, so simply enabling
the role does not authorize user-owned decisions. Future typed producers must
record the check owner, delegates, assessor and question basis. Fully resolved,
disabled and user-owned paths make no packet-formatting, adjudication or shadow
call. The existing reviewer or authorized owner remains responsible otherwise.

Packets carry the unresolved check only, exact source excerpts with their
recorded hashes, lookup coverage/omissions, earlier claims and allowed actions.
Related unresolved items in one package share a batch; completed items are
excluded, and separate package batches consume the same run budget.
Paths without excerpts, stale hashes and unsupported exact column selections
fail closed. Chat runners receive TOON through the shared output boundary,
with stable instructions before changing evidence and repairs. Decisions API
runners retain native object state/questions and use the existing decisions
endpoint, never chat completions. The current finite-choice endpoint can select
a complete recorded behavioral proposal, retaining its explanation, evidence
and matching/missing behavior. It can also decline or defer. If no complete
proposal exists, that connector cannot supply the required support fields and
records incomplete without a call; it never invents an explanation or embeds
JSON chat inside a typed choice. An eligible later configured conversational
route may be selected before any model call.

Both adapters normalize to `reviewAnswer`, `reviewQuestion` and
`reviewResolution`. Subject and attempt identity come from the trusted request.
Earlier judgments remain recorded; a later answer never wins by arrival order.
Material conflicts stay incomplete and an accepted blocker requires an explicit
evidence-linked resolution, including a finding-to-resolution link. `adjudication.json` beside the gate run records packets,
raw replies, execution/usage, normalized results and the shared budget.
Native decisions aggregate usage and served model are retained separately when
reported; aggregate tokens are not guessed into an input/output split. The
explicit `recordAdjudicationShadow` experiment hook owns a separate budget and
records later outcomes without applying any reply to production. Ordinary
runs do not call it.

## Connector lanes

- `claude`: Claude subscription.
- `pi:<provider>`: first segment of a pi model id, for example `pi:openai`,
  `pi:anthropic`, or `pi:openrouter`.
- `pi:openrouter:anthropic`: OpenRouter model ids containing `anthropic/`.
- `pi`: legacy parent covering every pi provider and the API Decider's lane.
- `pi-anthropic`: legacy alias of direct `pi:anthropic`, not OpenRouter Anthropic.

A lane is limited if its own entry or a parent entry is limited. An OpenRouter
limit does not disable direct OpenAI/Anthropic connectors or Claude. A `pi`
parent limit does disable all pi connectors and the OpenRouter API Decider.
Unqualified legacy pi models retain lane `pi` because they do not identify a
provider. `orch health` lists granular lanes, including saved provider lanes.

```sh
orch health set pi:openrouter limited --for 3h
orch health set pi:openrouter:anthropic limited --for 3h
orch health set pi:anthropic ok
```

`allowed_models` remains keyed by conversational harness `claude`/`pi`.
`allowed_when_limited` keeps those harness keys and adds optional `lane` for
a connector-specific condition. Without `lane`, the condition uses the legacy
harness parent.

```toml
[allowed_models]
pi = ["openrouter/~deepseek/deepseek-flash-latest"]
claude = ["sonnet"]

[[allowed_when_limited]]
harness = "pi"
lane = "pi:openrouter"
allow = { claude = ["haiku"] }
```

## Doctor and keys

Doctor prints per-role choices, binaries, and per-provider credential state.
OpenRouter uses a free read-only `GET /api/v1/key`. A returned, nonempty
Anthropic-only `allowed_models` or `allowed_providers` restriction scopes an
empty key to `pi:openrouter:anthropic`. If the endpoint provides no such
proof, doctor says `unknown scope` and attributes empty credit to
`pi:openrouter`. Labels and configured model names are not scope evidence.

Null `limit_remaining` means no reported cap, not zero. A failed HTTP probe
is reported as a diagnostic, not evidence of exhausted credit. OpenAI and
Anthropic only check credential presence through pi's read-only
`auth check --json --no-refresh` or provider key variables. No secrets are
printed. No readiness or fallback path writes `health.json` automatically.

## Failure and triage behavior

With no usable gate runner, deterministic checks, items and the gate
record are still written; a normal gate run has state `incomplete` when its
subject and required system records are valid. `pass_runners`, the
report, and compare output show actual pass runners.

When the Decider is off, `orch spawn <id> --auto` prints one guidance message,
starts no lead, and invents no tier. Supply `--auto --tier settled|standard|planning`
to select a dispatch rule without triage. The first allowed, non-limited,
approved profile wins. `--apply` still controls launch. A hand-chosen
`orch spawn <id> --harness H --model M [--effort E]` also bypasses triage;
effort defaults to the task's effort, or high for an unresolved auto task.

Quick shadow mode records `skipped: decider off`, leaves the tier empty, and
proceeds. Quick enforce mode fails closed and gives the manual-profile/tier
message. Triage shadow also records unavailable skips rather than fake ratings.

## Claude execution boundary

Claude passes run `claude -p` with Read/Grep/Glob and Bash limited to
`jbcontext search`, `orch lsp`, `rg` and `fd`, non-interactive `dontAsk`
permissions and disabled slash commands. The strict empty server configuration
excludes ambient servers, and the gate runs no ctx server. Tool-less tasks stay
tool-less even with ctx enabled. The ctx tool, shell modes, repository-context
exclusion and the unchanged-checkout check are in `gate.md` (Context tools for
passes); the guard rules are in `context.md`.

Keep gate state artifacts outside the code checkout or ignored: the
unchanged-checkout scan does not exempt non-ignored orch artifacts.
