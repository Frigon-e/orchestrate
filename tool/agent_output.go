package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/toon-format/toon-go"
)

// outputAudience is explicit, never inferred from a terminal. --human selects
// readable JSON; --json retains each command's established machine contract.
// Selection (including excerpts, limits and omission metadata) belongs to the
// caller and happens once, before any of these serializers run.
type outputAudience uint8

const (
	outputAgent outputAudience = iota
	outputHuman
	outputJSON
)

func explicitAudience(args []string) outputAudience {
	human, machine := false, false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		switch arg {
		case "--human", "--human=true":
			human = true
		case "--human=false":
			human = false
		case "--json", "--json=true":
			machine = true
		case "--json=false":
			machine = false
		}
	}
	if machine {
		return outputJSON
	}
	if human {
		return outputHuman
	}
	return outputAgent
}

func outputFailure(w io.Writer, audience outputAudience, err error) error {
	if err == nil {
		return nil
	}
	code := 1
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg == "" {
			return err
		} // A structured result already explains the exit.
		code = ee.code
	}
	if emitErr := writeOutput(w, audience, map[string]any{"status": "failed", "error": err.Error()}); emitErr != nil {
		return emitErr
	}
	return &exitError{code: code}
}

// gateInformation is the state-only v2 selection shared by every audience.
// Evidence and findings remain in the stable report artifact; completion,
// reasons and expansion links keep the default agent view actionable.
type gateInformation struct {
	Budget        *RunBudget         `json:"budget,omitempty"`
	SchemaVersion int                `json:"schema_version"`
	State         string             `json:"state"`
	Completion    reviewCompletion   `json:"completion"`
	Reasons       []reviewNextAction `json:"reasons"`
	NextActors    []string           `json:"next_actors"`
	NextActions   []reviewAction     `json:"next_actions"`
	Failures      []string           `json:"failures"`
	Gate          string             `json:"gate"`
	Deterministic string             `json:"deterministic"`
	Blockers      string             `json:"blockers"`
	Concerns      string             `json:"concerns"`
	Questions     string             `json:"questions"`
	Dropped       string             `json:"dropped"`
	Coverage      string             `json:"coverage,omitempty"`
	Spec          string             `json:"spec"`
	Report        string             `json:"report"`
	Details       string             `json:"details"`
	Experiment    string             `json:"experiment,omitempty"`
	Bench         string             `json:"bench,omitempty"`
	Warning       string             `json:"warning,omitempty"`
	Tokens        string             `json:"tokens,omitempty"`
	Shadow        string             `json:"shadow,omitempty"`
	QuestionCards string             `json:"question_cards,omitempty"`
	Notes         []string           `json:"notes,omitempty"`
	PassErrors    map[string]string  `json:"pass_failed,omitempty"`
	Help          []string           `json:"help,omitempty"`
}

// selectedOutput retains the original JSON bytes for machine compatibility and
// an ordered JSON-model value for presentation. encoding/json runs first so
// embedded fields, MarshalJSON, RawMessage, omitempty and null keep their meaning.
// Never send application structs directly to toon.Marshal.
type selectedOutput struct {
	json  []byte
	value any
}

