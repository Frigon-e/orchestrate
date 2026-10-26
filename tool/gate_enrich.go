package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Enrichment makes the gate's question cards easier to settle, with no model
// call: only whitespace-normalized exact duplicates share a card. Related
// questions keep independent cards and routes. Spec and code hints are bounded
// search results, not proof that an answer is absent or that a question is settled.

const (
	enrichSpecShare  = 0.6 // share of a question's identifiers a spec line must hold
	enrichMaxHints   = 3
	enrichSnippet    = ctxSnippet
	enrichParallel   = 8
	enrichQueryChars = 300
	enrichMaxFails   = 3 // failed searches after which the rest are skipped
)

// gateOwnerWords marks a question the owner should decide: money, prices,
// tax, refunds, legal, privacy, retention, consent, GDPR, permissions, roles,
// security, wording or naming users see, default behaviour and policy. Words
// match from the start of a word, so "refund" also finds "refunds".
var gateOwnerWords = []string{
	"money", "pric", "tax", "refund", "legal", "privacy", "retention", "consent", "gdpr",
	"permission", "role", "security", "user-visible", "user visible", "wording",
	"shown to users", "shown to the user", "shown to customers", "shown to the customer",
	"default behaviour", "default behavior", "policy", "policies",
}

var ownerRe = regexp.MustCompile(`(?i)\b(?:` + strings.Join(escapeAll(gateOwnerWords), "|") + `)`)

func escapeAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = regexp.QuoteMeta(s)
	}
	return out
}

var enrichStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the is are was were be been being do does did of to in on at for from by with as and or not no
		it its this that these those which what whom who how when where whether should would could can may might must will shall
		if then than so such into onto per via vs there their they them we us our you your i or any each every both either
		one more most other same also only just still yet`) {
		enrichStop[w] = true
	}
}

type specHint struct {
	Line int
	Text string
}

// gateQuestionPacket is supplied by the original reviewer, never a formatter.
// Missing fields stay missing; routing advice does not grant authority.
type gateQuestionPacket struct {
	Goal           string             `json:"goal"`
	KnownFacts     []string           `json:"known_facts"`
	EvidenceIDs    []string           `json:"evidence_ids"`
	Unknown        string             `json:"unknown"`
	Options        []gatePacketOption `json:"options"`
	Recommendation string             `json:"recommendation"`
	Rationale      string             `json:"rationale"`
	Authority      string             `json:"authority"`
	NextAction     string             `json:"next_action"`
}
type gatePacketOption struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Gain string `json:"gain"`
	Cost string `json:"cost"`
}
type gateQuestionDelivery struct {
	ID        string             `json:"id"`
	Package   string             `json:"package"`
	Packet    gateQuestionPacket `json:"packet"`
	Readiness string             `json:"readiness"`
	Missing   []string           `json:"missing,omitempty"`
	Search    []evidenceLookup   `json:"search"`
}

type enrichedQuestion struct {
	Readiness  cardReadiness
	Search     []evidenceLookup
	N          int // the question's card number in the gate
	Record     int // one-based position in gate.json, stable for this gate run
	Q          gateQuestion
	Merged     []string           // original text of exact normalized duplicates
	Members    []enrichedQuestion // each duplicate's original identity and route
	Answer     string             // archived card or the gate's answer evidence, never inferred from hints
	CodeReview string             // what the bounded code search actually established
	Tag        string
	Why        string
	Spec       []specHint
	Code       []codeHit
	Facts      []symFact
	Card       string // file name without .md
	Blocker    bool
}

type enrichResult struct {
	Total, Cards         int
	Owner, Lead, Unclear int
	SpecHints, CodeHits  int
	SymbolFacts          int
	Minor                []enrichedQuestion
	Answered             []enrichedQuestion
	Items                []enrichedQuestion
	Notes                []string
}

func (r *enrichResult) summary() string {
	return fmt.Sprintf("%d -> %d after merging; owner %d, lead-may-decide %d, unclear %d; spec hints %d; code hits %d; symbol facts %d",
		r.Total, r.Cards, r.Owner, r.Lead, r.Unclear, r.SpecHints, r.CodeHits, r.SymbolFacts)
}

// tokens are the words of a question that carry meaning: lowercase, without
// punctuation or stop words.
func questionTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range regexp.MustCompile(`[a-z0-9_]+`).FindAllString(strings.ToLower(s), -1) {
		if len(w) > 1 && !enrichStop[w] {
			out[w] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

var (
	idBacktick = regexp.MustCompile("`([^`\n]{2,60})`")
	idCode     = regexp.MustCompile(`\b[A-Z]{2,6}-\d{1,4}\b`)
	idSnake    = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)
	idCamel    = regexp.MustCompile(`\b(?:[A-Z][a-z0-9]+){2,}\b|\b[a-z]+(?:[A-Z][a-z0-9]*)+\b`)
	idDotted   = regexp.MustCompile(`\b[a-z][A-Za-z0-9]*\.[A-Z][A-Za-z0-9]*\b`)
	idPath     = regexp.MustCompile(`[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+/?`)
	idUnit     = regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s?(?:ms|seconds?|secs?|minutes?|mins?|hours?|days?|mb|kb|gb)\b`)
)

// questionIdentifiers lists what a question names exactly: backticked spans,
// snake_case and CamelCase words, ids like FRD-03, dotted calls, file paths
// and numbers with units. They are lowercased and de-duplicated.
func questionIdentifiers(q string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if len(s) >= 2 && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, m := range idBacktick.FindAllStringSubmatch(q, -1) {
		span := m[1]
		if i := strings.Index(span, "("); i > 0 {
			span = span[:i]
		}
		if !strings.Contains(span, " ") {
			add(span)
		}
	}
	for _, re := range []*regexp.Regexp{idCode, idSnake, idCamel, idDotted, idUnit} {
		for _, m := range re.FindAllString(q, -1) {
			add(m)
		}
	}
	for _, m := range idPath.FindAllString(q, -1) {
		m = strings.Trim(m, ".")
		last := m[strings.LastIndex(strings.TrimSuffix(m, "/"), "/")+1:]
		if len(m) >= 5 && (strings.Contains(last, ".") || strings.HasSuffix(m, "/") || strings.Count(m, "/") >= 2) {
			add(m)
		}
	}
	return out
}

// specHints finds spec places that hold at least enrichSpecShare of the
// question's identifiers (and at least two). It looks at windows of three
// lines, because the spec wraps its prose, and reports the first line of the
// window that holds one. The result is a hint only.
func specHints(lines []string, ids []string) []specHint {
	if len(ids) < 2 {
		return nil
	}
	need := max(2, int(math.Ceil(enrichSpecShare*float64(len(ids)))))
	lower := make([]string, len(lines))
	for i, l := range lines {
		lower[i] = strings.ToLower(l)
	}
	type cand struct{ at, n int }
	var cands []cand
	for i := range lines {
		win := strings.Join(lower[i:min(i+3, len(lines))], "\n")
		n := 0
		for _, id := range ids {
			if strings.Contains(win, id) {
				n++
			}
		}
		if n >= need {
			cands = append(cands, cand{i, n})
		}
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].n > cands[b].n })
	var out []specHint
	var taken []int
	for _, c := range cands {
		near := false
		for _, t := range taken {
			if c.at-t < 3 && t-c.at < 3 {
				near = true
			}
		}
		if near {
			continue
		}
		taken = append(taken, c.at)
		line := c.at
		for k := c.at; k < min(c.at+3, len(lines)); k++ {
			for _, id := range ids {
				if strings.Contains(lower[k], id) {
					line = k
					goto found
				}
			}
		}
	found:
		out = append(out, specHint{Line: line + 1, Text: short(strings.TrimSpace(lines[line]), 160)})
		if len(out) == enrichMaxHints {
			break
		}
	}
	return out
}

// routeQuestion advises who should decide. An owner word wins over an
// Authority "May decide" phrase; with neither the question is unclear.
func routeQuestion(q string, mayDecide []string) (tag, why string) {
	if m := ownerRe.FindString(q); m != "" {
		return "owner", "mentions " + strings.ToLower(m)
	}
	low := strings.ToLower(q)
	for _, p := range mayDecide {
		if phraseIn(low, p) {
			return "lead-may-decide", "the spec's Authority lets a lead decide: " + p
		}
	}
	return "unclear", "no routing rule matched"
}

// phraseIn reports whether every word of phrase occurs in text, ignoring a
// plural s, so "query shapes" finds "the query shape".
func phraseIn(text, phrase string) bool {
	words := strings.Fields(phrase)
	if len(words) == 0 {
		return false
	}
	if len(words) == 1 {
		return strings.Contains(text, phrase)
	}
	for _, w := range words {
		if !strings.Contains(text, strings.TrimSuffix(w, "s")) {
			return false
		}
	}
	return true
}

// mayDecidePhrases are the phrases of the Authority "May decide:" line that
// can match a question: two or more words, or an identifier.
func mayDecidePhrases(sp *spec) []string {
	var out []string
	for _, p := range strings.Split(labelled(sp.sections["Authority"], "May decide"), ",") {
		p = strings.ToLower(strings.Trim(strings.TrimSpace(p), "`.;"))
		if len(strings.Fields(p)) >= 2 || strings.ContainsAny(p, "_./") {
			out = append(out, p)
		}
	}
	return out
}

