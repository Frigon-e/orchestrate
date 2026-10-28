package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ctxEnvFor is the env of the checkout the command runs in. Work marked
// private (the lead's task.md) never uses jbcontext.
func codeEnvFor(a *app) (*codeEnv, error) {
	cwd := a.cwd
	if ctxReadOnly() {
		root, err := ctxPinnedRoot()
		if err != nil {
			return nil, err
		}
		cwd = root
	}
	top, err := git(cwd, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return nil, fail("not inside a git repository: %s", a.cwd)
	}
	cfg, err := loadConfig(top)
	if err != nil {
		return nil, err
	}
	private := false
	if dir := os.Getenv("ORCH_TASK_DIR"); dir != "" {
		if fm, _, err := readTask(filepath.Join(dir, "task.md")); err == nil && fm["private"] == "true" {
			private = true
		}
	}
	return newCodeEnv(top, codeOpts{Cfg: cfg, Private: private}), nil
}

func which(bin string) string {
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	return ""
}

// retrievalReadiness is the one-line state of the retrieval layer for `orch doctor`:
// the index and the tools, and what to run when something is off. A stale index
// is normal for up to a prompt: orch-guard refreshes it in the background.
func retrievalReadiness(e *codeEnv) string {
	var parts, fixes []string
	switch {
	case e.NoJB:
		parts = append(parts, "jbcontext off ("+e.NoJBWhy+")")
	case which(e.JB) == "":
		parts = append(parts, "jbcontext missing")
	default:
		head, _ := git(e.Code, "rev-parse", "HEAD")
		revs, ok := e.jbIndexed()
		fresh := false
		for _, r := range revs {
			fresh = fresh || r == head
		}
		switch {
		case !ok:
			parts = append(parts, "index status unavailable")
		case fresh:
			parts = append(parts, "index fresh")
		case len(revs) == 0:
			parts = append(parts, "no index")
			fixes = append(fixes, "jbcontext index --project-path "+e.Code)
		default:
			parts = append(parts, "index behind HEAD")
		}
	}
	for _, t := range []struct {
		name string
		ok   bool
	}{{"gopls", which(e.Gopls) != ""}, {"ts server", tsServerPath(e.Code) != ""}, {"rg", which(e.RG) != ""}} {
		if t.ok {
			parts = append(parts, t.name+" ok")
		} else {
			parts = append(parts, t.name+" missing")
		}
	}
	if _, why := tsServerChoice(e.Code); why != "" {
		parts = append(parts, why)
		e.note(why)
	}
	line := "retrieval: " + strings.Join(parts, ", ")
	if len(fixes) > 0 {
		line += " (run: " + strings.Join(fixes, "; ") + ")"
	}
	if sts := e.langStatuses(); len(sts) > 0 {
		var ls []string
		for _, s := range sts {
			ls = append(ls, s.String())
		}
		line += "\nlanguages: " + strings.Join(ls, "; ")
	}
	return line
}

// tsServerPath reports availability using the same executable check as
// langStatuses. A configured path may exist without being a usable server.
func tsServerPath(code string) string {
	if p := tslsBin(code); p != "" {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	return ""
}
