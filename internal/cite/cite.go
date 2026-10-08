// Package cite defines source citations and verifies them against the source text.
//
// A citation is a byte span into a document plus the exact text found there.
// Verify re-reads the span and fails if the quote does not match, so nothing
// downstream (including an LLM) can present a passage the source does not contain.
package cite

import (
	"fmt"
	"strings"
)

// Citation points to an exact byte span [Start, End) in a source document.
type Citation struct {
	Doc       string `json:"doc"`
	ClauseID  string `json:"clause_id"`
	Start     int    `json:"start"`
	End       int    `json:"end"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Quote     string `json:"quote"`
}

// New builds a citation for src[start:end].
func New(doc, clauseID, src string, start, end int) Citation {
	return Citation{
		Doc:       doc,
		ClauseID:  clauseID,
		Start:     start,
		End:       end,
		LineStart: LineOf(src, start),
		LineEnd:   LineOf(src, max(end-1, start)),
		Quote:     src[start:end],
	}
}

// LineOf returns the 1-based line number containing byte offset off.
func LineOf(src string, off int) int {
	if off > len(src) {
		off = len(src)
	}
	return strings.Count(src[:off], "\n") + 1
}

// Verify checks that the source contains exactly c.Quote at c's offsets.
func Verify(src string, c Citation) error {
	if c.Start < 0 || c.End > len(src) || c.Start > c.End {
		return fmt.Errorf("citation %s §%s: offsets [%d,%d) out of range (source has %d bytes)",
			c.Doc, c.ClauseID, c.Start, c.End, len(src))
	}
	if got := src[c.Start:c.End]; got != c.Quote {
		return fmt.Errorf("citation %s §%s: quote mismatch at [%d,%d): source has %q, citation says %q",
			c.Doc, c.ClauseID, c.Start, c.End, got, c.Quote)
	}
	return nil
}

func (c Citation) String() string {
	if c.LineStart == c.LineEnd {
		return fmt.Sprintf("%s §%s L%d", c.Doc, c.ClauseID, c.LineStart)
	}
	return fmt.Sprintf("%s §%s L%d-%d", c.Doc, c.ClauseID, c.LineStart, c.LineEnd)
}
