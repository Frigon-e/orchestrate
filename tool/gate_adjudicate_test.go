package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/toon-format/toon-go"
)

const adjudicationSourceFixture = "rename(temp, destination)\n"

func adjudicationFixture(t *testing.T) adjudicationEnvelope {
	t.Helper()
	s, cs, as, _, p := reducerFixture("go")
	cs[0].Assessors = append(cs[0].Assessors, adjudicatorRole)
	cs[0].DecisionDelegates = []string{adjudicatorRole}
	p.Packet.Evidence[0].ContentHash = subjectDigest([]byte(adjudicationSourceFixture))
	p.CurrentHashes = evidenceTestHashes(p.Packet)
	q := reviewQuestion{SchemaVersion: 1, ID: "atomic-choice", ItemID: cs[0].Item.ID, ItemRevision: cs[0].Item.Revision, CheckID: cs[0].ID, Question: "Does the writer preserve the previous destination on failure?", Authority: reviewAuthority{Owner: "owner", Delegated: true, Basis: "Recorded technical implementation delegation"}}
	for _, id := range []string{"reuse", "extend", "new", "unclear"} {
		q.Options = append(q.Options, reviewQuestionOption{id, id, "Select the implementation strategy"})
	}
	p.Questions = []reviewQuestion{q}
	g := &gateReviewRecords{Checks: cs, Answers: as, Policy: p}
	packet, err := buildAdjudicationPacket(s, g, q, func(evidenceRecord) (string, error) { return adjudicationSourceFixture, nil })
	if err != nil {
		t.Fatal(err)
	}
	return adjudicationEnvelope{Subject: s, AttemptID: "attempt-atomic", Packets: []adjudicationPacket{packet}, Review: g}
}

// adjudicationPairFixture adds a related unresolved item in the same package, so
// the text transport sends two rows of the same shape: a uniform packet table.
func adjudicationPairFixture(t *testing.T) adjudicationEnvelope {
	t.Helper()
	env := adjudicationFixture(t)
	c := env.Review.Checks[0]
	c.Item.ID += "-related"
	a := env.Review.Answers[0]
	a.ID += "-related"
	a.ItemID = c.Item.ID
	q := env.Review.Policy.Questions[0]
	q.ID += "-related"
	q.ItemID = c.Item.ID
	env.Review.Checks = append(env.Review.Checks, c)
	env.Review.Answers = append(env.Review.Answers, a)
	env.Review.Policy.Questions = append(env.Review.Policy.Questions, q)
	p, err := buildAdjudicationPacket(env.Subject, env.Review, q, func(evidenceRecord) (string, error) { return adjudicationSourceFixture, nil })
	if err != nil {
		t.Fatal(err)
	}
	env.Packets = append(env.Packets, p)
	return env
}

func adjudicationReplyFixture(env adjudicationEnvelope) adjudicationReplyBatch {
	b := adjudicationReplyBatch{SchemaVersion: 1}
	for _, p := range env.Packets {
		b.Replies = append(b.Replies, adjudicationProposals(p)[p.Claims[0].ID])
	}
	return b
}

func adjudicationRoleFixture(t *testing.T, p profile) *roleRun {
	t.Helper()
	t.Setenv("ORCH_HEALTH", filepath.Join(t.TempDir(), "health.json"))
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", "exit 99"))
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "exit 99"))
	s := site{cfg: config{userConfig: userConfig{Roles: map[string]modelRole{adjudicatorRole: {Use: []profile{p}}}}}}
	rr, err := s.newRoleRun(adjudicatorRole, modelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Readiness is seeded, not probed. Tests must not contact a provider.
	rr.keys = map[string]keyReadiness{"openrouter": {state: "ok", lane: "pi:openrouter"}}
	return rr
}

func adjudicationJSON(t *testing.T, b adjudicationReplyBatch) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAdjudicatorRoleAbsentDisabledAndIndependent(t *testing.T) {
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", "exit 99"))
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "exit 99"))
	s := roleSite(profile{Harness: "claude", Model: "reviewer"})
	s.cfg.Roles["decider"] = modelRole{Use: []profile{{Harness: "api", Provider: "openrouter", Model: "triage"}}}
	s.cfg.TriageModel = "legacy-triage"
	beforeGate, _ := s.newRoleRun("gate", modelOptions{})
	beforeDecider := s.role("decider")
	absent, _ := s.newRoleRun(adjudicatorRole, modelOptions{Runner: "claude:override"})
	if absent.enabled || len(absent.choices) != 1 || absent.state(absent.choices[0]) != "disabled" {
		t.Fatal("override enabled absent role", absent)
	}
	yes, no := true, false
	s.cfg.Roles[adjudicatorRole] = modelRole{Enabled: &yes, Use: []profile{{Harness: "claude", Model: "adjudicator"}}}
	enabled, _ := s.newRoleRun(adjudicatorRole, modelOptions{})
	if !enabled.enabled || enabled.choices[0].Model != "adjudicator" {
		t.Fatal(enabled)
	}
	afterGate, _ := s.newRoleRun("gate", modelOptions{})
	if !reflect.DeepEqual(beforeGate.choices, afterGate.choices) || !reflect.DeepEqual(beforeDecider, s.role("decider")) {
		t.Fatal("adjudicator toggling changed reviewer/triage routing")
	}
	s.cfg.Roles[adjudicatorRole] = modelRole{Enabled: &no, Use: enabled.choices}
	disabled, _ := s.newRoleRun(adjudicatorRole, modelOptions{Runner: "pi:override"})
	if disabled.enabled || disabled.state(disabled.choices[0]) != "disabled" {
		t.Fatal("explicit disable lost to override")
	}
	if !slices.Contains(modelRoles, adjudicatorRole) {
		t.Fatal("role missing from discovery")
	}
}

