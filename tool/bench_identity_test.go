package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBenchReportRejectsCorruptKeysButAllowsNoKey(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	benchRecordFixture(t, h, "run", 10, false)
	for _, args := range [][]string{{"bench", "report", "fixture"}, {"bench", "report", "fixture", "--json"}} {
		h.must(args...)
	}
	keys := filepath.Join(benchDir(), "fixture", "keys")
	if err := os.MkdirAll(keys, 0700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, keys, map[string]string{"key-1.json": "corrupt"})
	for _, args := range [][]string{{"bench", "report", "fixture"}, {"bench", "report", "fixture", "--json"}} {
		code, out := h.run(args...)
		if code == 0 || !strings.Contains(out, "key-1.json") || !strings.Contains(out, "invalid character") {
			t.Errorf("%v hid corrupt key: exit %d\n%s", args, code, out)
		}
	}
}

// Reproduce through the snapshot scorer used by bench compare, without a model
// or a live store. Two independent behaviors deliberately share a target.
func TestBenchCompareKeeps29BehaviorsWithSharedRanges(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	dir := benchFixture(t, h, "identity", 1, false)
	rec := readRecord(t, dir)
	rec.Findings = nil
	subject := evidenceTestSubject("bench")
	rec.Subject = subject
	rec.Review = &gateReviewRecords{Policy: reviewPolicy{Version: reviewReducerPolicyVersion, CurrentSubject: subject, Packet: evidencePacket{evidenceEnvelope: evidenceEnvelope{evidenceSchemaVersion, subject, evidenceComplete}}, CurrentHashes: map[string]string{}}}
	var items []gateItem
	key := &benchKey{ID: "key-1", Itemset: "same"}
	for i := 1; i <= 29; i++ {
		id := fmt.Sprintf("api#%d", i)
		loc := fmt.Sprintf("src/service.py:%d-%d", i*10, i*10+5)
		if i == 2 {
			loc = "src/service.py:10-15"
		}
		class, judgment := "duplicate", reviewJudgmentReuse
		if i > 2 {
			class = "none"
			judgment = reviewJudgmentNew
			loc = ""
		}
		if i > 16 {
			class = "unknown"
			judgment = reviewJudgmentUnclear
		}
		items = append(items, gateItem{ID: id, Package: "api", Text: fmt.Sprintf("Behavior %d", i), Revision: fmt.Sprint(i)})
		key.Items = append(key.Items, benchKeyItem{ID: id, Package: "api", Item: fmt.Sprintf("Behavior %d", i), Class: class, Candidate: loc})
		revision := fmt.Sprint(i)
		rec.Review.Checks = append(rec.Review.Checks, reviewCheck{Item: reviewItem{SchemaVersion: 1, ID: id, Revision: revision, Package: "api", Requirement: fmt.Sprintf("Behavior %d", i), Checks: []string{"reuse"}}, ID: "reuse", Required: true, Actor: "fixture", Assessors: []string{"fixture"}})
		recordGateAnswer(&rec, id, "reuse", "fixture", "explicit supported judgment", judgment, "implements this behavior", "matching behavior", "")
		if judgment == reviewJudgmentNew {
			// C3 requires real search coverage bound to the behavior and the
			// check's repository-wide scope, not a relabeled reviewer reply.
			lookupID := "search-" + id
			rec.Review.Policy.Packet.Lookups = append(rec.Review.Policy.Packet.Lookups, evidenceLookup{
				SchemaVersion: evidenceSchemaVersion, ID: lookupID, Query: "Code that implements " + items[len(items)-1].Text,
				Scope: ".", Unit: "matches", ExecutionStatus: lookupSucceeded, Status: "empty", CountKind: evidenceCountExact,
				Counts: lookupCounts{Requested: evidenceTestPtr(1), Succeeded: evidenceTestPtr(1), Failed: evidenceTestPtr(0), Total: evidenceTestPtr(0)},
			})
			answer := &rec.Review.Answers[len(rec.Review.Answers)-1]
			answer.LookupIDs = []string{lookupID}
			answer.EvidenceIDs, answer.Support.EvidenceIDs = nil, nil
			answer.Rationale = "The complete repository-wide search found no implementation of this behavior"
			answer.Support.Rationale = answer.Rationale
		}
		answerID := rec.Review.Answers[len(rec.Review.Answers)-1].ID
		rec.Items = append(rec.Items, gateItemResult{ID: id, Package: "api", Revision: revision, AnswerID: answerID, Verdict: string(judgment), Candidate: loc, Verified: true})
	}
	b, _ := json.Marshal(rec)
	its, _ := json.Marshal(items)
	writeFiles(t, dir, map[string]string{"gate.json": string(b), "items.json": string(its)})
	run := &benchRun{dir: filepath.Dir(dir), Itemset: "same", Groups: map[string]benchGroup{"group:api": {Packages: []string{"api"}}}}
	// The fixture directory is the stored snapshot, not a newly recorded run.
	run.dir = filepath.Join(benchDir(), "fixture", "runs", "identity")
	copyDirForBenchIdentity(t, dir, filepath.Join(run.dir, "snapshot"))
	score, err := benchRescore(run, key)
	if err != nil {
		t.Fatal(err)
	}
	if score.Classes["none"].Match != 14 || score.Classes["unknown"].Match != 13 {
		t.Fatalf("new/unclear judgments were not scored explicitly: %+v", score.Classes)
	}
	got := score.Classes["duplicate"]
	if got.Total != 2 || got.Match != 2 || score.KeyAgree["group"].Items != 29 || score.KeyAgree["group"].Agree != 29 {
		t.Fatalf("lost behavior identity: %+v; %s; %+v", got, score.ItemNote, score.KeyAgree["group"])
	}
	rec.Review.Answers[0].Judgment = reviewJudgmentExtend
	rec.Review.Answers[0].MissingBehavior = "needs validation"
	rec.Items[0].Verdict = verdictExtend
	key.Items[0].Class = "variation_site"
	b, _ = json.Marshal(rec)
	writeFiles(t, filepath.Join(run.dir, "snapshot"), map[string]string{"gate.json": string(b)})
	partial, err := benchRescore(run, key)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Classes["duplicate"].Total != 1 || partial.Classes["duplicate"].Match != 1 || partial.Classes["variation_site"].Match != 1 {
		t.Fatalf("explicit extension was collapsed into duplicate: %+v", partial)
	}
	rec.Review.Answers[28].AnswerState = reviewAnswerUnavailable
	b, _ = json.Marshal(rec)
	writeFiles(t, filepath.Join(run.dir, "snapshot"), map[string]string{"gate.json": string(b)})
	unavailable, err := benchRescore(run, key)
	if err != nil {
		t.Fatal(err)
	}
	if c := unavailable.KeyAgree["group"]; c.Answered != 28 || c.Missing != 1 || c.Items != 29 {
		t.Fatalf("unavailable answer inflated coverage: %+v", c)
	}
	rec.Review.Answers[28].AnswerState = reviewAnswerAnswered
	rec.Review.Policy.Stale = true
	b, _ = json.Marshal(rec)
	writeFiles(t, filepath.Join(run.dir, "snapshot"), map[string]string{"gate.json": string(b)})
	stale, err := benchRescore(run, key)
	if err != nil {
		t.Fatal(err)
	}
	if coverage := stale.KeyAgree["group"]; coverage.Valid != 0 || coverage.Supported != 0 || coverage.Invalid != 29 {
		t.Fatalf("stale review was scored as current supported answers: %+v", coverage)
	}
}

