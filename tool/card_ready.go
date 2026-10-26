package main

import (
	"fmt"
	"regexp"
	"strings"
)

// cardReadiness is independent of route: readiness cannot grant decision authority.
// This is a structural check, not verification that the evidence supports a claim.
type cardReadiness struct {
	Status  string
	Missing []string
}

// checkCardReady accepts legacy cards without ready:. Recompute structural checks
// so a stale ready: ready cannot publish an incomplete card. Explicit research
// blockers are conservative holds, not a substitute for checking the structure.
// No retrieval or model calls take place here.
func checkCardReady(src string) cardReadiness {
	c, err := parseCard(src)
	if err != nil {
		return cardReadiness{"unclear", []string{err.Error()}}
	}
	return c.readiness(src)
}

func (c *card) readiness(src string) cardReadiness {
	if failures := cardRetrievalFailures(src); len(failures) > 0 {
		return cardReadiness{"failed_retrieval", failures}
	}
	if c.ready == "failed_retrieval" {
		return cardReadiness{"failed_retrieval", []string{"resolve the recorded retrieval failure and update ready:"}}
	}
	if !cardContent(c.context) {
		return cardReadiness{"missing_context", []string{"add Context with the goal, checked state and constraints"}}
	}
	var tradeoffs []string
	if len(c.options) < 2 {
		tradeoffs = append(tradeoffs, "add at least two options with Gain: and Cost: effects")
	}
	for _, o := range c.options {
		if !cardEffect(o.body, "gain") {
			tradeoffs = append(tradeoffs, fmt.Sprintf("option %s needs a Gain: effect", o.id))
		}
		if !cardEffect(o.body, "cost") {
			tradeoffs = append(tradeoffs, fmt.Sprintf("option %s needs a Cost: effect", o.id))
		}
	}
	if len(tradeoffs) > 0 {
		return cardReadiness{"missing_tradeoffs", tradeoffs}
	}
	var evidence []string
	if c.recommend == "" {
		evidence = append(evidence, "add recommend: with an option id and explain its basis in that option")
	}
	if !cardContent(c.action) && !cardContent(c.owner) && !cardContent(c.next) {
		evidence = append(evidence, "add action:, owner:, or Next with the required next action")
	}
	if len(evidence) > 0 {
		return cardReadiness{"missing_evidence", evidence}
	}
	// Authors can record gaps that structural checks cannot discover. Clear them
	// explicitly after research, never by guessing an answer. A declared unclear
	// cannot make a parseable card unclear; hold it as an unresolved evidence gap.
	switch c.ready {
	case "missing_context", "missing_tradeoffs", "missing_evidence":
		return cardReadiness{c.ready, []string{"resolve the research gap recorded by ready: " + c.ready + " and update that field"}}
	case "unclear":
		return cardReadiness{"missing_evidence", []string{"resolve the preparation uncertainty recorded by ready: unclear and update that field"}}
	}
	return cardReadiness{Status: "ready"}
}

// Require an explicit effect, not just an option label, code sample or include.
// Accept common markdown emphasis and bullet markers around Gain:/Cost:.
func cardEffect(body, label string) bool {
	var ft fenceTracker
	for _, line := range strings.Split(body, "\n") {
		if ft.inside(line) {
			continue
		}
		line = strings.TrimLeft(strings.TrimSpace(line), "-*+ ")
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.Trim(key, " *_"), label) && cardContent(strings.TrimLeft(value, " *_")) {
			return true
		}
	}
	return false
}

func cardContent(s string) bool {
	s = strings.TrimSpace(s)
	switch strings.ToLower(strings.Trim(s, " .-*_:")) {
	case "", "tbd", "todo", "...":
		return false
	}
	return true
}

// These include A5's exact CodeReview failure/skipped forms. Ignore fenced code:
// a quoted test fixture is not an attached retrieval report. INDEX notes are
// surfaced separately, never treated as proof about every card in the folder.
var retrievalFailureRe = regexp.MustCompile(`(?i)(code search (failed|did not run|skipped)|search fail(ure|ed)|retrieval fail(ure|ed)|no code hints for some questions|(?:search|retrieval):\s*(failed|error|unavailable))`)

func cardRetrievalFailures(src string) []string {
	var failures []string
	var ft fenceTracker
	for _, line := range strings.Split(src, "\n") {
		if !ft.inside(line) && retrievalFailureRe.MatchString(line) {
			failures = append(failures, strings.TrimSpace(line))
		}
	}
	return failures
}

// INDEX.md is an overview, not an answerable card. Preserve its notes, warning
// sections and retrieval failure lines in the CLI warnings block, including
// continuation lines. Do not dump the ordinary card table into every ask result.
func cardIndexWarnings(src string) []string {
	var warnings []string
	var ft fenceTracker
	var paragraph []string
	inWarnings := false
	flush := func() {
		if len(paragraph) > 0 {
			warnings = append(warnings, strings.Join(paragraph, "\n"))
			paragraph = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		if ft.inside(line) {
			continue
		}
		text := strings.TrimSpace(line)
		lower := strings.ToLower(text)
		if strings.HasPrefix(text, "#") {
			flush()
			heading := strings.TrimSpace(strings.TrimLeft(lower, "#"))
			inWarnings = strings.Contains(heading, "warning") || heading == "notes" || heading == "retrieval failures"
			continue
		}
		if text == "" {
			flush()
			continue
		}
		if inWarnings || len(paragraph) > 0 || strings.HasPrefix(lower, "note:") || strings.HasPrefix(lower, "warning:") || retrievalFailureRe.MatchString(line) {
			paragraph = append(paragraph, text)
		}
	}
	flush()
	return warnings
}
