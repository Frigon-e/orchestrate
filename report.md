# Paid ctx-gate experiments

## Executive result

**Paid execution is complete: $0.353890410 USD**, including the failed pilot, failures, native tool loops and repairs. All 28 amended planned runs completed, plus the separately disclosed earlier pilot: **29 paid exposures and 230 underlying provider requests**. There will be no additional model calls. Deterministic comparisons, private verification, archival and live-store protection are complete, subject to the reporting and data-quality limitations below.

- Branch: `orch/paid-experiments`.
- Production baseline: `8a062d2c2f8d15c0916e9bd87ba6cc58378e7c1b`.
- Pre-registration/amendment history is retained in Git and [`checks/manifest.json`](checks/manifest.json).
- Actual charges and zero-dispatch setup failures are retained in [`checks/ledger.jsonl`](checks/ledger.jsonl).
- **No production code, default, allowlist, live benchmark data, adjudicator or classifier setting was changed.** No task spawning, integration or product acceptance was authorized.
- Live benchmark protection: **PASS**, exact before/after listing equality, including modes, sizes, nanosecond mtimes and all 466 regular-file hashes.

**Recommendation: retain current defaults.** E2 does not establish a universal format winner. E6 establishes real execution compatibility and exposes variability, not validated workflow accuracy or model independence. GLM's six fixture gates passed, but more passes are not automatically better judgments: these fixtures contain weak acceptance checks and no independently adjudicated whole-workflow answer key.

## Design and authority

| Experiment | Controlled design | Planned runs | Planning estimate | Actual cost |
|---|---|---:|---:|---:|
| E2 | Equivalent hybrid, TOON and compact JSON; eight-case fixture packet and two-case real CRM packet; two trials per cell | 12 | $0.24 | $0.029949469 |
| E2 earlier pilot | Original 4,096-token exposure, separately retained after truncation | 1 | Separately disclosed | $0.002733090 |
| E6 | Current gate through Flash and configured GLM; Go, Python, TypeScript and one real CRM group; two trials per cell | 16 | $0.80 | $0.321207851 |
| **Total** | No extra paid trials or fallback routes | **29 including pilot** | $1.04273309 including pilot | **$0.353890410** |

The original manifest was approved before spending. Message004 approved the disclosed 16,384-token exploratory follow-up, not an untouched held-out confirmation. Messages005-011 superseded the earlier startup guard and execution policy. The final approved hard cap was $5.75, with no new dispatch at $5.00 measured and measured plus conservative full in-flight bounds at most $5.50. Actual spending is below even the original $5.00 cap.

Runs used named no-focus panes in an owned `paid-runs` tab. Known-cost gate rejections were results, not reasons to replace a paid trial. Zero-dispatch CLI, readiness and session-write failures were retained and corrected inline. No paid run was silently repeated. E2 had no schema repair; the native gate's declared repair/tool-loop allowance remained inside each E6 run's measured cost.

E2 is an isolated reply experiment, not a complete workflow. The constructed hybrid preserves the same fields but is not a byte-exact historical prompt. Native E6 kept `--ctx-shell=open`, code dossiers, current profiles and both adversaries for full fixture gates. CRM used **only `group:crm1-ui-shell+3`**, covering shell, commerce, money and people. CRM experiment state is not a pass or planning authorization for the full real spec. The output cap and temporary network/session isolation are experimental constraints, not changed defaults.

Both routes use OpenRouter. Native response receipts identify `deepseek/deepseek-v4.1-flash-1017` for all 116 Flash requests and `z-ai/glm-5.3` for all 101 GLM requests. Moving aliases and variable provider tariffs prevent claims of independent models/providers or universally reproducible pricing. Shared prefixes and recorded cache counters do not establish a controlled cold-versus-warm comparison.

## Subjects and evidence

- Code fixtures come from the integrated baseline and use current bodies that trim without case conversion. These are behavior controls, not a claim that every whole fixture workflow has perfect acceptance tests.
- The real CRM spec is byte-exact SHA256 `b5811b75d2b7e626023014fdcb13eb02e65377e359019cff150a7aec927158f4`.
- CRM code is committed HEAD `a87b826d30db1c1d07a5adaab02f53a39475dec9`, excluding the user's five uncommitted working-tree changes. The rendering-seam source blob is `3531e14ca4d92c220b99ca0330a3116ffb13ddfb`.
- Six packet representations, frozen keys, gate inputs and private configurations have pre-execution SHA256 identities in the manifest. Canonical values and array order round-tripped equally across representations.
- The requested `c3-hardening/report.md` was absent. No delivery evidence was invented.
- Startup readiness reported no semantic index in the experiment worktree and no TypeScript language server. No backend was installed. Partial or unknown retrieval instrumentation is not evidence of absence.
- Source/spec/key contents, raw responses, role sessions and private benchmark snapshots are excluded from Git. Their private work directory is recorded in the task's `checks/work-dir.txt`.

