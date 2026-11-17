package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// setupFindings checks the machine setup a lead run depends on: installed
// links, hook scripts, tools and Claude trust. It only reads.
func setupFindings(home, repo, root string, lookPath func(string) (string, error)) []finding {
	var fs []finding
	add := func(sev, where, msg, fix string) { fs = append(fs, finding{sev, where, msg, fix}) }
	backend := detectBackendName(os.Environ())

	// 1. orch on PATH.
	orchBin := filepath.Join(home, ".local", "bin", "orch")
	if _, err := os.Lstat(orchBin); err != nil {
		add("warn", "setup", "~/.local/bin/orch is missing", "ln -s ~/.agents/skills/orchestrate/scripts/orch ~/.local/bin/orch")
	} else if _, err := os.Stat(orchBin); err != nil {
		add("error", "setup", "~/.local/bin/orch is a dangling link", "relink it to scripts/orch")
	}

	// 2. skill links.
	for _, rel := range []string{".agents/skills/orchestrate", ".claude/skills/orchestrate"} {
		p := filepath.Join(home, filepath.FromSlash(rel))
		if _, err := os.Lstat(p); err != nil {
			add("warn", "setup", "~/"+rel+" is missing", "link ~/"+rel+" to the orchestrate skill directory")
		} else if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err != nil {
			add("error", "setup", "~/"+rel+" is a dangling link or has no SKILL.md", "relink ~/"+rel+" to the orchestrate skill directory")
		}
	}

	// 3. hooks registered in Claude settings.
	const hookFix = "scripts/install-guards"
	if b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err != nil {
		if os.IsNotExist(err) {
			add("warn", "setup", "~/.claude/settings.json is missing", hookFix)
		} else {
			add("warn", "setup", "settings.json not readable", "check ~/.claude/settings.json")
		}
	} else {
		var settings claudeHooks
		if err := json.Unmarshal(b, &settings); err != nil {
			add("warn", "setup", "settings.json not readable", "check ~/.claude/settings.json")
		} else {
			found := false
			for _, groups := range settings.Hooks {
				for _, g := range groups {
					for _, h := range g.Hooks {
						if !strings.Contains(h.Command, "skills/orchestrate/hooks/") {
							continue
						}
						found = true
						script := hookScript(h.Command, home)
						if fi, err := os.Stat(script); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
							add("error", "setup", "hook script missing or not executable: "+script, hookFix)
						} else if filepath.Base(script) == "orch-guard" {
							if msg := staleGuard(script); msg != "" {
								add("warn", "setup", msg, hookFix)
							}
						}
					}
				}
			}
			if !found {
				add("warn", "setup", "no orchestrate hook is registered in settings.json", hookFix)
			} else {
				for _, h := range unregisteredHooks(settings, filepath.Join(home, ".agents", "skills", "orchestrate", "hooks", "claude", "settings-fragment.json")) {
					add("warn", "setup", "Claude hook not registered in settings.json: "+h, hookFix)
				}
			}
		}
	}

	// 3b. the pi guard extension, a link to this skill's tool/pi_ctx_reminder.ts.
	piExt := filepath.Join(home, ".pi", "agent", "extensions", "ctx-reminder.ts")
	if fi, err := os.Lstat(piExt); err != nil {
		add("warn", "setup", "pi tool guard extension is missing: ~/.pi/agent/extensions/ctx-reminder.ts", hookFix)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		add("warn", "setup", "~/.pi/agent/extensions/ctx-reminder.ts is a regular file, not a link to the skill", hookFix)
	} else if got, err := filepath.EvalSymlinks(piExt); err != nil {
		add("error", "setup", "~/.pi/agent/extensions/ctx-reminder.ts is a dangling link", hookFix)
	} else if want, err := filepath.EvalSymlinks(filepath.Join(home, ".agents", "skills", "orchestrate", "tool", "pi_ctx_reminder.ts")); err == nil && got != want {
		add("warn", "setup", "~/.pi/agent/extensions/ctx-reminder.ts points at "+got+", not at this skill's "+want, hookFix)
	}

	// 3c. the orch MCP server registered for Claude Code (claude mcp add --scope user).
	// Only ~/.claude.json is read; claude itself is not run.
	const mcpFix = "scripts/install-guards"
	var claudeCfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if b, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		_ = json.Unmarshal(b, &claudeCfg)
	}
	if srv, ok := claudeCfg.MCPServers["orch"]; !ok {
		add("warn", "setup", "orch MCP server is not registered for Claude Code (run scripts/install-guards)", mcpFix)
	} else if fi, err := os.Stat(srv.Command); filepath.Base(srv.Command) != "orch" || err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		add("warn", "setup", "orch MCP server for Claude Code points elsewhere: "+srv.Command+" is not an orch executable (run scripts/install-guards)", mcpFix)
	}

	// 4. tools.
	if _, err := lookPath("git"); err != nil {
		add("error", "setup", "git is not on PATH", "install git")
	}
	if _, err := lookPath("go"); err != nil {
		add("warn", "setup", "go is not on PATH (orch builds itself with Go)", "install Go")
	}
	switch backend {
	case "herdr":
		if _, err := lookPath("herdr"); err != nil {
			add("error", "setup", "herdr is not on PATH", "install Herdr or fix PATH")
		}
	case "orca":
		bin := envOr("ORCH_ORCA", "orca")
		if _, err := lookPath(bin); err != nil {
			add("error", "setup", "orca is not on PATH", "install Orca or set ORCH_ORCA")
		} else if _, err := runClaudeAuth(bin, "status", "--json"); err != nil {
			add("warn", "setup", "orca status did not answer", "start the Orca app, then run: orca status")
		}
	case "":
		add("warn", "setup", "no multiplexer backend detected (herdr and Orca checks skipped)", "run inside a multiplexer or set ORCH_BACKEND=<name>")
	}

	// 5. Claude trust for the worktrees dir.
	if trustDir := backendTrustDir(backend, root); trustDir != "" {
		if b, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
			var cj struct {
				Projects map[string]struct {
					Trusted bool `json:"hasTrustDialogAccepted"`
				} `json:"projects"`
			}
			dir := trustDir
			if json.Unmarshal(b, &cj) == nil && !cj.Projects[dir].Trusted {
				add("warn", "setup", "Claude stops at its trust prompt in new lead worktrees ("+dir+")",
					"set projects["+dir+"].hasTrustDialogAccepted to true in ~/.claude.json (a parent directory's entry is not inherited)")
			}
		}
	}

	// 6. run root owner.
	if owner := rootOwner(root); owner != "" {
		a, errA := filepath.EvalSymlinks(owner)
		if errA != nil {
			a = owner
		}
		b, errB := filepath.EvalSymlinks(repo)
		if errB != nil {
			b = repo
		}
		if a != b {
			add("warn", "setup", fmt.Sprintf("run root %s belongs to %s, not this repo", root, owner), "use ORCH_HOME or rename one of the repos so the roots differ")
		}
	}

	// 7. prunable worktrees.
	if out, err := git(repo, "worktree", "list", "--porcelain"); err == nil {
		for _, block := range strings.Split(out, "\n\n") {
			path := ""
			prunable := false
			for _, line := range strings.Split(block, "\n") {
				if p, ok := strings.CutPrefix(line, "worktree "); ok {
					path = p
				}
				if line == "prunable" || strings.HasPrefix(line, "prunable ") {
					prunable = true
				}
			}
			if prunable {
				add("warn", "setup", "prunable worktree entry: "+path, "git worktree prune")
			}
		}
	}
	return fs
}

