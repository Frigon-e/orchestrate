package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// repositoryIdentity keeps both the checkout and common Git directory. Linked
// worktrees share storage, but are not interchangeable approval subjects.
type repositoryIdentity struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Origin    string `json:"origin"`
	CommonDir string `json:"common_dir"`
}

func subjectRepoInfo(code string) repositoryIdentity {
	r := repositoryIdentity{Path: code}
	if code == "" {
		return r
	}
	r.Name = filepath.Base(code)
	r.Origin, _ = git(code, "remote", "get-url", "origin")
	r.CommonDir, _ = git(code, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if r.CommonDir != "" {
		if filepath.Base(r.CommonDir) == ".git" {
			r.Name = filepath.Base(filepath.Dir(r.CommonDir))
		} else {
			r.Name = strings.TrimSuffix(filepath.Base(r.CommonDir), ".git")
		}
	}
	return r
}

// Historical benchmark IDs retain their exact format. Approval uses the full
// subject digest below instead of these short, descriptive archive keys.
func subjectBenchID(s benchSubject) string {
	id := "s-" + s.Spec.Hash + "-" + orDefault(benchPrefix(s.Commit, 7), "nocommit")
	if s.Dirty.State == "modified" {
		id += "-dirty"
	}
	dirty := s.Dirty
	dirty.DiffBlob, dirty.DiffBytes = "", 0
	dirty.Untracked = append([]string{}, dirty.Untracked...)
	dirty.Modified = append([]string{}, dirty.Modified...)
	sort.Strings(dirty.Untracked)
	sort.Strings(dirty.Modified)
	facts, _ := json.Marshal(struct {
		Repo                        repositoryIdentity
		Dirty                       benchDirty
		CommitBasis, CommitEvidence string
	}{s.Repo, dirty, s.CommitBasis, s.CommitEvidence})
	return id + "-" + subjectDigest(facts)[:6]
}

func subjectDigest(b []byte) string  { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func subjectJSONDigest(v any) string { b, _ := json.Marshal(v); return subjectDigest(b) }

type checkoutStamp struct {
	status, diff, head, tree string
	files                    map[string]string
}

func checkoutGit(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("checkout check: git %s: %w", args[0], err)
	}
	return string(out), nil
}

// Only declared runtime state may be excluded in addition to Git's ignored
// untracked paths. The exclusion itself is part of the approval subject.
func subjectRuntimePath(root string) (string, error) {
	home := os.Getenv("ORCH_HOME")
	if home == "" {
		return "", nil
	}
	home, err := filepath.Abs(expandHome(home))
	if err != nil {
		return "", err
	}
	if real, e := filepath.EvalSymlinks(home); e == nil {
		home = real
	}
	rel, err := filepath.Rel(root, home)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", fmt.Errorf("ORCH_HOME cannot exclude the entire reviewed repository")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", nil
	}
	return filepath.ToSlash(rel), nil
}

func checkoutFingerprint(root string) (checkoutStamp, error) {
	s := checkoutStamp{files: map[string]string{}}
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return s, err
	}
	runtime, err := subjectRuntimePath(root)
	if err != nil {
		return s, err
	}
	paths := []string{"--", ":/"}
	if runtime != "" {
		paths = append(paths, ":(top,literal,exclude)"+runtime)
	}
	if s.head, err = checkoutGit(root, "rev-parse", "HEAD"); err != nil {
		return s, err
	}
	if s.tree, err = checkoutGit(root, "rev-parse", "HEAD^{tree}"); err != nil {
		return s, err
	}
	if s.status, err = checkoutGit(root, append([]string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, paths...)...); err != nil {
		return s, err
	}
	diff, err := checkoutGit(root, append([]string{"diff", "HEAD", "--binary", "--no-ext-diff", "--no-textconv"}, paths...)...)
	if err != nil {
		return s, err
	}
	s.diff = subjectDigest([]byte(diff))
	names, err := checkoutGit(root, append([]string{"ls-files", "--cached", "--others", "--exclude-standard", "-z"}, paths...)...)
	if err != nil {
		return s, err
	}
	// Stream every included file, not just porcelain names. This also detects
	// unreadable clean files, modes with core.filemode=false, and assume-unchanged
	// content. The record stores only a digest, not a copy of the source tree.
	pathsToStamp, err := checkoutSourcePaths(root, names, runtime, paths)
	if err != nil {
		return s, err
	}
	for _, path := range pathsToStamp {
		if path == "" {
			continue
		}
		value, err := checkoutPathStamp(root, path)
		if err != nil {
			return s, fmt.Errorf("checkout fingerprint %q: %w", path, err)
		}
		s.files[path] = value
	}
	return s, nil
}