func selectOutput(v any) (selectedOutput, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return selectedOutput{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	value, err := orderedOutputJSON(dec)
	if err != nil {
		return selectedOutput{}, err
	}
	return selectedOutput{json: b, value: value}, nil
}

// Normalized equality ignores object key order, treats -0 as 0, and preserves
// array order and every string byte. Integers outside the interoperable safe
// range [-(2^53-1), 2^53-1] become decimal strings BEFORE toon-go can use float64.
// Exponent and fractional spellings of integral values receive the same rule.
// Non-integral JSON decimals that cannot survive float64's shortest decimal
// representation are also losslessly stringified, never silently rounded.
// Machine JSON is deliberately not normalized: its existing bytes are a contract.
func outputNumber(n json.Number) (any, error) {
	// Avoid expanding attacker-controlled exponent spellings into huge big.Ints.
	// Out-of-domain numbers stay lossless strings, including their exponent.
	if len(n) > 4096 {
		return string(n), nil
	}
	if i := strings.IndexAny(string(n), "eE"); i >= 0 {
		exp, err := strconv.Atoi(string(n)[i+1:])
		if err != nil || exp > 4096 || exp < -4096 {
			return string(n), nil
		}
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok {
		return nil, fmt.Errorf("invalid JSON number %q", n)
	}
	if r.IsInt() {
		limit := big.NewInt(9007199254740991)
		if new(big.Int).Abs(r.Num()).Cmp(limit) > 0 {
			return r.Num().String(), nil
		}
		return r.Num().Int64(), nil
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return string(n), nil
	}
	round, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	if !ok || round.Cmp(r) != 0 {
		return string(n), nil
	}
	return f, nil
}

func orderedOutputJSON(dec *json.Decoder, normalize ...bool) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := token.(type) {
	case json.Delim:
		switch v {
		case '{':
			var fields []toon.Field
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return nil, err
				}
				value, err := orderedOutputJSON(dec, normalize...)
				if err != nil {
					return nil, err
				}
				fields = append(fields, toon.Field{Key: key.(string), Value: value})
			}
			_, err := dec.Token()
			return toon.NewObject(fields...), err
		case '[':
			items := make([]any, 0)
			for dec.More() {
				value, err := orderedOutputJSON(dec, normalize...)
				if err != nil {
					return nil, err
				}
				items = append(items, value)
			}
			_, err := dec.Token()
			return items, err
		}
	case json.Number:
		if len(normalize) > 0 && !normalize[0] {
			return v, nil
		}
		return outputNumber(v)
	default:
		return token, nil
	}
	return nil, fmt.Errorf("unexpected JSON delimiter %v", token)
}

func (s selectedOutput) render(audience outputAudience) ([]byte, error) {
	switch audience {
	case outputJSON:
		var b bytes.Buffer
		if err := json.Indent(&b, s.json, "", "  "); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	case outputHuman:
		return []byte(humanOutputJSON(s.value, 0)), nil
	default:
		return toon.Marshal(s.value)
	}
}

