---
name: route-work
description: Decides how a coding task gets done, by which model and in which context. Size scope with a cheap Haiku scout, keep single-use output (browser, wide reads, logs) out of the main context, judge how much design work is still open, then either do it yourself or hand a concrete plan to a cheaper implementer agent, and verify the result. Use this for any request to implement, fix, change, refactor, or add something in a codebase (bug reports, feature requests, "make X do Y", tickets, multi-part changes), especially when the scope isn't clear yet or the work could be split up. Not needed for pure questions, explanations, or reviews where no code changes.
---

# Route work by what's still undecided

The goal is to spend strong-model attention only on decisions and cheap-model tokens on finding and typing. What sets the right model isn't how big the task sounds. It's **how much judgment remains** once the code is understood. A large feature with a precise plan is mechanical; a two-line fix with an unclear root cause isn't. What sets *where* work happens is context: what your session already holds, and whether a step's output will matter to a later decision.

Roles (a **delegate** is any subagent doing the work; the **plan** is what you hand it):
- **You (the main session):** judgment. Root causes, design decisions, plans, final verification.
- **`Explore` (Haiku):** finding and sizing. Given the sizing prompt in step 1, it reports change sites, blast radius, tests, risk flags, open questions and a size guess. It gathers evidence; it doesn't decide.
- **`implementer` (Sonnet, medium effort by default):** executing a plan. Pass `model: "haiku"` for short, checkable plans (see step 3).
- **`fable-planner` (Fable, read-only):** last-resort diagnosis and plan when you and the delegates are stuck. Rare and expensive; see step 4.
- **`lens-reviewer` (Sonnet):** checks a change through one narrow lens (spec, callers, security, …), reporting only high-confidence issues of that class. Used for risky changes, not routine ones.
- **`ui-verifier` (Sonnet):** drives the browser for every UI, visual or E2E check and returns a verdict with defects, never screenshots.

## Context is a routing input

Context costs twice. Every turn re-reads all of it, and recall and instruction-following get worse as it grows, most of all when it is unrelated to the current step. So before doing a step yourself, ask whether its raw output will matter to a later decision. If not, a subagent does it and you keep only its conclusion.

- **Browser and UI: always a subagent.** Every browser call, screenshot and visual or E2E check goes to `ui-verifier`, whatever the size of the change or the session. Give it the URL, the steps, the checks and the viewports.
- **Sizing goes to a scout.** Don't read code to estimate how big a change is: an `Explore` agent given the sizing prompt below reports the change sites and a size guess (step 1). Open only the files the decision turns on or that you will edit.
- **Single-use output goes to a subagent:** wide reads across a subsystem, log digging, searches whose hits you'll mostly discard.
- **Long sessions tip the balance.** Past about 100K, delegate even small steps whose output you won't reuse. Work that builds on what you already hold (a fix in code you just read) stays with you; re-teaching it to a delegate costs more.

Tell the user the route in one line when you pick it (for example "Scope is clear and small, so fixing directly" or "Triaging first; the request spans billing and export"), so they can redirect early.

## 1. Is triage needed?

Skip it when the change site is already in your context, or the user named the file or function and the change is obviously small. A scout pass on a known small fix only adds latency.

Triage when the request describes behavior rather than location ("exports sometimes drop rows"), spans areas you haven't seen, or you can't yet say which files change or how big the change is. The scout's size estimate, not your own reading, sets the route in step 3. For requests touching several independent areas, run one scout per area in parallel.

Prompt the scout with the request in the user's words plus anything you already know:

```
Goal: <the change>. Report facts only, no recommendation: change site
(path, symbol, line), blast radius (reference count and first 6 sites),
tests that cover it and the command to run them, risk flags (auth,
persistence or migrations, concurrency, money, public API, external I/O)
each with the path that raised it, up to 3 open questions, and a size guess
(small: 1 file under about 30 lines; medium: 2 to 5 files; large:
cross-module) with one line of evidence. Search with orch ctx; every path
and line must come from output you saw.
Search with orch ctx (text, sym, find, batch); read files with Read and
offset/limit (a file over about 30 KB needs a range). grep, rg over files,
cat, head, tail, sed -n and find chains are blocked by hooks and each block
wastes a turn.
```

## 2. Check the evidence, then judge

Treat the scout's report as a map: open the 1–3 change sites the decision turns on (usually ones you'll edit anyway) before deciding. That tells you whether the "small" guess is real without a wide read. Reading a report can't show what the scout missed, so when completeness matters (every caller, every config site), spot-check one area yourself or split it into narrower scouts whose coverage you can check.

Then answer:
- **Open decisions:** is the root cause known? Is the intended behavior unambiguous? Are there design choices left (data shape, algorithm, error semantics, API)?
- **Risk:** auth or security, persistence or migrations, concurrency, money, public APIs, external I/O.

Ask the user only when an open decision is truly theirs (product behavior, compatibility tradeoffs), as a question card (`question-card` skill). Settle technical ones yourself.

## 3. Route

