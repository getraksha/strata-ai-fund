package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"strata/internal/llm"
	"strata/internal/state"
)

// fakeModel scripts the LLM deterministically:
//   - status: draft for v1/v2, final for v3, with quotes that exist in each file
//   - §4.2: material, action on OB-04 with a real quote
//   - §6.2: ambiguous, so it escalates
//   - everything else: cosmetic, no action
func fakeModel() llm.Model {
	return llm.Func{ModelName: "fake", F: func(req llm.Request) (json.RawMessage, error) {
		if req.Name == "document_status" {
			switch {
			case strings.Contains(req.User, "v3_final.md"):
				return json.Marshal(map[string]any{"status": "final", "quote": "This decision is effective today.", "confidence": 0.95})
			case strings.Contains(req.User, "v2_revised.md"):
				return json.Marshal(map[string]any{"status": "draft", "quote": "issued for further public comment", "confidence": 0.95})
			default:
				return json.Marshal(map[string]any{"status": "draft", "quote": "issued for public comment", "confidence": 0.95})
			}
		}
		ans := map[string]any{
			"rule_change_material": false, "company_action_required": false, "direction": "cosmetic",
			"summary": "no change in meaning", "rationale": "", "quotes": []any{}, "actions": []any{},
			"confidence": 0.9, "ambiguous": false, "ambiguity_reason": "",
		}
		switch {
		case strings.Contains(req.User, `"id": "4.2"`):
			ans["rule_change_material"], ans["company_action_required"], ans["direction"] = true, true, "stricter"
			ans["quotes"] = []any{map[string]any{"version": "new", "text": "quarterly."}}
			ans["actions"] = []any{map[string]any{"item_id": "OB-04", "action": "Move to quarterly reports"}}
		case strings.Contains(req.User, `"id": "6.2"`):
			ans["ambiguous"], ans["ambiguity_reason"] = true, "scripted ambiguity"
		}
		return json.Marshal(ans)
	}}
}

