package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func evidenceTestPtr[T any](v T) *T { return &v }

func evidenceTestSubject(label string) *gateSubject {
	s := &gateSubject{
		Version:    gateSubjectVersion,
		Repo:       repositoryIdentity{Name: "generic-cli", Path: "/checkout/" + label, CommonDir: "/repository/.git"},
		SpecSHA256: subjectDigest([]byte("spec " + label)), Commit: "commit-" + label, Tree: "tree-" + label,
		Fingerprint: subjectDigest([]byte("files " + label)),
	}
	s.ID = s.digest()
	return s
}

func evidenceTestPacket(language string) evidencePacket {
	path, encoding, start, end := "src/export.py", "codepoint", 12, 21
	if language == "typescript" {
		path, encoding, start, end = "src/export.ts", "utf16", 10, 20
	}
	return evidencePacket{
		evidenceEnvelope: evidenceEnvelope{SchemaVersion: evidenceSchemaVersion, Subject: evidenceTestSubject("current"), Completeness: evidenceComplete},
		Lookups: []evidenceLookup{{
			SchemaVersion: evidenceSchemaVersion, ID: "lookup-01", Query: "export", Scope: "src/", Unit: "matches", Exclusions: []string{"tests", "generated"},
			Counts:          lookupCounts{Requested: evidenceTestPtr(1), Succeeded: evidenceTestPtr(1), Failed: evidenceTestPtr(0), Total: evidenceTestPtr(1)},
			ExecutionStatus: lookupSucceeded, Status: "ok", CountKind: evidenceCountExact, Errors: []string{}, Omissions: []string{},
		}},
		Evidence: []evidenceRecord{{
			SchemaVersion: evidenceSchemaVersion, ID: "90071992547409931234567890", SourceKind: evidenceSourceCode, Language: language, Path: path,
			Range:       &evidenceRange{StartLine: 2, EndLine: 2, StartColumn: evidenceTestPtr(start), EndColumn: evidenceTestPtr(end), ColumnEncoding: encoding},
			ContentHash: subjectDigest([]byte("'héllo 😀'")), LookupID: "lookup-01", Selected: evidenceTestPtr(true), Rendered: evidenceTestPtr(true), Read: nil, LocatorValid: evidenceTestPtr(true),
		}},
		Revalidations: []evidenceRevalidation{},
	}
}

func evidenceTestHashes(p evidencePacket) map[string]string {
	out := map[string]string{}
	for _, e := range p.Evidence {
		out[e.ID] = e.ContentHash
	}
	return out
}

func evidenceTestRoundTrip[T any](t *testing.T, original T) T {
	t.Helper()
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded T
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("round trip changed state\noriginal: %#v\ndecoded: %#v\njson: %s", original, decoded, b)
	}
	return decoded
}

func TestEvidenceSearchSupportRequiresZeroResults(t *testing.T) {
	for _, total := range []int{0, 7} {
		p := evidenceTestPacket("python")
		p.Evidence = nil
		p.Lookups[0].Counts.Total = evidenceTestPtr(total)
		if total == 0 {
			p.Lookups[0].Status = "empty"
		}
		support := claimSupport{SchemaVersion: evidenceSchemaVersion, State: claimSupported, Assessor: "reviewer", Method: "model", Rationale: "A completed search found no implementation"}
		err := support.ValidateSearch(p, []string{p.Lookups[0].ID}, p.Subject)
		if (err == nil) != (total == 0) {
			t.Fatalf("search-only support with total=%d: %v", total, err)
		}
	}
}

