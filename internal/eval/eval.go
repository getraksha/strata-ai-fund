// Package eval scores assessments against the hand-written answer key
// (testdata/expected_changes.json).
//
// Per change it checks three judgments: rule_change_material,
// company_action_required and escalate. Because model calls are not
// deterministic, the same eval can be run several times; Summarize reports
// per-change accuracy across runs and whether every run agreed (stability).
package eval

import (
	"encoding/json"
	"fmt"
	"os"

	"strata/internal/assess"
)

// Expected is the answer key.
type Expected struct {
	Docket   string `json:"docket"`
	Versions []struct {
		File   string `json:"file"`
		Status string `json:"status"`
	} `json:"versions"`
	Transitions []Transition `json:"transitions"`
}

type Transition struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Changes []struct {
		Old      *string `json:"section_old"`
		New      *string `json:"section_new"`
		Type     string  `json:"type"`
		Material bool    `json:"material"`
		Action   bool    `json:"action"`
		Escalate bool    `json:"escalate"`
		Note     string  `json:"note"`
	} `json:"changes"`
}

func LoadExpected(path string) (*Expected, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e Expected
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &e, nil
}

// StatusFor returns the expected status of a version file.
func (e *Expected) StatusFor(file string) string {
	for _, v := range e.Versions {
		if v.File == file {
			return v.Status
		}
	}
	return ""
}

// Verdict is the triple being scored.
type Verdict struct {
	Material bool `json:"material"`
	Action   bool `json:"action"`
	Escalate bool `json:"escalate"`
}

func (v Verdict) String() string { return yn(v.Material) + "/" + yn(v.Action) + "/" + yn(v.Escalate) }

// Row compares one expected change with one run's result.
type Row struct {
	Key       string  `json:"key"`
	Note      string  `json:"note"`
	Found     bool    `json:"found"`
	DecidedBy string  `json:"decided_by"`
	Want      Verdict `json:"want"`
	Got       Verdict `json:"got"`
}

func (r Row) MaterialOK() bool { return r.Found && r.Want.Material == r.Got.Material }
func (r Row) ActionOK() bool   { return r.Found && r.Want.Action == r.Got.Action }
func (r Row) EscalateOK() bool { return r.Found && r.Want.Escalate == r.Got.Escalate }

// Scorecard is one run of one transition.
type Scorecard struct {
	Transition      string   `json:"transition"`
	Rows            []Row    `json:"rows"`
	Total           int      `json:"total"`
	MaterialCorrect int      `json:"material_correct"`
	ActionCorrect   int      `json:"action_correct"`
	EscalateCorrect int      `json:"escalate_correct"`
	QuotesVerified  int      `json:"quotes_verified"`
	QuotesRejected  int      `json:"quotes_rejected"`
	Unexpected      []string `json:"unexpected_changes,omitempty"`
}

// Key identifies a change the same way the answer key does.
func Key(typ, oldID, newID string) string {
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	return typ + " " + dash(oldID) + "->" + dash(newID)
}

// Score compares one assessed transition with its expected changes.
func Score(exp Transition, rep assess.Report) Scorecard {
	sc := Scorecard{Transition: exp.From + " -> " + exp.To}
	got := map[string]assess.Result{}
	for _, r := range rep.Results {
		ch := r.Impact.Change
		got[Key(string(ch.Type), ch.OldID, ch.NewID)] = r
		sc.QuotesVerified += len(r.Evidence)
		sc.QuotesRejected += r.QuotesRejected
	}
	seen := map[string]bool{}
	for _, c := range exp.Changes {
		if c.Type == "unchanged" {
			continue
		}
		k := Key(c.Type, deref(c.Old), deref(c.New))
		seen[k] = true
		row := Row{Key: k, Note: c.Note, Want: Verdict{c.Material, c.Action, c.Escalate}}
		if r, ok := got[k]; ok {
			row.Found, row.DecidedBy = true, r.DecidedBy
			row.Got = Verdict{r.Material, r.ActionRequired, r.Escalate}
		}
		sc.Rows = append(sc.Rows, row)
		sc.Total++
		if row.MaterialOK() {
			sc.MaterialCorrect++
		}
		if row.ActionOK() {
			sc.ActionCorrect++
		}
		if row.EscalateOK() {
			sc.EscalateCorrect++
		}
	}
	for _, r := range rep.Results {
		ch := r.Impact.Change
		if k := Key(string(ch.Type), ch.OldID, ch.NewID); !seen[k] {
			sc.Unexpected = append(sc.Unexpected, k)
		}
	}
	return sc
}

// SummaryRow is one expected change across all runs.
type SummaryRow struct {
	Key        string   `json:"key"`
	Note       string   `json:"note"`
	Want       Verdict  `json:"want"`
	Got        []string `json:"got_per_run"` // verdict per run, or "missing"
	MaterialOK int      `json:"material_ok_runs"`
	ActionOK   int      `json:"action_ok_runs"`
	EscalateOK int      `json:"escalate_ok_runs"`
	Stable     bool     `json:"stable"` // every run gave the same verdict
}

// Summary aggregates several runs of the same transition.
type Summary struct {
	Transition      string       `json:"transition"`
	Runs            int          `json:"runs"`
	Rows            []SummaryRow `json:"rows"`
	Checks          int          `json:"checks"` // rows x runs, per judgment
	MaterialCorrect int          `json:"material_correct"`
	ActionCorrect   int          `json:"action_correct"`
	EscalateCorrect int          `json:"escalate_correct"`
	Stable          int          `json:"stable_rows"`
	QuotesVerified  int          `json:"quotes_verified"`
	QuotesRejected  int          `json:"quotes_rejected"`
	Unexpected      []string     `json:"unexpected_changes,omitempty"`
}

// Summarize combines scorecards of the same transition from several runs.
// Rows come from the answer key, so they line up across runs.
func Summarize(cards []Scorecard) Summary {
	if len(cards) == 0 {
		return Summary{}
	}
	s := Summary{Transition: cards[0].Transition, Runs: len(cards)}
	unexpected := map[string]bool{}
	for i, row := range cards[0].Rows {
		sr := SummaryRow{Key: row.Key, Note: row.Note, Want: row.Want, Stable: true}
		for _, sc := range cards {
			r := sc.Rows[i]
			v := "missing"
			if r.Found {
				v = r.Got.String()
			}
			sr.Got = append(sr.Got, v)
			if v != sr.Got[0] {
				sr.Stable = false
			}
			if r.MaterialOK() {
				sr.MaterialOK++
			}
			if r.ActionOK() {
				sr.ActionOK++
			}
			if r.EscalateOK() {
				sr.EscalateOK++
			}
		}
		s.Rows = append(s.Rows, sr)
		s.MaterialCorrect += sr.MaterialOK
		s.ActionCorrect += sr.ActionOK
		s.EscalateCorrect += sr.EscalateOK
		if sr.Stable {
			s.Stable++
		}
	}
	for _, sc := range cards {
		s.Checks += sc.Total
		s.QuotesVerified += sc.QuotesVerified
		s.QuotesRejected += sc.QuotesRejected
		for _, u := range sc.Unexpected {
			if !unexpected[u] {
				unexpected[u] = true
				s.Unexpected = append(s.Unexpected, u)
			}
		}
	}
	return s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
