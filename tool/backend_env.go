package main

import (
	"sort"
	"strings"
)

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
