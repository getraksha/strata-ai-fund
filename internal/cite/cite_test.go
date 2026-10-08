package cite

import (
	"strings"
	"testing"
)

const src = "line one\nshall file reports annually.\nline three\n"

func TestNewAndVerify(t *testing.T) {
	start := strings.Index(src, "annually.")
	c := New("doc.md", "4.2", src, start, start+len("annually."))
	if c.Quote != "annually." || c.LineStart != 2 || c.LineEnd != 2 {
		t.Fatalf("unexpected citation: %+v", c)
	}
	if err := Verify(src, c); err != nil {
		t.Fatalf("valid citation rejected: %v", err)
	}
}

func TestVerifyRejectsTamperedQuote(t *testing.T) {
	start := strings.Index(src, "annually.")
	c := New("doc.md", "4.2", src, start, start+len("annually."))
	c.Quote = "quarterly."
	if err := Verify(src, c); err == nil {
		t.Fatal("tampered quote was accepted")
	}
}

func TestVerifyRejectsBadOffsets(t *testing.T) {
	for _, c := range []Citation{
		{Start: -1, End: 3},
		{Start: 5, End: 2},
		{Start: 0, End: len(src) + 1},
	} {
		if err := Verify(src, c); err == nil {
			t.Fatalf("out-of-range citation accepted: %+v", c)
		}
	}
}
