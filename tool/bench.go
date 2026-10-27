package main

// Durable, machine-local benchmark snapshots. Gate artifacts are copied before
// publishing a run, so pruning the gate directory cannot change history.
import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type benchCost struct {
	USD     *float64  `json:"usd"`
	Basis   string    `json:"basis"`
	Parts   costParts `json:"parts"`
	Missing int       `json:"missing_requests,omitempty"`
}
type benchBuild struct {
	Version    string `json:"orch_version"`
	BinaryHash string `json:"binary_hash"`
	Dirty      *bool  `json:"dirty"`
}
type benchInputs struct {
	Repo        string `json:"repo"`
	Commit      string `json:"commit"`
	SpecPath    string `json:"spec_path"`
	SpecHash    string `json:"spec_hash"`
	SpecSHA256  string `json:"spec_sha256"`
	SpecVariant string `json:"spec_variant"`
}
type benchConfig struct {
	Roles        map[string]modelRole `json:"roles"`
	Runners      map[string]string    `json:"runners"`
	Dossier      string               `json:"dossier"`
	Cache        string               `json:"cache"`
	CtxTool      string               `json:"ctx_tool"`
	ContextFiles string               `json:"context_files"`
	Verdicts     *bool                `json:"verdicts"`
	Shadow       string               `json:"shadow"`
	Budgets      map[string]int       `json:"budgets"`
	Flags        []string             `json:"orch_flags"`
	Command      string               `json:"command"`
}
type benchGroup struct {
	Packages    []string        `json:"packages"`
	Seconds     *int            `json:"pass_seconds"`
	Requests    *int            `json:"requests"`
	Turns       *int            `json:"turns"`
	Tokens      map[string]*int `json:"tokens"`
	MaxContext  *int            `json:"max_context"`
	Cost        benchCost       `json:"cost"`
	Runner      string          `json:"runner"`
	Errors      []string        `json:"errors"`
	TreeChanged bool            `json:"tree_changed"`
	Exploration *sessionProfile `json:"exploration,omitempty"`
}
type benchFindings struct {
	BySource   map[string]int `json:"by_source"`
	Verified   int            `json:"verified"`
	Unverified int            `json:"unverified"`
	Blockers   int            `json:"blockers"`
	Concerns   int            `json:"concerns"`
	Reuse      int            `json:"reuse_findings"`
	Dropped    int            `json:"dropped"`
}
type benchItems struct {
	WithQueries        int `json:"with_queries"`
	QueriesRun         int `json:"queries_run"`
	CandidateLocations int `json:"candidate_locations"`
	BodiesShown        int `json:"bodies_shown"`
}
type benchMetric struct {
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Better string  `json:"better"`
}
type benchRun struct {
	historicalGateResult
	SchemaVersion  int                    `json:"schema_version"`
	ID             string                 `json:"id"`
	Label          string                 `json:"label"`
	Suite          string                 `json:"suite"`
	Arm            string                 `json:"arm"`
	CreatedAt      time.Time              `json:"created_at"`
	Build          benchBuild             `json:"build"`
	Inputs         benchInputs            `json:"inputs"`
	Config         benchConfig            `json:"config"`
	Groups         map[string]benchGroup  `json:"groups"`
	Findings       *benchFindings         `json:"findings"`
	Items          *benchItems            `json:"items"`
	Scores         map[string]benchScore  `json:"scores"`
	Raw            map[string]string      `json:"raw"`
	Metrics        map[string]benchMetric `json:"metrics"`
	SourceHash     string                 `json:"source_hash"`
	ArmDefinition  map[string]any         `json:"arm_definition"`
	Subject        string                 `json:"subject,omitempty"`
	Itemset        string                 `json:"itemset,omitempty"`
	KeySubjects    map[string]string      `json:"key_subjects,omitempty"`
	Blobs          map[string][]string    `json:"blobs,omitempty"`
	PromptsStored  *bool                  `json:"prompts_stored,omitempty"`
	WallSeconds    benchWall              `json:"wall_seconds"`
	SubjectHistory []benchSubjectMove     `json:"subject_history,omitempty"`
	Kind           string                 `json:"kind,omitempty"`
	State          string                 `json:"state,omitempty"`
	Note           string                 `json:"note,omitempty"`
	Warnings       []string               `json:"warnings,omitempty"`
	SnapshotHash   string                 `json:"snapshot_hash,omitempty"`
	dir            string
}
type benchSuite struct {
	Repo         string                    `toml:"repo"`
	SpecPath     string                    `toml:"spec_path"`
	BaseCommit   string                    `toml:"base_commit"`
	Groups       []string                  `toml:"groups"`
	ReferenceKey string                    `toml:"reference_key"`
	Arms         map[string]map[string]any `toml:"arms"`
	Subject      benchSuiteSubject         `toml:"subject"`
	Provenance   map[string]string         `toml:"provenance,omitempty"`
	crossSubject bool
	totalAll     bool
}

