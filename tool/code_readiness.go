package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

var watchPIDRe = regexp.MustCompile(`pid (\d+)`)

// watchStatus reads `jbcontext watch status`: whether the daemon runs and the
// roots it watches.
func (e *codeEnv) watchStatus() (line, pid string, roots []string) {
	ctx, cancel := context.WithTimeout(context.Background(), jbStatusLimit)
	defer cancel()
	out, err := exec.CommandContext(ctx, e.JB, "watch", "status").Output()
	if err != nil {
		return "unavailable", "", nil
	}
	for i, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		l = strings.TrimSpace(l)
		if i == 0 {
			line = l
			if m := watchPIDRe.FindStringSubmatch(l); m != nil {
				pid = m[1]
			}
			continue
		}
		if f := strings.Fields(l); len(f) >= 2 && strings.HasPrefix(f[len(f)-1], "/") {
			roots = append(roots, f[len(f)-1])
		}
	}
	return line, pid, roots
}

func which(bin string) string {
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	return ""
}

// retrievalReadiness is the one-line state of the retrieval layer for `orch doctor`:
// the index, the watch and the tools, and what to run when something is off.
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
			parts = append(parts, "index stale")
			fixes = append(fixes, "jbcontext index --project-path "+e.Code)
		}
		_, _, roots := e.watchStatus()
		watched := false
		for _, r := range roots {
			watched = watched || evalOr(r) == evalOr(e.Code)
		}
		if watched {
			parts = append(parts, "watch on")
		} else {
			parts = append(parts, "watch off")
			fixes = append(fixes, "jbcontext watch add --project-path "+e.Code)
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
