// Package diff compares two versions of a regulatory document clause by clause
// and emits change records whose every quote is a verifiable source citation.
//
// Clauses are aligned in four passes:
//  1. identical text (whitespace-insensitive): same number = unchanged,
//     different number = renumbered
//  2. same number with similar text (or the same heading): modified
//  3. different number with similar text: renumbered_modified
//  4. anything left over: deleted (old) or added (new)
//
// Within a matched pair, a word-level LCS diff produces edits that cite the
// exact changed words in both versions.
package diff

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"strata/internal/cite"
	"strata/internal/clause"
)

// Type classifies a change between two versions.
type Type string

const (
	Added              Type = "added"
	Deleted            Type = "deleted"
	Modified           Type = "modified"
	Renumbered         Type = "renumbered"
	RenumberedModified Type = "renumbered_modified"
)

// Alignment thresholds on word-level similarity (2*LCS / (len(a)+len(b))).
const (
	SameIDMinSimilarity = 0.5 // same number, different heading: below this it is a replacement
	MoveMinSimilarity   = 0.6 // different number: at or above this it is the same provision, moved
)

// Edit is one contiguous run of changed words. Old is nil for a pure
// insertion, New is nil for a pure deletion.
type Edit struct {
	Old *cite.Citation `json:"old,omitempty"`
	New *cite.Citation `json:"new,omitempty"`
}

// Change is one clause-level difference between two versions.
type Change struct {
	Type       Type           `json:"type"`
	OldID      string         `json:"old_id,omitempty"`
	NewID      string         `json:"new_id,omitempty"`
	Similarity float64        `json:"similarity"`
	Old        *cite.Citation `json:"old,omitempty"` // whole old clause
	New        *cite.Citation `json:"new,omitempty"` // whole new clause
	Edits      []Edit         `json:"edits,omitempty"`
}

// Result is the full comparison of two documents.
type Result struct {
	OldDoc    string   `json:"old_doc"`
	NewDoc    string   `json:"new_doc"`
	Unchanged int      `json:"unchanged"`
	Changes   []Change `json:"changes"`
}

type comparer struct {
	a, b   *clause.Document
	at, bt [][]clause.Token
}

// Compare aligns the clauses of a (old) and b (new) and returns their differences.
func Compare(a, b *clause.Document) Result {
	cp := &comparer{a: a, b: b, at: tokenize(a), bt: tokenize(b)}
	matchA := make([]bool, len(a.Clauses))
	matchB := make([]bool, len(b.Clauses))
	link := func(i, j int) { matchA[i], matchB[j] = true, true }
	res := Result{OldDoc: a.Name, NewDoc: b.Name}
	var changes []Change

	// Pass 1: identical text. Prefer a partner with the same number.
	byKey := map[string][]int{}
	for j := range b.Clauses {
		k := clause.Key(cp.bt[j])
		byKey[k] = append(byKey[k], j)
	}
	for i, ca := range a.Clauses {
		j := -1
		for _, c := range byKey[clause.Key(cp.at[i])] {
			if matchB[c] {
				continue
			}
			if b.Clauses[c].ID == ca.ID {
				j = c
				break
			}
			if j < 0 {
				j = c
			}
		}
		if j < 0 {
			continue
		}
		link(i, j)
		if b.Clauses[j].ID == ca.ID {
			res.Unchanged++
		} else {
			changes = append(changes, cp.change(Renumbered, i, j, 1))
		}
	}

	// Pass 2: same number, edited text.
	idxB := map[string]int{}
	for j, c := range b.Clauses {
		idxB[c.ID] = j
	}
	for i, ca := range a.Clauses {
		if matchA[i] {
			continue
		}
		j, ok := idxB[ca.ID]
		if !ok || matchB[j] {
			continue
		}
		sim := similarity(cp.at[i], cp.bt[j])
		if sim < SameIDMinSimilarity && ca.Heading != b.Clauses[j].Heading {
			continue
		}
		link(i, j)
		changes = append(changes, cp.change(Modified, i, j, sim))
	}

	// Pass 3: different number, similar text (moved and edited). Best pairs first.
	type cand struct {
		i, j int
		sim  float64
	}
	var cands []cand
	for i := range a.Clauses {
		if matchA[i] {
			continue
		}
		for j := range b.Clauses {
			if matchB[j] {
				continue
			}
			if s := similarity(cp.at[i], cp.bt[j]); s >= MoveMinSimilarity {
				cands = append(cands, cand{i, j, s})
			}
		}
	}
	sort.SliceStable(cands, func(x, y int) bool { return cands[x].sim > cands[y].sim })
	for _, c := range cands {
		if matchA[c.i] || matchB[c.j] {
			continue
		}
		link(c.i, c.j)
		changes = append(changes, cp.change(RenumberedModified, c.i, c.j, c.sim))
	}

	// Pass 4: leftovers.
	for i := range a.Clauses {
		if !matchA[i] {
			changes = append(changes, cp.change(Deleted, i, -1, 0))
		}
	}
	for j := range b.Clauses {
		if !matchB[j] {
			changes = append(changes, cp.change(Added, -1, j, 0))
		}
	}

	rank := map[Type]int{Deleted: 0, Renumbered: 1, RenumberedModified: 2, Modified: 3, Added: 4}
	sort.SliceStable(changes, func(x, y int) bool {
		kx, ky := changes[x].sortID(), changes[y].sortID()
		if kx != ky {
			return sectionLess(kx, ky)
		}
		return rank[changes[x].Type] < rank[changes[y].Type]
	})
	res.Changes = changes
	return res
}

