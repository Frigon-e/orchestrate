# Repo config

`<repo>/.agents/orchestrate.toml` holds the orchestrator's settings for one
repo: where tasks branch from, what every brief names, and the few facts
that change how work here is split or scheduled. It is not documentation.
Leads already load the repo's CLAUDE.md or AGENTS.md and its skills, and
the board prints every note after each restart, so the file stays small.
Anyone who can commit to the repo controls it, so nothing about how leads
launch belongs here (that is the user config).

## Keys

| Key | Default | Holds |
|---|---|---|
| `base_branch` | the checkout's current branch | The branch work lands on, as the repo's docs say; not always the forge default. |
| `lead_skills` | `["route-work"]` | Skills every task here needs. A skill only some tasks need goes in those task.md files. |
| `check` | none | The one command the repo's docs name for verifying a change. Leads run it before `done`. |
| `quiet_minutes` | 30 | Idle minutes before the watcher says `quiet`. Raise it only when normal steps run silent for longer. |
| `stall_minutes` | 15 | Minutes a working lead may report nothing and change nothing in its worktree before the watcher says `stalled`. Raise it when a normal step (a long test run) runs longer. |
| `human_only` | none | Question areas only a person decides (for example `billing`, `tax`, `legal`). `orch sweep` sends every card with a matching `area:` to the user, whatever the model passes answered. |
| `notes` | none | At most 5 one-line notes of 160 characters, printed on the board. orch refuses more. |
| `locks` | none | At most 5 `name = "when to hold it"` pairs for resources one lead may use at a time (a test VM, a device, a fixed port). Every brief tells the lead to hold each lock for that span; orch makes others wait. |

## What a note is for

A fact that changes how you split, schedule or brief tasks in this repo,
and that the repo's own docs don't tell an orchestrator:

- areas that always change together, so one lead owns both;
- an ordering rule between kinds of task (schema changes merge before the
  API work that reads them).

A resource only one lead may use at a time is a `locks` entry, not a note:
orch enforces it. Write a note as the rule and its reason, so it can be
judged later: `schema changes merge before the API work that reads them:
the API tests run against the merged schema`.

Not a note:

- **How to do the work**: commands, conventions, layout. Leads read that
  from the repo. If the repo lacks it, offer the user a task that adds it
  there; never write it yourself.
- **What one task needs**: that task's task.md.
- **State**: lead ids, dates, counts, branch or ticket status.
- **Anything already enforced** by a check, a hook or orch.
- **A user preference across repos**: your own memory.

## Changing it

Add a key, a note or a rewording only when all of these hold:

1. **Seen**: it fixes something that went wrong in this repo, and you can
   name the lead and what happened, or the user asked for it. A lead's
   `note "config: ..."` is evidence, not a request.
2. **Orchestrator's**: it fits "What a note is for".
3. **One home**: it is not already in the repo's CLAUDE.md, AGENTS.md,
   skills or this file.
4. **Rewrite first**: reword a note that covers the same ground; at the cap,
   drop the weakest note to make room.

Correcting or removing a note that has proved wrong needs only the evidence.

Show the user the exact diff and the evidence. Once they approve, a ship
lead on the cheapest model makes it: its Spec holds the diff, its commit
message names the evidence, its Acceptance is `orch config-check
.agents/orchestrate.toml` passing in its worktree, and it merges through
the repo's own flow. You never edit the file.

## Setup

When the board says `repo config: none` and the user is starting more than
a one-off task, offer setup; the defaults suit a single task. Setup is a
ship lead on a mid-tier model that writes the file on its branch. Its Spec:

- `base_branch`: the branch the repo's docs (CLAUDE.md, AGENTS.md,
  CONTRIBUTING) say work lands on.
- `check`: the command those docs name. Leave it unset when they name
  none, and say so.
- `lead_skills`: `route-work`, plus only skills the repo says every change
  needs.
- `locks`: each resource the repo's docs or tooling say one run at a
  time may use, held for the whole span that shares state (deploy through
  reading results, not just the run).
- `notes`: only what "What a note is for" allows, each found in the
  repo's docs or tooling. None is a fine answer.

Acceptance: `orch config-check .agents/orchestrate.toml` passes, and the
`done` line or a report gives each value with its `path:line` evidence.
Show the user the file before the merge.
