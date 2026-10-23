# Lens reviews

Load this only when a change is risky or a large delegated diff.

A general "review this" pass mostly produces nitpicks. Narrow reviewers that each hunt for one class of defect find the real problems, so pick lenses from what triage and your own reading turned up and run them as parallel `lens-reviewer` agents (Sonnet), one lens each:

| Trigger | Lenses |
|---|---|
| Delegated work touching several files | `spec` |
| Changed signature, return value, exception, or semantics of shared code | `callers` |
| Bug fix in code with meaningful history | `history` |
| Code with docstrings, invariants, or a CLAUDE.md with rules | `contracts` |
| New or changed tests are the main evidence it works | `tests` |
| Risk flags from triage: auth/input, persistence, concurrency, money | `security`, `data`, `concurrency`, `money` (whichever match) |
| New error handling, retries, fallbacks, or catch blocks | `errors` |
| Large diff that adds helpers, abstractions, or parallel code paths | `simplify` |
| None of the above, but you still want a second look | `bugs` |

Two or three lenses are typical, and more than five usually means the change should have been split. Pass each one the lens, the requirement or plan, the diff command, and the scope report if there is one.

Reviewers report only findings that meet their lens threshold (separate evidence, impact and likelihood ratings, defined in the `lens-reviewer` agent), each with a failure scenario and evidence. Verify each finding against the code yourself before acting. You're already in the diff, so this is cheap, and it catches the false positives that slip past self-rating. Send real defects back to the `implementer` with the finding as the plan, or fix them directly if small.