func TestAdjudicatorResolvedAndUserOwnedPathsHaveZeroCalls(t *testing.T) {
	for _, mode := range []string{"resolved", "user-owned", "absent", "explicit-disabled"} {
		t.Run(mode, func(t *testing.T) {
			env := adjudicationFixture(t)
			q := &env.Review.Policy.Questions[0]
			disabled := false
			roles := map[string]modelRole{adjudicatorRole: {Use: []profile{{Harness: "claude", Model: "haiku"}}}}
			switch mode {
			case "resolved":
				q.Answer = "reuse"
				q.AnsweringActor = "owner"
			case "user-owned":
				q.Authority.Delegated = false
				env.Review.Checks[0].DecisionDelegates = nil
			case "absent":
				delete(roles, adjudicatorRole)
			case "explicit-disabled":
				roles[adjudicatorRole] = modelRole{Enabled: &disabled}
			}
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "echo called >> '"+calls+"'; exit 99"))
			// No source files exist. Any packet formatting would require loading them
			// and create a failure audit. No audit and no subprocess proves early exit.
			rec := &gateRecord{Subject: env.Subject, Review: env.Review}
			offerGateAdjudication(site{cfg: config{userConfig: userConfig{Roles: roles}}}, rec, t.TempDir(), dir, time.Now())
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatal("unexpected adjudicator or formatter runner call")
			}
			if _, err := os.Stat(filepath.Join(dir, "adjudication.json")); !os.IsNotExist(err) {
				t.Fatal("resolved/unauthorized path formatted a packet")
			}
		})
	}
}

func TestAdjudicationProductionSeamWithFakeRunner(t *testing.T) {
	env := adjudicationFixture(t)
	// Two unresolved items from the same package share one model invocation.
	c := env.Review.Checks[0]
	c.Item.ID += "-related"
	a := env.Review.Answers[0]
	a.ID += "-related"
	a.ItemID = c.Item.ID
	q := env.Review.Policy.Questions[0]
	q.ID += "-related"
	q.ItemID = c.Item.ID
	env.Review.Checks = append(env.Review.Checks, c)
	env.Review.Answers = append(env.Review.Answers, a)
	env.Review.Policy.Questions = append(env.Review.Policy.Questions, q)
	p, err := buildAdjudicationPacket(env.Subject, env.Review, q, func(evidenceRecord) (string, error) { return adjudicationSourceFixture, nil })
	if err != nil {
		t.Fatal(err)
	}
	env.Packets = append(env.Packets, p)
	repo, dir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src/export.go"), []byte(adjudicationSourceFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]any{"type": "result", "result": adjudicationJSON(t, adjudicationReplyFixture(env))})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_FAKE_ADJ_REPLY", string(result))
	calls, args := filepath.Join(dir, "calls"), filepath.Join(dir, "args")
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "echo called >> '"+calls+"'\nprintf '%s\\n' \"$@\" > '"+args+"'\nprintf '%s\\n' \"$ORCH_FAKE_ADJ_REPLY\""))
	t.Setenv("ORCH_HEALTH", filepath.Join(dir, "health.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	s := site{cfg: config{userConfig: userConfig{Roles: map[string]modelRole{adjudicatorRole: {Use: []profile{{Harness: "claude", Model: "haiku"}}}}}}}
	rec := &gateRecord{Subject: env.Subject, Review: env.Review, At: time.Now().Unix()}
	offerGateAdjudication(s, rec, repo, dir, time.Now())
	raw, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(raw), "called") != 1 {
		t.Fatalf("fake runner calls %s %v", raw, err)
	}
	argv, err := os.ReadFile(args)
	if err != nil || !strings.Contains(string(argv), "Decision packets (TOON)") || !strings.Contains(string(argv), "--tools\n\n") {
		t.Fatal("missing tool-less TOON execution", string(argv), err)
	}
	audit, err := os.ReadFile(filepath.Join(dir, "adjudication.json"))
	if err != nil || !strings.Contains(string(audit), "input_usage_unknown") {
		t.Fatal("missing audit/unknown usage", err)
	}
	r := ReduceReview(env.Subject, env.Review.Checks, env.Review.Answers, nil, env.Review.Policy)
	if r.State != "pass" {
		t.Fatalf("production normalization failed: %+v audit=%s", r, audit)
	}
}

