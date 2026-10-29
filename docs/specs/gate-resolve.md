---
harness: claude
model: sonnet
effort: medium
base: orch/int-decision-model
---
# Gate findings ledger and orch gate resolve

## Behaviour
Each spec keeps a findings ledger across gate runs. Every verified blocker
finding a run reports gets a short ID (L1, L2, ...) that never changes and is
never reused. The author (the user or the orchestrator) closes an item with
`orch gate resolve <spec> <L-id> fixed|accepted|rejected|duplicate --reason "..."`
(`duplicate` also needs `--of <L-id>`; `open` reopens an item), and
`orch gate resolve <spec>` with no ID lists the items. The gate report gets a
`## Findings ledger` section, and `orch plan` known risks name each risk's
L-id and ledger state.

A decision model (use and role `gate_match`, on the Decider route) is asked,
for each new finding, which existing ledger item it restates. It starts in
shadow: it only records its answer, and the gate behaves exactly as today. The
gate verdict, gate.json, the exit code and the stdout summary do not change in
this spec. The author's resolve action is the outcome that grades the matcher.

Why: as of 2026-10-28, 34 recorded gate runs produced 0 passes. Finding IDs
digest the model's prose (`tool/gate_reduce.go:591`), so no finding is ever the
same across runs, and no command can close one. The round cap in commit 32eee3f
(`tool/plan.go#completedGateRounds`, `tool/plan.go#gateKnownRisks`) lets
planning proceed, but the leads get an unordered list with no memory. The
earlier ledger design (docs/specs/gate-convergence.md) failed its gate on 8
contract blockers because it changed the pass rule and the reducer at once;
this spec changes neither.