// dedupeQuestions only collapses identical text after whitespace normalization,
// within a package and answer state. Do not discard punctuation, case, negation,
// word order or additional evidence needs. The first identity remains canonical.
func dedupeQuestions(items []enrichedQuestion) []enrichedQuestion {
	type identity struct {
		pkg, question, answer, archive string
		decisive                       bool
		packet                         string
	}
	seen := map[identity]int{}
	var out []enrichedQuestion
	for _, it := range items {
		key := identity{it.Q.Package, flat(it.Q.Question), it.Q.AnsweredBy, it.Answer, it.Q.Decisive, subjectJSONDigest(it.Q.Packet)}
		if i, ok := seen[key]; ok {
			out[i].Merged = append(out[i].Merged, it.Q.Question)
			out[i].Members = append(out[i].Members, it)
			if routeRank(it.Tag) < routeRank(out[i].Tag) {
				out[i].Tag, out[i].Why = it.Tag, it.Why
			}
			continue
		}
		seen[key] = len(out)
		it.Members = []enrichedQuestion{it}
		out = append(out, it)
	}
	return out
}

func routeRank(tag string) int {
	switch tag {
	case "owner":
		return 0
	case "lead-may-decide":
		return 2
	default:
		return 1
	}
}

func questionAction(tag string) string {
	switch tag {
	case "owner":
		return "settle-by-owner"
	case "lead-may-decide":
		return "settle-by-lead"
	default:
		return "clarify-authority"
	}
}

// archivedAnswer checks identity as well as the filename. Old similar-question
// cards are not evidence that all questions formerly merged into them are answered.
func archivedAnswer(qdir string, it enrichedQuestion) (string, error) {
	path := filepath.Join(qdir, "answered", it.Card+".md")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	record, err := questionRecord(string(b))
	if err != nil || (record != 0 && record != it.Record) {
		return "", nil
	}
	c, err := parseCard(string(b))
	if err == nil && flat(c.question) == flat(it.Q.Question) {
		return "answered/" + it.Card + ".md", nil
	}
	return "", nil
}

var gateCardNumber = regexp.MustCompile(`^q([1-9][0-9]*)(?:-.+)?\.md$`)
var gateQuestionRecord = regexp.MustCompile(`(?m)^gate-question: ([1-9][0-9]*)$`)

func questionRecord(src string) (int, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	header, _, _ := strings.Cut(strings.TrimPrefix(src, "---\n"), "\n---")
	if m := gateQuestionRecord.FindStringSubmatch(header); m != nil {
		return strconv.Atoi(m[1])
	}
	return 0, nil
}

// When gate.json records an answer, retain the original card and its identity
// before removing it from the open queue. Existing archives are never rewritten.
func preserveRecordedAnswers(qdir string, answered []enrichedQuestion) error {
	for _, it := range answered {
		if it.Q.AnsweredBy == "" || it.Card == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(qdir, it.Card+".md"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		dir := filepath.Join(qdir, "answered")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, it.Card+".md")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			answer, readErr := archivedAnswer(qdir, it)
			if readErr != nil {
				return readErr
			}
			if answer == "" {
				return fmt.Errorf("answer archive %s has a different question identity; preserve it and resolve the conflict before enriching", path)
			}
			continue
		}
		if err != nil {
			return err
		}
		_, err = f.Write(append(b, []byte("\n## Answer evidence\n"+it.Q.AnsweredBy+"\n")...))
		closeErr := f.Close()
		if err != nil {
			_ = os.Remove(path)
			return err
		}
		if closeErr != nil {
			_ = os.Remove(path)
			return closeErr
		}
	}
	return nil
}