func TestEvidenceSourceFixturesAndStrings(t *testing.T) {
	for _, language := range []string{"python", "typescript"} {
		t.Run(language, func(t *testing.T) {
			p := evidenceTestPacket(language)
			p = evidenceTestRoundTrip(t, p)
			// The same Unicode span uses codepoint columns in this Python
			// fixture and UTF-16 units in TypeScript. Hash the original UTF-8
			// excerpt bytes, never the language server's position encoding.
			line := "    return 'héllo 😀'"
			r := p.Evidence[0].Range
			var excerpt string
			if language == "typescript" {
				line = "  return 'héllo 😀';"
				units := utf16.Encode([]rune(line))
				excerpt = string(utf16.Decode(units[*r.StartColumn-1 : *r.EndColumn-1]))
			} else {
				excerpt = string([]rune(line)[*r.StartColumn-1 : *r.EndColumn-1])
			}
			if excerpt != "'héllo 😀'" || p.Evidence[0].ContentHash != subjectDigest([]byte(excerpt)) {
				t.Fatalf("range/hash no longer identify exact source bytes: %q", excerpt)
			}
			if err := p.ValidateAttachment([]string{p.Evidence[0].ID}, p.Subject, evidenceTestHashes(p)); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(p)
			var object map[string]json.RawMessage
			if err := json.Unmarshal(b, &object); err != nil {
				t.Fatal(err)
			}
			if _, ok := object["schema_version"]; !ok {
				t.Fatal("missing shared version")
			}
			var rows []map[string]json.RawMessage
			if err := json.Unmarshal(object["evidence"], &rows); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"id", "content_hash", "lookup_id"} {
				if rows[0][field][0] != '"' {
					t.Fatalf("%s must remain a JSON string: %s", field, rows[0][field])
				}
			}
			if _, ok := rows[0]["subject_id"]; ok {
				t.Fatal("inherited subject must not be repeated")
			}
			if _, ok := rows[0]["completeness"]; ok {
				t.Fatal("inherited completeness must not be repeated")
			}
			var subject map[string]json.RawMessage
			if err := json.Unmarshal(object["subject"], &subject); err != nil {
				t.Fatal(err)
			}
			if string(subject["version"]) != "1" || subject["schema_version"] != nil {
				t.Fatalf("existing A2 subject shape changed: %s", object["subject"])
			}
		})
	}
	for _, data := range []string{
		`{"schema_version":1,"id":9007199254740993}`,
		`{"schema_version":1,"content_hash":9007199254740993}`,
		`{"schema_version":1,"lookup_id":9007199254740993}`,
	} {
		var e evidenceRecord
		if err := json.Unmarshal([]byte(data), &e); err == nil {
			t.Fatalf("numeric identity/hash accepted: %s", data)
		}
	}
	p := evidenceTestPacket("python")
	p.Evidence[0].ContentHash = "00090071992547409931234567890"
	evidenceTestRoundTrip(t, p)
}

