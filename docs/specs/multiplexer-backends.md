---
harness: claude
model: sonnet
effort: medium
base: main
---
# Multiplexer backends and native capabilities

## Behaviour
orch runs against a backend: the program that holds the panes, terminals and
worktrees the leads live in. Herdr is the only backend today, and orch is
welded to it. This spec puts a small interface between orch core and the
backend, moves herdr behind it with no change in behaviour, and adds Orca as the
second backend. Any later multiplexer is added by writing one file and passing
the conformance suite, with no edit to orch core.

Beyond the required operations, a backend may bring native capabilities: named
features such as running the repo's worktree setup and archive scripts, waking
the orchestrator on a lead's report, or hosting a decision gate. When the active
backend declares a capability, orch uses the native one; otherwise orch runs its
own implementation, which every backend gets. Orca is the first backend to bring
native capabilities. The set is open: a later backend may bring different ones,
and adding a capability name touches the registry and its tests, never an
existing backend.

Why: as of 2026-11-08 `tool/herdr.go` is the only seam and about 50 call sites in
eight files build herdr arguments and parse its JSON inline (`orch lsp refs
herdrCmd herdrJSON agentMap paneExists`). `orch spawn`, `sub`, `prune`, `adopt`
and `become` fail unless `HERDR_ENV=1` or `HERDR_PANE_ID` is set
(`tool/orchestrator.go:307`, `:1648`, `:1701`, `tool/sub.go:150`,
`tool/prune.go:69`), and the orchestrator's role marker is keyed by the herdr
pane id (`tool/hook.go:79-83`). Orca ships features orch re-implements
(worktree setup and archive scripts, a mailbox, gates), and orch cannot use them.
Research: reports/Orca and herdr orchestration integration.md.

## Design rules
- Core never names a backend. Orch core code outside the backend files checks
  `Backend.Has(cap)` and never `name == "herdr"` or `name == "orca"`. A test
  fails the build when a core file imports a backend file's identifiers or
  contains a backend env prefix (`HERDR_`, `ORCA_`).
- The interface is derived from what orch core needs, not from a backend's
  command list. Required operations are the ones orch uses on herdr today:
  `Self`, `Start`, `Prompt`, `Read`, `Close`, `Focus`, `Rename`, `Status`,
  `Wait`, `List`, `Resolve`. A feature only one backend has is a capability.
- Handles are opaque. A `Handle` is a backend-owned string. Core stores it,
  compares it for equality and passes it back; it never parses it. The
  `workspace:` prefix parsing in placement (`placePane`) moves into the herdr
  backend. A stored handle can go stale (herdr pane closed, Orca restart);
  `Resolve(h)` returns the live handle or `ErrGone`. `paneExists` becomes
  `Resolve`.
- Identity is not env injection. `Self()` returns the orchestrator or lead
  identity through whatever the backend offers. Orca's launch commands have no
  `--env`, so core must not assume `ORCH_*` reach a child through the launcher;
  `Start` returns the identity, and a backend that cannot inject env writes the
  identity to a file the lead's `orch` calls read, keyed by the handle. The role
  marker file (`tool/hook.go:79-95`) is keyed by `Self().ID`, not by
  `HERDR_PANE_ID`. `ORCH_ROLE`, when set, still wins (`tool/hook.go:73-78`).
- Normalised vocabulary. `State` is `idle`, `working`, `blocked`, `done` or
  `unknown`. A backend maps its native values into it and maps anything it does
  not recognise to `unknown`, never an error. A blocked agent also carries a
  `BlockedReason` string for display.
- Foreign env is stripped. `Start` removes every registered backend env prefix
  except its own from the child environment, so a lead started from an Orca
  terminal does not inherit `ORCA_PANE_KEY` and report to Orca through its
  hooks. Registering a backend registers its prefix.
- Capabilities are named contracts with a fallback. A capability has a name, a
  Go interface, an orch-owned implementation (the fallback) and a conformance
  test. The registry is a map from name to contract in one file. A backend
  declares the capabilities it implements at detection time. Config can force the
  fallback for a capability on any backend (`backend.<name>.native`, a list of
  capability names; default per capability below), so a flaky native feature
  never blocks work.
- The ledger and disk state stay the source of truth. A native capability is a
  transport or an executor. orch writes the ledger entry, the lead record and the
  decision record whichever side does the work, and on disagreement the disk
  state wins.