## Design rules
- The pass rule does not change. The ledger never feeds `tool/gate_reduce.go#ReduceReview` and an L-id state never changes `rec.State`, `rec.Reduction` or the exit code at `tool/gate.go:1314-1317`. Letting accepted or rejected items stop blocking is a separate later decision for the user.
- `reviewResolution` (`tool/gate_state.go:196-226`) is neither reused nor removed. It needs evidence-backed support from an assessor of the check (`tool/gate_reduce.go:200-229`); its only producer is gate adjudication (`tool/gate_adjudicate.go:801`, `tool/gate_adjudicate.go:1217`), whose role is disabled by default (`tool/runners.go:76-79`). An author's resolve has a reason and no evidence, so feeding it in would either forge evidence or change the pass rule. Removing it is a separate decision.
- gate.json keeps its schema: no field is added to `gateRecord` (`tool/gate.go:94-142`). Ledger state lives in its own file.
- Ledger file: `<run root>/gate-ledger/<first 16 hex of sha256(spec_rel)>.json`, where `spec_rel` is `rec.SpecRel` (set from `loadGateSpec`, `tool/gate.go:984-990`). The run root is shared by every worktree of the repo (`tool/paths.go#runRoot`), so all branches that gate the same spec path share one ledger. That is accepted because the ledger changes no verdict. `pruneGate` (`tool/gate_pane.go#pruneGate`) only touches `gate/<hash>/`, so the ledger is never pruned.
- Locking: every read-modify-write of a ledger holds `withFlock` (`tool/locks.go:84-96`) on `<ledger file>.lock`, never on the data file, because the write is `writeAtomic` (`tool/state.go:209-215`), which renames over it. A gate run takes its candidate snapshot without the lock, calls the matcher, then reloads and merges under the lock, so a resolve made while the matcher runs is never lost.
- What enters the ledger: from a non-experiment run whose state is `fail`, `incomplete` or `pass`, every entry of `rec.Findings` with `Severity == "blocker"`, `Verified` true and a non-empty `ID`. The ID is set by `tool/gate_reduce.go#prepareGateReview` (`tool/gate_reduce.go:575`, `tool/gate_reduce.go:591`, `tool/gate_reduce.go:618`). Concerns, unverified findings, deterministic issues and questions stay out. Experiment runs (`rec.Experiment != ""`), `error` and `stale` runs, dry runs and an empty `rec.SpecRel` read and write no ledger.
- Deterministic merge (the fallback, and the only merge in this spec): a finding whose ID equals the `finding` of a sighting already in the ledger is the same item (append a sighting). Any other finding becomes a new `open` item numbered `next`. A sighting of a `fixed` item reopens it to `open` with a history entry. `accepted`, `rejected` and `duplicate` items only gain the sighting. A matcher answer never merges, never changes a state and is shown nowhere (not in the report, the list or plan), so authors label without seeing it.
- Check keys (`<item>/<check>` from the matching `rec.Review.Findings` entry, such as `@adversary-a/adversary-a` or `<pkg>@package/scope`) are stable across spec hashes; finding IDs are not. The ledger stores the check key and uses it only to order matcher candidates.
- Ledger JSON, shared by both code packages (Go names are the lead's; JSON keys are the contract): `{"schema":1,"spec_rel":"...","next":13,"items":[...]}`. Item: `id` (`L<n>`), `state` (`open`, `fixed`, `accepted`, `rejected`, `duplicate`), `of` (duplicate only), `reason`, `closed_at` (Unix seconds), `severity`, `source`, `check`, `where`, `claim`, `evidence`, `why`, `seen` (list of `{"hash","at","finding"}`; the first is the run that created it), `match` (optional `{"decision","choice","restates","candidates"}`), `history` (list of `{"state","of","reason","at"}`).
- Matcher seam: the ledger code defines two package-level function variables that are nil unless the gate-match package sets them in an `init` function in its own file. `gateLedgerMatch(ctx, s site, rel string, candidates []ledgerItem, fresh []ledgerItem) map[string]ledgerMatch` returns answers keyed by fresh finding ID; nil, or a missing key, means no answer. `gateLedgerOutcome(root string, it ledgerItem, state, of string)` is called once, on an item's first change from `open` to a closed state. Deleting the gate-match file cuts the use with no other edit.
- Decision model rules (`references/decision-model.md:10-26`) apply: outcome declared below, raw answers kept, fail open, shadow until at least 100 outcomes (`tool/decision_log.go:31`), one helper (`tool/decision_log.go#askDecision`), raw JSON validated. Promotion (merging a matched finding into its item) is a later spec.
- Command surface: one subcommand, `orch gate resolve`, dispatched inside `cmdGate` the way `enrich` and `compare` are (`tool/gate.go:1061-1067`). No new top-level command and no separate list subcommand.
- Sequencing: build this spec first, on `orch/int-decision-model`. The unspawned structured-reports spec (docs/specs/structured-reports.md) must be revised before it is planned anyway: it names the removed question classifier role and builds its own sorter queue, which conflicts with decision-model rule 5. When it is revised, its gate-report package rewrites `writeGate` around the call this spec adds after it and leaves `## Findings ledger` (rendered by the ledger code, which it must not own) in report.md; its concern-sorter package rebases its `tool/runners.go` and `tool/paths.go` edits onto the `gate_match` entries; its report-docs package keeps the ledger text in references/gate.md and SKILL.md.

## Authority
May decide: internal names, file layout inside owned files, test layout, exact wording of report and list lines
Must escalate: any pass-rule or gate.json change, any new top-level command or subcommand, sending the provider more than a finding's where, claim and evidence and the candidates' where, claim and evidence, letting a matcher answer change ledger state
Never: push, merge, edit files another package owns, delete or rewrite gate records or decision records

## Packages

### gate-ledger: Findings ledger, orch gate resolve, report and plan
kind: ship
model: sonnet
owns: tool/gate_ledger.go, tool/gate_ledger_test.go, tool/gate.go, tool/plan.go, tool/gate_cap_test.go

New file tool/gate_ledger.go holds the ledger types, load and locked save, the merge, `cmdGateResolve`, the report section and the two seam variables, all per Design rules. Hook points in tool/gate.go, and nowhere else in that file:
- `cmdGate` dispatch (`tool/gate.go:1061-1067`): `resolve` calls `cmdGateResolve(a, args[1:])`. Add the resolve form to the usage text at `tool/gate.go:1117`.
- After `writeGate` succeeds (`tool/gate.go:1248-1250`): call `recordGateLedger(s, rec, runDir, a.now())`. It returns a note on any ledger error; append it to `view.Notes`. It never returns an error to `cmdGate` and never changes `rec`.
- Optional: share the spec-path code of `loadGateSpec` (`tool/gate.go:959-990`) with resolve.

`recordGateLedger`: skip per Design rules; take the snapshot; the fresh findings are the run's ledger findings whose ID has no sighting in the snapshot. When `gateLedgerMatch` is set and the snapshot has at least one item not in state `duplicate`, call it with those items as candidates (the matcher picks per finding). Then, under the lock, reload, merge, store each fresh item's `match` from the answer map, and save. Last, write the report section into `<runDir>/report.md`.

Report section, written by its own function with `writeAtomic`: replace an existing `## Findings ledger` section (up to the next `## ` heading or the end), else insert it right before `## Question cards` when present, else append. `orch gate enrich` rewrites only from `## Question cards` on (`tool/gate_enrich.go:876-894`), and enrichment runs after this call (`tool/gate.go:1285-1293`), so the section survives both. Lines:
- `Ledger for <rel>: open N, fixed N, accepted N, rejected N, duplicate N.`
- `This run: new L12, L13; seen again L4.`
- One line per open item seen in this run: `L12 [blocker, <source>] <where>: <claim>`.
- `Older open items not seen in this run: N (list: orch gate resolve <spec>).` when N > 0.
- `Close an item: orch gate resolve <spec> <L-id> fixed|accepted|rejected|duplicate --reason "..." [--of <L-id>]`

`cmdGateResolve`: flags `--reason`, `--of`. One positional (spec) lists; three (spec, L-id, state) resolve; anything else exits 2 with the usage. The spec path resolves to the same `spec_rel` as `loadGateSpec` without parsing the spec, so a spec with format problems can still be resolved. No ledger file: exit 1, `no ledger for <rel>; run orch gate <spec> first`.
- Resolve: state is `fixed`, `accepted`, `rejected`, `duplicate` or `open`; `--reason` must be non-blank (exit 2); `--of` is required for `duplicate` and refused otherwise (exit 2); an unknown item, or an `--of` that is unknown, the item itself, a higher number or itself a `duplicate`, exits 1. Under the lock: set state, `of`, `reason`, `closed_at` (cleared for `open`), append history, save, then call `gateLedgerOutcome` if set and this is the item's first change from `open` to a closed state (no earlier closed state in its history). Print `L4: open -> accepted`.
- List: first line `Ledger for <rel>: open N, fixed N, accepted N, rejected N, duplicate N`, then one line per item ordered open, fixed, accepted, rejected, duplicate, then by number: `L4 open [blocker, adversary-a] runs 3, last <hash>: <where>: <claim>`, cut to 240 characters, plus ` | <reason>` (and `of L2` for a duplicate) for closed items. `match` is never printed.

`orch plan` known risks: at `tool/plan.go:394-398`, load the ledger for `rel` (missing file: nil) and pass it to `tool/plan.go#gateKnownRisks`. With a nil ledger the output is byte-identical to today. With a ledger: each `finding-` bullet whose finding ID has a sighting with `hash == rec.Hash` starts with its L-id and state, for example `L4 (open): ...`, `L4 (accepted: <reason>): ...`, `L7 (duplicate of L2: <reason>): ...`. Nothing is omitted. Then one bullet `Accepted risk L2: <where>: <claim> (<reason>)` per accepted item not already listed. The header gains `Ledger: orch gate resolve <rel> lists every item.` The 25-bullet cap (`tool/plan.go:721`) covers all bullets.

#### Builds
- Keeps a locked, atomic per-spec ledger with stable L-ids and merges each run by exact finding ID only.
  id: ledger-store
  search: Code that saves gate findings for a spec path across runs, when a gate run finishes, producing stable short IDs
- Closes, reopens and lists ledger items through orch gate resolve.
  id: ledger-resolve
- Writes the Findings ledger section into report.md before the Question cards.
  id: ledger-report
- Annotates plan known risks with L-ids and ledger state and adds accepted risks.
  id: ledger-plan-risks
  reuse: tool/plan.go#gateKnownRisks
- Exposes the matcher and outcome seams and calls them as specified.
  id: ledger-seams

#### Acceptance
`go test -p 2 -count=1 -run 'Ledger|PlanGateCap|GateExperiment|GateRecordCompat|Enrich' ./...` in tool/ passes, and `go vet ./...` in tool/ is clean. New tests in tool/gate_ledger_test.go show:
- two runs with disjoint finding IDs give L1..Ln, then L(n+1) on; an ID seen again appends a sighting; a `fixed` item seen again is `open` with a history entry; a number is never reused after resolve;
- an experiment run, an `error` or `stale` run and an empty `SpecRel` create no ledger file;
- a resolve made between the snapshot and the locked merge survives the merge;
- resolve refuses a blank reason and `duplicate` without `--of` (exit 2), and an unknown, self, newer or duplicate `--of` (exit 1); list order is as specified and shows no `match`;
- the report section sits before `## Question cards`, a second write replaces it, and `addReportSection` afterwards keeps it;
- with a seam stub that returns a confident match for every fresh finding, gate.json bytes, `rec.State` and the exit code equal those of the same run with the seam nil, and the finding still gets a new L-id;
- the match seam is not called when the snapshot is empty and never gets `duplicate` items; the outcome seam is called once on the first close, not on a reopen and not on a later close.
In tool/gate_cap_test.go: the existing known-risk tests pass unchanged (nil ledger), and a new test shows L-id annotations, a rejected risk still listed, and an older accepted item added.

### gate-match: Shadow matcher on the decision helper
kind: ship
model: sonnet
depends: gate-ledger
owns: tool/gate_match.go, tool/gate_match_test.go, tool/runners.go, tool/paths.go, tool/orch_test.go

New file tool/gate_match.go sets the two seam variables from Design rules in its `init` function and registers the metric there.

Role: add `gate_match` to `modelRoles` (`tool/runners.go:40`). With no config entry, `site.role` gives it the decider's connector list, the way it does for `wake` (`tool/runners.go:70-75`), so it is enabled by default and `enabled = false` turns it off. Config validation treats it like `decider` and `wake`: api harness only, with a provider (`tool/paths.go:409-413`, including the error text).

Matcher (`gateLedgerMatch`): for each fresh finding, at most `gateMatchMaxFindings` (40) per run in finding order, one `askDecision` call (`tool/decision_log.go:175-236`) with use `gate_match`, role `gate_match`, subject `rel`, action nil (shadow). At most 4 calls in flight; each call gets its own `context.WithTimeout` of 20 s under a 90 s deadline for the whole run. These limits are package variables so tests can shorten them. Findings past the cap are not asked.
- Candidates: the given items, with the fresh finding's `check` first, then the latest sighting first, then lower number first; at most 12.
- State, exactly these keys and nothing else: `spec` (rel), `where`, `claim`, `evidence` (the finding's own strings; `evidence` is the locator the model wrote, never file contents). The helper caps each at 2000 characters (`tool/decision_log.go:104-116`). `why` is not sent.
- Questions:
  - `restates`, type `noul`, instructions `Does the new finding report the same problem as one of the ledger items listed under "which", so that one fix would settle both?`, criteria `true`: `the same problem, even if worded differently or citing other lines`, `false`: `a different problem, or a related one that needs its own fix`.
  - `which`, type `choice`, instructions `Which ledger item does the new finding restate? Pick none when it restates no item.`, criteria: one key per candidate L-id with value `<where>: <claim> (evidence: <evidence>)` cut to 600 characters, plus `none`: `the finding restates none of the listed items`.
- Validation: `Keys` `restates`, `which`; `Fields` `restates`: `noul`, `which`: `choice`, `probabilities` (`tool/decision_log.go:139-170`).
- Meta (stored, never sent): `spec_rel`, `gate_hash`, `run_at`, `finding`, `candidates`.
- Answer: on success return `{decision: result ID, choice: which.choice, restates: restates.noul, candidates}` for that finding ID. On any error, including `errDecisionDisabled`, return nothing for it: the item is created as with no matcher.

Declared outcome (`gateLedgerOutcome`), recorded with `recordDecisionOutcome` (`tool/decision_log.go:280-309`) when the item has a `match`: `duplicate` (bool: the author's first close was `duplicate`), `of` (the `--of` item, or empty), `state`, `choice`, `of_offered` (bool: `of` was a candidate), `choice_correct` (bool: for a duplicate, `choice == of`; otherwise `choice == "none"`). The helper keeps only the first outcome per decision. Register `decisionMetric{Name: "restates", Key: "restates", Field: "noul", Outcome: "duplicate"}` (`tool/decision_log.go:345-358`), so `orch decisions report --use gate_match` shows AUC and calibration and says `fewer than 100 outcomes` until there are 100.

Test isolation: in `TestMain` (`tool/orch_test.go:24-36`), set `ORCH_TRIAGE_URL`, when it is unset, to an HTTP URL on host 127.0.0.1 at the closed port 9, so no test that runs the gate twice reaches the real provider through the default decider route. Tests that need a server keep using `t.Setenv`.

#### Builds
- Adds the gate_match role with the decider's default route and api-only config.
  id: match-role
  reuse: tool/runners.go#site.role
- Asks the helper which open ledger item each new finding restates, sending only where, claim and evidence, in shadow.
  id: match-ask
  reuse: tool/decision_log.go#askDecision
  search: Code that asks the decider a question and records the answer, when a use needs a cheap model judgment, producing a decision record
- Records the author's first close as the outcome and registers the restates metric.
  id: match-outcome
  reuse: tool/decision_log.go#recordDecisionOutcome, tool/decision_log.go#registerDecisionMetric
- Keeps every test off the real decider endpoint.
  id: match-test-isolation

#### Acceptance
`go test -p 2 -count=1 -run 'Match|Ledger|Decision|Config|Runner|Triage|Wake' ./...` in tool/ passes, and `go vet ./...` in tool/ is clean. Tests in tool/gate_match_test.go with a fake `decisionCall` show: the state has exactly the keys `spec`, `where`, `claim`, `evidence`; the questions have the two keys and types above, and a long candidate is cut to 600 characters; at most 12 candidates, same check first; a call slower than the shortened per-call limit is recorded with reason `timeout` and the item is still created with no `match`; a disabled role writes no decision record; an HTTP 500 and a reply with no `which.probabilities` are recorded with reasons `http_500` and `invalid_answer`; a duplicate close (with --of) records duplicate true and `choice_correct` by the rule, a `rejected` close records duplicate false, and a second close records nothing; the report output contains the line "metric restates (restates.noul vs duplicate)"; a `gate_match` role with harness `claude` is refused by config loading, and with no entry it uses the decider's connectors.

### ledger-docs: Ledger and resolve documentation
kind: ship
model: sonnet
effort: low
depends: gate-match
owns: references/gate.md, references/model-roles.md, SKILL.md

`references/gate.md`: a `## Findings ledger` section after `## Rounds and the cap` (`references/gate.md:9-19`): what enters the ledger, L-ids, the resolve states and `--of`, the list form, the report section, the known-risk annotations, the shadow matcher, and that the pass rule is unchanged. `references/model-roles.md`: a `## Gate match role` section modelled on `## Wake role` (`references/model-roles.md:78-92`): default route, `enabled = false`, shadow only, the outcome and `orch decisions report --use gate_match`. `SKILL.md` (`SKILL.md:87-100`): one or two sentences: after a failed round, close each ledger item with `orch gate resolve` (use `duplicate --of` when an item restates an older one), then rerun or let the round cap plan with known risks.

#### Builds
- Documents the ledger loop, the resolve command and the gate_match role.
  id: docs-ledger

#### Acceptance
`rg -n 'orch gate resolve' references/gate.md SKILL.md` matches in both files, and `rg -n 'gate_match' references/model-roles.md` matches.

## Open questions
None.
