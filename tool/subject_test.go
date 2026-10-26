package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func saveSubjectApproval(t *testing.T, h *harness, code string) (*gateRecord, []byte, string) {
	t.Helper()
	src := mustRead(t, filepath.Join(code, "spec.md"))
	subject, err := captureGateSubject(code, src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &gateRecord{Spec: "spec.md", Hash: specHash(src), Subject: subject,
		ReviewPolicy: &gateReviewPolicy{Version: gateReviewPolicyVersion, CtxTool: "on", ContextFiles: "off", CtxShell: "open"}}
	initGateReview(rec, &spec{}, string(src), nil)
	reduceGateRecord(rec)
	dir := gateDir(h.a.siteRoot(t), specHash(src))
	if err := writeGate(dir, rec); err != nil {
		t.Fatal(err)
	}
	return rec, src, dir
}

func TestPlanSubjectApprovalLegacyRefused(t *testing.T) {
	h := setup(t)
	h.commitSpec(planHeader + planPkg("alpha", "kind: ship\nowns: app.py", "") + planTail)
	src := mustRead(t, filepath.Join(h.repo, "spec.md"))
	dir := gateDir(h.a.siteRoot(t), specHash(src))
	// Historical bytes, not a legacy writer for new records.
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"gate.json": `{"status":"pass","spec":"spec.md","hash":"` + specHash(src) + `"}`})
	var legacy gateRecord
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "gate.json")), &legacy); err != nil {
		t.Fatal(err)
	}
	if err := writeGate(dir, &legacy); err != nil {
		t.Fatal(err)
	}
	wantContains(t, string(mustRead(t, filepath.Join(dir, "report.md"))), "Historical result: **pass** (legacy record, not a v2 state)", "off (legacy)")
	code, out := h.run("plan", "spec.md", "--dry-run", "--no-ctx")
	if code != 1 {
		t.Fatalf("legacy approval authorized planning: exit %d\n%s", code, out)
	}
	wantContains(t, out, "unbound or legacy approval", "fresh review", "nothing was created")
	if gatePassed(h.a.siteRoot(t), h.repo, src) {
		t.Fatal("legacy gatePassed")
	}
}

func TestPlanGateStateOnlyApproval(t *testing.T) {
	h := setup(t)
	h.commitSpec(planHeader + planPkg("alpha", "kind: ship\nowns: app.py", "") + planTail)
	_, src, dir := saveSubjectApproval(t, h, h.repo)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "gate.json")), &fields); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pass", "fail", "incomplete", "stale", "error", "experiment", ""} {
		t.Run(orDefault(state, "missing"), func(t *testing.T) {
			delete(fields, "status")
			fields["state"], _ = json.Marshal(state)
			if state != "pass" {
				// An obsolete pass cannot override the current state or supply it.
				fields["status"] = json.RawMessage(`"pass"`)
			}
			b, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			writeFiles(t, dir, map[string]string{"gate.json": string(b)})
			if got := gatePassed(h.a.siteRoot(t), h.repo, src); got != (state == "pass") {
				t.Fatalf("state %q approval = %v", state, got)
			}
		})
	}
}

