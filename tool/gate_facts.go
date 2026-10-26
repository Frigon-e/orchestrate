package main

import (
	"os/exec"
	"strings"
	"sync"
)

// The gate's use of the symbol facts of the retrieval layer: the question
// cards get the facts of the symbols their questions name.

// addSymbolFacts fills the Facts of each item from gopls, one lookup per
// distinct symbol, a few at a time. It returns the number of cards that got
// facts and a note when gopls is missing or a call failed.
func addSymbolFacts(items []enrichedQuestion, cacheDir, code string) (int, string) {
	per := make([][]string, len(items))
	distinct := map[string]bool{}
	var order []string
	for i, it := range items {
		per[i] = goSymbols(it.Q.Question)
		for _, s := range per[i] {
			if !distinct[s] {
				distinct[s] = true
				order = append(order, s)
			}
		}
	}
	if len(order) == 0 {
		return 0, ""
	}
	g := &goplsRunner{bin: goplsBin(), dir: code, cacheDir: cacheDir}
	if _, err := exec.LookPath(g.bin); err != nil {
		return 0, "no Go symbol facts: " + g.bin + " was not found"
	}
	facts := map[string]*symFact{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, goplsParallel)
	for _, s := range order {
		wg.Add(1)
		sem <- struct{}{}
		go func(s string) {
			defer wg.Done()
			defer func() { <-sem }()
			f, err := g.symbolFacts(s)
			if err == nil {
				mu.Lock()
				facts[s] = &f
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	n := 0
	for i := range items {
		for _, s := range per[i] {
			if f := facts[s]; f != nil {
				items[i].Facts = append(items[i].Facts, *f)
			}
		}
		if len(items[i].Facts) > 0 {
			n++
		}
	}
	return n, g.note
}

// addTSFacts adds TypeScript symbol facts to the items. It returns the number
// of cards that got any (counted by the caller with the Go ones) and a note
// when no TypeScript server is available or a request failed.
func addTSFacts(items []enrichedQuestion, cacheDir, code string, sp *spec) (int, string) {
	per := make([][]tsJob, len(items))
	need := map[tsJob]bool{}
	var order []tsJob
	roots := map[string]int{}
	owns := map[string][]string{}
	for _, p := range sp.packages {
		owns[p.id] = p.owns
	}
	for i := range items {
		skip := map[string]bool{}
		for _, f := range items[i].Facts {
			if len(f.Defs) > 0 {
				skip[f.Symbol[strings.LastIndex(f.Symbol, ".")+1:]] = true
			}
		}
		syms := tsSymbols(items[i].Q.Question, skip)
		if len(syms) == 0 {
			continue
		}
		root := tsProjectRoot(code, items[i].Q.Question, owns[items[i].Q.Package])
		if root == "" {
			continue
		}
		roots[root]++
		for _, s := range syms {
			j := tsJob{root, s}
			per[i] = append(per[i], j)
			if !need[j] {
				need[j] = true
				order = append(order, j)
			}
		}
	}
	if len(order) == 0 {
		return 0, ""
	}
	facts, note := tsFactsFor(order, cacheDir, code, roots)
	n := 0
	for i := range items {
		got := false
		for _, j := range per[i] {
			if f := facts[j]; f != nil {
				if len(f.Defs) > 0 {
					items[i].Facts = dropGoMiss(items[i].Facts, f.Symbol)
				}
				items[i].Facts = append(items[i].Facts, *f)
				got = true
			}
		}
		if got && !hasGoFacts(items[i]) {
			n++
		}
	}
	return n, note
}

func hasGoFacts(it enrichedQuestion) bool {
	for _, f := range it.Facts {
		if f.Lang != "typescript" {
			return true
		}
	}
	return false
}
