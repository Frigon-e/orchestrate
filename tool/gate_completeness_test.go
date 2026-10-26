package main

import (
	"path/filepath"
	"strings"
	"testing"
)

const cleanPackageReply = `{"packages":[{"id":"api","will_change":[],"claims":[],"questions":[],"reuse":[]}]}`

func TestGateReplyValidation(t *testing.T) {
	sp := parseSpec(threePkgSpec())
	for _, tc := range []struct{ name, reply, reason string }{
		{"missing packages", `{}`, "packages"},
		{"null packages", `{"packages":null}`, "packages"},
		{"id only", `{"packages":[{"id":"api"}]}`, "explicit arrays"},
		{"null array", strings.Replace(cleanPackageReply, `"claims":[]`, `"claims":null`, 1), "explicit arrays"},
		{"null question", strings.Replace(cleanPackageReply, `"questions":[]`, `"questions":[null]`, 1), "salvaged"},
		{"missing decisiveness", strings.Replace(cleanPackageReply, `"questions":[]`, `"questions":[{"question":"Overwrite?"}]`, 1), "salvaged"},
		{"unknown question package", strings.Replace(cleanPackageReply, `"questions":[]`, `"questions":[{"package":"unknown","question":"Overwrite?","changes_result":true}]`, 1), "unknown package"},
		{"wrong array type", strings.Replace(cleanPackageReply, `"claims":[]`, `"claims":{}`, 1), "salvaged"},
		{"unknown package", strings.Replace(cleanPackageReply, `"api"`, `"unknown"`, 1), "unknown package"},
		{"duplicate package", strings.Replace(cleanPackageReply, `}]}`, `},{"id":"api","will_change":[],"claims":[],"questions":[],"reuse":[]}]}`, 1), "duplicate package"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &gateRecord{}
			err := applyGroup("group:api", []string{"api"})(tc.reply, rec, t.TempDir(), sp)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err = %v, want %s", err, tc.reason)
			}
			if len(rec.Items) != 0 {
				t.Fatalf("invalid reply established item coverage: %+v", rec.Items)
			}
		})
	}
	for _, reply := range []string{`{}`, `{"findings":null}`, `{"findings":{}}`, `{"findings":[{"severity":"warning","where":"Design rules","claim":"bad enum","evidence":"writer.py:1"}]}`} {
		if err := applyFindings("adversary-a")(reply, &gateRecord{}, t.TempDir(), sp); err == nil {
			t.Errorf("accepted %s", reply)
		}
	}
}

func TestGateFutureFileOwnershipIsStructural(t *testing.T) {
	sp := groupSpec(map[string]string{"api": "api/", "writer": "writer/", "web": "web/"}, map[string]string{"web": "api"}, "api", "writer", "web")
	rec := &gateRecord{}
	scopeFor("api", scopeReply{WillChange: []string{"writer/new.py", "web/new.ts"}}, rec, t.TempDir(), sp)
	if len(rec.Findings) != 2 {
		t.Fatalf("findings: %+v", rec.Findings)
	}
	for i, sev := range []string{"blocker", "concern"} {
		f := rec.Findings[i]
		if !f.Verified || f.Support != "structural" || f.Severity != sev {
			t.Errorf("future ownership: %+v", f)
		}
	}
}

func TestGateFutureFileOwnershipBlocksApproval(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	h.fakePi(gatePasses(map[string]string{
		"package":     `{"id":"@ID@","will_change":[],"claims":[],"questions":[],"reuse":[]}`,
		"package-api": `{"id":"api","will_change":["migrations/future.sql"],"claims":[],"questions":[],"reuse":[]}`,
	}))
	if exists(filepath.Join(h.repo, "migrations", "future.sql")) {
		t.Fatal("fixture must be a future file")
	}
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("future-file ownership collision passed: %s", out)
	}
	if view := wantGateState(t, out, "fail"); view.Blockers != "1 verified" {
		t.Fatalf("future ownership blocker missing: %+v", view)
	}
}

func TestGateSalvagedFindingsRemainDiagnostic(t *testing.T) {
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"writer.py": "def write_rows(rows):\n    return rows\n"})
	raw := `{"findings":[{"severity":"concern","where":"Design rules","claim":"writer exists","evidence":"writer.py:1"}]`
	rec := &gateRecord{}
	err := applyFindings("adversary-a")(raw, rec, repo, nil)
	if err == nil || len(rec.Findings) != 1 || len(rec.Salvaged) != 1 {
		t.Fatalf("salvaged findings: err %v record %+v", err, rec)
	}
}

// Exercise the public gate with a fake reviewer, not just the decoder.
func TestGateReviewCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name, reply, state string
		code               int
	}{
		{"empty object", `{}`, "incomplete", 1},
		{"package ID only", `{"packages":[{"id":"api"}]}`, "incomplete", 1},
		{"explicit clean review", `{"findings":[]}`, "pass", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			gdir := h.gateRepo(1)
			h.fakePi(gatePasses(map[string]string{
				"group": cleanPackageReply,
				"adv-a": tc.reply,
			}))
			code, out := h.run("gate", "spec.md")
			if code != tc.code {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			wantGateState(t, out, tc.state)
			rec := readRecord(t, gdir)
			if tc.code != 0 && len(rec.Reasons) == 0 {
				t.Fatal("incomplete review has no reasons")
			}
			if !exists(filepath.Join(gdir, "gate.json")) {
				t.Fatal("missing gate record")
			}
		})
	}
}
