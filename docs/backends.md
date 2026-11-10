# Writing a backend

A backend is the program that holds the panes, terminals and worktrees the leads
live in. orch core talks to it through the `Backend` interface in
`tool/backend.go`; herdr (`tool/backend_herdr.go`) and Orca
(`tool/backend_orca.go`) are the two implementations. A new backend is one file
plus its test, with no edit to orch core. The design is in
`docs/specs/multiplexer-backends.md`.

## Required operations

| Method | Contract |
|---|---|
| `Name` | the registered name |
| `Self` | identity of the calling process, or `ErrNotInside`. Makes no backend calls. |
| `Start` | launch an agent, return its `Identity`. Strips foreign env prefixes. |
| `Prompt`, `Read`, `Focus`, `Rename` | act on a handle |
| `Close` | end the agent; a gone handle is `ErrGone` |
| `Status` | normalised `AgentStatus`; a gone handle is `ErrGone` |
| `Wait` | block until the agent is in one of the given states or the timeout passes |
| `List` | every agent the backend knows |
| `Resolve` | the live handle for a stored one, or `ErrGone`. Backend unreachable is another error. |
| `Declares` | capability names implemented natively |

Rules:

- A `Handle` is an opaque string. Core stores it, compares it and passes it back.
  A stored handle can go stale (pane closed, Orca restart); `Resolve` returns the
  replacement and the backend uses only the replacement afterwards.
- `State` is `idle`, `working`, `blocked`, `done` or `unknown`. Map every native
  value through `normaliseState` with a table; anything not in it is `unknown`,
  never an error. `BlockedReason` is set only with `blocked`.
- Do not report a launched agent as `working` until the backend says so.

## Capabilities

A capability is a name, a Go interface (the contract), an orch-owned fallback and
a conformance check. The registry is `capRegistry` in `tool/backend_caps.go`.
`Declares()` lists what the backend implements; the type must implement the
contract. Config `backend.<name>.native` (a list of capability names) turns
declared ones off per backend: `Active.Has(name)` is true only when declared and
not listed out. Core asks `Has` and never compares a backend name.

| Capability | Contract | Fallback | herdr | orca |
|---|---|---|---|---|
| `layout` | `Layout`: split a pane, create a tab, `Contains` | separate starts, no placement | native | not declared |
| `worktree.manage` | `WorktreeManager`: create, open, remove, list open | orch runs `git worktree` | native | native |
| `worktree.setup-hook` | `SetupHook`: result `Started` or `Done(exit)` | orch's runner (`scriptHooks`) | not declared | native |
| `worktree.archive-hook` | `ArchiveHook`: may block removal | orch's runner | not declared | native |
| `status.blocked-reason` | `BlockedReasoner` marker: `Status` carries `BlockedReason` | `blocked` only | not declared | native |
| `state.dir` | `StateDirer`: run-state directory | `~/.local/state/orchestrate` | native | not declared |
| `wake`, `gate`, `dispatch` | reserved | orch's own | not declared | not implemented |
| `browser` | reserved | none | not declared | not wired |

A capability that offers lead-to-lead messaging is never registered.

### Hooks

The repo owns one setup and one archive script. Both runners give them
`ORCH_ROOT_PATH`, `ORCH_WORKTREE_PATH` and `ORCH_WORKSPACE_NAME` (Orca's
equivalents are `ORCA_ROOT_PATH`, `ORCA_WORKTREE_PATH`, `ORCA_WORKSPACE_NAME`)
and run them with the worktree as working directory. Scripts must not rely on any
other variable. Only orch's own runner also sets `ORCH_BRANCH` and `ORCH_TASK_ID`.

- Native runs are blind: orch never reads the script text, only the result.
- orch's runner (`tool/backend_hooks.go`) runs when the backend does not declare
  the capability, config lists it out, or the backend refuses before running
  anything (`ErrHookRefused`). It reads `hooks.setup` / `hooks.archive` from
  `<repo>/.agents/orchestrate.toml`, then `scripts.setup` / `scripts.archive` from
  `orca.yaml`. 120 s timeout.
- A hook that ran and failed is never rerun and never falls back. A native
  failure is `*HookFailedError`; the runner's is a `HookDone` with the exit code.
- Orca specifics: `CreateWorktree` with `WorktreeSpec.NativeSetup` passes
  `--setup run`, and Orca only starts the script, so the result is `Started`,
  never `Done`. A failing setup leaves the worktree created with no error. `RunArchive`
  arms the next `RemoveWorktree` of that path to pass `--run-hooks`; a failing
  hook (`worktree_archive_hook_failed`) comes back from `RemoveWorktree` as
  `*HookFailedError` and blocks removal.
- Remove through `removeWorktreeWithArchive`: orch's merged and clean checks run
  first, then the archive hook, then the removal. The backend never forces,
  never deletes the branch, never allows a failed archive hook.
