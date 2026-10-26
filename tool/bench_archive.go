package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const benchArchiveLimit = 512 * 1024 * 1024

func benchArchiveFiles(suite string) (map[string][]byte, error) {
	if e := benchSuiteName(suite); e != nil {
		return nil, e
	}
	files := map[string][]byte{}
	var totalSize int64
	add := func(rel string) error {
		if _, ok := files[rel]; ok {
			return nil
		}
		path := filepath.Join(benchDir(), filepath.FromSlash(rel))
		if e := benchSafe(path); e != nil {
			return e
		}
		fi, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !fi.Mode().IsRegular() {
			return fail("non-regular archive source: %s", path)
		}
		if fi.Size() > benchArchiveLimit-totalSize {
			return fail("benchmark export exceeds 512 MB")
		}
		totalSize += fi.Size()
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		files[rel] = b
		return nil
	}
	root := filepath.Join(benchDir(), suite)
	if e := benchSafe(root); e != nil {
		return nil, e
	}
	if e := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fail("symlink in export: %s", path)
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, _ := filepath.Rel(benchDir(), path)
		return add(filepath.ToSlash(rel))
	}); e != nil {
		return nil, e
	}
	var spec func(string) error
	itemset := func(id string) error {
		if id == "" {
			return nil
		}
		_, e := objectGet("itemset", id)
		if e != nil {
			return e
		}
		rel := "objects/itemset/" + id + ".json"
		if _, ok := files[rel]; ok {
			return nil
		}
		if e := add(rel); e != nil {
			return e
		}
		var set benchItemset
		if e := json.Unmarshal(files[rel], &set); e != nil {
			return e
		}
		if set.Spec != "" {
			return spec(set.Spec)
		}
		return nil
	}
	blob := func(id string) error {
		if id == "" {
			return nil
		}
		if _, e := objectGet("blob", id); e != nil {
			return e
		}
		if e := add("objects/blob/" + id); e != nil {
			return e
		}
		meta := "objects/blob/" + id + ".meta.json"
		if benchExists(filepath.Join(benchDir(), meta)) {
			return add(meta)
		}
		return nil
	}
	spec = func(id string) error {
		if _, ok := files["objects/spec/"+id+".md"]; ok {
			return nil
		}
		if _, e := objectGet("spec", id); e != nil {
			return e
		}
		if e := add("objects/spec/" + id + ".md"); e != nil {
			return e
		}
		if e := add("objects/spec/" + id + ".json"); e != nil {
			return e
		}
		v, e := benchSpecLoad(id)
		if e != nil {
			return e
		}
		if e := itemset(v.Itemset); e != nil {
			return e
		}
		if e := blob(v.NotesBlob); e != nil {
			return e
		}
		if v.VariantOf != "" {
			if e := spec(v.VariantOf); e != nil {
				return e
			}
		}
		if v.Parent != nil && objectHas("spec", v.Parent.Hash) {
			return spec(v.Parent.Hash)
		}
		return nil
	}
	subject := func(id string) error {
		s, e := benchSubjectLoad(id)
		if e != nil {
			return e
		}
		if e := add("subjects/" + id + ".json"); e != nil {
			return e
		}
		if e := blob(s.Dirty.DiffBlob); e != nil {
			return e
		}
		if s.Spec.State == "unknown" && !objectHas("spec", s.Spec.Hash) {
			return nil
		}
		return spec(s.Spec.Hash)
	}
	runs, e := benchRuns(suite)
	if e != nil {
		return nil, e
	}
	for _, r := range runs {
		if r.Subject == "" {
			return nil, fail("migrate run %s before export", r.ID)
		}
		if e := subject(r.Subject); e != nil {
			return nil, e
		}
		if e := itemset(r.Itemset); e != nil {
			return nil, e
		}
		for name, ids := range r.Blobs {
			if name == "prompts" && r.PromptsStored != nil && !*r.PromptsStored {
				continue
			}
			for _, id := range ids {
				if e := blob(id); e != nil {
					return nil, e
				}
			}
		}
		for _, move := range r.SubjectHistory {
			if e := subject(move.From); e != nil {
				return nil, e
			}
			if e := subject(move.To); e != nil {
				return nil, e
			}
		}
		for _, id := range r.KeySubjects {
			if e := subject(id); e != nil {
				return nil, e
			}
		}
	}
	keys, e := benchKeys(suite)
	if e != nil {
		return nil, e
	}
	for _, k := range keys {
		if e := subject(k.Subject); e != nil {
			return nil, e
		}
		if e := itemset(k.Itemset); e != nil {
			return nil, e
		}
		for _, id := range k.PromptBlobs {
			if e := blob(id); e != nil {
				return nil, e
			}
		}
	}
	s, e := benchSuiteLoad(suite)
	if e != nil {
		return nil, e
	}
	if s.Subject.Default != "" {
		if e := subject(s.Subject.Default); e != nil {
			return nil, e
		}
	}
	return files, nil
}
func benchArchiveMember(name string) error {
	if name == "" || strings.Contains(name, "\\") || filepath.IsAbs(name) || filepath.ToSlash(filepath.Clean(name)) != name {
		return fail("unsafe archive member %q", name)
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsRune(p, 0) {
			return fail("unsafe archive component %q", p)
		}
	}
	if parts[0] == "objects" {
		if len(parts) != 3 || (parts[1] != "spec" && parts[1] != "itemset" && parts[1] != "blob") {
			return fail("invalid object archive path %q", name)
		}
		return nil
	}
	if parts[0] == "subjects" {
		if len(parts) != 2 || !strings.HasSuffix(parts[1], ".json") {
			return fail("invalid subject archive path")
		}
		return nil
	}
	if e := benchSuiteName(parts[0]); e != nil {
		return e
	}
	if len(parts) < 2 {
		return fail("invalid suite archive path")
	}
	if parts[1] != "suite.toml" && parts[1] != "keys" && parts[1] != "runs" && parts[1] != "reports" {
		return fail("unknown suite archive path %q", name)
	}
	return nil
}
func benchArchiveValidate(files map[string][]byte) error {
	get := func(path string) ([]byte, error) {
		b, ok := files[path]
		if !ok {
			return nil, fail("archive reference missing: %s", path)
		}
		return b, nil
	}
	object := func(kind, id string) error {
		ext := ""
		if kind == "spec" {
			ext = ".md"
			if !benchHex16.MatchString(id) {
				return fail("invalid spec id")
			}
		}
		if kind == "itemset" {
			ext = ".json"
			if !benchHex16.MatchString(id) {
				return fail("invalid itemset id")
			}
		}
		if kind == "blob" && !benchHex64.MatchString(id) {
			return fail("invalid blob id")
		}
		b, e := get("objects/" + kind + "/" + id + ext)
		if e != nil {
			return e
		}
		return benchObjectCheck(kind, id, b)
	}
	subject := func(id string) error {
		if e := benchName(id); e != nil {
			return e
		}
		b, e := get("subjects/" + id + ".json")
		if e != nil {
			return e
		}
		var s benchSubject
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		if s.ID != id || benchSubjectID(s) != id || !benchHex16.MatchString(s.Spec.Hash) {
			return fail("archive subject identity mismatch")
		}
		if s.Dirty.DiffBlob != "" {
			if e := object("blob", s.Dirty.DiffBlob); e != nil {
				return e
			}
		}
		if _, ok := files["objects/spec/"+s.Spec.Hash+".md"]; s.Spec.State != "unknown" || ok {
			return object("spec", s.Spec.Hash)
		}
		return nil
	}
	suites := map[string]bool{}
	for name := range files {
		if e := benchArchiveMember(name); e != nil {
			return e
		}
		part, _, _ := strings.Cut(name, "/")
		if part != "objects" && part != "subjects" {
			suites[part] = true
		}
	}
	if len(suites) != 1 {
		return fail("benchmark archive must contain exactly one suite")
	}
	for suite := range suites {
		if _, e := get(suite + "/suite.toml"); e != nil {
			return e
		}
	}
	for name, data := range files {
		if e := benchArchiveMember(name); e != nil {
			return e
		}
		parts := strings.Split(name, "/")
		if parts[0] == "objects" {
			switch parts[1] {
			case "spec":
				{
					id := strings.TrimSuffix(strings.TrimSuffix(parts[2], ".md"), ".json")
					if e := object("spec", id); e != nil {
						return e
					}
					meta, e := get("objects/spec/" + id + ".json")
					if e != nil {
						return e
					}
					var v benchSpecVersion
					if e := json.Unmarshal(meta, &v); e != nil {
						return e
					}
					text, e := get("objects/spec/" + id + ".md")
					if e != nil {
						return e
					}
					if v.Hash != id || v.SHA256 != benchHash(text) {
						return fail("archive spec sha256 mismatch")
					}
					if e := object("itemset", v.Itemset); e != nil {
						return e
					}
					if v.VariantOf != "" {
						if e := object("spec", v.VariantOf); e != nil {
							return e
						}
					}
					if v.NotesBlob != "" {
						if e := object("blob", v.NotesBlob); e != nil {
							return e
						}
					}
				}
			case "itemset":
				id := strings.TrimSuffix(parts[2], ".json")
				if e := object("itemset", id); e != nil {
					return e
				}
				var set benchItemset
				if e := json.Unmarshal(data, &set); e != nil {
					return e
				}
				if set.Spec != "" {
					if e := object("spec", set.Spec); e != nil {
						return e
					}
				}
			case "blob":
				if strings.HasSuffix(name, ".meta.json") {
					if e := object("blob", strings.TrimSuffix(parts[2], ".meta.json")); e != nil {
						return e
					}
				} else {
					if e := object("blob", parts[2]); e != nil {
						return e
					}
				}
			}
			continue
		}
		if parts[0] == "subjects" {
			if e := subject(strings.TrimSuffix(parts[1], ".json")); e != nil {
				return e
			}
			continue
		}
		if len(parts) == 2 && parts[1] == "suite.toml" {
			var s benchSuite
			if _, e := toml.Decode(string(data), &s); e != nil {
				return e
			}
			for arm := range s.Arms {
				if e := benchName(arm); e != nil {
					return e
				}
			}
			if s.Subject.Default != "" {
				if e := subject(s.Subject.Default); e != nil {
					return e
				}
			}
			if s.ReferenceKey != "" {
				if e := benchKeyName(s.ReferenceKey); e != nil {
					return e
				}
				if _, e := get(parts[0] + "/keys/" + s.ReferenceKey + ".json"); e != nil {
					return e
				}
			}
		}
		if len(parts) == 4 && parts[1] == "runs" && parts[3] == "run.json" {
			var r benchRun
			if e := json.Unmarshal(data, &r); e != nil {
				return e
			}
			if e := benchRunValidate(&r, parts[0], parts[2]); e != nil {
				return e
			}
			if e := subject(r.Subject); e != nil {
				return e
			}
			subjectBytes, _ := get("subjects/" + r.Subject + ".json")
			var bound benchSubject
			_ = json.Unmarshal(subjectBytes, &bound)
			if r.Inputs.SpecHash != bound.Spec.Hash {
				return fail("archive run spec differs from subject")
			}
			for _, id := range r.KeySubjects {
				if e := subject(id); e != nil {
					return e
				}
			}
			for _, move := range r.SubjectHistory {
				if e := subject(move.From); e != nil {
					return e
				}
				if e := subject(move.To); e != nil {
					return e
				}
			}
			if r.Itemset != "" {
				if e := object("itemset", r.Itemset); e != nil {
					return e
				}
			}
			if r.Kind != "report-only" {
				b, e := get(parts[0] + "/runs/" + r.ID + "/snapshot/gate.json")
				if e != nil {
					return e
				}
				if e := benchSnapshotCheck(&r, b); e != nil {
					return e
				}
			}
			for kind, ids := range r.Blobs {
				if kind == "prompts" && r.PromptsStored != nil && !*r.PromptsStored {
					for _, id := range ids {
						if !benchHex64.MatchString(id) {
							return fail("invalid hashed-only prompt id")
						}
					}
					continue
				}
				for _, id := range ids {
					if e := object("blob", id); e != nil {
						return e
					}
				}
			}
		}
		if len(parts) == 3 && parts[1] == "keys" && strings.HasSuffix(parts[2], ".json") && !strings.HasSuffix(parts[2], ".original.json") {
			var k benchKey
			if e := json.Unmarshal(data, &k); e != nil {
				return e
			}
			if e := benchKeyName(k.ID); e != nil {
				return e
			}
			if parts[2] != k.ID+".json" {
				return fail("archive key identity mismatch")
			}
			if e := subject(k.Subject); e != nil {
				return e
			}
			if k.BaseKey != "" {
				if e := benchKeyName(k.BaseKey); e != nil {
					return e
				}
				if _, e := get(parts[0] + "/keys/" + k.BaseKey + ".json"); e != nil {
					return e
				}
			}
			if e := object("itemset", k.Itemset); e != nil {
				return e
			}
			b, e := get(parts[0] + "/keys/" + k.ID + ".original.json")
			if e != nil {
				return e
			}
			if benchHash(b) != k.OriginalSHA256 {
				return fail("archive key original changed")
			}
			for _, id := range k.PromptBlobs {
				if e := object("blob", id); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func cmdBenchExport(a *app, args []string) error {
	fs := a.flags("bench export")
	out := fs.String("out", "", "private .tar.gz output file")
	pos, e := parseArgs(fs, args)
	if e != nil {
		return e
	}
	if e := exactArgs(pos, 1, "bench export <suite> --out FILE.tar.gz"); e != nil {
		return e
	}
	if *out == "" {
		return fail("--out is required")
	}
	files, e := benchArchiveFiles(pos[0])
	if e != nil {
		return e
	}
	if e := benchArchiveValidate(files); e != nil {
		return e
	}
	fmt.Fprintln(a.errw, "warning: archive contains private spec text, item text, repository paths and prompts; no redaction")
	path := expandHome(*out)
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.cwd, path)
	}
	// Export destinations are explicit, never sourced from records.
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range benchSortedKeys(files) {
		b := files[name]
		if e = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(b)), Typeflag: tar.TypeReg}); e != nil {
			break
		}
		if _, e = tw.Write(b); e != nil {
			break
		}
	}
	for _, close := range []func() error{tw.Close, gz.Close, f.Close} {
		if ce := close(); e == nil {
			e = ce
		}
	}
	if e != nil {
		return e
	}
	fmt.Fprintf(a.out, "export: %s (%d files)\n", path, len(files))
	return nil
}
func cmdBenchImportArchive(a *app, args []string) error {
	pos, e := parseArgs(a.flags("bench import-archive"), args)
	if e != nil {
		return e
	}
	if e := exactArgs(pos, 1, "bench import-archive FILE.tar.gz"); e != nil {
		return e
	}
	f, e := os.Open(expandHome(pos[0]))
	if e != nil {
		return e
	}
	defer f.Close()
	gz, e := gzip.NewReader(f)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var size int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if h.Typeflag != tar.TypeReg {
			return fail("archive links and non-regular entries are forbidden")
		}
		if e := benchArchiveMember(h.Name); e != nil {
			return e
		}
		if _, ok := files[h.Name]; ok {
			return fail("duplicate archive member %q", h.Name)
		}
		if h.Size < 0 || h.Size > benchArchiveLimit-size || len(files) >= 100000 {
			return fail("benchmark archive exceeds size or entry limit")
		}
		size += h.Size
		b, e := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if e != nil {
			return e
		}
		if int64(len(b)) != h.Size {
			return fail("archive entry size mismatch")
		}
		files[h.Name] = b
	}
	if len(files) == 0 {
		return fail("empty benchmark archive")
	}
	if e := benchArchiveValidate(files); e != nil {
		return e
	}
	// Check every existing destination before publishing any imported byte.
	for name, b := range files {
		path := filepath.Join(benchDir(), filepath.FromSlash(name))
		if e := benchSafe(path); e != nil {
			return e
		}
		if old, e := os.ReadFile(path); e == nil {
			if string(old) != string(b) {
				return fail("archive conflicts with existing file %s", name)
			}
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	if e := benchStoreInit(a.now()); e != nil {
		return e
	}
	for _, name := range benchSortedKeys(files) {
		if e := benchImmutable(filepath.Join(benchDir(), filepath.FromSlash(name)), files[name]); e != nil {
			return e
		}
	}
	if e := benchReadme(); e != nil {
		return e
	}
	fmt.Fprintf(a.out, "import-archive: %d files\n", len(files))
	return nil
}