func TestAdjudicationPacketExactEvidenceSubsetAuthorityAndOmissions(t *testing.T) {
	env := adjudicationFixture(t)
	g := env.Review
	q := g.Policy.Questions[0]
	// A completed unrelated group is never delivered.
	extra := g.Answers[0]
	extra.ID = "unrelated"
	extra.ItemID = "other#done"
	g.Answers = append(g.Answers, extra)
	g.Policy.Packet.Lookups[0].Completeness = evidencePartial
	g.Policy.Packet.Lookups[0].Status = "partial"
	g.Policy.Packet.Lookups[0].Omissions = []string{"caller cleanup has not been inspected"}
	p, err := buildAdjudicationPacket(env.Subject, g, q, func(evidenceRecord) (string, error) { return adjudicationSourceFixture, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Claims) != 1 || len(p.Evidence) != 1 || p.Evidence[0].Excerpt != adjudicationSourceFixture || len(p.Lookups[0].Omissions) != 1 {
		t.Fatalf("lost evidence/omissions or leaked completed subset: %+v", p)
	}
	for _, bad := range []string{"", "only src/export.go:1", "rename(other, destination)\n"} {
		if _, err := buildAdjudicationPacket(env.Subject, g, q, func(evidenceRecord) (string, error) { return bad, nil }); err == nil {
			t.Fatal("accepted placeholder/stale excerpt")
		}
	}
	g.Policy.Packet.Lookups[0].Unit = "source_read"
	if _, err := buildAdjudicationPacket(env.Subject, g, q, func(evidenceRecord) (string, error) { return adjudicationSourceFixture, nil }); err == nil {
		t.Fatal("source read was treated as targeted search")
	}
	g.Checks[0].DecisionDelegates = nil
	if _, err := buildAdjudicationPacket(env.Subject, g, q, func(evidenceRecord) (string, error) { t.Fatal("loaded source before authority check"); return "", nil }); err == nil {
		t.Fatal("question invented delegation")
	}
}

func TestAdjudicationSourceRangeAndEscape(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("first\r\nsecond\nthird"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(repo, "escape")); err != nil {
		t.Fatal(err)
	}
	got, err := adjudicationSource(repo, evidenceRecord{Path: "source.go", Range: &evidenceRange{StartLine: 1, EndLine: 2}})
	if err != nil || got != "first\r\nsecond\n" {
		t.Fatal(got, err)
	}
	for _, e := range []evidenceRecord{{Path: "escape"}, {Path: "../secret"}, {Path: filepath.Join(outside, "secret")}, {Path: "source.go", Range: &evidenceRange{StartLine: 1, EndLine: 2, StartColumn: evidenceTestPtr(1), EndColumn: evidenceTestPtr(2), ColumnEncoding: "utf8"}}} {
		if _, err := adjudicationSource(repo, e); err == nil {
			t.Fatal("accepted escape/unsupported range", e)
		}
	}
}

func TestAdjudicationReplyValidationNeverImprovesInvalidState(t *testing.T) {
	cases := map[string]func(*adjudicationEnvelope, *adjudicationReplyBatch){
		"subject":  func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.SubjectID = "different-subject" },
		"revision": func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.Replies[0].ItemRevision = "old" },
		"item":     func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.Replies[0].ItemID = "other" },
		"question": func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.Replies[0].QuestionID = "other" },
		"action":   func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.Replies[0].Action = "approve_gate" },
		"judgment": func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.Replies[0].Judgment = "satisfied" },
		"evidence": func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) {
			b.Replies[0].EvidenceIDs = []string{"unknown-id"}
		},
		"claim": func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) {
			b.Replies[0].ClaimIDs = []string{"unknown-claim"}
		},
		"unaddressed": func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.Replies[0].ClaimIDs = nil },
		"matching":    func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.Replies[0].MatchingBehavior = "" },
		"rationale":   func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.Replies[0].Rationale = "" },
		"support":     func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) { b.Replies[0].Support = nil },
		"owner":       func(e *adjudicationEnvelope, _ *adjudicationReplyBatch) { e.Review.Checks[0].DecisionDelegates = nil },
		"authority-changed": func(e *adjudicationEnvelope, _ *adjudicationReplyBatch) {
			e.Review.Policy.Questions[0].Authority.Basis = "changed"
		},
		"unsupported-fields": func(_ *adjudicationEnvelope, b *adjudicationReplyBatch) {
			b.Replies[0].ResolutionFindingIDs = []string{"nonexistent-blocker"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env := adjudicationFixture(t)
			b := adjudicationReplyFixture(env)
			before := evidenceTestRoundTrip(t, *env.Review)
			mutate(&env, &b)
			if n, err := normalizeAdjudication(env, b); err == nil {
				t.Fatalf("accepted invalid reply: %+v", n)
			}
			// Validation never applies even a partially valid batch.
			if name != "owner" && name != "authority-changed" && !reflect.DeepEqual(before, *env.Review) {
				t.Fatal("normalization mutated earlier judgments")
			}
		})
	}
	env := adjudicationFixture(t)
	for _, raw := range []string{`{"schema_version":1,"replies":[],"attempt_id":"attacker"}`, adjudicationJSON(t, adjudicationReplyFixture(env)) + ` {}`, "not json"} {
		if _, err := parseAdjudicationReply(raw); err == nil {
			t.Fatal("accepted unknown envelope fields/trailing text")
		}
	}
}

