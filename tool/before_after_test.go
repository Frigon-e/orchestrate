package main

// These are paired, black-box regression experiments, not a second model bench.
// Both sides receive the same fixture bytes and fake backend transcripts. Gate's
// original CLI has no --json flag, so its persisted gate.json is the wire record.
import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func beforeAfterBinary(t *testing.T, before bool) string {
	t.Helper()
	if before {
		bin := os.Getenv("ORCH_BASELINE_BIN")
		if bin == "" {
			t.Skip("BEFORE not run: set ORCH_BASELINE_BIN to the original wip/ctx-gate-base binary")
		}
		abs, err := filepath.Abs(bin)
		if err != nil {
			t.Fatal(err)
		}
		if st, err := os.Stat(abs); err != nil || st.Mode()&0111 == 0 {
			t.Fatalf("invalid ORCH_BASELINE_BIN %q: %v", abs, err)
		}
		return abs
	}
	bin := filepath.Join(t.TempDir(), "orch")
	cmd := exec.Command("go", "build", "-p", "2", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build AFTER: %v: %s", err, out)
	}
	return bin
}

type beforeAfterCase struct {
	name string
	run  func(*testing.T, *harness, string, bool) string
}

func beforeAfterCLI(t *testing.T, h *harness, bin string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = h.repo
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("orch %v exceeded local 12s safety cap: %s", args, out.String())
	}
	if err == nil {
		return 0, out.String()
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode(), out.String()
	}
	t.Fatalf("orch %v: %v", args, err)
	return -1, ""
}

func beforeAfterJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("decode: %v: %s", err, text)
	}
	return v
}

