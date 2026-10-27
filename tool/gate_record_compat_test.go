package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A record written while the ctx switches existed keeps its ctx settings in the
// compare view; a record without them shows none.
func TestGateCompareShowsLegacyCtxSettingsOnly(t *testing.T) {
	for _, mode := range []string{"", "open", "ctx-only"} {
		t.Run(orDefault(mode, "legacy"), func(t *testing.T) {
			var out strings.Builder
			describeExperiment(&out, "A", "shell", &gateRecord{CtxTool: "on", CtxShell: mode}, t.TempDir())
			wantContains(t, out.String(), "ctx_tool: on; ctx_shell: "+orDefault(mode, "open"))
		})
	}
	var out strings.Builder
	describeExperiment(&out, "B", "new", &gateRecord{}, t.TempDir())
	if strings.Contains(out.String(), "ctx_") {
		t.Errorf("a new record shows ctx settings: %s", out.String())
	}
}

// A record written while --ctx-profile existed still loads, and its profile still
// shows; a record without one shows none. The flag itself is gone.
func TestGateRecordWithRemovedCtxProfileStillLoads(t *testing.T) {
	var old gateRecord
	if err := json.Unmarshal(mustRead(t, filepath.Join("testdata", "gate-record-ctx-profile.json")), &old); err != nil {
		t.Fatal(err)
	}
	if old.CtxProfile != "lean" || old.ReviewPolicy == nil || old.ReviewPolicy.CtxProfile != "lean" {
		t.Fatalf("the stored profile was lost: %+v", old)
	}
	var out strings.Builder
	describeExperiment(&out, "A", "lean-vs-default", &old, t.TempDir())
	wantContains(t, out.String(), "(cache short, groups group)", "ctx_shell: ctx-only; context_files: off; ctx_profile: lean")
	out.Reset()
	describeExperiment(&out, "B", "new", &gateRecord{}, t.TempDir())
	for _, gone := range []string{"ctx_profile", "context_files", "cache short"} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("a new record shows %s: %s", gone, out.String())
		}
	}
}

// The removed --context-files, --cache and --keep-sessions switches left their
// fields in old records: they load and the report still prints them.
func TestGateRecordWithRemovedSwitchesPrintsInReport(t *testing.T) {
	var old gateRecord
	if err := json.Unmarshal(mustRead(t, filepath.Join("testdata", "gate-record-ctx-profile.json")), &old); err != nil {
		t.Fatal(err)
	}
	if old.Cache != "short" || old.ContextFiles != "off" || old.ReviewPolicy.ContextFiles != "off" {
		t.Fatalf("a removed field was lost on load: %+v", old)
	}
	dir := t.TempDir()
	if err := writeGate(dir, &old); err != nil {
		t.Fatal(err)
	}
	wantContains(t, string(mustRead(t, filepath.Join(dir, "report.md"))), "ctx_tool: on; ctx_shell: ctx-only; context_files: off; ctx_profile: lean")
}

func TestGateCtxFlagsWereRemoved(t *testing.T) {
	h := setup(t)
	for _, args := range [][]string{{"--ctx-profile", "lean"}, {"-ctx-profile", "lean"}, {"--ctx-profile=lean"}, {"--ctx-tool", "on"}, {"--ctx-shell", "open"}} {
		code, out := h.run(append([]string{"gate", "spec.md"}, args...)...)
		if code == 0 {
			t.Errorf("%v: exit %d, output %q", args, code, out)
		}
	}
}
