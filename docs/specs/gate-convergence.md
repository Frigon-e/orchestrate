---
harness: claude
model: sonnet
effort: medium
base: main
---
# Gate convergence

## Behaviour
`orch gate` reaches a pass once every blocker is handled, instead of finding a
new batch each round. Each spec keeps a findings ledger across runs. The author
(user or orchestrator) closes a finding as fixed, accepted risk or rejected,
with a reason. A rerun reviews what changed since the last run and rechecks the
open findings; it does not start from nothing. The gate passes when no verified
blocker is open. Open questions and unsupported claims are listed as advisory
and travel to `orch plan` as known risks; they no longer withhold a pass.

Specs describe intent, contracts and acceptance. The gate checks those, not
function signatures or internal layout that a lead may decide.

Why: as of 2026-10-28, 34 recorded gate runs across all repos produced 0
passes (15 fail, 8 incomplete, 11 unreadable). Each round sends fresh
reviewers over the whole spec with no memory of earlier rounds, every fix adds
text to attack, and no command can close a finding, so every spec reached
`orch plan` through `--skip-gate`.

## Design rules
- The ledger is keyed by the spec's repo-relative path, not its content hash, and lives in the run root next to `gate/`. It records each finding's ledger ID (L1, L2, ...), first and last run, severity, text, state (`open`, `fixed`, `accepted`, `rejected`), reason, who closed it and when.
- `fixed` is a claim the next run verifies: the reviewers are asked about it and it reopens when they still support it. `accepted` and `rejected` close the finding for good unless the spec text it cites changes.
- Finding identity across runs is decided by the reviewers, not by text digests: a rerun gives every pass the open and `fixed` ledger items with their IDs, and each pass answers per ID `still open` or `resolved` with evidence before reporting new findings. A new finding that restates a ledger item is merged into it.
- A new finding on spec text unchanged since the previous run is capped at concern unless two passes report it independently. New findings on changed text keep their severity.
- Pass rule: deterministic checks clean, required passes complete, and no open verified blocker in the ledger. Questions and unsupported claims are advisory.
- The first run of a spec (empty ledger) is a full review, as today.
- gate.json keeps its schema; ledger state is a separate file. Existing gate records stay readable.
- Build after the structured-reports spec has merged: both touch tool/gate.go and references/gate.md.

## Authority
May decide: internal names, ledger file format, prompt wording, test layout
Must escalate: the pass rule, the severity cap, anything that lets a blocker close without an author action or a reviewer's evidence
Never: push, merge, edit files another package owns, delete gate records

## Packages

### gate-ledger: Findings ledger and orch gate resolve
kind: ship
model: sonnet
owns: tool/gate_ledger.go, tool/gate_ledger_test.go

A ledger per spec path with load, save (locked, atomic) and merge of a run's findings. `orch gate resolve <spec> <L-id> fixed|accepted|rejected --reason "<text>"` closes an item; `orch gate ledger <spec>` lists items by state. Register the subcommands where `orch gate` parses its arguments, through a hook that gate-converge calls, so this package edits no gate.go code.

#### Builds
- Keeps findings per spec across runs with stable L-ids.
  id: ledger-store
- Lets the author close a finding with a reason.
  id: ledger-resolve

#### Acceptance
`go test -p 2 -count=1 -run 'Ledger' ./...` in tool/ passes, covering: merge keeps L-ids across runs; resolve requires a reason; `fixed` stays in the recheck set; concurrent saves do not lose items.

### gate-converge: Delta review and the new pass rule
kind: ship
model: sonnet
depends: gate-ledger
owns: tool/gate.go, tool/gate_reduce.go, tool/gate_reduce_test.go, tool/orch_test.go

On a rerun, give each pass the diff between the last reviewed spec text and now plus the open and `fixed` ledger items, and require per-item answers before new findings. Apply the severity cap and the merge rule, write results to the ledger, and reduce the state with the new pass rule. The report lists open ledger items first, then advisory questions and unsupported claims. `orch plan` copies open advisory items and accepted risks into each affected task.md as known risks.

#### Builds
- Reruns review the change and recheck open findings.
  id: delta-review
- Passes when no verified blocker is open.
  id: pass-rule
- Plan carries advisory items and accepted risks into task briefs.
  id: plan-risks

#### Acceptance
`go test -p 2 -count=1 -run 'Gate|Plan' ./...` in tool/ passes. With a fake reviewer: a run with one blocker fails; after `orch gate resolve ... accepted` the rerun passes; a `fixed` blocker the reviewer still supports reopens; a new blocker on unchanged text becomes a concern unless a second pass reports it; an open question alone does not withhold a pass and appears in the planned task.md.

### gate-docs: Intent-level specs
kind: ship
model: haiku
depends: gate-converge
owns: references/spec-format.md, references/gate.md, SKILL.md

`references/spec-format.md`: a spec states behaviour, contracts between packages, decisions, authority and acceptance; it leaves function signatures and internal layout to the lead unless they are a shared contract. `references/gate.md`: the ledger, `orch gate resolve`, delta reruns and the pass rule. `SKILL.md`: when a gate fails, close or fix each blocker and rerun; never loop on fresh full reviews.

#### Builds
- Spec guidance is intent-level and the gate docs describe the ledger loop.
  id: docs-intent

#### Acceptance
`rg -n 'orch gate resolve' references/gate.md SKILL.md` matches in both files.

## Open questions
None.
