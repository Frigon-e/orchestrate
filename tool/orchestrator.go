package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const panesPerTab = 4

func cmdNew(a *app, args []string) error {
	fs := a.flags("new")
	kind := fs.String("kind", "", "ship or scout")
	harness := fs.String("harness", "", "claude or pi")
	model := fs.String("model", "", "model id or alias, or `default`")
	effort := fs.String("effort", "", "effort level, or `default`")
	base := fs.String("base", "", "base branch (default: repo config, else the current branch)")
	deps := fs.String("depends", "", "comma-separated task ids that must be done first")
	degraded := fs.Bool("degraded", false, "allow a degraded fallback profile (ask the user first)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "new <id> --kind ship|scout --harness claude|pi --model M --effort E"); err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	var depList []string
	for _, d := range strings.Split(*deps, ",") {
		if d = strings.TrimSpace(d); d != "" {
			depList = append(depList, d)
		}
	}
	path, err := newTask(s, taskOpts{id: pos[0], kind: *kind, harness: *harness, model: *model, effort: *effort, base: *base, deps: depList, degraded: *degraded})
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, path)
	return nil
}

// taskOpts describes one task.md to scaffold. fill replaces the body of the
// named `# Section` of the template; spec, when set, adds a `spec:` line.
type taskOpts struct {
	id, kind, harness, model, effort, base string
	deps                                   []string
	fill                                   map[string]string
	spec                                   string
	owns                                   []string
	degraded                               bool
	integration, specHash                  string // set by orch plan
}

// newTask scaffolds task.md for one task and returns its path.
func newTask(s site, o taskOpts) (string, error) {
	switch {
	case o.kind != "ship" && o.kind != "scout":
		return "", fail("--kind must be ship or scout")
	case o.harness == "auto" && (o.model != "auto" || o.effort != "auto"):
		return "", fail("--harness auto needs --model auto and --effort auto")
	case o.harness != "claude" && o.harness != "pi" && o.harness != "auto":
		return "", fail("--harness must be claude or pi (or auto, to let `orch spawn --auto` choose)")
	case o.model == "" || o.effort == "":
		return "", fail("--model and --effort are required (use `default` to inherit)")
	}
	if err := harnessUsable(s.cfg, o.harness, o.model, o.degraded, time.Now()); err != nil {
		return "", err
	}
	id := o.id
	dir, err := taskDir(s.root, id)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "task.md")
	if _, err := os.Stat(path); err == nil {
		return "", fail("%s already exists; edit it instead", path)
	}
	baseBranch := s.cfg.BaseBranch
	if o.base != "" {
		baseBranch = o.base
	}
	if strings.HasPrefix(baseBranch, "-") {
		return "", fail("base %q is not a branch or commit", baseBranch)
	}
	sha, err := git(s.repo, "rev-parse", "--verify", "--quiet", "--short=12", baseBranch+"^{commit}")
	if err != nil || sha == "" {
		return "", fail("base %q does not resolve in %s", baseBranch, s.repo)
	}
	for _, d := range o.deps {
		if !taskIDRe.MatchString(d) {
			return "", fail("--depends: %q is not a task id", d)
		}
	}
	skill, err := skillDir()
	if err != nil {
		return "", err
	}
	tmpl, err := os.ReadFile(filepath.Join(skill, "references", "task-template.md"))
	if err != nil {
		return "", err
	}
	if err := claimRoot(s.root, s.repo); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "inbox", "handled"), 0o755); err != nil {
		return "", err
	}
	text := strings.NewReplacer(
		"@@ID@@", id,
		"@@KIND@@", o.kind,
		"@@REPO@@", s.repo,
		"@@BASE@@", baseBranch+"@"+sha,
		"@@HARNESS@@", o.harness,
		"@@MODEL@@", o.model,
		"@@EFFORT@@", o.effort,
		"@@DEPENDS@@", strings.Join(o.deps, ", "),
		"@@REPORT_BACK@@", reportBack(dir, id, o.kind, s.repo, baseBranch, sha, s.cfg, skill),
	).Replace(string(tmpl))
	if o.spec != "" {
		text = strings.Replace(text, "id: "+id+"\n", "id: "+id+"\nspec: "+o.spec+"\n", 1)
	}
	if degradedProfile(s.cfg, o.harness, o.model) {
		text = strings.Replace(text, "\ndepends_on:", "\ndegraded: true\ndepends_on:", 1)
	}
	if len(o.owns) > 0 {
		text = strings.Replace(text, "\ndepends_on:", "\nowns: "+strings.Join(o.owns, ", ")+"\ndepends_on:", 1)
	}
	if o.integration != "" {
		text = strings.Replace(text, "\ndepends_on:", "\nintegration: "+o.integration+"\ndepends_on:", 1)
	}
	if o.specHash != "" {
		text = strings.Replace(text, "\ndepends_on:", "\nspec_hash: "+o.specHash+"\ndepends_on:", 1)
	}
	for name, body := range o.fill {
		text = fillSection(text, name, body)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", err
	}
	if me := orchID(); me != "" {
		if err := os.WriteFile(filepath.Join(dir, "owner"), []byte(me+"\n"), 0o644); err != nil {
			return "", err
		}
	}
	return path, nil
}

// fillSection swaps the body of `# name` for body.
func fillSection(text, name, body string) string {
	head := "\n# " + name + "\n\n"
	i := strings.Index(text, head)
	if i < 0 {
		return text
	}
	start := i + len(head)
	end := len(text)
	if j := strings.Index(text[start:], "\n# "); j >= 0 {
		end = start + j
	}
	return text[:start] + body + "\n" + text[end:]
}

// reportBack is the generated section that tells a lead how to talk back.
func reportBack(dir, id, kind, repo, base, sha string, cfg config, skill string) string {
	me := selfPath()
	deliverable := fmt.Sprintf("commits on your own branch, in your own worktree, never in the main checkout: "+
		"`git -C %s worktree add -b <branch> %s %s` (branch per the repo's convention, else `orch/%s`). "+
		"Name the branch and head sha in your done line; never merge into `%s` or push unless a message tells you to",
		repo, worktreePath(dir, id), sha, id, base)
	if kind != "scout" && herdrManaged() {
		deliverable = fmt.Sprintf("commits on branch `%s`, in your own worktree `%s`: orch already made it from %s and you start inside it, so do not run `git worktree add` and never work in the main checkout. "+
			"Name the branch and head sha in your done line; never merge into `%s` or push unless a message tells you to",
			shipBranch(id), worktreePath(dir, id), sha, base)
	}
	if kind == "scout" {
		deliverable = fmt.Sprintf("a self-contained report at `%s` (findings with file:line evidence, then a recommendation); no branch, no edits to the repo", filepath.Join(dir, "report.md"))
	}
	skills := "none"
	if len(cfg.LeadSkills) > 0 {
		skills = strings.Join(cfg.LeadSkills, ", ")
	}
	lines := []string{
		fmt.Sprintf("You are lead `%s`. The orchestrator is your only contact: never address the user directly; anything for them goes up as needs-decision.", id),
		fmt.Sprintf("Read the lead protocol before starting: `%s`.", filepath.Join(skill, "references", "lead.md")),
		fmt.Sprintf("- Report: `%s report --dir %s <state> [--key <slug>] \"<one line>\"`. States: working, needs-decision, question, escalate, blocked, paused, done, failed, note.", me, dir),
		fmt.Sprintf("- Messages: when a line starting `orch:` says a message arrived, run `%s inbox --dir %s`. Run it once more before you report `done`, so a late redirect is not missed.", me, dir),
		"- Deliverable: " + deliverable + ".",
		"- Skills to load for the work: " + skills + ".",
	}
	if cfg.Check != "" {
		lines = append(lines, fmt.Sprintf("- Repo check command: `%s`.", cfg.Check))
	}
	names := make([]string, 0, len(cfg.Locks))
	for name := range cfg.Locks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("- Lock `%s`: hold it %s. Take it with `%s lock acquire %s --dir %s --wait 3600` "+
			"(run it in the background: it waits for the current holder), release it with `%s lock release %s --dir %s`. "+
			"Closing your pane also frees it.", name, cfg.Locks[name], me, name, dir, me, name, dir))
	}
	return strings.Join(lines, "\n")
}