func benchDir() string {
	if s := os.Getenv("ORCH_BENCH_DIR"); s != "" {
		return expandHome(s)
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		h, _ := os.UserHomeDir()
		base = filepath.Join(h, ".local", "share")
	}
	return filepath.Join(base, "orchestrate", "bench")
}
func benchHash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func benchSuiteLoad(name string) (benchSuite, error) {
	var s benchSuite
	if e := benchSuiteName(name); e != nil {
		return s, e
	}
	path := filepath.Join(benchDir(), name, "suite.toml")
	if e := benchSafe(path); e != nil {
		return s, e
	}
	_, e := toml.DecodeFile(path, &s)
	if e != nil {
		return s, e
	}
	return s, benchSuiteCheck(s)
}
func benchSuiteWrite(name string, s benchSuite) error {
	if e := benchSuiteName(name); e != nil {
		return e
	}
	if e := benchSuiteCheck(s); e != nil {
		return e
	}
	var b strings.Builder
	if e := toml.NewEncoder(&b).Encode(s); e != nil {
		return e
	}
	path := filepath.Join(benchDir(), name, "suite.toml")
	if e := benchSafe(path); e != nil {
		return e
	}
	data := []byte(b.String())
	old, e := os.ReadFile(path)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if e == nil {
		if string(old) == string(data) {
			return nil
		}
		// A new run may suggest another arm/default, but it cannot replace
		// a historical suite definition. Reports never apply this proposal.
		path = filepath.Join(benchDir(), name, "reports", "suite-proposals", benchHash(data)+".toml")
	}
	return benchImmutable(path, data)
}
func benchBuildNow() benchBuild {
	b := benchBuild{Version: "unknown"}
	if exe, e := os.Executable(); e == nil {
		if data, e := os.ReadFile(exe); e == nil {
			b.BinaryHash = benchHash(data)
		}
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		b.Version = info.Main.Version
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				b.Version = s.Value
			}
			if s.Key == "vcs.modified" {
				v := s.Value == "true"
				b.Dirty = &v
			}
		}
	}
	return b
}
func benchRunLoad(suite, id string) (*benchRun, error) {
	if e := benchName(id); e != nil {
		return nil, e
	}
	runs, e := benchRuns(suite)
	if e != nil {
		return nil, e
	}
	var found *benchRun
	for _, r := range runs {
		if r.ID == id || r.Label == id {
			if found != nil {
				return nil, fail("ambiguous run %q; use an id", id)
			}
			found = r
		}
	}
	if found == nil {
		return nil, fail("run %q not found in suite %s", id, suite)
	}
	return found, nil
}
func benchRuns(suite string) ([]*benchRun, error) {
	if e := benchSuiteName(suite); e != nil {
		return nil, e
	}
	if e := benchSafe(filepath.Join(benchDir(), suite, "runs")); e != nil {
		return nil, e
	}
	entries, e := os.ReadDir(filepath.Join(benchDir(), suite, "runs"))
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var runs []*benchRun
	for _, d := range entries {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		r := &benchRun{dir: filepath.Join(benchDir(), suite, "runs", d.Name())}
		if e := benchReadJSON(filepath.Join(r.dir, "run.json"), r); e != nil {
			return nil, e
		}
		if e := benchRunValidate(r, suite, d.Name()); e != nil {
			return nil, e
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].CreatedAt.Before(runs[j].CreatedAt)
	})
	return runs, nil
}
func benchSource(a *app, source string) (string, error) {
	p := expandHome(source)
	if !filepath.IsAbs(p) {
		p = filepath.Join(a.cwd, p)
	}
	if filepath.Base(p) == "gate.json" {
		p = filepath.Dir(p)
	}
	if benchExists(filepath.Join(p, "gate.json")) || benchExists(filepath.Join(p, "report.md")) {
		return p, nil
	}
	root, e := repoRoot(a.cwd)
	if e != nil {
		return "", e
	}
	if e := benchName(source); e != nil {
		return "", e
	}
	files, _ := filepath.Glob(filepath.Join(runRoot(root), "gate", "*", "experiments", source, "gate.json"))
	if len(files) != 1 {
		return "", fail("benchmark source %q: expected a directory with gate.json or a unique experiment name", source)
	}
	return filepath.Dir(files[0]), nil
}
func benchCopy(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fail("refusing symlink in benchmark snapshot: %s", p)
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return benchMkdir(target)
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		return benchImmutable(target, b)
	})
}

