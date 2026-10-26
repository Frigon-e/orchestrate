package main

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// experimentManifest is a pre-registration record, not an experiment runner or
// permission to change defaults. Spend is integer millionths of Currency.
type experimentManifest struct {
	SchemaVersion     int                 `json:"schema_version"`
	Baseline          string              `json:"baseline"`
	HeldOutCases      []experimentCase    `json:"held_out_cases"`
	TrialCount        int                 `json:"trial_count"`
	MaximumSpend      *experimentSpend    `json:"maximum_spend"`
	PromotionCriteria *experimentCriteria `json:"promotion_criteria"`
	ModelRoutes       []experimentRoute   `json:"model_routes"`
	CacheSettings     *experimentCache    `json:"cache_settings"`
	Mode              string              `json:"mode"` // shadow or live
	Paid              bool                `json:"paid"`
}

type experimentCase struct {
	ID       string `json:"id"`
	Subject  string `json:"subject"` // immutable subject/answer-key identifier
	Category string `json:"category"`
}
type experimentSpend struct {
	Currency string `json:"currency"`
	Micros   *int64 `json:"micros"`
}
type experimentRoute struct {
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type experimentCache struct {
	Mode     string `json:"mode"`     // cold, warm or both
	Settings string `json:"settings"` // provider settings, including disabled caching
}

// Pointers distinguish explicitly declared zero thresholds from missing fields.
// Rates are fractions in [0,1]; cost and latency are positive per-decision limits.
type experimentCriteria struct {
	DeterministicFalsePasses *int     `json:"deterministic_false_passes"`
	MaxFalseApprovalRate     *float64 `json:"max_false_approval_rate"`
	MaxFalseBlockerRate      *float64 `json:"max_false_blocker_rate"`
	MinCoverage              *float64 `json:"min_coverage"`
	MinSupportQuality        *float64 `json:"min_support_quality"`
	MaxCostMicros            *int64   `json:"max_cost_micros"`
	MaxLatencyMillis         *int64   `json:"max_latency_millis"`
}

// Validate refuses incomplete pre-registration even for shadow/unpaid trials.
// It validates declarations only, not their truth or satisfaction by results.
func (m experimentManifest) Validate() error {
	var errs []error
	require := func(ok bool, field string) {
		if !ok {
			errs = append(errs, fmt.Errorf("experiment manifest: invalid or missing %s", field))
		}
	}
	require(m.SchemaVersion == 1, "schema_version (must be 1)")
	require(strings.TrimSpace(m.Baseline) != "", "baseline")
	require(m.TrialCount > 0, "trial_count")
	require(m.Mode == "shadow" || m.Mode == "live", "mode")
	if m.MaximumSpend == nil {
		require(false, "maximum_spend")
	} else {
		require(len(m.MaximumSpend.Currency) == 3 && strings.Trim(m.MaximumSpend.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "", "maximum_spend.currency")
		require(m.MaximumSpend.Micros != nil && *m.MaximumSpend.Micros >= 0 && (!m.Paid || *m.MaximumSpend.Micros > 0), "maximum_spend.micros")
	}
	categories := map[string]bool{"contradictory_judgments": false, "insufficient_evidence": false, "authority_boundaries": false, "provider_failures": false}
	seen := map[string]bool{}
	for _, c := range m.HeldOutCases {
		require(strings.TrimSpace(c.ID) != "" && !seen[c.ID], "held_out_cases.id (unique)")
		seen[c.ID] = true
		require(strings.TrimSpace(c.Subject) != "", "held_out_cases.subject")
		if _, ok := categories[c.Category]; ok {
			categories[c.Category] = true
		} else {
			require(false, "held_out_cases.category")
		}
	}
	for _, category := range []string{"contradictory_judgments", "insufficient_evidence", "authority_boundaries", "provider_failures"} {
		require(categories[category], "held_out_cases."+category)
	}
	require(len(m.ModelRoutes) > 0, "model_routes")
	routes := map[string]bool{}
	for _, r := range m.ModelRoutes {
		require(strings.TrimSpace(r.Role) != "" && strings.TrimSpace(r.Provider) != "" && strings.TrimSpace(r.Model) != "", "model_routes role/provider/model")
		key := r.Role + "\x00" + r.Provider + "\x00" + r.Model
		require(!routes[key], "model_routes (unique)")
		routes[key] = true
	}
	if m.CacheSettings == nil {
		require(false, "cache_settings")
	} else {
		require(m.CacheSettings.Mode == "cold" || m.CacheSettings.Mode == "warm" || m.CacheSettings.Mode == "both", "cache_settings.mode")
		require(strings.TrimSpace(m.CacheSettings.Settings) != "", "cache_settings.settings")
	}
	if c := m.PromotionCriteria; c == nil {
		require(false, "promotion_criteria")
	} else {
		require(c.DeterministicFalsePasses != nil && *c.DeterministicFalsePasses == 0, "promotion_criteria.deterministic_false_passes (must be zero)")
		for _, r := range []struct {
			name  string
			value *float64
		}{{"max_false_approval_rate", c.MaxFalseApprovalRate}, {"max_false_blocker_rate", c.MaxFalseBlockerRate}, {"min_coverage", c.MinCoverage}, {"min_support_quality", c.MinSupportQuality}} {
			require(r.value != nil && !math.IsNaN(*r.value) && !math.IsInf(*r.value, 0) && *r.value >= 0 && *r.value <= 1, "promotion_criteria."+r.name)
		}
		require(c.MaxCostMicros != nil && *c.MaxCostMicros > 0, "promotion_criteria.max_cost_micros")
		require(c.MaxLatencyMillis != nil && *c.MaxLatencyMillis > 0, "promotion_criteria.max_latency_millis")
	}
	return errors.Join(errs...)
}

// ValidateRunnable is the same fail-closed preflight for all modes, including a
// live paid record. There is deliberately no execution or promotion mechanism.
func (m experimentManifest) ValidateRunnable() error { return m.Validate() }
