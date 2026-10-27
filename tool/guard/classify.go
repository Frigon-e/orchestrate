package guard

import (
	"regexp"
	"strings"
)

var (
	rgValue = union(grepValue, set("--iglob", "-T", "--type-not", "-r", "--replace", "-d", "--max-depth", "-M", "--max-columns"))
	fdValue = set("-e", "-E", "-t", "-d", "-x", "-X", "-S", "-c", "-j", "--extension", "--exclude", "--type", "--max-depth",
		"--size", "--changed-within", "--changed-before", "--owner", "--color", "--threads", "--base-directory")
	jbValue  = set("--project-path", "--git-remote-url", "--limit", "--revision", "-p", "--path-filter", "--branch")
	lspValue = set("--limit")

	docExtensions = set("md", "txt", "json", "yaml", "yml", "toml")
	docTypes      = union(docExtensions, set("markdown"))

	ident    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	symLead  = regexp.MustCompile(`^(func|def|class|type|interface) `)
	symTrail = regexp.MustCompile(`\\?\($`)
	capital  = regexp.MustCompile(`[A-Z]`)
	flagWord = regexp.MustCompile(`^-[A-Za-z]+$`)
)

func union(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// lookup is one tool call that counts as a lookup.
type lookup struct {
	tool, query string
	symbol      bool
}

// positionals are the words of an argument list that are neither flags nor flag values.
func positionals(args []string, valueFlags map[string]bool) []string {
	var out []string
	for at := 0; at < len(args); at++ {
		arg := args[at]
		if arg == "--" {
			return append(out, args[at+1:]...)
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			if valueFlags[arg] {
				at++
			}
			continue
		}
		out = append(out, arg)
	}
	return out
}

// rgParts are the pattern, paths, globs, types and -w flag of an rg call.
func rgParts(args []string) (pattern string, paths, globs, types []string, word bool) {
	var pat *string
	var free []string
	for at := 0; at < len(args); at++ {
		arg := args[at]
		nxt := ""
		if at+1 < len(args) {
			nxt = args[at+1]
		}
		if arg == "--" {
			free = append(free, args[at+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			if arg == "-e" || arg == "--regexp" {
				if pat == nil {
					pat = &nxt
				}
				at++
				continue
			}
			switch {
			case arg == "-g" || arg == "--glob" || arg == "--iglob":
				globs = append(globs, nxt)
			case arg == "-t" || arg == "--type":
				types = append(types, nxt)
			case strings.HasPrefix(arg, "--glob=") || strings.HasPrefix(arg, "--iglob="):
				globs = append(globs, arg[strings.Index(arg, "=")+1:])
			case strings.HasPrefix(arg, "--type="):
				types = append(types, arg[strings.Index(arg, "=")+1:])
			case arg == "--word-regexp" || (flagWord.MatchString(arg) && strings.Contains(arg[1:], "w")):
				word = true
			}
			if rgValue[arg] {
				at++
			}
			continue
		}
		free = append(free, arg)
	}
	if pat == nil && len(free) > 0 {
		pat, free = &free[0], free[1:]
	}
	if pat != nil {
		pattern = *pat
	}
	return pattern, free, globs, types, word
}

func docTarget(kind, value string) bool {
	value = strings.TrimSpace(value)
	if kind == "type" {
		return docTypes[value]
	}
	name := value[strings.LastIndex(value, "/")+1:]
	if !strings.Contains(name, ".") {
		return false
	}
	ext := strings.Trim(value[strings.LastIndex(value, ".")+1:], "{}")
	if ext == "" {
		return false
	}
	for _, part := range strings.Split(ext, ",") {
		if !docExtensions[part] {
			return false
		}
	}
	return true
}

// docOnly: every glob, type and path target of an rg call is a doc target.
func docOnly(globs, types, paths []string) bool {
	type target struct{ kind, value string }
	var targets []target
	for _, g := range globs {
		targets = append(targets, target{"glob", g})
	}
	for _, t := range types {
		targets = append(targets, target{"type", t})
	}
	for _, p := range paths {
		targets = append(targets, target{"path", p})
	}
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if !docTarget(t.kind, t.value) {
			return false
		}
	}
	return true
}

// symbolName is the identifier an rg pattern names, or "" when it does not name a symbol.
func symbolName(args []string) string {
	pattern, paths, globs, types, word := rgParts(args)
	if docOnly(globs, types, paths) {
		return ""
	}
	text := strings.NewReplacer(`\b`, "", `\<`, "", `\>`, "").Replace(pattern)
	text = symTrail.ReplaceAllString(symLead.ReplaceAllString(text, ""), "")
	if !ident.MatchString(text) {
		return ""
	}
	if word || strings.Contains(text, ".") || strings.Contains(text, "_") || capital.MatchString(text[1:]) {
		return text
	}
	return ""
}

// symbolRg: an rg whose pattern names a symbol, so orch lsp would answer it better.
func symbolRg(args []string) bool { return symbolName(args) != "" }

func lookupShell(line string) (lookup, bool) {
	listing := segments(line, true)
	if len(listing) > 0 {
		if name, _ := command(listing[0]); name == "cd" {
			listing = listing[1:]
		}
	}
	if len(listing) == 0 {
		return lookup{}, false
	}
	name, args := command(listing[0])
	first := func(words []string) string {
		if len(words) == 0 {
			return ""
		}
		return words[0]
	}
	switch {
	case name == "jbcontext" && len(args) > 0 && args[0] == "search":
		return lookup{"jbcontext", first(positionals(args[1:], jbValue)), false}, true
	case name == "orch" && len(args) >= 2 && args[0] == "lsp" && !strings.HasPrefix(args[1], "-"):
		return lookup{"lsp:" + args[1], strings.Join(positionals(args[2:], lspValue), " "), false}, true
	case name == "orch" && len(args) >= 2 && args[0] == "ctx" && !strings.HasPrefix(args[1], "-"):
		var words []string
		for _, w := range args[2:] {
			if !strings.HasPrefix(w, "-") {
				words = append(words, w)
			}
		}
		return lookup{"ctx:" + args[1], strings.Join(words, " "), false}, true
	case name == "rg":
		pattern, _, _, _, _ := rgParts(args)
		return lookup{"rg", pattern, symbolRg(args)}, true
	case name == "fd":
		return lookup{"fd", first(positionals(args, fdValue)), false}, true
	}
	return lookup{}, false
}

func readQuery(path string, offset, limit any) string {
	empty := func(v any) bool { return v == nil || v == "" }
	if empty(offset) && empty(limit) {
		return path
	}
	part := func(v any) string {
		if empty(v) {
			return ""
		}
		return pyStr(v)
	}
	return path + ":" + part(offset) + "-" + part(limit)
}

// lookupClaude is the lookup a Claude tool call makes, if it makes one.
func lookupClaude(ev *Event) (lookup, bool) {
	name := ev.Str("tool_name")
	input, ok := ev.Object("tool_input")
	if !ok {
		return lookup{}, false
	}
	switch {
	case name == "Bash":
		line, ok := input["command"].(string)
		if !ok {
			return lookup{}, false
		}
		return lookupShell(line)
	case name == "Read":
		path, ok := input["file_path"].(string)
		if !ok || path == "" {
			return lookup{}, false
		}
		return lookup{"read", readQuery(path, input["offset"], input["limit"]), false}, true
	case name == "LSP":
		op, ok := input["operation"].(string)
		if !ok || op == "" {
			return lookup{}, false
		}
		where := "" + pyGet(input, "filePath") + ":" + pyGet(input, "line")
		return lookup{"lsp:" + op, where, false}, true
	case strings.HasPrefix(name, "mcp__jbcontext__"):
		query := ""
		for _, key := range []string{"query", "q"} {
			if truthy(input[key]) {
				query = pyStr(input[key])
				break
			}
		}
		return lookup{"jbcontext", query, false}, true
	}
	return lookup{}, false
}

// pyGet is str(d.get(key, "")): a missing key is empty, a null is "None".
func pyGet(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	return pyStr(v)
}