// benchRecord is used by the command and the gate completion hook. Unknown
// historical fields stay null; never substitute today's configuration/build.
func benchRecord(a *app, source, suite, label, arm string) (*benchRun, error) {
	return benchRecordWith(a, source, suite, label, arm, benchRecordOpts{})
}
func benchRecordWith(a *app, source, suite, label, arm string, opts benchRecordOpts) (*benchRun, error) {
	if suite == "" {
		return nil, fail("--suite is required (or configure bench_suite in the user config)")
	}
	if e := benchRecordOptsCheck(opts); e != nil {
		return nil, e
	}
	if e := benchSuiteName(suite); e != nil {
		return nil, e
	}
	for _, s := range []string{suite, label, arm} {
		if s != "" {
			if e := benchName(s); e != nil {
				return nil, e
			}
		}
	}
	dir, e := benchSource(a, source)
	if e != nil {
		return nil, e
	}
	data, e := os.ReadFile(filepath.Join(dir, "gate.json"))
	if os.IsNotExist(e) {
		return benchRecordReportOnly(a, dir, suite, label, arm, opts)
	}
	if e != nil {
		return nil, e
	}
	var rec gateRecord
	if e = json.Unmarshal(data, &rec); e != nil {
		return nil, e
	}
	actualKind := "gate-run"
	if rec.Experiment != "" {
		actualKind = "experiment"
	}
	if opts.Kind != "" && opts.Kind != actualKind {
		return nil, fail("source is %s, not %s", actualKind, opts.Kind)
	}
	if rec.Hash == "" {
		var c benchCapture
		b, _ := os.ReadFile(filepath.Join(dir, "subject.json"))
		_ = json.Unmarshal(b, &c)
		if c.Subject.ID != "" {
			rec.Hash = specHash([]byte(c.Text))
		} else if opts.SpecText != "" {
			b, err := os.ReadFile(expandHome(opts.SpecText))
			if err != nil {
				return nil, err
			}
			rec.Hash = specHash(b)
		} else {
			return nil, fail("gate record has no spec hash; supply --spec-text")
		}
	}
	if storedGateResultLabel(&rec) == "" || rec.At == 0 {
		return nil, fail("source is not a finished gate run")
	}
	if label == "" {
		label = orDefault(rec.Experiment, "run")
	}
	if arm == "" {
		arm = label
	}
	if e := benchName(label); e != nil {
		return nil, e
	}
	if e := benchName(arm); e != nil {
		return nil, e
	}
	root := filepath.Join(benchDir(), suite)
	if e = benchMkdir(root); e != nil {
		return nil, e
	}
	var result *benchRun
	if e := benchSafe(filepath.Join(root, ".lock")); e != nil {
		return nil, e
	}
	e = benchWithLock(filepath.Join(root, ".lock"), func() error {
		s, e := benchSuiteLoad(suite)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		var raw struct {
			PassTokens map[string]map[string]json.RawMessage `json:"pass_tokens"`
		}
		_ = json.Unmarshal(data, &raw)
		hash := benchHash(data)
		snapshotHash, e := benchSourceIdentity(dir, opts)
		if e != nil {
			return e
		}
		runs, e := benchRuns(suite)
		if e != nil {
			return e
		}
		for _, r := range runs {
			if r.SourceHash == hash && r.SnapshotHash == snapshotHash && r.Label == label && r.Arm == arm {
				result = r
				return nil
			}
		}
		at := time.Unix(rec.At, 0).UTC()
		id := at.Format("20060102-1504") + "-" + benchPrefix(label, 50)
		base := id
		for n := 2; benchExists(filepath.Join(root, "runs", id)); n++ {
			suffix := "-" + strconv.Itoa(n)
			id = benchPrefix(base, 64-len(suffix)) + suffix
		}
		r := &benchRun{SchemaVersion: 2, SnapshotHash: snapshotHash, ID: id, Label: label, Suite: suite, Arm: arm, CreatedAt: at, Build: rec.BenchBuild, Inputs: rec.BenchInputs, Config: rec.BenchConfig, Groups: map[string]benchGroup{}, Scores: map[string]benchScore{}, Raw: map[string]string{"gate": filepath.Join(dir, "gate.json"), "sessions": filepath.Join(dir, "sessions"), "dossiers": filepath.Join(dir, "dossier")}, Metrics: map[string]benchMetric{}, SourceHash: hash}
		r.Kind, r.State = "gate-run", rec.State
		retainHistoricalGateResult(r, &rec)
		if rec.Experiment != "" {
			r.Kind = "experiment"
		}
		if r.Build.Version == "" {
			r.Build.Version = "unknown"
		}
		if r.Inputs.SpecPath == "" {
			r.Inputs.SpecPath = rec.Spec
		}
		r.Inputs.SpecHash = rec.Hash
		if r.Inputs.Repo == "" {
			r.Inputs.Repo = benchSourceRepo(dir)
		}
		r.Config.Runners = rec.PassRunners
		r.Config.Cache = rec.Cache
		r.Config.CtxTool = orDefault(rec.LookupTool, rec.CtxTool)
		r.Config.ContextFiles = rec.ContextFiles
		names := map[string]bool{}
		for k := range rec.PassSeconds {
			names[k] = true
		}
		for k := range rec.PassTokens {
			names[k] = true
		}
		for k := range rec.Groups {
			names[k] = true
		}
		for k := range rec.PassErrors {
			names[k] = true
		}
		for n := range names {
			g := benchGroup{Packages: rec.Groups[n], Runner: rec.PassRunners[n], Tokens: map[string]*int{}, Cost: benchCost{Basis: "none"}}
			if v, ok := rec.PassSeconds[n]; ok {
				g.Seconds = &v
			}
			t := rec.PassTokens[n]
			fields := raw.PassTokens[n]
			ptr := func(k string, v int) *int {
				if _, ok := fields[k]; ok {
					return &v
				}
				return nil
			}
			g.Requests = ptr("requests", t.Requests)
			g.Turns = ptr("turns", t.Turns)
			g.MaxContext = ptr("max_context", t.MaxContext)
			for k, v := range map[string]int{"input": t.Input, "output": t.Output, "cache_read": t.CacheRead, "cache_write": t.CacheWrite} {
				g.Tokens[k] = ptr(k, v)
			}
			if _, ok := fields["cost_usd"]; ok {
				g.Cost = benchCost{USD: &t.Cost, Basis: "reported", Parts: t.CostParts, Missing: t.NoCost}
				if t.NoCost >= t.Requests && t.Cost == 0 {
					g.Cost.USD = nil
					g.Cost.Basis = "none"
				}
			}
			if msg := rec.PassErrors[n]; msg != "" {
				g.Errors = []string{msg}
			}
			for _, p := range rec.TreeChangedPasses {
				if p == n {
					g.TreeChanged = true
				}
			}
			if p, ok := profileSession(filepath.Join(dir, "sessions", passFile(n)), r.Inputs.SpecPath); ok {
				g.Exploration = &p
			}
			r.Groups[n] = g
		}
		f := benchFindings{BySource: map[string]int{}, Dropped: rec.Dropped}
		for _, v := range rec.Findings {
			f.BySource[v.Source]++
			if v.Verified {
				f.Verified++
			} else {
				f.Unverified++
			}
			if v.Severity == "blocker" {
				f.Blockers++
			}
			if v.Severity == "concern" {
				f.Concerns++
			}
			if v.Source == "reuse" {
				f.Reuse++
			}
		}
		r.Findings = &f
		gdir := dir
		if rec.Experiment != "" {
			gdir = filepath.Dir(filepath.Dir(dir))
		}
		ad := loadArm(gdir, rec.Experiment, dir, &rec)
		benchCaptureShadowCosts(r, ad.rows)
		if ad.stats != "" {
			r.Items = &benchItems{}
			_, e = fmt.Sscanf(ad.stats, "items with queries %d; queries run %d; distinct candidate locations %d; bodies shown %d", &r.Items.WithQueries, &r.Items.QueriesRun, &r.Items.CandidateLocations, &r.Items.BodiesShown)
			if e != nil {
				return e
			}
		}
		if s.Arms == nil {
			s.Arms = map[string]map[string]any{}
		}
		def := map[string]any{}
		b, _ := json.Marshal(r.Config)
		_ = json.Unmarshal(b, &def)
		for k, v := range def {
			if v == nil {
				delete(def, k)
			}
		}
		def["spec_variant"] = r.Inputs.SpecVariant
		if _, ok := s.Arms[arm]; ok {
			r.ArmDefinition = def
		} else {
			s.Arms[arm] = def
			r.ArmDefinition = def
		}
		if s.Repo == "" {
			s.Repo = r.Inputs.Repo
			s.SpecPath = r.Inputs.SpecPath
			s.BaseCommit = r.Inputs.Commit
		}
		if len(s.Groups) == 0 {
			for n := range rec.Groups {
				s.Groups = append(s.Groups, n)
			}
			sort.Strings(s.Groups)
		}
		if e := benchMkdir(filepath.Join(root, "runs")); e != nil {
			return e
		}
		tmp, e := os.MkdirTemp(filepath.Join(root, "runs"), ".record-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(tmp)
		snap := filepath.Join(tmp, "snapshot")
		if e := benchMkdir(snap); e != nil {
			return e
		}
		// Copy only required artifacts, not recursive experiments or live sessions.
		for _, name := range []string{"gate.json", "items.json", "checklist.json", "dossier", "report.md", "subject.json"} {
			src := filepath.Join(dir, name)
			if !benchExists(src) {
				continue
			}
			info, e := os.Lstat(src)
			if e != nil {
				return e
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fail("refusing symlink in benchmark snapshot: %s", src)
			}
			if info.IsDir() {
				if e := benchCopy(src, filepath.Join(snap, name)); e != nil {
					return e
				}
			} else {
				b, e := os.ReadFile(src)
				if e != nil {
					return e
				}
				if e := benchWrite(filepath.Join(snap, name), b); e != nil {
					return e
				}
			}
		}
		// Normalise shadow placement so loadArm reads the snapshot without the source.
		rows := ad.rows
		if len(rows) > 0 {
			var b strings.Builder
			for _, row := range rows {
				v, _ := json.Marshal(row)
				b.Write(v)
				b.WriteByte('\n')
			}
			if e := benchWrite(filepath.Join(tmp, "shadow.jsonl"), []byte(b.String())); e != nil {
				return e
			}
			sd := shadowDir(snap, rec.Experiment)
			if e := benchMkdir(sd); e != nil {
				return e
			}
			if e := benchWrite(filepath.Join(sd, "items.jsonl"), []byte(b.String())); e != nil {
				return e
			}
		}
		if e := benchEnrichRun(a, r, &rec, dir, snap, opts); e != nil {
			return e
		}
		if s.Subject.Default == "" {
			s.Subject.Default = r.Subject
		}
		if e := benchSuiteWrite(suite, s); e != nil {
			return e
		}
		keys, e := benchKeys(suite)
		if e != nil {
			return e
		}
		if len(keys) > 0 {
			k := keys[len(keys)-1]
			r.dir = tmp
			score, e := benchRescore(r, k)
			if e != nil {
				return e
			}
			r.Scores[k.ID] = score
			r.KeySubjects = map[string]string{k.ID: k.Subject}
		}
		if e := benchJSON(filepath.Join(tmp, "run.json"), r); e != nil {
			return e
		}
		dest := filepath.Join(root, "runs", id)
		if e := benchSafe(dest); e != nil {
			return e
		}
		if e := benchVerifyRun(r, snap); e != nil {
			return e
		}
		if e := os.Rename(tmp, dest); e != nil {
			return e
		}
		r.dir = dest
		result = r
		return nil
	})
	if e != nil {
		return nil, e
	}
	if e := benchReadme(); e != nil {
		return nil, e
	}
	return result, nil
}

// A key keeps the complete original entries as well as normalized scoring fields.
type benchKeyItem struct {
	Revision   string          `json:"revision,omitempty"`
	Candidates []string        `json:"candidates,omitempty"`
	ID         string          `json:"id"`
	Package    string          `json:"package"`
	Item       string          `json:"item"`
	Class      string          `json:"class"`
	Candidate  string          `json:"candidate"`
	Original   json.RawMessage `json:"original"`
}
type benchKey struct {
	Format         string         `json:"format"`
	ID             string         `json:"id"`
	CreatedAt      time.Time      `json:"created_at"`
	Note           string         `json:"note"`
	FromReference  bool           `json:"from_reference"`
	PromptHash     string         `json:"prompt_hash"`
	Items          []benchKeyItem `json:"items"`
	Counts         map[string]int `json:"counts"`
	Subject        string         `json:"subject"`
	Itemset        string         `json:"itemset"`
	Author         string         `json:"author"`
	Blind          string         `json:"blind"`
	BaseKey        string         `json:"base_key,omitempty"`
	PromptBlobs    []string       `json:"prompt_blobs,omitempty"`
	OriginalSHA256 string         `json:"original_sha256"`
}

func benchClass(v string) string {
	if normVerdict(v) == verdictUnclr {
		return "unknown"
	}
	switch judgeVerdict(v) {
	case verdictReuse:
		return "duplicate"
	case verdictExtend:
		return "variation_site"
	case verdictNew:
		return "none"
	case verdictUnclr:
		return "unknown"
	}
	return ""
}
func benchNormalize(data []byte) ([]benchKeyItem, error) {
	var rows []json.RawMessage
	if e := json.Unmarshal(data, &rows); e != nil {
		var obj struct {
			Items []json.RawMessage `json:"items"`
		}
		if e = json.Unmarshal(data, &obj); e != nil {
			return nil, e
		}
		rows = obj.Items
	}
	if len(rows) == 0 {
		return nil, fail("answer key has no items")
	}
	var out []benchKeyItem
	ids := map[string]bool{}
	for _, raw := range rows {
		var v struct {
			ID        string `json:"id"`
			ItemID    string `json:"item_id"`
			Package   string `json:"package"`
			Item      string `json:"item"`
			Seeks     string `json:"seeks"`
			Class     string `json:"class"`
			Verdict   string `json:"verdict"`
			Candidate string `json:"candidate"`
			Target    struct {
				Path   string          `json:"path"`
				Symbol string          `json:"symbol"`
				Lines  json.RawMessage `json:"lines"`
			} `json:"target"`
		}
		if e := json.Unmarshal(raw, &v); e != nil {
			return nil, e
		}
		it := benchKeyItem{ID: orDefault(v.ID, v.ItemID), Package: v.Package, Item: orDefault(v.Seeks, v.Item), Class: benchClass(orDefault(v.Class, v.Verdict)), Candidate: v.Candidate, Original: raw}
		if it.Class == "" {
			return nil, fail("key item %s has invalid class/verdict", it.ID)
		}
		if it.ID != "" && it.Package == "" {
			it.Package, _, _ = strings.Cut(it.ID, "#")
		}
		if it.ID == "" {
			it.ID = itemKey(it.Package, it.Item)
		}
		if ids[it.ID] {
			return nil, fail("duplicate key item %s", it.ID)
		}
		ids[it.ID] = true
		if v.Class != "" && it.Class != "none" {
			var lines string
			if json.Unmarshal(v.Target.Lines, &lines) == nil && lines != "" {
				if strings.Contains(lines, ":") {
					it.Candidate = lines
				} else {
					it.Candidate = v.Target.Path + ":" + lines
				}
			} else if v.Target.Symbol != "" {
				it.Candidate = v.Target.Path + "#" + v.Target.Symbol
			} else {
				it.Candidate = v.Target.Path
			}
		}
		out = append(out, it)
	}
	return out, nil
}
func benchKeys(suite string) ([]*benchKey, error) {
	if e := benchSuiteName(suite); e != nil {
		return nil, e
	}
	if e := benchSafe(filepath.Join(benchDir(), suite, "keys")); e != nil {
		return nil, e
	}
	files, e := filepath.Glob(filepath.Join(benchDir(), suite, "keys", "key-*.json"))
	if e != nil {
		return nil, e
	}
	var keys []*benchKey
	for _, f := range files {
		if strings.HasSuffix(f, ".original.json") {
			continue
		}
		k := &benchKey{}
		if e := benchReadJSON(f, k); e != nil {
			return nil, fmt.Errorf("answer key %s: %w", f, e)
		}
		if e := benchKeyName(k.ID); e != nil {
			return nil, e
		}
		if filepath.Base(f) != k.ID+".json" {
			return nil, fail("key identity differs from path: %s", f)
		}
		if k.Subject != "" {
			if e := benchName(k.Subject); e != nil {
				return nil, e
			}
		}
		if k.BaseKey != "" {
			if e := benchKeyName(k.BaseKey); e != nil {
				return nil, e
			}
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(keys[i].ID, "key-"))
		b, _ := strconv.Atoi(strings.TrimPrefix(keys[j].ID, "key-"))
		return a < b
	})
	return keys, nil
}
func benchKeyLoad(suite, n string) (*benchKey, error) {
	if n != "" {
		if !strings.HasPrefix(n, "key-") {
			n = "key-" + n
		}
		if e := benchKeyName(n); e != nil {
			return nil, e
		}
	}
	ks, e := benchKeys(suite)
	if e != nil {
		return nil, e
	}
	if n == "" && len(ks) > 0 {
		return ks[len(ks)-1], nil
	}
	for _, k := range ks {
		if k.ID == n || k.ID == "key-"+n {
			return k, nil
		}
	}
	return nil, fail("key %q not found in %s", n, suite)
}
func benchKeyAdd(a *app, suite, file, note, prompt string, from bool, options ...benchKeyOpts) (*benchKey, error) {
	var opts benchKeyOpts
	if len(options) > 0 {
		opts = options[0]
	}
	if e := benchSuiteName(suite); e != nil {
		return nil, e
	}
	data, e := os.ReadFile(expandHome(file))
	if e != nil {
		return nil, e
	}
	items, e := benchNormalize(data)
	if e != nil {
		return nil, e
	}
	dir := filepath.Join(benchDir(), suite, "keys")
	if e := benchMkdir(dir); e != nil {
		return nil, e
	}
	if e := benchSafe(filepath.Join(benchDir(), suite, ".lock")); e != nil {
		return nil, e
	}
	var result *benchKey
	e = benchWithLock(filepath.Join(benchDir(), suite, ".lock"), func() error {
		keys, e := benchKeys(suite)
		if e != nil {
			return e
		}
		subject, set, e := benchKeyBinding(a, suite, items, opts)
		if e != nil {
			return e
		}
		promptTexts := opts.PromptTexts
		if len(promptTexts) == 0 && prompt != "" {
			promptTexts = []string{prompt}
		}
		promptIDs := []string{}
		for _, text := range promptTexts {
			promptIDs = append(promptIDs, benchHash([]byte(text)))
		}
		n := 1
		for _, k := range keys {
			original := filepath.Join(dir, k.ID+".original.json")
			if e := benchSafe(original); e != nil {
				return e
			}
			old, e := os.ReadFile(original)
			if e != nil {
				return e
			}
			if string(old) == string(data) && k.Subject == subject && k.Itemset == set.ID && k.Author == orDefault(opts.Author, "unknown") && k.Blind == orDefault(opts.Blind, "unknown") && k.BaseKey == opts.BaseKey && k.Note == note && k.PromptHash == benchHash([]byte(prompt)) && strings.Join(k.PromptBlobs, "\x00") == strings.Join(promptIDs, "\x00") {
				result = k
				return nil
			}
			v, _ := strconv.Atoi(strings.TrimPrefix(k.ID, "key-"))
			n = max(n, v+1)
		}
		format := "reference-v1"
		for _, it := range items {
			var raw map[string]json.RawMessage
			_ = json.Unmarshal(it.Original, &raw)
			if _, ok := raw["class"]; ok {
				format = "key-v2"
				break
			}
		}
		k := &benchKey{Format: format, ID: fmt.Sprintf("key-%d", n), CreatedAt: a.now().UTC(), Note: note, FromReference: from, Items: items, Counts: map[string]int{}, Subject: subject, Itemset: set.ID, Author: orDefault(opts.Author, "unknown"), Blind: orDefault(opts.Blind, "unknown"), BaseKey: opts.BaseKey, OriginalSHA256: benchHash(data), PromptHash: benchHash([]byte(prompt))}
		for _, it := range items {
			k.Counts[it.Class]++
		}
		for _, text := range promptTexts {
			id, e := benchBlob([]byte(text), "prompt.md", file, a.now())
			if e != nil {
				return e
			}
			k.PromptBlobs = append(k.PromptBlobs, id)
		}
		md := fmt.Sprintf("# %s\n\nDate: %s\n\nProvenance: %s\n\nAuthor: %s\nBlind: %s\nSubject: %s\nItem set: %s\nBase key: %s\nOriginal SHA256: %s\n\nFrom reference: %t\n\nClasses: %v\n\nPrompt hash: %s\nPrompt blobs: %s\n\n%s\n", k.ID, k.CreatedAt.Format(time.RFC3339), note, k.Author, k.Blind, k.Subject, k.Itemset, k.BaseKey, k.OriginalSHA256, from, k.Counts, k.PromptHash, strings.Join(k.PromptBlobs, ", "), prompt)
		if e := benchImmutable(filepath.Join(dir, k.ID+".original.json"), data); e != nil {
			return e
		}
		if e := benchImmutable(filepath.Join(dir, k.ID+".md"), []byte(md)); e != nil {
			return e
		}
		if e := benchImmutableJSON(filepath.Join(dir, k.ID+".json"), k); e != nil {
			return e
		}
		result = k
		return nil
	})
	if e != nil {
		return nil, e
	}
	return result, benchReadme()
}