func TestAdjudicationChatAndTypedRepliesNormalizeIdentically(t *testing.T) {
	env := adjudicationFixture(t)
	req, err := formatAdjudication(env, "decisions", "")
	if err != nil {
		t.Fatal(err)
	}
	selected := env.Packets[0].Claims[0].ID
	typed, err := normalizeAdjudicationDecisions(env, req, map[string]decAnswer{env.Packets[0].QuestionID: {Type: "choice", Choice: selected}})
	if err != nil {
		t.Fatal(err)
	}
	raw := adjudicationJSON(t, adjudicationReplyFixture(env))
	b, err := parseAdjudicationReply(raw)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := normalizeAdjudication(env, b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(chat, typed) {
		t.Fatalf("different normalized types: %+v / %+v", chat, typed)
	}
	if chat.Answers[0].Support.Assessor != adjudicatorRole || chat.Answers[0].ExecutionID != env.AttemptID {
		t.Fatal("reply chose trusted identity")
	}
	if _, ok := req.State["packets"].([]adjudicationPacket); !ok {
		t.Fatal("native object state was replaced by TOON")
	}
	text, err := formatAdjudication(env, "text", "")
	if err != nil {
		t.Fatal(err)
	}
	repair, err := formatAdjudication(env, "text", "bad schema")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text.Prompt, adjudicatorPrefix) || !strings.HasPrefix(repair.Prompt, text.Prompt) || !strings.Contains(text.Prompt, "excerpt:") || !strings.Contains(text.Prompt, adjudicationSourceFixture[:len(adjudicationSourceFixture)-1]) {
		t.Fatal("missing TOON source or unstable prefix")
	}
	for _, answers := range []map[string]decAnswer{{env.Packets[0].QuestionID: {Type: "text", Choice: selected}}, {env.Packets[0].QuestionID: {Type: "choice", Choice: `{"action":"resolve"}`}}, {"other": {Type: "choice", Choice: selected}}} {
		if _, err := normalizeAdjudicationDecisions(env, req, answers); err == nil {
			t.Fatal("unsupported typed fields accepted as chat")
		}
	}
	env.Packets[0].Claims[0].Answer.Support = nil
	if _, err := formatAdjudication(env, "decisions", ""); err == nil {
		t.Fatal("typed connector invented missing explanation/support")
	}
}

func TestAdjudicationConflictsAndExplicitBlockerResolutions(t *testing.T) {
	for _, mode := range []string{"agree", "conflict", "blocker", "explicit-resolution"} {
		t.Run(mode, func(t *testing.T) {
			env := adjudicationFixture(t)
			if mode == "blocker" || mode == "explicit-resolution" {
				_, _, _, fs, _ := reducerFixture("go")
				fs[0].EvidenceIDs = env.Review.Answers[0].EvidenceIDs
				env.Review.Findings = fs
				env.Packets[0].Claims = append(env.Packets[0].Claims, adjudicationClaim{ID: fs[0].ID, Finding: &fs[0]})
			}
			b := adjudicationReplyFixture(env)
			if mode == "conflict" {
				b.Replies[0].Judgment = reviewJudgmentExtend
				b.Replies[0].MissingBehavior = "Failure cleanup still needs extension"
			}
			if mode == "explicit-resolution" {
				b.Replies[0].ResolutionFindingIDs = []string{env.Review.Findings[0].ID}
			}
			n, err := normalizeAdjudication(env, b)
			if err != nil {
				t.Fatal(err)
			}
			rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
			audit := executeAdjudication(rr, newRunBudget(defaultAdjudicationLimits(), time.Now()), env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
				return adjudicationResponse{Text: adjudicationJSON(t, b)}, nil
			})
			if audit.Error != "" || !reflect.DeepEqual(*audit.Normalized, n) {
				t.Fatal(audit.Error)
			}
			applyAdjudication(env.Review, audit)
			r := ReduceReview(env.Subject, env.Review.Checks, env.Review.Answers, env.Review.Findings, env.Review.Policy)
			want := map[string]string{"agree": "pass", "conflict": "incomplete", "blocker": "fail", "explicit-resolution": "pass"}[mode]
			if r.State != want {
				t.Fatalf("got %s want %s: %+v", r.State, want, r)
			}
			if len(env.Review.Answers) != 2 {
				t.Fatal("adjudication overwrote an earlier judgment")
			}
			if mode == "explicit-resolution" && env.Review.Findings[0].ResolutionID == "" {
				t.Fatal("missing superseded finding resolution link")
			}
		})
	}
}

