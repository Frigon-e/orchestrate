package main

import (
	"encoding/json"
	"maps"
	"math"
	"strings"
	"testing"
)

func validExperimentManifest() experimentManifest {
	return experimentManifest{SchemaVersion: 1, Baseline: "hybrid-v1@subject-key", TrialCount: 20, MaximumSpend: &experimentSpend{Currency: "USD", Micros: evidenceTestPtr(int64(5_000_000))}, Mode: "shadow", Paid: true,
		HeldOutCases: []experimentCase{{"contradiction", "subject-1", "contradictory_judgments"}, {"missing", "subject-2", "insufficient_evidence"}, {"owner", "subject-3", "authority_boundaries"}, {"outage", "subject-4", "provider_failures"}},
		ModelRoutes:  []experimentRoute{{Role: "reviewer", Provider: "fake", Model: "deterministic"}}, CacheSettings: &experimentCache{Mode: "both", Settings: "explicit provider cache settings; cold and warm isolated"},
		PromotionCriteria: &experimentCriteria{DeterministicFalsePasses: evidenceTestPtr(0), MaxFalseApprovalRate: evidenceTestPtr(0.0), MaxFalseBlockerRate: evidenceTestPtr(0.05), MinCoverage: evidenceTestPtr(0.95), MinSupportQuality: evidenceTestPtr(0.95), MaxCostMicros: evidenceTestPtr(int64(100_000)), MaxLatencyMillis: evidenceTestPtr(int64(30_000))},
	}
}
func TestManifestRequiredFields(t *testing.T) {
	b, err := json.Marshal(validExperimentManifest())
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err = json.Unmarshal(b, &object); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"shadow", "live"} {
		for _, field := range []string{"schema_version", "baseline", "held_out_cases", "trial_count", "maximum_spend", "promotion_criteria", "model_routes", "cache_settings", "mode"} {
			t.Run(mode+"/missing-"+field, func(t *testing.T) {
				copy := map[string]json.RawMessage{}
				maps.Copy(copy, object)
				copy["mode"], _ = json.Marshal(mode)
				delete(copy, field)
				raw, _ := json.Marshal(copy)
				var m experimentManifest
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				if err := m.ValidateRunnable(); err == nil {
					t.Fatalf("paid %s accepted missing %s", mode, field)
				}
			})
		}
	}
	for _, mode := range []string{"shadow", "live"} {
		m := validExperimentManifest()
		m.Mode = mode
		if err := m.ValidateRunnable(); err != nil {
			t.Fatal(err)
		}
		evidenceTestRoundTrip(t, m)
	}
}
func TestManifestRejectsInvalidDeclarations(t *testing.T) {
	cases := []struct {
		name, field string
		mutate      func(*experimentManifest)
	}{
		{"future schema", "schema_version", func(m *experimentManifest) { m.SchemaVersion = 2 }},
		{"blank baseline", "baseline", func(m *experimentManifest) { m.Baseline = "  " }},
		{"zero trials", "trial_count", func(m *experimentManifest) { m.TrialCount = 0 }},
		{"negative spend", "maximum_spend", func(m *experimentManifest) { m.MaximumSpend.Micros = evidenceTestPtr(int64(-1)) }},
		{"paid without budget", "maximum_spend", func(m *experimentManifest) { m.MaximumSpend.Micros = evidenceTestPtr(int64(0)) }},
		{"omitted unpaid cap", "maximum_spend.micros", func(m *experimentManifest) { m.Paid = false; m.MaximumSpend.Micros = nil }},
		{"missing blocker threshold", "max_false_blocker_rate", func(m *experimentManifest) { m.PromotionCriteria.MaxFalseBlockerRate = nil }},
		{"missing coverage", "min_coverage", func(m *experimentManifest) { m.PromotionCriteria.MinCoverage = nil }},
		{"missing support quality", "min_support_quality", func(m *experimentManifest) { m.PromotionCriteria.MinSupportQuality = nil }},
		{"missing currency", "currency", func(m *experimentManifest) { m.MaximumSpend.Currency = "" }},
		{"missing held-out category", "held_out_cases", func(m *experimentManifest) { m.HeldOutCases = m.HeldOutCases[:3] }},
		{"duplicate case", "held_out_cases.id", func(m *experimentManifest) { m.HeldOutCases[1].ID = m.HeldOutCases[0].ID }},
		{"unbound subject", "held_out_cases.subject", func(m *experimentManifest) { m.HeldOutCases[0].Subject = "" }},
		{"empty route", "model_routes", func(m *experimentManifest) { m.ModelRoutes[0].Model = "" }},
		{"duplicate route", "model_routes", func(m *experimentManifest) { m.ModelRoutes = append(m.ModelRoutes, m.ModelRoutes[0]) }},
		{"unspecified cache", "cache_settings", func(m *experimentManifest) { m.CacheSettings.Settings = "" }},
		{"invalid cache mode", "cache_settings", func(m *experimentManifest) { m.CacheSettings.Mode = "auto" }},
		{"invalid mode", "mode", func(m *experimentManifest) { m.Mode = "production" }},
		{"trust failure allowed", "deterministic_false_passes", func(m *experimentManifest) { m.PromotionCriteria.DeterministicFalsePasses = evidenceTestPtr(1) }},
		{"undeclared zero trust", "deterministic_false_passes", func(m *experimentManifest) { m.PromotionCriteria.DeterministicFalsePasses = nil }},
		{"missing approval rate", "max_false_approval_rate", func(m *experimentManifest) { m.PromotionCriteria.MaxFalseApprovalRate = nil }},
		{"negative blocker rate", "max_false_blocker_rate", func(m *experimentManifest) { m.PromotionCriteria.MaxFalseBlockerRate = evidenceTestPtr(-0.1) }},
		{"coverage over one", "min_coverage", func(m *experimentManifest) { m.PromotionCriteria.MinCoverage = evidenceTestPtr(1.1) }},
		{"nan support", "min_support_quality", func(m *experimentManifest) { m.PromotionCriteria.MinSupportQuality = evidenceTestPtr(math.NaN()) }},
		{"infinite approval", "max_false_approval_rate", func(m *experimentManifest) { m.PromotionCriteria.MaxFalseApprovalRate = evidenceTestPtr(math.Inf(1)) }},
		{"missing decision cost", "max_cost_micros", func(m *experimentManifest) { m.PromotionCriteria.MaxCostMicros = nil }},
		{"missing latency", "max_latency_millis", func(m *experimentManifest) { m.PromotionCriteria.MaxLatencyMillis = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validExperimentManifest()
			m.Mode = "live"
			tc.mutate(&m)
			err := m.ValidateRunnable()
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("wanted %s refusal, got %v", tc.field, err)
			}
		})
	}
	m := validExperimentManifest()
	m.Paid = false
	m.MaximumSpend.Micros = evidenceTestPtr(int64(0))
	if err := m.Validate(); err != nil {
		t.Fatalf("explicit zero unpaid budget should be valid: %v", err)
	}
}
