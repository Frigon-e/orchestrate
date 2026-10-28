---
name: orchestrate
description: Runs this session as an orchestrator that routes work to lead agents, each a Claude Code or Pi session in its own Herdr pane, briefed by a task.md (intent, scope, pointers, spec, acceptance, authority), and relays everything between the leads and the user. Use when the user asks to orchestrate, coordinate, fan out or run several tasks in parallel, hand work to leads or panes, or when a request is too big for one context (several features, research then build, a plan then its implementation). Needs Herdr (HERDR_ENV=1) and Go.
---

# Orchestrate

You route; leads work. Your context is the scarce resource: it holds the
user's intent, the board and the decisions, never code, diffs or plans. So you
never edit project files, implement, write a plan or merge. Each lead is a
full session in its own Herdr pane with its own skills and subagents (leads
use `route-work`). Every message between the user and a lead goes through you.

`orch` below means `~/.agents/skills/orchestrate/scripts/orch` (a wrapper
that builds the Go tool on first use). Always call it by that full path.

## Start

1. `orch become` marks this pane's session as the orchestrator. Hooks then
   refuse repo edits, non-scout subagents and git or PR mutations, and print
   the board after a restart or compaction.
2. `orch status` shows the leads, their last reports and any open decisions.
3. When at least one lead is open, keep a watcher running. On Claude Code,
   run `orch watch` with the Monitor tool; each line it prints wakes you.
   Monitor stops at its timeout (30 minutes at most), so start it again
   when it ends while leads are still open; the watcher keeps its place on
   disk, so no event is lost or repeated. The watcher exits when none of your leads
   are open, so start it again after the next spawn.
4. Several orchestrators can work in one repo, one per Herdr pane. Each
   lead belongs to the pane that spawned it; your board and your watcher
   cover only your leads, and the board names the others' open leads in one
   line. `max_leads` and locks are shared. Leave another orchestrator's
   leads alone unless the user asks; `orch adopt <id>` takes one over (for
   example when its orchestrator ended), keeping the watcher's place.

## Intake: what is a task

For each user message, decide which of these it is before acting:

- **A question you can answer** from the board, a lead's report or one or
  two file reads: answer it. A wider search goes to an `Explore` agent
  (`Explore` and `claude-code-guide` are the only in-process subagents you
  run), never your own reads.
- **A decision a lead is waiting on**: send it (see Loop).
- **A task**: work that changes a repo or needs sustained investigation.
  Give each one a lead. Choose:
  - **Kind.** `ship` produces commits on a branch. `scout` produces
    `report.md`: research, diagnosis, a design or a plan.
  - **Planning goes to a lead.** When design questions are still open, a
    scout lead on the strongest model writes the plan. Ship leads then
    implement it (`--depends <planner>`). Don't plan in your own context.
  - **Split by deliverable.** Give one lead to each independent deliverable
    with disjoint files. A change and its tests stay together. Don't split
    small work: each lead is a whole session.
  - **Harness and model.** Check the user's rules first: `orch dispatch`
    prints them (first good fit wins; a list offers choices). Without a
    fitting rule, match the model to the judgment the task still needs: the
    strongest for planning and unclear root causes, a mid tier for a settled
    plan (its subagents do the typing), `default` to inherit the harness
    config. Effort: low for explicit, well-understood work, high or above
    for ambiguous investigation or design, never max unless the user asks.
    Claude and Pi leads are interchangeable; families mix by lead, not
    inside one. A Pi lead names its model as `provider/id` (for example
    `openrouter/deepseek/deepseek-chat-v3.1`; `pi --list-models` lists them)
    and its effort becomes `--thinking`. Two gaps: orch cannot read a Pi
    lead's context size yet (the CTX column stays `-`), and the lead Stop
    hook does not guard Pi, so check a Pi lead's `done` evidence closely.
  - **Ask the user first** when the scope or a product decision is theirs.
    Settle technical questions in the brief.

## Brief

```
orch new <id> --kind ship|scout --harness claude|pi --model <m> --effort <e> [--depends a,b] [--base <branch>]
```

This scaffolds `task.md` in the run directory; the command prints its path.
Fill every `{{...}}` with the Edit tool; the hooks allow that directory.
Then run `orch check <id>`, which refuses placeholders, empty sections, a
Scope without In/Out, an Authority without its three lines, and
dependencies that are not done. Then `orch spawn <id>`. Exit 75 means the
lead cap is reached: queue the task and spawn it when a lead closes.

