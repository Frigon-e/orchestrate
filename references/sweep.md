# Question sweep

Use a sweep when many open questions (from ADRs, specs, notes, earlier runs)
need answers and the person should see only the ones that are really theirs.
Several passes answer on their own. `orch sweep` compares their records with
the exact cards and current code, without making model calls.

Agreement is not proof. A citation that resolves is not claim support. Sweep
requires an attributable support assessment as well as agreement, and keeps
the assessment's method visible rather than calling a model score verified.

## Stages

| Stage | Who | Output |
|---|---|---|
| 1 Collect | one Flash scout | source inventory in `<topic>/manifest.json`, cards in `questions/`, and required `dropped.json` (write `[]` when empty) |
| 2 Evidence | Flash scouts, one per area | `evidence/<area>.md`: facts with source quotations, no recommendations |
| 3 Bind and answer | code, then fresh independent `sonnet` and `flash` passes | run `orch sweep <topic>` to publish current bindings in `sweep/results.json`; passes write `passes/<pass>/<key>.json` |
| 4 Compare | `orch sweep <topic>` | settled, disagree, recheck, user; durable bindings and outcomes in `sweep/results.json` |
| 5 Settle | one `settle` pass on disagreements only | `passes/settle/<key>.json`, with the same binding and support requirements |
| 6 You | `orch ask --cards <topic>/sweep/for-user` | one page of the real owner questions |

Keep `<topic>` outside the code checkout, preferably under the normal
orchestrate runtime directory. If runtime state is inside the checkout, use
the declared `ORCH_HOME` exclusion. Arbitrary non-ignored topic files inside
the code checkout change the subject, including when passes write answers.
Do not broaden exclusions to hide code changes.

Why Flash writes no recommendations: weighing options is judgment, while
finding what code, an ADR or a competitor does is gathering. The quoted facts
can be checked, but checking their location alone does not validate an
interpretation.

## Source accounting

Collect writes `manifest.json` from the original source questions, not from
only the cards that survived collection. Preserve stable source IDs and the
original question text. A card mapping accounts for a source that is still
open or is resolved by this sweep. Use a separate dropped entry for each
merged, previously answered or deliberately excluded source.

```json
{
  "schema_version": 1,
  "questions": [
    {"id": "adr7-question1", "question": "Which transport?", "card": "transport"},
    {"id": "notes-question3", "question": "Which transport should we use?"},
    {"id": "old-product-question2", "question": "Keep the abandoned product?"}
  ]
}
```

`card` is the filename without `.md` under `questions/`. Each card must have
one source mapping. Do not silently map several sources to the same card;
retain their separate identities through `duplicate_of` instead.

```json
[
  {"source_id": "notes-question3", "question": "Which transport should we use?",
   "reason": "The same decision and option meanings as transport.",
   "duplicate_of": "transport"},
  {"source_id": "old-product-question2", "question": "Keep the abandoned product?",
   "reason": "The product was abandoned and is outside the shipped scope.",
   "disposition": "not applicable"}
]
```

Every dropped entry needs its manifest `source_id`, exact source `question`,
a nonempty `reason`, and exactly one of:

- `duplicate_of`: an existing card key. This preserves the alias.
- `answered_by`: the source pointer that already answers it. Also supply
  `subject`, `evidence_records` and `claim_support` as described below.
  Copy `subject` from `sweep/results.json`'s `manifest_subject`, which binds
  these resolutions to the exact manifest bytes and current A2 code subject.
  The assessment must reference the exact `answered_by` path/line pointer
  (or opted-in URL), not a different resolving citation.
- `disposition`: an explicit disposition, with a concrete justification in
  `reason`. This records a scope or applicability decision, not claim support.

Missing, unreadable, malformed or null accounting files, invalid entries,
repeated accounting, nonexistent duplicate targets, and unaccounted sources
make the command exit nonzero. Findings remain visible in the output and
`sweep/results.json` records `accounting_ok: false`. Do not apply any outcome
from a failed sweep. Sweep cannot discover source questions omitted from the
inventory; review the collect inventory against its original inputs.

## Binding answers

Run `orch sweep <topic>` after collecting the inventory and cards. Missing
answers go to `recheck`; the command still publishes `sweep/results.json`.
Each `questions.<key>` record contains the current `card_rev` and `subject`.
Copy both into every independent answer file before assessing the question.
Never copy another pass's answer or relabel an old assessment with new hashes.

- `card_rev` is full SHA-256 of the card's exact raw file bytes. This covers
  question text, area, option IDs, labels and meanings, context, and any other
  card change. Even formatting changes require a fresh pass.
- `subject` is the A2 `gateSubject`, using those same card bytes as its spec
  input. It includes repository and checkout identity, commit, tree, dirty
  and untracked content/modes, and the declared runtime exclusion.
- Any missing, invalid or different binding makes that answer ineligible and
  sends the card to `recheck`, including a stale final-pass answer. No automatic
  reuse or revalidation is attempted. Human-only cards still go to the user.
- Each evidence record also binds its own current source bytes. This catches
  changes in named external checkouts even when the primary subject is stable.

The answer format retains the existing fields and adds bindings and B1
support records. The placeholders below must be replaced with current hashes
and the full copied A2 subject object, not the abbreviated placeholder.