func TestGateSubjectContentAndModes(t *testing.T) {
	// Language-neutral content identity must cover body-only changes in Python
	// and TypeScript, not just symbol/interface changes visible to a language server.
	repos := []struct{ name, manifest, config, path, before, after string }{
		{"python", "pyproject.toml", "[project]\nname = 'fixture'\nversion = '0.1.0'\n", "app.py", "def value():\n    return 1\n", "def value():\n    return 2\n"},
		{"typescript", "package.json", "{\"name\":\"fixture\",\"private\":true}\n", "app.ts", "export function value() { return 1; }\n", "export function value() { return 2; }\n"},
	}
	for _, repo := range repos {
		for _, kind := range []string{"body", "dirty-body", "untracked-content", "deletion", "mode", "untracked-mode", "symlink", "commit"} {
			t.Run(repo.name+"/"+kind, func(t *testing.T) {
				h := setup(t)
				writeFiles(t, h.repo, map[string]string{repo.manifest: repo.config, repo.path: repo.before})
				h.git("add", ".")
				h.git("commit", "-qm", "source")
				h.commitSpec(planHeader + planPkg("alpha", "kind: ship\nowns: "+repo.path, "") + planTail)
				file := filepath.Join(h.repo, repo.path)
				extra := filepath.Join(h.repo, "new source.txt")
				switch kind {
				case "dirty-body":
					writeFiles(t, h.repo, map[string]string{repo.path: repo.after})
				case "untracked-content", "untracked-mode":
					writeFiles(t, h.repo, map[string]string{"new source.txt": "one"})
				case "symlink":
					if err := os.Symlink("missing-a", extra); err != nil {
						t.Fatal(err)
					}
				}
				_, src, _ := saveSubjectApproval(t, h, h.repo)
				if !gatePassed(h.a.siteRoot(t), h.repo, src) {
					t.Fatal("unchanged subject lost approval")
				}
				h.must("plan", "spec.md", "--dry-run", "--no-ctx")
				switch kind {
				case "body", "commit":
					writeFiles(t, h.repo, map[string]string{repo.path: repo.after})
				case "dirty-body":
					writeFiles(t, h.repo, map[string]string{repo.path: repo.after + "\n"})
				case "untracked-content":
					writeFiles(t, h.repo, map[string]string{"new source.txt": "two"})
				case "deletion":
					if err := os.Remove(file); err != nil {
						t.Fatal(err)
					}
				case "mode":
					h.git("config", "core.filemode", "false")
					if err := os.Chmod(file, 0755); err != nil {
						t.Fatal(err)
					}
				case "untracked-mode":
					if err := os.Chmod(extra, 0755); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Remove(extra); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("missing-b", extra); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "commit" {
					h.git("add", repo.path)
					h.git("commit", "-qm", "body change")
				}
				if gatePassed(h.a.siteRoot(t), h.repo, src) {
					t.Fatal("changed subject reused approval")
				}
				code, out := h.run("plan", "spec.md", "--no-ctx")
				if code != 1 {
					t.Fatalf("stale approval authorized planning: %d\n%s", code, out)
				}
				wantContains(t, out, "stale approval", "changed", "fresh review", "nothing was created")
				if len(allTasks(h.a.siteRoot(t))) != 0 {
					t.Fatal("refused plan created tasks")
				}
			})
		}
	}
}

func TestGateSubjectRepoAndWorktreeIdentity(t *testing.T) {
	h := setup(t)
	h.commitSpec(planHeader + planPkg("alpha", "kind: ship\nowns: app.ts", "") + planTail)
	_, src, _ := saveSubjectApproval(t, h, h.repo)
	root := h.a.siteRoot(t)
	// Even a copied record and identical spec must not authorize a distinct repo.
	other := filepath.Join(h.tmp, "other")
	if _, err := git(h.tmp, "clone", "-q", h.repo, other); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, other, map[string]string{"app.ts": "export const value = 2;\n"})
	if ok, why := gateApproval(root, other, src); ok || !strings.Contains(why, "repository/checkout identity") {
		t.Fatalf("other repo: %v %s", ok, why)
	}
	wt := filepath.Join(h.tmp, "linked")
	h.git("worktree", "add", "-q", "-b", "other", wt)
	writeFiles(t, wt, map[string]string{"app.ts": "export const value = 3;\n"})
	if gatePassed(root, wt, src) {
		t.Fatal("linked worktree reused approval")
	}
	code, out := h.run("plan", filepath.Join(wt, "spec.md"), "--dry-run", "--no-ctx")
	if code != 1 {
		t.Fatalf("worktree approved: %d\n%s", code, out)
	}
	wantContains(t, out, "stale approval", "repository/checkout identity")
	// A review in the linked worktree is consumed against that worktree, even
	// when the command starts in the main checkout.
	saveSubjectApproval(t, h, wt)
	h.must("plan", filepath.Join(wt, "spec.md"), "--dry-run", "--no-ctx")
}