// questionNumbers reuses existing exact identities, including archived cards.
// Older gates numbered only unresolved questions. Reusing their filenames keeps
// those runs compatible without renumbering survivors when an answer is recorded.
func questionNumbers(qdir string, qs []gateQuestion) ([]int, error) {
	numbers := make([]int, len(qs))
	used := map[int]bool{}
	for _, dir := range []string{filepath.Join(qdir, "answered"), qdir} {
		paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			base := filepath.Base(path)
			m := gateCardNumber.FindStringSubmatch(base)
			if m == nil {
				continue
			}
			n, err := strconv.Atoi(m[1])
			if err != nil || used[n] {
				continue
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			c, err := parseCard(string(b))
			if err != nil {
				continue
			}
			// Additive metadata distinguishes identical wordings after one record
			// is answered. Legacy cards fall back to exact text and filename.
			record, err := questionRecord(string(b))
			if err != nil || record > len(qs) {
				continue
			}
			for i, q := range qs {
				if record != 0 && record != i+1 {
					continue
				}
				if q.Decisive && numbers[i] == 0 && base == cardKey(n, q.Package)+".md" && flat(c.question) == flat(q.Question) {
					numbers[i], used[n] = n, true
					break
				}
			}
		}
	}
	ordinal := 0
	for i, q := range qs {
		if !q.blocks() {
			continue
		}
		ordinal++
		if numbers[i] != 0 {
			continue
		}
		n := ordinal
		for used[n] {
			n++
		}
		numbers[i], used[n] = n, true
	}
	return numbers, nil
}