func TestAdjudicationRunBudgetRepairFallbackAndUsageUnknown(t *testing.T) {
	env := adjudicationFixture(t)
	rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
	l := defaultAdjudicationLimits()
	l.Requests = 1
	b := newRunBudget(l, time.Now())
	calls := 0
	run := func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
		calls++
		return adjudicationResponse{Text: "invalid JSON"}, nil
	}
	audit := executeAdjudication(rr, b, env, run)
	if calls != 1 || audit.Reason != reviewBudgetExhausted || b.Requests != 1 || !b.UnknownInput || !b.UnknownOutput {
		t.Fatalf("repair reset/lost unknown usage: calls=%d audit=%+v budget=%+v", calls, audit, b)
	}
	rr2 := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "sonnet"})
	again := executeAdjudication(rr2, b, env, run)
	if calls != 1 || again.Reason != reviewBudgetExhausted {
		t.Fatal("provider switch reset shared budget")
	}
	b = newRunBudget(defaultAdjudicationLimits(), time.Now())
	calls = 0
	audit = executeAdjudication(rr, b, env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
		calls++
		text := "bad"
		if calls == 2 {
			text = adjudicationJSON(t, adjudicationReplyFixture(env))
		}
		return adjudicationResponse{Text: text}, nil
	})
	if audit.Error != "" || calls != 2 || b.Requests != 2 || b.Repairs != 1 {
		t.Fatalf("repair not shared/bounded: %+v %d", audit, b.Requests)
	}
	b = newRunBudget(defaultAdjudicationLimits(), time.Now())
	b.Limits.Requests = 4
	if err := b.admit(1, false, false); err != nil {
		t.Fatal(err)
	}
	b.LastRoute = (profile{Harness: "claude", Model: "haiku"}).String()
	// A new role/profile does not create a new budget. Its provider switch is charged.
	audit = executeAdjudication(rr2, b, env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
		return adjudicationResponse{Text: adjudicationJSON(t, adjudicationReplyFixture(env))}, nil
	})
	if audit.Error != "" || b.Fallbacks != 1 || b.Requests != 2 {
		t.Fatalf("fallback not shared: %+v %+v", audit, b)
	}
	if err := b.admit(1, false, true); !errors.Is(err, errAdjudicationBudget) {
		t.Fatal("fallback counter reset", err)
	}
}

func TestAdjudicationBudgetExhaustionAndDollarEstimate(t *testing.T) {
	for _, kind := range []string{"requests", "input", "output", "elapsed", "repair", "fallback", "dollars"} {
		t.Run(kind, func(t *testing.T) {
			l := defaultAdjudicationLimits()
			b := newRunBudget(l, time.Now())
			repair, fallback := false, false
			switch kind {
			case "requests":
				b.Requests = l.Requests
			case "input":
				b.ReportedInput = l.InputTokens
			case "output":
				b.ReportedOutput = l.OutputTokens
			case "elapsed":
				b.Started = time.Now().Add(-time.Hour)
			case "repair":
				b.Repairs = l.Repairs
				repair = true
			case "fallback":
				b.Fallbacks = l.Fallbacks
				fallback = true
			case "dollars":
				b.Limits.MaxUSD = 0.01
				b.Limits.ReliableEstimate = true
				b.Limits.EstimatedRequestUSD = 0.02
			}
			if err := b.admit(1, repair, fallback); !errors.Is(err, errAdjudicationBudget) {
				t.Fatal(kind, err)
			}
		})
	}
	b := newRunBudget(defaultAdjudicationLimits(), time.Now())
	b.Limits.MaxUSD = 1
	if err := b.admit(1, false, false); err == nil || errors.Is(err, errAdjudicationBudget) {
		t.Fatal("unreliable dollar cap silently accepted")
	}
	env := adjudicationFixture(t)
	rec := &gateRecord{At: time.Now().Add(-5 * time.Second).Unix(), PassSeconds: map[string]int{"unreported": 1, "reported": 2}, PassTokens: map[string]passTokens{"reported": {Requests: 2, Input: 10, CacheRead: 3, CacheWrite: 2, Output: 7, NoCost: 1}}}
	seeded := seedAdjudicationBudget(rec, defaultAdjudicationLimits(), time.Now())
	if seeded.Requests != 3 || seeded.ReportedInput != 15 || seeded.ReportedOutput != 7 || !seeded.UnknownInput || !seeded.UnknownOutput || !seeded.UnknownCost {
		t.Fatal("seed invented complete/zero usage", seeded)
	}
	rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
	calls := 0
	audit := executeAdjudication(rr, seeded, env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
		calls++
		return adjudicationResponse{}, nil
	})
	if calls != 0 || audit.Reason != reviewBudgetExhausted || audit.NextAction == "" {
		t.Fatal("already exhausted run made a call", audit)
	}
	applyAdjudication(env.Review, audit)
	r := ReduceReview(env.Subject, env.Review.Checks, env.Review.Answers, nil, env.Review.Policy)
	if r.State != "incomplete" || !slices.ContainsFunc(r.Reasons, func(r reviewNextAction) bool { return r.Reason == reviewBudgetExhausted }) {
		t.Fatal("exhaustion improved state", r)
	}
}