func copyDirForBenchIdentity(t *testing.T, src, dst string) {
	t.Helper()
	// Keep this fixture small and exact: only scorer inputs are needed.
	if err := os.MkdirAll(dst, 0700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dst, map[string]string{"gate.json": string(mustRead(t, filepath.Join(src, "gate.json"))), "items.json": string(mustRead(t, filepath.Join(src, "items.json"))), "checklist.json": "{}"})
}

func TestBenchCompareUnverifiedIsNotPositive(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"items.json": `[{"id":"p#one","package":"p","revision":"r1"}]`})
	a := loadArm(dir, "", dir, &gateRecord{Items: []gateItemResult{{ID: "p#one", Package: "p", Revision: "r1", Verdict: verdictReuse, Candidate: "src/service.ts:1-9"}}})
	ref := map[string]refEntry{"p#one": {Package: "p", Verdict: verdictReuse, Candidate: "src/service.ts:1-9"}}
	if s := scoreLocations(a, ref, "group", verdictReuse); s.Match != 0 {
		t.Fatalf("unverified candidate counted correct: %+v", s)
	}
	coverage := keyViewOf(a, ref).Agree["group"]
	if coverage.Items != 1 || coverage.Answered != 1 || coverage.Invalid != 1 || coverage.Supported != 0 {
		t.Fatalf("unverified answer lost from coverage: %+v", coverage)
	}
}

