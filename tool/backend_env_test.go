package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// envBackend is a fakeBackend that also serves identity.env.
type envBackend struct {
	*fakeBackend
	env map[string]string
	err error
}

func (e *envBackend) SelfEnv(context.Context) (map[string]string, error) { return e.env, e.err }

func newEnvBackend(env map[string]string, err error, declare ...string) *Active {
	return &Active{Backend: &envBackend{fakeBackend: newFakeBackend("envmux", declare...), env: env, err: err}}
}

// recorder is a fake process environment.
type recorder struct{ vars map[string]string }

func (r *recorder) lookup(k string) (string, bool) { v, ok := r.vars[k]; return v, ok }
func (r *recorder) set(k, v string) error          { r.vars[k] = v; return nil }

func TestBridgeSelfEnvSetsOnlyUnsetKeys(t *testing.T) {
	r := &recorder{vars: map[string]string{"ORCH_ROLE": "orchestrator"}}
	a := newEnvBackend(map[string]string{"ORCH_ROLE": "lead", "ORCH_TASK_DIR": "/t", "ORCH_EPOCH": ""}, nil, CapIdentityEnv)
	bridgeSelfEnv(context.Background(), a, r.lookup, r.set, nil)
	if r.vars["ORCH_ROLE"] != "orchestrator" {
		t.Errorf("existing ORCH_ROLE overwritten: %q", r.vars["ORCH_ROLE"])
	}
	if r.vars["ORCH_TASK_DIR"] != "/t" {
		t.Errorf("unset ORCH_TASK_DIR not set: %v", r.vars)
	}
	if v, ok := r.vars["ORCH_EPOCH"]; !ok || v != "" {
		t.Errorf("unset key with empty value must still be set: %v", r.vars)
	}
}

func TestBridgeSelfEnvSkipsForeignPrefixes(t *testing.T) {
	r := &recorder{vars: map[string]string{}}
	a := newEnvBackend(map[string]string{"ORCA_PANE_KEY": "x", "HERDR_PANE_ID": "y", "ORCH_ROLE": "lead", "A=B": "z"}, nil, CapIdentityEnv)
	bridgeSelfEnv(context.Background(), a, r.lookup, r.set, nil)
	if len(r.vars) != 1 || r.vars["ORCH_ROLE"] != "lead" {
		t.Errorf("only ORCH_ROLE may be set, got %v", r.vars)
	}
}

func TestBridgeSelfEnvToleratesError(t *testing.T) {
	r := &recorder{vars: map[string]string{}}
	a := newEnvBackend(map[string]string{"ORCH_ROLE": "lead"}, errors.New("boom"), CapIdentityEnv)
	var warn bytes.Buffer
	bridgeSelfEnv(context.Background(), a, r.lookup, r.set, &warn)
	if len(r.vars) != 0 {
		t.Errorf("a failed SelfEnv must set nothing: %v", r.vars)
	}
	if !strings.Contains(warn.String(), "boom") {
		t.Errorf("warning missing: %q", warn.String())
	}
	bridgeSelfEnv(context.Background(), a, r.lookup, r.set, nil) // nil warn must not panic
}

func TestBridgeSelfEnvNeedsTheCapability(t *testing.T) {
	r := &recorder{vars: map[string]string{}}
	// Implements SelfEnver but does not declare it: not native, so not used.
	bridgeSelfEnv(context.Background(), newEnvBackend(map[string]string{"ORCH_ROLE": "lead"}, nil), r.lookup, r.set, nil)
	// Declares nothing and implements nothing.
	bridgeSelfEnv(context.Background(), &Active{Backend: newFakeBackend("plain")}, r.lookup, r.set, nil)
	bridgeSelfEnv(context.Background(), nil, r.lookup, r.set, nil)
	// Config lists it out of the native set.
	a := newEnvBackend(map[string]string{"ORCH_ROLE": "lead"}, nil, CapIdentityEnv)
	a.nativeSet = true
	bridgeSelfEnv(context.Background(), a, r.lookup, r.set, nil)
	if len(r.vars) != 0 {
		t.Errorf("nothing may be set without the capability: %v", r.vars)
	}
}

func TestIdentityEnvCapabilityRegistry(t *testing.T) {
	def, ok := lookupCapability(CapIdentityEnv)
	if !ok || def.Contract == nil || def.FallbackDoc == "" {
		t.Fatalf("identity.env not registered: %+v", def)
	}
	a := newEnvBackend(nil, nil, CapIdentityEnv)
	if err := checkDeclared(a.Backend); err != nil {
		t.Errorf("a backend implementing SelfEnver may declare identity.env: %v", err)
	}
	if !a.Has(CapIdentityEnv) {
		t.Error("Has must be true for a declaring SelfEnver")
	}
	if err := checkDeclared(newFakeBackend("plain", CapIdentityEnv)); err == nil {
		t.Error("declaring identity.env without SelfEnv must fail")
	}
	// Fallback: no env, nil error.
	fb, native, ok := capabilityAs[SelfEnver](&Active{Backend: newFakeBackend("plain")}, CapIdentityEnv)
	if !ok || native {
		t.Fatalf("fallback missing: native=%v ok=%v", native, ok)
	}
	if env, err := fb.SelfEnv(context.Background()); env != nil || err != nil {
		t.Errorf("fallback = %v, %v", env, err)
	}
}
