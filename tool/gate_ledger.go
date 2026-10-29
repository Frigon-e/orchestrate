package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The findings ledger keeps one list of blocker findings per spec path across
// gate runs, with short stable IDs (L1, L2, ...). It changes no verdict: the
// pass rule, gate.json and the exit code never read it.

const (
	gateLedgerHintMin = 0.7
	gateLedgerUsage   = "gate resolve <spec> | gate resolve <spec> <L-id> fixed|accepted|rejected|duplicate|open --reason \"...\" [--of <L-id>]"
)

type ledgerSighting struct {
	Hash    string `json:"hash"`
	At      int64  `json:"at"`
	Finding string `json:"finding"`
}

// ledgerMatch is the shadow matcher's answer for a new finding. It is stored
// and shown only as the hint of an open item in the list.
type ledgerMatch struct {
	Decision   string   `json:"decision,omitempty"`
	Choice     string   `json:"choice,omitempty"`
	P          float64  `json:"p"`
	Restates   bool     `json:"restates,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	HintAt     int64    `json:"hint_at,omitempty"`
}

type ledgerHistory struct {
	State  string `json:"state"`
	Of     string `json:"of,omitempty"`
	Reason string `json:"reason,omitempty"`
	At     int64  `json:"at"`
}

type ledgerItem struct {
	ID       string           `json:"id"`
	State    string           `json:"state"`
	Of       string           `json:"of,omitempty"`
	Reason   string           `json:"reason,omitempty"`
	ClosedAt int64            `json:"closed_at,omitempty"`
	Severity string           `json:"severity"`
	Source   string           `json:"source"`
	Check    string           `json:"check,omitempty"`
	Where    string           `json:"where"`
	Claim    string           `json:"claim"`
	Evidence string           `json:"evidence"`
	Why      string           `json:"why,omitempty"`
	Seen     []ledgerSighting `json:"seen"`
	Match    *ledgerMatch     `json:"match,omitempty"`
	History  []ledgerHistory  `json:"history,omitempty"`
}

type gateLedger struct {
	Schema  int          `json:"schema"`
	SpecRel string       `json:"spec_rel"`
	Next    int          `json:"next"`
	Items   []ledgerItem `json:"items"`
}

// The seams are nil unless the gate-match package sets them in an init
// function of its own file.
var (
	// gateLedgerMatch answers, per fresh finding ID, which candidate it restates.
	gateLedgerMatch func(ctx context.Context, s site, rel string, candidates []ledgerItem, fresh []ledgerItem) map[string]ledgerMatch
	// gateLedgerOutcome records the author's first close of an item. led is the
	// ledger as loaded under the lock, so it can follow `of` links.
	gateLedgerOutcome func(root string, led gateLedger, it ledgerItem, state, of string) error
)

var ledgerStates = []string{"open", "fixed", "accepted", "rejected", "duplicate"}

func ledgerPath(root, rel string) string {
	sum := sha256.Sum256([]byte(rel))
	return filepath.Join(root, "gate-ledger", hex.EncodeToString(sum[:])[:16]+".json")
}

// loadLedger reads the ledger at path. A missing file is an empty ledger and
// exists false; a file that does not parse is an error naming it.
func loadLedger(path, rel string) (led *gateLedger, exists bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &gateLedger{Schema: 1, SpecRel: rel, Next: 1}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("gate ledger %s: %w", path, err)
	}
	var l gateLedger
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, true, fmt.Errorf("gate ledger %s: %w", path, err)
	}
	if l.Schema != 1 {
		return nil, true, fmt.Errorf("gate ledger %s: unknown schema %d", path, l.Schema)
	}
	if l.Next < 1 {
		l.Next = 1
	}
	return &l, true, nil
}

func saveLedger(path string, led *gateLedger) error {
	b, err := json.MarshalIndent(led, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

// withLedgerLock holds the ledger's lock file, never the data file: the data
// file is replaced by a rename.
func withLedgerLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return withFlock(path+".lock", fn)
}

func (l *gateLedger) item(id string) *ledgerItem {
	for i := range l.Items {
		if l.Items[i].ID == id {
			return &l.Items[i]
		}
	}
	return nil
}

// byFinding is the item that has a sighting of the finding ID.
func (l *gateLedger) byFinding(id string) *ledgerItem {
	for i := range l.Items {
		for _, s := range l.Items[i].Seen {
			if s.Finding == id {
				return &l.Items[i]
			}
		}
	}
	return nil
}

func (l *gateLedger) counts() map[string]int {
	c := map[string]int{}
	for _, it := range l.Items {
		c[it.State]++
	}
	return c
}

func (l *gateLedger) countsLine() string {
	c := l.counts()
	return fmt.Sprintf("open %d, fixed %d, accepted %d, rejected %d, duplicate %d", c["open"], c["fixed"], c["accepted"], c["rejected"], c["duplicate"])
}

func ledgerNum(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "L"))
	if err != nil || !strings.HasPrefix(id, "L") {
		return -1
	}
	return n
}

// hint is the matcher's hint for an open item: the root of its choice and the
// rounded probability.
func (l *gateLedger) hint(it *ledgerItem) (string, bool) {
	m := it.Match
	if it.State != "open" || m == nil || m.Choice == "" || m.Choice == "none" || m.P < gateLedgerHintMin {
		return "", false
	}
	root := l.item(m.Choice)
	for hops := 0; root != nil && root.State == "duplicate" && hops <= len(l.Items); hops++ {
		root = l.item(root.Of)
	}
	if root == nil || root.State == "duplicate" {
		return "", false
	}
	return fmt.Sprintf("likely same as %s, %d%%", root.ID, int(m.P*100+0.5)), true
}

// ledgerFinding is a blocker finding of a run with its check key.
type ledgerFinding struct {
	f     gateFinding
	check string
}

// ledgerFindings are the run's verified blocker findings that have an ID.
func ledgerFindings(rec *gateRecord) []ledgerFinding {
	checks := map[string]string{}
	if rec.Review != nil {
		for _, rf := range rec.Review.Findings {
			checks[rf.ID] = rf.ItemID + "/" + rf.CheckID
		}
	}
	var out []ledgerFinding
	seen := map[string]bool{}
	for _, f := range rec.Findings {
		if f.Severity != "blocker" || !f.Verified || f.ID == "" || seen[f.ID] {
			continue
		}
		seen[f.ID] = true
		out = append(out, ledgerFinding{f: f, check: checks[f.ID]})
	}
	return out
}

func newLedgerItem(lf ledgerFinding, hash string, at int64) ledgerItem {
	f := lf.f
	return ledgerItem{State: "open", Severity: f.Severity, Source: f.Source, Check: lf.check, Where: f.Where, Claim: f.Claim, Evidence: f.Evidence, Why: f.Why,
		Seen: []ledgerSighting{{Hash: hash, At: at, Finding: f.ID}}}
}

// ledgerMerge adds the run's findings by exact finding ID. It returns the IDs
// of the items created and of the items seen again, in finding order.
func ledgerMerge(led *gateLedger, fs []ledgerFinding, hash string, at int64) (created, again []string) {
	for _, lf := range fs {
		if it := led.byFinding(lf.f.ID); it != nil {
			it.Seen = append(it.Seen, ledgerSighting{Hash: hash, At: at, Finding: lf.f.ID})
			if it.State == "fixed" {
				it.State, it.Of, it.Reason, it.ClosedAt = "open", "", "", 0
				it.History = append(it.History, ledgerHistory{State: "open", Reason: "seen again in run " + hash, At: at})
			}
			again = append(again, it.ID)
			continue
		}
		it := newLedgerItem(lf, hash, at)
		it.ID = fmt.Sprintf("L%d", led.Next)
		led.Next++
		led.Items = append(led.Items, it)
		created = append(created, it.ID)
	}
	return created, again
}

// recordGateLedger merges the run into the spec's ledger and writes the report
// section. It never changes rec; a failure is a stderr line and a report line.
func (a *app) recordGateLedger(s site, rec *gateRecord, runDir string, now time.Time) {
	if rec.Experiment != "" || rec.SpecRel == "" || (rec.State != "fail" && rec.State != "incomplete" && rec.State != "pass") {
		return
	}
	rel := rec.SpecRel
	path := ledgerPath(s.root, rel)
	fs := ledgerFindings(rec)
	body, err := a.updateLedger(s, rec, rel, path, fs, now)
	if err != nil {
		fmt.Fprintf(a.errw, "orch: gate ledger for %s not updated: %v\n", rel, err)
		body = "Ledger not updated: " + flat(err.Error()) + "\n"
	}
	if body == "" {
		return
	}
	// Best effort: report.md may not be writable.
	_ = setReportSection(filepath.Join(runDir, "report.md"), "## Findings ledger", body)
}

// updateLedger returns the report section body, or "" when the run has nothing
// for a ledger that does not exist yet.
func (a *app) updateLedger(s site, rec *gateRecord, rel, path string, fs []ledgerFinding, now time.Time) (string, error) {
	snap, exists, err := loadLedger(path, rel)
	if err != nil {
		return "", err
	}
	if !exists && len(fs) == 0 {
		return "", nil
	}
	var fresh []ledgerItem
	for _, lf := range fs {
		if snap.byFinding(lf.f.ID) == nil {
			fresh = append(fresh, newLedgerItem(lf, rec.Hash, now.Unix()))
		}
	}
	var answers map[string]ledgerMatch
	if gateLedgerMatch != nil && len(fresh) > 0 {
		var cands []ledgerItem
		for _, it := range snap.Items {
			if it.State != "duplicate" {
				cands = append(cands, it)
			}
		}
		if len(cands) > 0 {
			answers = gateLedgerMatch(context.Background(), s, rel, cands, fresh)
		}
	}
	var body string
	err = withLedgerLock(path, func() error {
		led, _, err := loadLedger(path, rel)
		if err != nil {
			return err
		}
		created, again := ledgerMerge(led, fs, rec.Hash, now.Unix())
		for _, id := range created {
			it := led.item(id)
			if m, ok := answers[it.Seen[0].Finding]; ok {
				m := m
				m.HintAt = 0
				it.Match = &m
			}
		}
		if err := saveLedger(path, led); err != nil {
			return err
		}
		spec := rec.Spec
		if spec == "" {
			spec = rel
		}
		body = ledgerReportBody(led, rel, spec, created, again)
		return nil
	})
	return body, err
}

func ledgerLine(it *ledgerItem) string {
	return short(flat(fmt.Sprintf("%s [%s, %s] %s: %s", it.ID, it.Severity, it.Source, it.Where, it.Claim)), 240)
}

func ledgerReportBody(led *gateLedger, rel, spec string, created, again []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ledger for %s: %s.\n", rel, led.countsLine())
	var parts []string
	if len(created) > 0 {
		parts = append(parts, "new "+strings.Join(created, ", "))
	}
	if len(again) > 0 {
		parts = append(parts, "seen again "+strings.Join(again, ", "))
	}
	if len(parts) == 0 {
		parts = append(parts, "no blocker findings")
	}
	fmt.Fprintf(&b, "This run: %s.\n", strings.Join(parts, "; "))
	touched := map[string]bool{}
	for _, id := range append(append([]string{}, created...), again...) {
		touched[id] = true
	}
	older := 0
	for i := range led.Items {
		it := &led.Items[i]
		if it.State != "open" {
			continue
		}
		if touched[it.ID] {
			b.WriteString(ledgerLine(it) + "\n")
		} else {
			older++
		}
	}
	if older > 0 {
		fmt.Fprintf(&b, "Older open items not seen in this run: %d (list: orch gate resolve %s).\n", older, spec)
	}
	fmt.Fprintf(&b, "Close an item: orch gate resolve %s <L-id> fixed|accepted|rejected|duplicate --reason \"...\" [--of <L-id>]\n", spec)
	return b.String()
}

// gateSpecRel resolves a spec argument to the spec_rel the gate records,
// without parsing the spec, so a spec with format problems can be resolved.
func gateSpecRel(a *app, arg string) (site, string, error) {
	s, code, real, err := gateSpecLocation(a, arg)
	if err != nil {
		return s, "", err
	}
	rel := gateSpecRelOf(code, real)
	if rel == "" {
		return s, "", fail("spec %s: the gate records no path for it", arg)
	}
	return s, rel, nil
}

func cmdGateResolve(a *app, args []string) error {
	fs := a.flags("gate resolve")
	reason := fs.String("reason", "", "why the item is closed or reopened")
	of := fs.String("of", "", "the item a duplicate restates")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 && len(pos) != 3 {
		return &exitError{code: 2, msg: "usage: orch " + gateLedgerUsage}
	}
	var id, state string
	if len(pos) == 3 {
		id, state = pos[1], pos[2]
		valid := false
		for _, st := range ledgerStates {
			valid = valid || st == state
		}
		if !valid {
			return &exitError{code: 2, msg: fmt.Sprintf("state is fixed, accepted, rejected, duplicate or open, not %q", state)}
		}
		if strings.TrimSpace(*reason) == "" {
			return &exitError{code: 2, msg: "--reason must say why"}
		}
		if state == "duplicate" && *of == "" {
			return &exitError{code: 2, msg: "a duplicate needs --of <L-id>"}
		}
		if state != "duplicate" && *of != "" {
			return &exitError{code: 2, msg: "--of is only for duplicate"}
		}
	}
	s, rel, err := gateSpecRel(a, pos[0])
	if err != nil {
		return err
	}
	path := ledgerPath(s.root, rel)
	if len(pos) == 1 {
		return a.listLedger(path, rel, pos[0])
	}
	if _, exists, err := loadLedger(path, rel); err != nil {
		return fail("%v", err)
	} else if !exists {
		return fail("no ledger for %s; run orch gate %s first", rel, pos[0])
	}
	var line string
	err = withLedgerLock(path, func() error {
		led, _, err := loadLedger(path, rel)
		if err != nil {
			return err
		}
		it := led.item(id)
		if it == nil {
			return fail("no item %s in the ledger for %s", id, rel)
		}
		if state == "duplicate" {
			root := led.item(*of)
			switch {
			case root == nil:
				return fail("--of %s: no such item", *of)
			case root.ID == it.ID:
				return fail("--of %s: an item cannot duplicate itself", *of)
			case ledgerNum(root.ID) > ledgerNum(it.ID):
				return fail("--of %s: newer than %s; point at the older item", *of, id)
			case root.State == "duplicate":
				return fail("--of %s is itself a duplicate of %s; use %s", *of, root.Of, root.Of)
			}
		}
		prev := it.State
		first := prev == "open" && state != "open"
		for _, h := range it.History {
			if h.State != "open" {
				first = false
			}
		}
		if first && gateLedgerOutcome != nil {
			if err := gateLedgerOutcome(s.root, *led, *it, state, *of); err != nil {
				fmt.Fprintf(a.errw, "orch: gate ledger outcome for %s not recorded: %v\n", id, err)
			}
		}
		at := a.now().Unix()
		it.State, it.Of, it.Reason, it.ClosedAt = state, *of, strings.TrimSpace(*reason), at
		if state == "open" {
			it.ClosedAt = 0
		}
		it.History = append(it.History, ledgerHistory{State: state, Of: *of, Reason: it.Reason, At: at})
		if err := saveLedger(path, led); err != nil {
			return err
		}
		line = fmt.Sprintf("%s: %s -> %s", id, prev, state)
		return nil
	})
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			return err
		}
		return fail("%v", err)
	}
	fmt.Fprintln(a.out, line)
	return nil
}

func (a *app) listLedger(path, rel, arg string) error {
	led, exists, err := loadLedger(path, rel)
	if err != nil {
		return fail("%v", err)
	}
	if !exists {
		return fail("no ledger for %s; run orch gate %s first", rel, arg)
	}
	items := make([]*ledgerItem, len(led.Items))
	for i := range led.Items {
		items[i] = &led.Items[i]
	}
	rank := map[string]int{}
	for i, st := range []string{"open", "fixed", "accepted", "rejected", "duplicate"} {
		rank[st] = i
	}
	sort.SliceStable(items, func(i, j int) bool {
		if ri, rj := rank[items[i].State], rank[items[j].State]; ri != rj {
			return ri < rj
		}
		return ledgerNum(items[i].ID) < ledgerNum(items[j].ID)
	})
	fmt.Fprintf(a.out, "Ledger for %s: %s\n", rel, led.countsLine())
	var shown []string
	for _, it := range items {
		last := ""
		if n := len(it.Seen); n > 0 {
			last = it.Seen[n-1].Hash
		}
		line := short(flat(fmt.Sprintf("%s %s [%s, %s] runs %d, last %s: %s: %s", it.ID, it.State, it.Severity, it.Source, len(it.Seen), last, it.Where, it.Claim)), 240)
		if it.State != "open" {
			if it.State == "duplicate" {
				line += " | of " + it.Of
			}
			line += " | " + flat(it.Reason)
		}
		if h, ok := led.hint(it); ok {
			line += " | " + h
			if it.Match.HintAt == 0 {
				shown = append(shown, it.ID)
			}
		}
		fmt.Fprintln(a.out, line)
	}
	if len(shown) == 0 {
		return nil
	}
	err = withLedgerLock(path, func() error {
		cur, _, err := loadLedger(path, rel)
		if err != nil {
			return err
		}
		changed := false
		for _, id := range shown {
			if it := cur.item(id); it != nil && it.State == "open" && it.Match != nil && it.Match.HintAt == 0 {
				it.Match.HintAt = a.now().Unix()
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return saveLedger(path, cur)
	})
	if err != nil {
		fmt.Fprintf(a.errw, "orch: gate ledger hint time not saved: %v\n", err)
	}
	return nil
}