type benchScore struct {
	LegacyClasses map[string]locationScore `json:"legacy_classes,omitempty"`
	Unavailable   string                   `json:"unavailable,omitempty"`
	MappingIssues []string                 `json:"mapping_issues,omitempty"`
	Population    []string                 `json:"population,omitempty"`
	Classes       map[string]locationScore `json:"classes"`
	Judges        map[string]benchJudge    `json:"judges"`
	Coverage      string                   `json:"coverage"`
	KeySubject    string                   `json:"key_subject"`
	MatchedItems  int                      `json:"matched_items"`
	ItemNote      string                   `json:"item_note,omitempty"`
	// Seen separates retrieval, hints, rendered spans and unknown visibility.
	// KeyAgree includes coverage for missing, invalid and unsupported answers.
	Seen     map[string]seenCount `json:"seen,omitempty"`
	KeyAgree map[string]keyAgree  `json:"key_agree,omitempty"`
}
type benchJudge struct {
	Confusion map[string]map[string]int `json:"confusion"`
	Agree     int                       `json:"agree"`
	Both      int                       `json:"both"`
	Items     int                       `json:"items"`
	Cost      benchCost                 `json:"cost"`
	Seconds   float64                   `json:"seconds"`
}

func benchKeyReferences(k *benchKey) []refEntry {
	var set benchItemset
	if path, err := benchObjectPath("itemset", k.Itemset); err == nil {
		_ = benchReadJSON(path, &set)
	}
	var refs []refEntry
	for _, it := range k.Items {
		v := judgeVerdict(it.Class)
		if it.Class == "unknown" {
			v = verdictUnclr
		}
		r := refEntry{ID: it.ID, Revision: it.Revision, Package: it.Package, Item: it.Item, Verdict: v, Candidate: it.Candidate, Candidates: it.Candidates}
		// Key import paths remain untouched. Optional scoring metadata can be
		// recovered from the immutable original or its canonical source itemset.
		var extra struct {
			Revision   string   `json:"revision"`
			Candidates []string `json:"candidates"`
		}
		_ = json.Unmarshal(it.Original, &extra)
		if r.Revision == "" {
			r.Revision = extra.Revision
		}
		if len(r.Candidates) == 0 {
			r.Candidates = extra.Candidates
		}
		for _, source := range set.Items {
			if source.ID == it.ID && r.Revision == "" {
				r.Revision = source.Revision
			}
		}
		refs = append(refs, r)
	}
	return refs
}

