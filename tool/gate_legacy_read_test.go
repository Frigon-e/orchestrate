package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGateStateHistoricalStatusReadOnly(t *testing.T) {
	for _, label := range []string{"pass", "fail", "unverified", "experiment"} {
		t.Run(label, func(t *testing.T) {
			data := []byte(`{"status":"` + label + `","spec":"spec.md","at":123}`)
			var rec gateRecord
			if err := json.Unmarshal(data, &rec); err != nil {
				t.Fatal(err)
			}
			if rec.State != "" || rec.HistoricalStatus != label || storedGateResultLabel(&rec) != label {
				t.Fatalf("invented a current state or lost history: %+v", rec)
			}
			b, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(b, &fields)
			if fields["status"] != nil || string(fields["historical_status"]) != `"`+label+`"` {
				t.Fatalf("historical re-export resurrected legacy status: %s", b)
			}
			var again gateRecord
			if err := json.Unmarshal(b, &again); err != nil || again.HistoricalStatus != label || again.State != "" {
				t.Fatalf("history did not round-trip: %+v %v", again, err)
			}
			// Current recorded state takes precedence over obsolete duplicate data.
			if err := json.Unmarshal([]byte(`{"schema_version":2,"state":"stale","status":"pass"}`), &rec); err != nil {
				t.Fatal(err)
			}
			if rec.State != "stale" || rec.HistoricalStatus != "" || storedGateResultLabel(&rec) != "stale" {
				t.Fatalf("legacy status overrode current state: %+v", rec)
			}
		})
	}
	var rec gateRecord
	if json.Unmarshal([]byte(`{"status":`), &rec) == nil {
		t.Fatal("corrupt stored gate JSON accepted")
	}
}

func TestBenchHistoricalArchiveStatusReadOnly(t *testing.T) {
	benchHarness(t)
	path := filepath.Join(benchDir(), "history", "runs", "old", "run.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := `{"schema_version":1,"id":"old","label":"old","suite":"history","arm":"old","status":"unverified"}`
	writeFiles(t, filepath.Dir(path), map[string]string{"run.json": original})
	run, err := benchRunLoad("history", "old")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "" || run.HistoricalStatus != "unverified" || run.dir != filepath.Dir(path) {
		t.Fatalf("old archive state/path changed: %+v", run)
	}
	if string(mustRead(t, path)) != original {
		t.Fatal("archive read changed the original")
	}
	b, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip benchRun
	if err := json.Unmarshal(b, &roundTrip); err != nil || roundTrip.HistoricalStatus != "unverified" || roundTrip.State != "" {
		t.Fatalf("archive label lost: %+v %v", roundTrip, err)
	}
	if strings.Contains(string(b), `"status":`) {
		t.Fatalf("archive re-export wrote legacy status: %s", b)
	}
}

func TestBenchCapturesStateWithoutLegacyStatus(t *testing.T) {
	for _, state := range []string{"error", "stale", "fail", "incomplete", "pass", "experiment"} {
		t.Run(state, func(t *testing.T) {
			h := benchHarness(t)
			h.gateRepo(1)
			dir := benchFixture(t, h, state, 1, true)
			rec := readRecord(t, dir)
			rec.State = state
			b, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			writeFiles(t, dir, map[string]string{"gate.json": string(b)})
			run, err := benchRecord(h.a, dir, "states", state, state)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := benchRunLoad("states", run.ID)
			if err != nil || loaded.State != state || loaded.HistoricalStatus != "" {
				t.Fatalf("capture changed state: %+v %v", loaded, err)
			}
			if b := string(mustRead(t, filepath.Join(loaded.dir, "run.json"))); strings.Contains(b, `"status":`) || strings.Contains(b, `"historical_status":`) {
				t.Fatalf("new archive wrote legacy status: %s", b)
			}
		})
	}
}

func TestBenchHistoricalGateImportAndReportLabels(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	dir := benchFixture(t, h, "old", 1, true)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "gate.json")), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "state")
	delete(fields, "schema_version")
	fields["status"] = json.RawMessage(`"unverified"`)
	original, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"gate.json": string(original)})
	run, err := benchRecord(h.a, dir, "history", "old", "old")
	if err != nil || run.State != "" || run.HistoricalStatus != "unverified" {
		t.Fatalf("historical import lost unknown state: %+v %v", run, err)
	}
	if string(mustRead(t, filepath.Join(dir, "gate.json"))) != string(original) {
		t.Fatal("import mutated original gate record")
	}
	for _, tc := range []struct{ report, state, historical string }{
		{"# Gate\nStatus: **fail** (spec version abc)\n", "", "**fail** (spec version abc)"},
		{"# Gate\nStatus: unverified\n", "", "unverified"},
		{"# Gate\nReview state: **incomplete** (schema 2)\n", "incomplete", ""},
		{"# Gate\nStatus: fail\nReview state: **incomplete** (schema 2)\n", "incomplete", ""},
		{"# Gate\n", "", ""},
	} {
		state, historical := readStoredGateReport(tc.report)
		if state != tc.state || historical.HistoricalStatus != tc.historical {
			t.Fatalf("report label changed: %q -> %q %+v", tc.report, state, historical)
		}
	}
}

func TestAuditGateStateAndHistoricalLabels(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ dir, json, result, label string }{
		{"current-pass", `{"state":"pass","status":"fail"}`, "ok", "pass"},
		{"current-stale", `{"state":"stale","status":"pass"}`, "problem", "stale"},
		{"current-incomplete", `{"state":"incomplete"}`, "problem", "incomplete"},
		{"old-pass", `{"status":"pass"}`, "ok", "pass"},
		{"old-unknown", `{"status":"unverified"}`, "problem", "unverified"},
	} {
		path := filepath.Join(root, "gate", tc.dir)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, path, map[string]string{"gate.json": tc.json})
		events := gateEvents(root)
		found := false
		for _, event := range events {
			if event.Text == " "+tc.label+" (spec version ): 0 blocker(s), 0 deterministic issue(s)" && event.Result == tc.result {
				found = true
			}
		}
		if !found {
			t.Fatalf("audit did not preserve current/historical label: %+v", events)
		}
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGateRecordIgnoresRemovedDossierFields(t *testing.T) {
	data := []byte(`{"schema_version":2,"state":"experiment","spec":"spec.md","only":["group:a"],"cache":"short",
		"dossier":"code","dossier_chars":12345,"dossier_manifest":{"lookups":[{"id":"l1"}]},
		"shadow":[{"exp":"x"}],"review_policy":{"dossier":"code","verdicts":true},
		"pass_errors":{"group:a":"late"},"notes":["kept"]}`)
	var rec gateRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.State != "experiment" || rec.Cache != "short" || rec.PassErrors["group:a"] != "late" || len(rec.Notes) != 1 {
		t.Fatalf("old record lost current fields: %+v", rec)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{`"dossier":"code"`, "dossier_chars", "dossier_manifest", `"l1"`, `"shadow":[`} {
		if strings.Contains(string(b), old) {
			t.Fatalf("removed field %s was written back: %s", old, b)
		}
	}
	var out strings.Builder
	describeExperiment(&out, "A", "old", &rec, t.TempDir())
	if !strings.Contains(out.String(), "experiment A: old (cache short") {
		t.Fatalf("old record did not describe: %q", out.String())
	}
	if arm := loadArm(t.TempDir(), "old", t.TempDir(), &rec); arm == nil {
		t.Fatal("old record did not load as an arm")
	}
}
