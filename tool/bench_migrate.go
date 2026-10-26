package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Migration is a proposal, never an overlay implicitly consumed by reports.
// In particular it cannot turn unbound historical evidence into current approval.
type benchMigration struct {
	Schema            string                     `json:"schema_version"`
	OriginalSHA256    string                     `json:"original_run_sha256"`
	Basis             string                     `json:"basis"`
	Run               map[string]json.RawMessage `json:"proposed_run"`
	SnapshotAdditions map[string]json.RawMessage `json:"snapshot_additions,omitempty"`
}

func cmdBenchMigrate(a *app, args []string) error {
	fs := a.flags("bench migrate")
	suite := fs.String("suite", "", "suite to propose migration for (default all)")
	run := fs.String("run", "", "one run to reconstruct or correct without rewriting it")
	options := benchRecordFlags(fs)
	pos, e := parseArgs(fs, args)
	if e != nil {
		return e
	}
	if e := exactArgs(pos, 0, "bench migrate [--suite S --run ID] [--commit SHA --commit-basis user|note --commit-evidence TEXT]"); e != nil {
		return e
	}
	opts := options()
	if e := benchRecordOptsCheck(opts); e != nil {
		return e
	}
	if *run != "" && *suite == "" {
		return fail("--run requires --suite")
	}
	if opts.Commit != "" && *run == "" {
		return fail("a historical --commit must target exactly one --run")
	}
	if *suite != "" {
		if e := benchSuiteName(*suite); e != nil {
			return e
		}
	}
	if *run != "" {
		if e := benchName(*run); e != nil {
			return e
		}
	}
	// Build the complete proposal in a private copy. Nothing is published if
	// reconstruction or validation fails. No import recipe is executed.
	additions := map[string][]byte{}
	outputs := []string{}
	e = benchIsolated(func(source string) error {
		storePath := filepath.Join(benchDir(), "store.json")
		if benchExists(storePath) {
			var store map[string]any
			if e := benchReadJSON(storePath, &store); e != nil {
				return e
			}
			version, _ := store["schema_version"].(float64)
			if version != 1 && version != 2 {
				return fail("unsupported store schema %v", version)
			}
			if version == 1 {
				store["schema_version"] = 2
				b, e := json.MarshalIndent(store, "", "  ")
				if e != nil {
					return e
				}
				b = append(b, '\n')
				additions[filepath.Join("migrations", "store-"+benchHash(b)+".json")] = b
				// Only the isolated copy needs this for reconstruction helpers.
				if e := benchJSON(storePath, store); e != nil {
					return e
				}
			}
		}
		suites := []string{*suite}
		if *suite == "" {
			var e error
			suites, e = benchSuiteDirs()
			if e != nil {
				return e
			}
		}
		for _, name := range suites {
			if e := benchSuiteName(name); e != nil {
				return e
			}
			runs, e := benchRuns(name)
			if e != nil {
				return e
			}
			selected := *run == ""
			for _, r := range runs {
				if *run != "" && r.ID != *run && r.Label != *run {
					continue
				}
				selected = true
				if r.SchemaVersion >= 2 && opts == (benchRecordOpts{}) {
					outputs = append(outputs, fmt.Sprintf("migrate: %s/%s already schema 2; original unchanged", name, r.ID))
					continue
				}
				snap := filepath.Join(r.dir, "snapshot")
				var rec gateRecord
				if e := benchReadJSON(filepath.Join(snap, "gate.json"), &rec); e != nil {
					return e
				}
				originalBytes, e := os.ReadFile(filepath.Join(r.dir, "run.json"))
				if e != nil {
					return e
				}
				proposal := benchMigration{Schema: "bench-migration/v1", OriginalSHA256: benchHash(originalBytes), Basis: "proposal only; original identity and scores unchanged; no current approval or reproducibility inferred; reconstructed registration/history times use archived run created_at as a deterministic anchor, not a correction or evaluation date"}
				if e := json.Unmarshal(originalBytes, &proposal.Run); e != nil {
					return e
				}
				old := r.Subject
				oldBlobs, oldWall, oldPrompts, oldHistory := r.Blobs, r.WallSeconds, r.PromptsStored, r.SubjectHistory
				// Registration timestamps are derived from the archived import timestamp,
				// not today's time. Repeated proposals must be byte-identical.
				fixed := *a
				fixed.now = func() time.Time { return r.CreatedAt }
				if e := benchEnrichRun(&fixed, r, &rec, snap, snap, opts); e != nil {
					return e
				}
				if old != "" {
					r.Blobs, r.WallSeconds, r.PromptsStored, r.SubjectHistory = oldBlobs, oldWall, oldPrompts, oldHistory
				}
				if old != "" && old != r.Subject {
					r.SubjectHistory = append(r.SubjectHistory, benchSubjectMove{At: r.CreatedAt, From: old, To: r.Subject, Basis: opts.CommitBasis, Evidence: opts.CommitEvidence})
				}
				r.SchemaVersion = 2
				b, e := json.Marshal(r)
				if e != nil {
					return e
				}
				var updated map[string]json.RawMessage
				if e := json.Unmarshal(b, &updated); e != nil {
					return e
				}
				for _, field := range []string{"schema_version", "subject", "itemset", "blobs", "prompts_stored", "wall_seconds", "subject_history", "warnings", "inputs"} {
					if value, ok := updated[field]; ok {
						proposal.Run[field] = value
					}
				}
				if e := benchVerifyRun(r, snap); e != nil {
					return e
				}
				// Reconstruction can add subject.json to the copy. Keep this as proposal
				// data rather than adding it to the original snapshot.
				subjectPath := filepath.Join(name, "runs", r.ID, "snapshot", "subject.json")
				if !benchExists(filepath.Join(source, subjectPath)) {
					if b, e := os.ReadFile(filepath.Join(benchDir(), subjectPath)); e == nil {
						proposal.SnapshotAdditions = map[string]json.RawMessage{"subject.json": b}
					} else if !os.IsNotExist(e) {
						return e
					}
				}
				b, e = json.MarshalIndent(proposal, "", "  ")
				if e != nil {
					return e
				}
				b = append(b, '\n')
				path := filepath.Join(name, "runs", r.ID, "migrations", benchHash(b)+".json")
				additions[path] = b
				outputs = append(outputs, "migrate: "+name+"/"+r.ID+" proposal "+filepath.ToSlash(path)+"; original unchanged")
			}
			if !selected {
				return fail("run %q not found", *run)
			}
		}
		// Publish only newly registered objects. Existing registration metadata is
		// never replaced; snapshot/run/store/readme writes stay in the private copy.
		for _, prefix := range []string{"objects", "subjects"} {
			root := filepath.Join(benchDir(), prefix)
			e := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
				if os.IsNotExist(e) && path == root {
					return nil
				}
				if e != nil {
					return e
				}
				if d.IsDir() {
					return nil
				}
				rel, e := filepath.Rel(benchDir(), path)
				if e != nil {
					return e
				}
				b, e := os.ReadFile(path)
				if e != nil {
					return e
				}
				existing, e := os.ReadFile(filepath.Join(source, rel))
				if e == nil {
					if string(existing) != string(b) {
						return fail("migration requires rewriting existing object %s; stopped", rel)
					}
					return nil
				}
				if !os.IsNotExist(e) {
					return e
				}
				additions[rel] = b
				return nil
			})
			if e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return e
	}
	// Use immutable publication for every addition, including repeat runs.
	for _, rel := range benchSortedKeys(additions) {
		if strings.HasPrefix(rel, "..") {
			return fail("invalid migration artifact path")
		}
		if e := benchImmutable(filepath.Join(benchDir(), rel), additions[rel]); e != nil {
			return e
		}
	}
	for _, line := range outputs {
		fmt.Fprintln(a.out, line)
	}
	return nil
}