```json
{
  "choice": "script",
  "confidence": 0.8,
  "reason": "The documented interface requires a script invocation.",
  "evidence": ["docs/adr/0007.md:12"],
  "owner": "agent",
  "card_rev": "<full card SHA-256 from results.json>",
  "subject": {"...": "copy the full current questions.<key>.subject object"},
  "evidence_records": [{
    "schema_version": 1,
    "id": "adr7-interface",
    "subject_id": "<copied subject.id>",
    "source_kind": "documentation",
    "path": "docs/adr/0007.md",
    "range": {"start_line": 12, "end_line": 12},
    "content_hash": "<SHA-256 of exactly the selected raw source bytes>",
    "lookup_id": "adr7-read",
    "locator_valid": true
  }],
  "claim_support": {
    "schema_version": 1,
    "state": "supported",
    "assessor": "sonnet-interface-review",
    "method": "model",
    "evidence_ids": ["adr7-interface"],
    "rationale": "The quoted interface is the one this option implements.",
    "excerpt": "The interface accepts a script invocation.",
    "failure_scenario": "Without a script entry point, the documented caller cannot start the operation."
  }
}
```

## Citation validity and claim support

- `choice` must be one of the card's option IDs. Free-text cards go to the user.
- Legacy `evidence` strings stay readable: `path:line` in the repo, or
  `<root>/path:line` for another checkout named by
  `--roots polar=/path,stripe=/path`. Every supplied local pointer must resolve.
  A supplemental legacy URL does not negate assessed local support, but is
  not itself used as support without the URL snapshot requirements below.
- `evidence_records` use B1 `evidenceRecord` fields. Give each an ID, current
  `subject_id`, valid `source_kind`, path, `content_hash`, origin `lookup_id`,
  and `locator_valid: true`. Sweep checks the locator and bytes itself.
  Unknown or partial completeness does not count as support.
- Hash selected raw bytes with full SHA-256. A line range is one-based and
  inclusive, including its original line terminators. Without `range`, hash
  the whole file. Sweep currently accepts line ranges and whole files, not
  column slicing. Symlinks may not escape the repo or named evidence root.
- `claim_support` is a B1 `claimSupport`: `supported`, an assessor, a method
  (`structural`, `model`, or `human`), referenced evidence IDs, a rationale,
  and a nonempty quoted excerpt present in the cited source bytes. Source
  and test claims also require a concrete `failure_scenario`. Supply one for
  other behavioral claims where applicable. Unsupported, conflicting, unknown,
  incomplete or stale assessments remain unresolved.
- A live URL is never fetched or verified by this command. `--accept-urls`
  allows an explicitly assessed quotation snapshot: use the URL as record
  `path`, omit `range`, and hash the exact quoted `excerpt` bytes. The flag
  does not supply an assessment and never makes a bare URL claim support.
- `confidence` is descriptive model output, not a support assessment or proof.
  Valid citations without an assessment are visible as unresolved work in
  `recheck`, pass observations, and any user card.
- `owner` is `agent` (default), `user`, or an outside expert such as
  `accountant`. A current bound pass naming another owner routes to the user.

## What `orch sweep` decides

- **settled**: at least `--min-passes` (default 2) current bound passes chose
  the same option, each with explicit current claim support. Agreement alone
  never settles a question.
- **final**: the valid supported passes differed and the pass named by
  `--final` (default `settle`) answered with current binding and support.
- **disagree**: the supported passes differed and there is no valid supported
  final answer. Run the final pass on these.
- **recheck**: answers are missing, stale, invalid, pick a non-option, have
  unresolved citations, or lack claim support. Reassess the affected passes.
- **user**: the card's `area:` is in repo `human_only`, a current bound pass
  named another owner, or the card is free-text. `sweep/for-user/<key>.md`
  preserves the card and adds each pass's answer, citation observation and
  attributable support or unresolved work. Human-only routing wins even if
  models agree or their bindings have gone stale.

`sweep/results.json` records each card's binding, status, choice, explanation,
and pass observations (`citation_valid` separate from `claim_support`).
Keep `manifest.json` and `dropped.json` with these outcomes as the source
accounting record. Existing unbound answers remain readable history but need
fresh binding and assessment before they can settle anything.

## Spec outline

```
### collect: Gather every open question
kind: scout
model: flash
Write manifest.json with every original source question and its stable ID.
Write one card per distinct question in questions/, with an area such as
billing, tax, legal, product or infra. Account for every merged or skipped
source in dropped.json, with a reason and duplicate_of, answered_by plus
support, or a justified disposition. Write [] if none were dropped.
Do not answer open questions. Do not reconstruct the inventory from cards.

### evidence-billing: Evidence for the billing area
kind: scout
model: flash
depends: collect
For each billing card gather source quotations with exact paths/ranges and
content hashes. Record what the code and ADRs say. Do not recommend.

### answer-billing-sonnet: Answer the billing cards
kind: scout
model: sonnet
depends: evidence-billing
Use the current published card_rev and A2 subject for each billing card.
Read its card and evidence/billing.md. Inspect the cited source bytes before
recording an attributable claim_support assessment. Write passes/sonnet/.
Do not read other passes. Leave unsupported work unresolved.
```

Repeat evidence and answer packages for each area. A brief cannot prevent a
lead from reading another pass's folder, so name only its own files in Pointers.

## Rules that keep it honest

- Money, tax and legal areas go in `human_only`. Model agreement about an
  invoicing rule is not an accountant's answer.
- Passes can share a wrong interpretation of the same evidence. Structural
  validation establishes freshness and attributable support records, not
  independent factual verification or semantic truth.
- `orch sweep` writes nothing to `questions/answered/`. Move a card there
  yourself once the person has answered it, and update the source accounting
  with the recorded resolution before the next sweep.
- Cost: one Flash collect, area evidence scouts, independent answer passes,
  and a stronger final pass only for disagreements. Comparison, bindings and
  accounting are deterministic and add no model calls.
