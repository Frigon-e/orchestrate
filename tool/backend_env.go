package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
)

// bridgeSelfEnv gives this process the env a backend could not pass at launch.
// It does nothing unless the backend has identity.env. It sets only keys that
// are unset (never overwrites), skips every registered backend prefix, and
// never fails: an error is reported to warn (nil to stay silent) and ignored.
func bridgeSelfEnv(ctx context.Context, a *Active, lookup func(string) (string, bool), set func(k, v string) error, warn io.Writer) {
	if a == nil || !a.Has(CapIdentityEnv) {
		return
	}
	se, ok := a.Backend.(SelfEnver)
	if !ok {
		return
	}
	env, err := se.SelfEnv(ctx)
	if err != nil {
		if warn != nil {
			fmt.Fprintf(warn, "orch: identity env: %v\n", err)
		}
		return
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" || strings.Contains(k, "=") || foreignEnvKey("", k) {
			continue
		}
		if _, exists := lookup(k); exists {
			continue
		}
		if err := set(k, env[k]); err != nil && warn != nil {
			fmt.Fprintf(warn, "orch: identity env %s: %v\n", k, err)
		}
	}
}

// foreignEnvKey says key carries a registered backend prefix other than own's
// (own "" means any).
func foreignEnvKey(own, key string) bool {
	for _, r := range registeredBackends() {
		if r.name != own && strings.HasPrefix(key, r.prefix) {
			return true
		}
	}
	return false
}

// childEnv builds the environment for an agent that backend own starts: base
// (os.Environ form) plus extra, with every registered backend env prefix
// stripped except own's. A lead started from another multiplexer's terminal
// therefore does not inherit its variables and report to it through its hooks.
// extra wins over base for the same key.
func childEnv(own string, base []string, extra map[string]string) []string {
	var keep []string
	for _, r := range registeredBackends() {
		if r.name != own {
			keep = append(keep, r.prefix) // prefixes to strip
		}
	}
	foreign := func(key string) bool {
		for _, p := range keep {
			if strings.HasPrefix(key, p) {
				return true
			}
		}
		return false
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if foreign(k) {
			continue
		}
		if _, over := extra[k]; over {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		if !foreign(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}
