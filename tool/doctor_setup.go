package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// setupFindings checks the machine setup a lead run depends on: installed
// links, hook scripts, tools and Claude trust. It only reads.
func setupFindings(home, repo, root string, lookPath func(string) (string, error)) []finding {
	var fs []finding
	add := func(sev, where, msg, fix string) { fs = append(fs, finding{sev, where, msg, fix}) }
	inHerdr := os.Getenv("HERDR_ENV") == "1"

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
	const hookFix = "python3 ~/.agents/skills/orchestrate/hooks/claude/install_settings.py --apply"
	if b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err != nil {
		if os.IsNotExist(err) {
			add("warn", "setup", "~/.claude/settings.json is missing", hookFix)
		} else {
			add("warn", "setup", "settings.json not readable", "check ~/.claude/settings.json")
		}
	} else {
		var settings struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
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
								add("warn", "setup", msg, filepath.Join(filepath.Dir(script), "..", "..", "scripts", "build-guard"))
							}
						}
					}
				}
			}
			if !found {
				add("warn", "setup", "no orchestrate hook is registered in settings.json", hookFix)
			}
		}
	}

	// 4. tools.
	if _, err := lookPath("git"); err != nil {
		add("error", "setup", "git is not on PATH", "install git")
	}
	if _, err := lookPath("go"); err != nil {
		add("warn", "setup", "go is not on PATH (orch builds itself with Go)", "install Go")
	}
	if inHerdr {
		if _, err := lookPath("herdr"); err != nil {
			add("error", "setup", "herdr is not on PATH", "install Herdr or fix PATH")
		}
	}

	// 5. Claude trust for the worktrees dir.
	if inHerdr {
		if b, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
			var cj struct {
				Projects map[string]struct {
					Trusted bool `json:"hasTrustDialogAccepted"`
				} `json:"projects"`
			}
			dir := filepath.Dir(root)
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