// Git omits unversionable special files (for example FIFOs) from ls-files.
// Walk the nonignored source set as well, so these cannot silently bypass the
// fail-closed check. Ignored directory entries let us prune caches without
// traversing their contents. Tracked paths remain included even if ignored.
func checkoutSourcePaths(root, names, runtime string, pathspec []string) ([]string, error) {
	included := map[string]bool{}
	ancestors := map[string]bool{}
	for _, path := range strings.Split(names, "\x00") {
		if path == "" {
			continue
		}
		included[path] = true
		for parent := filepath.ToSlash(filepath.Dir(path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			ancestors[parent] = true
		}
	}
	ignoredText, err := checkoutGit(root, append([]string{"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z"}, pathspec...)...)
	if err != nil {
		return nil, err
	}
	ignored := map[string]bool{}
	for _, path := range strings.Split(ignoredText, "\x00") {
		if path != "" {
			ignored[strings.TrimSuffix(path, "/")] = true
		}
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return walkErr
		}
		isIgnored := false
		for parent := rel; parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			if ignored[parent] {
				isIgnored = true
				break
			}
		}
		if rel == ".git" || rel == runtime || (isIgnored && !included[rel] && !ancestors[rel]) {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if walkErr != nil {
			return fmt.Errorf("checkout fingerprint %q: %w", rel, walkErr)
		}
		if !d.IsDir() {
			// ls-files --ignored also omits special files. Ask Git's ignore
			// rules directly for entries absent from both enumerations.
			if !included[rel] {
				_, err := checkoutGit(root, "check-ignore", "--quiet", "--", rel)
				if err == nil {
					return nil
				}
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					return err
				}
			}
			included[rel] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(included))
	for path := range included {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

// No symlink is followed. Deletions have a distinct stamp; unreadable or
// unsupported paths fail closed instead of becoming a reusable marker.
func checkoutPathStamp(root, path string) (string, error) {
	// A replaced parent directory may itself be a symlink. Stamp that target
	// instead of following it to an unrelated file outside the checkout.
	for parent := filepath.Dir(filepath.FromSlash(path)); parent != "."; parent = filepath.Dir(parent) {
		fi, err := os.Lstat(filepath.Join(root, parent))
		if os.IsNotExist(err) {
			return "deleted", nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filepath.Join(root, parent))
			return fmt.Sprintf("ancestor-link:%s:%o:%s", parent, fi.Mode(), target), err
		}
	}
	full := filepath.Join(root, filepath.FromSlash(path))
	fi, err := os.Lstat(full)
	if os.IsNotExist(err) {
		return "deleted", nil
	}
	if err != nil {
		return "", err
	}
	mode := fmt.Sprintf("%o:", fi.Mode())
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		return mode + "link:" + target, err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("unsupported file mode %s", fi.Mode())
	}
	f, err := os.Open(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return mode + fmt.Sprintf("%x", hash.Sum(nil)), nil
}

const gateSubjectVersion = 1
const gateReviewPolicyVersion = "complete-review-v1"

type gateSubject struct {
	Version     int                `json:"version"`
	ID          string             `json:"id"`
	Repo        repositoryIdentity `json:"repo"`
	SpecSHA256  string             `json:"spec_sha256"`
	Commit      string             `json:"commit"`
	Tree        string             `json:"tree"`
	Fingerprint string             `json:"fingerprint"`
	RuntimePath string             `json:"runtime_path,omitempty"`
}

type gateReviewPolicy struct {
	Version      string `json:"version"`
	CtxTool      string `json:"ctx_tool"`
	ContextFiles string `json:"context_files"`
	CtxShell     string `json:"ctx_shell"`
	CtxProfile   string `json:"ctx_profile"`
}

func (s gateSubject) digest() string { s.ID = ""; return subjectJSONDigest(s) }
func (s *gateSubject) valid() bool {
	return s != nil && s.Version == gateSubjectVersion && s.Repo.Path != "" && s.Repo.CommonDir != "" && len(s.SpecSHA256) == 64 && s.Commit != "" && s.Tree != "" && len(s.Fingerprint) == 64 && s.ID == s.digest()
}

func gateSubjectFromStamp(code string, src []byte, stamp checkoutStamp) (*gateSubject, error) {
	code, err := filepath.EvalSymlinks(code)
	if err != nil {
		return nil, err
	}
	runtime, err := subjectRuntimePath(code)
	if err != nil {
		return nil, err
	}
	repo := subjectRepoInfo(code)
	if repo.CommonDir == "" {
		return nil, fmt.Errorf("repository identity could not be read")
	}
	repo.CommonDir, err = filepath.EvalSymlinks(repo.CommonDir)
	if err != nil {
		return nil, err
	}
	s := &gateSubject{Version: gateSubjectVersion, Repo: repo, SpecSHA256: subjectDigest(src), Commit: strings.TrimSpace(stamp.head), Tree: strings.TrimSpace(stamp.tree), RuntimePath: runtime,
		Fingerprint: subjectJSONDigest(struct {
			Status, Diff string
			Files        map[string]string
		}{stamp.status, stamp.diff, stamp.files})}
	s.ID = s.digest()
	return s, nil
}

func captureGateSubject(code string, src []byte) (*gateSubject, error) {
	stamp, err := checkoutFingerprint(code)
	if err != nil {
		return nil, err
	}
	return gateSubjectFromStamp(code, src, stamp)
}

func gateSubjectChanges(before, after *gateSubject) []string {
	var changed []string
	if before.Repo != after.Repo {
		changed = append(changed, "repository/checkout identity")
	}
	if before.SpecSHA256 != after.SpecSHA256 {
		changed = append(changed, "spec content")
	}
	if before.Commit != after.Commit {
		changed = append(changed, "commit")
	}
	if before.Tree != after.Tree {
		changed = append(changed, "committed tree")
	}
	if before.Fingerprint != after.Fingerprint {
		changed = append(changed, "checkout content/modes (including dirty and untracked paths)")
	}
	if before.RuntimePath != after.RuntimePath {
		changed = append(changed, "runtime exclusion")
	}
	return changed
}

// Readable legacy records remain history, never evidence of a reviewed subject.
func gateApproval(root, code string, src []byte) (bool, string) {
	b, err := os.ReadFile(filepath.Join(gateDir(root, specHash(src)), "gate.json"))
	if err != nil {
		return false, "no readable approval"
	}
	var r gateRecord
	if json.Unmarshal(b, &r) != nil {
		return false, "no readable approval"
	}
	if !r.Subject.valid() || r.ReviewPolicy == nil || r.ReviewPolicy.Version != gateReviewPolicyVersion {
		return false, "unbound or legacy approval; a fresh review is required"
	}
	if r.SchemaVersion != gateStatusSchemaVersion || r.State != gateStatePass || r.Review == nil {
		return false, "recorded gate is not an accepted reducer pass; a fresh review is required"
	}
	now, err := captureGateSubject(code, src)
	if err != nil {
		return false, "subject unverified: " + err.Error()
	}
	if changed := gateSubjectChanges(r.Subject, now); len(changed) > 0 {
		return false, "stale approval: " + strings.Join(changed, ", ") + " changed; a fresh review is required"
	}
	policy := r.Review.Policy
	policy.CurrentSubject = now
	// Revalidate our full-file source observations at the approval boundary.
	// Ignored sources and symlink targets may change without a checkout-ID change.
	// Other producers retain their own declared hash/range basis; never guess it.
	hashes := make(map[string]string, len(policy.CurrentHashes))
	maps.Copy(hashes, policy.CurrentHashes)
	sourceReads := map[string]bool{}
	for _, lookup := range policy.Packet.Lookups {
		if lookup.Unit == "source_read" {
			sourceReads[lookup.ID] = true
		}
	}
	for _, e := range policy.Packet.Evidence {
		if e.SourceKind == evidenceSourceCode && e.Range == nil && sourceReads[e.LookupID] {
			hashes[e.ID] = ""
			if bytes, err := os.ReadFile(filepath.Join(code, e.Path)); err == nil {
				hashes[e.ID] = subjectDigest(bytes)
			}
		}
	}
	policy.CurrentHashes = hashes
	result := ReduceReview(r.Subject, r.Review.Checks, r.Review.Answers, r.Review.Findings, policy)
	if result.State != gateStatePass || result.Completion.Required == 0 {
		return false, "recorded review does not reduce to pass; a fresh review is required"
	}
	return true, ""
}

func finishGateSubject(rec *gateRecord, code, specPath string) {
	if !rec.Subject.valid() {
		return
	}
	current, err := os.ReadFile(specPath)
	if err == nil {
		var now *gateSubject
		now, err = captureGateSubject(code, current)
		if err == nil {
			if changed := gateSubjectChanges(rec.Subject, now); len(changed) > 0 {
				rec.SubjectStale = changed
				rec.Reasons = append(rec.Reasons, "review subject changed: "+strings.Join(changed, ", "))
			}
		}
	}
	if err != nil {
		rec.TreeCheckError = err.Error()
		rec.Reasons = append(rec.Reasons, "subject unverified: "+err.Error())
	}
}
