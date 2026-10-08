package eval

import (
	"path/filepath"
	"testing"

	"strata/internal/assess"
	"strata/internal/cite"
	"strata/internal/diff"
	"strata/internal/impact"
)

func result(typ diff.Type, oldID, newID string, material, action, escalate bool, quotes, rejected int) assess.Result {
	return assess.Result{
		Impact:         impact.Impact{Change: diff.Change{Type: typ, OldID: oldID, NewID: newID}},
		DecidedBy:      assess.ByModel,
		Material:       material,
		ActionRequired: action,
		Escalate:       escalate,
		Evidence:       make([]cite.Citation, quotes),
		QuotesRejected: rejected,
	}
}

func loadKey(t *testing.T) *Expected {
	t.Helper()
	exp, err := LoadExpected(filepath.Join("..", "..", "testdata", "expected_changes.json"))
	if err != nil {
		t.Fatal(err)
	}
	return exp
}

// v2 -> v3 key: 1.2 yes/no/no, 4.1 yes/yes/no, 5.1 yes/yes/yes, 7.1 yes/yes/no (3.2 unchanged, skipped)
func run1() assess.Report {
	return assess.Report{Results: []assess.Result{
		result(diff.Modified, "1.2", "1.2", true, false, false, 1, 0),
		result(diff.Modified, "4.1", "4.1", true, true, false, 2, 0),
		result(diff.Modified, "5.1", "5.1", true, true, false, 1, 1),   // missed the escalation
		result(diff.Modified, "7.1", "7.1", false, false, false, 0, 0), // missed material and action
		result(diff.Added, "", "9.9", true, true, false, 0, 0),         // not in the key
	}}
}

func TestScore(t *testing.T) {
	exp := loadKey(t)
	if exp.StatusFor("v3_final.md") != "final" || exp.StatusFor("v1_proposed.md") != "draft" {
		t.Fatal("answer key statuses not loaded")
	}
	sc := Score(exp.Transitions[1], run1())

	if sc.Total != 4 {
		t.Fatalf("total = %d, want 4 (unchanged entries are skipped)", sc.Total)
	}
	if sc.MaterialCorrect != 3 || sc.ActionCorrect != 3 || sc.EscalateCorrect != 3 {
		t.Errorf("material %d, action %d, escalate %d; want 3, 3, 3",
			sc.MaterialCorrect, sc.ActionCorrect, sc.EscalateCorrect)
	}
	if sc.QuotesVerified != 4 || sc.QuotesRejected != 1 {
		t.Errorf("quotes verified/rejected = %d/%d, want 4/1", sc.QuotesVerified, sc.QuotesRejected)
	}
	if len(sc.Unexpected) != 1 || sc.Unexpected[0] != "added -->9.9" {
		t.Errorf("unexpected = %v", sc.Unexpected)
	}
}

func TestSummarizeAcrossRuns(t *testing.T) {
	exp := loadKey(t)
	r2 := run1()
	r2.Results[2].Escalate = true // second run gets §5.1 right

	s := Summarize([]Scorecard{Score(exp.Transitions[1], run1()), Score(exp.Transitions[1], r2)})

	if s.Runs != 2 || s.Checks != 8 {
		t.Fatalf("runs %d checks %d, want 2 and 8", s.Runs, s.Checks)
	}
	if s.EscalateCorrect != 7 || s.MaterialCorrect != 6 {
		t.Errorf("escalate %d/8, material %d/8; want 7 and 6", s.EscalateCorrect, s.MaterialCorrect)
	}
	if s.Stable != 3 {
		t.Errorf("stable rows = %d, want 3 (only §5.1 flipped)", s.Stable)
	}
	for _, row := range s.Rows {
		if row.Key == "modified 5.1->5.1" && (row.Stable || row.EscalateOK != 1) {
			t.Errorf("§5.1 row = %+v", row)
		}
	}
	if len(s.Unexpected) != 1 {
		t.Errorf("unexpected should be de-duplicated across runs: %v", s.Unexpected)
	}
}