- Wiring: `orch spawn` creates the worktree through `createWorktreeWithSetup`
  (a native `Started` never blocks; a runner script that exits non-zero is noted
  in the status log and the spawn goes on). `orch close` and `orch prune` remove
  through `removeWorktreeWithArchive`; a failing archive hook keeps the worktree
  and prints `worktree_archive_hook_failed`. `orch prune` keeps a plain
  `git worktree remove` only when the backend has no native archive hook and the
  repo has no archive script.
- The repo config takes `[hooks]` with `setup` and `archive` strings; any other
  key under it is rejected.
- A backend without `layout` is placed with one `Start` in the new worktree.
  When the backend chose the worktree location, `lead.json` keeps it as
  `worktree_path` (absent when it is where orch would put it). Callers that
  still compute `worktreePath(dir, id)` are `verify`, `guard`, `stall`, `sub`,
  `integration` and `wake_decision`; they are wrong for such a lead.
- Orca rows: every field is optional. Real `terminal list` rows have `handle`,
  `title`, `worktreeId`, `worktreePath`, `tabId`; a ps agent row that names no
  handle or pane key is matched as the lone agent of its terminal's worktree.

## Env and identity

- Every backend registers an env prefix (`HERDR_`, `ORCA_`). `Start` removes every
  other registered prefix from the child environment with
  `childEnv(own, base, extra)`, so a lead started from another multiplexer's
  terminal does not report to it through its hooks.
- Identity is not env injection. A backend whose launcher has no `--env` (Orca)
  writes the identity to a file keyed by the handle
  (`$ORCH_IDENTITY_DIR`, default `~/.local/state/orchestrate/identities`), holding
  the id, the stable pane key used to find a stale handle again, and the
  requested `ORCH_*` env. `Self()` reads it from the terminal's own env.
- The role marker and lead owner are keyed by `Self().ID`. `ORCH_ROLE` still wins.
- `lead.json` carries `Backend`; a record without it is read as the legacy
  backend (`legacyBackendName`). A backend refuses to resolve another backend's
  record with an error that names both.

## Registering

From the backend file's `init` (registration order is file name order, and the
first matching `detect` wins when `ORCH_BACKEND` is unset):

```go
func init() {
	registerBackend("name", "NAME_", func(getenv func(string) string) bool {
		return getenv("NAME_SESSION") != ""
	}, func() (Backend, error) { return &nameBackend{}, nil })
}
```

Name the binary override `ORCH_<NAME>` (`ORCH_HERDR`, `ORCH_ORCA`). Keep every
command and JSON shape of the backend in its one file. Core files must not name
the backend or contain its env prefix; a scan test fails the build if they do.

## Passing the conformance suite

Write a fake: a binary the test re-executes (the test binary itself, switched by
an env var, as `ORCH_FAKE_HERDR` and `ORCH_FAKE_ORCA` do) or an in-process fake
(`tool/backend_fake_test.go`). Never test against the real program. Then:

```go
func TestConformanceName(t *testing.T) {
	RunConformance(t, func(t testing.TB) Backend { return &nameBackend{} })
}
```

The suite covers every required operation and one check per declared capability,
and fails a backend that returns a raw native status or declares a capability
whose contract it does not implement. Add backend-specific tests for status
mapping, stale handles and anything native. Run:

```
cd tool && go vet ./... && go test -p 2 -count=1 -run 'Backend|Conformance' ./...
```

## Unverified (Orca 1.4.253)

Two checks the orca backend was meant to perform could not be done and stay open.
Neither changes a contract:

1. **Complete `terminal wait` status values.** Only the status `running` and the
   `timeout` error have been seen. The backend maps every unrecognised status
   to `unknown`.
2. **Whether Orca rejects hook events from a foreign pane key.** Not tested. The
   backend relies on foreign `ORCA_*` being stripped from other backends'
   children (`childEnv`), not on Orca rejecting them.

Also assumed, not yet checked against a live Orca: the field names in the
`worktree ps --agent-wait` agent rows (`state`, `agentType`, `paneKey`,
`terminalHandle`, `title`, `agentWait`, `blockedReason`), the `terminal list`
rows (`handle`, `paneKey`, `title`), the error codes for a missing terminal or
worktree (`*_not_found`) and a dirty worktree (`worktree_dirty`), that `--repo
path:<dir>` is accepted, and that `terminal list` carries the pane key. The
Orca fake in `tool/backend_orca_test.go` encodes these assumptions; change both
together when they are checked.

## Run root without state.dir

A backend with no `state.dir` capability (Orca) keeps its run root at `~/.local/state/orchestrate/<repo>`. A plain shell outside any multiplexer follows the `.last-root` marker in that directory to the root the last session claimed, so `orch status` from a plain shell finds the Orca session's state.

## identity.env

Orca's `agent launch` takes no environment, so the Orca backend records a lead's requested variables in an identity file. On startup orch calls `SelfEnv` and sets each recorded variable that is not already set, never overwriting and never setting `ORCA_*` or `HERDR_*`. This is how an Orca lead gets `ORCH_ROLE`, `ORCH_TASK_DIR` and `ORCH_EPOCH`.
