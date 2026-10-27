package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// pyQuote is a JSON string as Python's json.dumps writes it (ensure_ascii).
func pyQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= ' ' && r <= '~':
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyFloat is Python's repr of a float; text is the JSON literal, used when f is not finite.
func pyFloat(f float64, text string) string {
	e := strconv.FormatFloat(f, 'e', -1, 64)
	if strings.ContainsAny(e, "IN") {
		return text
	}
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mant, expText, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	switch {
	case exp >= 16 || exp < -4:
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		esign := "+"
		if exp < 0 {
			esign, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, esign, exp)
	case exp >= 0:
		for len(digits) < exp+1 {
			digits += "0"
		}
		frac := digits[exp+1:]
		if frac == "" {
			frac = "0"
		}
		return sign + digits[:exp+1] + "." + frac
	}
	return sign + "0." + strings.Repeat("0", -exp-1) + digits
}

// pyDumpRaw re-writes a JSON document the way Python's json.dumps writes the
// parsed value: keys in input order, ", " and ": " separators, ASCII only.
func pyDumpRaw(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	if pyDumpTok(dec, &b) != nil {
		return string(raw)
	}
	return b.String()
}

func pyDumpTok(dec *json.Decoder, b *strings.Builder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		closer := "]"
		if t == '{' {
			closer = "}"
		}
		b.WriteString(string(t))
		for first := true; dec.More(); first = false {
			if !first {
				b.WriteString(", ")
			}
			if t == '{' {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				b.WriteString(pyQuote(key.(string)) + ": ")
			}
			if err := pyDumpTok(dec, b); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		b.WriteString(closer)
	case string:
		b.WriteString(pyQuote(t))
	case json.Number:
		b.WriteString(pyStr(t))
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case nil:
		b.WriteString("null")
	}
	return nil
}

// pyDumpValue is pyDumpRaw for a value that is already decoded.
func pyDumpValue(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return pyDumpRaw(raw)
}
