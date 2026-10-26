package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Per-item part of `orch gate compare`: the verdict of each judge for each
// item (the group pass, the shadow decider, the shadow flash judge) next to a
// reference answer key, their agreement and the precision and recall of each
// arm's reuse findings against the key.

// refEntry is one line of a reference file: a strong model's verdict on one
// item. Verdict values: duplicate (or reuse), extend, new, unclear.
type refEntry struct {
	ID         string   `json:"id,omitempty"`
	Revision   string   `json:"revision,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	Issue      string   `json:"-"`
	Package    string   `json:"package"`
	Item       string   `json:"item"` // an item id or the item's text
	Verdict    string   `json:"verdict"`
	Candidate  string   `json:"candidate"`
	Note       string   `json:"note"`
}

func loadReference(path string) ([]refEntry, error) {
	b, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, err
	}
	var out []refEntry
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("reference %s: %v", path, err)
	}
	for i := range out {
		if normVerdict(out[i].Verdict) == "" {
			return nil, fmt.Errorf("reference %s: entry %d has verdict %q; use duplicate, reuse, extend, new or unclear", path, i+1, out[i].Verdict)
		}
	}
	return out, nil
}

// Reference identities never depend on a source location or approximate text.
// Legacy exact text is readable, but needs an explicit mapping for quality.
func resolveReference(ref []refEntry, items []gateItem) map[string]refEntry {
	return resolveMappedReference(ref, items, "", "", nil)
}

type itemMapping struct {
	Key         string             `json:"key"`
	FromItemset string             `json:"from_itemset"`
	ToItemset   string             `json:"to_itemset"`
	Items       []itemMappingEntry `json:"items"`
}
type itemMappingEntry struct {
	KeyID        string   `json:"key_id"`
	KeyRevision  string   `json:"key_revision"`
	ItemID       string   `json:"item_id"`
	ItemRevision string   `json:"item_revision"`
	Candidates   []string `json:"candidates,omitempty"`
}

func readItemMapping(path, key, from, to string) (*itemMapping, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		SchemaVersion int           `json:"schema_version"`
		Mappings      []itemMapping `json:"mappings"`
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("mapping: %w", err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("mapping: expected one JSON document")
	}
	if file.SchemaVersion != 1 {
		return nil, fmt.Errorf("mapping: unsupported schema version %d", file.SchemaVersion)
	}
	var selected *itemMapping
	for _, m := range file.Mappings {
		if m.Key == "" || m.FromItemset == "" || m.ToItemset == "" {
			return nil, fmt.Errorf("mapping: key and both itemsets are required")
		}
		if m.Key != key || m.FromItemset != from || m.ToItemset != to {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("mapping: ambiguous population binding for %s (%s -> %s)", key, from, to)
		}
		copy := m
		selected = &copy
	}
	return selected, nil
}

func resolveMappedReference(ref []refEntry, items []gateItem, from, to string, mapping *itemMapping) map[string]refEntry {
	out := map[string]refEntry{}
	actual := map[string][]gateItem{}
	keys := map[string][]refEntry{}
	for _, it := range items {
		actual[it.ID] = append(actual[it.ID], it)
	}
	for i, r := range ref {
		id := orDefault(r.ID, itemKey(r.Package, r.Item))
		if id == "" {
			id = fmt.Sprintf("entry-%d", i+1)
		}
		r.ID = id
		keys[id] = append(keys[id], r)
	}
	mapped := map[string][]itemMappingEntry{}
	used := map[string]int{}
	invalidMapping := ""
	if mapping != nil {
		for _, m := range mapping.Items {
			mapped[m.KeyID] = append(mapped[m.KeyID], m)
			used[m.ItemID]++
			if len(keys[m.KeyID]) != 1 || len(actual[m.ItemID]) != 1 || len(mapped[m.KeyID]) > 1 || used[m.ItemID] > 1 {
				invalidMapping = "mapping contains unknown or ambiguous identities"
				continue
			}
			if keys[m.KeyID][0].Revision != m.KeyRevision || actual[m.ItemID][0].Revision != m.ItemRevision {
				invalidMapping = "mapping contains stale revisions"
			}
			for _, target := range m.Candidates {
				if !pointerRe.MatchString(target) {
					invalidMapping = "mapping contains invalid evidence target"
				}
			}
		}
	}
	for _, id := range benchSortedKeys(keys) {
		for n, r := range keys[id] {
			target := id
			got := actual[id]
			_, slug, qualified := strings.Cut(id, "#")
			stable := qualified && requirementIDRe.MatchString(slug) && len(got) == 1 && r.Revision != "" && got[0].Revision == r.Revision
			sameSet := from != "" && from == to && len(got) == 1 && (r.Revision == "" || r.Revision == got[0].Revision)
			if len(keys[id]) != 1 {
				r.Issue = "ambiguous key identity"
			} else if !stable && !sameSet {
				ms := mapped[id]
				if len(ms) != 1 {
					r.Issue = "missing or ambiguous explicit mapping"
				} else {
					m := ms[0]
					got = actual[m.ItemID]
					if used[m.ItemID] != 1 || len(got) != 1 || m.KeyRevision != r.Revision || m.ItemRevision != got[0].Revision {
						r.Issue = "ambiguous or stale mapping"
					} else {
						target = m.ItemID
						if len(m.Candidates) > 0 {
							r.Candidate = ""
							r.Candidates = m.Candidates
						}
					}
				}
			}
			if ms := mapped[id]; (stable || sameSet) && len(ms) == 1 {
				if ms[0].ItemID != id {
					r.Issue = "mapping conflicts with exact item identity"
				} else if len(ms[0].Candidates) > 0 {
					r.Candidate = ""
					r.Candidates = ms[0].Candidates
				}
			}
			if invalidMapping != "" {
				r.Issue = invalidMapping
			}
			if r.Issue != "" {
				target = fmt.Sprintf("unmapped:%s:%d", id, n+1)
			}
			if _, exists := out[target]; exists {
				r.Issue = "ambiguous target identity"
				target = fmt.Sprintf("unmapped:%s:%d", id, n+1)
			}
			out[target] = r
		}
	}
	return out
}

func referenceIssues(ref map[string]refEntry) []string {
	var out []string
	for _, id := range benchSortedKeys(ref) {
		if r := ref[id]; r.Issue != "" {
			out = append(out, r.ID+": "+r.Issue)
		}
	}
	return out
}

func (r refEntry) targets() []string {
	out := append([]string{}, r.Candidates...)
	if r.Candidate != "" {
		out = append(out, r.Candidate)
	}
	return out
}

type scoredAnswer struct {
	Revision  string
	Verdict   string
	Candidate string
	Present   bool
	Valid     bool
	Supported bool
}

// armData is what compare knows about one experiment's items.
type armData struct {
	name          string
	answers       map[string]map[string]scoredAnswer
	evidence      []evidenceRecord
	manifestKnown bool
	reference     map[string]refEntry
	locations     map[string]map[string]map[string]bool // judge -> verdict -> locations
	checklist     map[string][]itemCands
	stats         string
	items         []gateItem
	group         map[string]string
	judges        map[string]map[string]string // judge -> item -> verdict
	rows          []shadowRow
	pass          map[string]float64         // reuse-finding positives by verdict kind, not used when group is set
	finding       map[string]map[string]bool // verdict kind -> item ids, from the findings of a run without item results
}

func readItemsFile(dir string) []gateItem {
	b, err := os.ReadFile(filepath.Join(dir, "items.json"))
	if err != nil {
		return nil
	}
	var its []gateItem
	_ = json.Unmarshal(b, &its)
	return its
}

func readShadowRows(gdir, name string) []shadowRow {
	b, err := os.ReadFile(filepath.Join(shadowDir(gdir, name), "items.jsonl"))
	if err != nil {
		return nil
	}
	var out []shadowRow
	for _, l := range strings.Split(string(b), "\n") {
		var r shadowRow
		if l != "" && json.Unmarshal([]byte(l), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func loadArm(gdir, name, dir string, rec *gateRecord) *armData {
	a := &armData{name: name, items: readItemsFile(dir), group: map[string]string{}, judges: map[string]map[string]string{}, finding: map[string]map[string]bool{}, answers: map[string]map[string]scoredAnswer{"group": {}}}
	for _, r := range rec.Items {
		if r.Verdict != "" {
			a.group[r.ID] = normVerdict(r.Verdict)
		}
		v := normVerdict(r.Verdict)
		valid := v != "" && (r.AnswerValid == nil || *r.AnswerValid) && ((v != verdictReuse && v != verdictExtend) || r.Verified)
		a.answers["group"][r.ID] = scoredAnswer{Revision: r.Revision, Verdict: v, Candidate: r.Candidate, Present: r.Verdict != "", Valid: valid}
	}
	if rec.Review != nil {
		replayed := ReduceReview(rec.Subject, rec.Review.Checks, rec.Review.Answers, rec.Review.Findings, rec.Review.Policy)
		bindingValid := replayed.State != "error" && replayed.State != "stale"
		unsupported := map[string]bool{}
		for _, reason := range replayed.Reasons {
			if reason.Reason == reviewUnsupportedClaim {
				unsupported[reason.Check] = true
			}
		}
		for _, e := range rec.Review.Policy.Packet.Evidence {
			if rec.Review.Policy.Packet.ValidateAttachment([]string{e.ID}, rec.Review.Policy.CurrentSubject, rec.Review.Policy.CurrentHashes) == nil {
				a.evidence = append(a.evidence, e)
			}
		}
		a.manifestKnown = bindingValid
		// Current replayable judgments take precedence over legacy projections.
		a.answers["group"] = map[string]scoredAnswer{}
		for _, check := range rec.Review.Checks {
			if check.ID != "reuse" {
				continue
			}
			var matches []reviewAnswer
			for _, answer := range rec.Review.Answers {
				if answer.ItemID == check.Item.ID && answer.CheckID == check.ID {
					matches = append(matches, answer)
				}
			}
			ans := scoredAnswer{Present: len(matches) > 0}
			if len(matches) == 1 {
				r := matches[0]
				ans.Present = r.AnswerState != reviewAnswerMissing && r.AnswerState != reviewAnswerUnavailable && r.AnswerState != reviewAnswerBudget
				ans.Revision = r.ItemRevision
				ans.Verdict = normVerdict(string(r.Judgment))
				ans.Valid = bindingValid && r.AnswerState == reviewAnswerAnswered && ans.Verdict != "" && r.Validate(check.Item, rec.Review.Policy.Packet, rec.Review.Policy.CurrentSubject, rec.Review.Policy.CurrentHashes) == nil
				ans.Supported = ans.Valid && allowedSupport(r.Support, check) && !unsupported[check.key()]
				for _, it := range rec.Items {
					if it.ID == r.ItemID && it.AnswerID == r.ID && it.Revision == r.ItemRevision && it.Verified && (it.AnswerValid == nil || *it.AnswerValid) {
						ans.Candidate = it.Candidate
					}
				}
				if ans.Verdict == verdictReuse || ans.Verdict == verdictExtend {
					ans.Valid = ans.Valid && ans.Candidate != ""
				}
			}
			a.answers["group"][check.Item.ID] = ans
		}
	}
	if len(a.items) == 0 && rec.Review != nil {
		for _, check := range rec.Review.Checks {
			if check.ID == "reuse" {
				a.items = append(a.items, gateItem{ID: check.Item.ID, Package: check.Item.Package, Text: check.Item.Requirement, Revision: check.Item.Revision})
			}
		}
	}
	if len(a.items) == 0 {
		for _, r := range rec.Items {
			a.items = append(a.items, gateItem{ID: r.ID, Package: r.Package, Text: r.Text, Revision: r.Revision})
		}
	}
	a.locations = map[string]map[string]map[string]bool{}
	b, _ := os.ReadFile(filepath.Join(dir, "checklist.json"))
	_ = json.Unmarshal(b, &a.checklist)
	a.stats = checklistStats(dir, a.checklist)
	if a.checklist == nil {
		a.checklist = legacyChecklist(dir)
		if len(a.checklist) == 0 {
			a.checklist = nil
		}
	}
	for _, r := range rec.Items {
		if r.Verified && (r.AnswerValid == nil || *r.AnswerValid) {
			a.addLocation("group", r.Verdict, r.Candidate)
		}
	}
	for _, f := range rec.Findings {
		kind := map[string]string{"reuse": verdictReuse, "extend": verdictExtend}[f.Source]
		if f.Verified {
			a.addLocation("group", kind, f.Evidence)
		}
	}
	a.rows = readShadowRows(gdir, name)
	for _, r := range a.rows {
		if r.Verdict == "" {
			continue
		}
		if a.judges[r.Judge] == nil {
			a.judges[r.Judge] = map[string]string{}
		}
		a.judges[r.Judge][r.Item] = normVerdict(r.Verdict)
		if a.answers[r.Judge] == nil {
			a.answers[r.Judge] = map[string]scoredAnswer{}
		}
		a.answers[r.Judge][r.Item] = scoredAnswer{Verdict: normVerdict(r.Verdict), Candidate: r.At, Present: true, Valid: r.Error == "" && r.Skipped == "" && normVerdict(r.Verdict) != ""}
		// Historical shadow rows have no support assessment. Keep them readable,
		// but do not turn a candidate number into a supported quality positive.
		if r.At != "" {
			a.addLocation(r.Judge, r.Verdict, r.At)
		} else if n, err := strconv.Atoi(r.Best); err == nil {
			for _, ic := range a.checklist[r.Package] {
				if ic.Item.ID == r.Item && n > 0 && n <= len(ic.Cands) {
					a.addLocation(r.Judge, r.Verdict, ic.Cands[n-1].at())
				}
			}
		}
	}
	return a
}

func (a *armData) answer(judge, id string) scoredAnswer {
	ans := a.answers[judge][id]
	for _, item := range a.items {
		if item.ID == id && item.Revision != "" && item.Revision != ans.Revision {
			ans.Valid = false
			ans.Supported = false
		}
	}
	return ans
}

func (a *armData) verdict(judge, id string) string {
	if ans, ok := a.answers[judge][id]; ok {
		return ans.Verdict
	}
	if judge == "group" {
		return a.group[id]
	}
	return a.judges[judge][id]
}

func pct(n, d int) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d%%", n*100/d)
}

func printItemComparison(w io.Writer, arms []*armData, ref map[string]refEntry, hasRef bool) {
	for i, a := range arms {
		fmt.Fprintf(w, "checklist %s: %s\n", string(rune('A'+i)), orDefault(a.stats, "not recorded"))
	}
	itemsByID := map[string]gateItem{}
	var order []string
	for _, a := range arms {
		for _, it := range a.items {
			if _, ok := itemsByID[it.ID]; !ok {
				itemsByID[it.ID] = it
				order = append(order, it.ID)
			}
		}
	}
	if len(order) == 0 {
		return
	}
	judges := []string{"group", "decider", "flash"}
	cell := func(a *armData, id string) string {
		var parts []string
		for _, j := range judges {
			parts = append(parts, j+"="+orDefault(a.verdict(j, id), "-"))
		}
		for _, item := range a.items {
			if item.ID == id && item.Revision != "" {
				parts = append(parts, "revision="+item.Revision)
				break
			}
		}
		return strings.Join(parts, " ")
	}
	fmt.Fprintf(w, "per-item verdicts (%d items; group = the group pass, decider and flash = shadow judges, record only; raw ID columns do not assert revision equivalence):\n", len(order))
	for _, id := range order {
		var line strings.Builder
		fmt.Fprintf(&line, "  %s \"%s\"", id, short(flat(itemsByID[id].Text), 40))
		for i, a := range arms {
			fmt.Fprintf(&line, " | %s [%s]", string(rune('A'+i)), cell(a, id))
		}
		if hasRef {
			line.WriteString(" | reference=")
			line.WriteString(orDefault(normVerdict(ref[id].Verdict), "-"))
		}
		fmt.Fprintln(w, line.String())
	}
	// Agreement between each pair of judges.
	type judge struct {
		label string
		get   func(id string) string
	}
	var js []judge
	for i, a := range arms {
		for _, j := range judges {
			a, j := a, j
			js = append(js, judge{string(rune('A'+i)) + "." + j, func(id string) string { return a.verdict(j, id) }})
		}
	}
	sameArm := func(x, y string) bool { return x[0] == y[0] && len(x) > 1 && len(y) > 1 && x[1] == '.' && y[1] == '.' }
	for i := range js {
		for k := i + 1; k < len(js); k++ {
			x, y := js[i], js[k]
			related := sameArm(x.label, y.label) || y.label == "reference" || (x.label[2:] == y.label[min(2, len(y.label)):] && x.label[0] != y.label[0])
			if !related {
				continue
			}
			both, agree := 0, 0
			var dis []string
			for _, id := range order {
				vx, vy := x.get(id), y.get(id)
				if vx == "" || vy == "" {
					continue
				}
				both++
				if vx == vy {
					agree++
				} else if len(dis) < 10 {
					dis = append(dis, fmt.Sprintf("    %s \"%s\": %s vs %s", id, short(flat(itemsByID[id].Text), 80), vx, vy))
				}
			}
			if both == 0 {
				continue
			}
			fmt.Fprintf(w, "legacy raw agreement %s ~ %s: %d/%d (%s; paired raw answers only, excludes missing; not support accuracy)\n", x.label, y.label, agree, both, pct(agree, both))
			for _, d := range dis {
				fmt.Fprintln(w, d)
			}
		}
	}
	// Cost and time of each judge.
	for i, a := range arms {
		for _, j := range []string{"decider", "flash"} {
			n, errs := 0, 0
			var cost, secs float64
			tokens := 0
			for _, r := range a.rows {
				if r.Judge != j {
					continue
				}
				n++
				cost += r.Cost
				secs += r.Seconds
				tokens += r.Tokens
				if r.Error != "" {
					errs++
				}
			}
			if n > 0 {
				fmt.Fprintf(w, "judge %s.%s: %d rows, %d errors, $%.4f, %.0fs, %d tokens\n", string(rune('A'+i)), j, n, errs, cost, secs, tokens)
			}
		}
	}
	if !hasRef {
		return
	}
	printKeyView(w, arms, ref)
	printLocationScores(w, arms, ref, judges)
}

func printLocationScores(w io.Writer, arms []*armData, ref map[string]refEntry, judges []string) {
	for i, a := range arms {
		refs := ref
		if a.reference != nil {
			refs = a.reference
		}
		for _, issue := range referenceIssues(refs) {
			fmt.Fprintf(w, "mapping %s: %s; dependent quality unavailable\n", string(rune('A'+i)), issue)
		}
		for _, kind := range []string{verdictReuse, verdictExtend, verdictNew, verdictUnclr} {
			for _, j := range judges {
				s := scoreLocations(a, refs, j, kind)
				if s.Unavailable != "" {
					fmt.Fprintf(w, "reference %s, %s.%s (behaviors): unavailable (identity mapping)\n", kind, string(rune('A'+i)), j)
				} else {
					fmt.Fprintf(w, "reference %s, %s.%s (behaviors): %d supported predictions, %d keyed behaviors, %d matches; precision %s, recall %s; %d missing/invalid/unsupported excluded from precision; non-key predictions excluded\n", kind, string(rune('A'+i)), j, s.Found, s.Total, s.Match, pct(s.Match, s.Found), pct(s.Match, s.Total), s.Excluded)
				}
				if kind == verdictReuse || kind == verdictExtend {
					legacy := scoreLegacyLocations(a, refs, j, kind)
					fmt.Fprintf(w, "legacy reference %s, %s.%s (locations, not behavior quality): %d found, %d in the reference, %d match; precision %s, recall %s\n", kind, string(rune('A'+i)), j, legacy.Found, legacy.Total, legacy.Match, pct(legacy.Match, legacy.Found), pct(legacy.Match, legacy.Total))
				}
			}
		}
	}
}
