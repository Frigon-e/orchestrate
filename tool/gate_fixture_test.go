package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Attach the reducer's valid control to actual fixture checkout bytes and a
// freshly captured subject. This avoids claiming language portability solely
// from changing a language label on fabricated evidence.
func fixtureReview(t *testing.T, h *harness, f portabilityFixture) (*gateSubject, []reviewCheck, []reviewAnswer, reviewPolicy) {
	t.Helper()
	_, cs, as, _, p := reducerFixture(f.language)
	s, err := captureGateSubject(h.repo, []byte("Normalize file names without changing case"))
	if err != nil {
		t.Fatal(err)
	}
	p.CurrentSubject = s
	p.Packet.Subject = s
	p.Packet.Lookups[0].Query = "Normalize"
	p.Packet.Lookups[0].Scope = "parser/"
	cs[0].SearchScope = "parser/"
	cs[0].SearchQuery = p.Packet.Lookups[0].Query
	cs[0].Item.ID = "parser#normalize"
	cs[0].Item.Package = "parser"
	cs[0].Item.Requirement = "Trim file names without changing their case"
	as[0].ItemID = cs[0].Item.ID
	e := &p.Packet.Evidence[0]
	e.Path = f.path
	e.Range = nil
	e.ContentHash = subjectDigest(mustRead(t, filepath.Join(h.repo, f.path)))
	p.CurrentHashes = evidenceTestHashes(p.Packet)
	as[0].MatchingBehavior = "Trims file names without changing their case"
	as[0].Support.Excerpt = f.old
	as[0].Support.FailureScenario = "Whitespace must be removed without changing letter case"
	as[0].Support.Rationale = "The current Normalize body trims whitespace"
	as[0].Rationale = as[0].Support.Rationale
	return s, cs, as, p
}

func TestFixtureGateTrust(t *testing.T) {
	for _, f := range portabilityFixtures {
		t.Run(f.name, func(t *testing.T) {
			h := fixtureRepo(t, f.name)
			t.Run("reuse-extend-new-and-unsupported", func(t *testing.T) {
				kinds := []string{"complete reuse", "partial reuse"}
				// These reducer decisions do not inspect language-specific evidence.
				if f.name == "go" {
					kinds = append(kinds, "new behavior", "unsupported", "stale")
				}
				for _, kind := range kinds {
					t.Run(kind, func(t *testing.T) {
						s, cs, as, p := fixtureReview(t, h, f)
						want := "pass"
						switch kind {
						case "partial reuse":
							as[0].Judgment = reviewJudgmentExtend
							as[0].MissingBehavior = "Add lower-case normalization as an optional mode"
						case "new behavior":
							as[0].Judgment = reviewJudgmentNew
							as[0].MatchingBehavior = "No streaming chunk processor exists"
							cs[0].Item.Requirement = "Process a file as bounded streaming chunks"
							as[0].Support.Rationale = "The complete recorded search found no streaming processor"
							as[0].Rationale = as[0].Support.Rationale
							p.Packet.Lookups[0].Query = "stream file chunks"
							cs[0].SearchQuery = p.Packet.Lookups[0].Query
							p.Packet.Lookups[0].Counts.Total = evidenceTestPtr(0)
							p.Packet.Lookups[0].Status = "empty"
							// A completed empty search is coverage, not an evidence row.
							as[0].LookupIDs = []string{p.Packet.Lookups[0].ID}
							as[0].EvidenceIDs = nil
							as[0].Support.EvidenceIDs = nil
							p.Packet.Evidence = nil
							p.CurrentHashes = nil
							as[0].Support.Excerpt = "0 production matches for stream file chunks"
						case "unsupported":
							as[0].Support = nil
							want = "incomplete"
						case "stale":
							p.CurrentSubject = evidenceTestSubject("changed")
							want = "stale"
						}
						r := ReduceReview(s, cs, as, nil, p)
						if r.State != want {
							t.Fatalf("%s: %+v", kind, r)
						}
					})
				}
			})
			if f.name == "go" {
				t.Run("product-answer-needs-owner", func(t *testing.T) {
					s, cs, as, p := fixtureReview(t, h, f)
					p.Questions = []reviewQuestion{{SchemaVersion: evidenceSchemaVersion, ID: "case-mode", ItemID: cs[0].Item.ID, ItemRevision: cs[0].Item.Revision, CheckID: cs[0].ID, Question: "Should file names be lower-cased?", Authority: reviewAuthority{Owner: "owner", Basis: "Public CLI behavior"}, Options: []reviewQuestionOption{{"yes", "Lower-case", "Changes output"}, {"no", "Preserve case", "Keeps output"}}, Answer: "yes", AnsweringActor: "reviewer"}}
					r := ReduceReview(s, cs, as, nil, p)
					if r.State != "incomplete" {
						t.Fatalf("reviewer settled product decision: %+v", r)
					}
					p.Questions[0].AnsweringActor = "owner"
					if r = ReduceReview(s, cs, as, nil, p); r.State != "pass" {
						t.Fatalf("owner control failed: %+v", r)
					}
				})
				t.Run("future-file-blocks", func(t *testing.T) {
					sp := groupSpec(map[string]string{"api": "parser/", "library": "other/"}, nil, "api", "library")
					rec := &gateRecord{}
					scopeFor("api", scopeReply{WillChange: []string{"other/future" + f.ext}}, rec, h.repo, sp)
					if len(rec.Findings) != 1 || !rec.Findings[0].Verified || rec.Findings[0].Severity != "blocker" || rec.Findings[0].Support != "structural" {
						t.Fatalf("future ownership not structural blocker: %+v", rec)
					}
				})
			}
			t.Run("dirty-tree-invalidates-approval", func(t *testing.T) {
				h.commitSpec(planHeader + planPkg("api", "kind: ship\nowns: parser/", "") + planTail)
				// Approval can bind an already dirty checkout, but not a later mutation.
				before := string(mustRead(t, filepath.Join(h.repo, f.path)))
				writeFiles(t, h.repo, map[string]string{f.path: before + "\n"})
				_, src, _ := saveSubjectApproval(t, h, h.repo)
				if !gatePassed(h.a.siteRoot(t), h.repo, src) {
					t.Fatal("dirty control lost approval")
				}
				writeFiles(t, h.repo, map[string]string{f.path: strings.Replace(before, f.old, f.current, 1)})
				code, out := h.run("plan", "spec.md", "--no-ctx")
				if code != 1 || !strings.Contains(out, "stale approval") || len(allTasks(h.a.siteRoot(t))) != 0 {
					t.Fatalf("stale dirty checkout approved: %d %s", code, out)
				}
			})
		})
	}
}