func writeOutput(w io.Writer, audience outputAudience, v any) error {
	s, err := selectOutput(v)
	if err != nil {
		return err
	}
	b, err := s.render(audience)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// humanOutputJSON keeps short scalar objects (not the root) on one line. It
// preserves encounter order, all alternatives and uncertainty, without a model.
func humanOutputJSON(v any, depth int) string {
	indent := strings.Repeat("  ", depth)
	child := indent + "  "
	switch v := v.(type) {
	case toon.Object:
		if len(v.Fields) == 0 {
			return "{}"
		}
		rows := make([]string, 0, len(v.Fields))
		scalars := true
		for _, f := range v.Fields {
			key, _ := json.Marshal(f.Key)
			rows = append(rows, string(key)+": "+humanOutputJSON(f.Value, depth+1))
			switch f.Value.(type) {
			case toon.Object, []any:
				scalars = false
			}
		}
		inline := "{" + strings.Join(rows, ", ") + "}"
		if depth > 0 && scalars && len(indent)+len(inline) <= 120 {
			return inline
		}
		return "{\n" + child + strings.Join(rows, ",\n"+child) + "\n" + indent + "}"
	case []any:
		if len(v) == 0 {
			return "[]"
		}
		rows := make([]string, len(v))
		for i, item := range v {
			rows[i] = humanOutputJSON(item, depth+1)
		}
		return "[\n" + child + strings.Join(rows, ",\n"+child) + "\n" + indent + "]"
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// ctxOmission describes selection, not serializer clipping. The budget counts
// UTF-8 excerpt bytes, not tokens or serialized bytes. Metadata is never dropped.
type ctxOmission struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Bytes  int    `json:"bytes"`
	Expand string `json:"expand"`
}

type ctxCommonFile struct {
	Path    string   `json:"path"`
	Sources []string `json:"sources"`
	Lookups []int    `json:"lookups"`
}

type ctxV2Selection struct {
	SchemaVersion int             `json:"schema_version"`
	Status        string          `json:"status"`
	Common        []ctxCommonFile `json:"common,omitempty"`
	Result        any             `json:"result"`
	ExcerptBudget int             `json:"excerpt_budget_bytes,omitempty"`
	ExcerptBytes  int             `json:"excerpt_bytes"`
	Omissions     []ctxOmission   `json:"omissions"`
	Notes         []string        `json:"notes"`
	Sources       string          `json:"sources"`
}

// ctxV2Agent is the same selection as ctxV2Selection for a model or a person:
// empty omissions and notes, the excerpt byte counters and the binary path in
// sources add tokens and no information, so they are left out. --json keeps
// the full selection.
type ctxV2Agent struct {
	SchemaVersion int             `json:"schema_version"`
	Status        string          `json:"status"`
	Common        []ctxCommonFile `json:"common,omitempty"`
	SearchedScope string          `json:"searched_scope,omitempty"` // shared by the lookups of a batch
	Excluded      []string        `json:"excluded,omitempty"`       // shared by the lookups of a batch
	Result        any             `json:"result"`
	Omissions     []ctxOmission   `json:"omissions,omitempty"`
	Notes         []string        `json:"notes,omitempty"`
	Sources       string          `json:"sources"`
}

// ctxHoistScope lifts the searched scope and the exclusion list out of the
// lookups of a batch when every lookup that has one has the same, so a reader
// sees each once. They still apply to every lookup.
func ctxHoistScope(views []*ctxView) (scope string, excluded []string) {
	withScope, withExcl := 0, 0
	sameScope, sameExcl := true, true
	for _, v := range views {
		if v.SearchedScope != "" {
			if withScope > 0 && v.SearchedScope != scope {
				sameScope = false
			}
			scope = v.SearchedScope
			withScope++
		}
		if len(v.Excluded) > 0 {
			if withExcl > 0 && !slices.Equal(v.Excluded, excluded) {
				sameExcl = false
			}
			excluded = v.Excluded
			withExcl++
		}
	}
	if !sameScope || withScope < 2 {
		scope = ""
	}
	if !sameExcl || withExcl < 2 {
		excluded = nil
	}
	for _, v := range views {
		if scope != "" {
			v.SearchedScope = ""
		}
		if excluded != nil {
			v.Excluded = nil
		}
	}
	return scope, excluded
}

// ctxDropMatch clears the match flag on every line when none of the lines is a
// context row: a match flag that is true on every row says nothing.
func ctxDropMatch(views []*ctxView) {
	for _, v := range views {
		for _, p := range v.Parts {
			for _, f := range p.Files {
				for _, l := range f.Lines {
					if !l.Match {
						return
					}
				}
			}
		}
	}
	for _, v := range views {
		for _, p := range v.Parts {
			for fi := range p.Files {
				for li := range p.Files[fi].Lines {
					p.Files[fi].Lines[li].Match = false
				}
			}
		}
	}
}

// ctxEvidenceID is a short handle for a source line or file: 32 bits are enough
// to tell the rows of one answer apart, and the selection keys repeats on the
// content itself, not on this handle.
func ctxEvidenceID(path string, line int, text string) string {
	hash := sha256.Sum256(fmt.Appendf(nil, "%s:%d\n%s", path, line, text))
	return fmt.Sprintf("ctx-%x", hash[:4])
}

// ctxEmitV2 selects once, including source excerpts and a single shared budget.
// Legacy v1 machine JSON deliberately retains its old indentation transforms
// and prose-only caps. --json --schema-version 2 opts into this same selection.
func (a *app) ctxEmitV2(e *ctxEnv, data any, f ctxFmt, budget int) error {
	var shown []*ctxView // the selected copies, for the agent-facing trims
	selection := ctxV2Selection{SchemaVersion: 2, Status: "ok", Result: data, ExcerptBudget: budget, Omissions: []ctxOmission{}, Notes: e.Notes(), Sources: e.Sources()}
	remaining := budget
	plain := !e.ReadOnly // gate agents run read-only and cite ids; other sessions do not
	seenSources := map[string]bool{}
	keep := func(id, source, text, expand string) bool {
		if budget > 0 && len(text) > remaining {
			selection.Omissions = append(selection.Omissions, ctxOmission{id, source, len(text), expand})
			return false
		}
		remaining -= len(text)
		selection.ExcerptBytes += len(text)
		return true
	}
	selectView := func(v *ctxView) (*ctxView, error) {
		// Do not mutate cached observations, or a subsequent legacy JSON call.
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		var out ctxView
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, err
		}
		out.finishTruth()
		for _, p := range out.Parts {
			// Raw contains status, capability and uncertainty metadata in v2.
			// Only source excerpts consume the excerpt budget, never that truth.
			for fi := range p.Files {
				file := &p.Files[fi]
				if out.Kind == "sym" || out.Kind == "read" {
					native := e.Lines(file.Path)
					for i := range file.Lines {
						n := file.Lines[i].N
						if n > 0 && n <= len(native) {
							file.Lines[i].Text = native[n-1]
						}
					}
				}
				var identity strings.Builder
				for _, line := range file.Lines {
					fmt.Fprintf(&identity, "%d:%s\n", line.N, line.Text)
				}
				file.ID = ctxEvidenceID(file.Path, 0, identity.String())
				lines := file.Lines[:0]
				for _, line := range file.Lines {
					id := ctxEvidenceID(file.Path, line.N, line.Text)
					at := fmt.Sprintf("%s:%d", file.Path, line.N)
					seen := fmt.Sprintf("%s\n%s", at, line.Text)
					if seenSources[seen] {
						ref := id
						if plain {
							ref = at // no line ids to point at: name the row
						} else {
							file.References = append(file.References, id)
						}
						// Keep this lookup's match/range/enclosing relationship even
						// when its source body was already emitted as context.
						line.ID, line.Reference, line.Text = "", ref, ""
						lines = append(lines, line)
						continue
					}
					line.ID = id
					if plain {
						// A session that cites no evidence ids does not need one per row;
						// the file keeps its id and every row its path and line.
						line.ID, id = "", at
					}
					expand := fmt.Sprintf("Use native read on %s with offset %d and limit 1.", filepath.Join(e.Code, file.Path), line.N)
					if keep(id, at, line.Text, expand) {
						lines = append(lines, line)
						seenSources[seen] = true
					}
				}
				file.Lines = lines
			}
		}
		shown = append(shown, &out)
		return &out, nil
	}
	switch v := data.(type) {
	case *ctxView:
		out, err := selectView(v)
		if err != nil {
			return err
		}
		selection.Status, selection.Result = out.Status, out
	case []*ctxView:
		common := map[string]*ctxCommonFile{}
		for i, view := range v {
			paths := map[string]bool{}
			for _, path := range view.XPaths {
				paths[path] = true
			}
			for _, part := range view.Parts {
				for _, file := range part.Files {
					paths[file.Path] = true
				}
			}
			for path := range paths {
				row := common[path]
				if row == nil {
					row = &ctxCommonFile{Path: path}
					common[path] = row
				}
				row.Lookups = append(row.Lookups, i+1)
				found := false
				for _, source := range row.Sources {
					found = found || source == view.Kind
				}
				if !found {
					row.Sources = append(row.Sources, view.Kind)
				}
			}
		}
		for _, row := range common {
			if len(row.Lookups) > 1 {
				sort.Strings(row.Sources)
				selection.Common = append(selection.Common, *row)
			}
		}
		sort.Slice(selection.Common, func(i, j int) bool { return selection.Common[i].Path < selection.Common[j].Path })
		views := make([]*ctxView, 0, len(v))
		tipShown := false
		for _, view := range v {
			out, err := selectView(view)
			if err != nil {
				return err
			}
			if tipShown {
				out.Tip = ""
			}
			tipShown = tipShown || out.Tip != ""
			views = append(views, out)
		}
		selection.Status, selection.Result = ctxBatchStatus(views), views
	case *ctxBrief:
		views := make([]*ctxView, 0, len(v.Facts))
		for _, fact := range v.Facts {
			view := e.symView(fact, defaultSymOpts(f))
			out, err := selectView(view)
			if err != nil {
				return err
			}
			views = append(views, out)
		}
		// Candidate snippets in legacy briefs were whitespace-normalized. V2
		// extracts the current native range instead of re-encoding those strings.
		type candidate struct {
			ID    string    `json:"id"`
			At    string    `json:"at"`
			In    string    `json:"in,omitempty"`
			Via   string    `json:"via"`
			Lines []ctxLine `json:"lines"`
		}
		candidates := make([]candidate, 0, len(v.LikeRows))
		for _, r := range v.LikeRows {
			path, location, has := strings.Cut(r.At, ":")
			startText, _, _ := strings.Cut(location, "-")
			start, _ := strconv.Atoi(startText)
			item := candidate{ID: ctxEvidenceID(r.At, 0, r.Via), At: r.At, In: r.In, Via: r.Via, Lines: []ctxLine{}}
			if has && start > 0 {
				native := e.Lines(path)
				for i := start - 1; i < len(native) && i < start+2; i++ {
					text := native[i]
					expand := fmt.Sprintf("Use native read on %s with offset %d and limit 3.", filepath.Join(e.Code, path), start)
					if keep(ctxEvidenceID(path, i+1, text), fmt.Sprintf("%s:%d", path, i+1), text, expand) {
						item.Lines = append(item.Lines, ctxLine{N: i + 1, Text: text})
					}
				}
			}
			candidates = append(candidates, item)
		}
		// The brief's legacy prose duplicates these observations, so v2 selects
		// the inventories and source-backed symbol views, not both copies.
		selection.Result = struct {
			ID         string      `json:"id"`
			Files      []briefFile `json:"files"`
			Symbols    []*ctxView  `json:"symbols"`
			Candidates []candidate `json:"candidates"`
			Missing    []string    `json:"not_found"`
		}{v.ID, v.FileRows, views, candidates, v.Names}
	}
	if len(selection.Omissions) > 0 && selection.Status != "failed" {
		selection.Status = "partial"
	}
	selection.Notes, selection.Sources = e.Notes(), e.Sources()
	audience := ctxAudience(a.out)
	if audience == outputJSON {
		return writeOutput(a.out, audience, selection)
	}
	ctxDropMatch(shown)
	var scope string
	var excluded []string
	if _, ok := data.([]*ctxView); ok {
		scope, excluded = ctxHoistScope(shown)
	}
	return writeOutput(a.out, audience, ctxV2Agent{selection.SchemaVersion, selection.Status, selection.Common, scope, excluded, selection.Result, selection.Omissions, selection.Notes, e.SourcesBrief()})
}

// agentScalar is the shared boundary for legacy prose renderers and embedded
// metadata cells. It uses the same conforming encoder, not hand-written quoting.
// New information views should use writeOutput for the entire selected object.
func agentScalar(v string) string {
	s, _ := selectOutput(v) // A Go string is always JSON serializable.
	b, err := s.render(outputAgent)
	if err != nil { // encoding/json already guarantees valid UTF-8.
		panic(err)
	}
	return string(b)
}
