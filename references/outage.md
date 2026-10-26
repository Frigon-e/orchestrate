# Provider outage behavior and deterministic coverage

## Outcome rules

A transport failure is not a successful review. Connection refusal/reset, HTTP
401/402/403/429/5xx and recognized transport timeout diagnostics skip the failed
lane for the current role run. Each invocation gets at most one transport
fallback to the next configured usable choice. These errors never write health.
The visible note includes the failed runner and the exact manual hint:
`orch health set <lane> limited --for 3h`.

Two cases are deliberately different from a provider outage:

- A provider that replies with malformed or empty review content has not proved
  a transport outage. The existing schema-repair slot can use the next usable
  route. If recovery fails, required checks remain `incomplete` with
  `missing_answer`, and diagnostics name the runner and invalid or empty reply.
- A stalled process has no activity during the stall window. Its existing
  retry slot uses the next usable route, inside the original pass deadline.
  If both attempts stall, the check is `incomplete` / `dependency_unavailable`.
  A process that reaches the full wall cap cannot start another route after
  that deadline. It is `incomplete` / `budget_exhausted`, with a timeout message.

Schema repair, coverage repair and stall recovery share one slot. Transport
fallbacks, repairs and route changes use the existing shared `RunBudget`.
No fresh conversation or provider switch resets that budget. No deadline is
split, extended or restarted. A run may remain incomplete even if a third
configured route exists after its two permitted attempts fail. An outage does
not authorize more attempts or a weaker model.

## Component matrix

The default conversational review order is OpenRouter, then Claude. OpenAI is
an additional configured choice in the injection matrix, not a routing change.
API roles use their configured decisions connector, normally OpenRouter.

| Component | Its provider is unavailable | Other providers are unavailable | Recovery |
|---|---|---|---|
| Group reviews and both adversaries | One transport fallback; bounded next-route schema/stall recovery; incomplete if no answer | Healthy configured routes remain eligible | A new gate run tries its preferred route again |
| Triage `spawn`, `--auto`, `quick` | Durable sanitized skip, no invented tier; shadow proceeds, enforce refuses; auto starts no lead and gives manual-profile/`--tier` guidance | Claude/OpenAI health limits do not disable the OpenRouter API connector | A new request can rate the task; a skipped result is not a cached tier |
| Question classifier | Bounded unavailable record; published cards, gate JSON and report bytes stay identical; no gate-budget use | Only its own configured connector health matters | Existing terminal manifests do not retry automatically; a new decision input can queue a new job |
| Enabled adjudicator | Check stays incomplete; attempts consume local and shared budget; no endless retry | Existing configured authorized routes retain their eligibility | A later explicitly budgeted attempt can resume unresolved work |
| Spec queries | Explicit `--generate` reports a skip and exits unsuccessfully; no invented query text; dry run makes no call | Existing role filtering is unchanged | The next generation uses the recovered configured role |
| Offline sweep | Fewer/missing/unreadable pass files stay `recheck`; no settlement below the independent-pass requirement | Does not dispatch providers or read health | Re-run after the required pass files exist; human-only cards stay human-only |
| Lead `new`, `spawn`, `plan` | Refuse a manually limited lane; degraded choices still require consent | Independent healthy lanes remain usable | Clear health and preflight again; a running lead can explicitly respawn on another allowed harness |

Sweep is an offline comparator. Outages affect it only through the pass files
produced elsewhere. It has no provider execution or provider-failure manifest
contract. It reports `N of M passes answered` when passes are missing, rather
than inventing provider availability data.

## Provider combinations

The gate test places unavailable lanes first so a healthy preferred lane cannot
hide an untested failed provider. Each row runs refusal, 429, 503, transport
timeout, 401, malformed and empty reply injections.

| Mask | Unavailable lanes |
|---|---|
| 1 | OpenRouter |
| 2 | Claude/Anthropic service through the Claude harness |
| 4 | OpenAI |
| 3 | OpenRouter + Claude |
| 5 | OpenRouter + OpenAI |
| 6 | Claude + OpenAI |
| 7 | All three |

Claude subscription health (`claude`), direct Anthropic through pi
(`pi:anthropic`), and Anthropic models through OpenRouter
(`pi:openrouter:anthropic`) are separate lanes. The hierarchy tests cover all
three, the `pi-anthropic` alias, provider-child limits, the `pi` parent and
explicit `ok` recovery. An OpenAI limit does not disable Claude or OpenRouter.

## Verification boundary

`tool/provider_outage_test.go` exercises the real command handlers, scheduler,
classifier worker, typed API adapters and lead preflight.
`tool/provider_outage_transport_test.go` supplies a reusable `RoundTripper` that
intercepts every request, including readiness probes. No injected HTTP request
opens a socket. Conversational models are executable shell fakes, not installed
Pi or Claude sessions. Fixtures include a non-Go repository with Python and
TypeScript sources. Real process stalls and wall caps use short fake deadlines.

Direct Anthropic through pi also has seven-mode next-route regressions. The
production classifier worker resolves its real role before the fake API call.
Existing classifier tests additionally verify nonblocking card queues,
late-result discard and hard wall limits. Existing E1 and adjudicator tests
verify budget reservation/accounting across repairs and fallback. Dispatch tests
exercise the respawn decision surface, not a live lead respawn. Tests use only
temporary config, health and run stores; no live keys, bench store or model calls.
These tests prove local orchestration behavior, not vendor-side retry behavior,
SDK protocol compatibility, provider uptime or exact opaque provider usage.

Run from `tool`:

```sh
go build ./... && go test -p 2 -run 'Outage|Provider|Health|Fallback|Runner|Triage|Classif|Adjudicat' .
```
