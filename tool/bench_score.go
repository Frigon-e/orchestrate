package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Bump the profile when scoring semantics change, and schema when this envelope
// changes. Neither version asserts that historical inputs meet today's gate.
const benchScorerSchema = "bench-score/v1"
const benchScorerProfile = "behavior-identity/v1"

type benchScoreInput struct {
	Path   string `json:"path"`
	ID     string `json:"id"`
	SHA256 string `json:"sha256,omitempty"`
	State  string `json:"state"`
}
type benchScoreMetric struct {
	Numerator   int            `json:"numerator"`
	Denominator int            `json:"denominator"`
	Basis       string         `json:"basis"`
	Exclusions  map[string]int `json:"exclusions"`
	Unknown     bool           `json:"unknown"`
}

type benchScoreArtifact struct {
	ScorerSchema string                      `json:"scorer_schema_version"`
	Profile      string                      `json:"profile_version"`
	Suite        string                      `json:"suite"`
	Run          string                      `json:"run"`
	Key          string                      `json:"key"`
	PopulationID string                      `json:"population_id"`
	MappingID    string                      `json:"mapping_id"`
	Inputs       []benchScoreInput           `json:"inputs"`
	Warnings     map[string][]string         `json:"metric_warnings"`
	Metrics      map[string]benchScoreMetric `json:"metrics"`
	Score        benchScore                  `json:"score"`
}

