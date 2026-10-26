package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// historicalGateResult is a read-only compatibility boundary for stored gate
// records, bench archives and report prose. A historical label is NOT a v2
// state: in particular, "unverified" cannot distinguish error/stale/incomplete.
// New runs never populate it. Re-exports use an explicit historical label, never
// resurrect the removed status field or manufacture a historical review state.
type historicalGateResult struct {
	HistoricalStatus string `json:"historical_status,omitempty"`
}

func (r *gateRecord) UnmarshalJSON(data []byte) error {
	type current gateRecord
	var decoded struct {
		*current
		Status string `json:"status"`
	}
	*r = gateRecord{}
	decoded.current = (*current)(r)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if r.State == "" && r.HistoricalStatus == "" {
		r.HistoricalStatus = decoded.Status
	}
	return nil
}

func (r *benchRun) UnmarshalJSON(data []byte) error {
	type current benchRun
	var decoded struct {
		*current
		Status string `json:"status"`
	}
	// The archive loader assigns this private path before decoding.
	dir := r.dir
	*r = benchRun{dir: dir}
	decoded.current = (*current)(r)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if r.State == "" && r.HistoricalStatus == "" {
		r.HistoricalStatus = decoded.Status
	}
	return nil
}

// storedGateResultLabel is for history display and import completeness only.
// Approval always requires a recorded v2 state and a fresh reducer replay.
func storedGateResultLabel(r *gateRecord) string {
	if r.State != "" {
		return r.State
	}
	return r.HistoricalStatus
}

func retainHistoricalGateResult(run *benchRun, rec *gateRecord) {
	if rec.State == "" {
		run.historicalGateResult = rec.historicalGateResult
	}
}

func readStoredGateReport(report string) (string, historicalGateResult) {
	var historical historicalGateResult
	for _, line := range strings.Split(report, "\n") {
		if after, ok := strings.CutPrefix(line, "Review state: **"); ok {
			state, _, ok := strings.Cut(after, "**")
			if ok {
				return state, historicalGateResult{}
			}
		}
		if strings.HasPrefix(line, "Status:") && historical.HistoricalStatus == "" {
			// Keep the exact old prose value, including any formatting/version.
			historical.HistoricalStatus = strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
		}
	}
	return "", historical
}

// shadowRow is one judge's answer for one item, as old runs saved it under
// <gatedir>/shadow/<experiment or "run">/items.jsonl. New runs write none.
type shadowRow struct {
	Item       string             `json:"item"`
	Package    string             `json:"package"`
	Judge      string             `json:"judge"` // decider or flash
	Verdict    string             `json:"verdict,omitempty"`
	Answers    map[string]string  `json:"answers,omitempty"`
	Confidence map[string]float64 `json:"confidence,omitempty"`
	Best       string             `json:"best_candidate,omitempty"`
	At         string             `json:"best_at,omitempty"`
	How        string             `json:"how_implemented,omitempty"`
	Seconds    float64            `json:"seconds"`
	Tokens     int                `json:"tokens,omitempty"`
	Cost       float64            `json:"cost_usd,omitempty"`
	CostKnown  bool               `json:"cost_known,omitempty"`
	StateChars int                `json:"state_chars,omitempty"`
	Cut        string             `json:"cut,omitempty"`
	Model      string             `json:"model,omitempty"`
	Skipped    string             `json:"skipped,omitempty"`
	Error      string             `json:"error,omitempty"`
}

func shadowDir(gdir, name string) string {
	if name == "" {
		name = "run"
	}
	return filepath.Join(gdir, "shadow", name)
}

// reuseCand is one candidate location of an old run's reuse checklist.
type reuseCand struct {
	Path  string `json:"path"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

func (c reuseCand) at() string {
	if c.End > c.Start {
		return fmt.Sprintf("%s:%d-%d", c.Path, c.Start, c.End)
	}
	return fmt.Sprintf("%s:%d", c.Path, c.Start)
}

// itemCands is one item of an old run's checklist with the candidates found for it.
type itemCands struct {
	Item       gateItem
	Cands      []reuseCand
	Queries    []string
	QueriesRun int
}

// judgeVerdict maps shadow classes and legacy answers to internal verdicts.
func judgeVerdict(wd string) string {
	switch strings.ToLower(strings.TrimSpace(wd)) {
	case "duplicate", "yes", "reuse":
		return verdictReuse
	case "variation_site", "partly", "extend":
		return verdictExtend
	case "none", "no", "new":
		return verdictNew
	}
	return ""
}
