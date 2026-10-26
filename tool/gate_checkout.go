package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

func checkoutChanges(root string, before, after checkoutStamp) []string {
	paths := map[string]bool{}
	for path, value := range before.files {
		if after.files[path] != value {
			paths[path] = true
		}
	}
	for path, value := range after.files {
		if before.files[path] != value {
			paths[path] = true
		}
	}
	if before.head != after.head {
		names, err := checkoutGit(root, "diff", "--name-only", "-z", strings.TrimSpace(before.head), strings.TrimSpace(after.head))
		if err == nil {
			for _, path := range strings.Split(names, "\x00") {
				if path != "" {
					paths[path] = true
				}
			}
		}
	}
	if len(paths) == 0 && (before.status != after.status || before.diff != after.diff || before.head != after.head) {
		paths["(index or HEAD)"] = true
	}
	var out []string
	for path := range paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

type checkoutGuard struct {
	root          string
	before        checkoutStamp
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	flight        map[string]bool
	files, passes map[string]bool
	failure       string
}

func newCheckoutGuard(root string) (*checkoutGuard, error) {
	before, err := checkoutFingerprint(root)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &checkoutGuard{root: root, before: before, ctx: ctx, cancel: cancel, flight: map[string]bool{}, files: map[string]bool{}, passes: map[string]bool{}}, nil
}

func (g *checkoutGuard) run(name string, run func() (string, string, error)) (string, string, error) {
	g.mu.Lock()
	if g.ctx.Err() != nil {
		g.mu.Unlock()
		return "", "", g.ctx.Err()
	}
	g.flight[name] = true
	g.mu.Unlock()
	out, hit, err := run()
	g.mu.Lock()
	defer g.mu.Unlock()
	after, checkErr := checkoutFingerprint(g.root)
	var changed []string
	if checkErr == nil {
		changed = checkoutChanges(g.root, g.before, after)
	}
	if checkErr != nil || len(changed) > 0 {
		for _, path := range changed {
			g.files[path] = true
		}
		for pass := range g.flight {
			g.passes[pass] = true
		}
		if checkErr != nil {
			g.failure = checkErr.Error()
		}
		g.cancel()
	}
	delete(g.flight, name)
	if g.ctx.Err() != nil {
		return "", hit, fmt.Errorf("checkout changed or could not be checked; pass discarded")
	}
	return out, hit, err
}

func (g *checkoutGuard) finish(rec *gateRecord) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Recheck even when no pass detected a change: the interval after the last
	// pass is also part of the review.
	if g.ctx.Err() == nil {
		after, err := checkoutFingerprint(g.root)
		if err != nil {
			g.failure = err.Error()
			g.cancel()
		} else if changed := checkoutChanges(g.root, g.before, after); len(changed) > 0 {
			for _, path := range changed {
				g.files[path] = true
			}
			g.cancel()
		}
	}
	if g.ctx.Err() == nil {
		return
	}
	rec.TreeChanged = nil
	rec.TreeChangedPasses = nil
	for path := range g.files {
		rec.TreeChanged = append(rec.TreeChanged, path)
	}
	for pass := range g.passes {
		rec.TreeChangedPasses = append(rec.TreeChangedPasses, pass)
	}
	sort.Strings(rec.TreeChanged)
	sort.Strings(rec.TreeChangedPasses)
	rec.TreeCheckError = g.failure
	// Capture facts only. ReduceReview owns state and keeps the useful review,
	// even when a changed subject or failed checkout check takes precedence.
	if rec.PassErrors == nil {
		rec.PassErrors = map[string]string{}
	}
	rec.PassErrors["all"] = "checkout changed or could not be checked; approval withheld; no files reverted"
}
