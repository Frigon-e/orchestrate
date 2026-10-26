package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type benchRecordOpts struct {
	Kind           string `json:"-"`
	Repo           string `json:"repo,omitempty"`
	Commit         string `json:"commit,omitempty"`
	CommitBasis    string `json:"commit_basis,omitempty"`
	CommitEvidence string `json:"commit_evidence,omitempty"`
	SpecText       string `json:"spec_text,omitempty"`
	DirtyFrom      string `json:"dirty_from,omitempty"`
	Console        string `json:"console,omitempty"`
	ConsoleStart   string `json:"console_start,omitempty"`
	WithSessions   bool   `json:"with_sessions,omitempty"`
}

func benchRecordOptsCheck(o benchRecordOpts) error {
	if o.Commit == "" && (o.CommitBasis != "" || o.CommitEvidence != "") {
		return fail("--commit-basis and --commit-evidence require --commit")
	}
	if o.Commit != "" {
		if o.CommitBasis != "user" && o.CommitBasis != "note" {
			return fail("--commit requires --commit-basis user|note")
		}
		if strings.TrimSpace(o.CommitEvidence) == "" {
			return fail("--commit requires --commit-evidence")
		}
		if !regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`).MatchString(o.Commit) {
			return fail("--commit must be a commit hash")
		}
	}
	if o.ConsoleStart != "" && o.Console == "" {
		return fail("--console-start requires --console")
	}
	return nil
}
func benchSourceRepo(dir string) string {
	for p := dir; ; p = filepath.Dir(p) {
		if owner := rootOwner(p); owner != "" {
			return owner
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	r, _ := repoRoot(dir)
	return r
}
func benchSourceIdentity(dir string, opts benchRecordOpts) (string, error) {
	records := []string{}
	add := func(p, name string) error {
		return filepath.WalkDir(p, func(path string, d fs.DirEntry, e error) error {
			if os.IsNotExist(e) && path == p {
				return nil
			}
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fail("refusing symlink in benchmark source: %s", path)
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fail("non-regular benchmark source: %s", path)
			}
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			rel, _ := filepath.Rel(p, path)
			records = append(records, name+"/"+rel+":"+benchHash(b))
			return nil
		})
	}
	for _, name := range []string{"gate.json", "items.json", "checklist.json", "dossier", "report.md", "subject.json", "passes", "sessions"} {
		if e := add(filepath.Join(dir, name), name); e != nil {
			return "", e
		}
	}
	var rec gateRecord
	_ = json.Unmarshal(readBenchSource(filepath.Join(dir, "gate.json")), &rec)
	if rec.Experiment != "" {
		if e := benchName(rec.Experiment); e != nil {
			return "", e
		}
	}
	gdir := dir
	if rec.Experiment != "" {
		gdir = filepath.Dir(filepath.Dir(dir))
	}
	if e := add(shadowDir(gdir, rec.Experiment), "shadow"); e != nil {
		return "", e
	}
	for _, path := range []string{opts.SpecText, opts.DirtyFrom, opts.Console, opts.ConsoleStart} {
		if path != "" {
			if e := add(expandHome(path), path); e != nil {
				return "", e
			}
		}
	}
	b, _ := json.Marshal(opts)
	records = append(records, string(b))
	return benchHash([]byte(strings.Join(records, "\n"))), nil
}
func readBenchSource(path string) []byte { b, _ := os.ReadFile(path); return b }
func benchHistoricalCapture(rec *gateRecord, dir string, opts benchRecordOpts, now time.Time) (benchCapture, []string, error) {
	var c benchCapture
	repo := orDefault(opts.Repo, orDefault(rec.BenchInputs.Repo, benchSourceRepo(dir)))
	path := orDefault(rec.SpecRel, orDefault(rec.BenchInputs.SpecPath, rec.Spec))
	if filepath.IsAbs(path) && repo != "" {
		if rel, e := filepath.Rel(repo, path); e == nil {
			path = rel
		}
	}
	if !benchHex16.MatchString(rec.Hash) {
		return c, nil, fail("gate record has invalid spec hash %q", rec.Hash)
	}
	text, known := "", false
	if opts.SpecText != "" {
		b, e := os.ReadFile(expandHome(opts.SpecText))
		if e != nil {
			return c, nil, e
		}
		text = string(b)
		if specHash([]byte(text)) != rec.Hash {
			return c, nil, fail("--spec-text hash does not match gate record")
		}
		known = true
	} else if objectHas("spec", rec.Hash) {
		b, _ := objectGet("spec", rec.Hash)
		text, known = string(b), true
	} else if repo != "" {
		b, e := os.ReadFile(filepath.Join(repo, path))
		if e == nil && specHash(b) == rec.Hash {
			text, known = string(b), true
		}
		if !known {
			log, _ := git(repo, "log", "--all", "--format=%H", "--", path)
			for _, commit := range strings.Fields(log) {
				v, e := benchGitText(repo, commit, path)
				if e == nil && specHash([]byte(v)) == rec.Hash {
					text, known = v, true
					break
				}
			}
		}
	}
	info := benchRepoInfo(repo)
	c.Subject = benchSubject{SchemaVersion: 1, Repo: info, CommitBasis: "unknown", Dirty: benchDirty{State: "unknown"}, Spec: benchSpecRef{Hash: rec.Hash, Path: path, State: "unknown"}, CreatedAt: now.UTC(), CreatedBy: benchBuildNow()}
	warnings := []string{}
	if known {
		c.Text = text
		c.Spec, c.Itemset = benchSpecFromText(text, path, info, now)
		benchSpecHistory(repo, &c.Spec, true)
		if len(c.Spec.Commits) > 0 {
			c.Spec.State = "committed"
		} else {
			warnings = append(warnings, "spec text is not recorded in git; historical spec state unknown")
		}
		c.Subject.Spec.State = c.Spec.State
	} else {
		warnings = append(warnings, "spec text unknown: supply --repo or verified --spec-text; no commit or dirty state inferred")
	}
	if e := benchApplySubjectOpts(&c.Subject, opts); e != nil {
		return c, nil, e
	}
	c.Subject.ID = benchSubjectID(c.Subject)
	var v *benchSpecVersion
	if known {
		v = &c.Spec
	}
	c.Subject.Description = benchDescribe(c.Subject, v)
	return c, warnings, nil
}
func benchApplySubjectOpts(s *benchSubject, opts benchRecordOpts) error {
	if opts.Commit == "" && (opts.CommitBasis != "" || opts.CommitEvidence != "") {
		return fail("--commit-basis and --commit-evidence require --commit")
	}
	if opts.Commit != "" {
		if opts.CommitBasis != "user" && opts.CommitBasis != "note" {
			return fail("--commit requires --commit-basis user|note")
		}
		if strings.TrimSpace(opts.CommitEvidence) == "" {
			return fail("--commit requires --commit-evidence")
		}
		if !regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`).MatchString(opts.Commit) {
			return fail("--commit must be a commit hash")
		}
		commit := strings.ToLower(opts.Commit)
		if _, e := git(s.Repo.Path, "rev-parse", "--git-dir"); s.Repo.Path != "" && e == nil {
			resolved, e := git(s.Repo.Path, "rev-parse", "--verify", commit+"^{commit}")
			if e != nil {
				return fail("--commit %s is not in the repository", commit)
			}
			commit = resolved
		}
		s.Commit, s.CommitBasis, s.CommitEvidence = commit, opts.CommitBasis, opts.CommitEvidence
		s.CommittedAt, _ = git(s.Repo.Path, "log", "-1", "--format=%cI", commit)
		s.Branch = ""
	}
	if opts.DirtyFrom != "" {
		b, e := os.ReadFile(expandHome(opts.DirtyFrom))
		if e != nil {
			return e
		}
		status := string(b)
		if !strings.Contains(status, "\x00") {
			status = strings.ReplaceAll(strings.TrimSuffix(status, "\n"), "\n", "\x00")
		}
		for _, entry := range strings.Split(status, "\x00") {
			if entry != "" && (len(entry) < 4 || !regexp.MustCompile(`^(\?\?|[ MADRCU][ MADRCU]) `).MatchString(entry) || entry[:2] == "  ") {
				return fail("--dirty-from must contain git status --porcelain=v1 output")
			}
		}
		s.Dirty = benchDirtyFromStamp(checkoutStamp{status: status})
	}
	return nil
}
func benchEnrichRun(a *app, r *benchRun, rec *gateRecord, dir, snap string, opts benchRecordOpts) error {
	if e := benchStoreInit(a.now()); e != nil {
		return e
	}
	if rec.Experiment != "" {
		if e := benchName(rec.Experiment); e != nil {
			return e
		}
	}
	var c benchCapture
	b, e := os.ReadFile(filepath.Join(dir, "subject.json"))
	loaded := false
	if e == nil {
		if e := json.Unmarshal(b, &c); e != nil {
			return e
		}
		if c.Subject.ID != "" {
			if c.Subject.Spec.Hash != rec.Hash || (c.Spec.Hash != "" && (specHash([]byte(c.Text)) != rec.Hash || c.Spec.Hash != rec.Hash)) || (c.Spec.Hash == "" && c.Subject.Spec.State != "unknown") {
				return fail("spec text in subject.json does not match the gate record")
			}
			if c.Subject.ID != benchSubjectID(c.Subject) {
				return fail("subject.json identity mismatch")
			}
			loaded = true
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if !loaded {
		var warnings []string
		c, warnings, e = benchHistoricalCapture(rec, dir, opts, a.now())
		if e != nil {
			return e
		}
		r.Warnings = append(r.Warnings, warnings...)
	}
	if c.Diff != "" {
		if len(c.Diff) > 256*1024 || benchHash([]byte(c.Diff)) != c.Subject.Dirty.DiffSHA256 {
			return fail("dirty patch hash or size mismatch")
		}
		id, e := benchBlob([]byte(c.Diff), "dirty.patch", dir, a.now())
		if e != nil {
			return e
		}
		c.Subject.Dirty.DiffBlob = id
	}
	if loaded && (opts.Commit != "" || opts.DirtyFrom != "") {
		old := c.Subject.ID
		if c.Spec.Hash != "" {
			if e := benchSaveSpec(c.Spec, c.Itemset, c.Text); e != nil {
				return e
			}
		}
		if e := benchSaveSubject(c.Subject); e != nil {
			return e
		}
		if e := benchApplySubjectOpts(&c.Subject, opts); e != nil {
			return e
		}
		c.Subject.ID = benchSubjectID(c.Subject)
		c.Subject.Description = benchDescribe(c.Subject, &c.Spec)
		if old != c.Subject.ID {
			r.SubjectHistory = append(r.SubjectHistory, benchSubjectMove{At: a.now().UTC(), From: old, To: c.Subject.ID, Basis: opts.CommitBasis, Evidence: opts.CommitEvidence})
		}
	}
	if c.Spec.Hash != "" {
		if e := benchSaveSpec(c.Spec, c.Itemset, c.Text); e != nil {
			return e
		}
		r.Inputs.SpecSHA256 = c.Spec.SHA256
		if stored, e := benchSpecLoad(c.Spec.Hash); e == nil {
			c.Subject.Variant = stored.VariantLabel
			r.Inputs.SpecVariant = stored.VariantLabel
			c.Subject.Description = benchDescribe(c.Subject, stored)
		}
	}
	if e := benchSaveSubject(c.Subject); e != nil {
		return e
	}
	r.Subject, r.Inputs.Commit, r.Inputs.Repo, r.Inputs.SpecPath = c.Subject.ID, c.Subject.Commit, c.Subject.Repo.Path, c.Subject.Spec.Path
	r.Inputs.SpecHash = rec.Hash
	r.Blobs = map[string][]string{}
	r.WallSeconds.Basis = "none"
	if c.Subject.Dirty.DiffBlob != "" {
		r.Blobs["dirty_patch"] = []string{c.Subject.Dirty.DiffBlob}
	}
	if b != nil {
		id, e := benchBlob(b, "subject.json", dir, a.now())
		if e != nil {
			return e
		}
		r.Blobs["subject_json"] = []string{id}
	} else {
		path := filepath.Join(snap, "subject.json")
		if e := benchJSON(path, c); e != nil {
			return e
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		id, e := benchBlob(b, "subject.json", dir, a.now())
		if e != nil {
			return e
		}
		r.Blobs["subject_json"] = []string{id}
	}
	if data, e := os.ReadFile(filepath.Join(snap, "items.json")); e == nil {
		var items []gateItem
		if e := json.Unmarshal(data, &items); e != nil {
			return e
		}
		spec := rec.Hash
		if c.Spec.Hash == "" {
			spec = ""
		}
		set := benchMakeItemset("run", spec, items)
		if e := benchSaveItemset(set); e != nil {
			return e
		}
		r.Itemset = set.ID
	} else if !os.IsNotExist(e) {
		return e
	}
	if opts.Console != "" {
		data, e := os.ReadFile(expandHome(opts.Console))
		if e != nil {
			return e
		}
		id, e := benchBlob(data, filepath.Base(opts.Console), opts.Console, a.now())
		if e != nil {
			return e
		}
		r.Blobs["console"] = []string{id}
		if opts.ConsoleStart != "" {
			start, e := os.ReadFile(expandHome(opts.ConsoleStart))
			if e != nil {
				return e
			}
			id, e := benchBlob(start, filepath.Base(opts.ConsoleStart), opts.ConsoleStart, a.now())
			if e != nil {
				return e
			}
			r.Blobs["console_start"] = []string{id}
			seconds, e := benchConsoleSeconds(string(start), string(data))
			if e != nil {
				return e
			}
			r.WallSeconds = benchWall{Value: &seconds, Basis: "console"}
		}
	} else if opts.ConsoleStart != "" {
		return fail("--console-start requires --console")
	}
	files, e := filepath.Glob(filepath.Join(dir, "passes", "*.prompt"))
	if e != nil {
		return e
	}
	size := 0
	prompts := [][]byte{}
	for _, path := range files {
		fi, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !fi.Mode().IsRegular() {
			return fail("non-regular pass prompt: %s", path)
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		size += len(b)
		prompts = append(prompts, b)
	}
	stored := size <= 8*1024*1024
	r.PromptsStored = &stored
	for i, b := range prompts {
		id := benchHash(b)
		if stored {
			var e error
			id, e = benchBlob(b, filepath.Base(files[i]), files[i], a.now())
			if e != nil {
				return e
			}
		}
		r.Blobs["prompts"] = append(r.Blobs["prompts"], id)
	}
	if opts.WithSessions && benchExists(filepath.Join(dir, "sessions")) {
		if e := benchCopy(filepath.Join(dir, "sessions"), filepath.Join(snap, "sessions")); e != nil {
			return e
		}
	}
	return nil
}
func benchCaptureShadowCosts(r *benchRun, rows []shadowRow) {
	grouped := map[string][]shadowRow{}
	for _, row := range rows {
		if row.Skipped != "" {
			continue
		}
		name := "shadow-" + row.Package
		if row.Judge != "flash" {
			name = "shadow-" + row.Judge + ":" + row.Package
		}
		grouped[name] = append(grouped[name], row)
	}
	for name, rows := range grouped {
		g := r.Groups[name]
		if g.Cost.USD != nil {
			continue
		}
		total, seconds, known, missing := 0.0, 0.0, 0, 0
		for _, row := range rows {
			if row.CostKnown {
				total += row.Cost
				known++
			} else {
				missing++
			}
			seconds += row.Seconds
			if g.Runner == "" && row.Model != "" {
				g.Runner = "model:" + row.Model
			}
			if len(g.Packages) == 0 {
				g.Packages = []string{row.Package}
			}
		}
		g.Cost = benchCost{Basis: "none", Missing: missing}
		if known > 0 {
			g.Cost.USD = &total
			g.Cost.Basis = "reported"
		}
		if g.Seconds == nil {
			n := int(seconds)
			g.Seconds = &n
		}
		r.Groups[name] = g
	}
}

func benchConsoleSeconds(start, console string) (float64, error) {
	parse := func(s string) (time.Time, error) {
		if t, e := time.Parse(time.RFC3339, s); e == nil {
			return t, nil
		}
		return time.Parse("15:04:05", s)
	}
	a, e := parse(strings.TrimSpace(start))
	if e != nil {
		return 0, fail("console start must be RFC3339 or HH:MM:SS")
	}
	lines := strings.Split(strings.TrimSpace(console), "\n")
	b, e := parse(strings.TrimSpace(lines[len(lines)-1]))
	if e != nil {
		return 0, fail("console must end with an explicit RFC3339 or HH:MM:SS timestamp")
	}
	seconds := b.Sub(a).Seconds()
	if seconds < 0 {
		return 0, fail("console end precedes start; use dated RFC3339 timestamps")
	}
	return seconds, nil
}
func benchRecordReportOnly(a *app, dir, suite, label, arm string, opts benchRecordOpts) (*benchRun, error) {
	if opts.Kind != "" && opts.Kind != "report-only" {
		return nil, fail("source is report-only, not %s", opts.Kind)
	}
	if label == "" {
		return nil, fail("report-only import requires --label")
	}
	if arm == "" {
		arm = label
	}
	for _, s := range []string{label, arm} {
		if e := benchName(s); e != nil {
			return nil, e
		}
	}
	hash := filepath.Base(dir)
	if !benchHex16.MatchString(hash) {
		return nil, fail("report-only directory must be named by its 16-hex spec hash")
	}
	b, e := os.ReadFile(filepath.Join(dir, "report.md"))
	if e != nil {
		return nil, e
	}
	root := filepath.Join(benchDir(), suite)
	if e := benchMkdir(root); e != nil {
		return nil, e
	}
	if e := benchSafe(filepath.Join(root, ".lock")); e != nil {
		return nil, e
	}
	var result *benchRun
	e = benchWithLock(filepath.Join(root, ".lock"), func() error {
		identity, e := benchSourceIdentity(dir, opts)
		if e != nil {
			return e
		}
		runs, e := benchRuns(suite)
		if e != nil {
			return e
		}
		for _, r := range runs {
			if r.SnapshotHash == identity && r.Label == label && r.Arm == arm {
				result = r
				return nil
			}
		}
		// A report's mtime is import provenance, not a claim about execution time.
		id := a.now().UTC().Format("20060102-1504") + "-" + benchPrefix(label, 50)
		base := id
		for i := 2; benchExists(filepath.Join(root, "runs", id)); i++ {
			suffix := fmt.Sprintf("-%d", i)
			id = benchPrefix(base, 64-len(suffix)) + suffix
		}
		if e := benchMkdir(filepath.Join(root, "runs")); e != nil {
			return e
		}
		tmp, e := os.MkdirTemp(filepath.Join(root, "runs"), ".record-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(tmp)
		r := &benchRun{SchemaVersion: 2, ID: id, Label: label, Suite: suite, Arm: arm, Kind: "report-only", CreatedAt: a.now().UTC(), SnapshotHash: identity, Groups: map[string]benchGroup{}, Raw: map[string]string{"report": filepath.Join(dir, "report.md")}, Note: "Execution date and findings unknown. Report prose is not parsed into counts; created_at is the import date."}
		rec := gateRecord{Hash: hash, Spec: "unknown"}
		if v, e := benchSpecLoad(hash); e == nil {
			rec.Spec, rec.SpecRel = v.Path, v.Path
		}
		r.State, r.historicalGateResult = readStoredGateReport(string(b))
		snap := filepath.Join(tmp, "snapshot")
		if e := benchWrite(filepath.Join(snap, "report.md"), b); e != nil {
			return e
		}
		if e := benchEnrichRun(a, r, &rec, dir, snap, opts); e != nil {
			return e
		}
		s, e := benchSuiteLoad(suite)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if s.Subject.Default == "" {
			s.Subject.Default = r.Subject
		}
		if e := benchSuiteWrite(suite, s); e != nil {
			return e
		}
		if e := benchJSON(filepath.Join(tmp, "run.json"), r); e != nil {
			return e
		}
		dest := filepath.Join(root, "runs", id)
		if e := benchSafe(dest); e != nil {
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
	return result, benchReadme()
}