// Legacy ctx JSON exposes definition headers/reference summaries in parts.raw.
// Inspect that decoded field, not the command's default text rendering.
func beforeAfterRaw(result map[string]any) string {
	var lines []string
	parts, _ := result["parts"].([]any)
	for _, part := range parts {
		p, _ := part.(map[string]any)
		raw, _ := p["raw"].([]any)
		for _, line := range raw {
			if text, ok := line.(string); ok {
				lines = append(lines, text)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func beforeAfterEmit(t *testing.T, name, side, result string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"scenario": name, "side": side, "result": result})
	t.Logf("BEFORE_AFTER %s", b)
}

func beforeAfterGate(t *testing.T, h *harness, bin string, before bool, extra ...string) (int, map[string]any) {
	t.Helper()
	args := append([]string{"gate", "spec.md", "--ctx-tool", "off"}, extra...)
	var code int
	var out string
	if before {
		// The baseline binary still has the flag. The current build has no
		// headless switch, so it runs in process with the test seam instead.
		code, out = beforeAfterCLI(t, h, bin, append(args, "--headless")...)
	} else {
		t.Setenv("HERDR_ENV", "")
		code, out = h.run(append(args, "--json")...)
	}
	dir := gateDir(h.a.siteRoot(t), specHash(mustRead(t, filepath.Join(h.repo, "spec.md"))))
	path := filepath.Join(dir, "gate.json")
	if !exists(path) {
		t.Fatalf("gate exited %d without record: %s", code, out)
	}
	return code, beforeAfterJSON(t, string(mustRead(t, path)))
}

func beforeAfterGateStatus(t *testing.T, code int, rec map[string]any, before bool, bad string) string {
	t.Helper()
	state, _ := rec["status"].(string)
	if before {
		if code != 0 || state != "pass" {
			t.Fatalf("baseline did not reproduce %s: exit=%d record=%v", bad, code, rec)
		}
		return "BAD: pass"
	}
	state, _ = rec["state"].(string)
	reasons, _ := rec["reasons"].([]any)
	if code == 0 || state == "pass" || (state != "fail" && len(reasons) == 0) {
		t.Fatalf("AFTER trusted %s or omitted reasons: exit=%d record=%v", bad, code, rec)
	}
	if state == "fail" {
		findings, _ := rec["findings"].([]any)
		structural := false
		for _, f := range findings {
			row, _ := f.(map[string]any)
			if row["verified"] == true && row["severity"] == "blocker" && row["support"] == "structural" {
				structural = true
			}
		}
		if !structural {
			t.Fatalf("fail without structural blocker: %v", rec)
		}
		return "blocked: fail; verified structural blocker"
	}
	if rec["state"] != "incomplete" {
		t.Fatalf("expected decoded v2 incomplete state for %s: %v", bad, rec["state"])
	}
	return "incomplete; reason recorded"
}

func TestBeforeAfter(t *testing.T) {
	// Build once before child tests so its lifetime covers every AFTER scenario.
	bin := beforeAfterBinary(t, false)
	cases := []beforeAfterCase{}
	for _, reply := range []struct{ name, text string }{{"empty-adversary", `{}`}, {"id-only-adversary", `{"packages":[{"id":"api"}]}`}} {
		cases = append(cases, beforeAfterCase{reply.name, func(t *testing.T, h *harness, bin string, before bool) string {
			h.gateRepo(1)
			h.fakePi(gatePasses(map[string]string{"group": cleanPackageReply, "adv-a": reply.text}))
			code, rec := beforeAfterGate(t, h, bin, before)
			return beforeAfterGateStatus(t, code, rec, before, reply.name)
		}})
	}
	cases = append(cases, beforeAfterCase{"future-file-ownership", func(t *testing.T, h *harness, bin string, before bool) string {
		h.commitSpec(planHeader + planPkg("api", "kind: ship\nowns: api/", "") + planPkg("writer", "kind: ship\nowns: writer/", "") + planTail)
		h.fakePi(gatePasses(map[string]string{"package-api": `{"id":"api","will_change":["writer/future.py"],"claims":[],"questions":[],"reuse":[]}`}))
		code, rec := beforeAfterGate(t, h, bin, before)
		return beforeAfterGateStatus(t, code, rec, before, "future-file collision")
	}})
	for _, lang := range []string{"py", "ts"} {
		for _, mutation := range []string{"body", "second-worktree", "untracked", "delete", "mode"} {
			cases = append(cases, beforeAfterCase{"approval-" + lang + "-" + mutation, func(t *testing.T, h *harness, bin string, before bool) string {
				path, body, changed := "app.py", "def value():\n    return 1\n", "def value():\n    return 2\n"
				if lang == "ts" {
					path, body, changed = "app.ts", "export function value() { return 1; }\n", "export function value() { return 2; }\n"
				}
				writeFiles(t, h.repo, map[string]string{path: body})
				h.commitSpec(planHeader + planPkg("api", "kind: ship\nowns: "+path, "") + planTail)
				h.fakePi(gatePasses(map[string]string{"group": cleanPackageReply}))
				code, rec := beforeAfterGate(t, h, bin, before)
				stateKey := "state"
				if before {
					stateKey = "status"
				}
				if code != 0 || rec[stateKey] != "pass" {
					t.Fatalf("approval control failed: %d %v", code, rec)
				}
				spec := "spec.md"
				switch mutation {
				case "body":
					writeFiles(t, h.repo, map[string]string{path: changed})
				case "untracked":
					writeFiles(t, h.repo, map[string]string{"extra." + lang: changed})
				case "delete":
					if err := os.Remove(filepath.Join(h.repo, path)); err != nil {
						t.Fatal(err)
					}
				case "mode":
					if err := os.Chmod(filepath.Join(h.repo, path), 0755); err != nil {
						t.Fatal(err)
					}
				case "second-worktree":
					wt := filepath.Join(h.tmp, "second")
					h.git("worktree", "add", "-q", "--detach", wt, "HEAD")
					spec = filepath.Join(wt, "spec.md")
				}
				code, out := beforeAfterCLI(t, h, bin, "plan", spec, "--no-ctx")
				if before {
					if code != 0 || len(allTasks(h.a.siteRoot(t))) != 1 {
						t.Fatalf("baseline approval not reusable: %d %s", code, out)
					}
					return "BAD: changed checkout authorized"
				}
				if code != 1 || !strings.Contains(out, "stale approval") || len(allTasks(h.a.siteRoot(t))) != 0 {
					t.Fatalf("AFTER reused approval: %d %s", code, out)
				}
				return "stale approval refused; no tasks created"
			}})
		}
	}
	for _, mutation := range []string{"source", "HEAD"} {
		cases = append(cases, beforeAfterCase{"verify-mutates-" + mutation, func(t *testing.T, h *harness, bin string, before bool) string {
			h.setCheck("true")
			h.commitSpec(intSpec())
			code, out := beforeAfterCLI(t, h, bin, "plan", "spec.md", "--skip-gate", "--no-ctx")
			if code != 0 {
				t.Fatal(out)
			}
			h.leadWork("alpha", map[string]string{"alpha/f": "original\n"})
			check := "echo changed > alpha/f"
			if mutation == "HEAD" {
				check = "git -c core.hooksPath=/dev/null commit --allow-empty -qm advanced"
			}
			h.setCheck(check)
			tip := h.branchTip(intBranch)
			code, out = beforeAfterCLI(t, h, bin, "verify", "alpha")
			integrated := exists(filepath.Join(h.dir("alpha"), "integrated"))
			if before {
				if code != 0 || !integrated {
					t.Fatalf("baseline did not integrate mutating check: %d %s", code, out)
				}
				return "BAD: mutating check integrated"
			}
			if code != 1 || integrated || h.branchTip(intBranch) != tip {
				t.Fatalf("AFTER integrated mutated candidate: %d %s", code, out)
			}
			return "refused; integration HEAD and marker unchanged"
		}})
	}
	for _, f := range portabilityFixtures {
		if f.name == "go" {
			continue
		}
		f := f
		for _, kind := range []string{"excluded-hits", "changed-body", "wrong-receiver", "missing-server", "failed-backend", "fifth-definition"} {
			cases = append(cases, beforeAfterCase{f.name + "-" + kind, func(t *testing.T, _ *harness, bin string, before bool) string {
				h := fixtureRepo(t, f.name)
				switch kind {
				case "excluded-hits":
					var raw strings.Builder
					for range 2105 {
						raw.WriteString(fixtureMatch(f.test, 1, "Normalize test"))
					}
					raw.WriteString(fixtureMatch(f.path, f.line, "Normalize production"))
					raw.WriteString(fixtureSummary)
					fixtureRG(t, h, raw.String())
					code, out := beforeAfterCLI(t, h, bin, "ctx", "text", "Normalize", "--json")
					v := beforeAfterJSON(t, out)
					if code != 0 {
						t.Fatal(out)
					}
					if before {
						if strings.Contains(out, f.path) {
							t.Fatalf("baseline unexpectedly retained production: %v", v)
						}
						return "BAD: 2105 excluded hits hide production"
					}
					r, _ := v["result"].(map[string]any)
					if r["total"] != float64(1) || !strings.Contains(out, f.path) {
						t.Fatalf("AFTER production lost: %v", v)
					}
					return "1 production hit retained after 2105 tests"
				case "changed-body":
					original := string(mustRead(t, filepath.Join(h.repo, f.path)))
					current := strings.Replace(original, f.old, f.current, 1)
					writeFiles(t, h.repo, map[string]string{f.path: current})
					b, _ := json.Marshal(map[string]any{"type": "search_result", "results": []any{map[string]any{"result": map[string]any{"sourcePosition": map[string]string{"relativePath": f.path}}, "content": original, "contentStartLine": 1}}})
					jb := fakeRunner(t, "jbcontext", "case \"$1\" in status) echo '{\"type\":\"status_result\",\"indices\":[]}';; search) printf '%s' '"+string(b)+"';; esac")
					t.Setenv("ORCH_JBCONTEXT", jb)
					code, out := beforeAfterCLI(t, h, bin, "ctx", "find", "Code that normalizes a file name", "--json")
					v := beforeAfterJSON(t, out)
					if code != 0 {
						t.Fatal(out)
					}
					if before {
						if strings.Contains(out, f.current) {
							t.Fatalf("baseline unexpectedly refreshed body: %v", v)
						}
						return "BAD: stale indexed body rendered"
					}
					if !strings.Contains(out, f.current) {
						t.Fatalf("AFTER stale body: %v", v)
					}
					return "current checkout body rendered"
				case "failed-backend":
					t.Setenv("ORCH_RG", fakeRunner(t, "rg", "echo failed >&2; exit 2"))
					code, out := beforeAfterCLI(t, h, bin, "ctx", "text", "Normalize", "--json")
					v := beforeAfterJSON(t, out)
					if before {
						if code != 0 {
							t.Fatalf("baseline unexpectedly fails: %d %v", code, v)
						}
						return "BAD: failed rg returned exit 0"
					}
					r, _ := v["result"].(map[string]any)
					if code != 1 || r["status"] != "failed" {
						t.Fatalf("AFTER failure hidden: %d %v", code, v)
					}
					return "exit 1; status failed, not zero"
				case "wrong-receiver", "missing-server", "fifth-definition":
					symbol := "Normalize"
					extra := []string{}
					if kind == "wrong-receiver" {
						symbol = "Wrong.Normalize"
					}
					raw := fixtureDeclaration(t, h, f)
					if kind == "fifth-definition" {
						raw = ""
						for _, dir := range []string{"aa", "ab", "ac", "ad", "ae"} {
							path := dir + "/reader" + f.ext
							body := "def Normalize(value):\n    return value.strip()\n"
							if f.name == "ts" {
								body = "export function Normalize(value: string) { return value.trim(); }\n"
							}
							if err := os.MkdirAll(filepath.Join(h.repo, dir), 0755); err != nil {
								t.Fatal(err)
							}
							writeFiles(t, h.repo, map[string]string{path: body})
							raw += fixtureMatch(path, 1, strings.Split(body, "\n")[0])
						}
						raw += fixtureSummary
						extra = []string{"--in", "ae"}
					}
					fixtureRG(t, h, raw)
					args := append([]string{"ctx", "sym", symbol, "--json"}, extra...)
					code, out := beforeAfterCLI(t, h, bin, args...)
					v := beforeAfterJSON(t, out)
					if code != 0 {
						t.Fatal(out)
					}
					r, _ := v["result"].(map[string]any)
					if before {
						if r["status"] != nil {
							t.Fatalf("baseline unexpectedly has truth status: %v", v)
						}
						if kind == "fifth-definition" {
							if r["shown"] != float64(0) {
								t.Fatalf("baseline fifth definition unexpectedly retained: %v", v)
							}
							return "BAD: scoped fifth definition hidden (shown 0)"
						}
						if r["total"] != float64(2) {
							t.Fatalf("baseline symbol fixture changed: %v", v)
						}
						if kind == "wrong-receiver" {
							if r["query"] != "Wrong.Normalize" || strings.Contains(beforeAfterRaw(r), "candidate (identity not verified)") {
								t.Fatalf("baseline receiver response changed: %v", r)
							}
							return "BAD: 2 definitions for nonexistent receiver, no identity warning"
						}
						if !strings.Contains(beforeAfterRaw(r), "used by 0") {
							t.Fatalf("baseline unexpectedly avoids false zero: %v", r)
						}
						return "BAD: missing server has no partial status; by-name zero references"
					}
					if r["status"] != "partial" {
						t.Fatalf("missing server not partial: %v", v)
					}
					errors, _ := r["errors"].([]any)
					if r["count_kind"] == "exact" || (kind == "missing-server" && len(errors) == 0) {
						t.Fatalf("missing server count uncertainty not recorded: %v", r)
					}
					if kind == "wrong-receiver" && !strings.Contains(beforeAfterRaw(r), "candidate (identity not verified)") {
						t.Fatalf("wrong receiver not marked candidate: %v", v)
					}
					if kind == "missing-server" && strings.Contains(beforeAfterRaw(r), "used by 0") {
						t.Fatalf("missing server became zero references: %v", r)
					}
					if kind == "fifth-definition" && (r["shown"] != float64(1) || r["total"] != float64(1) || !strings.Contains(out, "ae/reader")) {
						t.Fatalf("fifth definition hidden: %v", v)
					}
					return "partial; candidate identity/counts explicit"
				}
				return ""
			}})
		}
	}
	for _, kind := range []string{"exact-duplicate-questions", "related-questions-and-authority"} {
		cases = append(cases, beforeAfterCase{kind, func(t *testing.T, h *harness, bin string, before bool) string {
			src := planHeader + planPkg("api", "kind: ship\nowns: app.py", "") + planTail
			src = strings.Replace(src, "May decide:", "May decide: test layout,", 1)
			h.commitSpec(src)
			q1 := "What retention applies to `app.py` and `cache_days`?"
			q2 := "What retention applies to `app.py` and `cache_days`?"
			if kind == "related-questions-and-authority" {
				q2 = "Which test layout should verify `app.py` and `cache_days` using deterministic fixtures and isolated clocks?"
			}
			dir := gateDir(h.a.siteRoot(t), specHash([]byte(src)))
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			rec := map[string]any{"spec": "spec.md", "questions": []any{
				map[string]any{"package": "api", "question": q1, "changes_result": true},
				map[string]any{"package": "api", "question": q2, "changes_result": true},
			}}
			b, _ := json.Marshal(rec)
			writeFiles(t, dir, map[string]string{"gate.json": string(b), "report.md": "# Gate report\n"})
			code, out := beforeAfterCLI(t, h, bin, "gate", "enrich", "spec.md")
			if code != 0 {
				t.Fatalf("enrich: %d %s", code, out)
			}
			files, _ := filepath.Glob(filepath.Join(dir, "questions", "q*.md"))
			want := 1
			if !before && kind == "related-questions-and-authority" {
				want = 2
			}
			if len(files) != want {
				t.Fatalf("question count %d want %d", len(files), want)
			}
			tags := []string{}
			for _, file := range files {
				card, err := parseCard(string(mustRead(t, file)))
				if err != nil {
					t.Fatal(err)
				}
				tag := card.route
				if tag == "" {
					for _, section := range card.extra {
						if section.title == "Tag" {
							tag = strings.Fields(section.body)[0]
						}
					}
				}
				tags = append(tags, tag)
			}
			if kind == "exact-duplicate-questions" {
				if tags[0] != "owner" {
					t.Fatalf("duplicate lost owner: %v", tags)
				}
				return "1 exact-duplicate card; owner retained"
			}
			if before {
				if tags[0] != "lead-may-decide" {
					t.Fatalf("baseline did not lower authority: %v", tags)
				}
				return "BAD: 2 related questions merged; longer lead route replaces owner"
			}
			if strings.Join(tags, ",") != "owner,lead-may-decide" {
				t.Fatalf("AFTER independent authority lost: %v", tags)
			}
			return "2 independent cards; owner and lead routes retained"
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, before := range []bool{true, false} {
				side := "AFTER"
				if before {
					side = "BEFORE"
				}
				t.Run(side, func(t *testing.T) {
					t.Cleanup(func() {
						if t.Failed() {
							beforeAfterEmit(t, tc.name, side, "FINDING: comparison assertion failed; inspect this test's failure output")
						}
					})
					sideBin := bin
					if before {
						sideBin = beforeAfterBinary(t, true)
					}
					h := setup(t)
					result := tc.run(t, h, sideBin, before)
					beforeAfterEmit(t, tc.name, side, result)
				})
			}
		})
	}
}

// V2 lookup provenance did not exist in the original binary. Do not fabricate a
// BEFORE reducer or run today's reducer over an invented legacy transcript.
func TestBeforeAfterAbsence(t *testing.T) {
	for _, language := range []string{"python", "typescript"} {
		for _, kind := range []string{"failed", "partial", "capped", "unrelated-query"} {
			name := "absence-" + language + "-" + kind
			t.Run(name, func(t *testing.T) {
				t.Run("BEFORE", func(t *testing.T) {
					beforeAfterEmit(t, name, "BEFORE", "n/a on baseline: v2 lookup provenance and ReduceReview absent")
					t.Skip("n/a on baseline: v2 lookup provenance and ReduceReview absent")
				})
				t.Run("AFTER", func(t *testing.T) {
					s, cs, as, p := absenceReview(language)
					if r := ReduceReview(s, cs, as, nil, p); r.State != "pass" {
						t.Fatalf("completed-zero control: %+v", r)
					}
					l := &p.Packet.Lookups[0]
					switch kind {
					case "failed":
						l.ExecutionStatus = lookupFailed
						l.Status = "failed"
						l.Completeness = evidencePartial
						l.CountKind = evidenceCountUnknown
						l.Counts.Total = nil
						l.Counts.Succeeded = evidenceTestPtr(0)
						l.Counts.Failed = evidenceTestPtr(1)
						l.Errors = []string{"fake backend failed"}
					case "partial":
						l.Status = "partial"
						l.Completeness = evidencePartial
					case "capped":
						l.Omissions = []string{"retention cap"}
					case "unrelated-query":
						l.Query = "zzzz_unrelated_token"
					}
					replay := evidenceTestRoundTrip(t, struct {
						S *gateSubject
						C []reviewCheck
						A []reviewAnswer
						P reviewPolicy
					}{s, cs, as, p})
					r := ReduceReview(replay.S, replay.C, replay.A, nil, replay.P)
					if r.State != "incomplete" || r.Completion.Completed != 0 || len(r.Reasons) == 0 {
						t.Fatalf("untrustworthy absence passed: %+v", r)
					}
					beforeAfterEmit(t, name, "AFTER", "incomplete; 0 completed; reason recorded (decoded v2 replay)")
				})
			})
		}
	}
}