func benchScoreArm(ad *armData, k *benchKey) benchScore {
	ref := ad.reference
	if ref == nil {
		ref = resolveReference(benchKeyReferences(k), ad.items)
	}
	s := benchScore{Classes: map[string]locationScore{}, LegacyClasses: map[string]locationScore{}, Judges: map[string]benchJudge{}, MappingIssues: referenceIssues(ref)}
	if len(s.MappingIssues) > 0 {
		s.Unavailable = "quality unavailable: " + strings.Join(s.MappingIssues, "; ")
	}
	for _, id := range benchSortedKeys(ref) {
		r := ref[id]
		s.Population = append(s.Population, r.ID+"@"+r.Revision)
	}
	kv := keyViewOf(ad, ref)
	s.Seen, s.KeyAgree = kv.Seen, kv.Agree
	for _, kind := range []string{verdictReuse, verdictExtend, verdictNew, verdictUnclr} {
		s.Classes[benchClass(kind)] = scoreLocations(ad, ref, "group", kind)
		if kind == verdictReuse || kind == verdictExtend {
			s.LegacyClasses[benchClass(kind)] = scoreLegacyLocations(ad, ref, "group", kind)
		}
	}
	for judge, verdicts := range ad.judges {
		j := benchJudge{Confusion: map[string]map[string]int{}, Cost: benchCost{Basis: "none"}}
		cost := 0.0
		known := 0
		for id, v := range verdicts {
			ans := ad.answer(judge, id)
			if len(s.MappingIssues) > 0 || !ans.Valid || !ans.Supported {
				continue
			}
			actual := benchClass(ref[id].Verdict)
			pred := benchClass(v)
			if actual != "" && pred != "" {
				if j.Confusion[actual] == nil {
					j.Confusion[actual] = map[string]int{}
				}
				j.Confusion[actual][pred]++
			}
			if g := ad.verdict("group", id); g != "" {
				j.Both++
				if g == v {
					j.Agree++
				}
			}
		}
		for _, row := range ad.rows {
			if row.Judge == judge {
				j.Items++
				j.Seconds += row.Seconds
				if row.CostKnown {
					known++
					cost += row.Cost
				} else {
					j.Cost.Missing++
				}
			}
		}
		if known > 0 {
			j.Cost.USD = &cost
			j.Cost.Basis = "reported"
		}
		s.Judges[judge] = j
	}
	return s
}
func benchRescore(r *benchRun, k *benchKey) (benchScore, error) {
	if e := benchKeyName(k.ID); e != nil {
		return benchScore{}, e
	}
	var s benchScore
	if r.Kind == "report-only" {
		s.Unavailable = "quality unavailable: report-only run has no scoring inputs"
		s.ItemNote = s.Unavailable
		s.Coverage = "key coverage: not recorded (report-only)"
		return s, nil
	}
	snap := filepath.Join(r.dir, "snapshot")
	var rec gateRecord
	if e := benchReadJSON(filepath.Join(snap, "gate.json"), &rec); e != nil {
		return s, e
	}
	if rec.Experiment != "" {
		if e := benchName(rec.Experiment); e != nil {
			return s, e
		}
	}
	packages := benchRunPackages(r, nil)
	ad := benchArmScoped(snap, &rec, packages)
	filtered := *k
	filtered.Items = nil
	for _, it := range k.Items {
		if len(packages) == 0 || packages[it.Package] {
			filtered.Items = append(filtered.Items, it)
		}
	}
	from, to := k.Itemset, r.Itemset
	mappingPath := filepath.Join(benchDir(), r.Suite, "keys", "mapping.json")
	var mapping *itemMapping
	var mappingErr error
	if err := benchSafe(mappingPath); err != nil {
		mappingErr = err
	} else {
		mapping, mappingErr = readItemMapping(mappingPath, k.ID, from, to)
	}
	// Validate against the complete key and run itemset before applying scope.
	allItems := readItemsFile(snap)
	if len(allItems) == 0 {
		allItems = ad.items
	}
	resolved := resolveMappedReference(benchKeyReferences(k), allItems, from, to, mapping)
	ad.reference = map[string]refEntry{}
	for id, ref := range resolved {
		if len(packages) > 0 && !packages[ref.Package] {
			continue
		}
		if mappingErr != nil {
			ref.Issue = mappingErr.Error()
		}
		ad.reference[id] = ref
	}
	s = benchScoreArm(ad, &filtered)
	s.Coverage = fmt.Sprintf("key coverage: %d of %d items in the compared groups", len(filtered.Items), len(k.Items))
	if changed, known := benchChangedKeyItems(r, &filtered); known {
		s.Coverage += fmt.Sprintf("; %d key items whose package changed between versions", changed)
	}
	s.KeySubject = k.Subject
	s.MatchedItems = len(ad.reference) - len(s.MappingIssues)
	s.ItemNote = fmt.Sprintf("identity coverage: %d/%d key behaviors mapped; %d excluded from conditional quality; no fuzzy joins; legacy location heuristics are not behavior accuracy", s.MatchedItems, len(filtered.Items), len(s.MappingIssues))
	if len(s.MappingIssues) > 0 {
		s.ItemNote += "; " + s.Unavailable
	}
	return s, nil
}