func TestBenchCompareMissingRangeCountersStayUnknown(t *testing.T) {
	var p sessionProfile
	if err := json.Unmarshal([]byte(`{"requests":1,"calls":{"read":7}}`), &p); err != nil {
		t.Fatal(err)
	}
	r := &benchRun{Groups: map[string]benchGroup{"group:p": {Exploration: &p}}}
	var total, ranged *float64
	for _, row := range benchToolRows {
		if row.name == "Native reads" {
			total = benchToolSum(r, nil, row.count)
		}
		if row.name == "Native reads with a range" {
			ranged = benchToolSum(r, nil, row.count)
		}
	}
	if total == nil || *total != 7 || ranged != nil {
		t.Fatalf("missing instrumentation became zero: total=%v range=%v", total, ranged)
	}
	if strings.Contains(p.String(), "0 ranged") {
		t.Fatal(p.String())
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ranged_reads") || strings.Contains(string(b), "whole_reads") {
		t.Fatalf("JSON fabricated historical range counts: %s", b)
	}
	if err := json.Unmarshal([]byte(`{"calls":{"read":null}}`), &p); err != nil {
		t.Fatal(err)
	}
	if _, known := p.totalReads(); known {
		t.Fatal("null total-read counter became zero")
	}
	// An actual zero is distinct from missing and stays zero after a round trip.
	if err := json.Unmarshal([]byte(`{"calls":{"read":0},"ranged_reads":0,"whole_reads":0}`), &p); err != nil {
		t.Fatal(err)
	}
	if n, known := p.totalReads(); !known || n != 0 || !p.rangeReadsKnown() {
		t.Fatalf("lost measured zero: %+v", p)
	}
}