func TestAdjudicationDeadlineAndReportedCaps(t *testing.T) {
	env := adjudicationFixture(t)
	rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
	b := newRunBudget(defaultAdjudicationLimits(), time.Now())
	audit := executeAdjudication(rr, b, env, func(ctx context.Context, _ profile, _ adjudicationRequest) (adjudicationResponse, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing hard deadline")
		}
		return adjudicationResponse{Text: adjudicationJSON(t, adjudicationReplyFixture(env)), Usage: executionUsage{Availability: "available", InputTokens: evidenceTestPtr(b.Limits.InputTokens + 1), OutputTokens: evidenceTestPtr(int64(1))}}, nil
	})
	if audit.Reason != reviewBudgetExhausted || audit.Normalized != nil || b.Requests != 1 {
		t.Fatal("over-budget reply improved state", audit)
	}
}

func TestAdjudicationChargesReportedRequestsAndPreflightsAuthority(t *testing.T) {
	env := adjudicationFixture(t)
	rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
	l := defaultAdjudicationLimits()
	l.Requests = 1
	b := newRunBudget(l, time.Now())
	audit := executeAdjudication(rr, b, env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
		return adjudicationResponse{Text: adjudicationJSON(t, adjudicationReplyFixture(env)), ReportedRequests: 2}, nil
	})
	if audit.Reason != reviewBudgetExhausted || audit.Normalized != nil || b.Requests != 2 {
		t.Fatal("discarded reported extra requests", audit, b)
	}
	env.Review.Checks[0].DecisionDelegates = nil
	audit = executeAdjudication(rr, newRunBudget(defaultAdjudicationLimits(), time.Now()), env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
		t.Fatal("user-owned decision reached a runner")
		return adjudicationResponse{}, nil
	})
	if audit.Error == "" {
		t.Fatal("authority checked only after model call")
	}
}

func TestAdjudicationValidUncertaintyDoesNotRetry(t *testing.T) {
	for _, action := range []string{"request_evidence", "defer_to_owner"} {
		t.Run(action, func(t *testing.T) {
			env := adjudicationFixture(t)
			rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
			reply := adjudicationReplyFixture(env)
			r := &reply.Replies[0]
			r.Action = action
			r.Judgment = reviewJudgmentUnclear
			r.Support = nil
			r.RequestedLookup = "Inspect the caller's failure cleanup"
			r.RemainingQuestion = env.Packets[0].Question
			calls := 0
			b := newRunBudget(defaultAdjudicationLimits(), time.Now())
			audit := executeAdjudication(rr, b, env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
				calls++
				return adjudicationResponse{Text: adjudicationJSON(t, reply)}, nil
			})
			if audit.Error != "" || calls != 1 || b.Repairs != 0 || audit.Normalized.Answers[0].NextAction == "" {
				t.Fatal("unchanged uncertainty resubmitted", audit, calls)
			}
			applyAdjudication(env.Review, audit)
			result := ReduceReview(env.Subject, env.Review.Checks, env.Review.Answers, nil, env.Review.Policy)
			if result.State != "incomplete" || env.Review.Policy.Questions[0].Answer != "" {
				t.Fatal("uncertainty approved a decision", result)
			}
		})
	}
}