func TestGateSubjectRuntimeArtifacts(t *testing.T) {
	for _, inside := range []bool{false, true} {
		t.Run(onOff(inside), func(t *testing.T) {
			h := setup(t)
			if inside {
				t.Setenv("ORCH_HOME", filepath.Join(h.repo, ".runtime[gate]"))
			}
			writeFiles(t, h.repo, map[string]string{".gitignore": "ignored/\n"})
			h.commitSpec(planHeader + planPkg("alpha", "kind: ship\nowns: app.py", "") + planTail)
			_, src, dir := saveSubjectApproval(t, h, h.repo)
			for _, path := range []string{filepath.Join(dir, "dossier"), filepath.Join(dir, "sessions"), filepath.Join(h.repo, "ignored")} {
				if err := os.MkdirAll(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			writeFiles(t, dir, map[string]string{"dossier/alpha.md": "review output", "sessions/run.jsonl": "session"})
			writeFiles(t, h.repo, map[string]string{"ignored/cache": "ignored output"})
			if !gatePassed(h.a.siteRoot(t), h.repo, src) {
				t.Fatal("runtime artifacts invalidated approval")
			}
			writeFiles(t, h.repo, map[string]string{"report.md": "arbitrary source is NOT an artifact exclusion"})
			if gatePassed(h.a.siteRoot(t), h.repo, src) {
				t.Fatal("arbitrary file excluded")
			}
		})
	}
}

func TestGateSubjectUnreadableAndCompletion(t *testing.T) {
	h := setup(t)
	h.commitSpec(planHeader + planPkg("alpha", "kind: ship\nowns: app.py", "") + planTail)
	rec, src, _ := saveSubjectApproval(t, h, h.repo)
	// A FIFO is deterministically unreadable as a regular source file, including
	// when tests run as root. Fingerprinting must refuse it without blocking.
	if err := syscall.Mkfifo(filepath.Join(h.repo, "unreadable"), 0600); err != nil {
		t.Fatal(err)
	}
	if ok, why := gateApproval(h.a.siteRoot(t), h.repo, src); ok || !strings.Contains(why, "subject unverified") {
		t.Fatalf("unreadable subject: %v %s", ok, why)
	}
	finishGateSubject(rec, h.repo, filepath.Join(h.repo, "spec.md"))
	if rec.TreeCheckError == "" || len(rec.Reasons) == 0 {
		t.Fatalf("completion accepted unreadable subject: %+v", rec)
	}
	if err := os.Remove(filepath.Join(h.repo, "unreadable")); err != nil {
		t.Fatal(err)
	}
	rec.TreeCheckError = ""
	writeFiles(t, h.repo, map[string]string{"app.py": "def value(): return 2\n"})
	finishGateSubject(rec, h.repo, filepath.Join(h.repo, "spec.md"))
	if len(rec.SubjectStale) == 0 {
		t.Fatal("completion accepted changed subject")
	}
}

func TestGateSubjectRecordsReviewAndReusesApproval(t *testing.T) {
	h := setup(t)
	dir := h.gateRepo(1)
	h.fakePi(gatePasses(nil))
	h.must("gate", "spec.md", "--ctx-shell", "open")
	rec := readRecord(t, dir)
	if !rec.Subject.valid() || rec.ReviewPolicy == nil || rec.ReviewPolicy.CtxShell != "open" || rec.CtxShell != "open" {
		t.Fatalf("unbound review: %+v", rec)
	}
	src := mustRead(t, filepath.Join(h.repo, "spec.md"))
	if !gatePassed(h.a.siteRoot(t), h.repo, src) {
		t.Fatal("actual review not reusable")
	}
	h.must("plan", "spec.md", "--dry-run", "--no-ctx")
	if err := os.WriteFile(filepath.Join(h.repo, "unreadable"), []byte("private"), 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(h.repo, "unreadable"), 0600)
	if f, err := os.Open(filepath.Join(h.repo, "unreadable")); err == nil {
		f.Close()
		t.Skip("permission check requires non-root file permissions")
	}
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("unreadable checkout passed gate: %d %s", code, out)
	}
	failed := readRecord(t, dir)
	if failed.State != "error" || failed.TreeCheckError == "" || len(failed.Reasons) == 0 {
		t.Fatalf("unreadable gate: %+v", failed)
	}
}

func TestGateSubjectInvalidAndFullSpecIdentity(t *testing.T) {
	h := setup(t)
	h.commitSpec(planHeader + planPkg("alpha", "kind: ship\nowns: app.py", "") + planTail)
	rec, src, dir := saveSubjectApproval(t, h, h.repo)
	original := *rec.Subject
	for _, kind := range []string{"missing", "empty", "corrupt", "other-spec", "old-policy"} {
		t.Run(kind, func(t *testing.T) {
			subject := original
			rec.Subject = &subject
			rec.ReviewPolicy.Version = gateReviewPolicyVersion
			switch kind {
			case "missing":
				rec.Subject = nil
			case "empty":
				rec.Subject = &gateSubject{}
			case "corrupt":
				rec.Subject.ID = strings.Repeat("0", 64)
			case "other-spec":
				rec.Subject.SpecSHA256 = subjectDigest([]byte("different spec in the same short-hash directory"))
				rec.Subject.ID = rec.Subject.digest()
			case "old-policy":
				rec.ReviewPolicy.Version = "old-review-policy"
			}
			if err := writeGate(dir, rec); err != nil {
				t.Fatal(err)
			}
			if gatePassed(h.a.siteRoot(t), h.repo, src) {
				t.Fatal("invalid binding authorized planning")
			}
			code, out := h.run("plan", "spec.md", "--dry-run", "--no-ctx")
			if code != 1 {
				t.Fatalf("invalid approval: %d %s", code, out)
			}
			if kind == "other-spec" {
				wantContains(t, out, "spec content")
			} else {
				wantContains(t, out, "fresh review")
			}
		})
	}
}

func TestGateSubjectRuntimeReview(t *testing.T) {
	h := setup(t)
	t.Setenv("ORCH_HOME", filepath.Join(h.repo, ".gate-runtime"))
	h.gateRepo(1)
	dir := gateDir(h.a.siteRoot(t), specHash(mustRead(t, filepath.Join(h.repo, "spec.md"))))
	h.fakePi(gatePasses(nil))
	h.must("gate", "spec.md", "--ctx-tool", "off", "--ctx-shell", "ctx-only")
	rec := readRecord(t, dir)
	// ctx-only has no effect when the ctx tool is off. Record the effective
	// policy rather than falsely claiming that the shell was restricted.
	if rec.ReviewPolicy == nil || rec.CtxShell != "open" || rec.ReviewPolicy.CtxShell != "open" || rec.ReviewPolicy.CtxTool != "off" {
		t.Fatalf("effective review policy: %+v", rec.ReviewPolicy)
	}
	if !rec.Subject.valid() || rec.Subject.RuntimePath != ".gate-runtime" {
		t.Fatalf("runtime binding: %+v", rec.Subject)
	}
	h.must("plan", "spec.md", "--dry-run", "--no-ctx")
}

func TestCheckoutIgnoredSpecialFiles(t *testing.T) {
	h := setup(t)
	writeFiles(t, h.repo, map[string]string{".gitignore": "ignored.pipe\n"})
	before, err := checkoutFingerprint(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(h.repo, "ignored.pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := checkoutFingerprint(h.repo)
	if err != nil {
		t.Fatalf("gitignored special file must be excluded: %v", err)
	}
	if changed := checkoutChanges(h.repo, before, after); len(changed) != 0 {
		t.Fatalf("ignored special file changed fingerprint: %v", changed)
	}
}

func TestCheckoutCompletionRechecks(t *testing.T) {
	h := setup(t)
	guard, err := newCheckoutGuard(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.cancel()
	writeFiles(t, h.repo, map[string]string{"after-final-pass.py": "changed"})
	rec := &gateRecord{State: gateStatePass, PassErrors: map[string]string{}}
	guard.finish(rec)
	if rec.State != gateStatePass || len(rec.TreeChanged) != 1 || rec.PassErrors["all"] == "" {
		t.Fatalf("completion did not record the change without deciding state: %+v", rec)
	}
}
