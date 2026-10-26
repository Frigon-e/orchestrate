# Question card file

A lead writes one card per open `needs-decision` or `blocked` line at
`<task dir>/questions/<key>.md`, where `<key>` is the `--key` of that report.
`orch ask` turns the open cards into one page for the user. A decision with
no card still appears, as a free-text question using the status line.

Outside orchestrate (for example the `brainstorm` skill), `orch ask --cards
<topic>/questions` checks every root `<key>.md` in that folder, with no run
and no `orch send` line. Only ready cards routed `owner` or `unclear` enter
the answer page. The CLI output counts and lists cards settled by the lead
and cards that need more research or preparation. It also surfaces `INDEX.md`
notes and retrieval warnings. Give these warnings to the user with the page;
do not share the page alone. Move an answered card to `questions/answered/`
so it leaves the page. Asking never changes cards or answered history.
Includes are confined to `<topic>/`, and the page goes to `<topic>/ask/`
unless `--out` says otherwise.

## Format

```
---
question: Does a script make the question page, or does the orchestrator write it?
recommend: script
change: easy
route: owner
ready: ready
owner: orchestrator
---
## Context
Markdown, 4-8 sentences or a short list: goal, state (with the files or
numbers you checked), cause, constraints, and what you have not verified.
Do not repeat the options here.

include: diagram.svg How a question moves from lead to you

## Options
### script: A script makes the page
Gain: ...
Cost: ...

### orch: The orchestrator writes the page
Gain: ...
Cost: ...
include: shot.png Screenshot of the current page

## Next
Markdown.
```

- Front matter is `key: value` lines between `---` lines. `question` is
  required. `recommend` is optional and must be an option id. `change` is
  optional: `easy` or `hard` (how costly it is to change the answer later).
  `area` is optional: the topic of the question. `orch sweep` uses it to
  route human-only areas; the page does not show it.
- `route` records decision authority: `owner`, `lead-may-decide`, or `unclear`.
  Product choices stay with their owner unless explicitly delegated. Missing
  and unknown routes are treated as `unclear`, not as permission for the lead.
  A ready `lead-may-decide` card is listed under "Settled by lead" in the
  output, not asked on the page. This routing does not record a user answer.
- `ready` is separate from `route`. It records preparation, not authority.
  The check computes readiness even when the field is absent, and a stale
  `ready: ready` cannot bypass it. Explicit `missing_context`,
  `missing_tradeoffs`, `missing_evidence`, and `failed_retrieval` values keep
  recorded gaps open until the author clears them after research. A parseable
  card marked `ready: unclear` is held as `missing_evidence`, not a parse error.
- `action` or `owner` may name the required action or responsible person.
  A nonempty `## Next` also meets the action requirement.
- Sections are `## Context`, `## Options`, `## Next`. Any other `## `
  section renders after Context.
- Under Options, each `### <id>: <label>` starts an option. The id is a
  lowercase slug (letters, digits, `-`); the body is markdown. No options
  means a free-text question in run-status `orch ask`. One option is an error.
  For `--cards`, supply at least two options, each with explicit `Gain:` and
  `Cost:` lines and nonempty effects. Markdown emphasis and bullets are allowed.

## Includes

A line that is exactly `include: <relpath>` or `include: <relpath> <alt text>`
in a section is replaced by the rendered file. `relpath` is relative to the
`questions/` directory.

| Extension | Result |
|---|---|
| `.md` | Rendered markdown (raw HTML is dropped). |
| `.html` | Inserted after cleaning: scripts, event handlers, form controls, ids and `javascript:` links are removed. Layout, `class` and `style` stay. |
| `.mmd`, `.mermaid` | Mermaid diagram. |
| `.svg`, `.png`, `.jpg`, `.jpeg`, `.webp`, `.gif` | Image; alt text is required. An SVG runs no script. |

Limits: the file must resolve (symlinks followed) inside your task directory,
exist, and be at most 1 MiB. Other extensions are refused.

SVG diagrams: the page can be 360px wide, and an image only shrinks. Draw
for a viewBox about 420 wide, with text at 14px or more, and stack steps
from top to bottom, not side by side. An `<img>` does not take the page's
theme, so put the colours in a `<style>` with an
`@media (prefers-color-scheme: dark)` block, and keep text contrast at 4.5:1
in both themes.

## Deterministic readiness (`orch ask --cards`)

Readiness uses no model and performs no search. It reports these states:

| `ready` state | Required repair |
|---|---|
| `unclear` | Fix the card's syntax or unreadable file. This state is only for cards that cannot be parsed or loaded. |
| `failed_retrieval` | Retry or resolve an attached search failure, then update the card with the result and search limits. |
| `missing_context` | Add Context with the checked state, goal and constraints. |
| `missing_tradeoffs` | Add two or more options, each with Gain and Cost effects. |
| `missing_evidence` | Add a recommendation or next action/owner, or resolve an explicit `ready: missing_evidence` research gap. |
| `ready` | The structural requirements pass. This is not proof that evidence supports a claim. |

The output gives each withheld card's reason and required repair. A refused
include is `missing_evidence`, not a complete question with a free-text fallback.
Counts account for every root card, including bad filenames. `INDEX.md` is an
overview, not a card; its notes and warning sections appear under `warnings`.
An INDEX warning alone does not mark every unrelated card as failed retrieval.
Attach failures to affected cards under `## Review evidence`, using explicit
text such as `Code search failed: timeout`, `Code search did not run: missing
index`, or `Code search skipped after earlier search failures.` Fenced examples
are not retrieval reports.

A research gap asks the lead to check facts, not the owner to guess. A true
owner decision remains with the owner after the research is complete. Do not
turn missing evidence or a failed search into delegated decision authority.

Run-status `orch ask` keeps its existing behavior: a parse or include error
produces a free-text fallback and a warning. This readiness filter currently
applies only to `--cards`; gate publication is a separate integration.