// claudeHooks is the hooks key of a Claude Code settings.json.
type claudeHooks struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// unregisteredHooks lists each hook of the fragment at path (what
// install-guards registers) that settings lack, as "<event> <command>", the
// way install_settings.py --check matches them: by event, matcher and command.
// A missing or unreadable fragment lists nothing.
func unregisteredHooks(settings claudeHooks, path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var fragment claudeHooks
	if json.Unmarshal(b, &fragment) != nil {
		return nil
	}
	type key struct{ event, matcher, command string }
	have := map[key]bool{}
	for event, groups := range settings.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				have[key{event, g.Matcher, h.Command}] = true
			}
		}
	}
	var out []string
	for _, event := range slices.Sorted(maps.Keys(fragment.Hooks)) {
		for _, g := range fragment.Hooks[event] {
			for _, h := range g.Hooks {
				if !have[key{event, g.Matcher, h.Command}] {
					out = append(out, event+" "+h.Command)
				}
			}
		}
	}
	return out
}

// detectBackendName is the backend selectBackend would pick for env, by name,
// without building it: ORCH_BACKEND, else the first registered backend whose
// detect matches, else "".
func detectBackendName(env []string) string {
	getenv := envGetter(env)
	regs := registeredBackends()
	if want := getenv("ORCH_BACKEND"); want != "" {
		for _, r := range regs {
			if r.name == want {
				return want
			}
		}
		return ""
	}
	for _, r := range regs {
		if r.detect(getenv) {
			return r.name
		}
	}
	return ""
}

