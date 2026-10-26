package other

// Normalize deliberately preserves whitespace for literal file names.
func Normalize(value string) string { return value }

type Literal struct{}

func (Literal) Parse(value string) string { return value }