func TestAdjudicationUniformPacketsRule(t *testing.T) {
	field := func(key string, value any) toon.Field { return toon.Field{Key: key, Value: value} }
	row := func(fields ...toon.Field) any { return toon.NewObject(fields...) }
	cases := []struct {
		name string
		rows any
		want bool
	}{
		{"identical rows", []any{row(field("a", 1), field("b", "x")), row(field("a", 2), field("b", "y"))}, true},
		{"different lengths", []any{row(field("a", []any{1})), row(field("a", []any{1, 2, 3}))}, true},
		{"different scalar kinds", []any{row(field("a", "1")), row(field("a", 1.0))}, true},
		{"one packet", []any{row(field("a", 1))}, true},
		{"no packets", []any{}, false},
		{"not a list", row(field("a", 1)), false},
		{"different field order", []any{row(field("a", 1), field("b", 2)), row(field("b", 2), field("a", 1))}, false},
		{"different field set", []any{row(field("a", 1)), row(field("a", 1), field("b", 2))}, false},
		{"null against array", []any{row(field("a", nil)), row(field("a", []any{}))}, false},
		{"empty against non-empty array", []any{row(field("a", []any{})), row(field("a", []any{1}))}, false},
		{"different item shape", []any{row(field("a", []any{1})), row(field("a", []any{row(field("k", 1))}))}, false},
		{"different nested field", []any{row(field("a", row(field("k", 1)))), row(field("a", row(field("k", 1), field("j", 2))))}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := uniformPackets(selectedOutput{value: c.rows}); got != c.want {
				t.Fatalf("uniform=%v want %v", got, c.want)
			}
		})
	}
}

func TestAdjudicationPacketFormatGuard(t *testing.T) {
	env := adjudicationPairFixture(t)
	s, err := selectOutput(env.Packets)
	if err != nil {
		t.Fatal(err)
	}
	agentRender := func(s selectedOutput) ([]byte, error) { return s.render(outputAgent) }
	format, reason, body := adjudicationPacketBody(s, true, agentRender)
	if format != "toon" || reason != "" || !strings.Contains(string(body), "schema_version") {
		t.Fatalf("uniform lossless packets should stay TOON: %s %s %q", format, reason, body)
	}
	req, err := formatAdjudicationText(env, "", true)
	if err != nil || req.Format != "toon" || !strings.Contains(req.Prompt, "Decision packets (TOON):") || req.InputAllowance != int64(len(req.Prompt)) {
		t.Fatalf("toon request: %+v %v", req.Format, err)
	}
	// A rendering that decodes but loses fields must fall back to the source JSON.
	format, reason, body = adjudicationPacketBody(s, true, func(selectedOutput) ([]byte, error) { return []byte("schema_version: 1\n"), nil })
	if format != "json" || reason != "toon round trip mismatch" || string(body) != string(s.json) {
		t.Fatalf("lossy round trip not caught: %s %s", format, reason)
	}
	// An undecodable body is the same fallback.
	if format, reason, _ = adjudicationPacketBody(s, true, func(selectedOutput) ([]byte, error) { return []byte("\t\tunexpected"), nil }); format != "json" || reason != "toon round trip mismatch" {
		t.Fatalf("undecodable toon not caught: %s %s", format, reason)
	}
	if format, reason, _ = adjudicationPacketBody(s, true, func(selectedOutput) ([]byte, error) { return nil, errors.New("encode failed") }); format != "json" || reason != "toon encode error" {
		t.Fatalf("encode error not caught: %s %s", format, reason)
	}
	// Mixed packets and the JSON repair resend never reach the TOON encoder.
	env.Packets[1].Claims = nil
	mixed, err := selectOutput(env.Packets)
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	format, reason, body = adjudicationPacketBody(mixed, true, func(s selectedOutput) ([]byte, error) { reached = true; return agentRender(s) })
	if format != "json" || reason != "packets are not uniformly shaped" || reached || string(body) != string(mixed.json) {
		t.Fatalf("mixed packets used TOON: %s %s reached=%v", format, reason, reached)
	}
	if req, err = formatAdjudicationText(env, "", true); err != nil || req.Format != "json" || !strings.Contains(req.Prompt, "Decision packets (JSON):") {
		t.Fatalf("mixed request did not use JSON: %+v %v", req.Format, err)
	}
	if format, reason, _ = adjudicationPacketBody(s, false, agentRender); format != "json" || reason != "unusable reply" {
		t.Fatalf("repair resend not JSON: %s %s", format, reason)
	}
}

func TestAdjudicationUnusableReplyRetriesOnceAsJSON(t *testing.T) {
	env := adjudicationPairFixture(t)
	rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
	b := newRunBudget(defaultAdjudicationLimits(), time.Now())
	var prompts []string
	audit := executeAdjudication(rr, b, env, func(_ context.Context, _ profile, req adjudicationRequest) (adjudicationResponse, error) {
		prompts = append(prompts, req.Prompt)
		if len(prompts) == 1 {
			return adjudicationResponse{Text: "not an adjudication reply"}, nil
		}
		return adjudicationResponse{Text: adjudicationJSON(t, adjudicationReplyFixture(env))}, nil
	})
	if audit.Error != "" || audit.Normalized == nil {
		t.Fatalf("one unusable reply voided the pass: %+v", audit)
	}
	if len(prompts) != 2 || b.Repairs != 1 || b.Requests != 2 {
		t.Fatalf("retry not exactly once or budget not shared: calls=%d %+v", len(prompts), b)
	}
	if !strings.Contains(prompts[0], "Decision packets (TOON):") || !strings.Contains(prompts[1], "Decision packets (JSON):") {
		t.Fatal("repair did not switch the packet encoding")
	}
	if !strings.Contains(prompts[1], "Repair only the invalid response schema/fields") {
		t.Fatal("repair text missing")
	}
	want := []adjudicationPacketFormat{{Format: "toon"}, {Format: "json", Reason: "unusable reply"}}
	if !reflect.DeepEqual(audit.TextPackets, want) {
		t.Fatalf("recorded formats %+v want %+v", audit.TextPackets, want)
	}
	if len(audit.Execution.Attempts) != 2 || audit.Execution.Attempts[0].Status != "failed" || audit.Execution.Attempts[1].Status != "succeeded" {
		t.Fatalf("attempt records: %+v", audit.Execution.Attempts)
	}
	applyAdjudication(env.Review, audit)
	if r := ReduceReview(env.Subject, env.Review.Checks, env.Review.Answers, nil, env.Review.Policy); r.State != "pass" {
		t.Fatalf("repaired reply did not decide the pass: %+v", r)
	}
}