// worktreePath puts a ship lead's worktree beside the run state: next to the
// other Herdr worktrees of the repo, or under the run root otherwise.
func worktreePath(dir, id string) string {
	root := filepath.Dir(dir)
	if filepath.Base(root) == ".orchestrate" {
		return filepath.Join(filepath.Dir(root), id)
	}
	return filepath.Join(root, "worktrees", id)
}

func cmdCheck(a *app, args []string) error {
	if err := exactArgs(args, 1, "check <id>"); err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	problems, warnings, err := checkTask(s.root, args[0])
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(a.out, "warning: "+w)
	}
	for _, p := range problems {
		fmt.Fprintln(a.out, "refused: "+p)
	}
	if len(problems) > 0 {
		return &exitError{code: 1}
	}
	fmt.Fprintf(a.out, "%s: ok\n", args[0])
	return nil
}

func cmdSpawn(a *app, args []string) error {
	fs := a.flags("spawn")
	override := fs.Bool("override-tier", false, "spawn even if triage says the task needs a stronger model (enforce mode)")
	auto := fs.Bool("auto", false, "choose the profile from triage and the dispatch rules (a dry run unless --apply)")
	apply := fs.Bool("apply", false, "with --auto: set the chosen profile and spawn")
	degraded := fs.Bool("degraded", false, "with --auto: allow a degraded fallback profile (ask the user first)")
	tier := fs.String("tier", "", "with --auto: settled, standard or planning; skip triage")
	harness := fs.String("harness", "", "choose a profile by hand: claude or pi")
	model := fs.String("model", "", "model for a hand-chosen profile")
	effort := fs.String("effort", "", "effort for a hand-chosen profile (default: task effort, or high)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "spawn <id> [--override-tier] [--auto [--apply] [--degraded]]"); err != nil {
		return err
	}
	if (*apply || *degraded || *tier != "") && !*auto {
		return fail("--apply, --degraded and --tier go with --auto")
	}
	if *tier != "" && *tier != "settled" && *tier != "standard" && *tier != "planning" {
		return fail("--tier is settled, standard or planning")
	}
	if *harness != "" || *model != "" || *effort != "" {
		if *auto || (*harness != "claude" && *harness != "pi") || *model == "" {
			return fail("choose --harness claude|pi and --model M without --auto")
		}
		return a.spawnChosen(pos[0], *harness, *model, *effort)
	}
	if *auto {
		return a.spawnAuto(pos[0], *apply, *degraded, *tier)
	}
	a.overrideTier = *override
	return a.spawn(pos[0], "")
}

// spawnOverride is the profile a replacement lead will have when it differs
// from what task.md says now: respawn --harness, or the pick of --auto.
// degraded means the user approved a degraded stand-in.
type spawnOverride struct {
	harness, model, effort string
	degraded               bool
}

// preflight makes the refusals that depend only on the task and the machine,
// not on a free slot or a model call, so a respawn or an auto pick can check
// them before it changes anything. fm is the task's front matter with ov laid
// over it.
func (a *app) preflight(id string, ov *spawnOverride) (s site, dir string, fm map[string]string, warnings []string, err error) {
	if os.Getenv("HERDR_ENV") != "1" || os.Getenv("HERDR_WORKSPACE_ID") == "" {
		return s, "", nil, nil, fail("spawn must run inside a Herdr pane (HERDR_ENV=1)")
	}
	s, err = a.site()
	if err != nil {
		return s, "", nil, nil, err
	}
	if why := spawnBlocked(s.root); why != "" {
		return s, "", nil, nil, fail("%s", why)
	}
	problems, warnings, err := checkTask(s.root, id)
	if err != nil {
		return s, "", nil, nil, err
	}
	if ov != nil { // the profile on disk is the one about to be replaced
		kept := problems[:0:0]
		for _, p := range problems {
			if !strings.HasPrefix(p, "frontmatter: harness is auto") {
				kept = append(kept, p)
			}
		}
		problems = kept
	}
	if len(problems) > 0 {
		return s, "", nil, nil, fail("task.md is not ready:\n  %s", strings.Join(problems, "\n  "))
	}
	dir, _ = taskDir(s.root, id)
	fm, _, err = readTask(filepath.Join(dir, "task.md"))
	if err != nil {
		return s, "", nil, nil, err
	}
	allowDegraded := fm["degraded"] == "true"
	if ov != nil {
		fm["harness"], fm["model"], fm["effort"] = ov.harness, ov.model, ov.effort
		allowDegraded = ov.degraded
	}
	if err := harnessUsable(s.cfg, fm["harness"], fm["model"], allowDegraded, a.now()); err != nil {
		return s, "", nil, nil, err
	}
	if fm["private"] == "true" && fm["harness"] != "claude" {
		return s, "", nil, nil, fail("%s is marked private, so it may only run on Claude, not %s", id, fm["harness"])
	}
	return s, dir, fm, warnings, nil
}

// spawnSlot refuses a task that already has an open lead and a spawn past
// max_leads. It is called twice: early, so a doomed spawn fails before the
// triage model call, and again under the spawn guard, where it is the
// authoritative answer.
func (a *app) spawnSlot(s site, dir, id string) ([]openLead, error) {
	if l, err := loadLead(dir); err != nil {
		return nil, err
	} else if l != nil && l.ClosedAt == 0 {
		return nil, fail("lead %s is already open in pane %s", id, l.PaneID)
	}
	leads := openLeads(s.root)
	if len(leads) >= s.cfg.MaxLeads {
		return nil, &exitError{code: 75, msg: fmt.Sprintf(
			"deferred: %d/%d leads open; close one first or raise max_leads in %s",
			len(leads), s.cfg.MaxLeads, userConfigPath())}
	}
	return leads, nil
}

