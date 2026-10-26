package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

type benchSuiteSubject struct {
	Default string `toml:"default"`
}
type benchWall struct {
	Value *float64 `json:"value"`
	Basis string   `json:"basis"`
}
type benchSubjectMove struct {
	At       time.Time `json:"at"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Basis    string    `json:"basis"`
	Evidence string    `json:"evidence"`
}
type benchRepo = repositoryIdentity
type benchDirty struct {
	State      string   `json:"state"`
	Untracked  []string `json:"untracked"`
	Modified   []string `json:"modified"`
	DiffSHA256 string   `json:"diff_sha256"`
	DiffBlob   string   `json:"diff_blob,omitempty"`
	DiffBytes  int      `json:"diff_bytes,omitempty"`
}
type benchSpecRef struct {
	Hash  string `json:"hash"`
	Path  string `json:"path"`
	State string `json:"state"`
}
type benchSubject struct {
	SchemaVersion  int          `json:"schema_version"`
	ID             string       `json:"id"`
	Repo           benchRepo    `json:"repo"`
	Commit         string       `json:"commit"`
	CommitBasis    string       `json:"commit_basis"`
	CommitEvidence string       `json:"commit_evidence"`
	CommittedAt    string       `json:"committed_at"`
	Branch         string       `json:"branch"`
	Dirty          benchDirty   `json:"dirty"`
	Spec           benchSpecRef `json:"spec"`
	Variant        string       `json:"variant,omitempty"`
	Description    string       `json:"description"`
	CreatedAt      time.Time    `json:"created_at"`
	CreatedBy      benchBuild   `json:"created_by"`
}
type benchSpecCommit struct {
	Commit      string   `json:"commit"`
	CommittedAt string   `json:"committed_at"`
	Subject     string   `json:"subject"`
	Refs        []string `json:"refs,omitempty"`
}
type benchSpecParent struct {
	Hash   string `json:"hash"`
	Commit string `json:"commit"`
}
type benchSpecPackage struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Kind        string `json:"kind"`
	Builds      int    `json:"builds"`
	ContentHash string `json:"content_hash"`
	Owns        int    `json:"owns"`
}
type benchSpecVersion struct {
	SchemaVersion  int                `json:"schema_version"`
	Hash           string             `json:"hash"`
	SHA256         string             `json:"sha256"`
	Bytes          int                `json:"bytes"`
	Lines          int                `json:"lines"`
	Path           string             `json:"path"`
	Title          string             `json:"title"`
	Repo           benchRepo          `json:"repo"`
	Commits        []benchSpecCommit  `json:"commits"`
	State          string             `json:"state"`
	CommittedHash  string             `json:"committed_hash,omitempty"`
	Parent         *benchSpecParent   `json:"parent"`
	VariantOf      string             `json:"variant_of,omitempty"`
	VariantLabel   string             `json:"variant_label,omitempty"`
	Author         string             `json:"author,omitempty"`
	NotesBlob      string             `json:"notes_blob,omitempty"`
	Packages       []benchSpecPackage `json:"packages"`
	Itemset        string             `json:"itemset"`
	FormatProblems int                `json:"format_problems"`
	AddedAt        time.Time          `json:"added_at"`
	AddedBy        benchBuild         `json:"added_by"`
}
type benchItemset struct {
	SchemaVersion int        `json:"schema_version"`
	ID            string     `json:"id"`
	Origin        string     `json:"origin"`
	Spec          string     `json:"spec,omitempty"`
	OrchVersion   string     `json:"orch_version,omitempty"`
	Items         []gateItem `json:"items"`
}
type benchCapture struct {
	Subject benchSubject     `json:"subject"`
	Spec    benchSpecVersion `json:"spec"`
	Itemset benchItemset     `json:"itemset"`
	Text    string           `json:"spec_text"`
	Diff    string           `json:"diff,omitempty"`
}

func benchCanonicalItems(items []gateItem) ([]byte, error) {
	if items == nil {
		items = []gateItem{}
	}
	return json.Marshal(items)
}
func benchMakeItemset(origin, spec string, items []gateItem) benchItemset {
	if items == nil {
		items = []gateItem{}
	}
	b, _ := benchCanonicalItems(items)
	return benchItemset{SchemaVersion: 1, ID: benchHash(b)[:16], Origin: origin, Spec: spec, Items: items}
}
func benchSubjectID(s benchSubject) string { return subjectBenchID(s) }
func benchDescribe(s benchSubject, v *benchSpecVersion) string {
	commit := "commit not recorded"
	if s.Commit != "" {
		commit = "at " + benchPrefix(s.Commit, 7)
		if s.CommitBasis != "git" {
			commit += " (asserted by " + s.CommitBasis + ")"
		}
	}
	detail := []string{}
	if s.Branch != "" {
		detail = append(detail, s.Branch)
	}
	if s.CommittedAt != "" {
		detail = append(detail, benchPrefix(s.CommittedAt, 10))
	}
	if len(detail) > 0 {
		commit += " (" + strings.Join(detail, ", ") + ")"
	}
	dirty := "working tree state not recorded"
	switch s.Dirty.State {
	case "clean":
		dirty = "working tree clean"
	case "untracked-only":
		dirty = fmt.Sprintf("working tree clean except %d untracked paths", len(s.Dirty.Untracked))
	case "modified":
		dirty = "working tree with uncommitted changes"
	}
	spec := fmt.Sprintf("spec %s version %s (%s)", s.Spec.Path, s.Spec.Hash, s.Spec.State)
	if v != nil {
		builds := 0
		for _, p := range v.Packages {
			builds += p.Builds
		}
		spec += fmt.Sprintf(", %d lines, %d packages, %d Builds items", v.Lines, len(v.Packages), builds)
		if len(v.Commits) > 0 {
			spec += ", committed as " + benchPrefix(v.Commits[0].Commit, 9)
		}
		if v.VariantOf != "" {
			spec += "; variant " + v.VariantLabel + " of " + v.VariantOf
		}
	}
	return fmt.Sprintf("%s %s; %s; %s.", orDefault(s.Repo.Name, "repository unknown"), commit, dirty, spec)
}
func benchRepoInfo(code string) benchRepo { return subjectRepoInfo(code) }
func benchSpecFromText(text, path string, repo benchRepo, now time.Time) (benchSpecVersion, benchItemset) {
	sp := parseSpec(text)
	hash := specHash([]byte(text))
	items := []gateItem{}
	v := benchSpecVersion{SchemaVersion: 1, Hash: hash, SHA256: benchHash([]byte(text)), Bytes: len(text), Lines: strings.Count(text, "\n"), Path: filepath.ToSlash(path), Title: sp.title, Repo: repo, State: "unknown", AddedAt: now.UTC(), AddedBy: benchBuildNow(), FormatProblems: len(sp.problems)}
	if text != "" && !strings.HasSuffix(text, "\n") {
		v.Lines++
	}
	for _, p := range sp.packages {
		v.Packages = append(v.Packages, benchSpecPackage{ID: p.id, Title: p.title, Kind: p.kind, Builds: len(p.builds), ContentHash: benchHash([]byte(p.raw)), Owns: len(p.owns)})
		items = append(items, packageItems(p)...)
	}
	set := benchMakeItemset("parser", hash, items)
	set.OrchVersion = v.AddedBy.Version
	v.Itemset = set.ID
	return v, set
}
func benchGitText(repo, commit, path string) (string, error) {
	return checkoutGit(repo, "show", commit+":"+filepath.ToSlash(path))
}
func benchSpecHistory(repo string, v *benchSpecVersion, all bool) {
	args := []string{"log"}
	if all {
		args = append(args, "--all")
	}
	args = append(args, "--format=%H", "--", v.Path)
	log, e := git(repo, args...)
	if e != nil {
		return
	}
	matched := false
	for _, c := range strings.Fields(log) {
		text, e := benchGitText(repo, c, v.Path)
		if e != nil {
			continue
		}
		hash := specHash([]byte(text))
		if hash == v.Hash {
			matched = true
			at, _ := git(repo, "log", "-1", "--format=%cI", c)
			title, _ := git(repo, "log", "-1", "--format=%s", c)
			refs, _ := git(repo, "branch", "--format=%(refname:short)", "--contains", c)
			v.Commits = append(v.Commits, benchSpecCommit{Commit: c, CommittedAt: at, Subject: title, Refs: strings.Fields(refs)})
		} else if matched {
			v.Parent = &benchSpecParent{Hash: hash, Commit: c}
			break
		}
	}
}
func benchDirtyFromStamp(stamp checkoutStamp) benchDirty {
	d := benchDirty{State: "clean", DiffSHA256: stamp.diff}
	entries := strings.Split(stamp.status, "\x00")
	for i := 0; i < len(entries); i++ {
		ent := entries[i]
		if len(ent) < 4 {
			continue
		}
		if ent[:2] == "??" {
			d.Untracked = append(d.Untracked, ent[3:])
		} else {
			d.Modified = append(d.Modified, ent[3:])
			if strings.ContainsAny(ent[:2], "RC") {
				i++
			}
		}
	}
	sort.Strings(d.Untracked)
	sort.Strings(d.Modified)
	if len(d.Modified) > 0 {
		d.State = "modified"
	} else if len(d.Untracked) > 0 {
		d.State = "untracked-only"
	}
	return d
}
func captureSubject(code, specAbs string, src []byte, now time.Time) (benchSubject, benchSpecVersion, benchItemset, error) {
	stamp, err := checkoutFingerprint(code)
	return captureSubjectStamp(code, specAbs, src, now, stamp, err)
}
func captureSubjectStamp(code, specAbs string, src []byte, now time.Time, stamp checkoutStamp, stampErr error) (benchSubject, benchSpecVersion, benchItemset, error) {
	path, e := filepath.Rel(code, specAbs)
	if e != nil {
		path = specAbs
	}
	repo := benchRepoInfo(code)
	v, set := benchSpecFromText(string(src), path, repo, now)
	s := benchSubject{SchemaVersion: 1, Repo: repo, CommitBasis: "unknown", Dirty: benchDirty{State: "unknown"}, Spec: benchSpecRef{Hash: v.Hash, Path: v.Path, State: "unknown"}, CreatedAt: now.UTC(), CreatedBy: v.AddedBy}
	if stampErr == nil {
		s.Commit = strings.TrimSpace(stamp.head)
		s.CommitBasis = "git"
		s.Dirty = benchDirtyFromStamp(stamp)
		s.Branch, _ = git(code, "rev-parse", "--abbrev-ref", "HEAD")
		s.CommittedAt, _ = git(code, "log", "-1", "--format=%cI")
		head, he := benchGitText(code, "HEAD", v.Path)
		if he == nil && specHash([]byte(head)) == v.Hash {
			v.State = "committed"
			benchSpecHistory(code, &v, false)
		} else if _, e := git(code, "ls-files", "--error-unmatch", "--", v.Path); e == nil {
			v.State = "modified"
			if he == nil {
				v.CommittedHash = specHash([]byte(head))
			}
		} else {
			v.State = "untracked"
		}
		s.Spec.State = v.State
	}
	s.ID = benchSubjectID(s)
	s.Description = benchDescribe(s, &v)
	return s, v, set, nil
}
func benchSpecLoad(hash string) (*benchSpecVersion, error) {
	if !benchHex16.MatchString(hash) {
		return nil, fail("invalid spec id %q", hash)
	}
	var v benchSpecVersion
	if e := benchReadJSON(filepath.Join(benchDir(), "objects", "spec", hash+".json"), &v); e != nil {
		return nil, e
	}
	if v.Hash != hash || !benchHex64.MatchString(v.SHA256) {
		return nil, fail("invalid spec metadata: %s", hash)
	}
	return &v, nil
}
func benchSubjectLoad(id string) (*benchSubject, error) {
	if e := benchName(id); e != nil {
		return nil, e
	}
	var s benchSubject
	if e := benchReadJSON(filepath.Join(benchDir(), "subjects", id+".json"), &s); e != nil {
		return nil, e
	}
	if s.ID != id || benchSubjectID(s) != id || !benchHex16.MatchString(s.Spec.Hash) {
		return nil, fail("subject identity mismatch: %s", id)
	}
	return &s, nil
}
func benchSaveItemset(set benchItemset) error {
	path, e := benchObjectPath("itemset", set.ID)
	if e != nil {
		return e
	}
	if benchExists(path) {
		_, e := objectGet("itemset", set.ID)
		return e
	}
	return benchPutJSON("itemset", set.ID, set)
}
func benchSaveSpec(v benchSpecVersion, set benchItemset, text string) error {
	if v.Hash != specHash([]byte(text)) || v.SHA256 != benchHash([]byte(text)) {
		return fail("spec text hash mismatch: %s", v.Hash)
	}
	if len(text) > 2*1024*1024 {
		return fail("spec text exceeds 2 MB")
	}
	if e := benchSaveItemset(set); e != nil {
		return e
	}
	if e := objectPut("spec", v.Hash, []byte(text)); e != nil {
		return e
	}
	path := filepath.Join(benchDir(), "objects", "spec", v.Hash+".json")
	if old, e := benchSpecLoad(v.Hash); e == nil {
		if old.SHA256 != v.SHA256 {
			return fail("spec sha256 differs: %s", v.Hash)
		}
		if v.VariantOf != "" && (old.VariantOf != v.VariantOf || old.VariantLabel != v.VariantLabel) {
			return fail("immutable spec variant differs: %s", v.Hash)
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	return benchImmutableJSON(path, v)
}
func benchSaveSubject(s benchSubject) error {
	if e := benchName(s.ID); e != nil {
		return e
	}
	if s.ID != benchSubjectID(s) {
		return fail("subject identity mismatch: %s", s.ID)
	}
	path := filepath.Join(benchDir(), "subjects", s.ID+".json")
	if old, e := benchSubjectLoad(s.ID); e == nil {
		a, b := *old, s
		a.CreatedAt = b.CreatedAt
		a.CreatedBy = b.CreatedBy
		a.Description = b.Description
		if !reflect.DeepEqual(a, b) {
			return fail("immutable subject facts differ for %s; subject id collision requires an explicit decision", s.ID)
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	return benchImmutableJSON(path, s)
}
func benchWriteGateSubject(runDir, code, path, text string, now time.Time, stamp checkoutStamp, stampErr error) error {
	s, v, set, e := captureSubjectStamp(code, path, []byte(text), now, stamp, stampErr)
	if e != nil {
		return e
	}
	c := benchCapture{Subject: s, Spec: v, Itemset: set, Text: text}
	if s.Dirty.State == "modified" {
		diff, e := checkoutGit(code, "diff", "HEAD", "--binary", "--no-ext-diff", "--no-textconv")
		if e == nil {
			c.Subject.Dirty.DiffBytes = len(diff)
			if len(diff) <= 256*1024 {
				c.Diff = diff
			}
		}
	}
	b, e := json.MarshalIndent(c, "", "  ")
	if e != nil {
		return e
	}
	if e := os.MkdirAll(runDir, 0700); e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(runDir, "subject.json"), append(b, '\n'), 0600)
}