- Never delegated, on any backend: `orch verify`, the spec gate and findings
  ledger, locks, cost routing, integration branches, task.md, and the rule that
  leads never message each other. A backend capability that offers lead-to-lead
  messaging is not registered.
- Destructive steps keep orch's checks. Before any worktree removal, native or
  not, orch checks the branch is merged, the tree is clean and uses `git branch
  -d`. `--force` still needs the user's approval. A native archive hook runs
  after these checks and never replaces them.
- Backend choice. `ORCH_BACKEND=<name>` wins; otherwise each registered backend's
  `Detect()` runs in registration order and the first match is used; otherwise
  commands that need a backend fail early with one message that names the
  detected env and the registered backends. There is no null backend. Commands
  that need no backend (`plan`, `verify`, `gate`, `bench`, `lsp`, `version`)
  never touch it.
- State compatibility. `lead.json` keeps its current `PaneID` field and gains
  `Backend` (name). A record without `Backend` is read as `herdr`. Run state
  location keeps its current rules (`ORCH_HOME`, then herdr's worktrees
  directory when its config exists, then `~/.local/state/orchestrate`,
  `tool/paths.go:198-234`); the herdr config lookup moves into the herdr backend
  as an optional `StateDir()` capability so another backend can name its own.
- Layout is optional. Splitting a pane and creating a tab (`tool/gate_pane.go`,
  `tool/orchestrator.go:560-580`, `tool/sub.go:62`) is the `layout` capability.
  Without it, orch starts each extra agent as a separate `Start` and skips tab
  and split placement.
- Fakes. `ORCH_HERDR` and the `os.Args[0]` fake (`tool/orch_test.go:407-410`)
  keep working. A backend gets one fake binary or in-process fake, and the
  conformance suite runs against every fake.

## Capability set (first release)
Declared in the registry; the fallback exists for all, native implementations
only where the backend row says so.

| Capability | Contract | Fallback | herdr | orca |
|---|---|---|---|---|
| `layout` | split a pane, create a tab, place a pane | separate starts, no placement | native | not declared |
| `worktree.manage` | create, open, remove a worktree for a lead, set parent | orch runs `git worktree` | native | native, default on |
| `worktree.setup-hook` | start the repo's setup script on a new worktree; result `Started` or `Done(exit)` | orch runs the script | not declared | native |
| `worktree.archive-hook` | run the repo's archive script before removal, may block it | orch runs the script | not declared | native |
| `status.blocked-reason` | say why an agent waits on a human | none, `blocked` only | not declared | native |
| `state.dir` | name the run-state directory | `~/.local/state/orchestrate` | native | not declared |
| `wake`, `gate`, `dispatch` | reserved names and contracts only | orch's own | not declared | not implemented here |
| `browser` | give `ui-verifier` the backend's browser | none | not declared | not wired here |

`wake`, `gate` and `dispatch` have their contract and fallback registered so the
names are taken, but the Orca native implementations are a follow-up spec: Orca
marks its orchestration layer experimental. `browser` is listed so the contract
shape is reserved.

## Hook scripts
The repo owns one setup script and one archive script. A runner passes them
`ORCH_ROOT_PATH`, `ORCH_WORKTREE_PATH` and `ORCH_WORKSPACE_NAME` (the common
subset Orca also provides as `ORCA_ROOT_PATH`, `ORCA_WORKTREE_PATH`,
`ORCA_WORKSPACE_NAME`) and run them with the worktree as working directory.
Scripts must not depend on a variable outside that set. Orch adds
`ORCH_BRANCH` and `ORCH_TASK_ID` only when it runs the script itself, and says so
in the docs.

Native runs are blind. When the backend declares the capability, orch never
reads the script text: it asks the backend to run it and reads only the result.
The Orca backend passes `--setup run` on create and `--run-hooks` on remove
(without `--run-hooks` Orca skips the archive script on `orca worktree rm`). A
failing archive hook blocks removal as Orca reports
(`worktree_archive_hook_failed`); orch does not add
`--allow-failed-archive-hook`, and does not rerun a hook that ran and failed.

