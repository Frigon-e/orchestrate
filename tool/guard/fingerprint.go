package guard

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Fingerprint identifies the source of orch-guard: the non-test Go files of
// guard and cmd/orch-guard, go.mod and go.sum, under the tool directory. It is
// the SHA-256 of one "<sha256>  <path>" line per file, paths relative to the
// tool directory in byte order, which is what `shasum -a 256` prints, so
// scripts/build-guard computes the same value.
func Fingerprint(toolDir string) (string, error) {
	var files []string
	for _, pattern := range []string{"guard/*.go", "cmd/orch-guard/*.go", "go.mod", "go.sum"} {
		matches, err := filepath.Glob(filepath.Join(toolDir, pattern))
		if err != nil {
			return "", err
		}
		for _, m := range matches {
			if rel, err := filepath.Rel(toolDir, m); err == nil && !strings.HasSuffix(rel, "_test.go") {
				files = append(files, filepath.ToSlash(rel))
			}
		}
	}
	sort.Strings(files)
	var sums strings.Builder
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(toolDir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), rel)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(sums.String()))), nil
}