When a spec with work packages exists (`references/spec-format.md`), run
`orch gate <spec>` first, then `orch plan <spec>` instead of one `orch new`
per package. The gate checks pointers and acceptance commands, then runs
read-only model passes and keeps only claims whose `path:line` evidence
exists; `orch gate <spec> --dry-run` lists the passes without calling a model.
`orch plan` refuses a spec whose current text has not passed the gate; never
use `--skip-gate` without the user's agreement. Read `gate/<hash>/report.md`
(short), judge the findings yourself, add a Claude adversarial pass on the
Design rules when the spec is risky, and show the user only what survives. A
model verdict is input, not truth. Decisive open questions arrive as cards:
`orch ask --cards <gate dir>/questions`. Keep product choices with the owner
and settle delegated technical ones with the lead. `orch plan` then makes the
tasks and prints the waves: check and spawn the first wave, then each later
wave when its dependencies are done.

Gate passes, question routing, the ctx shell and checkout check, options and
the reuse checklist: `references/gate.md`. Model roles and connector health:
`references/model-roles.md`; outages: `references/outage.md`. Benchmarks:
`references/bench.md` (no imports or paid experiments without approval). The
`#### Builds` and `search:` format: `references/spec-format.md`.

**Finding code, in this order** (`references/context.md`):
1. Location unknown: `jbcontext search "<one behavior>"`. One concept per
   query, several queries in one message. Results are candidates, not proof of
   absence; a fresh worktree branch searches the default branch's snapshot, so
   read before citing a line.
2. Known symbol: `orch lsp def|refs|callers|impls <Name>`, `orch lsp symbols
   <file>` and `orch lsp hover <path:line:col>` (exit 1 not found, 2 no language
   server; `orch lsp --help`).
3. Exact text: `rg`. 4. File names: `fd`. 5. Inspect source with native Read,
   offset and limit.

Send independent lookups as parallel calls in one message. `grep` (also
`egrep`, `fgrep`, `ag`, `git grep`), a bare `cat FILE` and `sed -n` over one
file stay blocked; plain `rg` is allowed, and plain `find` only when `fd` is
not installed. Gate agent passes get a read-only lookup shell (`--lookup-tool
on|off`, `--lookup-shell restricted|open`; see `references/gate.md`). A missing
language server is not zero callers (`orch lsp` exits 2). Repo-local servers need `ctx_trust = ["/absolute/repo"]` in user config;
`ctx_deny` wins. `orch doctor` reports retrieval readiness (index, watch,
tools).

What makes a brief work (template: `references/task-template.md`):
- **Intent** in the user's words. The lead falls back on it when the spec
  runs out.
- **Pointers** are `path#Symbol` (methods `path#Type.Method`) plus why it
  matters; use `path:line[-line]` for blocks, SQL, config, generated code or docs.
  They are gathered by a scout if
  needed. They point; they never paste.
- **Spec** carries every decision already made, so the lead doesn't remake
  it.
- **Acceptance** is runnable: commands and expected results, or for a
  scout, the evidence each finding needs.
- **Authority** says what the lead settles alone, what comes back to you,
  and the hard limits.
- Keep it under 15 KB (`orch check` warns above 15000 characters and names
  the largest sections). The brief is the contract: to change course,
  edit task.md, then `orch send` the lead a line saying what changed.

## Loop

Each watcher line is `orch: <id> <event>: <text>`. Act on it, then end
your turn:

| Event | Do |
|---|---|
| `done` | Check the evidence before believing it. Ship: `git log -1 <sha>` on the named branch, then `orch verify <id>`; the lead's own check claim is not evidence. When the spec has dependencies, verify requires a clean, committed candidate, pins it before checks and merges that exact commit into the spec's integration branch only if HEAD and non-ignored files stay unchanged; the merge result is checked too. `--no-merge` only validates the working tree and reports `verify: pass (not integrated)`; `integration: conflict` or `integration: check failed` is a needs-decision for the user (the tool never resolves it), and dependents cannot spawn until the merge lands. Scout: read `report.md`, or have a scout summarise a long one. Tell the user in a line or two. Merging into the base branch (the integration branch included) is a separate instruction given once the user approves, using the repo's own merge flow. Then `orch close <id>`. |
| `needs-decision [key]` | If the user's earlier answers settle it, `orch send <id> --resolve <key> "<answer>"`. Otherwise ask the user, batching open decisions, then send the answer. |
| `needs-decision [context]` | The lead's context is past the limit and the next work is unrelated; it wrote `handoff.md` and stopped. Decide it yourself, without the user: `orch compact <id>` when the next work shares some of its context, `orch respawn <id>` (fresh session on the same task and worktree, started from the handoff) when it shares none. Both resolve the decision. |
| `escalate [key]` | The lead is out of its depth. Follow the `escalate` rule under "Talking to the user". Never ask the lead to try again on the same model first. |
| `blocked` | Same as needs-decision; `orch peek <id>` if the line is unclear. |
| `failed` | Read why. Fix the brief and send, or close and respawn on a stronger model with the failure in the brief. Two failures on the same brief mean the brief is wrong. |
| `ui-blocked` | The pane waits at a prompt, often a permission. `orch peek <id>`. Tell the user and `orch focus <id>`; never answer permission prompts for them. |
| `stalled` / `looping` | A lead is busy but not getting anywhere: nothing reported and nothing changed in its worktree for `stall_minutes`, or its transcript repeats one tool call while the worktree stands still. `orch peek <id>` first. A long test run is the usual false alarm. Otherwise `orch send` a nudge naming the loop, and if the lead is a pi lead on a hard problem, tell it to `escalate`. The third alert on one task freezes it. |
| `quiet` | Idle with no report: `orch peek <id>`, then `orch send` a nudge. |
| `reminder` | A message sat unread while the lead was idle; the watcher rang the doorbell once more. Usually none; if it repeats, `orch peek <id>`. |
| `gone` | The agent or pane went away. Check `orch status`; respawn after `orch close <id> --force` only with the user's agreement if work may be lost. |