func cmdBench(a *app, args []string) error {
	if len(args) == 0 {
		return cmdBenchList(a, nil)
	}
	switch args[0] {
	case "--help", "-h":
		fmt.Fprintln(a.out, "bench: record/import snapshots; key add/rescore; spec add; list/show; compare/report; verify/migrate; export/import-archive; readme. Use orch bench <command> --help for flags.")
		return nil
	case "record", "import":
		fs := a.flags("bench " + args[0])
		suite := fs.String("suite", "", "suite slug")
		label := fs.String("label", "", "run label")
		arm := fs.String("arm", "", "arm name (default label)")
		kind := fs.String("kind", "", "gate-run, experiment or report-only")
		opts := benchRecordFlags(fs)
		pos, e := parseArgs(fs, args[1:])
		if e != nil {
			return e
		}
		if e := exactArgs(pos, 1, "bench "+args[0]+" <gate-run-dir|experiment> --suite S [--label L] [--arm A]"); e != nil {
			return e
		}
		if *kind != "" && *kind != "gate-run" && *kind != "experiment" && *kind != "report-only" {
			return fail("--kind must be gate-run, experiment or report-only")
		}
		if args[0] == "import" && (*suite == "" || *label == "") {
			return fail("import requires --suite and --label")
		}
		if *suite == "" {
			if s, e := a.site(); e == nil {
				*suite = s.cfg.UserBenchSuite
			}
		}
		options := opts()
		options.Kind = *kind
		r, e := benchRecordWith(a, pos[0], *suite, *label, *arm, options)
		if e != nil {
			return e
		}
		fmt.Fprintf(a.out, "bench: %s/%s (recorded, or already present)\n", r.Suite, r.ID)
		return nil
	case "list":
		return cmdBenchList(a, args[1:])
	case "show":
		pos, e := parseArgs(a.flags("bench show"), args[1:])
		if e != nil {
			return e
		}
		if e := exactArgs(pos, 1, "bench show <suite/run|run>"); e != nil {
			return e
		}
		suite, id, ok := strings.Cut(pos[0], "/")
		if ok {
			r, e := benchRunLoad(suite, id)
			if e != nil {
				return e
			}
			return benchShow(a, r)
		}
		suites, e := benchSuiteDirs()
		if e != nil {
			return e
		}
		var found *benchRun
		for _, s := range suites {
			r, e := benchRunLoad(s, pos[0])
			if e == nil {
				if found != nil {
					return fail("run is ambiguous; use suite/run")
				}
				found = r
			}
		}
		if found == nil {
			return fail("run %q not found", pos[0])
		}
		return benchShow(a, found)
	case "compare", "report":
		return cmdBenchReport(a, args[0], args[1:])
	case "readme":
		pos, e := parseArgs(a.flags("bench readme"), args[1:])
		if e != nil {
			return e
		}
		if e := exactArgs(pos, 0, "bench readme"); e != nil {
			return e
		}
		if e := benchReadme(); e != nil {
			return e
		}
		fmt.Fprintln(a.out, filepath.Join(benchDir(), "README.md"))
		return nil
	case "key":
		return cmdBenchKey(a, args[1:])
	case "spec":
		return cmdBenchSpec(a, args[1:])
	case "verify":
		return cmdBenchVerify(a, args[1:])
	case "migrate":
		return cmdBenchMigrate(a, args[1:])
	case "export":
		return cmdBenchExport(a, args[1:])
	case "import-archive":
		return cmdBenchImportArchive(a, args[1:])
	}
	return fail("unknown bench command %q; use record, import, list, show, compare, key, spec, report, verify, migrate, export, import-archive or readme", args[0])
}
func cmdBenchKey(a *app, args []string) error {
	if len(args) == 0 {
		return fail("use bench key add or rescore")
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(a.out, "bench key add <suite> <file.json> --note TEXT [--subject ID|--from-run RUN] [--author TEXT --blind true|false|unknown --items FILE --base-key key-N --prompt FILE --git-status FILE]\nbench key rescore <suite> <run> --key N")
		return nil
	}
	fs := a.flags("bench key " + args[0])
	note := fs.String("note", "", "key provenance")
	from := fs.Bool("from-reference", false, "old reference format")
	key := fs.String("key", "", "key number")
	var prompts benchStringFlags
	fs.Var(&prompts, "prompt", "prompt file to preserve (repeatable)")
	subject := fs.String("subject", "", "subject id")
	fromRun := fs.String("from-run", "", "subject from this run")
	author := fs.String("author", "unknown", "key author and model, if recorded")
	blind := fs.String("blind", "unknown", "true, false or unknown")
	items := fs.String("items", "", "blind item JSON file")
	baseKey := fs.String("base-key", "", "earlier key id")
	status := fs.String("git-status", "", "porcelain status file for the key subject")
	pos, e := parseArgs(fs, args[1:])
	if e != nil {
		return e
	}
	switch args[0] {
	case "rescore":
		if e := benchOnlyFlags(fs, "key"); e != nil {
			return e
		}
	case "add":
		if e := benchOnlyFlags(fs, "note", "from-reference", "prompt", "subject", "from-run", "author", "blind", "items", "base-key", "git-status"); e != nil {
			return e
		}
	default:
		return fail("unknown bench key command %q; use add or rescore", args[0])
	}
	if e := exactArgs(pos, 2, "bench key add <suite> <file.json> --note TEXT [--from-reference] [--prompt FILE] | bench key rescore <suite> <run> --key N"); e != nil {
		return e
	}
	switch args[0] {
	case "add":
		if *note == "" {
			return fail("key add requires --note provenance")
		}
		texts := []string{}
		for _, prompt := range prompts {
			b, e := os.ReadFile(expandHome(prompt))
			if e != nil {
				return e
			}
			texts = append(texts, string(b))
		}
		text := strings.Join(texts, "\n\n")
		k, e := benchKeyAdd(a, pos[0], pos[1], *note, text, *from, benchKeyOpts{Subject: *subject, FromRun: *fromRun, Author: *author, Blind: *blind, Items: *items, BaseKey: *baseKey, GitStatus: *status, PromptTexts: texts})
		if e != nil {
			return e
		}
		fmt.Fprintf(a.out, "key: %s/%s (%d items)\n", pos[0], k.ID, len(k.Items))
		return nil
	case "rescore":
		if *key == "" {
			return fail("rescore requires --key N")
		}
		artifact, e := benchRescoreWrite(pos[0], pos[1], *key)
		if e != nil {
			return e
		}
		return json.NewEncoder(a.out).Encode(artifact)
	}
	return fail("unknown bench key command %q", args[0])
}
func cmdBenchList(a *app, args []string) error {
	fs := a.flags("bench list")
	suite := fs.String("suite", "", "suite filter")
	pos, e := parseArgs(fs, args)
	if e != nil {
		return e
	}
	if e := exactArgs(pos, 0, "bench list [--suite S]"); e != nil {
		return e
	}
	var suites []string
	if *suite != "" {
		suites = []string{*suite}
	} else {
		var e error
		suites, e = benchSuiteDirs()
		if e != nil {
			return e
		}
	}
	n := 0
	fmt.Fprintln(a.out, "Suite | ID | Label | Date | Arm | Spec hash | Cost | Wall | Score")
	for _, s := range suites {
		rs, e := benchRuns(s)
		if e != nil {
			return e
		}
		for _, r := range rs {
			n++
			cost := benchRunCost(r)
			score := "not recorded"
			if k, e := benchKeyLoad(s, ""); e == nil {
				v, e := benchRescore(r, k)
				if e != nil {
					return e
				}
				if c, known := v.Classes["duplicate"]; known {
					if v.Unavailable != "" {
						score = k.ID + " unavailable (identity mapping)"
					} else {
						coverage := v.KeyAgree["group"]
						score = fmt.Sprintf("%s supported duplicate matches %d/%d key behaviors; answer coverage %d/%d", k.ID, c.Match, c.Total, coverage.Answered, coverage.Items)
					}
				}
			}
			fmt.Fprintf(a.out, "%s | %s | %s | %s | %s | %s | %s | %s | %s\n", s, r.ID, r.Label, r.CreatedAt.Format("2006-01-02"), r.Arm, r.Inputs.SpecHash, benchCostText(cost), benchGroupText(r, "wall", nil, false), score)
			fmt.Fprintln(a.out, benchTestedOn(r))
		}
	}
	fmt.Fprintf(a.out, "runs: %d\n", n)
	return nil
}
func benchDoctorLine() string {
	suites, runs, keys := 0, 0, 0
	ds, e := benchSuiteDirs()
	if e != nil {
		return fmt.Sprintf("bench: %s; %v", benchDir(), e)
	}
	for _, name := range ds {
		suites++
		rs, _ := benchRuns(name)
		ks, _ := benchKeys(name)
		runs += len(rs)
		keys += len(ks)
	}
	return fmt.Sprintf("bench: %s; %d suites, %d runs, %d keys", benchDir(), suites, runs, keys)
}
func benchReadme() error {
	path := filepath.Join(benchDir(), "README.md")
	if e := benchSafe(path); e != nil {
		return e
	}
	if benchExists(path) {
		return nil
	}
	return benchImmutable(path, []byte(benchReadmeText+benchReadmeSubjects))
}