// fixtureGatePassLimit bounds one gate run over a fixture. A run that waits on
// a pass forever fails with a message instead of hanging the test binary.
const fixtureGatePassLimit = 45 * time.Second

// runWithin runs the command and fails the test when it has not finished in
// limit. The stuck run is left behind: the test binary ends it.
func (h *harness) runWithin(limit time.Duration, args ...string) (int, string) {
	h.t.Helper()
	type result struct {
		code int
		out  string
	}
	done := make(chan result, 1)
	go func() {
		code, out := h.run(args...)
		done <- result{code, out}
	}()
	select {
	case r := <-done:
		return r.code, r.out
	case <-time.After(limit):
		h.t.Fatalf("orch %s did not finish within %s: a gate pass is waiting on a fake that never answers", strings.Join(args, " "), limit)
		return 0, ""
	}
}

func TestFixtureGateModelReplies(t *testing.T) {
	for _, f := range portabilityFixtures {
		for _, reply := range []string{`{}`, `{"packages":[{"id":"api"}]}`, cleanPackageReply} {
			t.Run(f.name+"/"+reply, func(t *testing.T) {
				h := fixtureRepo(t, f.name)
				h.commitSpec(planHeader + planPkg("api", "kind: ship\nowns: parser/", "") + planTail)
				h.fakePi(gatePasses(map[string]string{"group": reply, "adv-a": `{"findings":[]}`, "adv-b": `{"findings":[]}`}))
				code, out := h.runWithin(fixtureGatePassLimit, "gate", "spec.md")
				wantCode := 1
				if reply == cleanPackageReply {
					wantCode = 0
				}
				if code != wantCode {
					t.Fatalf("reviewer control: want %d got %d %s", wantCode, code, out)
				}
				src := mustRead(t, filepath.Join(h.repo, "spec.md"))
				if gatePassed(h.a.siteRoot(t), h.repo, src) != (wantCode == 0) {
					t.Fatal("approval did not match reviewer validity")
				}
				var rec gateRecord
				if err := json.Unmarshal(mustRead(t, filepath.Join(gateDir(h.a.siteRoot(t), specHash(src)), "gate.json")), &rec); err != nil {
					t.Fatal(err)
				}
				if wantCode != 0 && (rec.State != "incomplete" || len(rec.Reasons) == 0) {
					t.Fatalf("missing incomplete diagnostic: %+v", rec)
				}
			})
		}
	}
}

func TestFixtureMixedDirtyIntegration(t *testing.T) {
	h := fixtureRepo(t, "mixed")
	// A fake runner mutates a second root while reviewing a pre-existing dirty
	// Python root. The integration boundary must discard even a clean reply.
	writeFiles(t, h.repo, map[string]string{"processor/reader.py": "def Normalize(value):\n    return value.strip().lower()\n"})
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", "printf changed > cli/reader.ts\nprintf '{}'"))
	runner, err := roleSite(profile{Harness: "pi", Model: "fixture"}).newRoleRun("gate", modelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rec := &gateRecord{PassErrors: map[string]string{}, PassSeconds: map[string]int{}, PassKept: map[string]string{}, PassTokens: map[string]passTokens{}}
	var applied atomic.Int32
	passes := []gatePass{{name: "fixture", class: "package", prompt: "review", apply: func(_ string, _ *gateRecord, _ string, _ *spec) error { applied.Add(1); return nil }}}
	runGatePasses(filepath.Join(h.tmp, "gate"), h.repo, "fixture", passes, rec, nil, gateOpts{runner: runner, headless: true})
	if applied.Load() != 0 || len(rec.TreeChanged) != 1 || rec.TreeChanged[0] != "cli/reader.ts" || rec.PassErrors["all"] == "" {
		t.Fatalf("dirty integration trusted changed tree: %+v", rec)
	}
}