`orch send` delivers a message file plus a doorbell line in the lead's pane.
A lead acknowledges by reading its inbox. Messages arrive through `orch send`
(plus the watcher's single re-ring of an unread one); nothing else types into
a lead's pane.

## Talking to the user

- Report each lead in one line, with an outcome and its evidence. Never
  paste a lead's output.
- Ask questions in one batch, and say which lead is waiting on each.
  Show each one as a question card (`question-card` skill); a lead's card
  is in its task directory at `questions/<key>.md`.
- With two or more open questions, or any card with options or visuals, run
  `orch ask`, open the page with `lavish-axi <page>`, and wait with
  `lavish-axi poll <page>` under Monitor. Each answer arrives with its `orch send ... --resolve` line; send
  it, then rerun `orch ask` so answered questions leave the page. A single
  yes-or-no question stays in chat. Never run `lavish-axi share`.
- A ship lead's `done` is a claim, not a result. Run `orch verify <id>`
  before you accept it: it runs the repo check command in the lead's
  worktree and lists files changed outside the task's `owns`. Integration
  requires no uncommitted or untracked non-ignored files. Verify pins HEAD
  before scope and acceptance checks, rejects changed HEAD or non-ignored
  files after checks, then merges only that SHA. It checks the merge result
  too; conflicts are aborted, failed or mutating checks roll back the merge
  and clean their non-ignored outputs. Ignored build outputs are allowed.
  The integrated marker records both candidate and merge SHAs. An already
  merged candidate records that no post-merge check ran. `--no-merge` keeps
  preliminary working-tree validation, with `verify: pass (not integrated)`;
  tasks without an integration branch use the same wording. Neither is
  integration approval. On `fail` or `unverified`, send the output back with
  `orch send` and do not report the task as done to the user. Set `check` in
  the repo config, or `verify` cannot pass.
- `escalate` means a lead hit something beyond its model. Read its card
  (`questions/<key>.md`: Context, Tried, Blocked). A question about this
  code goes to one fresh Sonnet agent with the card and the paths it names;
  tell the lead the answer as concrete steps with `orch send --resolve`, not
  a discussion. A design-level problem goes to an Opus planner, and you
  update the plan before the lead continues. A second `escalate` on the same
  task means the task was put on too weak a model: respawn it on a stronger
  one from its branch and note why.
- **Triage.** At `spawn`, orch asks a decision model (the Decider role in
  `references/model-roles.md`) five typed
  questions about the task text: clarity, difficulty, verifiable,
  cross-cutting, cause known. Fixed thresholds turn the answers into a tier
  in `task.md` (`tier:`): `settled` (clear, easy, a check that can fail: a
  cheap model may take it), `standard`, or `planning` (open-ended, hard or
  unknown cause: Opus). Low confidence moves the tier up one step. It never
  sees code, only the brief. In `shadow` mode (the default) it only records
  to `triage/triage.jsonl` and warns when a weak model runs a task rated
  above settled. `orch triage <id>` rates a task by hand; `orch triage
  report` joins the ratings with outcomes (escalated, failed, stalled,
  verify failed, respawned, frozen) and says when there are enough (20) to
  trust the rates. Treat the tier as a second opinion on your own routing,
  not a verdict. `enforce` mode (user config `triage_mode`) refuses a
  `weak_models` entry on anything but settled work; `spawn --override-tier`
  gets past it.
- **Allowed models.** `[allowed_models]` in the user config is the whole
  list of conversational harness models orch will start; `orch models` prints it. The API Decider does not use this list. `new`, `plan`,
  `spawn`, `respawn` and the gate model refuse anything else, and a dispatch
  profile outside the list is a config error. A repo file cannot set it.
  The list is partly dynamic: `[[allowed_when_limited]]` adds models only
  while a harness is limited (Haiku is allowed as a cheap lead only while
  pi is down). An optional `lane = "pi:openrouter"` scopes that condition to
  one connector. `orch models` shows which are allowed right now.
  Never widen the list to get past an error: ask the user.
- **`spawn --auto`.** Make a task with `--harness auto --model auto --effort
  auto`, write the brief, then `orch spawn <id> --auto`. orch rates the
  brief (triage), takes the dispatch rule whose `tier` matches, and picks
  its first profile that is allowed, healthy and not degraded; effort
  follows the tier (low for settled, high otherwise). Without `--apply` it
  is a dry run that prints the pick; `--apply` sets the profile and spawns;
  `--degraded` allows a stand-in the user agreed to. It parks when nothing is
  usable. If the Decider is off, it prints guidance and starts nothing; use
  `--tier settled|standard|planning` to select the tier without triage.
  A task with a hand-chosen profile is never changed. Mark a task
  `private: true` in its front matter to keep it on Claude; its text is never sent to the triage model. Without an explicit `--tier`, auto cannot rate private work; choose its profile by hand. Use the dry run
  until `orch triage report` has 20 outcomes and looks sound; the tiers come
  from untuned thresholds.
- **Provider limits.** When a connector limit stops work,
  run `orch health set <claude|pi:provider|pi:openrouter:anthropic> limited --for <duration> --reason "..."`.
  `pi` remains the parent of all pi connectors; `pi-anthropic` aliases direct
  `pi:anthropic`. The user's say-so is the signal; a `ui-blocked` line that
  shows a limit is a hint. The watcher also records a Claude subscription
  rejection that carries a reset time (marked `(auto)`, expires at the reset);
  a manual entry always wins. `orch new`, `orch plan` and `spawn` refuse that lane,
  and `orch status` and `orch dispatch` say what to use now. Park by
  default: a dispatch profile marked `degraded = true` is a weaker
  stand-in, and you use it (`--degraded`) only after the user agrees. Never
  widen a fallback lead's authority. A lead already running when its
  provider dies: `orch respawn <id> --harness <other> --model <m>`; it
  recovers from the status log and the branch, so tell the user and expect
  rework. `orch health set <h> ok` clears a limit.
- **Quick jobs.** For a small, clear change (a few lines in 1 to 3 files),
  skip the spec, gate and plan: `orch quick <id> --owns <file>[,<file>]
  "<request>"`. It makes the task, has triage rate the request, and spawns a
  lead on the first usable settled profile (Flash when pi is healthy). It
  refuses, and leaves nothing behind, when the repo has no `check` command,
  `--owns` is not 1 to 3 plain files, the request is under 20 or over 800
  characters, spawning is held, or no settled profile is usable. The triage
  verdict (`quick` or `not-quick`, logged for `orch triage report`) follows
  `quick_mode` in the user config: `shadow` (the default) warns and runs
  anyway, `enforce` refuses, `off` disables the command. A quick lead has one
  attempt: it escalates instead of iterating, the watcher runs `orch verify`
  when it reports done, and a failed verification or a stall freezes the
  task. Then take it over or write a spec; do not send Flash round again.
  Use the full path for anything with a design choice, a signature change or
  more than 3 files.
- **Question sweeps.** To settle a pile of open questions, run answer passes
  on different models and let `orch sweep <topic>` compare them: only
  agreement backed by evidence that exists settles a question, and money,
  tax and legal areas (`human_only`) go to the user. See
  `references/sweep.md`.
- **Audit.** `orch audit [<id>] [--system S] [--since 6h] [--problems]`
  prints, oldest first, when each system went right or wrong: gate runs,
  triage verdicts, quick jobs, verify results, stalls, escalations,
  respawns, freezes, task outcomes, provider limits and holds, then a count
  per system. It only reads: status logs, the triage and gate records, and
  an `events.jsonl` that `orch health` and `orch hold` write. A limit that
  expires on its own (`--for`) leaves no event; the earlier `limited` line
  shows it started.
- **Circuit breakers.** orch freezes a task that stops converging: a
  third `escalate`, a third respawn, or the same report three times in a
  row. A frozen task shows as `FROZEN:` on the board and as a `frozen:`
  watcher event. Stop automatic recovery on it (`respawn` refuses), tell
  the user what happened in a few lines, and wait; `orch unfreeze <id>`
  clears it after they decide. Two frozen tasks hold all new spawns.
  `orch hold on <reason>` is the manual safe stop: running leads finish,
  nothing new starts. When state looks wrong or confusing, run
  `orch doctor` first: it lists each inconsistency with its fix and changes
  nothing. It also checks the installed links, hooks and Claude trust, and
  warns when Claude is not signed in. Prefer stopping to improvising when a
  state is one you do not recognise.
- Leads queue non-blocking questions with the `question` state; they do
  not wake you. `orch status` shows them under "open decisions" with the
  count of blocking and queued. Do not interrupt the user for a queued
  question. Keep the work going. Show the queue (`orch ask`) when a
  `needs-decision` or `blocked` report arrives, when the user asks for
  status or sends a new message after a gap, or when the user is active in
  the conversation and the queue is not empty. If a page you showed is still
  unanswered after 1-2 of your turns, treat the user as away, keep
  building, and say "N questions are waiting" at the start of your first
  reply after they return.
- Before closing a lead whose work isn't done, before force-closing, and
  before anything that pushes or merges, ask.

## Limits and housekeeping

- The lead cap (`max_leads`, default 3) is there for memory: each lead is
  a full agent plus its subagents. Raise it only on a machine that has the
  room.
- Close each lead's pane once its work is merged or dropped (`orch close`).
  Inside Herdr (`HERDR_ENV=1`), orch owns each ship lead's worktree: `orch
  spawn` makes it with `herdr worktree create` (branch `orch/<id>`, cut from
  the task's base sha, at the path below) in a workspace of its own, and
  the lead starts inside it and never runs `git worktree add`. Scouts have
  no worktree and share the orchestrator's 2x2 grid. orch links the
  checkout's `.env` into the new worktree (a symlink, only `.env`) unless
  the checkout is on `main` or `master`, has no `.env`, doesn't gitignore
  it, or the worktree already has one; a skip is logged as a note and
  printed. A repo file never runs a setup command. `orch close` then
  removes the worktree and `git branch -d`s the branch only when the tree
  is clean and the branch is merged into the base branch; otherwise it
  keeps both, says why and what to run, and never forces (`--force` skips
  the done check, not these guards). `orch respawn` reuses the worktree
  and branch. Outside Herdr the brief still tells the lead to run `git
  worktree add` itself, and `orch close` removes nothing.
- Per-repo settings, notes and locks live in
  `<repo>/.agents/orchestrate.toml`; the board prints the notes and held
  locks, and says when a repo has no file. Each brief tells the lead which
  locks to hold and when; a lead waiting on one reports `paused`. A lock
  frees when its lead closes or its pane dies; `orch lock release <name>
  --force` only with the user's agreement.
  `references/repo-config.md` owns its keys, what a note may hold, when
  to change it (rarely, with evidence and the user's approval) and setup.
  Settings a repo must not control live in
  `~/.config/orchestrate/config.toml`: `max_leads`, `lead_context_limit`, `unread_minutes` (default 10, 0 turns off the watcher's re-ring of an unread message),
  `claude_args` and `pi_args` (extra CLI flags for leads), `triage_mode` (shadow, enforce or off), `quick_mode` (same values; for `orch quick`), `triage_model`, `weak_models`, `ctx_index` (`full`, the default, builds a missing jbcontext index and refreshes a stale one; `incremental` only refreshes; `off` never calls jbcontext), `ctx_deny` (repos that never use jbcontext) and `dispatch`
  rules (a profile may set `degraded = true` for a weaker fallback). orch refuses them in a repo file, so a commit can't change how
  leads launch.
- Run state lives next to the repo's Herdr worktrees:
  `<herdr worktrees dir>/<repo>/.orchestrate/<id>/`. It holds task.md,
  lead.json (with the owning orchestrator pane), status.log, inbox/ and
  report.md; each orchestrator's watcher keeps `watch-<pane>.json` beside
  the task folders. Ship worktrees (made by orch inside Herdr) sit beside
  it at `<herdr worktrees dir>/<repo>/<id>`. Disk is the truth; the board
  is a view of it.
- The board's CTX column is each lead's context size, marked `!` past
  `lead_context_limit` (user config, default 150000). Before you send a
  lead new work that is unrelated to its task, check it: past the limit,
  respawn or compact first instead of waiting for the lead to ask.
- `orch status --json` gives the same board in machine-readable form.
  `orch peek <id> --lines N` tails a pane.