Verified on Orca 1.4.253 with a scratch repo (2026-11-08): `worktree create
--setup run` returns success at once, and the setup script runs afterwards in a
terminal titled "Setup" through a generated `setup-runner.sh`; a setup script
that exits 3 left the worktree created, produced no warning and no error, and
the Setup terminal stayed `running` (`terminal wait --for exit` timed out). So a
native setup result is `Started`, never `Succeeded`. The contract returns
`Started` or `Done(exitCode)`; the fallback runner returns `Done`, the Orca
backend returns `Started`. Orch must not start work that needs setup output on
the strength of a native `Started`; a lead brief that needs it says the setup may
still be running, and a setup script that must gate the lead writes its own
ready marker. For the archive hook, `worktree rm --run-hooks` ran the script,
and exit 4 blocked removal with error code `worktree_archive_hook_failed` and
`data.exitCode`, even with `--force`; without `--run-hooks` removal succeeded
with the warning "archive hook skipped".

Orch reads the script text only in the fallback runner, which runs when the
backend does not declare the capability, when `backend.<name>.native` lists it
out, or when the backend refuses before running anything (an unsupported flag, or
an Orca setup policy of `ask` that needs a decision). Refused-before-running and
ran-and-failed are different results: the first falls back, the second does not.
The fallback reads the script text from the repo's orch config keys
`hooks.setup` and `hooks.archive`, then from `scripts.setup` and
`scripts.archive` in `orca.yaml` when the keys are absent.

## Decisions made
- The user decided on 2026-11-08 that the Orca backend accepts Orca's agent launch
  defaults, including its permission mode. It passes no permission flag and does
  not override the setting; the user changes Orca's setting when they want
  otherwise.
- The user decided on 2026-11-08 that Orca's `worktree.manage` is on by default.
- Checks the orca-backend package performs and records in docs/backends.md (they
  change no contract here): the complete `terminal wait` status values (only
  `running` and the `timeout` error were seen), and whether Orca rejects hook
  events from a foreign pane key.

## Authority
May decide: Go type and file names, file layout inside owned files, test layout,
exact error and doc wording, which herdr call sites map to which interface method.
Must escalate: any change to a command's output, exit code or flags; any change
to lead.json beyond the approved `Backend` field; a change to the pass rule or
gate.json; adding a required interface method after the contract tests are
written; enabling an Orca native capability by default that this spec lists as
opt-in or not implemented; reading hook script text on a backend that runs the
hook natively.
Never: push, merge, edit files another package owns, remove `ORCH_HERDR`, change
the on-disk ledger, lead or decision file formats, delete worktrees, run `orch
spawn` or start panes in tests against a real herdr or Orca.

## Open questions
None.

## Packages

### backend-core: Interface, capability registry, detection and conformance suite
kind: ship
model: sonnet
owns: tool/backend.go, tool/backend_caps.go, tool/backend_detect.go, tool/backend_env.go, tool/backend_conformance_test.go, tool/backend_fake_test.go, tool/backend_core_test.go