## E2: equivalent representation

The amended population is **60 planned case-observations**: `(8 fixture + 2 CRM) x 2 trials x 3 formats`. All twelve calls completed. One TOON fixture reply was invalid JSON; the other eleven parsed. Missing and invalid cases remain in the denominator.

| Format | Present / planned | Native-valid / planned | Native-complete decisions | Supported-correct under frozen key | Matrix cost | Cost / supported-correct |
|---|---:|---:|---:|---:|---:|---:|
| Hybrid | 20/20 | 18/20 | 10 | 7 | $0.010970141 | $0.001567163 |
| TOON | 12/20 | 11/20 | 6 | 5 | $0.010370614 | $0.002074123 |
| Compact JSON | 20/20 | 18/20 | 9 | 7 | $0.008608714 | $0.001229816 |

Native-complete means the production reducer could accept a supported decision. It does not mean every correct deferral should pass. Owner and contradictory-evidence deferrals intentionally remain incomplete. Across formats, there were **52 present, 47 valid, 25 native-complete and 19 supported-correct decisions**. Including the failed pilot, E2 cost per supported-correct decision is **$0.001720135**.

| Format | Raw unsafe proposals / keyed native false approvals | Accepted actions disagreeing with key | Incorrect absence | Missed reuse/extension | Supported false blockers |
|---|---:|---:|---:|---:|---:|
| Hybrid | 2 / 2 | 3 | 3 | 1 | 0 |
| TOON | 1 / 1 | 1 | 1 | 0 | 0 |
| Compact JSON | 2 / 2 | 2 | 2 | 0 | 0 |

These are **mechanical fixed-key counts, not adjudicated production defect rates**:

1. The frozen trusted `go-provider` packet itself is malformed: `count_kind: unknown` with a total is a native system-data error. The original boolean may-pass smoke check mistook that error for an intended non-pass. Inputs and gold were not repaired after observing replies. This is an input/validation failure, not a model failure or proof that the negative control passed.
2. Accepted `go-wrong-name` new-code decisions disagree with the key. Independent packet/key adjudication is required before calling them production false approvals.
3. Hybrid CRM trial1 selected a new seam instead of the keyed extension. The scorer's false-approval flag uses expected may-pass state and misses this wrong action in an otherwise positive case. The separate accepted-wrong-action column retains it.
4. JSON CRM trial2 selected the expected action but lacked sufficient support and stayed incomplete. Missing support is not a successful positive or a supported blocker.
5. Zero supported false blockers does not mean every positive was accepted. No source/key ambiguity or incomplete case was removed to improve a rate.

TOON used about **5% more input tokens** than compact JSON on the heterogeneous fixture packet, but about **15% fewer** on the regular two-case CRM packet. Sampled output/reasoning length and discounted versus full provider tariffs also affected dollars. Compact JSON was cheapest in this matrix, but that is not a causal, provider-invariant codec result.

### Failed pilot and billing

The earlier pilot `E2-fixture-hybrid-t1` hit `finish_reason: length`, produced invalid JSON and had **0/8 valid/supported case coverage**. It is not a supported blocker or a silently replaced success. Complete-command latency was 35.230 seconds; provider generation time was separately 29.576 seconds, with 0.373 seconds reported latency. Its reasoning/completion split is inconsistent and was not repaired by inference.

Generation `gen-1792561488-D1OVkrrTAgUi7MvlBq2j` eventually reconciled exactly: raw `usage.cost`, generation `total_cost`, and settled key delta were all **$0.002733090**. The ledger preserves its original `cost:null` event and linked reconciliation, counted once. Early settlement-poll values were not journaled, so exact lag is unknown. Historical guard proofs do not certify the later simplified runner.

## E6: real native route smoke

All sixteen runs used actual native gate commands. Raw directories were snapshotted before same-spec full-gate outputs could be overwritten, and all runs were recorded in the private `paid-exploratory` suite.