func TestEvidenceLookupStatesRoundTrip(t *testing.T) {
	env := evidenceTestPacket("python").evidenceEnvelope
	cases := []evidenceLookup{
		{SchemaVersion: 1, ID: "not-requested", Scope: "src/", Unit: "matches", ExecutionStatus: lookupNotRequested, Completeness: evidenceUnknown, CountKind: evidenceCountUnknown, Counts: lookupCounts{Requested: evidenceTestPtr(0)}},
		{SchemaVersion: 1, ID: "empty", Query: "absent", Scope: "src/", Unit: "matches", ExecutionStatus: lookupSucceeded, Status: "empty", CountKind: evidenceCountExact, Counts: lookupCounts{Requested: evidenceTestPtr(1), Succeeded: evidenceTestPtr(1), Failed: evidenceTestPtr(0), Total: evidenceTestPtr(0)}},
		{SchemaVersion: 1, ID: "failed", Query: "absent", Scope: "src/", Unit: "matches", ExecutionStatus: lookupFailed, Status: "failed", Completeness: evidenceUnknown, CountKind: evidenceCountUnknown, Errors: []string{"backend failed"}, Counts: lookupCounts{Requested: evidenceTestPtr(1), Succeeded: evidenceTestPtr(0), Failed: evidenceTestPtr(1)}},
		{SchemaVersion: 1, ID: "unavailable", Query: "absent", Scope: "src/", Unit: "matches", ExecutionStatus: lookupUnavailable, Completeness: evidenceUnknown, CountKind: evidenceCountUnknown, Errors: []string{"no server"}},
		{SchemaVersion: 1, ID: "partial-failure", Query: "export", Scope: "src/", Unit: "matches", ExecutionStatus: lookupFailed, Status: "partial", Completeness: evidencePartial, CountKind: evidenceCountLowerBound, Errors: []string{"second query failed"}, Counts: lookupCounts{Requested: evidenceTestPtr(2), Succeeded: evidenceTestPtr(1), Failed: evidenceTestPtr(1), Total: evidenceTestPtr(2)}},
		{SchemaVersion: 1, ID: "bounded", Query: "export", Scope: "src/", Unit: "matches", ExecutionStatus: lookupSucceeded, Status: "partial", Completeness: evidencePartial, CountKind: evidenceCountLowerBound, Omissions: []string{"raw-rank cap"}, Counts: lookupCounts{Requested: evidenceTestPtr(1), Succeeded: evidenceTestPtr(1), Failed: evidenceTestPtr(0), Total: evidenceTestPtr(2)}},
		{SchemaVersion: 1, ID: "shown-partial", Query: "export", Scope: "src/", Unit: "matches", ExecutionStatus: lookupSucceeded, Status: "partial", Completeness: evidencePartial, CountKind: evidenceCountExact, Omissions: []string{"excerpt omitted"}, Counts: lookupCounts{Requested: evidenceTestPtr(1), Succeeded: evidenceTestPtr(1), Failed: evidenceTestPtr(0), Total: evidenceTestPtr(20)}},
	}
	encoded := map[string]bool{}
	for _, l := range cases {
		t.Run(l.ID, func(t *testing.T) {
			l = evidenceTestRoundTrip(t, l)
			if err := l.Validate(env); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(l)
			if encoded[string(b)] {
				t.Fatal("distinct lookup states encoded identically")
			}
			encoded[string(b)] = true
		})
	}
	if cases[0].Counts.Total != nil || cases[2].Counts.Total != nil || cases[1].Counts.Total == nil || *cases[1].Counts.Total != 0 {
		t.Fatal("unrequested/failed unknown results confused with successful empty results")
	}
	for _, c := range []evidenceCompleteness{evidenceComplete, evidencePartial, evidenceUnknown} {
		p := evidenceTestPacket("python")
		p.Completeness = c
		if err := evidenceTestRoundTrip(t, p).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEvidenceObservationsRoundTrip(t *testing.T) {
	values := []*bool{nil, evidenceTestPtr(false), evidenceTestPtr(true)}
	for _, selected := range values {
		for _, rendered := range values {
			for _, read := range values {
				for _, locator := range values {
					e := evidenceTestPacket("python").Evidence[0]
					e.Selected, e.Rendered, e.Read, e.LocatorValid = selected, rendered, read, locator
					if err := evidenceTestRoundTrip(t, e).Validate(); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	for _, kind := range []evidenceSourceKind{evidenceSourceCode, evidenceSourceTest, evidenceSourceSpec, evidenceSourceDocument, evidenceSourceAuthorHint, evidenceSourceToolOutput} {
		e := evidenceTestPacket("python").Evidence[0]
		e.SourceKind = kind
		if err := evidenceTestRoundTrip(t, e).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEvidenceLookupInvalidStates(t *testing.T) {
	cases := map[string]func(*evidenceLookup){
		"version":             func(l *evidenceLookup) { l.SchemaVersion = 2 },
		"id":                  func(l *evidenceLookup) { l.ID = " " },
		"execution":           func(l *evidenceLookup) { l.ExecutionStatus = "empty" },
		"status":              func(l *evidenceLookup) { l.Status = "success" },
		"count-kind":          func(l *evidenceLookup) { l.CountKind = "estimated" },
		"completeness":        func(l *evidenceLookup) { l.Completeness = "exact" },
		"negative":            func(l *evidenceLookup) { l.Counts.Total = evidenceTestPtr(-1) },
		"missing-total":       func(l *evidenceLookup) { l.Counts.Total = nil },
		"invented-total":      func(l *evidenceLookup) { l.CountKind = evidenceCountUnknown },
		"counts-overflow":     func(l *evidenceLookup) { l.Counts.Failed = evidenceTestPtr(2) },
		"unaccounted":         func(l *evidenceLookup) { l.Counts.Requested = evidenceTestPtr(2) },
		"missing-measurement": func(l *evidenceLookup) { l.Counts.Succeeded = nil },
		"successful-errors":   func(l *evidenceLookup) { l.Errors = []string{"failed"} },
		"fake-empty":          func(l *evidenceLookup) { l.Status = "empty" },
		"false-completeness":  func(l *evidenceLookup) { l.CountKind = evidenceCountLowerBound },
		"false-partial":       func(l *evidenceLookup) { l.Status = "partial" },
		"false-unrequested":   func(l *evidenceLookup) { l.ExecutionStatus = lookupNotRequested },
		"false-unavailable":   func(l *evidenceLookup) { l.ExecutionStatus = lookupUnavailable },
		"false-failure":       func(l *evidenceLookup) { l.ExecutionStatus = lookupFailed },
		"missing-scope":       func(l *evidenceLookup) { l.Scope = "" },
		"missing-unit":        func(l *evidenceLookup) { l.Unit = "" },
		"missing-query":       func(l *evidenceLookup) { l.Query = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := evidenceTestPacket("python")
			mutate(&p.Lookups[0])
			if err := p.Validate(); err == nil {
				t.Fatal("invalid lookup accepted")
			}
		})
	}
}

func TestEvidencePacketRejectsInvalidRecords(t *testing.T) {
	cases := map[string]func(*evidencePacket){
		"envelope-version":     func(p *evidencePacket) { p.SchemaVersion = 2 },
		"missing-subject":      func(p *evidencePacket) { p.Subject = nil },
		"unbound-subject":      func(p *evidencePacket) { p.Subject.ID = "forged" },
		"missing-completeness": func(p *evidencePacket) { p.Completeness = "" },
		"duplicate-lookup":     func(p *evidencePacket) { p.Lookups = append(p.Lookups, p.Lookups[0]) },
		"duplicate-evidence":   func(p *evidencePacket) { p.Evidence = append(p.Evidence, p.Evidence[0]) },
		"unknown-origin":       func(p *evidencePacket) { p.Evidence[0].LookupID = "unknown" },
		"wrong-origin-subject": func(p *evidencePacket) { p.Evidence[0].SubjectID = "old-subject" },
		"empty-origin":         func(p *evidencePacket) { p.Lookups[0].Status = "empty"; p.Lookups[0].Counts.Total = evidenceTestPtr(0) },
		"source-kind":          func(p *evidencePacket) { p.Evidence[0].SourceKind = "verified" },
		"content-hash":         func(p *evidencePacket) { p.Evidence[0].ContentHash = "" },
		"missing-path":         func(p *evidencePacket) { p.Evidence[0].Path = "" },
		"range-start":          func(p *evidencePacket) { p.Evidence[0].Range.StartLine = 0 },
		"range-order":          func(p *evidencePacket) { p.Evidence[0].Range.EndLine = 1 },
		"column-without-unit":  func(p *evidencePacket) { p.Evidence[0].Range.ColumnEncoding = "" },
		"column-without-end":   func(p *evidencePacket) { p.Evidence[0].Range.EndColumn = nil },
		"column-order":         func(p *evidencePacket) { p.Evidence[0].Range.EndColumn = evidenceTestPtr(1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := evidenceTestPacket("python")
			mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("invalid packet accepted")
			}
		})
	}
	for _, ids := range [][]string{{"unknown"}, {"90071992547409931234567890", "90071992547409931234567890"}} {
		p := evidenceTestPacket("python")
		if err := p.ValidateAttachment(ids, p.Subject, evidenceTestHashes(p)); err == nil {
			t.Fatalf("invalid attachment IDs accepted: %v", ids)
		}
	}
}

func TestEvidenceStaleAttachmentsRequireRevalidation(t *testing.T) {
	for _, change := range []string{"subject", "hash", "both", "locator"} {
		t.Run(change, func(t *testing.T) {
			p := evidenceTestPacket("typescript")
			hashes := evidenceTestHashes(p)
			e := &p.Evidence[0]
			if change == "subject" || change == "both" {
				old := evidenceTestSubject("old")
				e.SubjectID, p.Lookups[0].SubjectID = old.ID, old.ID
			}
			if change == "hash" || change == "both" {
				hashes[e.ID] = subjectDigest([]byte("changed body, unchanged signature"))
			}
			if change == "locator" {
				e.LocatorValid = nil
			}
			if err := p.ValidateAttachment([]string{e.ID}, p.Subject, hashes); err == nil || !strings.Contains(err.Error(), "revalidation required") {
				t.Fatalf("stale/unvalidated evidence attached without revalidation: %v", err)
			}
			r := evidenceRevalidation{SchemaVersion: 1, ID: "rv-1", EvidenceID: e.ID, FromSubjectID: effectiveSubjectID(e.SubjectID, p.evidenceEnvelope), FromContentHash: e.ContentHash, SubjectID: p.Subject.ID, ContentHash: hashes[e.ID], LocatorValid: true, Assessor: "reviewer-2", Method: "model", Rationale: "Reread current source range and rebound the observation."}
			p.Revalidations = []evidenceRevalidation{r}
			p = evidenceTestRoundTrip(t, p)
			if err := p.ValidateAttachment([]string{e.ID}, p.Subject, hashes); err != nil {
				t.Fatal(err)
			}
			if p.Evidence[0].ContentHash != r.FromContentHash {
				t.Fatal("revalidation rewrote historical evidence")
			}
			for _, bad := range []string{"old-hash", "old-subject", "current-hash", "current-subject", "assessor", "method", "locator", "unknown-id", "duplicate"} {
				t.Run(bad, func(t *testing.T) {
					q := evidenceTestRoundTrip(t, p)
					rv := &q.Revalidations[0]
					switch bad {
					case "old-hash":
						rv.FromContentHash = "wrong"
					case "old-subject":
						rv.FromSubjectID = "wrong"
					case "current-hash":
						rv.ContentHash = "wrong"
					case "current-subject":
						rv.SubjectID = "wrong"
					case "assessor":
						rv.Assessor = ""
					case "method":
						rv.Method = "confidence"
					case "locator":
						rv.LocatorValid = false
					case "unknown-id":
						rv.EvidenceID = "unknown"
					case "duplicate":
						q.Revalidations = append(q.Revalidations, *rv)
					}
					if err := q.ValidateAttachment([]string{e.ID}, q.Subject, hashes); err == nil {
						t.Fatal("invalid revalidation accepted")
					}
				})
			}
		})
	}
	p := evidenceTestPacket("python")
	id := p.Evidence[0].ID
	if err := p.ValidateAttachment([]string{id}, p.Subject, nil); err == nil {
		t.Fatal("unknown current hash accepted")
	}
	if err := p.ValidateAttachment([]string{id}, evidenceTestSubject("different"), evidenceTestHashes(p)); err == nil {
		t.Fatal("stale answer envelope accepted")
	}
}
