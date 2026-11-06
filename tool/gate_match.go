package main

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// The shadow matcher: for each new gate finding it asks the decision helper
// which open ledger item the finding restates, and records the answer on the
// item. The answer changes no ledger state; only the list hint shows it. The
// author's first close of a matched item is the outcome that grades it.

const (
	gateMatchUse           = "gate_match"
	gateMatchMaxCandidates = 12
	gateMatchCriterionCap  = 600
)

// The limits are variables so tests can shorten them.
var (
	gateMatchMaxFindings = 40
	gateMatchCallTimeout = 20 * time.Second
	gateMatchRunTimeout  = 90 * time.Second
)

func init() {
	gateLedgerMatch = gateMatchAsk
	gateLedgerOutcome = gateMatchOutcome
	registerDecisionMetric(gateMatchUse, decisionMetric{Name: "restates", Key: "restates", Field: "noul", Outcome: "duplicate"})
	registerDecisionMetric(gateMatchUse, decisionMetric{Name: "confirmed-hinted", Key: "restates", Field: "noul", Outcome: "confirmed_hinted"})
	registerDecisionMetric(gateMatchUse, decisionMetric{Name: "confirmed-unhinted", Key: "restates", Field: "noul", Outcome: "confirmed_unhinted"})
}

func ledgerLastSeen(it ledgerItem) int64 {
	var at int64
	for _, sg := range it.Seen {
		at = max(at, sg.At)
	}
	return at
}

// gateMatchCandidates orders the items for one fresh finding: its own check
// first, then the latest sighting, then the lower number.
func gateMatchCandidates(items []ledgerItem, check string) []ledgerItem {
	out := slices.Clone(items)
	num := func(id string) int {
		var n int
		fmt.Sscanf(id, "L%d", &n)
		return n
	}
	slices.SortStableFunc(out, func(a, b ledgerItem) int {
		if as, bs := a.Check == check, b.Check == check; as != bs {
			if as {
				return -1
			}
			return 1
		}
		if la, lb := ledgerLastSeen(a), ledgerLastSeen(b); la != lb {
			if la > lb {
				return -1
			}
			return 1
		}
		return num(a.ID) - num(b.ID)
	})
	if len(out) > gateMatchMaxCandidates {
		out = out[:gateMatchMaxCandidates]
	}
	return out
}

func gateMatchAsk(ctx context.Context, s site, rel string, candidates []ledgerItem, fresh []ledgerItem) map[string]ledgerMatch {
	runCtx, cancel := context.WithTimeout(ctx, gateMatchRunTimeout)
	defer cancel()
	runAt := time.Now().Unix()
	answers := map[string]ledgerMatch{}
	for i, f := range fresh {
		if i >= gateMatchMaxFindings || runCtx.Err() != nil {
			break
		}
		if len(f.Seen) == 0 {
			continue
		}
		cands := gateMatchCandidates(candidates, f.Check)
		ids := make([]string, len(cands))
		which := map[string]any{"none": "the finding restates none of the listed items"}
		for j, c := range cands {
			ids[j] = c.ID
			which[c.ID] = short(fmt.Sprintf("%s: %s (evidence: %s)", c.Where, c.Claim, c.Evidence), gateMatchCriterionCap)
		}
		questions := map[string]any{
			"restates": map[string]any{"type": "noul",
				"instructions": `Does the new finding report the same problem as one of the ledger items listed under "which", so that one fix would settle both?`,
				"criteria": map[string]any{
					"true":  "the same problem, even if worded differently or citing other lines",
					"false": "a different problem, or a related one that needs its own fix",
				}},
			"which": map[string]any{"type": "choice",
				"instructions": "Which ledger item does the new finding restate? Pick none when it restates no item.",
				"criteria":     which},
		}
		callCtx, callCancel := context.WithTimeout(runCtx, gateMatchCallTimeout)
		res, err := askDecision(callCtx, s, decisionRequest{
			Use: gateMatchUse, Role: gateMatchUse, Subject: rel,
			State:     map[string]any{"where": f.Where, "claim": f.Claim, "evidence": f.Evidence},
			Questions: questions,
			Keys:      []string{"restates", "which"},
			Fields:    map[string][]string{"restates": {"noul"}, "which": {"choice", "probabilities"}},
			Meta: map[string]any{"spec_rel": rel, "gate_hash": f.Seen[0].Hash, "run_at": runAt,
				"finding": f.Seen[0].Finding, "candidates": ids},
		})
		callCancel()
		if err != nil {
			continue // fail open: the item is created as with no matcher
		}
		w := res.Answers["which"]
		answers[f.Seen[0].Finding] = ledgerMatch{
			Decision:   res.ID,
			Choice:     w.Choice,
			P:          w.Probabilities[w.Choice],
			Restates:   res.Answers["restates"].Noul >= 0.5,
			Candidates: ids,
		}
	}
	return answers
}

// ledgerRoot follows `of` links until an item that is not a duplicate.
func ledgerRoot(led gateLedger, id string) string {
	for range led.Items {
		var it *ledgerItem
		for i := range led.Items {
			if led.Items[i].ID == id {
				it = &led.Items[i]
				break
			}
		}
		if it == nil || it.State != "duplicate" || it.Of == "" {
			return id
		}
		id = it.Of
	}
	return id
}

func gateMatchOutcome(root string, led gateLedger, it ledgerItem, state, of string) error {
	m := it.Match
	if m == nil || m.Decision == "" {
		return nil
	}
	dup := state == "duplicate"
	correct := m.Choice == "none"
	if dup {
		correct = of != "" && ledgerRoot(led, m.Choice) == of
	}
	hint := m.HintAt != 0
	confirmed := dup && correct
	out := map[string]any{
		"duplicate":      dup,
		"of":             of,
		"state":          state,
		"choice":         m.Choice,
		"of_offered":     of != "" && slices.Contains(m.Candidates, of),
		"choice_correct": correct,
		"hint":           hint,
		"confirmed":      confirmed,
	}
	if hint {
		out["confirmed_hinted"] = confirmed
	} else {
		out["confirmed_unhinted"] = confirmed
	}
	return recordDecisionOutcome(root, gateMatchUse, m.Decision, out)
}
