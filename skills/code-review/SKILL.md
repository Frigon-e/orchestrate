---
name: code-review
description: Review committed-range or work-in-progress changes along two axes - Standards (does the code follow this repo's documented coding standards?) and Spec (does the code match what the originating issue/spec asked for?). Runs both reviews in parallel sub-agents, then compiles findings with source tags. Use when the user wants to review a branch, a PR, work-in-progress changes, or asks to "review since X".
---

Two-axis review of the subject selected below:

- **Standards** - does the code conform to this repo's documented coding standards?
- **Spec** - does the code faithfully implement the originating issue / spec?

Both axes run as **parallel sub-agents** without seeing each other's findings. Compile afterwards. Independent passes over the same evidence are not independent factual verification.

## Process

### 1. Pin the review subject

Choose the mode that matches the request. Ask if ambiguous; do not silently omit local work.

- **Committed-range:** resolve the supplied commit, branch or tag with `git rev-parse --verify '<fixed-point>^{commit}'`. Pin its merge-base with `HEAD`, then capture `git diff --binary <merge-base-sha> <head-sha>` and `git log <fixed-point>..HEAD --oneline`. This is the three-dot comparison, excluding local changes. Ask for the fixed point if missing.
- **Work-in-progress (WIP):** compare the final checkout state with `HEAD`, including staged, unstaged and non-ignored untracked files. From the repo root, capture with a temporary index so the user's index is untouched:

  ```sh
  subject=$(mktemp -d) &&
  head=$(git rev-parse --verify HEAD) &&
  GIT_INDEX_FILE="$subject/index" git read-tree "$head" &&
  GIT_INDEX_FILE="$subject/index" git add -A -- . &&
  GIT_INDEX_FILE="$subject/index" git diff --cached --binary "$head" > "$subject/review.diff"
  ```

  Stop on any failed command. This reviews the net checkout, not intermediate index states: a staged edit undone by an unstaged edit cancels out. If the request includes those intermediate states, also capture `git diff --cached --binary` and `git diff --binary` as labelled evidence. Ignored files and changes inside submodules need explicit scope and separate capture. WIP alone excludes commits already in `HEAD`; if both modes are requested, label both subjects.

Save the patch once and give both passes that same snapshot, pinned SHAs, mode and commit list (none for WIP). Do not regenerate it between passes. Stop if empty. If the checkout changes during review, recapture and restart rather than mix versions. Remove temporary files after reporting.

### 2. Identify the spec source

Look for the originating spec, in this order:

1. Issue references in the commit messages (`#123`, `Closes #45`, GitLab `!67`, etc.) - fetch with the repo's tracker CLI (for example `gh issue view`).
2. A path the user passed as an argument.
3. A spec file under `docs/`, `specs/`, or `.scratch/` matching the branch name or feature.
4. If nothing is found, ask the user where the spec is. If they say there isn't one, the **Spec** sub-agent will skip and report "no spec available".

### 3. Identify the standards sources

Anything in the repo that documents how code should be written, such as `CODING_STANDARDS.md` or `CONTRIBUTING.md`.

On top of whatever the repo documents, the Standards axis always carries the **smell baseline** below - a fixed set of Fowler code smells (_Refactoring_, ch.3) that applies even when a repo documents nothing. Two rules bind it:

- **The repo overrides.** A documented repo standard always wins; where it endorses something the baseline would flag, suppress the smell.
- **Always a judgement call.** Each smell is a labelled heuristic ("possible Feature Envy"), never a hard violation - and, like any standard here, skip anything tooling already enforces.

Each smell reads *what it is* → *how to fix*; match it against the diff:

- **Mysterious Name** - a function, variable, or type whose name doesn't reveal what it does or holds. → rename it; if no honest name comes, the design's murky.
- **Duplicated Code** - the same logic shape appears in more than one hunk or file in the change. → extract the shared shape, call it from both.
- **Feature Envy** - a method that reaches into another object's data more than its own. → move the method onto the data it envies.
- **Data Clumps** - the same few fields or params keep travelling together (a type wanting to be born). → bundle them into one type, pass that.
- **Primitive Obsession** - a primitive or string standing in for a domain concept that deserves its own type. → give the concept its own small type.
- **Repeated Switches** - the same `switch`/`if`-cascade on the same type recurs across the change. → replace with polymorphism, or one map both sites share.
- **Shotgun Surgery** - one logical change forces scattered edits across many files in the diff. → gather what changes together into one module.
- **Divergent Change** - one file or module is edited for several unrelated reasons. → split so each module changes for one reason.
- **Speculative Generality** - abstraction, parameters, or hooks added for needs the spec doesn't have. → delete it; inline back until a real need shows.
- **Message Chains** - long `a.b().c().d()` navigation the caller shouldn't depend on. → hide the walk behind one method on the first object.
- **Middle Man** - a class or function that mostly just delegates onward. → cut it, call the real target direct.
- **Refused Bequest** - a subclass or implementer that ignores or overrides most of what it inherits. → drop the inheritance, use composition.

### 4. Spawn both sub-agents in parallel

Give each pass source IDs (`Standards:1`, `Spec:1`, etc.) in its own discovery order. Each finding must quote code with `path:line`, give a concrete failure scenario, and score three separate fields with reasons:

- **Evidence strength:** strong (quoted code and traced scenario support the claim), partial (code supports it but a named link is unverified), weak (speculation).
- **Impact:** high (security, data loss or core workflow failure), medium (limited incorrect behavior or material maintenance harm), low (minor harm).
- **Likelihood:** high (routine inputs), medium (plausible edge case), low (exceptional conditions), unknown (not established).

Include these definitions and source-ID instructions in both prompts. Do not combine the fields into a confidence number or treat agreement as stronger evidence. Keep uncertainty explicit.

**Standards sub-agent prompt** - include:

- The saved subject patch, mode, pinned SHAs and commit list.
- The list of standards-source files you found in step 3, **plus the smell baseline from step 3** pasted in full - the sub-agent has no other access to it.
- The brief: "Report per file/hunk: (a) every documented-standard violation, citing the rule and source; (b) baseline smells, naming each and quoting the hunk. Distinguish hard violations from judgement calls. Baseline smells are always judgement calls; repo standards override them. Skip tooling-enforced rules. Include the source IDs, scenario and three ratings above. Be concise without omitting findings."

**Spec sub-agent prompt** - include:

- The same saved subject patch, mode, pinned SHAs and commit list.
- The path or fetched contents of the spec.
- The brief: "Report: (a) requirements the spec asked for that are missing or partial; (b) behaviour in the diff that wasn't asked for (scope creep); (c) requirements that look implemented but where the implementation looks wrong. Quote the spec line for each finding. Include the source IDs, scenario and three ratings above. Be concise without omitting findings."

If the spec is missing, skip the Spec sub-agent and note this in the final report.

### 5. Compile

Start with `Subject: <mode, pinned SHAs, captured scope and exclusions>`. Keep `## Standards` and `## Spec` separate:

- Merge duplicates within the same axis only when defect, cause and failure scenario match. Keep all source IDs and distinct evidence; place the merged entry at its first occurrence.
- Link duplicates across axes by source ID, but do not merge their axis-specific claims or rerank either pass's order.
- Preserve dissent, contradictions and differing scores with their source IDs. Mark unresolved claims explicitly; do not silently choose a winner or average scores.
- Never drop a finding only because another pass missed it. A missing finding is not a rebuttal.

Use this compact format per entry:

```text
[Standards:1, Standards:3] path:line - defect
Code: <quote>; Failure: <input or sequence -> wrong result>
Evidence strength: strong - <reason>; Impact: medium - <reason>; Likelihood: low - <reason>
Links: Spec:2 (same defect); Dissent: <source ID and claim, or none>
```

End with original and deduplicated counts per axis, cross-axis links, unresolved disagreements and skipped passes. Do not total linked findings as separate unique defects.

## Why two axes

A change can pass one axis and fail the other:

- Code that follows every standard but implements the wrong thing → **Standards pass, Spec fail.**
- Code that does exactly what the issue asked but breaks the project's conventions → **Spec pass, Standards fail.**

Reporting them separately stops one axis from masking the other.
