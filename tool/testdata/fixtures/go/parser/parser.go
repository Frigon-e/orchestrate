package parser

import "strings"

// Normalize trims a file name without changing its case.
func Normalize(value string) string {
	return strings.TrimSpace(value)
}

type Parser struct{}

func (Parser) Parse(value string) string { return Normalize(value) }