// spawn starts a lead on a checked task; brief is appended to its launch
// prompt.
func (a *app) spawn(id, brief string) error {
	s, dir, fm, warnings, err := a.preflight(id, nil)
	if err != nil {
		return err
	}
	if err := requireMerged(s, dir, id, fm); err != nil {
		return err
	}
	me := orchID()
	if _, err := a.spawnSlot(s, dir, id); err != nil {
		return err
	}
	if fm["quick"] != "true" && fm["private"] != "true" { // quick jobs were rated by `orch quick` itself; private text never leaves the machine for triage
		if err := a.triageGate(s, id, fm); err != nil {
			return err
		}
	}
	name := leadName(id)
	epoch := newSessionID() // a respawn mints a new one, so `orch report` can refuse the replaced session
	env := []string{"ORCH_TASK_DIR=" + dir, "ORCH_TASK_ID=" + id, "ORCH_ROLE=lead", "ORCH_EPOCH=" + epoch}
	// The slot check, the pane and the lead record go under one guard, so two
	// spawns cannot both see a free slot or both take the same task. The slow
	// agent start and prompt below run outside it.
	var paneID string
	var l *lead
	managedShip := fm["kind"] != "scout" && herdrManaged()
	base := ""
	if managedShip {
		if fm["integration"] != "" { // requireMerged may have re-pinned the base
			tfm, _, err := readTask(filepath.Join(dir, "task.md"))
			if err != nil {
				return err
			}
			fm["base"] = tfm["base"]
		}
		_, base, _ = strings.Cut(fm["base"], "@")
		if !baseSHARe.MatchString(base) {
			return fail("task.md base %q has no commit sha", fm["base"])
		}
	}
	err = withFlock(filepath.Join(s.root, ".spawn.guard"), func() error {
		leads, err := a.spawnSlot(s, dir, id)
		if err != nil {
			return err
		}
		var pane, tabID, workspaceID string
		if managedShip {
			prev, _ := loadLead(dir)
			pane, tabID, workspaceID, err = a.placeShip(s, dir, id, base, prev, env)
		} else {
			pane, tabID, err = placePane(myLeads(leads, me), s.repo, env)
		}
		if err != nil {
			return err
		}
		paneID = pane
		_, _ = herdrCmd(herdrTimeout, "pane", "rename", paneID, name)
		l = &lead{ID: id, Name: name, Harness: fm["harness"], Model: fm["model"], PaneID: paneID, TabID: tabID, SpawnedAt: a.now().Unix(), Owner: me, Epoch: epoch, WorkspaceID: workspaceID}
		if l.Harness == "pi" {
			l.Session = newSessionID()
		}
		return saveLead(dir, l)
	})
	if err != nil {
		return err
	}
	start := append([]string{"agent", "start", name, "--kind", fm["harness"], "--pane", paneID, "--timeout", "90000", "--"}, harnessArgs(fm, s.cfg, l.Session)...)
	if err := startAgent(start); err != nil {
		_ = a.appendStatus(dir, "note", "agent did not report ready in pane "+paneID, "", "orch")
		fmt.Fprintf(a.out, "opened pane %s for %s, but the agent is not ready (%v). Look with: orch peek %s; give up with: orch close %s --force\n", paneID, name, err, id, id)
		return nil
	}
	launch := fmt.Sprintf("You are orchestrate lead %q. Read %s and follow it exactly, starting with its Report-back section.", id, filepath.Join(dir, "task.md"))
	if managedShip { // a brief written outside Herdr still tells the lead to make the worktree
		launch += fmt.Sprintf(" Your worktree already exists at %s on a branch orch made: start inside it and do not run `git worktree add`.", worktreePath(dir, id))
	}
	if brief != "" {
		launch += " " + brief
	}
	if _, err := herdrCmd(herdrTimeout, "agent", "prompt", paneID, launch); err != nil {
		_ = a.appendStatus(dir, "note", "agent prompt failed in pane "+paneID, "", "orch")
		return fail("opened pane %s for %s, but the launch prompt failed (%v). Look with: orch peek %s; give up with: orch close %s --force", paneID, name, err, id, id)
	}
	_, waitErr := herdrCmd(30*time.Second, "agent", "wait", paneID, "--until", "working", "--until", "blocked", "--timeout", "20000")
	if err := a.appendStatus(dir, "working", "spawned in pane "+paneID, "", "orch"); err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(a.out, "warning: "+w)
	}
	fmt.Fprintf(a.out, "spawned %s (%s %s) in pane %s\n", name, fm["harness"], fm["model"], paneID)
	if waitErr != nil {
		fmt.Fprintf(a.out, "warning: start of work not confirmed within 20s; check with: orch peek %s\n", id)
	}
	if watcherAlive(s.root, me) == 0 {
		fmt.Fprintf(a.out, "no watcher running: start `%s watch` with the Monitor tool\n", selfPath())
	}
	return nil
}

// A new pane is not an available shell until its prompt is up; Herdr refuses
// `agent start` at once with agent_pane_busy instead of waiting. Herdr 0.9.3
// has no readiness signal to poll (pane get reports agent_status "unknown"),
// so retry on that one error, bounded. Any other error returns unchanged.
var (
	agentStartBusyFor = 10 * time.Second
	agentStartBackoff = 100 * time.Millisecond
)

func startAgent(args []string) error {
	deadline := time.Now().Add(agentStartBusyFor)
	for wait := agentStartBackoff; ; wait = min(wait*2, time.Second) {
		_, err := herdrCmd(120*time.Second, args...)
		if err == nil || !strings.Contains(err.Error(), "agent_pane_busy") || time.Now().Add(wait).After(deadline) {
			return err
		}
		time.Sleep(wait)
	}
}

func harnessArgs(fm map[string]string, cfg config, session string) []string {
	var out []string
	model, effort := fm["model"], fm["effort"]
	if fm["harness"] == "claude" {
		if model != "default" {
			out = append(out, "--model", model)
		}
		if effort != "default" {
			out = append(out, "--effort", effort)
		}
		return append(out, cfg.ClaudeArgs...)
	}
	if model != "default" {
		out = append(out, "--model", model)
	}
	if effort != "default" {
		out = append(out, "--thinking", effort)
	}
	if session != "" {
		out = append(out, "--session-id", session)
	}
	return append(out, cfg.PiArgs...)
}