// enrichGate rewrites the question cards of a gate run in dir and writes
// questions/INDEX.md. It reads the questions from gate.json, so a rerun
// starts from the same input. code is the checkout to search, src the spec
// text and rel its repo-relative path.
func enrichGate(dir, code, src, rel string, sp *spec) (*enrichResult, error) {
	b, err := os.ReadFile(filepath.Join(dir, "gate.json"))
	if err != nil {
		return nil, fmt.Errorf("no gate.json in %s: run `orch gate` first", dir)
	}
	var rec gateRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("gate.json: %v", err)
	}
	res := &enrichResult{}
	lines := specLines(src)
	may := mayDecidePhrases(sp)

	qdir := filepath.Join(dir, "questions")
	numbers, err := questionNumbers(qdir, rec.Questions)
	if err != nil {
		return nil, err
	}
	var items, minor []enrichedQuestion
	for i, q := range rec.Questions {
		it := enrichedQuestion{Q: q, Blocker: q.blocks(), N: numbers[i], Record: i + 1}
		if it.N != 0 {
			it.Card = cardKey(it.N, q.Package)
		}
		it.Tag, it.Why = routeQuestion(q.Question, may)
		it.Spec = specHints(lines, questionIdentifiers(q.Question))
		it.Answer = q.AnsweredBy
		if it.Answer == "" && it.Card != "" {
			var err error
			it.Answer, err = archivedAnswer(qdir, it)
			if err != nil {
				return nil, err
			}
		}
		if it.Answer != "" {
			res.Answered = append(res.Answered, it)
		} else if q.blocks() {
			res.Total++
			items = append(items, it)
		} else {
			minor = append(minor, it)
		}
	}
	items = dedupeQuestions(items)
	res.Cards = len(items)

	// Code search: one jbcontext call per card, a few at a time.
	cfg, _ := loadConfig(code)
	env := newCtxEnv(code, ctxOpts{Cfg: cfg, Skip: []string{rel}})
	if why := env.jbMissing(); why != "" {
		for i := range items {
			items[i].CodeReview = "Code search did not run: " + why
			items[i].Search = []evidenceLookup{lookupRecord(items[i].Q.Question, ".", "concept", 0, enrichMaxHints, nil, why)}
		}
		if env.NoJB {
			res.Notes = append(res.Notes, why)
		} else {
			res.Notes = append(res.Notes, "no code hints: "+env.JB+" was not found")
		}
	} else {
		var mu sync.Mutex
		fails := 0
		sem := make(chan struct{}, enrichParallel)
		var wg sync.WaitGroup
		for i := range items {
			wg.Add(1)
			sem <- struct{}{}
			go func(it *enrichedQuestion) {
				defer wg.Done()
				defer func() { <-sem }()
				mu.Lock()
				stop := fails >= enrichMaxFails
				mu.Unlock()
				if stop {
					it.CodeReview = "Code search skipped after earlier search failures."
					it.Search = []evidenceLookup{lookupRecord(it.Q.Question, ".", "concept", 0, enrichMaxHints, nil, "skipped after earlier failures")}
					return
				}
				hits, err := env.Concept(short(strings.Join(strings.Fields(it.Q.Question), " "), enrichQueryChars), "", 3)
				it.Search = []evidenceLookup{lookupRecord(short(flat(it.Q.Question), enrichQueryChars), ".", "concept", len(hits), enrichMaxHints, err, "")}
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					it.CodeReview = "Code search failed: " + flat(err.Error())
					if fails++; fails == 1 {
						res.Notes = append(res.Notes, "no code hints for some questions: "+err.Error())
					}
					return
				}
				it.CodeReview = fmt.Sprintf("Bounded semantic search of the checkout for %q returned %d candidate(s), excluding the spec. This is not an exhaustive answer search.", short(flat(it.Q.Question), enrichQueryChars), len(hits))
				for _, h := range hits {
					if len(it.Code) < enrichMaxHints {
						it.Code = append(it.Code, h)
					}
				}
			}(&items[i])
		}
		wg.Wait()
		if fails >= enrichMaxFails {
			res.Notes = append(res.Notes, fmt.Sprintf("code search stopped after %d failures", fails))
		}
		res.Notes = append(res.Notes, env.Notes()...)
	}
	var note string
	res.SymbolFacts, note = addSymbolFacts(items, env.CacheDir, code)
	if note != "" {
		res.Notes = append(res.Notes, note)
	}
	nTS, note := addTSFacts(items, env.CacheDir, code, sp)
	res.SymbolFacts += nTS
	if note != "" {
		res.Notes = append(res.Notes, note)
	}
	for _, it := range items {
		switch it.Tag {
		case "owner":
			res.Owner++
		case "lead-may-decide":
			res.Lead++
		default:
			res.Unclear++
		}
		if len(it.Spec) > 0 {
			res.SpecHints++
		}
		if len(it.Code) > 0 {
			res.CodeHits++
		}
	}
	for i := range items {
		items[i].Readiness = checkCardReady(enrichedCard(items[i], rel))
	}
	res.Items, res.Minor = items, minor

	if err := os.MkdirAll(qdir, 0o755); err != nil {
		return nil, err
	}
	if err := preserveRecordedAnswers(qdir, res.Answered); err != nil {
		return nil, err
	}
	// Only replace generated root cards and their index. Keep review notes,
	// archived answers and attachments when enrichment is rerun.
	paths, err := filepath.Glob(filepath.Join(qdir, "*.md"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		name := filepath.Base(path)
		if name != "INDEX.md" && !gateCardNumber.MatchString(name) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	for _, it := range items {
		if err := os.WriteFile(filepath.Join(qdir, it.Card+".md"), []byte(enrichedCard(it, rel)), 0o644); err != nil {
			return nil, err
		}
	}
	// One selected packet, two serializers. No inference or model request.
	packets := make([]gateQuestionDelivery, 0, len(items))
	for _, it := range items {
		p := gateQuestionPacket{Unknown: it.Q.Question}
		if it.Q.Packet != nil {
			p = *it.Q.Packet
			if p.Unknown == "" {
				p.Unknown = it.Q.Question
			}
		}
		packets = append(packets, gateQuestionDelivery{it.Card, it.Q.Package, p, it.Readiness.Status, it.Readiness.Missing, it.Search})
	}
	for _, out := range []struct {
		name     string
		audience outputAudience
	}{{"packets.toon", outputAgent}, {"packets.json", outputHuman}} {
		var buf bytes.Buffer
		if err := writeOutput(&buf, out.audience, packets); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(qdir, out.name), buf.Bytes(), 0o644); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(qdir, "INDEX.md"), []byte(enrichedIndex(res, rel)), 0o644); err != nil {
		return nil, err
	}
	if err := addReportSection(filepath.Join(dir, "report.md"), res); err != nil {
		return nil, err
	}
	queueQuestionClassifier(dir, code, rel, sp, rec, res, cfg)
	return res, nil
}

