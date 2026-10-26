# Spec file

A spec describes one feature and splits it into work packages. `orch plan
<spec>` makes one task per package, so a spec with settled decisions goes
to leads with no planning in the orchestrator. The `brainstorm` skill
writes specs; a person or a scout lead can write one too.

The spec lives in the repo (where the repo keeps specs, for example
`docs/<product>/specs/`). Commit it to the base branch before `orch plan`:
leads read it from their own worktree. `orch plan` also accepts a spec that
sits in a linked worktree of the repo, when it is committed to that
worktree's branch (the spec's `base:`).

## Format

```
---
harness: claude
model: sonnet
effort: medium
base: dev
---
# Server transfer queue

## Behaviour
What the feature does, from the user's view. 1-3 short paragraphs.

## Design rules
Decisions already made, one per line, with a link to the ADR when one exists.

## Authority
May decide: names, internal structure, test layout
Must escalate: API shape, data model changes, anything a user sees
Never: push, merge, edit files another package owns

## Packages

### transfer-api: Transfer API and its tests
kind: ship
model: sonnet
depends: transfer-model
owns: internal/panel/transfer/api.go, internal/panel/transfer/api_test.go

What this package does. Prefer `path#Symbol` pointers - why it matters.

#### Builds
- Queues a transfer and keeps it across panel restarts.
  id: queue-durable
  search: Code that saves a transfer for a server, when a user queues it, producing a durable queue row
- Lists queued transfers in the panel.
  id: queue-list
  search: Code that lists queued transfers

#### Acceptance
`scripts/agent/check go ./internal/panel/transfer/` passes; a test shows
a queued transfer survives a panel restart.

### transfer-model: Transfer table and store
kind: ship
model: haiku
effort: low
owns: internal/panel/transfer/store.go, internal/panel/transfer/store_test.go, migrations/

Body.

#### Builds
- Stores queued transfers and their retry state.

#### Acceptance
...

## Open questions
None.
```

## Rules

- **Front matter** gives defaults for `harness` (claude or pi), `model`
  and `effort`; a package line overrides its default. `base` is a
  spec-wide default only (no package line sets it) and falls back to the
  repo config.
- **`## Behaviour`, `## Design rules` and `## Authority`** are required.
  Authority needs its three lines; every task gets them. Put cross-cutting
  decisions in Design rules, but keep a rule that concerns one area inside
  that package's body: every lead and every per-package gate pass reads the
  shared sections in full.
- **Size.** Every character of a brief is re-read by its lead on every turn,
  so a package body is a cost to each lead: keep a brief under 15 KB
  (`orch check` warns above 15000 characters and names the three largest
  sections). Cite other docs and spec sections by line range
  (`path:first-last`), do not paste them into a package, and do not point a
  package at a whole file larger than 8 KB. A package body is never cut to
  save space: split the package instead.
- **`## Packages`** holds one `### <id>: <title>` block per package:
  - `<id>` becomes the task id (lowercase, starts with a letter, at most
    27 characters).
  - The first lines are `key: value`: `kind` (ship or scout, required),
    `model`, `effort`, `harness`, `depends` (ids of other packages in
    this spec), `owns` (comma-separated files or directories, required
    for ship). A blank line ends them.
  - The text after the blank line is the package's own spec.
  - `#### Builds` lists plain-prose deliverables, one bullet per item. Indented
    continuation lines are joined; the text is kept verbatim. Optional indented
    `search:` lines are jbcontext queries, one behaviour per query; several are
    allowed on a bullet with distinct behaviours. Optional `reuse:` lines name
    known existing code (comma-separated pointers). Optional indented `id:` names
    a stable requirement; `supersedes:` records explicit split or merge lineage.
  - Search guidance: `Code that <action> on <entity>, when <trigger>, producing
    <result>`. Drop any slot that does not apply; never invent symbols or paths.
    Good (full): `Code that saves a payment method for a customer, when a checkout
    completes, producing a stored method row`. Good (short): `Code that lists
    payment gateways`. Bad (noun list): `payments, customers, checkout, gateways`.
    Omit `search:` for tiny bullets without a search target, or near-duplicates
    that would only repeat each other. The gate shows `search:` lines in the
    checklist as written and runs no lookup for them; `orch spec queries`
    reads them.
  - A ship package without Builds gets a warning, not an error: its reuse
    checklist is skipped. Scouts are exempt. More than 8 bullets warns to split
    the package. An explicit `id:` must match `[a-z][a-z0-9._-]*` and be unique
    within its package. Its identity is `<package>#<slug>` regardless of bullet
    order; numeric IDs are not allowed. Without `id:`, an item keeps its legacy
    `<package>#<n>` (1-based) key. Legacy keys are local to the exact spec or
    itemset, not cross-version identities. No items are guessed from body/acceptance.
  - `items.json` keeps `queries` and `reuse` arrays and adds optional `revision`
    and `supersedes` fields. The content revision is a SHA-256 of trimmed
    requirement text and its trimmed `search:` and `reuse:` declaration lines,
    preserving their spelling and order. Any content edit needs a fresh answer,
    including a cosmetic edit; changing only an explicit ID's position does not
    change its identity or revision. Overall gate approval still requires the
    exact reviewed spec and checkout. Old archives without these fields remain
    readable and their stored hashes still verify.
  - A split or merge uses new IDs with `supersedes: <old-id>[, <old-id>]`.
    Each old ID is a slug in the same package, no longer an active requirement.
    Several replacements may supersede the same old ID. Mappings are recorded
    as qualified IDs in `items.json`; they describe lineage, never transfer an
    old answer or approval. Do not match by wording or shared source functions.
  - Prefer `path#Symbol` (methods: `path#Type.Method`) anywhere existing code is
    referenced. `path:line` and `path:line-line` remain useful for a block inside
    a function, SQL, config, generated code or docs. The gate verifies the file
    defines the symbol via ctx's declaration tier; unsupported languages fall
    back to a word match with a note. Plan copies pointers as written.
  - `#### Acceptance` is required: commands and expected results, or for
    a scout, the evidence each finding needs.