// placePane fills `leads` tabs as 2x2 grids (left, right, bottom-left,
// bottom-right) and opens a new tab once every tab is full.
func placePane(leads []openLead, repo string, env []string) (paneID, tabID string, err error) {
	workspace := os.Getenv("HERDR_WORKSPACE_ID")
	var envFlags []string
	for _, kv := range env {
		envFlags = append(envFlags, "--env", kv)
	}
	byTab := map[string][]string{}
	var tabs []string
	for _, ol := range leads {
		ok, err := paneExists(ol.lead.PaneID)
		if err != nil {
			return "", "", err
		}
		if !ok {
			continue
		}
		if _, seen := byTab[ol.lead.TabID]; !seen {
			tabs = append(tabs, ol.lead.TabID)
		}
		byTab[ol.lead.TabID] = append(byTab[ol.lead.TabID], ol.lead.PaneID)
	}
	for _, tab := range tabs {
		panes := byTab[tab]
		if !strings.HasPrefix(tab, workspace+":") || len(panes) >= panesPerTab {
			continue
		}
		target, direction := panes[0], "right"
		switch len(panes) {
		case 2:
			direction = "down"
		case 3:
			target, direction = panes[1], "down"
		}
		var res struct {
			Result struct {
				Pane struct {
					PaneID string `json:"pane_id"`
				} `json:"pane"`
			} `json:"result"`
		}
		split := append([]string{"pane", "split", target, "--direction", direction, "--cwd", repo, "--no-focus"}, envFlags...)
		if err := herdrJSON(&res, herdrTimeout, split...); err != nil {
			return "", "", err
		}
		return res.Result.Pane.PaneID, tab, nil
	}
	label := "leads"
	if len(tabs) > 0 {
		label = fmt.Sprintf("leads-%d", len(tabs)+1)
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
	create := append([]string{"tab", "create", "--workspace", workspace, "--cwd", repo, "--label", label, "--no-focus"}, envFlags...)
	if err := herdrJSON(&res, herdrTimeout, create...); err != nil {
		return "", "", err
	}
	return res.Result.RootPane.PaneID, res.Result.Tab.TabID, nil
}

func cmdSend(a *app, args []string) error {
	fs := a.flags("send")
	resolve := fs.String("resolve", "", "also close the lead's open decision `KEY`")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return &exitError{code: 2, msg: "usage: orch send <id> [--resolve KEY] <text...|->"}
	}
	if *resolve != "" && !keyRe.MatchString(*resolve) {
		return fail("--resolve must be a short lowercase slug")
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	dir, err := taskDir(s.root, pos[0])
	if err != nil {
		return err
	}
	l, err := loadLead(dir)
	if err != nil {
		return err
	}
	if l == nil {
		return fail("no lead spawned for %s", pos[0])
	}
	body := strings.Join(pos[1:], " ")
	if body == "-" {
		b, err := io.ReadAll(a.stdin)
		if err != nil {
			return err
		}
		body = string(b)
	}
	if strings.TrimSpace(body) == "" {
		return fail("empty message")
	}
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(filepath.Join(inbox, "handled"), 0o755); err != nil {
		return err
	}
	var name string
	if err := withFlock(filepath.Join(inbox, ".guard"), func() error {
		seq := 0
		for _, pattern := range []string{"*.msg", "handled/*.msg"} {
			found, _ := filepath.Glob(filepath.Join(inbox, pattern))
			for _, p := range found {
				if n, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(p), ".msg")); err == nil && n > seq {
					seq = n
				}
			}
		}
		name = fmt.Sprintf("%03d", seq+1)
		header := "from: orchestrator\nat: " + a.now().Format(time.RFC3339) + "\n"
		if *resolve != "" {
			header += "re: " + *resolve + "\n"
		}
		if err := writeAtomic(filepath.Join(inbox, name+".msg"), []byte(header+"\n"+strings.TrimRight(body, "\n")+"\n")); err != nil {
			return err
		}
		if *resolve != "" {
			return a.appendStatus(dir, "resolved", "answered in message "+name, *resolve, "orch")
		}
		return nil
	}); err != nil {
		return err
	}
	status := ""
	if l.ClosedAt == 0 {
		if agents, err := agentMap(); err == nil {
			status = agents[l.PaneID].Status
		}
	}
	switch status {
	case "":
		fmt.Fprintf(a.out, "stored message %s; the lead is not running, so no doorbell was sent\n", name)
	case "blocked":
		fmt.Fprintf(a.out, "stored message %s; the lead is waiting at a prompt in its pane, so no doorbell was sent (orch peek %s)\n", name, l.ID)
	default:
		if err := ringDoorbell(l, dir, status); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "sent message %s to %s\n", name, l.Name)
	}
	return nil
}

// ringDoorbell types the new-message line into the lead's pane. It rings
// nothing for a lead that is not running or waits at a prompt, where typing
// would answer the prompt.
func ringDoorbell(l *lead, dir, status string) error {
	if status == "" || status == "blocked" {
		return nil
	}
	bell := fmt.Sprintf("orch: new message for you. Run: %s inbox --dir %s", selfPath(), dir)
	_, err := herdrCmd(herdrTimeout, "agent", "prompt", l.PaneID, bell)
	return err
}

type row struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Agent     string  `json:"agent"`
	Herdr     string  `json:"herdr"`
	State     string  `json:"state"`
	Last      string  `json:"last"`
	LastAt    int64   `json:"last_at,omitempty"`
	Pane      string  `json:"pane,omitempty"`
	Owner     string  `json:"owner,omitempty"`
	Context   int     `json:"context,omitempty"`
	Decisions []entry `json:"decisions"`
	Degraded  bool    `json:"degraded,omitempty"`
	// Subs counts the lead's open helper panes; SubList names them.
	Subs    int        `json:"subs,omitempty"`
	SubList []subAgent `json:"sub_list,omitempty"`
}

