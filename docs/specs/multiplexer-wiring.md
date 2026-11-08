---
harness: claude
model: sonnet
effort: medium
base: orch/int-multiplexer-backends
---
# Multiplexer backends: end-to-end wiring

## Behaviour
Follow-up to docs/specs/multiplexer-backends.md, whose four packages (backend
interface, herdr backend, gate pane on the layout capability, Orca backend) are
built and merged on `orch/int-multiplexer-backends`. This spec closes the gaps
that keep `orch spawn`, `orch close` and `orch prune` from working end to end on
a backend that has worktree management but no layout (Orca), and makes the Orca
backend tolerate the real shape of Orca's output. Herdr behaviour is unchanged.

## Design rules
- All rules of docs/specs/multiplexer-backends.md apply unchanged: core never
  names a backend, handles and paths come from the backend, orch's merged and
  clean checks run before any removal, native hook runs are blind, a hook that
  ran and failed is not rerun, and the disk state is the source of truth.
- Herdr output, exit codes and flags stay byte-identical (`ORCH_FAKE_HERDR` flow).
- Every Orca field is optional; a missing field maps to `unknown`, never an error.

## Authority
May decide: Go names, file layout inside owned files, test layout, exact wording.
Must escalate: any change to a command's output, exit code or flags on herdr; any
change to lead.json beyond the approved `Backend` field; enabling a native
capability this spec does not list.
Never: push, merge, edit files another package owns, run `orch spawn` or start
real herdr or Orca panes, agents or worktrees in tests, delete real worktrees.

## Packages

### backend-wiring: Make orch spawn, close and prune work end to end on a backend without layout
kind: ship
model: sonnet
owns: tool/worktree.go, tool/orchestrator.go, tool/prune.go, tool/paths.go, tool/backend_hooks.go, tool/backend_orca.go, tool/backend_wiring_test.go, tool/orch_test.go, docs/backends.md

Built after backend-core, herdr-backend, gate-pane-layout and orca-backend, which
leave these gaps (reported by the orca-backend worker, 2026-11-08):
- `placeShip` (tool/worktree.go) still requires both `worktree.manage` and
  `layout`, so `orch spawn` under Orca cannot place a lead, and orch computes the
  worktree path itself (`worktreePath`).
- Nothing calls `createWorktreeWithSetup` or `removeWithArchive`
  (`removeWorktreeWithArchive`).
- `tool/paths.go#repoKeys` rejects a `[hooks]` table in `.agents/orchestrate.toml`,
  so `hooks.setup` and `hooks.archive` (the fallback script source) cannot be set.
- Orca field names are assumed (ps agent rows `terminalHandle`, `sessionId`,
  `blockedReason`; terminal rows `paneKey`, `status`). Real output (2026-11-08)
  has terminal rows with `handle`, `title`, `worktreeId`, `worktreePath`,
  `tabId` and no `paneKey` or `status`; the ps agent row seen had `paneKey`,
  `parentPaneKey`, `state`, `agentType` and none of the other three (it was a
  structured chat agent, so a terminal-backed row is still unseen). Treat every
  Orca field as optional and map a missing one to unknown.

Work: `placeShip` places a lead with `worktree.manage` alone when the backend has
no `layout` (one `Start` in the new worktree; no tab or split); the worktree path
comes from the backend result and orch stores it in the lead record; `orch spawn`
calls the setup-hook helper (native `Started` or the fallback runner) when it
creates a worktree; `orch close` and `orch prune` run orch's merged and clean
checks first and then remove through the archive-hook helper (native
`--run-hooks`, or the fallback runner); `repoKeys` accepts a `[hooks]` table with
`setup` and `archive` strings and `loadBackendConfig` reads it; Orca field parsing
is made tolerant as above. The prune unknown-state text stays backend-neutral
(`Herdr/Orca state unknown` when no backend is active).

#### Builds
- Places a ship lead on a backend with worktree management and no layout, taking the worktree path from the backend.
  id: wiring-place
- Runs the setup hook on worktree creation and the archive hook on removal after orch's own safety checks, native or fallback.
  id: wiring-hooks
- Accepts hooks.setup and hooks.archive in the repo config.
  id: wiring-config
- Parses Orca rows with every field optional.
  id: wiring-orca-fields

#### Acceptance
`go test -p 2 -count=1 -run 'Backend|Orca|Hook|Wiring|Worktree|Prune|Orch' ./...` in tool/ passes and `go vet ./...` is clean. New tests in tool/backend_wiring_test.go, using the Orca fake, show:
- `orch spawn` under a fake Orca with `worktree.manage` and no `layout` creates the worktree, starts the lead in it, stores the backend-returned path, and `orch status` shows it;
- the setup helper is called once on create, and a native `Started` does not block the spawn; with the capability turned off the fallback runner runs the configured script;
- `orch close` and `orch prune` keep an unmerged or dirty worktree and never call remove; for a merged clean one they call remove with `--run-hooks` on Orca; a failing archive hook keeps the worktree and reports `worktree_archive_hook_failed`; the fallback runner path does the same without Orca;
- a config with `[hooks] setup = "..." archive = "..."` loads, and an unknown key under `[hooks]` is still rejected;
- ps and terminal rows missing `terminalHandle`, `sessionId`, `blockedReason`, `paneKey` or `status` parse, and the missing field gives `unknown`, never an error;
- the herdr flow (`ORCH_FAKE_HERDR`) is byte-identical before and after.

## Open questions
None.
