package main

import (
	"fmt"
	"io"
	"strings"
)

// Visibility, retrieval and support are different observations. Null delivery
// instrumentation is unknown, even if the checklist records a candidate.
type seenCount struct {
	Total            int `json:"total"`
	Seen             int `json:"seen"`
	Hint             int `json:"hint"`
	Retrieved        int `json:"retrieved"`
	Unknown          int `json:"unknown"`
	RetrievalUnknown int `json:"retrieval_unknown"`
}

type keyAgree struct {
	Agree       int    `json:"agree"`
	Both        int    `json:"both"`
	Items       int    `json:"items"`
	Answered    int    `json:"answered"`
	Valid       int    `json:"valid"`
	Supported   int    `json:"supported"`
	Missing     int    `json:"missing"`
	Invalid     int    `json:"invalid"`
	Unsupported int    `json:"unsupported"`
	Unmapped    int    `json:"unmapped"`
	SeenAgree   int    `json:"seen_agree"`
	SeenBoth    int    `json:"seen_both"`
	UnseenAgree int    `json:"unseen_agree"`
	UnseenBoth  int    `json:"unseen_both"`
	Unavailable string `json:"unavailable,omitempty"`
}

type keyView struct {
	Seen  map[string]seenCount `json:"seen"`
	Agree map[string]keyAgree  `json:"agree"`
}

type keyVisibility struct{ Retrieved, Hint, Rendered, Known, RetrievalKnown bool }

func (a *armData) visibility(id string, r refEntry) keyVisibility {
	v := keyVisibility{RetrievalKnown: a.checklist != nil}
	allKnown := len(r.targets()) > 0
	for _, target := range r.targets() {
		for _, rows := range a.checklist {
			for _, row := range rows {
				if row.Item.ID != id {
					continue
				}
				for _, c := range row.Cands {
					if sameLocation(c.at(), target) {
						v.Retrieved = true
					}
				}
			}
		}
		want := pointerRe.FindStringSubmatch(target)
		for _, item := range a.items {
			if item.ID != id {
				continue
			}
			for _, hint := range item.Reuse {
				got := pointerRe.FindStringSubmatch(hint)
				if got != nil && want != nil && got[1] == want[1] {
					v.Hint = true
				}
			}
		}
		// The checklist describes retrieval, not the bytes delivered. Only explicit
		// rendered source records whose span contains the target establish visibility.
		if !a.manifestKnown {
			allKnown = false
			continue
		}
		targetKnown := false
		for _, e := range a.evidence {
			if e.SourceKind != evidenceSourceCode || e.Rendered == nil || want == nil || e.Path != want[1] {
				continue
			}
			if !evidenceContainsTarget(e, target) {
				continue
			}
			targetKnown = true
			if *e.Rendered {
				v.Rendered = true
			}
		}
		allKnown = allKnown && targetKnown
	}
	v.Known = v.Rendered || allKnown
	return v
}

func (a *armData) keySeen(id string, r refEntry) (seen, viaHint bool) {
	v := a.visibility(id, r)
	return v.Rendered, v.Hint && !v.Rendered
}

func keyViewOf(a *armData, ref map[string]refEntry) keyView {
	v := keyView{Seen: map[string]seenCount{}, Agree: map[string]keyAgree{}}
	issues := strings.Join(referenceIssues(ref), "; ")
	for id, r := range ref {
		class := benchClass(r.Verdict)
		if class == "" {
			continue
		}
		visibility := a.visibility(id, r)
		hasTarget := len(r.targets()) > 0 && (class == "duplicate" || class == "variation_site")
		if hasTarget {
			c := v.Seen[class]
			c.Total++
			if visibility.Rendered {
				c.Seen++
			}
			if visibility.Hint {
				c.Hint++
			}
			if visibility.Retrieved {
				c.Retrieved++
			}
			if !visibility.Known {
				c.Unknown++
			}
			if !visibility.RetrievalKnown {
				c.RetrievalUnknown++
			}
			v.Seen[class] = c
		}
		for _, judge := range []string{"group", "decider", "flash"} {
			k := v.Agree[judge]
			k.Items++
			k.Unavailable = issues
			ans := a.answer(judge, id)
			switch {
			case r.Issue != "":
				k.Unmapped++
			case !ans.Present:
				k.Missing++
			case !ans.Valid:
				k.Answered++
				k.Invalid++
			case !ans.Supported:
				k.Answered++
				k.Valid++
				k.Unsupported++
			default:
				k.Answered++
				k.Valid++
				k.Supported++
				if issues == "" {
					k.Both++
					same := ans.Verdict == normVerdict(r.Verdict)
					if hasTarget && (ans.Verdict == verdictReuse || ans.Verdict == verdictExtend) {
						targetMatch := false
						for _, target := range r.targets() {
							if sameLocation(ans.Candidate, target) {
								targetMatch = true
							}
						}
						same = same && targetMatch
					}
					if same {
						k.Agree++
					}
					if hasTarget && visibility.Known {
						if visibility.Rendered {
							k.SeenBoth++
							if same {
								k.SeenAgree++
							}
						} else {
							k.UnseenBoth++
							if same {
								k.UnseenAgree++
							}
						}
					}
				}
			}
			v.Agree[judge] = k
		}
	}
	return v
}

func keyAgreeText(k keyAgree) string {
	if k.Items == 0 {
		return "answer coverage unavailable (no keyed behavior population)"
	}
	accuracy := fmt.Sprintf("%d/%d (%s)", k.Agree, k.Both, pct(k.Agree, k.Both))
	if k.Unavailable != "" {
		accuracy = "unavailable (identity mapping)"
	}
	return fmt.Sprintf("answer coverage %d/%d (%s) observed mapped answers; valid %d/%d; supported %d/%d; missing %d, invalid %d, unsupported %d, unmapped %d; conditional accuracy %s (supported mapped answers only)", k.Answered, k.Items, pct(k.Answered, k.Items), k.Valid, k.Items, k.Supported, k.Items, k.Missing, k.Invalid, k.Unsupported, k.Unmapped, accuracy)
}

func printKeyView(w io.Writer, arms []*armData, ref map[string]refEntry) {
	for i, a := range arms {
		refs := ref
		if a.reference != nil {
			refs = a.reference
		}
		v := keyViewOf(a, refs)
		label := string(rune('A' + i))
		for _, class := range []string{"duplicate", "variation_site"} {
			c := v.Seen[class]
			fmt.Fprintf(w, "visibility %s, arm %s: rendered %d/%d targets; retrieved %d/%d; author hint %d/%d; visibility unknown %d/%d; retrieval unknown %d/%d (all keyed targets; hints are not bodies)\n", class, label, c.Seen, c.Total, c.Retrieved, c.Total, c.Hint, c.Total, c.Unknown, c.Total, c.RetrievalUnknown, c.Total)
		}
		for _, j := range []string{"group", "decider", "flash"} {
			fmt.Fprintf(w, "agreement with the key, %s.%s: %s\n", label, j, keyAgreeText(v.Agree[j]))
		}
	}
}
