package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"orchestrate/orch/guard"
)

// `orch lookups report` reads the lookup log the lookup coach writes
// (orch-guard coach for Claude, tool/pi_ctx_reminder.ts for pi):
// one JSON line per lookup in $ORCH_TOOL_GUARD_STATE_DIR/lookups.jsonl. It
// answers whether workers picked the right tool, in the right order, in few turns.

// lookupLine is a row of the log; orch-guard writes it, so the type lives in guard.
type lookupLine = guard.LookupLine

type lookupToolStats struct {
	Lookups int     `json:"lookups"`
	Empty   int     `json:"empty"`
	Errors  int     `json:"errors"`
	EmptyPc float64 `json:"empty_rate"`
	ErrorPc float64 `json:"error_rate"`
}

type lookupReminderStats struct {
	Fired    int `json:"fired"`
	Followed int `json:"followed"`
	Ignored  int `json:"ignored"`
	// FollowRate is Followed/(Followed+Ignored), -1 when no answer is known (the
	// batch reminders carry none).
	FollowRate float64 `json:"follow_rate"`
}

type lookupHarnessReport struct {
	Harness  string `json:"harness"`
	Sessions int    `json:"sessions"`
	Lookups  int    `json:"lookups"`
	// Turns counts the turns that held a lookup; lookups without a turn are left out of the per-turn figures.
	Turns             int                            `json:"turns"`
	LookupsPerTurn    float64                        `json:"lookups_per_turn"`
	TurnsWithTwoPlus  float64                        `json:"share_turns_with_2plus"`
	SessionsFirstGood float64                        `json:"share_sessions_first_search_jbcontext_or_lsp"`
	SessionsWithFirst int                            `json:"sessions_with_a_search"`
	SymbolRg          int                            `json:"rg_symbol_pattern_calls"`
	Tools             map[string]lookupToolStats     `json:"tools"`
	Reminders         map[string]lookupReminderStats `json:"reminders"`
	RepeatedReads     []lookupRepeat                 `json:"repeated_reads"`
}

type lookupRepeat struct {
	Session string `json:"session"`
	Query   string `json:"query"`
	Count   int    `json:"count"`
}

type lookupReport struct {
	Since     string                `json:"since,omitempty"`
	Repo      string                `json:"repo,omitempty"`
	Log       string                `json:"log"`
	Lookups   int                   `json:"lookups"`
	Harnesses []lookupHarnessReport `json:"harnesses"`
}

// parseSince reads 90m, 36h or 7d.
func parseSince(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return d, nil
}

func cmdLookups(a *app, args []string) error {
	if len(args) == 0 || args[0] != "report" {
		return &exitError{code: 2, msg: "usage: orch lookups report [--since DUR] [--repo PATH] [--json]"}
	}
	fs := a.flags("lookups report")
	since := fs.String("since", "", "only lookups this recent, such as 90m, 36h or 7d")
	repo := fs.String("repo", "", "only lookups made in this repository")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return &exitError{code: 2, msg: "usage: orch lookups report [--since DUR] [--repo PATH] [--json]"}
	}
	var cutoff time.Time
	if *since != "" {
		d, err := parseSince(*since)
		if err != nil {
			return fail("--since %q is not a duration such as 36h or 7d", *since)
		}
		cutoff = a.now().Add(-d)
	}
	repoPath := ""
	if *repo != "" {
		if repoPath, err = filepath.Abs(*repo); err != nil {
			return fail("--repo: %v", err)
		}
	}
	path := guard.LogPath()
	lines, err := readLookupLog(path, cutoff, repoPath)
	if err != nil {
		return err
	}
	report := buildLookupReport(lines)
	report.Since, report.Repo, report.Log = *since, repoPath, path
	if *asJSON {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printLookupReport(a, report)
	return nil
}

