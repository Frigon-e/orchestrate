package main

import (
	"strings"
	"sync"
)

// Task briefs written by `orch new` and `orch plan` get a "Facts gathered by
// code" section from the retrieval layer's brief builder. It is a convenience:
// it never fails the command, is skipped with a note when the tools are missing
// and is left out with --no-ctx. A brief is not rewritten once a lead runs.

const taskBriefChars = 5000

var (
	taskCtxMu   sync.Mutex
	taskCtxEnvs = map[string]*ctxEnv{} // one env per repo and process: the index is checked once
)

func sharedCtxEnv(s site) *ctxEnv {
	taskCtxMu.Lock()
	defer taskCtxMu.Unlock()
	if e := taskCtxEnvs[s.repo]; e != nil {
		return e
	}
	e := newCtxEnv(s.repo, ctxOpts{Cfg: s.cfg})
	taskCtxEnvs[s.repo] = e
	return e
}

// taskFacts is the body of the section, or "" when the brief has nothing to
// brief (the placeholders of a fresh scaffold are not a task).
func taskFacts(s site, o taskOpts) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	body := strings.TrimSpace(o.fill["Spec"])
	if body == "" || strings.Contains(body, "{{") {
		return ""
	}
	e := sharedCtxEnv(s)
	if rel, _, ok := strings.Cut(o.spec, "#"); ok && rel != "" {
		taskCtxMu.Lock()
		e.Skip[rel] = true
		taskCtxMu.Unlock()
	}
	text := o.fill["Intent"] + "\n" + o.fill["Pointers"] + "\n" + body
	b := e.Briefs([]briefInput{{ID: o.id, Title: briefTitle(o.fill["Intent"]), Text: text, Body: body, Owns: o.owns}})[0]
	var sb strings.Builder
	sb.WriteString("Computed by code when this brief was written (`orch ctx brief`); exact for that moment. Check against the code before relying on it, and use `orch ctx` for more.\n\n")
	sb.WriteString(b.Render("", "task", taskBriefChars))
	for _, n := range e.Notes() {
		sb.WriteString("note: ")
		sb.WriteString(n)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
