package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

// backendReg is one registered backend.
type backendReg struct {
	name   string
	prefix string
	detect func(getenv func(string) string) bool
	ctor   func() (Backend, error)
}

var (
	backendMu   sync.RWMutex
	backendRegs []backendReg
)

// registerBackend adds a backend. Each backend file calls it from init, so
// registration order is file name order. The prefix is the backend's env
// prefix (for example "NAME_"); every other backend's Start strips it.
func registerBackend(name, envPrefix string, detect func(getenv func(string) string) bool, ctor func() (Backend, error)) {
	if name == "" || envPrefix == "" || detect == nil || ctor == nil {
		panic("registerBackend: name, prefix, detect and ctor are all required")
	}
	backendMu.Lock()
	defer backendMu.Unlock()
	for _, r := range backendRegs {
		if r.name == name {
			panic(fmt.Sprintf("registerBackend: %q is already registered", name))
		}
		if r.prefix == envPrefix {
			panic(fmt.Sprintf("registerBackend: prefix %q is already registered by %q", envPrefix, r.name))
		}
	}
	backendRegs = append(backendRegs, backendReg{name, envPrefix, detect, ctor})
}

func registeredBackends() []backendReg {
	backendMu.RLock()
	defer backendMu.RUnlock()
	return slices.Clone(backendRegs)
}

func registeredBackendNames() []string {
	var names []string
	for _, r := range registeredBackends() {
		names = append(names, r.name)
	}
	return names
}

func registeredPrefixes() []string {
	var ps []string
	for _, r := range registeredBackends() {
		ps = append(ps, r.prefix)
	}
	return ps
}

// envGetter reads a KEY=VALUE slice (os.Environ form) like os.Getenv.
func envGetter(env []string) func(string) string {
	return func(key string) string {
		for _, kv := range slices.Backward(env) {
			if k, v, ok := strings.Cut(kv, "="); ok && k == key {
				return v
			}
		}
		return ""
	}
}

// noBackendError is the one early-failure message: it names the env that was
// seen and the registered backends.
func noBackendError(env []string) error {
	var seen []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		for _, p := range registeredPrefixes() {
			if strings.HasPrefix(k, p) {
				seen = append(seen, k)
				break
			}
		}
	}
	sort.Strings(seen)
	detected := "none"
	if len(seen) > 0 {
		detected = strings.Join(seen, ", ")
	}
	names := "none"
	if n := registeredBackendNames(); len(n) > 0 {
		names = strings.Join(n, ", ")
	}
	return fail("no multiplexer backend found: detected env: %s; registered backends: %s; run inside one or set ORCH_BACKEND=<name>", detected, names)
}

// selectBackend picks the backend: ORCH_BACKEND wins, otherwise the first
// registered backend whose detect matches, otherwise the early failure. cfg
// carries backend.<name>.native.
func selectBackend(env []string, cfg backendConfig) (*Active, error) {
	getenv := envGetter(env)
	var chosen *backendReg
	regs := registeredBackends()
	if want := getenv("ORCH_BACKEND"); want != "" {
		for i := range regs {
			if regs[i].name == want {
				chosen = &regs[i]
			}
		}
		if chosen == nil {
			return nil, fail("ORCH_BACKEND=%s is not a registered backend; registered backends: %s", want, strings.Join(registeredBackendNames(), ", "))
		}
	} else {
		for i := range regs {
			if regs[i].detect(getenv) {
				chosen = &regs[i]
				break
			}
		}
		if chosen == nil {
			return nil, noBackendError(env)
		}
	}
	b, err := chosen.ctor()
	if err != nil {
		return nil, err
	}
	if err := checkDeclared(b); err != nil {
		return nil, err
	}
	native, set := cfg[chosen.name]
	return &Active{Backend: b, native: native, nativeSet: set}, nil
}

// backendFreeCommands never touch a backend.
var backendFreeCommands = []string{"plan", "verify", "gate", "bench", "lsp", "version"}

func commandNeedsBackend(cmd string) bool { return !slices.Contains(backendFreeCommands, cmd) }

// requireBackend is what a command calls before using a backend. For a command
// that needs none it returns nil without selecting anything.
func requireBackend(cmd string, env []string, cfg backendConfig) (*Active, error) {
	if !commandNeedsBackend(cmd) {
		return nil, nil
	}
	return selectBackend(env, cfg)
}

// backendConfig is the parsed backend.<name>.native lists. A name present with
// an empty list forces every fallback; a name absent leaves every declared
// capability native.
type backendConfig map[string][]string

// parseBackendConfig reads the decoded `backend` table: backend.<name>.native
// is a list of capability names. Unknown names and shapes fail.
func parseBackendConfig(raw map[string]any) (backendConfig, error) {
	cfg := backendConfig{}
	for name, v := range raw {
		tbl, ok := v.(map[string]any)
		if !ok {
			return nil, fail("backend.%s must be a table with a native list", name)
		}
		for k, nv := range tbl {
			if k != "native" {
				return nil, fail("unknown key %q", "backend."+name+"."+k)
			}
			var list []string
			switch l := nv.(type) {
			case []string:
				list = l
			case []any:
				for _, e := range l {
					s, ok := e.(string)
					if !ok {
						return nil, fail("%s must be a list of capability names", backendNativeKey(name))
					}
					list = append(list, s)
				}
			default:
				return nil, fail("%s must be a list of capability names", backendNativeKey(name))
			}
			for _, c := range list {
				if _, ok := lookupCapability(c); !ok {
					return nil, fail("%s: unknown capability %q; known: %s", backendNativeKey(name), c, strings.Join(capabilityNames(), ", "))
				}
			}
			if list == nil {
				list = []string{}
			}
			cfg[name] = list
		}
	}
	return cfg, nil
}
