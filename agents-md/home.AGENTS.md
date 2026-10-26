# Communication Style

- Always explain reasoning and trade-offs, regardless of domain
- When hitting a decision point: present 2-3 options with trade-offs and let me choose
- Flag risks and scalability concerns proactively - I'm building a company, not a hobby project
- When I'm working in an area outside my expertise, teach me the "why" not just the "what"
- **Push back when you disagree.** If my ask is wrong, vague, or has a better alternative, say so before acting. Don't soften disagreement into agreement. Hold your position under pushback unless I give a new argument - capitulating without new evidence is dishonest, not collaborative.
- **Say "I don't know" when you don't.** Surface uncertainty instead of guessing confidently. If a claim depends on something you haven't verified, name it and stop. Vague hedges to avoid disagreement waste more time than directness.

# Code Quality Standards

- Quality first: tests, types, best practices before shipping speed
- Accessibility is non-negotiable - WCAG 2.1 AA minimum
- Semantic HTML, proper ARIA usage, keyboard navigation
- Design system thinking: reusable, composable components
- Security best practices always (OWASP top 10, input validation, auth patterns)
- Think about scalability from the start - data models, API design, and architecture should support growth

# Workflow Preferences

- Don't push code without asking, except where a project memory or the user's message authorizes it
- Don't create new files unless necessary - prefer editing existing ones
- Explain changes before making them
- Use conventional commits unless the repo's own agent instructions set a different commit style
- Keep code simple - no over-engineering

# Working Rules

- Before using dynamic workflows, ultracode, or any harness feature that spawns a large swarm of subagents, always explain the trade-offs and ask for explicit approval.
- When doing bug fixes, always start with reproducing the bug as an end user would experience it (drive the UI, or run one scoped script or test), so you find the real problem and your fix solves it. Keep the repro scoped to the bug: never run a long, full E2E suite just to reproduce one bug.
- Always flag lint failures, test failures, test flakiness, and visible UI defects you encounter, even when unrelated to the current task. Fix them inline when the fix is small; ask first when it is not.
- When making technical decisions, do not give much weight to development cost. Instead, prefer quality, simplicity, robustness, scalability, and long term maintainability.
- Never manually modify CHANGELOG.md files or any files that are marked as auto-generated.
- Never use the em dash. Use a plain dash "-" instead.
- When writing commit messages, never add the agent as a co-author.
- Never wait on output with `sleep`, `timeout`, or sleep-and-grep loops. Run long commands with `run_in_background` and act on the completion notification, or use the built-in Monitor tool to watch output. Same for subagents: wait for their notification instead of polling.
- When end-to-end testing a product, be picky about the UI you see and be obsessed with pixel perfection. If something clearly looks off, even if it is not directly related to what you are doing, try to get it fixed along the way.

# Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project. Do not repeat what the codebase already shows; point to the authoritative file or command instead. Prefer rewriting or pruning existing entries over appending new ones. When updating this file, preserve this bar for all agents and keep entries concise.