// setup copies testdata into a temp dir so tests can modify files freely.
func setup(t *testing.T) (dir string, p *Project) {
	t.Helper()
	dir = t.TempDir()
	for _, f := range []string{"company.json", "v1_proposed.md", "v2_revised.md", "v3_final.md"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := Init(filepath.Join(dir, ".strata"), filepath.Join(dir, "company.json"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func ingest(t *testing.T, p *Project, dir, name string) {
	t.Helper()
	if _, err := p.Ingest(context.Background(), filepath.Join(dir, name), fakeModel()); err != nil {
		t.Fatalf("ingest %s: %v", name, err)
	}
}

func cardFor(t *testing.T, st *state.State, newID string) *state.Card {
	t.Helper()
	for _, c := range st.Cards {
		ch := c.Result.Impact.Change
		if ch.NewID == newID || (ch.NewID == "" && ch.OldID == newID) {
			return c
		}
	}
	t.Fatalf("no card for §%s", newID)
	return nil
}

func TestFullLoop(t *testing.T) {
	dir, p := setup(t)
	ingest(t, p, dir, "v1_proposed.md")
	ingest(t, p, dir, "v2_revised.md")

	st, err := p.State()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Cards) != 8 {
		t.Fatalf("v1 -> v2 should produce 8 cards, got %d", len(st.Cards))
	}
	c42, c62 := cardFor(t, st, "4.2"), cardFor(t, st, "6.2")
	if c42.Status != state.Pending || c62.Status != state.NeedsExpert {
		t.Fatalf("statuses: 4.2=%s 6.2=%s", c42.Status, c62.Status)
	}

	// Routing rules.
	if _, err := p.Review(c42.ID, "P-03", state.Approve, ""); err == nil {
		t.Error("P-03 is not routed §4.2 and must not decide it")
	}
	if _, err := p.Review(c42.ID, "P-02", state.Reject, ""); err == nil {
		t.Error("a rejection without a note must fail")
	}
	if _, err := p.Review(c62.ID, "P-05", state.Approve, ""); err == nil {
		t.Error("an escalated card must only be decided by counsel")
	}
	if _, err := p.Review(c42.ID, "P-02", state.Approve, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Review(c42.ID, "P-02", state.Approve, "again"); err == nil {
		t.Error("a decided card must not be decided twice")
	}
	if _, err := p.Review(c62.ID, "P-04", state.Approve, "audit removal confirmed"); err != nil {
		t.Fatal(err)
	}

	st, _ = p.State()
	if len(st.Tasks) != 1 || st.Tasks[0].ItemID != "OB-04" || st.Tasks[0].Owner != "P-02" {
		t.Fatalf("approved §4.2 should give Raj one task, got %+v", st.Tasks)
	}

	// Final version: obligations become effective; OB-01's deadline comes from the cited clause.
	beforeFinal := len(p.Log.Events())
	ingest(t, p, dir, "v3_final.md")
	st, _ = p.State()
	ob1, ok := st.Obligations["OB-01"]
	if !ok || ob1.Status != "effective" || ob1.EffectiveDate != "2026-09-10" || ob1.Due != "2026-11-09" {
		t.Fatalf("OB-01 after final = %+v", ob1)
	}
	if ob1.DueEvidence == nil || ob1.DueEvidence.ClauseID != "3.3" || !strings.Contains(ob1.DueEvidence.Quote, "60 days") {
		t.Fatalf("OB-01 due date must cite §3.3: %+v", ob1.DueEvidence)
	}
	if len(st.Obligations) != 9 {
		t.Errorf("all 9 obligations should be effective, got %d", len(st.Obligations))
	}

	// Roll back the final version: obligations go back to anticipated, cards from v3 disappear.
	if _, err := p.Rollback(beforeFinal, "P-01", "final decision ingested by mistake"); err != nil {
		t.Fatal(err)
	}
	st, _ = p.State()
	if len(st.Obligations) != 0 || len(st.Versions) != 2 || len(st.Cards) != 8 {
		t.Fatalf("after rollback: %d obligation changes, %d versions, %d cards", len(st.Obligations), len(st.Versions), len(st.Cards))
	}
	if len(st.Tasks) != 1 {
		t.Error("the approval before the rollback point must survive")
	}

	// Re-ingesting v3 must not reuse undone card ids.
	ingest(t, p, dir, "v3_final.md")
	st, _ = p.State()
	seen := map[string]bool{}
	for _, e := range p.Log.Events() {
		if e.Type == state.TypeChangeAssessed {
			var c state.ChangeAssessed
			_ = json.Unmarshal(e.Data, &c)
			if seen[c.CardID] {
				t.Fatalf("card id %s reused", c.CardID)
			}
			seen[c.CardID] = true
		}
	}

	// The log on disk still verifies after all of this.
	if _, err := Open(filepath.Join(dir, ".strata")); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

func TestIngestFailurePaths(t *testing.T) {
	dir, p := setup(t)
	ingest(t, p, dir, "v1_proposed.md")

	if _, err := p.Ingest(context.Background(), filepath.Join(dir, "v1_proposed.md"), fakeModel()); err == nil {
		t.Error("ingesting the same version twice must fail")
	}

	// The previous version is edited after ingest: refuse to diff against it.
	path := filepath.Join(dir, "v1_proposed.md")
	data, _ := os.ReadFile(path)
	os.WriteFile(path, append(data, []byte("\n### 9.9 Sneaky\nadded later\n")...), 0o644)
	if _, err := p.Ingest(context.Background(), filepath.Join(dir, "v2_revised.md"), fakeModel()); err == nil ||
		!strings.Contains(err.Error(), "changed since it was ingested") {
		t.Errorf("edited previous version must be detected, got %v", err)
	}
}

func TestOpenRejectsChangedCompanyContext(t *testing.T) {
	dir, _ := setup(t)
	path := filepath.Join(dir, "company.json")
	data, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(data), "120000", "40000", 1)), 0o644)
	if _, err := Open(filepath.Join(dir, ".strata")); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("changed company context must be detected, got %v", err)
	}
}

func TestRollbackValidation(t *testing.T) {
	_, p := setup(t)
	if _, err := p.Rollback(1, "P-01", "x"); err == nil {
		t.Error("cannot roll back when there is nothing after event 1")
	}
	if _, err := p.Rollback(0, "nobody", "x"); err == nil {
		t.Error("unknown actor must fail")
	}
}
