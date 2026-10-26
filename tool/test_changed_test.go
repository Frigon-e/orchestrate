package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tcRepo builds a temp git repo with the tool layout and one commit on main.
func tcRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, filepath.FromSlash(toolRel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		tcWrite(t, dir, name, body)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base", "--allow-empty"},
		{"checkout", "-q", "-b", "feature"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func tcWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const tcBase = "package main\n\n"

func tcFixture() map[string]string {
	return map[string]string{
		"alpha.go":           tcBase + "func Alpha() int { return 1 }\n",
		"alpha_test.go":      tcBase + "func TestAlpha(t *testing.T) { Alpha() }\n",
		"alpha_more_test.go": tcBase + "func TestAlphaMore(t *testing.T) {}\nfunc TestAlphaMore2(t *testing.T) {}\n",
		"beta.go":            tcBase + "func Beta() int { return 2 }\n",
		"beta_test.go":       tcBase + "func TestBeta(t *testing.T) { Beta() }\n",
		"user_test.go":       tcBase + "func TestUsesBeta(t *testing.T) { _ = Beta() }\nfunc TestUnrelated(t *testing.T) {}\n",
		"alphabet_test.go":   tcBase + "func TestAlphabet(t *testing.T) {}\n",
	}
}

func TestTestChangedNonTestFileSelectsStemTests(t *testing.T) {
	dir := tcRepo(t, tcFixture())
	tcWrite(t, dir, "alpha.go", tcBase+"var x = 1\nfunc Alpha() int { return 1 }\n")
	got, err := selectChangedTests(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"TestAlpha", "TestAlphaMore", "TestAlphaMore2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (alphabet_test.go must not match stem alpha)", got, want)
	}
}

func TestTestChangedTestFileSelectsOwnTests(t *testing.T) {
	dir := tcRepo(t, tcFixture())
	tcWrite(t, dir, "user_test.go", tcBase+"func TestUsesBeta(t *testing.T) {}\nfunc TestUnrelated(t *testing.T) {}\nfunc TestNew(t *testing.T) {}\n")
	got, err := selectChangedTests(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"TestNew", "TestUnrelated", "TestUsesBeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTestChangedChangedDeclPullsReferencingTest(t *testing.T) {
	dir := tcRepo(t, tcFixture())
	tcWrite(t, dir, "beta.go", tcBase+"func Beta() int { return 3 }\n")
	got, err := selectChangedTests(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"TestBeta", "TestUnrelated", "TestUsesBeta"}
	// user_test.go references Beta, so all of its tests come with it.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTestChangedCommittedStagedAndUntracked(t *testing.T) {
	dir := tcRepo(t, tcFixture())
	tcWrite(t, dir, "alpha.go", tcBase+"func Alpha() int { return 5 }\n")
	root := dir
	for range strings.Split(toolRel, "/") {
		root = filepath.Dir(root)
	}
	for _, args := range [][]string{
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-am", "alpha"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v\n%s", err, out)
		}
	}
	tcWrite(t, dir, "gamma.go", tcBase+"func Gamma() {}\n")
	tcWrite(t, dir, "gamma_test.go", tcBase+"func TestGamma(t *testing.T) {}\n")
	got, err := selectChangedTests(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"TestAlpha", "TestGamma"} {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Fatalf("missing %s in %v", w, got)
		}
	}
}

func TestTestChangedPatternAnchoredAndDeduplicated(t *testing.T) {
	dir := tcRepo(t, tcFixture())
	// Both the stem rule and the reference rule select TestBeta.
	tcWrite(t, dir, "beta.go", tcBase+"func Beta() int { return 4 }\n")
	tcWrite(t, dir, "beta_test.go", tcBase+"func TestBeta(t *testing.T) { Beta() }\n")
	got, err := selectChangedTests(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(got, " "), "TestBeta "); n != 1 {
		t.Fatalf("TestBeta appears %d times in %v", n, got)
	}
	p := runPattern(got)
	if !strings.HasPrefix(p, "^(") || !strings.HasSuffix(p, ")$") {
		t.Fatalf("pattern not anchored: %s", p)
	}
	if p != "^(TestBeta|TestUnrelated|TestUsesBeta)$" {
		t.Fatalf("pattern %s", p)
	}
}

func TestTestChangedEmptyDiffSelectsNothing(t *testing.T) {
	dir := tcRepo(t, tcFixture())
	got, err := selectChangedTests(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no tests, got %v", got)
	}
}

func TestTestChangedIdents(t *testing.T) {
	diff := "-func (r *T) Old() {\n+func (r T) New() {\n+type Thing struct{}\n+\tx := 1\n+func New() {}\n"
	got := changedIdents(diff)
	want := []string{"New", "Old", "Thing"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