const benchReadmeText = `# Orchestrate benchmark store

Generated by orch bench readme. Do not edit this file by hand.

Machine-local private data. Default: $ORCH_BENCH_DIR, otherwise
${XDG_DATA_HOME:-~/.local/share}/orchestrate/bench. Never commit this store
or put it inside ~/.agents or the repository being measured.

Keys are immutable, runs are append-only. Nothing is automatically deleted.
Gate pruning cannot remove history: recording copies the gate record, items,
checklist, dossiers and shadow rows. Original sessions are paths only and can vanish.

## Layout

- <suite>/suite.toml: repo, spec_path, base_commit, groups, reference_key,
  and [arms.<name>] with dossier, cache, runners, ctx_tool, context_files,
  shadow, spec_variant and orch flags. Create a suite by recording its first run,
  then edit suite.toml to set the groups for comparisons or add arm definitions.
  The first run initializes a suite definition. Later definition changes go to
  reports/suite-proposals/<sha256>.toml; the existing suite.toml stays byte-identical.
  Every run also retains its own arm definition. Reports never apply proposals.
- <suite>/keys/key-N.json: normalized versioned key and class counts.
  key-N.original.json preserves the input; key-N.md preserves provenance and prompt.
- <suite>/runs/YYYYMMDD-HHMM-label/run.json: schema_version 2. Build, inputs,
  resolved config, per-pass groups, findings, item metrics, scores, raw paths,
  and free metrics {value, unit, better: lower|higher|none}. Unknowns are null
  or empty legacy strings, never inferred from today's environment. Cost has
  basis reported|estimated|none; unknown USD is null. Partial totals are marked.
  snapshot/ holds durable scoring inputs; shadow.jsonl copies shadow rows.
  scores/<sha256>.json holds a versioned rescore with population/mapping ids,
  input hashes, denominators, exclusions and warnings. Old score-key-N.json files
  remain historical evidence. migrations/<sha256>.json holds reconstruction proposals.
  Rescore and migration never rewrite run.json, keys, prompts or old scores.
  note.md is optional.
- <suite>/reports/history-<sha256>.md: content-addressed history with an idempotent index.md.

## Commands

- Record: orch bench record /path/to/gate-run --suite crm-v1-reuse --label run3 --arm original
- Backfill: orch bench import /path/to/experiment --suite crm-v1-reuse --label arm1 --kind experiment
- Add key: orch bench key add crm-v1-reuse key.json --note 'blind review' --prompt prompt.md
  Use --from-reference for an old reference. Both old and new JSON formats are accepted.
- List: orch bench list --suite crm-v1-reuse
- Inspect: orch bench show crm-v1-reuse/run3
- Compare: orch bench compare crm-v1-reuse run3 arm1 --baseline run3 --key 2 --md
  Add --total to sum passes instead of joining per-group values with 'and'.
  --json returns the same table data for scripts. Unknowns print 'not recorded'.
- Rescore: orch bench key rescore crm-v1-reuse arm1 --key 2
  Validates an isolated copy before writing a new immutable score artifact.
  Uses the behavior-identity scorer, with explicit mapping when required.
  No overlap fuzzy match or model calls. Old scores stay labeled legacy.
- History: orch bench report crm-v1-reuse --runs run3,arm1 --since 2026-01-01 --key 2 --baseline run3 --md --save
  --json returns the report data. Without --save, print to stdout.
- Create this guide if missing: orch bench readme. An existing README stays byte-identical.

Gate integration: orch gate spec.md --only group:api --experiment arm1 --bench-suite crm-v1-reuse
or set bench_suite in user orchestrate.toml. Repo bench_suite is ignored. Default none does not record.

The newest key is the default and is always identified. Historical scores against
older keys remain available. Classes with fewer than 20 items need more evidence.
A config diff changing multiple factors is confounded, not a causal experiment.
Exact historical commands/builds that were not captured remain unknown.

Out of scope: bench run (executing suite arms), and a cross-system usage ledger.
`