| Situation | Route |
|---|---|
| Change is obvious once located, roughly ≤20–30 lines | **Do it yourself.** Writing a plan for a delegate would cost more than the change. |
| Open design decisions or unclear root cause | **Settle them yourself first** (investigate, decide, write the plan), then route the plan using the rows below. If deciding and doing can't be separated (exploratory debugging, delicate algorithms), implement it yourself. |
| Decisions settled, an exact plan for a short, single-concern change, and a test or check that can verify it | **`implementer` with `model: "haiku"`**, escalating per step 4 if it fails. Independent parts can go to parallel implementers if they touch different files. |
| Purely mechanical (renames, boilerplate, repetitive edits across many files) | **`implementer` with `model: "haiku"`.** |
| Decisions settled, but nothing mechanical can verify the result (UI, prose, config with no tests), or the work needs inference across several files or a long edit-test-fix run | **`implementer`** (Sonnet). |
| Settled but high-risk | **`implementer`** (Sonnet), then lens reviews (step 5). |

**Model rule:** `implementer` runs `sonnet` by default and `haiku` for mechanical or single-concern work a test decides. Use Opus only for ambiguous design work or when task.md names it. A subagent started without `model` inherits the parent's model, so an Opus lead must pass `model: "sonnet"` or `"haiku"` explicitly and never passes `opus` down by default.

Every spawn pays for a large cached prompt prefix, so prefer one implementer over several tiny ones, and don't move short, checkable work up a tier "to be safe". Keep Haiku delegates short: its price rises on long prompts and its context grows with every file it reads. Prices and evidence: `references/model-selection.md`.

## 4. Writing a plan worth delegating

Give the delegate the excerpt or the exact `path:line-range`, not "read the spec" or "read task.md". Never tell it to read a file that loads on its own (root and nested `CLAUDE.md`, skills it is given); point at the section it needs by `path:first-last`.

Every subagent prompt, including for built-in types (Explore, general-purpose), carries this sentence verbatim:

> Search with orch ctx (text, sym, find, batch); read files with Read and offset/limit (a file over about 30 KB needs a range). grep, rg over files, cat, head, tail, sed -n and find chains are blocked by hooks and each block wastes a turn.

The implementer's quality ceiling is the plan's specificity. It's told to stop rather than guess, so gaps come back as "blocked" instead of silent mistakes. Cover the gaps up front:

```
Goal: <one sentence, the user-visible outcome>
Change: `path/file.py` `func`: <exactly what changes>; `path/other.py`: <…>
Behavior: <inputs → outputs, including the edge cases you decided: empty, negative, missing, concurrent…>
Don't change: <adjacent code or behavior that must stay as is>
Unverified: <claims from notes, tickets or docs you haven't checked against the code; omit if none>
Tests: add <what> in `tests/…`; run `<command>`
```

If you can't fill in **Change** and **Behavior** concretely, the decisions aren't settled yet. Go back to step 3's second row.

When an implementer reports **blocked**, make the call it surfaced and either re-delegate with the answer or finish it yourself if that's quicker.

### When a delegate fails
You hold the verifier, so you make the escalation call, never the delegate. A failure means the tests you ran fail, not that the implementer says it's done.
1. **First failure:** re-delegate once to the **same model** with the failing output and what you learned (an informed retry). Often that's enough.
2. **Second failure on Haiku:** move up to `implementer` on Sonnet, with the failure history in the plan.
3. **Second failure on Sonnet, or the failures show a design gap:** stop delegating and take over the problem yourself, going back to step 3's second row.
4. **Still stuck (last resort):** call `fable-planner` only if you investigated yourself and the root cause or design is still unclear, a fix has failed with evidence, and the task is worth the cost. Skip it if this session already runs on Fable. Brief it in its input format, check its diagnosis against the code, then hand its plan to a Sonnet `implementer`. If it reports `Needs user`, ask the user; if low confidence, run the experiments it names first. One planner call per problem; a second means the problem needs the user.

## 5. Verify

- Implementers run tests and add a regression test. Check their report shows real test output, not just "done", then **rerun the tests yourself**. Your run is the verifier that decides whether to escalate. Use the runner's quiet flag or a summary-only check command so only the summary and failures enter your context.
- UI changes: start `ui-verifier` only after the automated check is green, once over the whole change with a written checklist from the plan's Behavior line. Tell it to stop at about 60 turns. A second pass re-checks only the defects that were fixed, not a full re-walk.
- Batch your own work: independent edits and tool calls go in one turn as parallel calls, and run the repo check once per batch of edits (narrow tests while iterating, the full staged check once per wave).
- **Read the final diff yourself.** It's cheap and catches drift from the plan, scope creep, and edits to tests that make them pass rather than check the behavior.
- For routine changes, stop there. The implementer's tests plus your diff read already cover them, and a reviewer mostly adds cost.

### Lens reviews (risky or delegated-and-large changes)
For auth or security, persistence, concurrency, money, shared-code signature changes, or large delegated diffs, run two or three narrow `lens-reviewer` agents in parallel, one lens each, and verify each finding against the code yourself. Lens table, prompts and handling: `references/lens-reviews.md`. Skip this for routine changes.

## Pitfalls
- Driving the browser or taking screenshots in the main session.
- Reading code to size a change that a scout could size.
- Delegating a change smaller than its plan.
- Triaging a location you already know.
- Letting the scout's size guess make the decision without opening the code.
- Handing a delegate a task with open design questions and a vague plan. The cheap model will make those decisions quietly and confidently.
- Spawning many scouts for one small area. One scout per independent area is enough.
- Taking an implementer's "done" as the verdict instead of rerunning the tests.
- Escalating to a bigger model without feeding it the failing output.
- Fanning out many small spawns when one implementer would do.
- Calling `fable-planner` before investigating yourself, or for a problem that is merely tedious. It is for stuck, not for hard-but-clear.
