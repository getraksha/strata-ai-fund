package diff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"strata/internal/clause"
)

func load(t *testing.T, name string) *clause.Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	d, err := clause.Parse(name, string(data))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type expectedFile struct {
	Transitions []struct {
		From    string `json:"from"`
		To      string `json:"to"`
		Changes []struct {
			Old  *string `json:"section_old"`
			New  *string `json:"section_new"`
			Type string  `json:"type"`
		} `json:"changes"`
	} `json:"transitions"`
}

func str(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}

// TestAgainstExpectedChanges checks the diff against the hand-written answer
// key: every planted change is found, nothing extra is reported, and every
// citation verifies against the source.
func TestAgainstExpectedChanges(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "expected_changes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp expectedFile
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	for _, tr := range exp.Transitions {
		t.Run(tr.From+"->"+tr.To, func(t *testing.T) {
			a, b := load(t, tr.From), load(t, tr.To)
			res := Compare(a, b)
			if err := VerifyAll(res, a, b); err != nil {
				t.Fatalf("citation verification failed: %v", err)
			}

			want := map[string]bool{}
			for _, c := range tr.Changes {
				if c.Type == "unchanged" {
					continue
				}
				want[c.Type+" "+str(c.Old)+"->"+str(c.New)] = true
			}
			got := map[string]bool{}
			for _, c := range res.Changes {
				o, n := c.OldID, c.NewID
				got[string(c.Type)+" "+str(nilIfEmpty(o))+"->"+str(nilIfEmpty(n))] = true
			}
			for k := range want {
				if !got[k] {
					t.Errorf("missing change: %s", k)
				}
			}
			for k := range got {
				if !want[k] {
					t.Errorf("unexpected change: %s", k)
				}
			}
			if t.Failed() {
				t.Logf("got: %v", keys(got))
			}
		})
	}
}

func TestEditPinpointsChangedWords(t *testing.T) {
	a, b := load(t, "v1_proposed.md"), load(t, "v2_revised.md")
	res := Compare(a, b)
	for _, c := range res.Changes {
		if c.NewID != "4.2" {
			continue
		}
		if len(c.Edits) != 1 {
			t.Fatalf("§4.2: want 1 edit, got %d", len(c.Edits))
		}
		e := c.Edits[0]
		if e.Old == nil || e.New == nil || e.Old.Quote != "annually." || e.New.Quote != "quarterly." {
			t.Fatalf("§4.2: unexpected edit %+v / %+v", e.Old, e.New)
		}
		return
	}
	t.Fatal("§4.2 change not found")
}

func TestVerifyAllCatchesTamperedQuote(t *testing.T) {
	a, b := load(t, "v1_proposed.md"), load(t, "v2_revised.md")
	res := Compare(a, b)
	for i := range res.Changes {
		if len(res.Changes[i].Edits) > 0 && res.Changes[i].Edits[0].New != nil {
			res.Changes[i].Edits[0].New.Quote = "something the source never said"
			if err := VerifyAll(res, a, b); err == nil {
				t.Fatal("tampered quote passed verification")
			}
			return
		}
	}
	t.Fatal("no edit found to tamper with")
}

func TestIdenticalDocumentsHaveNoChanges(t *testing.T) {
	a := load(t, "v2_revised.md")
	res := Compare(a, a)
	if len(res.Changes) != 0 || res.Unchanged != len(a.Clauses) {
		t.Fatalf("want 0 changes and %d unchanged, got %d changes, %d unchanged",
			len(a.Clauses), len(res.Changes), res.Unchanged)
	}
}

func TestSectionLess(t *testing.T) {
	ids := []string{"10.1", "2.10", "2.9", "2", "1.1"}
	sort.Slice(ids, func(i, j int) bool { return sectionLess(ids[i], ids[j]) })
	want := []string{"1.1", "2", "2.9", "2.10", "10.1"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got %v, want %v", ids, want)
		}
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