func TestAdjudicationTwoUnusableRepliesFail(t *testing.T) {
	env := adjudicationPairFixture(t)
	rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
	calls := 0
	audit := executeAdjudication(rr, newRunBudget(defaultAdjudicationLimits(), time.Now()), env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
		calls++
		return adjudicationResponse{Text: "still not an adjudication reply"}, nil
	})
	if calls != 2 || audit.Error == "" || audit.Normalized != nil || audit.Reason != reviewDependencyUnavailable {
		t.Fatalf("second failure not executed as before: calls=%d %+v", calls, audit)
	}
	if len(audit.TextPackets) != 2 || audit.TextPackets[1].Reason != "unusable reply" {
		t.Fatalf("recorded formats %+v", audit.TextPackets)
	}
}

func TestAdjudicationDecisionsRouteNeverUsesChat(t *testing.T) {
	env := adjudicationFixture(t)
	rr := adjudicationRoleFixture(t, profile{Harness: "api", Provider: "openrouter", Model: "perplexity/decider"})
	calls := 0
	audit := executeAdjudication(rr, newRunBudget(defaultAdjudicationLimits(), time.Now()), env, func(_ context.Context, p profile, req adjudicationRequest) (adjudicationResponse, error) {
		calls++
		if p.Harness != "api" || req.Prompt != "" || req.State == nil || req.Questions == nil {
			t.Fatal("decisions route silently used chat")
		}
		return adjudicationResponse{Decisions: map[string]decAnswer{env.Packets[0].QuestionID: {Type: "choice", Choice: env.Packets[0].Claims[0].ID}}}, nil
	})
	if audit.Error != "" || calls != 1 {
		t.Fatal(audit.Error, calls)
	}
	env.Packets[0].Claims[0].Answer.Support = nil
	audit = executeAdjudication(rr, newRunBudget(defaultAdjudicationLimits(), time.Now()), env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
		t.Fatal("unsupported typed fields made a call")
		return adjudicationResponse{}, nil
	})
	if audit.Reason != reviewDependencyUnavailable || !strings.Contains(audit.Error, "required evidence and explanation") {
		t.Fatal(audit)
	}
}

func TestAdjudicationCacheModesAndShadowAreRecordOnly(t *testing.T) {
	var results []reviewResult
	for _, cache := range []string{"off", "hit"} {
		env := adjudicationFixture(t)
		rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
		before := evidenceTestRoundTrip(t, *env.Review)
		calls := 0
		run := func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
			calls++
			return adjudicationResponse{Text: adjudicationJSON(t, adjudicationReplyFixture(env)), Cache: cache, NativeUsage: &shadowUsage{Tokens: 17, Cost: 0.01}}, nil
		}
		path := filepath.Join(t.TempDir(), "adjudication-shadow.json")
		off, err := recordAdjudicationShadow(false, rr, defaultAdjudicationLimits(), env, run, path, "")
		if err != nil || off != nil || calls != 0 {
			t.Fatal("ordinary shadow call", off, err)
		}
		a, err := recordAdjudicationShadow(true, rr, defaultAdjudicationLimits(), env, run, path, "owner later confirmed reuse")
		if err != nil || a.Error != "" || calls != 1 {
			t.Fatal(a, err)
		}
		if !reflect.DeepEqual(before, *env.Review) {
			t.Fatal("record-only experiment changed production")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"experiment_budget", "excerpt", "responses", "usage", "later_outcome", "native_decisions_usage", "total_tokens"} {
			if !strings.Contains(string(raw), field) {
				t.Fatal("missing experiment audit", field)
			}
		}
		applyAdjudication(env.Review, *a)
		results = append(results, ReduceReview(env.Subject, env.Review.Checks, env.Review.Answers, nil, env.Review.Policy))
	}
	if !reflect.DeepEqual(results[0], results[1]) || results[0].State != "pass" {
		t.Fatal("cache availability changed correctness", results)
	}
}