| Project | Route | Trial | Native state | Complete required checks | Actual provider USD | API requests | Command seconds |
|---|---|---:|---|---:|---:|---:|---:|
| Go | Flash | 1 | incomplete | 5/6 | .006375578 | 12 | 137.416 |
| Go | Flash | 2 | pass | 6/6 | .005328204 | 10 | 60.293 |
| Go | GLM | 1 | pass | 6/6 | .005708218 | 10 | 36.206 |
| Go | GLM | 2 | pass | 6/6 | .006842689 | 12 | 32.699 |
| Python | Flash | 1 | fail | 6/6 | .006426450 | 16 | 102.873 |
| Python | Flash | 2 | pass | 6/6 | .004694242 | 14 | 64.971 |
| Python | GLM | 1 | pass | 6/6 | .007849330 | 11 | 30.984 |
| Python | GLM | 2 | pass | 6/6 | .006320078 | 11 | 25.433 |
| TypeScript | Flash | 1 | pass | 6/6 | .007661072 | 11 | 91.151 |
| TypeScript | Flash | 2 | fail | 5/6 | .009646018 | 12 | 112.938 |
| TypeScript | GLM | 1 | pass | 6/6 | .008818870 | 14 | 38.947 |
| TypeScript | GLM | 2 | pass | 6/6 | .012027842 | 17 | 73.453 |
| CRM subset | Flash | 1 | experiment | 9/13 | .032482826 | 19 | 369.144 |
| CRM subset | Flash | 2 | experiment | 10/13 | .029835396 | 22 | 238.732 |
| CRM subset | GLM | 1 | experiment | 12/13 | .026498878 | 4 | 94.581 |
| CRM subset | GLM | 2 | experiment | 11/13 | .144692160 | 22 | 272.321 |

Native required-check completion is **not supported-correct behavior coverage**. A completed review can accept a blocker, while an experiment can exit zero with unresolved owner questions. CRM's four trial results retain unresolved owner-routed entries. Question IDs and discovery vary per run, so fewer questions or higher completion do not prove correct closure or better authority handling.

### Findings and variation

- All three Flash fixture outcomes changed between repetitions: incomplete/pass, fail/pass and pass/fail. GLM's six fixture runs passed. This is observed variation, not an accuracy ranking.
- Python Flash trial1 accepted a blocker treating same-named identity functions in other/vendor/generated modules as parser's public contract. Namespace/decoy conflation is a concrete follow-up concern; the cited bodies exist, but source-location verification alone does not establish contract equivalence. Independent adjudication is still needed before publishing a false-blocker rate.
- TypeScript Flash trial2 accepted a missing-behavioral-acceptance blocker. The fixture's diff-only acceptance command and non-asserting test file are genuinely weak controls. Do not label every rejection a model false blocker or every pass a correct approval.
- Go Flash trial1 kept an unsupported adversary claim incomplete. TypeScript Flash trial2 also retained an unsupported adversary claim alongside its accepted blocker. Fail and incomplete are distinct states, not launcher failures.
- GLM CRM cost changed **5.46x** between trials, with 4 versus 22 underlying requests. More/fewer requests are not inherently better quality. GLM cost **$0.218758065** across eight runs versus Flash **$0.102449786**. GLM used 101 API requests versus Flash's 116, demonstrating that request count alone does not predict dollars.
- GLM was faster in all six matched fixture command measurements, but its second CRM trial was slower than Flash's second. Two trials and shared provider infrastructure do not support a general latency, price or reliability guarantee.

Whole-workflow false approval, false blocker, incorrect absence, missed reuse, supported-answer accuracy and cost per supported-correct behavior are **unavailable for E6**. There is no independently mapped current whole-group key, and package-check reviews are not interchangeable with per-behavior supported verdicts. Historical CRM keys were not joined to current code. No key, support rationale, lookup proof or authority was manufactured to produce a score.

## Accounting and measurement limits

The ledger sums to **$0.353890410**, below both the latest $5.75 cap and original $5.00. An independent deterministic reparse of all **217 native raw SSE receipts** matches every E6 run's request count, generation-ID set and exact Decimal cost, totaling **$0.321207851**. All costs are known; the pilot's null is resolved by its linked event. Zero-dispatch setup events cost zero and are excluded from model-quality counts, but remain visible.

Provider response charges are authoritative under messages008-011. The final one-per-wave account snapshot showed $0.348126415 above the original baseline, **$0.005763995 less** than the complete own-response ledger. This is an aggregate settlement discrepancy consistent with earlier lag, not a claim of exact final account reconciliation. No further billing polling was performed and missing aggregate settlement was not recorded as free usage.

A row's `cumulative` is the request-level measured snapshot at that command's finish, including known charges in other still-running commands. It can exceed the prefix sum of completed-run rows; the final settled command-row sum is the reported total. Do not add cumulative snapshots together.

The capped custom Pi metadata omitted pricing tables, so native SDK/benchmark cost fields show zero. **Those zeros are not verified free runs.** Use the outer ledger and upstream receipts for money; retain immutable native records unchanged. Per-pass times, token/cache counters and exploration traces are retained privately. Do not sum parallel pass seconds and call it command wall time. Command latency excludes study preparation and operator waiting; it is not total elapsed time to conduct this study. Separate provider-only latency was not measured for every amended call.

