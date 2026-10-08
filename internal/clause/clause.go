// Package clause parses a regulatory markdown document into numbered clauses.
//
// Format assumption: a clause is a level-3 heading that starts with a dotted
// section number ("### 4.2 Inspection Reporting"), followed by its body text.
// "# " is the document title, "## " is a grouping heading. Every clause keeps
// exact byte offsets into the original source so citations can be verified.
//
// Documents that break the assumption (no numbered clauses, duplicate numbers,
// text outside any clause) are rejected with an error instead of being
// silently half-parsed.
package clause

import (
	"fmt"
	"regexp"
	"strings"
)

// Span is a half-open byte range [Start, End) into Document.Source.
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Clause is one numbered provision.
type Clause struct {
	ID          string // "4.2"
	Heading     string // "Inspection Reporting"
	Text        string // body text exactly as in the source (trimmed)
	Span        Span   // whole clause, from "###" to the end of the body
	HeadingSpan Span
	TextSpan    Span
}

// Token is a whitespace-delimited word with its source offsets.
type Token struct {
	Text       string
	Start, End int
}

// Document is a parsed regulatory document.
type Document struct {
	Name      string
	Source    string
	Title     string
	TitleSpan Span
	Preamble  Span // text between the title and the first section heading
	Clauses   []Clause
	Meta      map[string]string // front matter "key: value" pairs (docket, issued, ...)
}

var (
	reTitle  = regexp.MustCompile(`^#\s+(\S.*?)\s*$`)
	reClause = regexp.MustCompile(`^###\s+(\d+(?:\.\d+)*)\s+(\S.*?)\s*$`)
	reWord   = regexp.MustCompile(`\S+`)
)

type line struct {
	text       string
	start, end int // end excludes the line terminator
}

type blockKind int

const (
	kindLead blockKind = iota // text before the title
	kindTitle
	kindGroup
	kindClause
)

type block struct {
	kind      blockKind
	idx       int
	bodyStart int
}

// Parse splits src into clauses. name is used in citations and error messages.
func Parse(name, src string) (*Document, error) {
	doc := &Document{Name: name, Source: src, Meta: map[string]string{}}
	lines := splitLines(src)

	// Skip YAML front matter.
	i := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0].text) == "---" {
		closed := false
		for i = 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i].text) == "---" {
				closed = true
				i++
				break
			}
			if k, v, ok := strings.Cut(lines[i].text, ":"); ok {
				doc.Meta[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		if !closed {
			return nil, fmt.Errorf("%s:1: front matter opened with --- but never closed", name)
		}
	}

	leadStart := len(src)
	if i < len(lines) {
		leadStart = lines[i].start
	}
	cur := block{kind: kindLead, bodyStart: leadStart}
	seen := map[string]int{}

	closeBlock := func(end int) error {
		sp := trimSpan(src, cur.bodyStart, end)
		switch cur.kind {
		case kindTitle:
			doc.Preamble = sp
		case kindClause:
			c := &doc.Clauses[cur.idx]
			c.TextSpan = sp
			c.Text = src[sp.Start:sp.End]
			c.Span.End = max(sp.End, c.HeadingSpan.End)
		default:
			if sp.End > sp.Start {
				return fmt.Errorf("%s:%d: text outside any numbered clause would be dropped: %q",
					name, strings.Count(src[:sp.Start], "\n")+1, firstLine(src[sp.Start:sp.End]))
			}
		}
		return nil
	}

	for ; i < len(lines); i++ {
		ln := lines[i]
		level := headingLevel(ln.text)
		if level == 0 {
			continue
		}
		if err := closeBlock(ln.start); err != nil {
			return nil, err
		}
		lineNo := i + 1
		switch level {
		case 1:
			m := reTitle.FindStringSubmatchIndex(ln.text)
			if m == nil || doc.Title != "" {
				return nil, fmt.Errorf("%s:%d: expected exactly one non-empty \"# Title\" heading", name, lineNo)
			}
			doc.Title = ln.text[m[2]:m[3]]
			doc.TitleSpan = Span{ln.start + m[2], ln.start + m[3]}
			cur = block{kind: kindTitle, bodyStart: ln.end}
		case 2:
			cur = block{kind: kindGroup, bodyStart: ln.end}
		case 3:
			m := reClause.FindStringSubmatchIndex(ln.text)
			if m == nil {
				return nil, fmt.Errorf("%s:%d: clause heading must start with a section number, like \"### 4.2 Title\": %q",
					name, lineNo, ln.text)
			}
			id := ln.text[m[2]:m[3]]
			if first, dup := seen[id]; dup {
				return nil, fmt.Errorf("%s:%d: duplicate clause §%s (first defined on line %d)", name, lineNo, id, first)
			}
			seen[id] = lineNo
			doc.Clauses = append(doc.Clauses, Clause{
				ID:          id,
				Heading:     ln.text[m[4]:m[5]],
				HeadingSpan: Span{ln.start + m[4], ln.start + m[5]},
				Span:        Span{Start: ln.start, End: ln.end},
			})
			cur = block{kind: kindClause, idx: len(doc.Clauses) - 1, bodyStart: ln.end}
		default:
			return nil, fmt.Errorf("%s:%d: heading level %d is not supported (use # title, ## group, ### clause)",
				name, lineNo, level)
		}
	}
	if err := closeBlock(len(src)); err != nil {
		return nil, err
	}
	if len(doc.Clauses) == 0 {
		return nil, fmt.Errorf("%s: no numbered clauses found (expected headings like \"### 4.2 Title\"); unstructured documents are not supported yet", name)
	}
	return doc, nil
}

// Tokens returns the clause's heading and body words with absolute source offsets.
func (d *Document) Tokens(c Clause) []Token {
	var out []Token
	for _, sp := range []Span{c.HeadingSpan, c.TextSpan} {
		for _, m := range reWord.FindAllStringIndex(d.Source[sp.Start:sp.End], -1) {
			out = append(out, Token{
				Text:  d.Source[sp.Start+m[0] : sp.Start+m[1]],
				Start: sp.Start + m[0],
				End:   sp.Start + m[1],
			})
		}
	}
	return out
}

// Key is the whitespace-normalized text of a token list. Two clauses with the
// same Key differ at most in whitespace or line wrapping.
func Key(toks []Token) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.Text
	}
	return strings.Join(parts, " ")
}

func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n == len(s) || (s[n] != ' ' && s[n] != '\t') {
		return 0
	}
	return n
}

func splitLines(src string) []line {
	var out []line
	start := 0
	for start < len(src) {
		nl := strings.IndexByte(src[start:], '\n')
		if nl < 0 {
			out = append(out, line{text: src[start:], start: start, end: len(src)})
			break
		}
		text := strings.TrimSuffix(src[start:start+nl], "\r")
		out = append(out, line{text: text, start: start, end: start + len(text)})
		start += nl + 1
	}
	return out
}

// trimSpan trims surrounding whitespace from src[s:e]. An all-whitespace range
// becomes the empty span at s.
func trimSpan(src string, s, e int) Span {
	s0 := s
	for s < e && isSpace(src[s]) {
		s++
	}
	for e > s && isSpace(src[e-1]) {
		e--
	}
	if s == e {
		return Span{s0, s0}
	}
	return Span{s, e}
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
