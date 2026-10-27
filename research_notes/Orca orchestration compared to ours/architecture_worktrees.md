# Orca architecture: worktree lifecycle and agent spawning

Scope: github.com/stablyai/orca (MIT, TypeScript, Electron; ~106.9k stars, last push 2026-10-26). All code was read from `main` on 2026-10-26 via `gh api`. URL shorthand: `R` = https://github.com/stablyai/orca/blob/main/ ; docs are in `docs/site/content/docs/` and rendered at https://www.onorca.dev/docs/.
Items under "Inferences" are not verified in code. Items under "Gaps" were not checked.

## 1. How is a worktree created, named, linked to a branch, set up, and cleaned up?

### Takeaway
Orca drives plain `git worktree add` from the Electron main process, serialized, with a sanitized name, a prefixed branch, collision loop, a base-ref fetch, and a "prepared worktree" pool. After the add it copies or links env files, then runs `orca.yaml` setup in a visible "Setup" terminal. Removal is a multi-step flow (archive hook, PTY sweep, `git worktree remove`, safe `git branch -d`) that can run in the background.

### Cited Findings
- Creation runs in the main process: `ipcMain.handle('worktrees:create')`, with a `createWorktree:progress` event for UI progress. Local creation is `createLocalWorktree`/`performLocalWorktreeCreate`, serialized through `worktreeCreateGit.run(...)`. — [worktree-remote.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-remote.ts), [register-worktree-create-handlers.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktrees/create/register-worktree-create-handlers.ts)
- A "prepared worktree" pool exists (`consumePreparedWorktreeCreate`). It is skipped for sparse checkout and when checking out an existing branch. After a hit a rearm prepares the next one. — [worktree-remote.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-remote.ts)
- Name sanitizing (`sanitizeWorktreeName`): keeps Unicode letters/numbers and `._-`, other chars become `-`, collapses `--` and `..`, trims leading/trailing `.`/`-`, maps known emoji to shortcodes (or "workspace" if emoji only). — [worktree-logic.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-logic.ts)
- Blank task name yields a generated marine-creature name. — [first-session.mdx](https://www.onorca.dev/docs/first-session)
- Branch name: an explicit override must not start with `-` and must pass `git check-ref-format --branch`. Otherwise `${prefix}/${sanitizedName}`, where the prefix comes from the `branchPrefix` setting (`git-username`, custom, or none). Linear issues supply Linear's branch name as the override. — [worktree-branch-name.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-branch-name.ts), [worktree-remote.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-remote.ts)
- Collision loop (up to `WORKTREE_CREATE_MAX_SUFFIX_ATTEMPTS`) checks: existing local/remote branch, existing GitHub PR on the branch (via `gh`), existing path on disk, pending background removals. Generated names also consult a retired-name registry (`worktree-name-retirement.ts`). — [worktree-remote.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-remote.ts)
- Adopting an existing local branch is supported (`canCheckoutExistingLocalBranch`); metadata then gets `preserveBranchOnDelete: true`. — [worktree-remote.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-remote.ts)
- Path: `join(workspaceRoot, [repoName if nestWorkspaces], sanitizedName)`. Root is `settings.workspaceDir`, overridable per repo (`repo.worktreeBasePath`); `ensurePathWithinWorkspace` guards traversal; WSL mirrors the root to `<wslHome>/orca/workspaces`; SSH uses a relative root or sibling `<repoPath>/../<repoName>-<name>`. — [worktree-logic.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-logic.ts)
- Base ref: `resolveWorktreeCreateBase` uses `repo.worktreeBaseRef` or the default ref and refreshes the remote-tracking ref in parallel with name resolution. If the fetch fails but a local ref exists, creation falls back to the local base (offline creation). — [worktree-remote.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-remote.ts)
- Git invocation: `git worktree add [--no-checkout] --no-track -b <branch> <path> <base>` (or `git worktree add <path> <branch>` for an existing branch). Afterwards: `git config --local branch.<branch>.base <base>` (lineage), `--set-upstream-to` only for a same-name remote base, and `git config --local push.autoSetupRemote true` if unset. Sparse variant in `worktree-sparse-add.ts`. Progress parsed from git stderr. — [worktree-add.ts](https://github.com/stablyai/orca/blob/main/src/main/git/worktree-add.ts)
- Metadata stored via `store.setWorktreeMeta`: `instanceId`, `createdAt`, `lastActivityAt`, `baseRef`, `pushTarget`, linked issue/PR/Linear/GitLab/Bitbucket/Azure/Gitea items, `workspaceStatus`, `createdWithAgent`, creation provenance (desktop/CLI/automation). Worktree ID is `${repo.id}::${path}`. Parent/child lineage is recorded via `recordWorkspaceLineageForCreatedWorktree`. — [worktree-remote.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-remote.ts)
- Env and dependency carry-over, in order: (1) `repo.symlinkPaths` ("Worktree Shared Paths"; APFS clone-copy on macOS if possible, else link); (2) `worktree.sharedDirectories` in the primary checkout's `orca.yaml` (gitignored dirs, always linked; Windows tries a junction first); (3) `.worktreeinclude` at the primary root (literal gitignored paths, private copy; file <=256 KiB, <=1000 candidates, 2 GiB / 50,000-entry copy budget; globs and negation skipped with a warning). — [worktree-shared-directories.ts](https://github.com/stablyai/orca/blob/main/src/main/git/worktree-shared-directories.ts), [worktree-include-file.ts](https://github.com/stablyai/orca/blob/main/src/main/git/worktree-include-file.ts), [orca-yaml.mdx](https://www.onorca.dev/docs/model/orca-yaml)
- `orca.yaml` keys: `scripts.setup`, `scripts.archive`, `setupAgentStartupPolicy` (`start-immediately` | `wait-for-setup`), `issueCommand`, `defaultTabs`, `worktree.sharedDirectories`, `environmentRecipes`. A parse failure or duplicate key rejects the whole file; 256 KiB limit. Run policy settings (`setupRunPolicy`, `commandSourcePolicy`: `shared-only` | `local-only` | `run-both`) live in settings, not YAML. — [orca-yaml.mdx](https://www.onorca.dev/docs/model/orca-yaml), [hooks.ts](https://github.com/stablyai/orca/blob/main/src/main/hooks.ts)
- Setup and `defaultTabs` are read from the NEW worktree's `orca.yaml`; shared dirs, `.worktreeinclude` and the archive script are read from the primary checkout. — [hooks.ts](https://github.com/stablyai/orca/blob/main/src/main/hooks.ts), [orca-yaml.mdx](https://www.onorca.dev/docs/model/orca-yaml)
- Setup runs in a visible terminal titled "Setup". Main writes a runner script (Bash with `set -e`, or `.cmd` on native Windows) without executing it; the terminal runs it with env `ORCA_ROOT_PATH`, `ORCA_WORKTREE_PATH`, `ORCA_WORKSPACE_NAME` (plus `CONDUCTOR_ROOT_PATH`/`GHOSTX_ROOT_PATH` compat aliases) and a git credential-prompt guard. With `wait-for-setup`, the agent command is sequenced after the runner. Unapproved shared hooks are skipped without failing creation. CLI flag `--setup run|skip|inherit`. — [setup-hook-env-vars.ts](https://github.com/stablyai/orca/blob/main/src/main/setup-hook-env-vars.ts), [worktree-runner-script.ts](https://github.com/stablyai/orca/blob/main/src/main/worktree-runner-script.ts), [hooks.ts](https://github.com/stablyai/orca/blob/main/src/main/hooks.ts)
- Non-terminal hooks (`runHook`, used for archive): shell spawn, 120 s default timeout, 10 MiB output cap, SIGTERM then forced tree kill, detached process group on POSIX. — [hooks.ts](https://github.com/stablyai/orca/blob/main/src/main/hooks.ts)
- Removal order: re-list worktrees strictly and check locks; `assertWorktreeCleanForRemoval` unless forced; acquire a file-watcher removal gate; `stopPtysForDestructiveWorktreeRemoval`; remove linked shared paths; `git worktree remove [--force]` then delete leftovers (with `GIT_ASK_YESNO` stripped); branch delete; metadata purge and cache invalidation. — [remove-registered-local-worktree.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktrees/removal/remove-registered-local-worktree.ts), [worktree-removal.ts](https://github.com/stablyai/orca/blob/main/src/main/git/worktree-removal.ts)
- Branch delete uses `git branch -d` (safe). `-D` is used only for rollback of a failed creation. If git refuses, Orca checks whether the branch is already merged; otherwise it returns `preservedBranch` and the UI shows a "Review N Branches" toast. — [worktree-branch-removal.ts](https://github.com/stablyai/orca/blob/main/src/main/git/worktree-branch-removal.ts)
- Archive hook runs BEFORE PTY teardown. A failure throws `WorktreeArchiveHookFailedError` unless `allowFailure` is set (override recorded). CLI needs `--run-hooks`. — [worktree-archive-hook-gate.ts](https://github.com/stablyai/orca/blob/main/src/main/worktree-archive-hook-gate.ts)
- Removal can be backgrounded (`worktree-background-removal.ts`, returns `{removing: true}`); a pending-removal table blocks reuse of the path or branch. Orphan cleanup and Windows recovery paths exist. — [remove-registered-local-worktree.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktrees/removal/remove-registered-local-worktree.ts)
- Worktrees made outside Orca (plain `git worktree add`) show up as "external"/non-Orca worktrees; plain `git worktree remove` is detected on refresh. — [worktrees.mdx](https://www.onorca.dev/docs/model/worktrees)

### Inferences
- The retired-name registry plus pending-removal table suggest Orca hit races where a name or branch was reused while an async delete was still running. Not stated in code comments I read.
- `push.autoSetupRemote` is written to shared repo config (all worktrees of the repo share it), a side effect on the user's repo outside Orca.

### Gaps
- Default value of `settings.workspaceDir` not verified.
- `worktree-create-preparation.ts` internals (what exactly is pre-built) not read.
- Sleep/hibernate interplay with removal not traced in code.

## 2. How are agents launched and hosted (PTY, process model, prompt injection, restore)?

### Takeaway
Agents are ordinary CLI processes in a PTY. Orca keeps an agent catalog with per-agent launch and prompt-injection rules, assembles a rich env, and hosts `node-pty` in a separate long-lived daemon process so agents survive app quit, update and crash. Status comes from OSC titles plus managed per-agent hooks that call back into a local hook server.

### Cited Findings
- Agent catalog `TUI_AGENT_CONFIG` defines per agent `detectCmd`, `launchCmd`, `expectedProcess`, `promptInjectionMode`, draft-prompt flag or env, `preflightTrust`, ready signals for paste, Ctrl+Enter encoding. `promptInjectionMode` values seen: `argv`, `flag-prompt`, `flag-prompt-interactive`, `flag-interactive`, `stdin-after-start`, `hermes-query`. README lists ~35 agents plus "any CLI". — [tui-agent-config.ts](https://github.com/stablyai/orca/blob/main/src/shared/tui-agent-config.ts), [README](https://github.com/stablyai/orca/blob/main/README.md)
- `buildAgentStartupPlan` returns `{launchCommand, expectedProcess, followupPrompt, launchConfig, env, startupCommandDelivery}`; a `SleepingAgentLaunchConfig` is captured so the agent can be resumed later. — [tui-agent-startup.ts](https://github.com/stablyai/orca/blob/main/src/shared/tui-agent-startup.ts)
- Defaults: Claude `--dangerously-skip-permissions`, Codex `--dangerously-bypass-approvals-and-sandbox`, Gemini `--yolo` are pre-applied; editable under Settings, Agents with a Reset button. Some agents reject certain args (`UNSUPPORTED_TUI_AGENT_ARGS`, e.g. opencode/kilo reject `--dangerously-skip-permissions`). — [agents-sessions.mdx](https://www.onorca.dev/docs/model/agents-sessions), [tui-agent-launch-defaults.ts](https://github.com/stablyai/orca/blob/main/src/shared/tui-agent-launch-defaults.ts)
- Launch executor: `executeAgentLaunch` takes an `AgentLaunchIntent` with targets `existing`, `create-worktree`, `create-folder-workspace`, or reuse of a terminal. `decideAgentLaunchMode` picks `structured` (native chat session) vs `terminal`; a structured refusal downgrades to terminal. A workspace created for a launch is rolled back if the launch is cancelled before a surface is published. — [agent-launch-executor.ts](https://github.com/stablyai/orca/blob/main/src/main/agent-launch/agent-launch-executor.ts)
- Prompt delivery: via argv when the agent supports it, else a PTY write after the agent is ready. Dispositions are `handed-to-terminal`, `not-delivered`, `unconfirmed`, `journaled`; the code notes that a PTY write proves handoff, not consumption. A reused terminal always gets the prompt through the PTY. — [agent-launch-prompt-delivery.ts](https://github.com/stablyai/orca/blob/main/src/main/agent-launch/agent-launch-prompt-delivery.ts)
- PTY env assembly (`buildPtyHostEnv`) injects: proxy env, managed account env (Claude/Codex account switching), a git credential prompt guard (agents fail instead of prompting), agent hook server coordinates (e.g. `ORCA_AGENT_HOOK_ENDPOINT`), Pi/OMP extension overlays, OpenCode status plugin and `OPENCODE_CONFIG_DIR`, and a WSL hook relay. — [assembly.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/pty/host-env/assembly.ts)
- Process model: `node-pty` runs in a separate daemon process (`src/main/daemon/*`: `daemon-server.ts`, `session.ts`, `pty-subprocess.ts`). Client/server over a socket with NDJSON RPC (`createOrAttach`, `write`, `resize`, `kill`, `signal`, `listSessions`, `detach`, `getSnapshot`, `takePendingOutput`, `shutdown`, `pausePty`/`resumePty`); `PROTOCOL_VERSION` with a list of previous versions for upgrade. Each `Session` has an output pipeline (xterm-style emulator snapshot, scrollback), shell-ready barrier, termination controller. — [daemon/types.ts](https://github.com/stablyai/orca/blob/main/src/main/daemon/types.ts), [daemon/session.ts](https://github.com/stablyai/orca/blob/main/src/main/daemon/session.ts)
- Endpoint ownership rules for the daemon: bind `.p<hex>`, `link`, prove the incumbent dead, then `rename`; never `unlink`, no sweeper, never remove the endpoint on shutdown. — [daemon/AGENTS.md](https://github.com/stablyai/orca/blob/main/src/main/daemon/AGENTS.md)
- Restore: the daemon survives Cmd-Q, auto-update relaunch and app crash (warm reattach of live agent processes with scrollback including output produced while closed). Host reboot or daemon death loses processes, but layout and last persisted scrollback restore. Manage Sessions has a "Restart daemon" action that ends local terminals. — [session-restore.mdx](https://www.onorca.dev/docs/model/session-restore), [terminal.mdx](https://www.onorca.dev/docs/terminal)
- Status detection: OSC title sequences plus managed hooks per agent (Claude/Codex hook config, OpenCode plugin, Pi extension) feeding a local hook server (`src/main/agent-hooks/*`). States shown: working / needs-you / done / blocked / idle. Orca also pre-seeds workspace trust (`agent-workspace-trust*.ts`). — [agent-hooks/](https://github.com/stablyai/orca/tree/main/src/main/agent-hooks), [agents-sessions.mdx](https://www.onorca.dev/docs/model/agents-sessions)
- Hibernation (experimental, off by default): finished, idle, background agents (default 30 min, range 1 min to 24 h) are stopped and later resumed with the agent's own resume command (`claude --resume <id>`, `codex resume <id>`), reusing the captured launch command, args and private env. Allowed only for resumable agents (Claude, Codex, Gemini, Antigravity, OpenCode, Pi, MiMo, Droid, Grok, Devin, OMP); blocked while an orchestration Dispatch is unsettled or subagents are attached. Worktrees' panes hibernate as a unit. — [hibernation.mdx](https://www.onorca.dev/docs/agents/hibernation)
- A restart chip re-runs the same agent in the same cwd. — [agents-sessions.mdx](https://www.onorca.dev/docs/model/agents-sessions)

### Inferences
- The renderer likely uses xterm.js with WebGL (README says WebGL rendering); not verified in code.
- "Session restore" for agents after host reboot is effectively "relaunch with resume flags", which depends on each CLI's own transcript store; Orca does not persist model state itself.

### Gaps
- `runtime.createTerminal` internals and the exact renderer/main/daemon message path not traced.
- Per-agent hook installer details (files written into user config dirs) not read in full.
- `tui-agent-config.ts` read only to line 140.

## 3. State model and main/renderer split

### Takeaway
Main process owns state: an in-memory `Store` backed by a SQLite "profile state" database holding per-domain JSON documents with revisions and hashes. Renderer is a UI client over Electron IPC; the CLI, mobile and remote clients talk to a main-process runtime service over RPC.

### Cited Findings
- Source layout: `src/{main,preload,renderer,shared,cli,types,packages,wsl-guest}`, plus `mobile` and `cloud` (relay pairing mobile with desktop). — [repo tree](https://github.com/stablyai/orca/tree/main)
- `Store` holds in-memory `PersistedState`, writes are scheduled, and writable construction requires a SQLite authority. — [store.ts](https://github.com/stablyai/orca/blob/main/src/main/persistence/loading-store/store.ts)
- SQLite schema version 3: tables `profile_state_meta` (key/value incl. `profile_id`, `revision`, `legacy_json_acceptance`), `profile_state_documents` (domain PK, JSON payload, domain_version, revision, updated_at, content_hash) and automation-run tables. Revision conflicts raise `ProfileStateRevisionConflictError`; corruption raises `ProfileStateDocumentCorruptionError`; legacy JSON import/export exists. — [profile-state-database-schema.ts](https://github.com/stablyai/orca/blob/main/src/main/persistence/profile-state/profile-state-database-schema.ts), [profile-state-documents.ts](https://github.com/stablyai/orca/blob/main/src/main/persistence/profile-state/profile-state-documents.ts)
- Per-worktree metadata is keyed by a host-qualified worktree id; main owns workspace layouts and PTY bindings (switch `ORCA_LAYOUT_OWNER_IS_MAIN`); SSH PTY leases are persisted. — [persistence/](https://github.com/stablyai/orca/tree/main/src/main/persistence)
- `src/main/runtime/*` (`OrcaRuntimeService`) is the surface used by the `orca` CLI, mobile and remote clients. — [runtime/](https://github.com/stablyai/orca/tree/main/src/main/runtime)
- AGENTS.md requires every change to consider cross-platform, remote, mobile and non-git "folder workspaces". — [AGENTS.md](https://github.com/stablyai/orca/blob/main/AGENTS.md)

### Inferences
- Renderer is React with Zustand (inferred from a Zustand benchmark script in the tree).
- Documents-as-JSON-rows in SQLite is a migration path off a single JSON file, kept for atomic revision-checked writes and backup/quarantine.

### Gaps
- DB file name and location not verified.
- How much terminal scrollback lives in the DB vs files (`terminal-history*.ts`) not verified.

## 4. How does "fan one prompt across N agents" work mechanically?

### Takeaway
It is not a single composer action. The documented flow is manual: create N worktrees from the same start ref, pick a different agent in each, paste the same prompt. Scripted fan-out is done through `orca worktree create --agent --prompt` or the experimental `orca orchestration` command set.

### Cited Findings
- Docs describe `fix-login-race`, `-2`, `-3` worktrees each with a different agent and "Paste the same prompt into each"; losers are deleted with one click and their branches go with them. — [first-session.mdx](https://www.onorca.dev/docs/first-session), [parallel-agents.mdx](https://www.onorca.dev/docs/recipes/parallel-agents)
- The create-workspace composer has a single `quickAgent` combobox, not a multi-select. — [NewWorkspaceComposerAgentSection.tsx](https://github.com/stablyai/orca/blob/main/src/renderer/src/components/new-workspace/NewWorkspaceComposerAgentSection.tsx)
- CLI: `orca worktree create --name X --agent codex --prompt "..." --setup run|skip|inherit --json`. — [cli/reference.mdx](https://www.onorca.dev/docs/cli/reference)
- Experimental orchestration: Run, Task, Dispatch, Message, Decision gate, supervised workers. Commands include `run-create`, `task-create`, `worker-start --task <id> --worktree current|new-child|new-top-level --agent <id> [--model --effort --setup --on <host>]`, `check --wait`, `send --type worker_done`, `ask`, `gate-create/resolve`, `worker-read/release/retain/stop`; group addresses `@all`, `@idle`, `@claude`, `@codex`, `@worktree:<id>`; `orca terminal send` and `terminal wait --for tui-idle`. Workers get a preamble contract (send `worker_done` once with task and dispatch IDs, heartbeat, use `ask` for blocking questions). Legacy `orca orchestration run` is retired. — [orchestration.mdx](https://www.onorca.dev/docs/cli/orchestration)
- Worktree checkpoints: `worktree set --comment` and `--workspace-status`; parent/child lineage recorded on create; sidebar "Sleep/Delete with Descendants". — [worktree-checkpoints.mdx](https://www.onorca.dev/docs/cli/worktree-checkpoints)

### Inferences
- The "fan out" framing in marketing is a workflow pattern over cheap worktrees, not an engine feature; comparison (pick the winner) is human review of diffs, with no automated judge in the docs I read.

### Gaps
- `selectedAgents` usage in `WorktreeCardAgents.tsx` and a mobile hook was seen but not examined.
- Orchestration internals (dispatch store, circuit breaker) not read; only the docs.

## 5. SSH and remote worktrees

### Takeaway
Orca has four run modes (local, SSH, Remote Orca Server, cloud VM). For SSH it installs itself on the host and runs the worktree/terminal/agent logic there; the local create handler refuses SSH targets and delegates to a remote provider.

### Cited Findings
- Modes: local, SSH target, Remote Orca Server (desktop or `orca serve`, server owns projects, worktrees, terminals, agents), cloud VM per-workspace environments (`environmentRecipes`, experimental). — [ways-to-run.mdx](https://www.onorca.dev/docs/ways-to-run)
- A create request routed to SSH in the local handler throws `SshHostNotServedError`. Remote worktree path uses `computeRemoteWorktreePath`. — [worktree-remote.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-remote.ts), [worktree-logic.ts](https://github.com/stablyai/orca/blob/main/src/main/ipc/worktree-logic.ts)
- SSH stack: built-in `ssh2` client with system OpenSSH for GSSAPI/FIDO2, known_hosts verification, config import, port forwarding, SFTP; `managed-server` and `orcad` modules implement the managed remote daemon; a `legacy-relay` bridge exists. — [ssh/](https://github.com/stablyai/orca/tree/main/src/main/ssh), [orcad/](https://github.com/stablyai/orca/tree/main/src/main/orcad)
- Remote terminals need node-pty natives on the host; on Linux hosts without make, a C++ compiler and python3, files/git/editor work but terminals do not. Remote PTYs are leased via the relay and survive app close (default on; optional 60 s to 7 day bound). macOS 15+ hosts can lose local-network access after the SSH session that started the server ends. — [ssh.mdx](https://www.onorca.dev/docs/ssh)
- Hooks use a runtime-target abstraction so setup/archive can run on the host. — [hook-runtime-target.ts](https://github.com/stablyai/orca/blob/main/src/main/hook-runtime-target.ts)

### Inferences
- Remote worktrees go through the same lifecycle semantics via a remote provider that mirrors the local handlers; not confirmed by reading that provider.

### Gaps
- The orcad create path, install flow and protocol were not read. `remote-servers.mdx` and `recipes/remote-worktrees.mdx` were fetched but not read.

## 6. Known limitations, bugs and open issues

### Takeaway
Limits visible from docs and code: literal-only `.worktreeinclude`, whole-file rejection on a bad `orca.yaml`, daemon loss on reboot, experimental hibernation and orchestration, and permissive default agent flags. Open GitHub issues I sampled are mostly feature requests; I did not find worktree/PTY bugs in that sample, but the sample was shallow.

### Cited Findings
- `.worktreeinclude` supports literal paths only (globs/negation skipped) and has copy budgets. — [worktree-include-file.ts](https://github.com/stablyai/orca/blob/main/src/main/git/worktree-include-file.ts)
- `orca.yaml` with a duplicate key or parse error is rejected entirely. — [orca-yaml.mdx](https://www.onorca.dev/docs/model/orca-yaml)
- A feature branch's `.worktreeinclude`/shared-dir list does not affect the primary checkout; setup/archive scripts depend on the command-source policy. — [orca-yaml.mdx](https://www.onorca.dev/docs/model/orca-yaml)
- Native Windows setup runs as `.cmd` (Git Bash is opt-in). — [worktree-runner-script.ts](https://github.com/stablyai/orca/blob/main/src/main/worktree-runner-script.ts)
- Unmerged branches survive worktree deletion and need a manual "Review branches" step. — [worktree-branch-removal.ts](https://github.com/stablyai/orca/blob/main/src/main/git/worktree-branch-removal.ts)
- Host reboot or daemon death ends agent processes. — [session-restore.mdx](https://www.onorca.dev/docs/model/session-restore)
- Hibernation is experimental, off by default and limited to resumable agents. — [hibernation.mdx](https://www.onorca.dev/docs/agents/hibernation)
- Orchestration is documented as experimental. — [orchestration.mdx](https://www.onorca.dev/docs/cli/orchestration)
- Full-autonomy permission flags are pre-applied for Claude/Codex/Gemini (a security trade-off). — [agents-sessions.mdx](https://www.onorca.dev/docs/model/agents-sessions)
- Code comments reference platform quirks: OpenCode paste readiness on Windows ConPTY (#22479), OpenCode CSI-u Ctrl+Enter (#25372), Claude Ctrl+Enter on ConPTY (#29711). — [tui-agent-config.ts](https://github.com/stablyai/orca/blob/main/src/shared/tui-agent-config.ts)
- Sampled open issues (feature requests): [#8377 multi-worktree parallel view](https://github.com/stablyai/orca/issues/8377), [#13665 Docker sandboxes for agents](https://github.com/stablyai/orca/issues/13665), [#6074 multiple windows](https://github.com/stablyai/orca/issues/6074), [#1099 multi-repo workspaces](https://github.com/stablyai/orca/issues/1099), [#7568 multi-repo project groups](https://github.com/stablyai/orca/issues/7568), [#1082 Jujutsu workspaces](https://github.com/stablyai/orca/issues/1082), [#11557 multiple Claude subscriptions](https://github.com/stablyai/orca/issues/11557)

### Inferences
- #13665 (Docker sandboxes) plus the default skip-permission flags indicate agents run unsandboxed on the host; isolation is git-level only.
- #8377 suggests the side-by-side view of parallel worktrees is not satisfying users yet.

### Gaps
- Bug-labelled and closed issues, recent regressions and PR history were not surveyed. Only two search queries over open issues were run (sorted by reactions).
- No measurements of worktree-create latency or memory per agent.