// backendTrustDir is the directory whose Claude trust entry lead worktrees
// need under backend, or "" when there is nothing to check. Herdr keeps
// worktrees next to the run root. Orca needs none: it launches Claude leads
// with --dangerously-skip-permissions (orcaLaunchArgv), which skips the prompt,
// unless claude_args set their own permission mode.
func backendTrustDir(backend, root string) string {
	if backend == "herdr" {
		return filepath.Dir(root)
	}
	return ""
}

// claudeAuthFindings warns when `<bin> auth status` says Claude Code is not
// signed in. Only the loggedIn field is read; nothing else is kept or shown.
func claudeAuthFindings(bin string, run func(name string, args ...string) ([]byte, error)) []finding {
	const fix = "run: claude auth login"
	out, err := run(bin, "auth", "status")
	if err != nil {
		reason := "command error"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "timed out"
		}
		return []finding{{"warn", "setup", "claude auth status failed: " + reason, fix}}
	}
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(out, &st) != nil || st.LoggedIn == nil {
		return []finding{{"warn", "setup", "claude auth status gave unreadable output", "run: claude auth status"}}
	}
	if !*st.LoggedIn {
		return []finding{{"warn", "setup", "claude is not signed in", fix}}
	}
	return nil
}

// runClaudeAuth runs a command for claudeAuthFindings with a 5 second limit
// and returns stdout only.
func runClaudeAuth(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}

// staleGuard says why hooks/bin/orch-guard is out of date, or "" when it is current
// or cannot be compared. The binary reports the fingerprint of the sources it was
// built from; scripts/build-guard prints the fingerprint of the sources now.
func staleGuard(bin string) string {
	build := filepath.Join(filepath.Dir(bin), "..", "..", "scripts", "build-guard")
	want, err := exec.Command(build, "--print-fingerprint").Output()
	if err != nil {
		return ""
	}
	got, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "hook binary does not run: " + bin
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		return "hook binary is stale, built from older sources: " + bin
	}
	return ""
}

// hookScript extracts the script path from a hook command.
func hookScript(cmd, home string) string {
	for _, f := range strings.Fields(cmd) {
		f = strings.Trim(f, `"'`)
		if !strings.Contains(f, "skills/orchestrate/hooks/") {
			continue
		}
		f = strings.ReplaceAll(f, "${HOME}", home)
		f = strings.ReplaceAll(f, "$HOME", home)
		if f == "~" || strings.HasPrefix(f, "~/") {
			f = home + f[1:]
		}
		return f
	}
	return cmd
}