func TestBenchCompareMappingRequiresExplicitUnambiguousIdentity(t *testing.T) {
	refs := []refEntry{{ID: "p#save", Revision: "old", Package: "p", Item: "Save a record", Verdict: verdictExtend, Candidates: []string{"src/save.py:10-20", "src/save.ts:30-40"}}}
	items := []gateItem{{ID: "p#store", Revision: "new", Package: "p", Text: "Save a record"}}
	a := &armData{items: items, answers: map[string]map[string]scoredAnswer{"group": {"p#store": {Revision: "new", Verdict: verdictExtend, Candidate: "src/save.ts:31-38", Present: true, Valid: true, Supported: true}}}}
	missing := resolveMappedReference(refs, items, "from", "to", nil)
	if issues := referenceIssues(missing); len(issues) != 1 {
		t.Fatalf("exact/fuzzy text mapped without authority: %v", issues)
	}
	if s := scoreLocations(a, missing, "group", verdictExtend); s.Precision != nil || s.Recall != nil || s.Unavailable == "" || s.Total != 1 {
		t.Fatalf("unmapped quality claimed: %+v", s)
	}
	mapping := &itemMapping{Key: "key-1", FromItemset: "from", ToItemset: "to", Items: []itemMappingEntry{{KeyID: "p#save", KeyRevision: "old", ItemID: "p#store", ItemRevision: "new"}}}
	resolved := resolveMappedReference(refs, items, "from", "to", mapping)
	if s := scoreLocations(a, resolved, "group", verdictExtend); s.Match != 1 || s.Total != 1 || s.Found != 1 {
		t.Fatalf("mapped alternative evidence did not match: %+v", s)
	}
	if s := scoreLocations(a, resolved, "group", verdictReuse); s.Match != 0 || s.Found != 0 || s.Total != 0 {
		t.Fatalf("partial reuse forced to duplicate: %+v", s)
	}
	mapping.Items = append(mapping.Items, mapping.Items[0])
	if s := scoreLocations(a, resolveMappedReference(refs, items, "from", "to", mapping), "group", verdictExtend); s.Recall != nil || s.Unavailable == "" {
		t.Fatalf("ambiguous mapping scored: %+v", s)
	}
	mapping.Items = mapping.Items[:1]
	mapping.Items[0].ItemRevision = "stale"
	if len(referenceIssues(resolveMappedReference(refs, items, "from", "to", mapping))) != 1 {
		t.Fatal("stale revision accepted")
	}
	// Equal stable identities and revisions need no map, even after reordering.
	refs[0].ID = "p#store"
	refs[0].Revision = "new"
	if len(referenceIssues(resolveMappedReference(refs, items, "from", "to", nil))) != 0 {
		t.Fatal("exact stable identity not preferred")
	}
	// Explicit target relocation still applies when the behavior identity stayed
	// stable. The map cannot override that identity with a different behavior.
	refs[0].Candidates = []string{"src/old.py:1-2"}
	mapping.Items = []itemMappingEntry{{KeyID: "p#store", KeyRevision: "new", ItemID: "p#store", ItemRevision: "new", Candidates: []string{"src/save.ts:30-40"}}}
	if s := scoreLocations(a, resolveMappedReference(refs, items, "from", "to", mapping), "group", verdictExtend); s.Match != 1 {
		t.Fatalf("stable identity lost mapped evidence relocation: %+v", s)
	}
	refs[0].ID = "p#1"
	items[0].ID = "p#1"
	if len(referenceIssues(resolveMappedReference(refs, items, "from", "to", nil))) != 1 {
		t.Fatal("spec-local ordinals crossed versions without a map")
	}
}

func TestBenchCompareMappingFileValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json")
	valid := `{"schema_version":1,"mappings":[{"key":"key-1","from_itemset":"from","to_itemset":"to","items":[{"key_id":"p#save","key_revision":"r1","item_id":"p#store","item_revision":"r2"}]}]}`
	writeFiles(t, dir, map[string]string{"mapping.json": valid})
	m, err := readItemMapping(path, "key-1", "from", "to")
	if err != nil || m == nil {
		t.Fatalf("valid map: %+v %v", m, err)
	}
	if m, err := readItemMapping(path, "key-1", "from", "other"); err != nil || m != nil {
		t.Fatalf("another population's map applied: %+v %v", m, err)
	}
	for _, bad := range []string{`{"schema_version":2,"mappings":[]}`, strings.Replace(valid, `"items":`, `"typo":`, 1), valid + ` {}`} {
		writeFiles(t, dir, map[string]string{"mapping.json": bad})
		if _, err := readItemMapping(path, "key-1", "from", "to"); err == nil {
			t.Fatalf("invalid map accepted: %s", bad)
		}
	}
}

func TestBenchCompareGenericReuseFindingIsLegacyOnly(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"items.json": `[{"id":"p#partial","revision":"r1","package":"p","text":"Save partly"}]`})
	a := loadArm(dir, "", dir, &gateRecord{Findings: []gateFinding{{Source: "reuse", Claim: "Save partly", Where: "package p", Evidence: "src/save.py:10-20", Verified: true}}})
	ref := map[string]refEntry{"p#partial": {ID: "p#partial", Verdict: verdictExtend, Candidate: "src/save.py:10-20"}}
	if a.verdict("group", "p#partial") != "" {
		t.Fatal("generic finding became explicit duplicate judgment")
	}
	if s := scoreLocations(a, ref, "group", verdictReuse); s.Found != 0 || s.Match != 0 {
		t.Fatalf("heuristic counted as supported duplicate: %+v", s)
	}
	if s := scoreLegacyLocations(a, map[string]refEntry{"p#partial": {Verdict: verdictReuse, Candidate: "src/save.py:10-20"}}, "group", verdictReuse); s.Match != 1 {
		t.Fatal("legacy evidence became inaccessible")
	}
}