- **Ownership is disjoint.** No file or directory belongs to two packages,
  and no owned directory contains another package's file. A change and its
  tests stay in one package.
- **Model per package** follows the judgment the package still needs, not
  its size: haiku for a short package that acceptance tests can fail,
  sonnet for settled work that nothing mechanical checks or that is high
  risk, the strongest model only while a design is still open (which a
  finished spec should not have).
- **`## Open questions`** must be empty or say `None.`. `orch plan` refuses
  a spec with open questions: ask them first.

## What `orch gate` does

`orch gate <spec>` runs before `orch plan` and records a verdict for the
spec's exact text (a change to the spec voids it). It checks that every
`path#Symbol` or `path:line[-line]` pointer and every acceptance command exists, then runs four
read-only passes on a cheap model (the `gate` role, default DeepSeek
Flash through pi): one per package for scope and open questions, and two
adversaries. A model
claim counts only when its evidence line exists in the repo, and only a
question whose answer changes visible behaviour blocks. The verdict is
the `state` field: `error`, `stale`, `fail`, `incomplete`, or `pass`, in that
precedence order. Experiments use `experiment` and never authorize planning.
Unavailable reviewers leave required checks `incomplete`; invalid system
records cause `error`. Only a subject-bound `pass` with a successful current
reducer replay unlocks `orch plan`. The report, findings and
question cards are in `<run root>/gate/<hash>/`. Default stdout is the v2 TOON
selection; `--human` and `--json` serialize the same facts as readable and
machine JSON. Gate schema 1 is retired; use `--schema-version 2` or omit it.

## Requirement splits, merges and migration

For a split, both new requirements explicitly name their predecessor:

```
#### Builds
- Saves a transfer before acknowledging it.
  id: queue-save
  supersedes: queue-durable
  reuse: queue.py#Queue.save
- Reloads saved transfers on restart.
  id: queue-restore
  supersedes: queue-durable
  reuse: queue.py#Queue.restore
```

For a merge, the new requirement names both predecessors:

```
#### Builds
- Shows queued and running transfers in one accessible view.
  id: transfer-view
  supersedes: queue-list, running-list
  reuse: transfers.ts#TransferView.render
```

`orch spec ids <spec>` prints deterministic proposed `id:` lines for legacy
bullets, without editing the spec or writing any archive. Existing explicit
IDs are kept and proposed names avoid active and superseded IDs. Repeated requirement text
is marked as ambiguous history. Review the proposal and add IDs by hand.
Historical mappings always require an explicit mapping and revalidation; the
preview never accepts a historical answer or infers a split or merge. Two
requirements that cite the same function stay separate requirements.

## Inspect or propose queries

`orch spec queries <spec>` is a parser, with grouped text or `--json`.
`--package <id>` or `--item <package>#<slug>` selects a subset.
`--item <package>#<n>` still selects legacy bullets. It shows every item's
text, search lines and reuse pointers without a model or repo state.

`orch spec queries <spec> --generate --package <id>` proposes search lines
only for items without them, or Builds plus search lines for a legacy package.
It uses role `spec_queries`, never edits the spec, and prints text to paste.
`--dry-run` prints the prompt without calling a model. Whole-spec generation
requires explicit `--all`; a package or item selector is the normal use.

The gate runs no lookup for the checklist: it shows each item, its `reuse:`
pointers and its `search:` lines as written. Reference precision/recall in
`orch gate compare` scores locations rather than item wording.

## What `orch plan` does

`orch plan <spec> [--dry-run] [--skip-gate]` checks the whole spec first and
lists every problem; it makes no task until the spec is clean. It refuses a
spec the gate has not passed, a spec that
is not committed to the base branch or has uncommitted changes, unknown or
circular dependencies, overlapping ownership, and a task id already used
by a task from somewhere else.

Each task.md points to the spec (`spec: <path>#<id>` in its front matter)
and copies only the package's own text: Intent names the feature and the
package, Scope In is `owns` and Scope Out is the other packages' files,
Pointers cite the spec's sections as `path:first-last` line ranges, Spec is
the package body, Acceptance and Authority come from the spec. Scope Out
lists the other packages' files while that list stays under 1000
characters; past that it names the other packages and says where their
`owns:` lines are. A rerun skips tasks it made before
(`exists`), so it is safe to run again after adding packages. A task also
stores `spec_hash`; if its package changed since, the rerun reports it
`stale`, exits 1 and rewrites nothing. Close its lead, remove the task
directory by hand and rerun.

When any package has `depends`, plan cuts `orch/int-<spec file name>` from
the base and gives every ship task that base plus `integration: <branch>`.
`orch verify` requires a clean, committed candidate, pins HEAD before scope
and acceptance checks, rejects checks that move HEAD or write non-ignored
files, and merges only that pinned SHA into it. The merge result is checked
too, and the integrated marker binds candidate and merge SHAs; dependents
start from the integration tip. Ignored build outputs are allowed. Use
`--no-merge` for preliminary working-tree validation, reported as
`verify: pass (not integrated)`, not integration approval. An already merged
candidate reports that no post-merge check ran. Conflicts are aborted; failed
or mutating post-merge checks roll back and clean their non-ignored outputs.
The branch is never the base branch: you merge it there and delete it, and
`orch integration remove <slug>` drops its merge worktree.

Output lists each task with its status and the waves: a wave holds the
packages whose dependencies are all in earlier waves. Check and spawn the
first wave; spawn the next one when its dependencies are done.