func leadRows(root string, includeClosed bool) []row {
	agents, herdrErr := agentMap()
	var rows []row
	for _, dir := range allTasks(root) {
		l, _ := loadLead(dir)
		if l != nil && l.ClosedAt != 0 && !includeClosed {
			continue
		}
		fm, _, _ := readTask(filepath.Join(dir, "task.md"))
		entries := readStatus(dir)
		mine := leadEntries(entries)
		r := row{
			ID:        filepath.Base(dir),
			Kind:      fm["kind"],
			Agent:     fm["harness"] + "/" + fm["model"],
			State:     lastState(mine),
			Owner:     taskOwner(dir, l),
			Decisions: openDecisions(entries),
			Degraded:  fm["degraded"] == "true",
		}
		last := mine
		if len(last) == 0 {
			last = entries
		}
		if len(last) > 0 {
			e := last[len(last)-1]
			r.Last, r.LastAt = e.State+": "+e.Text, e.At
		}
		switch {
		case l == nil:
			r.Herdr = "draft"
		case l.ClosedAt != 0:
			r.Herdr, r.Pane = "closed", l.PaneID
		case herdrErr != nil:
			r.Herdr, r.Pane = "?", l.PaneID
		default:
			r.Herdr, r.Pane = "gone", l.PaneID
			if ag, ok := agents[l.PaneID]; ok {
				r.Herdr = ag.Status
				r.Context = contextTokens(ag.Agent, sessionOf(ag, l))
			}
		}
		if l != nil {
			r.SubList = openSubs(l)
			r.Subs = len(r.SubList)
		}
		if r.State == "" {
			r.State = "draft"
			if l != nil {
				r.State = "spawned"
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func cmdStatus(a *app, args []string) error {
	fs := a.flags("status")
	asJSON := fs.Bool("json", false, "machine-readable output")
	all := fs.Bool("all", false, "include closed leads")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	rows := leadRows(s.root, *all)
	if *asJSON {
		me := orchID()
		mine, others := splitRows(rows, me)
		b, _ := json.MarshalIndent(map[string]any{"root": s.root, "orchestrator": me, "watcher": watcherAlive(s.root, me) != 0,
			"notes": s.cfg.Notes, "leads": mine, "other_orchestrators": others}, "", "  ")
		fmt.Fprintln(a.out, string(b))
		return nil
	}
	fmt.Fprintln(a.out, a.board(s, rows))
	return nil
}

// cmdDispatch prints the user's rules for which harness and model take
// which kind of work; pick from them before `orch new`.
func cmdDispatch(a *app, args []string) error {
	if _, err := parseArgs(a.flags("dispatch"), args); err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	if len(s.cfg.Dispatch) == 0 {
		fmt.Fprintf(a.out, "no dispatch rules in %s: choose by the skill's Model guidance\n", userConfigPath())
		return nil
	}
	limited := loadHealth(a.now())
	for _, h := range healthLanes(limited) {
		if e, ok := limited[h]; ok {
			fmt.Fprintf(a.out, "health: %s %s\n", h, e)
		}
	}
	fmt.Fprintf(a.out, "dispatch rules (%s), first good fit wins; a list offers choices:\n", userConfigPath())
	for _, r := range s.cfg.Dispatch {
		use := make([]string, len(r.Use))
		for i, p := range r.Use {
			use[i] = p.String()
		}
		line := fmt.Sprintf("- %s -> %s", r.When, strings.Join(use, " | "))
		if r.Why != "" {
			line += " (" + r.Why + ")"
		}
		fmt.Fprintln(a.out, line)
		if note := dispatchNow(r, limited, func(p profile) bool { return s.cfg.modelAllowed(p.Harness, p.Model, a.now()) == nil }); note != "" {
			fmt.Fprintln(a.out, "  "+note)
		}
	}
	return nil
}

// dispatchNow says what to do with a rule while some harness is limited. It
// is empty when every profile is usable, so healthy output stays unchanged.
func dispatchNow(r dispatchRule, limited map[string]healthEntry, allowed func(profile) bool) string {
	anyLimited, anyDegraded := false, false
	var usable, degraded *profile
	for i := range r.Use {
		p := &r.Use[i]
		_, _, lim := limitedBy(limited, p.lane())
		anyLimited = anyLimited || lim
		anyDegraded = anyDegraded || p.Degraded
		switch {
		case lim, !allowed(*p):
		case p.Degraded:
			if degraded == nil {
				degraded = p
			}
		case usable == nil:
			usable = p
		}
	}
	if !anyLimited && !anyDegraded {
		return ""
	}
	switch {
	case usable != nil:
		return "now: " + usable.String()
	case degraded != nil:
		return "now: parked (the primary is limited). Degraded option: " + degraded.String() + ", only with the user's approval (--degraded)"
	}
	return "now: parked (every profile's harness is limited)"
}

// splitRows keeps the rows orchestrator me owns and, per other owner, the
// ids of its open leads.
func splitRows(rows []row, me string) (mine []row, others map[string][]string) {
	others = map[string][]string{}
	for _, r := range rows {
		switch {
		case ownedBy(r.Owner, me):
			mine = append(mine, r)
		case r.Herdr != "draft" && r.Herdr != "closed":
			others[r.Owner] = append(others[r.Owner], r.ID)
		}
	}
	return mine, others
}

func (a *app) board(s site, rows []row) string {
	me := orchID()
	rows, others := splitRows(rows, me)
	live := 0
	var unowned []string
	for _, r := range rows {
		if r.Herdr != "draft" && r.Herdr != "closed" {
			live++
			if r.Owner == "" && me != "" {
				unowned = append(unowned, r.ID)
			}
		}
	}
	total := live
	owners := make([]string, 0, len(others))
	for o, ids := range others {
		owners = append(owners, o)
		total += len(ids)
	}
	sort.Strings(owners)
	watcher := "stopped"
	if watcherAlive(s.root, me) != 0 {
		watcher = "running"
	}
	count := fmt.Sprintf("%d/%d", live, s.cfg.MaxLeads)
	if total != live {
		count = fmt.Sprintf("%d yours, %d/%d total", live, total, s.cfg.MaxLeads)
	}
	out := []string{
		"orchestrate: " + s.root,
		fmt.Sprintf("watcher: %s · leads open: %s", watcher, count),
	}
	limited := loadHealth(a.now())
	for _, h := range healthLanes(limited) {
		if e, ok := limited[h]; ok {
			out = append(out, fmt.Sprintf("health: %s %s; new tasks on it are refused (orch dispatch shows what to use)", h, e))
		}
	}
	if frozen := frozenTasks(s.root); len(frozen) > 0 {
		ids := make([]string, 0, len(frozen))
		for id := range frozen {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			out = append(out, fmt.Sprintf("FROZEN: %s - %s. Ask the user; orch unfreeze %s once they decide", id, frozen[id], id))
		}
	}
	if b, err := os.ReadFile(holdPath(s.root)); err == nil {
		out = append(out, "HOLD: "+strings.TrimSpace(string(b))+". New spawns are refused; lift with orch hold off")
	}
	var degradedIDs []string
	for _, r := range rows {
		if r.Degraded && r.Herdr != "closed" {
			degradedIDs = append(degradedIDs, r.ID)
		}
	}
	if len(degradedIDs) > 0 {
		out = append(out, "degraded: "+short(strings.Join(degradedIDs, ", "), 80)+" run on a weaker fallback profile; check their work more closely")
	}
	for _, o := range owners {
		line := fmt.Sprintf("other orchestrators: %s has %d open (%s)", o, len(others[o]), short(strings.Join(others[o], ", "), 80))
		// No pane means no watcher: those leads report into a log nobody reads.
		if exists, err := paneExists(o); err == nil && !exists {
			line += " - pane gone, nobody watches them; take over with orch adopt <id>"
		}
		out = append(out, line)
	}
	if len(unowned) > 0 {
		out = append(out, fmt.Sprintf("unowned: %s (spawned before per-orchestrator watchers, so every orchestrator sees them; claim with orch adopt <id>)", short(strings.Join(unowned, ", "), 80)))
	}
	if s.cfg.repoFile == "" {
		out = append(out, "repo config: none, using defaults (see Repo setup in the orchestrate skill)")
	}
	for _, n := range s.cfg.Notes {
		out = append(out, "note: "+n)
	}
	out = append(out, a.lockLines()...)
	if len(rows) == 0 {
		return strings.Join(append(out, "no tasks yet"), "\n")
	}
	out = append(out, fmt.Sprintf("%-20s %-5s %-16s %-8s %5s %6s  %s", "ID", "KIND", "AGENT", "HERDR", "CTX", "AGE", "LAST"))
	var decisions []string
	for _, r := range rows {
		ctx := "-"
		if r.Context > 0 {
			ctx = kTokens(r.Context)
			if r.Context >= s.cfg.LeadContextLimit {
				ctx += "!"
			}
		}
		agent := short(r.Agent, 16)
		if r.Subs > 0 {
			suffix := fmt.Sprintf("+%d", r.Subs)
			agent = short(r.Agent, 16-len(suffix)) + suffix
		}
		out = append(out, fmt.Sprintf("%-20s %-5s %-16s %-8s %5s %6s  %s", r.ID, r.Kind, agent, r.Herdr, ctx, a.age(r.LastAt), short(r.Last, 70)))
		for _, d := range r.Decisions {
			key := ""
			if d.Key != "" {
				key = " [" + d.Key + "]"
			}
			decisions = append(decisions, fmt.Sprintf("  %s%s %s: %s (%s)", r.ID, key, d.State, short(d.Text, 90), a.age(d.At)))
		}
	}
	if len(decisions) > 0 {
		queued := 0
		for _, r := range rows {
			for _, d := range r.Decisions {
				if d.State == "question" {
					queued++
				}
			}
		}
		out = append(out, fmt.Sprintf("open decisions: %d (%d blocking, %d queued)", len(decisions), len(decisions)-queued, queued))
		out = append(out, decisions...)
	}
	return strings.Join(out, "\n")
}

// watchSeen is the watcher's memory of one lead, kept in watch.json.
type watchSeen struct {
	SpawnedAt    int64  `json:"spawned_at"`
	Offset       int64  `json:"offset"`
	Herdr        string `json:"herdr,omitempty"`
	IdleSince    int64  `json:"idle_since,omitempty"`
	LastReport   int64  `json:"last_report,omitempty"`
	QuietAlerted bool   `json:"quiet_alerted,omitempty"`
	Frozen       bool   `json:"frozen,omitempty"`
	// Stall detection: see stall.go.
	FP           string `json:"fp,omitempty"`
	FPSince      int64  `json:"fp_since,omitempty"`
	LastSample   int64  `json:"last_sample,omitempty"`
	StallAlerted bool   `json:"stall_alerted,omitempty"`
	LoopAlerted  string `json:"loop_alerted,omitempty"`
	// RungFor is the oldest unread message the doorbell was rung again for.
	RungFor string `json:"rung_for,omitempty"`
	// Claude limit detection: see limits.go.
	LimitCheck int64  `json:"limit_check,omitempty"`
	LimitStamp string `json:"limit_stamp,omitempty"`
}

func cmdWatch(a *app, args []string) error {
	fs := a.flags("watch")
	interval := fs.Float64("interval", 5, "seconds between scans")
	timeout := fs.Int("timeout", 0, "checkpoint mode: exit 124 after `N` seconds with nothing actionable")
	once := fs.Bool("once", false, "scan once, then exit")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	if err := claimRoot(s.root, s.repo); err != nil {
		return err
	}
	me := orchID()
	pidFile, stateFile := watchFiles(s.root, me)
	if pid := watcherAlive(s.root, me); pid != 0 {
		return fail("a watcher is already running for this orchestrator (pid %d)", pid)
	}
	// The pid check above can't stop two watchers that start together: the
	// holder of this lock is the only one that may write the cursor.
	lockFile, err := os.OpenFile(strings.TrimSuffix(pidFile, ".pid")+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fail("a watcher is already running for this orchestrator")
		}
		return err
	}
	myPid := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(pidFile, fmt.Appendf(nil, "%s %d\n", myPid, a.now().Unix()), 0o644); err != nil {
		return err
	}
	defer func() {
		if b, err := os.ReadFile(pidFile); err == nil && strings.HasPrefix(string(b), myPid+" ") {
			os.Remove(pidFile)
		}
	}()
	seen := map[string]*watchSeen{}
	if b, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(b, &seen)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	started, herdrDown := a.now(), false
	say := func(msg string) { fmt.Fprintln(a.out, "orch: "+msg) }
	for {
		emitted := 0
		leads := myLeads(openLeads(s.root), me)
		if len(leads) == 0 {
			say("no open leads for this orchestrator; watcher exiting")
			return nil
		}
		agents, err := agentMap()
		if err != nil {
			if !herdrDown {
				say(fmt.Sprintf("herdr unreachable, watching status files only (%v)", err))
				emitted++
			}
			herdrDown = true
		} else {
			herdrDown = false
		}
		for _, ol := range leads {
			st := seen[ol.lead.ID]
			if st == nil {
				// A lead taken over from another orchestrator keeps that watcher's place.
				st = otherWatcherSeen(s.root, stateFile, ol.lead)
			}
			if st == nil || st.SpawnedAt != ol.lead.SpawnedAt {
				var offset int64
				if st != nil {
					offset = st.Offset
				}
				st = &watchSeen{SpawnedAt: ol.lead.SpawnedAt, Offset: offset}
			}
			seen[ol.lead.ID] = st
			emitted += a.watchLead(ol, st, agents, s.cfg, say)
		}
		b, _ := json.Marshal(seen)
		if err := writeAtomic(stateFile, b); err != nil {
			return err
		}
		if *once {
			return nil
		}
		if *timeout > 0 {
			if emitted > 0 {
				return nil
			}
			if a.now().Sub(started) >= time.Duration(*timeout)*time.Second {
				say(fmt.Sprintf("checkpoint, nothing actionable in %ds", *timeout))
				return &exitError{code: 124}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Duration(*interval * float64(time.Second))):
		}
	}
}

// otherWatcherSeen finds the furthest place another watcher reached in this
// lead's status log, or nil when none has watched it.
func otherWatcherSeen(root, stateFile string, l *lead) *watchSeen {
	files, _ := filepath.Glob(filepath.Join(root, "watch*.json"))
	var best *watchSeen
	for _, f := range files {
		if f == stateFile {
			continue
		}
		var m map[string]*watchSeen
		if b, err := os.ReadFile(f); err != nil || json.Unmarshal(b, &m) != nil {
			continue
		}
		if st := m[l.ID]; st != nil && st.SpawnedAt == l.SpawnedAt && (best == nil || st.Offset > best.Offset) {
			best = st
		}
	}
	return best
}

// watchLead prints one line per actionable event for one lead and returns how
// many it printed. agents is nil while Herdr is unreachable.
func (a *app) watchLead(ol openLead, st *watchSeen, agents map[string]herdrAgent, cfg config, say func(string)) int {
	id, now, emitted := ol.lead.ID, a.now().Unix(), 0
	emit := func(msg string) {
		say(id + " " + msg)
		emitted++
	}
	if f, err := os.Open(filepath.Join(ol.dir, "status.log")); err == nil {
		info, _ := f.Stat()
		if info.Size() < st.Offset {
			st.Offset = 0
		}
		if info.Size() > st.Offset {
			_, _ = f.Seek(st.Offset, io.SeekStart)
			chunk, _ := io.ReadAll(f)
			complete := chunk[:strings.LastIndex(string(chunk), "\n")+1]
			for _, line := range strings.Split(string(complete), "\n") {
				e, ok := parseLine(line)
				if !ok || e.By == "orch" {
					continue
				}
				st.LastReport, st.QuietAlerted, st.StallAlerted = now, false, false
				if e.State == "done" && fmQuick(ol.dir) {
					emit("done (quick): " + a.quickVerify(id))
				}
				if notifyStates[e.State] {
					key := ""
					if e.Key != "" {
						key = " [" + e.Key + "]"
					}
					emit(fmt.Sprintf("%s%s: %s", e.State, key, e.Text))
				}
			}
			st.Offset += int64(len(complete))
		}
		f.Close()
	}
	if why := frozenReason(readStatus(ol.dir)); why != "" && !st.Frozen {
		st.Frozen = true
		emit("frozen: " + why + ". Stop automatic recovery and ask the user; orch unfreeze " + id + " once they decide")
	} else if why == "" {
		st.Frozen = false
	}
	if agents == nil {
		return emitted
	}
	ag, ok := agents[ol.lead.PaneID]
	if !ok {
		if st.Herdr != "gone" {
			reason := "pane closed"
			if exists, err := paneExists(ol.lead.PaneID); err == nil && exists {
				reason = "agent exited"
			}
			emit(fmt.Sprintf("gone: %s (pane %s); see orch status", reason, ol.lead.PaneID))
		}
		st.Herdr = "gone"
		return emitted
	}
	if ol.lead.Harness == "claude" && now-st.LimitCheck >= 60 {
		st.LimitCheck = now
		after := st.LimitStamp
		if after == "" {
			after = time.Unix(ol.lead.SpawnedAt, 0).UTC().Format(time.RFC3339)
		}
		if found, ok := claudeLimitSince(sessionOf(ag, ol.lead), after); ok {
			st.LimitStamp = found.Stamp
			if found.ResetsAt > now {
				reason := "claude " + found.Type + " limit (auto, from transcript)"
				if wrote, err := autoLimit("claude", found.ResetsAt, reason, now); err == nil && wrote {
					emit(fmt.Sprintf("limit: claude %s limit until %s; recorded in health (auto). Clear early with: orch health set claude ok", found.Type, time.Unix(found.ResetsAt, 0).Format("15:04")))
				}
			}
		}
	}
	if ag.Status == "blocked" && st.Herdr != "blocked" {
		emit("ui-blocked: waiting at a prompt in its pane; run orch peek " + id + ". If it shows a usage or credit limit, run: orch health set " + ol.lead.Harness + " limited --for <duration>")
	}
	if ag.Status == "idle" || ag.Status == "done" {
		if st.IdleSince == 0 {
			st.IdleSince = now
		}
		waiting := waitingOnOrch[lastState(leadEntries(readStatus(ol.dir)))]
		idleFor := now - max(st.IdleSince, st.LastReport)
		if !waiting && !st.QuietAlerted && idleFor >= int64(cfg.QuietMinutes)*60 {
			emit(fmt.Sprintf("quiet: idle %dm without a report; run orch peek %s", idleFor/60, id))
			st.QuietAlerted = true
		}
	} else {
		st.IdleSince = 0
	}
	if pending := pendingMessages(ol.dir); len(pending) == 0 {
		st.RungFor = ""
	} else if (ag.Status == "idle" || ag.Status == "done") && cfg.UnreadMinutes > 0 && st.RungFor != filepath.Base(pending[0]) {
		if info, err := os.Stat(pending[0]); err == nil && a.now().Sub(info.ModTime()) >= time.Duration(cfg.UnreadMinutes)*time.Minute {
			if err := ringDoorbell(ol.lead, ol.dir, ag.Status); err == nil {
				st.RungFor = filepath.Base(pending[0])
				emit(fmt.Sprintf("reminder: %d unread message(s) for %dm; rang the doorbell again", len(pending), int(a.now().Sub(info.ModTime())/time.Minute)))
			}
		}
	}
	if ag.Status == "working" {
		a.stallCheck(ol, st, ag, cfg, now, emit)
	}
	st.Herdr = ag.Status
	return emitted
}

func (a *app) requireLead(id string) (string, *lead, error) {
	s, err := a.site()
	if err != nil {
		return "", nil, err
	}
	dir, err := taskDir(s.root, id)
	if err != nil {
		return "", nil, err
	}
	l, err := loadLead(dir)
	if err != nil {
		return "", nil, err
	}
	if l == nil || l.ClosedAt != 0 {
		return "", nil, fail("no open lead for %s", id)
	}
	return dir, l, nil
}

func cmdPeek(a *app, args []string) error {
	fs := a.flags("peek")
	n := fs.Int("lines", 40, "lines to show")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "peek <id> [--lines N]"); err != nil {
		return err
	}
	_, l, err := a.requireLead(pos[0])
	if err != nil {
		return err
	}
	// Herdr returns nothing for small --lines values, so always ask for 200+.
	out, err := herdrCmd(herdrTimeout, "agent", "read", l.PaneID, "--source", "recent-unwrapped", "--lines", strconv.Itoa(max(*n, 200)))
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	fmt.Fprintln(a.out, strings.Join(lines[max(0, len(lines)-*n):], "\n"))
	return nil
}

func cmdFocus(a *app, args []string) error {
	if err := exactArgs(args, 1, "focus <id>"); err != nil {
		return err
	}
	_, l, err := a.requireLead(args[0])
	if err != nil {
		return err
	}
	if _, err := herdrCmd(herdrTimeout, "agent", "focus", l.PaneID); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "focused "+l.Name)
	return nil
}

func cmdClose(a *app, args []string) error {
	fs := a.flags("close")
	force := fs.Bool("force", false, "close a lead that has not reported done or failed (needs the user's agreement)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "close <id> [--force]"); err != nil {
		return err
	}
	dir, l, err := a.requireLead(pos[0])
	if err != nil {
		return err
	}
	state := lastState(leadEntries(readStatus(dir)))
	if state != "done" && state != "failed" && !*force {
		if state == "" {
			state = "nothing"
		}
		return fail("%s last reported %s; close after done or failed, or pass --force once the user has agreed to drop its work", l.ID, state)
	}
	note := "closed by orchestrator"
	if *force {
		note += " (forced)"
	}
	if err := a.closeLead(dir, l, note); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "closed "+l.Name)
	if l.WorkspaceID == "" {
		return nil
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	fm, _, err := readTask(filepath.Join(dir, "task.md"))
	if err != nil {
		return err
	}
	return a.removeShipWorktree(s, dir, l, fm)
}

// closeLead closes the lead's pane, frees its locks and logs note.
func (a *app) closeLead(dir string, l *lead, note string) error {
	if err := a.closeSubs(dir, l); err != nil {
		return err
	}
	exists, err := paneExists(l.PaneID)
	if err != nil {
		return err
	}
	if exists {
		_, _ = herdrCmd(herdrTimeout, "pane", "close", l.PaneID)
		if still, err := paneExists(l.PaneID); err != nil || still {
			return fail("pane %s is still open after close", l.PaneID)
		}
	}
	l.ClosedAt = a.now().Unix()
	if err := saveLead(dir, l); err != nil {
		return err
	}
	if freed := releaseLocks(dir); len(freed) > 0 {
		note += "; released " + strings.Join(freed, ", ")
	}
	return a.appendStatus(dir, "note", note, "", "orch")
}

// contextKey is the decision key a lead reports when its context is too full
// for the next piece of work; compact and respawn both answer it.
const contextKey = "context"

const compactFocus = "Keep the task.md contract, decisions made with their reasons, the state of the work and the next steps. Drop file contents, tool output and anything unrelated to the next piece of work."

// cmdCompact has a lead's harness compact its context, then points the lead
// back at its contract.
func cmdCompact(a *app, args []string) error {
	if len(args) < 1 {
		return &exitError{code: 2, msg: "usage: orch compact <id> [focus...]"}
	}
	dir, l, err := a.requireLead(args[0])
	if err != nil {
		return err
	}
	agents, err := agentMap()
	if err != nil {
		return err
	}
	if st := agents[l.PaneID].Status; st != "idle" && st != "done" {
		return fail("%s is %s; compact only between turns (wait for idle, or orch peek %s)", l.ID, st, l.ID)
	}
	focus := strings.Join(args[1:], " ")
	if focus == "" {
		focus = compactFocus
	}
	before := contextTokens(agents[l.PaneID].Agent, sessionOf(agents[l.PaneID], l))
	if _, err := herdrCmd(16*time.Minute, "agent", "prompt", l.PaneID, "/compact "+focus, "--wait", "--timeout", "900000"); err != nil {
		return err
	}
	resume := fmt.Sprintf("orch: your context was compacted. Re-read %s and %s if it exists, then go on with the work in your last inbox message.",
		filepath.Join(dir, "task.md"), filepath.Join(dir, "handoff.md"))
	if _, err := herdrCmd(herdrTimeout, "agent", "prompt", l.PaneID, resume); err != nil {
		return err
	}
	if err := a.resolveContext(dir, "compacted"); err != nil {
		return err
	}
	line := "compacted " + l.Name
	if agents, err := agentMap(); err == nil && before > 0 {
		ag := agents[l.PaneID]
		line += fmt.Sprintf(" (%s -> %s)", kTokens(before), kTokens(contextTokens(ag.Agent, sessionOf(ag, l))))
	}
	fmt.Fprintln(a.out, line)
	return nil
}

// cmdRespawn replaces a lead whose context is spent with a fresh session on
// the same task, worktree and branch. It starts from the handoff.md the old
// lead wrote, which it renames so a later respawn needs a new one.
func cmdRespawn(a *app, args []string) error {
	fs := a.flags("respawn")
	harness := fs.String("harness", "", "restart on another harness (claude or pi), for example after a usage limit")
	model := fs.String("model", "", "model for the new harness (default: keep the task's)")
	effort := fs.String("effort", "", "effort for the new harness (default: keep the task's)")
	degraded := fs.Bool("degraded", false, "allow a degraded fallback profile (ask the user first)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "respawn <id> [--harness claude|pi [--model M] [--effort E] [--degraded]]"); err != nil {
		return err
	}
	if *harness != "" && *harness != "claude" && *harness != "pi" {
		return fail("--harness must be claude or pi")
	}
	s0, err := a.site()
	if err != nil {
		return err
	}
	dir, err := taskDir(s0.root, pos[0])
	if err != nil {
		return err
	}
	l, err := loadLead(dir)
	if err != nil {
		return err
	}
	handoff := filepath.Join(dir, "handoff.md")
	// A lead closed by an earlier respawn whose replacement did not start still
	// has its handoff: that respawn is retried without closing anything.
	retry := l != nil && l.ClosedAt != 0 && fileExists(handoff)
	if l == nil || (l.ClosedAt != 0 && !retry) {
		return fail("no open lead for %s", pos[0])
	}
	if why := frozenReason(readStatus(dir)); why != "" {
		return fail("%s is frozen (%s). Decide with the user, then run: orch unfreeze %s", l.ID, why, l.ID)
	}
	_, haveHandoff := os.Stat(handoff)
	if haveHandoff != nil && *harness == "" {
		return fail("%s has no handoff.md; ask the lead to write one (Context budget in the lead protocol) first", l.ID)
	}
	taskPath := filepath.Join(dir, "task.md")
	var ov *spawnOverride
	if *harness != "" {
		fm, _, err := readTask(taskPath)
		if err != nil {
			return err
		}
		m, e := fm["model"], fm["effort"]
		if *model != "" {
			m = *model
		}
		if *effort != "" {
			e = *effort
		}
		if *harness != fm["harness"] && *model == "" {
			return fail("moving %s from %s to %s needs --model: model names differ between harnesses", l.ID, fm["harness"], *harness)
		}
		ov = &spawnOverride{harness: *harness, model: m, effort: e, degraded: *degraded}
	}
	// Every refusal the replacement could make comes before the old lead is
	// closed or any file is touched.
	s, _, _, _, err := a.preflight(l.ID, ov)
	if err != nil {
		return err
	}
	how := "closed for respawn with a fresh context"
	if *harness != "" {
		how = "closed for respawn on " + *harness
	}
	if !retry {
		if err := a.closeLead(dir, l, how); err != nil {
			return err
		}
	}
	if err := a.resolveContext(dir, "respawned with a fresh context"); err != nil {
		return err
	}
	// From here the task files change; keep what is needed to undo it if the
	// replacement does not start.
	origTask, err := os.ReadFile(taskPath)
	if err != nil {
		return err
	}
	if ov != nil {
		for k, v := range map[string]string{"harness": ov.harness, "model": ov.model, "effort": ov.effort} {
			if err := setFrontMatter(taskPath, k, v); err != nil {
				return err
			}
		}
		if degradedProfile(s.cfg, ov.harness, ov.model) {
			if err := setFrontMatter(taskPath, "degraded", "true"); err != nil {
				return err
			}
		}
		_ = a.appendStatus(dir, "note", "respawned on "+ov.harness, "", "orch")
	}
	brief := ""
	kept := ""
	if haveHandoff == nil {
		kept = filepath.Join(dir, fmt.Sprintf("handoff-%d.md", a.now().Unix()))
		if err := os.Rename(handoff, kept); err != nil {
			_ = os.WriteFile(taskPath, origTask, 0o644)
			return err
		}
		brief = fmt.Sprintf("You replace a lead whose context filled up: read its handoff at %s before anything else. Its worktree and branch already exist; continue in them, and retake any lock the handoff names.", kept)
	} else {
		brief = fmt.Sprintf("You replace a lead whose provider failed mid-task; it left no handoff. Recover from disk: read the status log at %s, then run `git status` and `git log` in the worktree %s and read the diff against the base. Commit any uncommitted work that belongs to the task first, then continue the task. Its branch already exists; retake any lock it held. If the state is unclear, report `escalate` before changing anything.",
			filepath.Join(dir, "status.log"), worktreePath(dir, l.ID))
	}
	if err := a.spawn(l.ID, brief); err != nil {
		// A replacement that left a lead behind owns the new handoff path and
		// profile; otherwise put both back as they were.
		if nl, _ := loadLead(dir); nl == nil || nl.ClosedAt != 0 {
			_ = os.WriteFile(taskPath, origTask, 0o644)
			if kept != "" {
				_ = os.Rename(kept, handoff)
			}
		}
		return fail("closed %s, but the new lead did not start: %v. Once that is fixed, run `orch respawn %s` again: the handoff is kept", l.Name, err, l.ID)
	}
	return nil
}

// resolveContext closes an open context decision, if the lead raised one.
func (a *app) resolveContext(dir, how string) error {
	for _, d := range openDecisions(readStatus(dir)) {
		if d.Key == contextKey {
			return a.appendStatus(dir, "resolved", how, contextKey, "orch")
		}
	}
	return nil
}

// cmdConfigCheck validates a draft orchestrate.toml, such as one a setup
// lead wrote in its worktree, where orch status would read the main checkout.
// cmdAdopt moves an open lead to this orchestrator, for example after the
// orchestrator that spawned it ended.
func cmdAdopt(a *app, args []string) error {
	if err := exactArgs(args, 1, "adopt <id>"); err != nil {
		return err
	}
	me := orchID()
	if me == "" {
		return fail("adopt needs a Herdr pane (HERDR_PANE_ID is unset)")
	}
	dir, l, err := a.requireLead(args[0])
	if err != nil {
		return err
	}
	from := l.Owner
	l.Owner = me
	if err := saveLead(dir, l); err != nil {
		return err
	}
	if from == "" {
		from = "no orchestrator"
	}
	if err := a.appendStatus(dir, "note", "adopted by orchestrator "+me+" from "+from, "", "orch"); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s now belongs to %s (was %s); its events reach this orchestrator's watcher\n", l.ID, me, from)
	return nil
}

func cmdConfigCheck(a *app, args []string) error {
	pos, err := parseArgs(a.flags("config-check"), args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "config-check <file>"); err != nil {
		return err
	}
	path := pos[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.cwd, path)
	}
	if _, err := os.Stat(path); err != nil {
		return fail("%s: %v", path, err)
	}
	rc := repoConfig{QuietMinutes: 30, StallMinutes: 15}
	if err := readRepoConfig(path, &rc); err != nil {
		return err
	}
	if rc.QuietMinutes < 1 || rc.StallMinutes < 1 {
		return fail("%s: quiet_minutes and stall_minutes must be at least 1", path)
	}
	fmt.Fprintf(a.out, "ok: %s (%d of %d notes, %d locks)\n", path, len(rc.Notes), maxNotes, len(rc.Locks))
	return nil
}

func cmdBecome(a *app, args []string) error {
	fs := a.flags("become")
	off := fs.Bool("off", false, "turn the orchestrator role off for this pane")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	pane := os.Getenv("HERDR_PANE_ID")
	if pane == "" {
		return fail("become needs a Herdr pane (HERDR_PANE_ID is unset)")
	}
	marker := filepath.Join(rolesDir(), pane)
	if *off {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Fprintln(a.out, "orchestrator role off for this pane")
		return nil
	}
	var res struct {
		Result struct {
			Pane herdrAgent `json:"pane"`
		} `json:"result"`
	}
	if err := herdrJSON(&res, herdrTimeout, "pane", "current", "--current"); err != nil {
		return err
	}
	session := res.Result.Pane.Session.Value
	if err := os.MkdirAll(rolesDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"session": session, "at": a.now().Unix()})
	if err := os.WriteFile(marker, b, 0o644); err != nil {
		return err
	}
	if session == "" {
		fmt.Fprintf(a.out, "orchestrator role on for pane %s (no agent session found, so it applies to any session in this pane)\n", pane)
		return nil
	}
	fmt.Fprintf(a.out, "orchestrator role on for pane %s (session %s); guards and the status board are active\n", pane, session)
	return nil
}