// VerifyAll re-checks every citation in r against the source documents.
func VerifyAll(r Result, a, b *clause.Document) error {
	var errs []error
	check := func(c *cite.Citation, d *clause.Document) {
		if c == nil {
			return
		}
		if c.Doc != d.Name {
			errs = append(errs, fmt.Errorf("citation names %s but belongs to %s", c.Doc, d.Name))
			return
		}
		if err := cite.Verify(d.Source, *c); err != nil {
			errs = append(errs, err)
		}
	}
	for _, ch := range r.Changes {
		check(ch.Old, a)
		check(ch.New, b)
		for _, e := range ch.Edits {
			check(e.Old, a)
			check(e.New, b)
		}
	}
	return errors.Join(errs...)
}

func (cp *comparer) change(t Type, i, j int, sim float64) Change {
	ch := Change{Type: t, Similarity: math.Round(sim*100) / 100}
	if i >= 0 {
		c := cp.a.Clauses[i]
		ct := cite.New(cp.a.Name, c.ID, cp.a.Source, c.Span.Start, c.Span.End)
		ch.OldID, ch.Old = c.ID, &ct
	}
	if j >= 0 {
		c := cp.b.Clauses[j]
		ct := cite.New(cp.b.Name, c.ID, cp.b.Source, c.Span.Start, c.Span.End)
		ch.NewID, ch.New = c.ID, &ct
	}
	if i >= 0 && j >= 0 && t != Renumbered {
		ch.Edits = cp.edits(i, j)
	}
	return ch
}

// edits walks the word-level LCS and groups consecutive non-matching words
// into one Edit, citing the exact source span on each side.
func (cp *comparer) edits(i, j int) []Edit {
	x, y := cp.at[i], cp.bt[j]
	oldID, newID := cp.a.Clauses[i].ID, cp.b.Clauses[j].ID
	t := lcsTable(x, y)
	var out []Edit
	p, q := 0, 0
	for p < len(x) || q < len(y) {
		if p < len(x) && q < len(y) && x[p].Text == y[q].Text {
			p, q = p+1, q+1
			continue
		}
		p0, q0 := p, q
		for (p < len(x) || q < len(y)) && !(p < len(x) && q < len(y) && x[p].Text == y[q].Text) {
			if q >= len(y) || (p < len(x) && t[p+1][q] >= t[p][q+1]) {
				p++
			} else {
				q++
			}
		}
		var e Edit
		if p > p0 {
			c := cite.New(cp.a.Name, oldID, cp.a.Source, x[p0].Start, x[p-1].End)
			e.Old = &c
		}
		if q > q0 {
			c := cite.New(cp.b.Name, newID, cp.b.Source, y[q0].Start, y[q-1].End)
			e.New = &c
		}
		out = append(out, e)
	}
	return out
}

func tokenize(d *clause.Document) [][]clause.Token {
	out := make([][]clause.Token, len(d.Clauses))
	for i, c := range d.Clauses {
		out[i] = d.Tokens(c)
	}
	return out
}

// lcsTable[i][j] is the LCS length of x[i:] and y[j:].
func lcsTable(x, y []clause.Token) [][]int {
	t := make([][]int, len(x)+1)
	for i := range t {
		t[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i].Text == y[j].Text {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	return t
}

func similarity(x, y []clause.Token) float64 {
	if len(x)+len(y) == 0 {
		return 1
	}
	return 2 * float64(lcsTable(x, y)[0][0]) / float64(len(x)+len(y))
}

func (c Change) sortID() string {
	if c.NewID != "" {
		return c.NewID
	}
	return c.OldID
}

// sectionLess orders dotted section numbers numerically: 2.9 < 2.10 < 3.
func sectionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for k := 0; k < len(pa) && k < len(pb); k++ {
		na, _ := strconv.Atoi(pa[k])
		nb, _ := strconv.Atoi(pb[k])
		if na != nb {
			return na < nb
		}
	}
	return len(pa) < len(pb)
}
