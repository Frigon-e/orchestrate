package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func decisionServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("ORCH_TRIAGE_URL", srv.URL)
	t.Setenv("ORCH_TRIAGE_KEY", "k")
}

var decisionTestProfile = profile{Harness: "api", Provider: "openrouter", Model: "m"}

func TestDecisionRawKeepsAbsentFieldsAbsent(t *testing.T) {
	decisionServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"served","answers":{"q":{"type":"scalar"}},"usage":{"cost":0.5,"total_tokens":9}}`))
	})
	raw, model, u, err := postDecisionsRawWithContext(context.Background(), decisionTestProfile, nil, nil)
	if err != nil || model != "served" || u.Cost != 0.5 || u.Tokens != 9 {
		t.Fatalf("%v %v %+v", err, model, u)
	}
	if _, ok := numField(raw["q"], "noul"); ok {
		t.Fatal("absent noul must stay absent in the raw answer")
	}
	ans, _, _, err := postDecisionsWithContext(context.Background(), decisionTestProfile, nil, nil)
	if err != nil || ans["q"].Type != "scalar" {
		t.Fatalf("wrapper behaviour changed: %v %+v", err, ans)
	}
}

func TestDecisionHTTPErrorAndTimeoutReasons(t *testing.T) {
	s := decisionTestSite(t, true)
	decisionServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	})
	if _, err := askDecision(context.Background(), s, decisionReq("net")); err == nil {
		t.Fatal("HTTP 500 must fail")
	}
	decisionServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := askDecision(ctx, s, decisionReq("net")); err == nil {
		t.Fatal("timeout must fail")
	}
	recs := readDecisions(t, s, "net")
	if len(recs) != 2 || recs[0].Reason != "http_500" || recs[1].Reason != "timeout" {
		t.Fatalf("%+v", recs)
	}
}
