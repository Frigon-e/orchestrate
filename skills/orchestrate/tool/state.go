package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The status log is append-only, one line per event:
//
//	state [at=epoch] [key=slug] [by=orch]: text
//
// Leads write their states; the orchestrator writes `resolved` and notes,
// tagged by=orch so they never count as the lead's own state.
var (
	leadStates    = []string{"working", "needs-decision", "blocked", "question", "escalate", "paused", "done", "failed", "note"}
	notifyStates  = map[string]bool{"done": true, "needs-decision": true, "blocked": true, "escalate": true, "failed": true}
	waitingOnOrch = map[string]bool{"done": true, "failed": true, "needs-decision": true, "blocked": true, "escalate": true, "paused": true}
	lineRe        = regexp.MustCompile(`^([a-z-]+)((?: \[[a-z]+=[^\]]*\])*): ?(.*)$`)
	tagRe         = regexp.MustCompile(`\[([a-z]+)=([^\]]*)\]`)
	keyRe         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
)

type entry struct {
	State string `json:"state"`
	Text  string `json:"text"`
	Key   string `json:"key,omitempty"`
	By    string `json:"-"`
	At    int64  `json:"at"`
}

func (a *app) appendStatus(dir, state, text, key, by string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [at=%d]", state, a.now().Unix())
	if key != "" {
		fmt.Fprintf(&b, " [key=%s]", key)
	}
	if by != "" {
		fmt.Fprintf(&b, " [by=%s]", by)
	}
	fmt.Fprintf(&b, ": %s\n", strings.Join(strings.Fields(text), " "))
	f, err := os.OpenFile(filepath.Join(dir, "status.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func parseLine(line string) (entry, bool) {
	m := lineRe.FindStringSubmatch(strings.TrimRight(line, "\n"))
	if m == nil {
		return entry{}, false
	}
	e := entry{State: m[1], Text: m[3]}
	for _, t := range tagRe.FindAllStringSubmatch(m[2], -1) {
		switch t[1] {
		case "at":
			e.At, _ = strconv.ParseInt(t[2], 10, 64)
		case "key":
			e.Key = t[2]
		case "by":
			e.By = t[2]
		}
	}
	return e, true
}

func readStatus(dir string) []entry {
	b, err := os.ReadFile(filepath.Join(dir, "status.log"))
	if err != nil {
		return nil
	}
	var out []entry
	for _, line := range strings.Split(string(b), "\n") {
		if e, ok := parseLine(line); ok {
			out = append(out, e)
		}
	}
	return out
}

func leadEntries(entries []entry) []entry {
	var out []entry
	for _, e := range entries {
		if e.By != "orch" {
			out = append(out, e)
		}
	}
	return out
}

func lastState(entries []entry) string {
	for _, entrie := range slices.Backward(entries) {
		if s := entrie.State; s != "note" && s != "question" && slices.Contains(leadStates, s) {
			return s
		}
	}
	return ""
}

// openDecisions lists needs-decision, blocked, escalate and question lines still open: a keyed one
// until `resolved [key=k]`, a keyless one until any keyless resolved line or
// until the lead reports working, done or failed.
func openDecisions(entries []entry) []entry {
	var keys []string
	keyed := map[string]entry{}
	var keyless []entry
	for _, e := range entries {
		switch {
		case e.State == "needs-decision" || e.State == "blocked" || e.State == "question" || e.State == "escalate":
			if e.Key == "" {
				keyless = append(keyless, e)
			} else {
				if _, ok := keyed[e.Key]; !ok {
					keys = append(keys, e.Key)
				}
				keyed[e.Key] = e
			}
		case e.State == "resolved":
			if e.Key == "" {
				keyless = nil
			} else {
				delete(keyed, e.Key)
				keys = slices.DeleteFunc(keys, func(k string) bool { return k == e.Key })
			}
		case e.By != "orch" && (e.State == "working" || e.State == "done" || e.State == "failed"):
			keyless = nil
		}
	}
	var out []entry
	for _, k := range keys {
		if e, ok := keyed[k]; ok {
			out = append(out, e)
		}
	}
	return append(out, keyless...)
}

type lead struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Harness   string `json:"harness"`
	Model     string `json:"model"`
	PaneID    string `json:"pane_id"`
	TabID     string `json:"tab_id"`
	SpawnedAt int64  `json:"spawned_at"`
	ClosedAt  int64  `json:"closed_at,omitempty"`
	// Session is the harness session id orch chose at spawn, for harnesses
	// Herdr reports no session for (pi). Empty for the others.
	Session string `json:"session,omitempty"`
	// Owner is the Herdr pane of the orchestrator that watches this lead.
	// Empty on leads spawned before owners existed: every orchestrator sees those.
	Owner string `json:"owner,omitempty"`
	// Epoch is the token of the session spawn started; a respawn replaces it,
	// and `orch report` refuses a session holding an older one.
	Epoch string `json:"epoch,omitempty"`
	// WorkspaceID is the Herdr workspace orch made for a ship lead's worktree.
	// Empty on scouts and on leads whose worktree the lead made itself.
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// orchID names the orchestrator running this command by its Herdr pane, so
// several orchestrators can share one repo. Empty outside Herdr.
func orchID() string { return os.Getenv("HERDR_PANE_ID") }

// ownedBy says whether orchestrator me watches and lists a lead with owner.
func ownedBy(owner, me string) bool { return owner == "" || owner == me }

// taskOwner is the lead's owner, or for a draft the orchestrator that wrote it.
func taskOwner(dir string, l *lead) string {
	if l != nil {
		return l.Owner
	}
	b, _ := os.ReadFile(filepath.Join(dir, "owner"))
	return strings.TrimSpace(string(b))
}

func loadLead(dir string) (*lead, error) {
	b, err := os.ReadFile(filepath.Join(dir, "lead.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l lead
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fail("%s/lead.json: %v", dir, err)
	}
	return &l, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func saveLead(dir string, l *lead) error {
	b, _ := json.MarshalIndent(l, "", "  ")
	return writeAtomic(filepath.Join(dir, "lead.json"), append(b, '\n'))
}

// allTasks lists task directories (those holding a task.md), sorted by id.
func allTasks(root string) []string {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "task.md")); e.IsDir() && err == nil {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

type openLead struct {
	dir  string
	lead *lead
}

func openLeads(root string) []openLead {
	var out []openLead
	for _, dir := range allTasks(root) {
		if l, err := loadLead(dir); err == nil && l != nil && l.ClosedAt == 0 {
			out = append(out, openLead{dir, l})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].lead.SpawnedAt < out[j].lead.SpawnedAt })
	return out
}

// myLeads keeps the open leads that orchestrator me watches.
func myLeads(leads []openLead, me string) []openLead {
	var out []openLead
	for _, ol := range leads {
		if ownedBy(ol.lead.Owner, me) {
			out = append(out, ol)
		}
	}
	return out
}

func pendingMessages(dir string) []string {
	msgs, _ := filepath.Glob(filepath.Join(dir, "inbox", "*.msg"))
	sort.Strings(msgs)
	return msgs
}

var unsafeNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// watchFiles are the pid and state files of orchestrator me's watcher. Each
// orchestrator has its own, so two can watch one repo at once.
func watchFiles(root, me string) (pidFile, stateFile string) {
	name := "watch"
	if me != "" {
		name += "-" + unsafeNameRe.ReplaceAllString(me, "-")
	}
	return filepath.Join(root, name+".pid"), filepath.Join(root, name+".json")
}

// watcherAlive reports the pid of me's running watcher, or 0. A watcher
// started before per-orchestrator watchers (watch.pid) counts for everyone
// until it exits, so the upgrade does not start a duplicate.
func watcherAlive(root, me string) int {
	pidFile, _ := watchFiles(root, me)
	if pid := pidRunsWatch(pidFile); pid != 0 || me == "" {
		return pid
	}
	legacy, _ := watchFiles(root, "")
	return pidRunsWatch(legacy)
}

// pidRunsWatch checks a pid file and that the pid still runs `orch watch`,
// so a reused pid does not count.
func pidRunsWatch(pidFile string) int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	out, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if strings.Contains(string(out), "orch") && strings.Contains(string(out), " watch") {
		return pid
	}
	return 0
}

func (a *app) age(ts int64) string {
	if ts == 0 {
		return "-"
	}
	s := int64(a.now().Sub(time.Unix(ts, 0)).Seconds())
	if s < 3600 {
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
}

func short(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return string(r[:n-1]) + "…"
}
