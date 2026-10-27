package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
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