// readLookupLog keeps the lines inside the window and repository, in file order.
// A missing log is an empty one, and a line that is not JSON is skipped.
func readLookupLog(path string, cutoff time.Time, repo string) ([]lookupLine, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []lookupLine
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scan.Scan() {
		var l lookupLine
		if json.Unmarshal(scan.Bytes(), &l) != nil || l.Tool == "" {
			continue
		}
		if !cutoff.IsZero() {
			if ts, err := time.Parse(time.RFC3339Nano, l.TS); err != nil || ts.Before(cutoff) {
				continue
			}
		}
		if repo != "" && l.Repo != repo && !strings.HasPrefix(l.Repo, repo+string(filepath.Separator)) {
			continue
		}
		out = append(out, l)
	}
	return out, scan.Err()
}

var (
	lookupIdent  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	lookupLead   = regexp.MustCompile(`^(func|def|class|type|interface) `)
	lookupTrail  = regexp.MustCompile(`\\?\($`)
	lookupCapAft = regexp.MustCompile(`[A-Z]`)
)

// symbolPattern applies the symbol-via-lsp rule to an rg pattern alone: the log
// keeps neither the paths nor the -w flag, so a bare lowercase word is not counted.
func symbolPattern(pattern string) bool {
	s := strings.NewReplacer(`\b`, "", `\<`, "", `\>`, "").Replace(pattern)
	s = lookupTrail.ReplaceAllString(lookupLead.ReplaceAllString(s, ""), "")
	if !lookupIdent.MatchString(s) {
		return false
	}
	return strings.Contains(s, ".") || strings.Contains(s, "_") || lookupCapAft.MatchString(s[1:])
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func buildLookupReport(lines []lookupLine) lookupReport {
	byHarness := map[string][]lookupLine{}
	for _, l := range lines {
		byHarness[l.Harness] = append(byHarness[l.Harness], l)
	}
	names := make([]string, 0, len(byHarness))
	for name := range byHarness {
		names = append(names, name)
	}
	sort.Strings(names)
	report := lookupReport{Lookups: len(lines)}
	for _, name := range names {
		report.Harnesses = append(report.Harnesses, harnessReport(name, byHarness[name]))
	}
	return report
}

func harnessReport(name string, lines []lookupLine) lookupHarnessReport {
	r := lookupHarnessReport{
		Harness:   name,
		Lookups:   len(lines),
		Tools:     map[string]lookupToolStats{},
		Reminders: map[string]lookupReminderStats{},
	}
	sessions := map[string]bool{}
	firstSeen := map[string]bool{}
	turns := map[string]int{}
	withTurn := 0
	pending := map[string]string{}
	reads := map[string]map[string]int{}
	var firstGood int
	for _, l := range lines {
		sessions[l.Session] = true
		stats := r.Tools[l.Tool]
		stats.Lookups++
		if l.Empty {
			stats.Empty++
		}
		if !l.OK {
			stats.Errors++
		}
		r.Tools[l.Tool] = stats
		if l.Turn != nil {
			turns[l.Session+"\x00"+fmt.Sprint(l.Turn)]++
			withTurn++
		}
		if l.Tool != "read" && !firstSeen[l.Session] {
			firstSeen[l.Session] = true
			r.SessionsWithFirst++
			if l.Tool == "jbcontext" || strings.HasPrefix(l.Tool, "lsp:") {
				firstGood++
			}
		}
		if l.Tool == "rg" && symbolPattern(l.Query) {
			r.SymbolRg++
		}
		if l.Followed != nil && pending[l.Session] != "" {
			rs := r.Reminders[pending[l.Session]]
			if *l.Followed {
				rs.Followed++
			} else {
				rs.Ignored++
			}
			r.Reminders[pending[l.Session]] = rs
			pending[l.Session] = ""
		}
		if l.Reminder != nil {
			rs := r.Reminders[*l.Reminder]
			rs.Fired++
			r.Reminders[*l.Reminder] = rs
			if *l.Reminder == "concept-first" || *l.Reminder == "symbol-via-lsp" {
				pending[l.Session] = *l.Reminder
			}
		}
		if l.Tool == "read" {
			if reads[l.Session] == nil {
				reads[l.Session] = map[string]int{}
			}
			reads[l.Session][l.Query]++
		}
	}
	r.Sessions = len(sessions)
	r.Turns = len(turns)
	r.LookupsPerTurn = ratio(withTurn, len(turns))
	multi := 0
	for _, n := range turns {
		if n >= 2 {
			multi++
		}
	}
	r.TurnsWithTwoPlus = ratio(multi, len(turns))
	r.SessionsFirstGood = ratio(firstGood, r.SessionsWithFirst)
	for tool, s := range r.Tools {
		s.EmptyPc, s.ErrorPc = ratio(s.Empty, s.Lookups), ratio(s.Errors, s.Lookups)
		r.Tools[tool] = s
	}
	for kind, s := range r.Reminders {
		s.FollowRate = -1
		if s.Followed+s.Ignored > 0 {
			s.FollowRate = ratio(s.Followed, s.Followed+s.Ignored)
		}
		r.Reminders[kind] = s
	}
	for session, queries := range reads {
		for q, n := range queries {
			if n > 1 {
				r.RepeatedReads = append(r.RepeatedReads, lookupRepeat{Session: session, Query: q, Count: n})
			}
		}
	}
	sort.Slice(r.RepeatedReads, func(i, j int) bool {
		x, y := r.RepeatedReads[i], r.RepeatedReads[j]
		if x.Count != y.Count {
			return x.Count > y.Count
		}
		return x.Session+x.Query < y.Session+y.Query
	})
	return r
}

func lookupPct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func printLookupReport(a *app, report lookupReport) {
	if report.Lookups == 0 {
		fmt.Fprintf(a.out, "no lookups logged in %s\n", report.Log)
		return
	}
	for _, h := range report.Harnesses {
		fmt.Fprintf(a.out, "== %s ==\n", h.Harness)
		w := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "sessions\t%d\n", h.Sessions)
		fmt.Fprintf(w, "lookups\t%d\n", h.Lookups)
		fmt.Fprintf(w, "turns with a lookup\t%d\n", h.Turns)
		fmt.Fprintf(w, "lookups per turn\t%.2f mean, %s of turns with 2+\n", h.LookupsPerTurn, lookupPct(h.TurnsWithTwoPlus))
		fmt.Fprintf(w, "first search was jbcontext or lsp\t%s of %d sessions\n", lookupPct(h.SessionsFirstGood), h.SessionsWithFirst)
		fmt.Fprintf(w, "rg calls with a symbol pattern\t%d\n", h.SymbolRg)
		w.Flush()
		fmt.Fprintln(a.out)
		w = tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "tool\tlookups\tempty\terror")
		for _, tool := range sortedKeys(h.Tools) {
			s := h.Tools[tool]
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", tool, s.Lookups, lookupPct(s.EmptyPc), lookupPct(s.ErrorPc))
		}
		w.Flush()
		if len(h.Reminders) > 0 {
			fmt.Fprintln(a.out)
			w = tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "reminder\tfired\tfollowed")
			for _, kind := range sortedKeys(h.Reminders) {
				s := h.Reminders[kind]
				followed := "n/a"
				if s.FollowRate >= 0 {
					followed = fmt.Sprintf("%s (%d of %d)", lookupPct(s.FollowRate), s.Followed, s.Followed+s.Ignored)
				}
				fmt.Fprintf(w, "%s\t%d\t%s\n", kind, s.Fired, followed)
			}
			w.Flush()
		}
		if len(h.RepeatedReads) > 0 {
			fmt.Fprintln(a.out, "\nthe same read repeated in a session:")
			for _, rr := range h.RepeatedReads {
				fmt.Fprintf(a.out, "  %dx %s (%s)\n", rr.Count, rr.Query, rr.Session)
			}
		}
		fmt.Fprintln(a.out)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
