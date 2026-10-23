---
name: brainstorm
description: Runs a solo brainstorm with the user on a product idea, a feature or a technical architecture, in 5 stages (frame, diverge, converge, decide, record), with fresh lens agents for ideas and Lavish question pages for decisions. Ends in a spec that `orch plan` turns into orchestrate tasks, a short ADR for hard-to-undo choices, and parked ideas in ideas.md. Use when the user wants to brainstorm, explore options, think through a feature or a design, or decide an approach before building. Not for company strategy.
---

# Brainstorm

You think with the user, in one fast context. You make many ideas before
you judge them, the user makes every decision, and the result goes into
the repo as files that orchestrate can build from.

Ask every question with the `question-card` skill: context first, STE-style
language, the project's own terms. `orch` means
`~/.agents/skills/orchestrate/scripts/orch`; call it by that full path.

## Setup

1. Pick a topic slug (`transfer-queue`). The scratch folder is
   `<repo>/.brainstorm/<topic>/`. Make sure git ignores `.brainstorm/`; if
   not, add it to `.git/info/exclude` (a local file, no commit).
2. Pick the track from the user's words: **product** (a feature or product
   idea) or **architecture** (how to build something). Say which one in
   one line; the user can change it.
3. Read only what frames the topic: `docs/GLOSSARY-MAP.md` when it exists,
   the ADRs and specs the topic touches, and the product's `ideas.md`, so
   you do not suggest a parked idea again without its reason.

## 1. Frame

Write `frame.md` in the scratch folder, under 200 words:

- **Problem:** one sentence, from the user's view.
- **Who:** the user, operator or system that has the problem.
- **Limits:** hard constraints (compatibility, budget, deadline, stack).
- **Success:** how we know it works; numbers when there are numbers.
- **Not in scope.**
- **Criteria:** 3-5 measures to score ideas. Product: user value, reach,
  cost to build, risk. Architecture: simplicity, change cost later,
  performance, operations load, security.

Show the frame in chat and ask one yes-or-no card: "Is this the problem?"
Do not start stage 2 on a frame the user has not accepted.

## 2. Diverge

Make many ideas before you judge any. Pick each agent's model from how hard
its job is; do not move cheap work up a tier.

1. **Facts (cheap):** one `Explore` agent (Haiku) collects what exists: related code, docs, earlier decisions,
   parked ideas. Ask for a brief under 400 words, pointers not pasted
   code. Skip it when the topic does not touch existing code.
2. **Lenses (mid tier):** three fresh agents in parallel, each with
   `frame.md`, the facts brief and one lens. On Claude Code use the Agent
   tool with `model: "sonnet"` and tell each one it is read-only. No agent
   sees the others' ideas.
   - Product lenses: the user and their job; value and cost to the
     business; what is possible with the current code.
   - Architecture lenses: the simplest design that works; scale and
     performance; risk, security and operations.
   Each returns 5-8 ideas: a name, one sentence, why it could win, the
   biggest risk. Ask for that format and nothing more.
3. **Small topic:** when the frame has one obvious axis, skip the lenses
   and write the ideas yourself. Say so in one line.

Save all ideas to `ideas-raw.md` in the scratch folder. Do not show the raw
list to the user.

## 3. Converge

1. Merge ideas that are the same, and group the rest into 3-6 directions.
2. Score each direction against the frame's criteria (high, medium, low,
   with one reason each).
3. Write an idea map, `map.html`, in the scratch folder: one table, a
   column per criterion, one row per direction. Use semantic table markup
   and no script; `orch ask` cleans it.
4. Keep the top 2-3 directions. Every other direction goes on the parked
   list with a one-line reason.

## 4. Decide

1. Write one card per decision to `.brainstorm/<topic>/questions/<key>.md`
   (format: `~/.agents/skills/orchestrate/references/question-file.md`).
   The first card picks the direction and includes `../map.html`. Later
   cards settle the open choices inside that direction. Add a diagram when
   a choice is about structure or flow; draw it for a 360px-wide screen.
2. Run `orch ask --cards .brainstorm/<topic>/questions`. Read only its
   output lines, never the page. Fix any card it lists under `warnings`.
3. Open the page with `lavish-axi <page>` and wait with
   `lavish-axi poll <page>` (Monitor or a tracked background task). One yes-or-no question stays in chat.
4. Each answer arrives as "Answer for question <key>: ...". Move that card
   to `questions/answered/`, write the decision into `decisions.md` in the
   scratch folder, and rerun `orch ask --cards` when new cards are open.
5. When the user does not understand a card, write it again per the
   question-card re-pitch rules.

Stop deciding when the spec in stage 5 needs no more answers.

## 5. Record

Write each result where the repo keeps it; follow the repo's own rules
(its docs README, CLAUDE.md or AGENTS.md) over these defaults.

| Result | Where |
|---|---|
| Work to build | A spec in the format of `~/.agents/skills/orchestrate/references/spec-format.md`, where the repo keeps specs |
| An architecture choice that is hard to undo | An ADR of about 1 page: problem, options, decision, results. Tables and numbers go in the spec, and the ADR links to it |
| Parked or rejected directions | The product's `ideas.md`: one line each, with the reason and the date |
| New terms | Proposed to the glossary the `GLOSSARY-MAP.md` points to; the user approves each term |

For a spec:
1. Give each package disjoint files, its tests, a model from the judgment
   it still needs, and runnable acceptance.
2. Give each package `#### Builds` (plain-prose bullets, one deliverable
   each, with optional `search:` and `reuse:` lines) in the format of
   `~/.agents/skills/orchestrate/references/spec-format.md`. Find real targets
   with `orch ctx`; never invent paths or symbols.
3. `## Open questions` must say `None.`; if not, go back to stage 4.
4. Inspect with `orch spec queries <spec> --package <id>`. If needed,
   `--generate` proposes missing queries as pasteable text, never edits;
   `--dry-run` prints its prompt. Use a package/item selector, not whole-spec
   generation (which requires explicit `--all`).
5. Show the user the files, then ask before you commit them (never
   push). After the commit, run `orch gate <spec>` and `orch plan <spec>
   --dry-run`, and fix every problem they list. Tell the user the waves and
   that orchestrate starts the build with `orch plan <spec>`.

Keep `.brainstorm/<topic>/` until the user says the topic is done, then
delete it. End the Lavish session with `lavish-axi end <page>`.

## Rules

- Never run `lavish-axi share`: it publishes to a third-party site.
- The user decides; you recommend. Do not record a decision the user did
  not make.
- Keep your context lean: agents return short briefs, and you read tool
  output lines, not generated pages.
