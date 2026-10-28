package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// subsInLeadTab helpers stack in the lead's own tab; more open overflow
	// tabs in the same workspace, subsPerTab to a 2x2 grid.
	subsInLeadTab = 3
	subsPerTab    = 4
)

var subNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,11}$`)

// subAgent is a helper a ship lead started in an extra pane of its own
// worktree workspace. It has no task dir of its own and does not count
// against max_leads.
type subAgent struct {
	Name      string `json:"name"`
	Harness   string `json:"harness"`
	Model     string `json:"model"`
	PaneID    string `json:"pane_id"`
	TabID     string `json:"tab_id,omitempty"`
	StartedAt int64  `json:"started_at"`
	ClosedAt  int64  `json:"closed_at,omitempty"`
}

func openSubs(l *lead) []subAgent {
	var out []subAgent
	for _, s := range l.Subs {
		if s.ClosedAt == 0 {
			out = append(out, s)
		}
	}
	return out
}

// placeSub picks the pane for a new helper from the open helpers only, so a
// closed one frees its slot. Up to subsInLeadTab stack in the lead's tab; the
// rest fill 2x2 grids in overflow tabs of the same workspace. A slot whose
// split target pane is gone is skipped for the next valid one.
func placeSub(l *lead, wt string, env []string) (pane, tab string, err error) {
	var split struct {
		Result struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	splitAt := func(target, direction, tabID string) (string, string, bool, error) {
		ok, err := paneExists(target)
		if err != nil || !ok {
			return "", "", false, err
		}
		cmd := append([]string{"pane", "split", target, "--direction", direction, "--cwd", wt, "--no-focus"}, envArgs(env)...)
		if err := herdrJSON(&split, herdrTimeout, cmd...); err != nil {
			return "", "", false, err
		}
		return split.Result.Pane.PaneID, tabID, true, nil
	}
	byTab := map[string][]string{}
	var overflow []string
	allTabs := map[string]bool{}
	for _, s := range l.Subs {
		if s.TabID != "" && s.TabID != l.TabID {
			allTabs[s.TabID] = true
		}
	}
	for _, s := range openSubs(l) {
		t := s.TabID
		if t == "" {
			t = l.TabID
		}
		if t != l.TabID && len(byTab[t]) == 0 {
			overflow = append(overflow, t)
		}
		byTab[t] = append(byTab[t], s.PaneID)
	}
	if lead := byTab[l.TabID]; len(lead) < subsInLeadTab {
		target, direction := l.PaneID, "right"
		if len(lead) > 0 {
			target, direction = lead[len(lead)-1], "down"
		}
		if p, t, ok, err := splitAt(target, direction, l.TabID); err != nil || ok {
			return p, t, err
		}
		if len(lead) > 0 { // the previous helper's pane is gone: fall back to the lead pane
			if p, t, ok, err := splitAt(l.PaneID, "right", l.TabID); err != nil || ok {
				return p, t, err
			}
		}
	}
	for _, t := range overflow {
		panes := byTab[t]
		if len(panes) >= subsPerTab {
			continue
		}
		target, direction := panes[0], "right"
		switch len(panes) {
		case 2:
			direction = "down"
		case 3:
			target, direction = panes[1], "down"
		}
		if p, tt, ok, err := splitAt(target, direction, t); err != nil || ok {
			return p, tt, err
		}
	}
	label := l.ID + "-helpers"
	if n := len(allTabs); n > 0 {
		label = fmt.Sprintf("%s-%d", label, n+1)
	}
	var res struct {
		Result struct {
			Tab struct {
				TabID string `json:"tab_id"`
			} `json:"tab"`
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	create := append([]string{"tab", "create", "--workspace", l.WorkspaceID, "--cwd", wt, "--label", label, "--no-focus"}, envArgs(env)...)
	if err := herdrJSON(&res, herdrTimeout, create...); err != nil {
		return "", "", err
	}
	return res.Result.RootPane.PaneID, res.Result.Tab.TabID, nil
}

// subAgentName is the Herdr agent name of a helper: [a-z][a-z0-9_-]{0,31}.
func subAgentName(leadID, name string) string {
	id := leadID
	if len(id) > 15 {
		id = id[:15]
	}
	return "sub-" + id + "-" + name
}

// subLead loads the open lead the calling pane belongs to.
func (a *app) subLead() (dir string, l *lead, err error) {
	if os.Getenv("ORCH_ROLE") != "lead" || os.Getenv("ORCH_TASK_DIR") == "" {
		return "", nil, fail("orch sub runs inside a lead's pane (ORCH_ROLE=lead and ORCH_TASK_DIR)")
	}
	if os.Getenv("HERDR_ENV") != "1" {
		return "", nil, fail("orch sub must run inside a Herdr pane (HERDR_ENV=1)")
	}
	dir, err = leadDir("")
	if err != nil {
		return "", nil, err
	}
	l, err = loadLead(dir)
	if err != nil {
		return "", nil, err
	}
	if l == nil || l.ClosedAt != 0 {
		return "", nil, fail("this lead is not open")
	}
	if epoch := os.Getenv("ORCH_EPOCH"); epoch != "" && l.Epoch != "" && l.Epoch != epoch {
		return "", nil, fail("this lead session was replaced by a respawn; stop working")
	}
	return dir, l, nil
}

func cmdSub(a *app, args []string) error {
	if len(args) == 0 {
		return &exitError{code: 2, msg: "usage: orch sub start|list|read|close ..."}
	}
	switch args[0] {
	case "start":
		return a.subStart(args[1:])
	case "list":
		return a.subList(args[1:])
	case "read":
		return a.subRead(args[1:])
	case "close":
		return a.subClose(args[1:])
	}
	return &exitError{code: 2, msg: "usage: orch sub start|list|read|close ..."}
}

func (a *app) subStart(args []string) error {
	fs := a.flags("sub start")
	harness := fs.String("harness", "", "claude or pi")
	model := fs.String("model", "", "model for the helper")
	effort := fs.String("effort", "", "effort (default: the task's effort, or high)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || (*harness != "claude" && *harness != "pi") || *model == "" {
		return &exitError{code: 2, msg: `usage: orch sub start <name> --harness claude|pi --model M [--effort E] "<prompt>"`}
	}
	name, prompt := pos[0], strings.TrimSpace(pos[1])
	if !subNameRe.MatchString(name) {
		return fail("helper name %q must match [a-z][a-z0-9-]{0,11}", name)
	}
	if prompt == "" {
		return fail("a helper needs a prompt")
	}
	dir, l, err := a.subLead()
	if err != nil {
		return err
	}
	if l.WorkspaceID == "" {
		return fail("helpers need a managed ship worktree; %s has none (scouts and leads outside Herdr cannot start them)", l.ID)
	}
	open := openSubs(l)
	for _, s := range open {
		if s.Name == name {
			return fail("helper %s is already open", name)
		}
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	fm, _, err := readTask(filepath.Join(dir, "task.md"))
	if err != nil {
		return err
	}
	if err := harnessUsable(s.cfg, *harness, *model, fm["degraded"] == "true", a.now()); err != nil {
		return err
	}
	if fm["private"] == "true" && *harness != "claude" {
		return fail("%s is marked private, so helpers may only run on Claude, not %s", l.ID, *harness)
	}
	eff := *effort
	if eff == "" {
		eff = fm["effort"]
	}
	if eff == "" {
		eff = "high"
	}
	wt := worktreePath(dir, l.ID)
	env := []string{"ORCH_ROLE=sub", "ORCH_PARENT=" + l.ID}
	pane, tab, err := placeSub(l, wt, env)
	if err != nil {
		return err
	}
	agent := subAgentName(l.ID, name)
	_, _ = herdrCmd(herdrTimeout, "pane", "rename", pane, agent)
	sub := subAgent{Name: name, Harness: *harness, Model: *model, PaneID: pane, TabID: tab, StartedAt: a.now().Unix()}
	if err := withFlock(filepath.Join(dir, ".subs.guard"), func() error {
		cur, err := loadLead(dir)
		if err != nil || cur == nil {
			return fail("lead.json vanished")
		}
		cur.Subs = append(cur.Subs, sub)
		return saveLead(dir, cur)
	}); err != nil {
		_, _ = herdrCmd(herdrTimeout, "pane", "close", pane)
		return err
	}
	start := append([]string{"agent", "start", agent, "--kind", *harness, "--pane", pane, "--timeout", "90000", "--"},
		harnessArgs(map[string]string{"harness": *harness, "model": *model, "effort": eff}, s.cfg, "")...)
	if err := startAgent(start); err != nil {
		return fail("opened pane %s for helper %s, but the agent did not start (%v); remove it with: orch sub close %s", pane, name, err, name)
	}
	scope := "the lead's scope"
	if owns := splitList(fm["owns"]); len(owns) > 0 {
		scope = strings.Join(owns, ", ")
	}
	header := fmt.Sprintf("You are helper %s for lead %s, working in the lead's checkout at %s. Edit only files inside the lead's owned paths: %s. "+
		"Do not commit, do not run orch commands or start agents. When finished, say so and summarize in your last message because the lead reads your pane.\n\n",
		name, l.ID, wt, scope)
	if _, err := herdrCmd(herdrTimeout, "agent", "prompt", pane, header+prompt); err != nil {
		return fail("helper %s started in pane %s, but the prompt failed (%v)", name, pane, err)
	}
	fmt.Fprintln(a.out, agent)
	return nil
}

func (a *app) subList(args []string) error {
	fs := a.flags("sub list")
	all := fs.Bool("all", false, "include closed helpers")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	_, l, err := a.subLead()
	if err != nil {
		return err
	}
	agents, agentsErr := agentMap()
	n := 0
	for _, s := range l.Subs {
		if s.ClosedAt != 0 && !*all {
			continue
		}
		n++
		status := "closed"
		if s.ClosedAt == 0 {
			switch ag, ok := agents[s.PaneID]; {
			case agentsErr != nil:
				status = "?"
			case !ok:
				status = "gone"
			default:
				status = ag.Status
			}
		}
		fmt.Fprintf(a.out, "%s  %s/%s  %s  %s\n", s.Name, s.Harness, s.Model, status, s.PaneID)
	}
	if n == 0 {
		fmt.Fprintln(a.out, "no helpers")
	}
	return nil
}

// findSub returns the index of the open helper called name.
func findSub(l *lead, name string) (int, error) {
	for i, s := range l.Subs {
		if s.Name == name && s.ClosedAt == 0 {
			return i, nil
		}
	}
	return -1, fail("no open helper %q", name)
}

func (a *app) subRead(args []string) error {
	fs := a.flags("sub read")
	n := fs.Int("lines", 80, "lines to show")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "sub read <name> [--lines N]"); err != nil {
		return err
	}
	_, l, err := a.subLead()
	if err != nil {
		return err
	}
	if _, err := findSub(l, pos[0]); err != nil {
		return err
	}
	out, err := herdrCmd(herdrTimeout, "agent", "read", subAgentName(l.ID, pos[0]), "--source", "recent-unwrapped", "--lines", strconv.Itoa(max(*n, 200)))
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	fmt.Fprintln(a.out, strings.Join(lines[max(0, len(lines)-*n):], "\n"))
	return nil
}

func (a *app) subClose(args []string) error {
	if err := exactArgs(args, 1, "sub close <name>"); err != nil {
		return err
	}
	dir, l, err := a.subLead()
	if err != nil {
		return err
	}
	i, err := findSub(l, args[0])
	if err != nil {
		return err
	}
	if err := closeSubPane(l.Subs[i].PaneID); err != nil {
		return err
	}
	return withFlock(filepath.Join(dir, ".subs.guard"), func() error {
		cur, err := loadLead(dir)
		if err != nil || cur == nil {
			return fail("lead.json vanished")
		}
		if j, err := findSub(cur, args[0]); err == nil {
			cur.Subs[j].ClosedAt = a.now().Unix()
		}
		if err := saveLead(dir, cur); err != nil {
			return err
		}
		fmt.Fprintln(a.out, "closed helper "+args[0])
		return nil
	})
}

// closeSubPane closes a helper's pane; a pane already gone is fine.
func closeSubPane(pane string) error {
	exists, err := paneExists(pane)
	if err != nil {
		return err
	}
	if exists {
		_, _ = herdrCmd(herdrTimeout, "pane", "close", pane)
		if still, err := paneExists(pane); err != nil || still {
			return fail("pane %s is still open after close", pane)
		}
	}
	return nil
}

// closeSubs closes every open helper pane of l before the lead's own pane
// goes, marks them closed and logs how many.
func (a *app) closeSubs(dir string, l *lead) error {
	n := 0
	for i := range l.Subs {
		if l.Subs[i].ClosedAt != 0 {
			continue
		}
		if err := closeSubPane(l.Subs[i].PaneID); err != nil {
			return err
		}
		l.Subs[i].ClosedAt = a.now().Unix()
		n++
	}
	if n == 0 {
		return nil
	}
	if err := saveLead(dir, l); err != nil {
		return err
	}
	return a.appendStatus(dir, "note", fmt.Sprintf("closed %d helper panes", n), "", "orch")
}