func TestBenchCompareReadOnlyStoreByteIdentical(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	a := benchRecordFixture(t, h, "first", 10, false)
	b := benchRecordFixture(t, h, "second", 20, false)
	benchKeyFixture(t, h, a)
	// Copy to an isolated store, then compare every file hash before/after.
	src := benchDir()
	copy := filepath.Join(t.TempDir(), "copy")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		dest := filepath.Join(copy, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		bytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, bytes, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_BENCH_DIR", copy)
	hashes := func() map[string]string {
		out := map[string]string{}
		err := filepath.WalkDir(copy, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				bytes, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(copy, path)
				out[rel] = benchHash(bytes)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := hashes()
	out := h.must("bench", "compare", "fixture", a.ID, b.ID, "--json")
	if !strings.Contains(out, "legacy") || !strings.Contains(out, "coverage") {
		t.Fatal(out)
	}
	h.must("bench", "report", "fixture", "--json")
	if after := hashes(); !reflect.DeepEqual(before, after) {
		t.Fatalf("read-only report changed source store: before=%v after=%v", before, after)
	}
}

func TestBenchCompareIncompatibleSubjectsNeverRankWinner(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	a := benchRecordFixture(t, h, "one", 1, false)
	b := benchRecordFixture(t, h, "eleven", 11, false)
	subject, err := benchSubjectLoad(b.Subject)
	if err != nil {
		t.Fatal(err)
	}
	other := *subject
	other.Commit = "1234567abcdef"
	other.CommitBasis = "user"
	other.CommitEvidence = "fixture: another evaluation subject"
	other.ID = benchSubjectID(other)
	if err := benchSaveSubject(other); err != nil {
		t.Fatal(err)
	}
	b.Subject = other.ID
	if why := benchRankingCompatibility(a, b); why != "different subjects" {
		t.Fatalf("different subjects can rank: %s", why)
	}
	b.Subject = a.Subject
	b.Groups = map[string]benchGroup{}
	for i := range 10 {
		b.Groups[fmt.Sprintf("group:%d", i)] = benchGroup{Packages: []string{"api"}, Cost: benchCost{USD: evidenceTestPtr(float64(i + 1))}}
	}
	scoped := a.Groups["group:api"]
	scoped.Seconds = evidenceTestPtr(100)
	scoped.Cost.USD = evidenceTestPtr(1.0)
	b.Groups["group:api"] = scoped
	if why := benchRankingCompatibility(a, b); why != "different pass coverage" {
		t.Fatalf("different pass coverage can rank: %s", why)
	}
	key := benchKeyFixture(t, h, a)
	history, err := benchHistoryBuild("fixture", benchSuite{crossSubject: true}, []*benchRun{a, b}, a, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range history.Quality {
		if q.Pareto || q.Eligible || q.Exclusion == "" {
			t.Fatalf("unlabeled winner: %+v", q)
		}
	}
	for _, change := range history.Changes {
		if strings.Contains(change, "faster") || strings.Contains(change, "less") || strings.Contains(change, "slower") || strings.Contains(change, "more") {
			t.Fatal("incompatible headline winner: " + change)
		}
	}
	var out strings.Builder
	benchPrintHistory(&out, history)
	if !strings.Contains(out.String(), "not ranked:") {
		t.Fatal(out.String())
	}
}

func TestCompareAuthorHintDoesNotRenderBody(t *testing.T) {
	a := &armData{items: []gateItem{{ID: "p#one", Reuse: []string{"src/service.ts#handle"}}}}
	seen, hint := a.keySeen("p#one", refEntry{Candidate: "src/service.ts:10-20"})
	if seen || !hint {
		t.Fatalf("author hint claimed a rendered body: seen=%t hint=%t", seen, hint)
	}
}
