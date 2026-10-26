package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

func benchWithLock(path string, fn func() error) error {
	if e := benchSafe(path); e != nil {
		return e
	}
	if e := benchMkdir(filepath.Dir(path)); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	if e := f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if e := f.Close(); e != nil {
		return e
	}
	return withFlock(path, fn)
}

func benchPrefix(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

var benchNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func benchName(s string) error {
	if !benchNameRE.MatchString(s) || strings.Contains(s, "..") {
		return fail("invalid benchmark name %q: use 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit; '..' is forbidden", s)
	}
	return nil
}
func benchSuiteName(s string) error {
	if err := benchName(s); err != nil {
		return err
	}
	for _, reserved := range []string{"objects", "subjects", "store.json", "README.md"} {
		if strings.EqualFold(s, reserved) {
			return fail("reserved benchmark suite name %q", s)
		}
	}
	return nil
}

// Check the nearest existing parent as well as the lexical path. This covers
// new files below an existing link without following links out of the store.
func benchSafe(path string) error {
	root, err := filepath.Abs(benchDir())
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	inside := func(base, p string) bool {
		rel, e := filepath.Rel(base, p)
		return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	if !inside(root, path) {
		return fail("benchmark path outside store: %s", path)
	}
	resolve := func(p string) (string, error) {
		var suffix []string
		for {
			real, e := filepath.EvalSymlinks(p)
			if e == nil {
				for _, s := range slices.Backward(suffix) {
					real = filepath.Join(real, s)
				}
				return real, nil
			}
			if !os.IsNotExist(e) {
				return "", e
			}
			if _, e := os.Lstat(p); e == nil {
				return "", fail("unresolvable benchmark symlink: %s", p)
			}
			parent := filepath.Dir(p)
			if parent == p {
				return "", e
			}
			suffix = append(suffix, filepath.Base(p))
			p = parent
		}
	}
	realRoot, err := resolve(root)
	if err != nil {
		return err
	}
	realPath, err := resolve(path)
	if err != nil {
		return err
	}
	if !inside(realRoot, realPath) {
		return fail("benchmark symlink escapes store: %s", path)
	}
	return nil
}
func benchMkdir(path string) error {
	if e := benchSafe(path); e != nil {
		return e
	}
	return os.MkdirAll(path, 0700)
}
func benchWrite(path string, b []byte) error {
	if e := benchSafe(path); e != nil {
		return e
	}
	if e := benchMkdir(filepath.Dir(path)); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".write-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = benchSafe(path); e != nil {
		return e
	}
	return os.Rename(name, path)
}
func benchJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return benchWrite(path, append(b, '\n'))
}
func benchReadJSON(path string, v any) error {
	if e := benchSafe(path); e != nil {
		return e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func benchImmutable(path string, b []byte) error {
	if e := benchSafe(path); e != nil {
		return e
	}
	if e := benchMkdir(filepath.Dir(path)); e != nil {
		return e
	}
	if old, e := os.ReadFile(path); e == nil {
		if string(old) != string(b) {
			return fail("immutable benchmark object differs: %s", path)
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	// A hard link publishes the complete file without replacing a concurrent writer.
	f, e := os.CreateTemp(filepath.Dir(path), ".object-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Link(f.Name(), path); os.IsExist(e) {
		old, re := os.ReadFile(path)
		if re != nil {
			return re
		}
		if string(old) != string(b) {
			return fmt.Errorf("immutable benchmark object differs: %s", path)
		}
		return nil
	}
	return e
}
func benchImmutableJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return benchImmutable(path, append(b, '\n'))
}
func benchSuiteCheck(s benchSuite) error {
	for arm := range s.Arms {
		if e := benchName(arm); e != nil {
			return e
		}
	}
	if s.ReferenceKey != "" {
		if e := benchKeyName(s.ReferenceKey); e != nil {
			return e
		}
	}
	if s.Subject.Default != "" {
		if e := benchName(s.Subject.Default); e != nil {
			return e
		}
	}
	return nil
}

func benchRunValidate(r *benchRun, suite, id string) error {
	if r.SchemaVersion != 1 && r.SchemaVersion != 2 {
		return fail("unsupported run schema %d", r.SchemaVersion)
	}
	for _, n := range []string{r.Suite, r.ID, r.Label, r.Arm} {
		if e := benchName(n); e != nil {
			return e
		}
	}
	if r.ID != id || r.Suite != suite {
		return fail("benchmark record identity differs from its path: %s/%s", suite, id)
	}
	if r.Subject != "" {
		if e := benchName(r.Subject); e != nil {
			return e
		}
	}
	for k := range r.Scores {
		if e := benchKeyName(k); e != nil {
			return e
		}
	}
	for k, id := range r.KeySubjects {
		if e := benchKeyName(k); e != nil {
			return e
		}
		if e := benchName(id); e != nil {
			return e
		}
	}
	for _, move := range r.SubjectHistory {
		for _, id := range []string{move.From, move.To} {
			if e := benchName(id); e != nil {
				return e
			}
		}
	}
	return nil
}
func benchKeyName(s string) error {
	if e := benchName(s); e != nil {
		return e
	}
	if !regexp.MustCompile(`^key-[1-9][0-9]*$`).MatchString(s) {
		return fail("invalid key id %q", s)
	}
	return nil
}
func benchSuiteDirs() ([]string, error) {
	if e := benchSafe(benchDir()); e != nil {
		return nil, e
	}
	ds, e := os.ReadDir(benchDir())
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var out []string
	for _, d := range ds {
		if !d.IsDir() || benchSuiteName(d.Name()) != nil {
			continue
		}
		p := filepath.Join(benchDir(), d.Name(), "suite.toml")
		if e := benchSafe(p); e != nil {
			return nil, e
		}
		if benchExists(p) {
			out = append(out, d.Name())
		}
	}
	return out, nil
}