New files hold the `Backend` interface, `Handle`, `Identity`, `State`, `Spec`
(agent, harness, model, effort, cwd, prompt, env, name), the capability registry
(name to contract and fallback), registration (`registerBackend(name, prefix,
detect, ctor)` called from each backend file's `init`), detection and
`ORCH_BACKEND` selection, the early-failure message, config parsing for
`backend.<name>.native`, foreign-env stripping, and the shared conformance suite
(`RunConformance(t, factory)`) with its in-process fake. Nothing here calls
herdr or orca.

#### Builds
- Defines the required backend interface with opaque handles, normalised state and an identity that does not rely on env injection.
  id: backend-interface
- Registers backends by name, prefix and detect function and picks one from ORCH_BACKEND or detection, failing early with a message that names the registered backends.
  id: backend-registry
  search: Code that chooses between several backends from environment variables at startup
- Registers named capabilities with a contract and an orch-owned fallback, and resolves native versus fallback from the backend's declaration and config.
  id: capability-registry
- Strips every other registered backend's env prefix from a started agent's environment.
  id: env-strip
- Runs one conformance suite against any backend factory, covering required operations and each declared capability.
  id: conformance-suite

#### Acceptance
`go test -p 2 -count=1 -run 'Backend|Conformance|Capability' ./...` in tool/ passes and `go vet ./...` in tool/ is clean. New tests show:
- a second fake backend registered in a test is detected, selected by `ORCH_BACKEND`, and used with no edit to core files;
- no registered backend and no env gives the early-failure error naming the registered backends; commands that need no backend still run;
- a capability declared by the backend resolves native; undeclared, or listed out of `backend.<name>.native`, resolves the fallback;
- foreign prefixes are removed from the child env and the backend's own prefix is kept; a newly registered prefix is stripped from other backends' children;
- a core-file scan test fails when a non-backend file contains a registered backend name in a comparison or a backend env prefix;
- the conformance suite passes against the fake and fails against a fake that returns a raw native status.

### herdr-backend: Move herdr behind the interface with no behaviour change
kind: ship
model: sonnet
depends: backend-core
owns: tool/herdr.go, tool/backend_herdr.go, tool/backend_herdr_test.go, tool/orchestrator.go, tool/sub.go, tool/worktree.go, tool/prune.go, tool/lead.go, tool/locks.go, tool/guard.go, tool/hook.go, tool/state.go, tool/paths.go, tool/orch_test.go, tool/worktree_test.go, tool/sub_test.go

Move `herdrCmd`, `herdrJSON`, `agentMap` and `paneExists` (`tool/herdr.go`) into
`tool/backend_herdr.go`, implement the interface and the `layout`,
`worktree.manage` and `state.dir` capabilities, and replace every call site
(`orch lsp refs herdrCmd herdrJSON herdrCmdContext agentMap paneExists` is the
list) with interface calls. Keep the `agent wait --until working --until
blocked` start sequence and the `agent_pane_busy` retry (`tool/orchestrator.go:452-456`)
inside `Start`/`Prompt`. Replace `HERDR_ENV` and `HERDR_PANE_ID` reads
(`tool/orchestrator.go:307`, `:1648`, `:1701`, `tool/sub.go:150`, `tool/prune.go:69`,
`tool/hook.go:79`, `tool/worktree.go:12`) with `Self()` and `Has("worktree.manage")`.
Add `Backend` to `lead.json` per Design rules. gate_pane.go is a separate
package. No command output, exit code or flag changes.

#### Builds
- Implements the herdr backend with every herdr command and JSON shape contained in one file.
  id: herdr-impl
  reuse: tool/herdr.go#agentMap
- Replaces the herdr calls and env checks in the lead, sub, worktree, prune, lock and guard code with backend calls.
  id: herdr-callsites
- Keys the role marker and lead owner by Self().ID and records the backend name in lead.json, reading older records as herdr.
  id: herdr-identity

#### Acceptance
`go test -p 2 -count=1 -run 'Backend|Orch|Worktree|Sub|Prune|Lock|Guard|Hook' ./...` in tool/ passes unchanged apart from fakes, `go vet ./...` is clean, and the conformance suite passes against the herdr fake. New tests show:
- the full `orch_test.go` flow (spawn, status, send, close) gives byte-identical command output before and after, using the existing `ORCH_FAKE_HERDR` harness;
- a `lead.json` without `Backend` is read as herdr and rewritten with it; the role marker for an existing pane id is still found;
- outside herdr, `spawn`, `sub`, `prune`, `adopt` and `become` fail early with the registry message and not with the old HERDR_ENV text; `plan`, `verify` and `gate` still run;
- `rg 'HERDR_|herdr ' tool --glob '!tool/backend_herdr*' --glob '!*_test.go'` finds no match outside the herdr backend files (comments and the docs excepted).

### gate-pane-layout: Gate pane on the layout capability
kind: ship
model: sonnet
depends: herdr-backend
owns: tool/gate_pane.go, tool/gate_pane_test.go

Move the herdr calls in `tool/gate_pane.go` (tab create, pane split, close,
`send-keys`, `agent get`, lines 56 to 289) onto the interface and the `layout`
capability. Without `layout`, run each gate pass as its own `Start` and close
it, with no pool placement. Gate output and verdicts do not change.

#### Builds
- Runs gate passes through the backend, using layout placement when the backend has it and separate starts when it does not.
  id: gate-pane-backend

#### Acceptance
`go test -p 2 -count=1 -run 'GatePane|Gate' ./...` in tool/ passes unchanged, and a new test shows the gate completing with a backend that does not declare `layout`.

### orca-backend: Orca as the second backend, with its first native capabilities
kind: ship
model: sonnet
depends: herdr-backend
owns: tool/backend_orca.go, tool/backend_orca_test.go, tool/backend_hooks.go, tool/backend_hooks_test.go, docs/backends.md

New file `tool/backend_orca.go` registers `orca` with prefix `ORCA_`, a
`Detect()` that needs `ORCA_AGENT_SESSION_ID`, `ORCA_PANE_KEY` or `ORCA_TERMINAL_HANDLE`,
and an `ORCH_ORCA` binary override (default `orca`, as `ORCH_HERDR`). It wraps
`orca ... --json` and never speaks Orca's socket. Mapping:
- `Start` is `agent launch` (existing worktree) or `worktree create --agent`;
  reads `outcome.kind` and `launch.outcome.handle`; passes no env (there is no
  `--env`), so identity is written to the identity file keyed by the handle.
  `Resolve` re-lists after a `terminal_handle_stale` error and uses only the
  replacement handle.
- `Status` and `Wait` use `worktree ps --agent-wait --json` and `terminal wait`.
  `state` maps to the normalised set; a non-null `agentWait` or `blockedReason`
  gives `blocked` with the reason; an absent `agentWait` gives `unknown`.
- `List`, `Read`, `Close`, `Focus` and `Rename` use `worktree ps`, `terminal
  read`, `terminal close`, and the matching terminal and worktree commands.
- Declares `worktree.setup-hook`, `worktree.archive-hook`,
  `status.blocked-reason`, and `worktree.manage`, on by default (listing a
  capability out of `backend.orca.native` turns it off). Does not declare
  `layout`. `worktree.manage` creates with `worktree create --name <id> --branch
  orch/<id> --base-branch <base> --no-parent` (Orca infers a parent from the
  caller's directory otherwise; a scratch create from the orchestrate repo got
  that repo's main worktree as parent), reads the worktree path from the result
  (Orca puts it under `~/orca/workspaces/<repo>/<name>`, never computed by
  orch), and removes with `worktree rm` after orch's own merged and clean checks,
  without `--delete-branch` (orch deletes with `git branch -d`).
  Launch note, verified 2026-11-08: `worktree create --agent claude` starts
  `claude --dangerously-skip-permissions ...` and the TUI stops at a "Bypass
  Permissions" confirmation (default option "No, exit") until a human answers;
  `worktree ps --agent-wait` listed no agent row for it. `Start` must not treat a
  launched Orca agent as running until `Status` says so, and must report that
  prompt as `blocked`.

New file `tool/backend_hooks.go` holds the hook contract: the fallback runner for
`worktree.setup-hook` and `worktree.archive-hook` (reads the script text per Open
question 1, runs it with the env set from Hook scripts, 120 s timeout, working
directory the worktree), used by every backend that does not declare the
capability. docs/backends.md is the "writing a backend" guide: required
operations, the capability table, the env and identity rules, how to register and
how to pass the conformance suite.

#### Builds
- Implements the Orca backend over the orca CLI with a binary override and an in-process fake.
  id: orca-impl
- Maps Orca states and blocked reasons into the normalised state, with unknown for anything unrecognised.
  id: orca-status
- Runs the repo's setup and archive scripts blind through the backend when it declares the capability, and through orch's own runner (reading the script text) otherwise, with the same env contract.
  id: hook-runner
- Documents how to write a backend and lists the capability contracts.
  id: backend-guide

#### Acceptance
`go test -p 2 -count=1 -run 'Orca|Hook|Backend|Conformance' ./...` in tool/ passes and `go vet ./...` is clean. New tests show:
- the conformance suite passes against the Orca fake;
- in an environment with `ORCA_*` and no `HERDR_*`, `orch spawn` selects Orca; with both, `ORCH_BACKEND` decides; a lead started by the herdr backend from an Orca terminal has no `ORCA_*` in its env;
- `Status` maps `working`, a prompt `blockedReason`, an unrecognised state and an absent `agentWait` to `working`, `blocked` (with the reason), `unknown` and `unknown`;
- a stale handle is re-resolved once and the old handle is never used again;
- native setup is requested with `--setup run`; native archive is requested with `--run-hooks`; a failing archive hook blocks removal and orch does not retry with `--allow-failed-archive-hook`; orch's merged and clean checks run before the Orca remove in every case;
- on the native path orch never opens `orca.yaml` or the `hooks.*` keys (a test with those files unreadable still passes); a refusal before any script ran falls back to the orch runner, and a hook that ran and failed is not rerun;
- with the capability listed out of `backend.orca.native`, orch runs the script itself with the same variables, and the script sees `ORCH_WORKTREE_PATH` and the working directory as the worktree;
- a lead record written by the Orca backend carries `Backend: "orca"` and the herdr backend refuses to resolve it with an error that names the backend.

## Out of scope
Native Orca `wake`, `gate` and `dispatch` implementations; wiring `browser` to
`ui-verifier`; any backend beyond herdr and Orca; a null backend; moving existing
run state.
