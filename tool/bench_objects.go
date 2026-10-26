package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var benchHex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)
var benchHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func benchObjectPath(kind, id string) (string, error) {
	valid := benchHex16.MatchString(id)
	ext := ".json"
	switch kind {
	case "spec":
		ext = ".md"
	case "itemset":
	case "blob":
		valid = benchHex64.MatchString(id)
		ext = ""
	default:
		return "", fail("unknown benchmark object kind %q", kind)
	}
	if !valid {
		return "", fail("invalid %s object id %q", kind, id)
	}
	path := filepath.Join(benchDir(), "objects", kind, id+ext)
	return path, benchSafe(path)
}
func benchObjectCheck(kind, id string, b []byte) error {
	hash := benchHash(b)
	switch kind {
	case "spec":
		hash = specHash(b)
	case "itemset":
		var it benchItemset
		if e := json.Unmarshal(b, &it); e != nil {
			return e
		}
		canonical, e := benchCanonicalItems(it.Items)
		if e != nil {
			return e
		}
		hash = benchHash(canonical)[:16]
		if it.ID != id {
			return fail("itemset identity mismatch: %s", id)
		}
	}
	if hash != id {
		return fail("%s object hash mismatch: %s", kind, id)
	}
	return nil
}
func objectPut(kind, id string, b []byte) error {
	path, e := benchObjectPath(kind, id)
	if e != nil {
		return e
	}
	if e := benchObjectCheck(kind, id, b); e != nil {
		return e
	}
	return benchImmutable(path, b)
}
func objectGet(kind, id string) ([]byte, error) {
	path, e := benchObjectPath(kind, id)
	if e != nil {
		return nil, e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	if e := benchObjectCheck(kind, id, b); e != nil {
		return nil, e
	}
	if kind == "spec" {
		var meta benchSpecVersion
		if e := benchReadJSON(filepath.Join(benchDir(), "objects", "spec", id+".json"), &meta); e != nil {
			return nil, e
		}
		if meta.Hash != id || meta.SHA256 != benchHash(b) {
			return nil, fail("spec sha256 mismatch: %s", id)
		}
	}
	return b, nil
}
func objectHas(kind, id string) bool { _, e := objectGet(kind, id); return e == nil }
func benchPutJSON(kind, id string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return objectPut(kind, id, append(b, '\n'))
}
func benchBlob(b []byte, name, from string, now time.Time) (string, error) {
	id := benchHash(b)
	if e := objectPut("blob", id, b); e != nil {
		return "", e
	}
	path := filepath.Join(benchDir(), "objects", "blob", id+".meta.json")
	if e := benchSafe(path); e != nil {
		return "", e
	}
	if !benchExists(path) {
		meta := map[string]any{"name": name, "mime": "application/octet-stream", "size": len(b), "added_at": now.UTC(), "from": from}
		if e := benchImmutableJSON(path, meta); e != nil {
			return "", e
		}
	}
	return id, nil
}
func benchStoreInit(now time.Time) error {
	if e := benchMkdir(benchDir()); e != nil {
		return e
	}
	if e := os.Chmod(benchDir(), 0700); e != nil {
		return e
	}
	path := filepath.Join(benchDir(), "store.json")
	if e := benchSafe(path); e != nil {
		return e
	}
	if benchExists(path) {
		var s struct {
			Schema int `json:"schema_version"`
		}
		if e := benchReadJSON(path, &s); e != nil {
			return e
		}
		if s.Schema != 2 {
			return fail("benchmark store schema %d is not supported; use bench migrate", s.Schema)
		}
		return nil
	}
	return benchImmutableJSON(path, map[string]any{"schema_version": 2, "created_at": now.UTC(), "created_by": benchBuildNow()})
}