func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// gateQuestionCard is also used before enrichment, so the initial card cannot
// claim a repo-wide absence that neither review nor enrichment established.
func gateQuestionCard(q gateQuestion) string {
	tag, _ := routeQuestion(q.Question, nil)
	return enrichedCard(enrichedQuestion{Q: q, Tag: tag}, "")
}

func questionCardText(q gateQuestion, tag string, record int) string {
	identity := ""
	if record != 0 {
		identity = fmt.Sprintf("gate-question: %d\n", record)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nquestion: %s\nroute: %s\nauthority: %s\naction: %s\n%s", flat(q.Question), tag, tag, questionAction(tag), identity)
	if p := q.Packet; p != nil && p.Recommendation != "" {
		fmt.Fprintf(&b, "recommend: %s\n", flat(p.Recommendation))
	}
	b.WriteString("---\n## Context\n")
	fmt.Fprintf(&b, "Raised by the spec gate for package `%s`. The review reported this question as unresolved. Hints do not establish an answer or prove that one is absent.\n", q.Package)
	if p := q.Packet; p != nil {
		if p.Goal != "" {
			fmt.Fprintf(&b, "\nGoal: %s\n", p.Goal)
		}
		for _, fact := range p.KnownFacts {
			fmt.Fprintf(&b, "- Known fact (reviewer): %s\n", fact)
		}
		for _, id := range p.EvidenceIDs {
			fmt.Fprintf(&b, "- Evidence: %s\n", id)
		}
		if p.Unknown != "" {
			fmt.Fprintf(&b, "\nExact unknown: %s\n", p.Unknown)
		}
		if p.Authority != "" {
			fmt.Fprintf(&b, "\nReviewer authority basis (not a delegation): %s\n", p.Authority)
		}
		if len(p.Options) > 0 {
			b.WriteString("\n## Options\n")
		}
		for _, o := range p.Options {
			fmt.Fprintf(&b, "\n### %s: %s\n", o.ID, o.Text)
			if o.Gain != "" {
				fmt.Fprintf(&b, "Gain: %s\n", o.Gain)
			}
			if o.Cost != "" {
				fmt.Fprintf(&b, "Cost: %s\n", o.Cost)
			}
			if o.ID == p.Recommendation && p.Rationale != "" {
				fmt.Fprintf(&b, "\nRecommendation rationale: %s\n", p.Rationale)
			}
		}
		if p.NextAction != "" {
			fmt.Fprintf(&b, "\n## Next\n%s\n", p.NextAction)
		}
	}
	return b.String()
}

func enrichedCard(it enrichedQuestion, rel string) string {
	var b strings.Builder
	b.WriteString(questionCardText(it.Q, it.Tag, it.Record))
	fmt.Fprintf(&b, "\n## Tag\n%s (%s). Action: %s. Routing advice only; it does not settle the question.\n", it.Tag, it.Why, questionAction(it.Tag))
	b.WriteString("\n## Original questions\n")
	members := it.Members
	if len(members) == 0 {
		members = []enrichedQuestion{it}
	}
	for _, member := range members {
		fmt.Fprintf(&b, "- %s [%s; %s]: %s\n", cardKey(member.N, member.Q.Package), member.Tag, questionAction(member.Tag), member.Q.Question)
	}
	b.WriteString("\n## Review evidence\n")
	if len(questionIdentifiers(it.Q.Question)) < 2 {
		fmt.Fprintf(&b, "Spec hint search in `%s` skipped: fewer than two identifiers.\n", rel)
	} else {
		fmt.Fprintf(&b, "Scanned `%s` for co-occurring question identifiers in three-line windows; attached %d candidate spec hint(s). This heuristic does not establish absence.\n", rel, len(it.Spec))
	}
	if it.CodeReview != "" {
		fmt.Fprintf(&b, "\n%s\n", it.CodeReview)
	}
	if len(it.Spec) > 0 {
		b.WriteString("\n## The spec may already answer this\nA hint only; read the lines before relying on them.\n")
		for _, h := range it.Spec {
			fmt.Fprintf(&b, "- `%s:%d`: %s\n", rel, h.Line, h.Text)
		}
	}
	if len(it.Facts) > 0 {
		b.WriteString("\n## Exact symbol facts\n")
		for _, l := range factLines(it.Facts) {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	if len(it.Code) > 0 {
		b.WriteString("\n## Possibly relevant code\n")
		for _, h := range it.Code {
			fmt.Fprintf(&b, "- `%s:%d-%d`\n\n```\n%s\n```\n", h.Path, h.Start, h.End, strings.Join(h.Snippet, "\n"))
		}
	}
	src := b.String()
	ready := checkCardReady(src)
	// A review packet also needs the goal, rationale and next action. D2's
	// structural checker is necessary but cannot infer these missing facts.
	if ready.Status == "ready" {
		p := it.Q.Packet
		if p == nil || !cardContent(p.Goal) || len(p.KnownFacts)+len(p.EvidenceIDs) == 0 || !cardContent(p.Rationale) || !cardContent(p.Authority) || !cardContent(p.NextAction) {
			ready = cardReadiness{"missing_evidence", []string{"original review packet needs goal, known facts/evidence, recommendation rationale, authority and next action"}}
		}
	}
	return strings.Replace(src, "---\n", "---\nready: "+ready.Status+"\n", 1)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

func cell(s string) string { return strings.ReplaceAll(s, "|", "/") }

func enrichedIndex(res *enrichResult, rel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Gate questions\n\nSpec: `%s`\n\n", rel)
	fmt.Fprintf(&b, "%s\n\n", res.summary())
	for _, n := range res.Notes {
		fmt.Fprintf(&b, "Note: %s\n\n", n)
	}
	var pkgs []string
	byPkg := map[string][]enrichedQuestion{}
	for _, it := range res.Items {
		if _, ok := byPkg[it.Q.Package]; !ok {
			pkgs = append(pkgs, it.Q.Package)
		}
		byPkg[it.Q.Package] = append(byPkg[it.Q.Package], it)
	}
	for _, p := range pkgs {
		set := byPkg[p]
		sort.SliceStable(set, func(i, j int) bool { return routeRank(set[i].Tag) < routeRank(set[j].Tag) })
		fmt.Fprintf(&b, "## %s\n\n| card | tag | question | spec hint | code hits | go facts | action | ready |\n|---|---|---|---|---|---|---|---|\n", p)
		for _, it := range set {
			q := short(flat(it.Q.Question), 100)
			if len(it.Merged) > 0 {
				q += fmt.Sprintf(" (+%d exact duplicates)", len(it.Merged))
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", it.Card, it.Tag, cell(q), yesNo(len(it.Spec) > 0), yesNo(len(it.Code) > 0), yesNo(len(it.Facts) > 0), questionAction(it.Tag), it.Readiness.Status)
		}
		b.WriteString("\n")
		for _, it := range set {
			if len(it.Members) > 1 {
				for _, member := range it.Members {
					fmt.Fprintf(&b, "Member %s [%s; %s], shared card %s: %s\n\n", cardKey(member.N, member.Q.Package), member.Tag, questionAction(member.Tag), it.Card, member.Q.Question)
				}
			}
		}
	}
	if len(res.Answered) > 0 {
		b.WriteString("## Answer history (not reopened by enrichment)\n\n")
		for _, it := range res.Answered {
			id := it.Card
			if id == "" {
				id = it.Q.Package + " (no prior card)"
			}
			fmt.Fprintf(&b, "- %s [%s]: %s (answer: `%s`)\n", id, it.Tag, it.Q.Question, it.Answer)
		}
		b.WriteString("\n")
	}
	if len(res.Minor) > 0 {
		fmt.Fprintf(&b, "## Minor open questions (not blocking, no cards)\n\n")
		for _, it := range res.Minor {
			fmt.Fprintf(&b, "- %s [%s]: %s\n", it.Q.Package, it.Tag, short(flat(it.Q.Question), 140))
		}
	}
	return b.String()
}

// addReportSection puts the question summary in report.md, replacing an
// earlier one so a rerun does not stack them.
func addReportSection(path string, res *enrichResult) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(b)
	if i := strings.Index(text, "## Question cards\n"); i >= 0 {
		text = strings.TrimRight(text[:i], "\n") + "\n\n"
	}
	var section strings.Builder
	section.WriteString(text)
	fmt.Fprintf(&section, "## Question cards\n%s\n", res.summary())
	for _, n := range res.Notes {
		fmt.Fprintf(&section, "Note: %s\n", n)
	}
	return os.WriteFile(path, []byte(section.String()), 0o644)
}
