package assess

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"strata/internal/cite"
	"strata/internal/clause"
	"strata/internal/company"
	"strata/internal/diff"
	"strata/internal/impact"
	"strata/internal/llm"
)

type fixture struct {
	a, b *clause.Document
	cc   *company.Context
	rep  impact.Report
}

func setup(t *testing.T, oldName, newName string) fixture {
	t.Helper()
	load := func(name string) *clause.Document {
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
	a, b := load(oldName), load(newName)
	cc, err := company.Load(filepath.Join("..", "..", "testdata", "company.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := impact.Map(diff.Compare(a, b), a, b, cc)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{a, b, cc, rep}
}

func (f fixture) impact(t *testing.T, newID string) impact.Impact {
	t.Helper()
	for _, im := range f.rep.Impacts {
		if im.Change.NewID == newID || (im.Change.NewID == "" && im.Change.OldID == newID) {
			return im
		}
	}
	t.Fatalf("no impact for §%s", newID)
	return impact.Impact{}
}

// scripted returns a model that always answers with v.
func scripted(v any) llm.Model {
	return llm.Func{ModelName: "scripted", F: func(llm.Request) (json.RawMessage, error) {
		return json.Marshal(v)
	}}
}

func good() modelChange {
	return modelChange{
		Material:       true,
		ActionRequired: true,
		Direction:      "stricter",
		Summary:        "Vegetation inspection reports move from annual to quarterly.",
		Rationale:      "Four filings a year instead of one.",
		Quotes:         []Quote{{Version: "old", Text: "annually."}, {Version: "new", Text: "quarterly."}},
		Actions:        []Action{{ItemID: "OB-04", Action: "Change frequency to quarterly"}, {ItemID: "DOC-03", Action: "Add quarterly dates"}},
		Confidence:     0.92,
	}
}

func reviewerIDs(r Result) []string {
	var out []string
	for _, rv := range r.Reviewers {
		out = append(out, rv.PersonID)
	}
	return out
}

func hasReason(r Result, substr string) bool {
	for _, x := range r.Reasons {
		if strings.Contains(x, substr) {
			return true
		}
	}
	return false
}

func TestGoodAnswerIsAcceptedWithVerifiedCitations(t *testing.T) {
	f := setup(t, "v1_proposed.md", "v2_revised.md")
	as := &Assessor{Model: scripted(good()), Company: f.cc}
	r := as.assessChange(context.Background(), f.impact(t, "4.2"), f.a, f.b)

	if r.Escalate {
		t.Fatalf("unexpected escalation: %v", r.Reasons)
	}
	if r.DecidedBy != ByModel || !r.Material || len(r.Actions) != 2 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if len(r.Evidence) != 2 {
		t.Fatalf("want 2 citations, got %d", len(r.Evidence))
	}
	for _, c := range r.Evidence {
		src := f.a.Source
		if c.Doc == f.b.Name {
			src = f.b.Source
		}
		if err := cite.Verify(src, c); err != nil {
			t.Error(err)
		}
		if c.ClauseID != "4.2" {
			t.Errorf("citation points at §%s, want §4.2", c.ClauseID)
		}
	}
}

func TestGuardrailsEscalate(t *testing.T) {
	f := setup(t, "v1_proposed.md", "v2_revised.md")
	im := f.impact(t, "4.2")

	cases := []struct {
		name   string
		model  llm.Model
		reason string
	}{
		{"invented quote", scripted(func() modelChange {
			m := good()
			m.Quotes = []Quote{{Version: "new", Text: "monthly."}}
			return m
		}()), "quote not found"},
		{"quote from wrong version", scripted(func() modelChange {
			m := good()
			m.Quotes = []Quote{{Version: "old", Text: "quarterly."}}
			return m
		}()), "quote not found"},
		{"action on unknown item", scripted(func() modelChange {
			m := good()
			m.Actions = append(m.Actions, Action{ItemID: "OB-99", Action: "x"})
			return m
		}()), "unknown item"},
		{"ambiguous", scripted(func() modelChange {
			m := good()
			m.Ambiguous, m.AmbiguityReason = true, "reasonable time is undefined"
			return m
		}()), "ambiguity"},
		{"unclear direction", scripted(func() modelChange {
			m := good()
			m.Direction = "unclear"
			return m
		}()), "stricter or looser"},
		{"low confidence", scripted(func() modelChange {
			m := good()
			m.Confidence = 0.4
			return m
		}()), "below threshold"},
		{"action required but none given", scripted(func() modelChange {
			m := good()
			m.Actions = nil
			return m
		}()), "proposes no valid action"},
		{"actions given but none required", scripted(func() modelChange {
			m := good()
			m.ActionRequired = false
			return m
		}()), "says no company action is required"},
		{"material without quotes", scripted(func() modelChange {
			m := good()
			m.Quotes = nil
			return m
		}()), "no verified quote"},
		{"model error", llm.Func{ModelName: "err", F: func(llm.Request) (json.RawMessage, error) {
			return nil, errors.New("timeout")
		}}, "model call failed"},
		{"malformed output", llm.Func{ModelName: "bad", F: func(llm.Request) (json.RawMessage, error) {
			return json.RawMessage(`{"material": true, "surprise": 1}`), nil
		}}, "does not match schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			as := &Assessor{Model: tc.model, Company: f.cc}
			r := as.assessChange(context.Background(), im, f.a, f.b)
			if !r.Escalate || !hasReason(r, tc.reason) {
				t.Fatalf("want escalation with %q, got escalate=%v reasons=%v", tc.reason, r.Escalate, r.Reasons)
			}
			ids := strings.Join(reviewerIDs(r), ",")
			if !strings.Contains(ids, "P-04") {
				t.Errorf("escalation reviewer P-04 missing from %s", ids)
			}
			for _, a := range r.Actions {
				if a.ItemID == "OB-99" {
					t.Error("action on unknown item was kept")
				}
			}
		})
	}
}

func TestFailedCallFailsClosed(t *testing.T) {
	f := setup(t, "v1_proposed.md", "v2_revised.md")
	as := &Assessor{Model: llm.Func{ModelName: "err", F: func(llm.Request) (json.RawMessage, error) {
		return nil, errors.New("boom")
	}}, Company: f.cc}
	r := as.assessChange(context.Background(), f.impact(t, "4.2"), f.a, f.b)
	if r.DecidedBy != ByError || !r.Material || !r.ActionRequired || r.Direction != "unclear" {
		t.Fatalf("failed call should be treated as material+action-required+unclear, got %+v", r)
	}
}

func TestRenumberedIsDecidedByRule(t *testing.T) {
	f := setup(t, "v1_proposed.md", "v2_revised.md")
	calls := 0
	m := llm.Func{ModelName: "count", F: func(req llm.Request) (json.RawMessage, error) {
		calls++
		if req.Name == "document_status" {
			return json.Marshal(modelStatus{Status: "draft", Quote: "REVISED PROPOSED DECISION", Confidence: 0.9})
		}
		return json.Marshal(modelChange{Direction: "cosmetic", Confidence: 0.9, Quotes: []Quote{}, Actions: []Action{}})
	}}
	rep := (&Assessor{Model: m, Company: f.cc}).Assess(context.Background(), f.rep, f.a, f.b)

	renumbered := 0
	for _, r := range rep.Results {
		if r.Impact.Change.Type == diff.Renumbered {
			renumbered++
			if r.DecidedBy != ByRule || r.Material {
				t.Errorf("renumbered change: %+v", r)
			}
		}
	}
	// one status call + one call per non-renumbered change
	if want := 1 + len(rep.Results) - renumbered; calls != want {
		t.Errorf("model calls = %d, want %d", calls, want)
	}
}

func TestStatusQuoteIsVerified(t *testing.T) {
	f := setup(t, "v2_revised.md", "v3_final.md")

	ok := &Assessor{Company: f.cc, Model: scripted(modelStatus{Status: "final", Quote: "This decision is effective today.", Confidence: 0.95})}
	sr := ok.Status(context.Background(), f.b)
	if sr.Escalate || sr.Status != "final" || sr.Evidence == nil || sr.Evidence.ClauseID != "7.1" {
		t.Fatalf("unexpected status result: %+v", sr)
	}
	if err := cite.Verify(f.b.Source, *sr.Evidence); err != nil {
		t.Fatal(err)
	}

	bad := &Assessor{Company: f.cc, Model: scripted(modelStatus{Status: "final", Quote: "Adopted unanimously.", Confidence: 0.95})}
	if sr := bad.Status(context.Background(), f.b); !sr.Escalate {
		t.Fatal("invented status quote should escalate")
	}
}

func TestLocateHandlesLineWrapping(t *testing.T) {
	src := "### 3.2 Plan Contents\nEach WMP shall describe inspection programs,\nsystem hardening projects."
	start, end, ok := locate(src, 0, len(src), "inspection programs, system hardening")
	if !ok {
		t.Fatal("whitespace-flexible match failed")
	}
	if got := src[start:end]; got != "inspection programs,\nsystem hardening" {
		t.Fatalf("citation should keep the real source text, got %q", got)
	}
	if _, _, ok := locate(src, 0, len(src), "a"); ok {
		t.Error("trivially short quotes must be rejected")
	}
}