## Deterministic verification and private evidence

Before paid execution, repository build, vet and focused fixture/manifest tests passed:

```sh
cd tool
go build ./...
go vet ./...
go test -p 2 -count=1 -run 'Fixture|Manifest' .
```

The CPU-locked task `b9b8d4023` exited zero; the focused suite took 17.960 seconds. No production source changed afterward. No new full suite or guard/probe project was added during the final execution phase.

Actual E2 replies were graded with production `answer.Validate` and `ReduceReview`, with missing output and system-data errors retained. This is deterministic evaluation of observed replies, not a model-generated truth key. Initial serialization equivalence passed, but the malformed trusted fixture means the earlier blanket packet-validation interpretation was wrong.

Existing native `gate compare` and private `bench record/compare/report/verify` are used for E6. Four corrected invocation/configuration/scope refusals are preserved: gate compare needs spec plus two named experiments; the suite's original CRM-only scope excludes fixture groups by default; an obsolete configuration places the repo-only `check` setting in user scope; and a mixed-repository report requires an explicit cross-subject override. A fifth failure exposed an installed CLI mismatch: `bench report --help` advertises `--total`, but its argument validator rejects that flag. No production fix or suite-metadata rewrite was authorized. The final analysis uses the successful paid-run configuration, all four within-project comparison outputs and a CRM-scoped report, without relaxing cross-repository compatibility. Fixture-specific saved reports remain limited by the suite scope and report flag mismatch; their native records and successful comparisons are retained. Same-project full-pass comparisons explicitly use `--total=all`, retain warnings and do not rank across projects. No immutable record was rewritten to bypass a check. All four within-project, four-run benchmark comparisons and all three native CRM comparisons succeeded. Private `bench verify --suite paid-exploratory` passed with **0 errors and 2 informational historical-provenance notes** about copied subjects with note/unknown commit bases. Those notes are not current correctness evidence. The CRM-scoped JSON/saved report and native export completed successfully. The export contains all **16 native run records**. Fixture-specific report limitations did not discard verified records or prevent archival.

Three private archives are retained in the orchestrator task's `checks/` directory, outside the Git worktree, with owner-only mode `0600`:

| Private archive | Contents | SHA256 |
|---|---|---|
| `paid-exploratory.tar.gz` | Native benchmark suite, 16 runs, snapshots, role sessions and saved CRM report; 1,487,597 bytes | `55e9a127e80079a5124abdee849198a3d723f1eae0367bc78be5b8d700288bcc` |
| `paid-supplement.tar.gz` | Frozen packets, response receipts, command/score outputs, driver/config, before/after inventory and native receipt audit; 424 files | `703b7de6c0723ba609ab1b7453c2f4cac6ef5855241fcf91ca47dd241880bf0d` |
| `paid-evaluation.tar.gz` | Three actual temporary production-scorer/serialization test adapters, not shipping code | `3bafaa4b6602d899ed745826ba5508383c06f2868eb7c6b30c08c32fe6bbeb72` |

These archives contain private material and have no redaction. They are not committed or published. The owned `paid-runs` tab was closed after all model panes finished; no unrelated tab was closed.

Live store final protection is **PASS**: 568 entries, 466 files and 102 directories. Before and after listing SHA256 are both **`e2f6647ced1ea8ac251436b083d8374ef620580a608070b3f90a87a026c29f1c`**. Fields are relative path, type, mode, size, nanosecond mtime and file SHA256. The final listing is private `bench-after-final.json`. The live 23 runs and two keys remain unchanged; historical keys were not used as current truth.

## Recommendations

| Area | Decision and rationale |
|---|---|
| E2 format | Keep current direction/defaults. TOON's input benefit depends on shape; the cheapest observed JSON arm is not a workflow or safety win. Repair/adjudicate the packet/key defects in a separately scoped follow-up before using these screens for promotion. |
| E6 gate route | Keep current Flash default. GLM is execution-compatible and promising on measured fixture latency, but more passes are not a correctness key, overall cost was higher, CRM variance was large and n=2 is too small. |
| Gate trust/review | Investigate namespace/contract conflation and weak acceptance fixtures separately. Do not change production during this paid experiment or present location verification as semantic truth. |
| E3 excerpts / E4 prefixes | Deferred. Keep current behavior; no controlled causal result was collected. |
| E5 adjudication / question classifier | Keep disabled/current defaults. No authorized delegation or classifier promotion was tested. |
| Budget | No further money or model calls requested. Unused budget is not permission for extra trials. |

No safety, independent held-out confirmation, provider independence, full-CRM approval or product acceptance claim is made.