// CLI commands are synchronous. Scope the environment override to this call,
// restore even on failure, and never let enrichment operate on the source store.
func benchIsolated(fn func(source string) error) error {
	source, e := filepath.Abs(benchDir())
	if e != nil {
		return e
	}
	if e := benchSafe(source); e != nil {
		return e
	}
	info, e := os.Lstat(source)
	if e != nil {
		return e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fail("benchmark store must be a regular directory")
	}
	tmp, e := os.MkdirTemp("", "orch-bench-validate-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	old, had := os.LookupEnv("ORCH_BENCH_DIR")
	if e := os.Setenv("ORCH_BENCH_DIR", tmp); e != nil {
		return e
	}
	defer func() {
		if had {
			_ = os.Setenv("ORCH_BENCH_DIR", old)
		} else {
			_ = os.Unsetenv("ORCH_BENCH_DIR")
		}
	}()
	if e := benchCopy(source, tmp); e != nil {
		return e
	}
	return fn(source)
}

func benchScoreInputs(r *benchRun, k *benchKey) ([]benchScoreInput, error) {
	paths := map[string]string{}
	add := func(path, id string) { paths[path] = id }
	// Capture every archived scoring input, including checklist, dossiers,
	// manifests, sessions and original scores. New derived artifacts are excluded.
	e := filepath.WalkDir(r.dir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			if path != r.dir && (d.Name() == "scores" || d.Name() == "migrations") {
				return filepath.SkipDir
			}
			return nil
		}
		add(path, "")
		return nil
	})
	if e != nil {
		return nil, e
	}
	add(filepath.Join(benchDir(), r.Suite, "suite.toml"), "")
	for _, suffix := range []string{".json", ".original.json", ".md"} {
		add(filepath.Join(benchDir(), r.Suite, "keys", k.ID+suffix), k.ID)
	}
	mapping := filepath.Join(benchDir(), r.Suite, "keys", "mapping.json")
	add(mapping, "mapping")
	object := func(kind, id string) error {
		if id == "" {
			return nil
		}
		path, e := benchObjectPath(kind, id)
		if e != nil {
			return e
		}
		add(path, id)
		if kind == "spec" {
			add(strings.TrimSuffix(path, ".md")+".json", id)
		}
		return nil
	}
	for _, id := range []string{r.Itemset, k.Itemset} {
		if e := object("itemset", id); e != nil {
			return nil, e
		}
	}
	for _, id := range []string{r.Subject, k.Subject} {
		if id == "" {
			continue
		}
		path := filepath.Join(benchDir(), "subjects", id+".json")
		add(path, id)
		s, e := benchSubjectLoad(id)
		if e != nil {
			return nil, e
		}
		if e := object("spec", s.Spec.Hash); e != nil {
			return nil, e
		}
		if e := object("blob", s.Dirty.DiffBlob); e != nil {
			return nil, e
		}
	}
	for _, ids := range r.Blobs {
		for _, id := range ids {
			if e := object("blob", id); e != nil {
				return nil, e
			}
		}
	}
	for _, id := range k.PromptBlobs {
		if e := object("blob", id); e != nil {
			return nil, e
		}
	}
	inputs := []benchScoreInput{}
	for _, path := range benchSortedKeys(paths) {
		if e := benchSafe(path); e != nil {
			return nil, e
		}
		rel, e := filepath.Rel(benchDir(), path)
		if e != nil {
			return nil, e
		}
		input := benchScoreInput{Path: filepath.ToSlash(rel), ID: paths[path], State: "recorded"}
		b, e := os.ReadFile(path)
		if os.IsNotExist(e) {
			input.State = "unknown"
		} else if e != nil {
			return nil, e
		} else {
			input.SHA256 = benchHash(b)
			if input.ID == "" {
				input.ID = input.SHA256
			}
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

func benchScoreArtifactBuild(r *benchRun, k *benchKey) (benchScoreArtifact, error) {
	v := benchScoreArtifact{ScorerSchema: benchScorerSchema, Profile: benchScorerProfile, Suite: r.Suite, Run: r.ID, Key: k.ID, MappingID: "unknown", Warnings: map[string][]string{}, Metrics: map[string]benchScoreMetric{}}
	var e error
	v.Score, e = benchRescore(r, k)
	if e != nil {
		return v, e
	}
	v.Inputs, e = benchScoreInputs(r, k)
	if e != nil {
		return v, e
	}
	for _, input := range v.Inputs {
		if strings.HasSuffix(input.Path, "/keys/mapping.json") && input.SHA256 != "" {
			v.MappingID = input.SHA256
		}
	}
	groups := map[string][]string{}
	for name, g := range r.Groups {
		groups[name] = append([]string{}, g.Packages...)
		sort.Strings(groups[name])
	}
	population, e := json.Marshal(struct {
		Subject    string              `json:"subject"`
		KeySubject string              `json:"key_subject"`
		RunItemset string              `json:"run_itemset"`
		KeyItemset string              `json:"key_itemset"`
		Groups     map[string][]string `json:"groups"`
		Items      []string            `json:"items"`
	}{r.Subject, k.Subject, r.Itemset, k.Itemset, groups, v.Score.Population})
	if e != nil {
		return v, e
	}
	v.PopulationID = benchHash(population)
	v.Warnings["reproducibility"] = []string{"historical identity only; reproducibility not tested; scorer version is not current gate approval"}
	if r.Subject != "" {
		if subject, e := benchSubjectLoad(r.Subject); e == nil {
			if subject.Commit == "" {
				v.Warnings["reproducibility"] = append(v.Warnings["reproducibility"], "commit unknown")
			}
			if subject.CommitBasis != "git" {
				v.Warnings["reproducibility"] = append(v.Warnings["reproducibility"], "commit not git-verified: "+subject.CommitBasis)
			}
		}
	}
	if r.Subject == "" || k.Subject == "" {
		v.Warnings["reproducibility"] = append(v.Warnings["reproducibility"], "legacy unbound subject: unknown; no current approval or reproducibility inferred")
	}
	if r.WallSeconds.Value == nil {
		v.Warnings["wall_seconds"] = []string{"not recorded; pass time is not end-to-end elapsed time"}
	}
	for group, g := range r.Groups {
		if g.Cost.USD == nil || g.Cost.Missing > 0 {
			v.Warnings["cost/"+group] = []string{fmt.Sprintf("known cost coverage incomplete; %d requests missing; never inferred from tokens or current prices", g.Cost.Missing)}
		}
	}
	v.Warnings["legacy"] = []string{"original scores and legacy location heuristics are historical evidence, not current behavior quality or approval"}
	for name, c := range v.Score.Classes {
		exclusions := map[string]int{"missing_invalid_unsupported": c.Excluded, "unmapped": len(v.Score.MappingIssues)}
		v.Metrics["class/"+name+"/precision"] = benchScoreMetric{c.Match, c.Found, "supported predictions within mapped key; non-key predictions excluded", exclusions, c.Precision == nil}
		v.Metrics["class/"+name+"/recall"] = benchScoreMetric{c.Match, c.Total, "all keyed behaviors of class, including missing answers", map[string]int{}, c.Recall == nil}
		v.Metrics["class/"+name+"/f1"] = benchScoreMetric{2 * c.Match, c.Found + c.Total, c.Denominator, exclusions, c.F1 == nil}
		warnings := []string{}
		if c.Unavailable != "" {
			warnings = append(warnings, c.Unavailable)
		}
		if c.Excluded > 0 {
			warnings = append(warnings, fmt.Sprintf("%d missing, invalid or unsupported answers excluded from supported predictions; retained in recall population", c.Excluded))
		}
		if c.Precision == nil {
			warnings = append(warnings, "precision unknown: no supported prediction denominator or identity unavailable")
		}
		if c.Recall == nil {
			warnings = append(warnings, "recall unknown: no class population or identity unavailable")
		}
		v.Warnings["class/"+name] = warnings
	}
	for judge, c := range v.Score.KeyAgree {
		exclusions := map[string]int{"missing": c.Missing, "invalid": c.Invalid, "unsupported": c.Unsupported, "unmapped": c.Unmapped}
		v.Metrics["answers/"+judge+"/coverage"] = benchScoreMetric{c.Answered, c.Items, "all keyed behaviors", map[string]int{}, c.Items == 0}
		v.Metrics["answers/"+judge+"/valid"] = benchScoreMetric{c.Valid, c.Items, "all keyed behaviors", map[string]int{}, c.Items == 0}
		v.Metrics["answers/"+judge+"/supported"] = benchScoreMetric{c.Supported, c.Items, "all keyed behaviors", map[string]int{}, c.Items == 0}
		v.Metrics["answers/"+judge+"/conditional_accuracy"] = benchScoreMetric{c.Agree, c.Both, "supported mapped answers", exclusions, c.Both == 0 || c.Unavailable != ""}
		v.Warnings["answers/"+judge] = []string{fmt.Sprintf("coverage denominator %d; conditional denominator %d; missing %d, invalid %d, unsupported %d, unmapped %d", c.Items, c.Both, c.Missing, c.Invalid, c.Unsupported, c.Unmapped)}
	}
	for judge, c := range v.Score.Seen {
		v.Metrics["visibility/"+judge+"/rendered"] = benchScoreMetric{c.Seen, c.Total, "all keyed behaviors with targets; unknown instrumentation is not unseen", map[string]int{"unknown": c.Unknown}, c.Total == 0 || c.Unknown > 0}
		v.Metrics["visibility/"+judge+"/retrieved"] = benchScoreMetric{c.Retrieved, c.Total, "all keyed behaviors with targets", map[string]int{"unknown": c.RetrievalUnknown}, c.Total == 0 || c.RetrievalUnknown > 0}
		v.Warnings["visibility/"+judge] = []string{fmt.Sprintf("target denominator %d; visibility unknown %d; retrieval unknown %d; hints are not rendered bodies", c.Total, c.Unknown, c.RetrievalUnknown)}
	}
	if v.Score.Unavailable != "" {
		v.Warnings["quality"] = []string{v.Score.Unavailable}
	}
	return v, nil
}

func benchOriginalScoreText(r *benchRun, key, class string) string {
	score, ok := r.Scores[key]
	if !ok {
		path := filepath.Join(r.dir, "score-"+key+".json")
		if !benchExists(path) {
			return "not recorded"
		}
		if e := benchReadJSON(path, &score); e != nil {
			return "unreadable historical score: " + e.Error()
		}
	}
	c, ok := score.Classes[class]
	if !ok {
		return "not recorded"
	}
	return fmt.Sprintf("%d/%d matched; precision %s, recall %s, F1 %s; historical basis, not current behavior quality", c.Match, c.Total, benchPercent(c.Precision), benchPercent(c.Recall), benchPercent(c.F1))
}

func benchRescoreWrite(suite, run, key string) (benchScoreArtifact, error) {
	var artifact benchScoreArtifact
	if e := benchSuiteName(suite); e != nil {
		return artifact, e
	}
	if e := benchName(run); e != nil {
		return artifact, e
	}
	if !strings.HasPrefix(key, "key-") {
		key = "key-" + key
	}
	if e := benchKeyName(key); e != nil {
		return artifact, e
	}
	var rel string
	e := benchIsolated(func(source string) error {
		if e := benchValidateRescore(suite); e != nil {
			return e
		}
		r, e := benchRunLoad(suite, run)
		if e != nil {
			return e
		}
		k, e := benchKeyLoad(suite, key)
		if e != nil {
			return e
		}
		artifact, e = benchScoreArtifactBuild(r, k)
		if e != nil {
			return e
		}
		rel = filepath.Join(suite, "runs", r.ID, "scores")
		// Check all inputs against the source before publishing after copy validation.
		for _, input := range artifact.Inputs {
			b, e := os.ReadFile(filepath.Join(source, filepath.FromSlash(input.Path)))
			if input.State == "unknown" && os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			if input.SHA256 != benchHash(b) {
				return fail("benchmark input changed during isolated validation: %s", input.Path)
			}
		}
		return nil
	})
	if e != nil {
		return artifact, e
	}
	b, e := json.MarshalIndent(artifact, "", "  ")
	if e != nil {
		return artifact, e
	}
	b = append(b, '\n')
	return artifact, benchImmutable(filepath.Join(benchDir(), rel, benchHash(b)+".json"), b)
}
