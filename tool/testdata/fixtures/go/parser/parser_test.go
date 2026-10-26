package parser

import "testing"

func TestNormalize(t *testing.T) {
	if Normalize(" file ") != "file" {
		t.Fatal("trim")
	}
}
