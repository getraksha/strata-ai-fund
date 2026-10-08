package state

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"strata/internal/assess"
)

func newLog(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	return l, path
}

func mustAppend(t *testing.T, l *Log, actor, typ string, data any) Event {
	t.Helper()
	e, err := l.Append(actor, typ, data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAppendReopenVerifies(t *testing.T) {
	l, path := newLog(t)
	mustAppend(t, l, "system", TypeContextLoaded, ContextLoaded{CompanyName: "X"})
	mustAppend(t, l, "system", TypeVersionIngested, VersionIngested{Doc: "v1.md"})

	re, err := Open(path)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	if n := len(re.Events()); n != 2 {
		t.Fatalf("events = %d, want 2", n)
	}
	if _, err := Create(path); err == nil {
		t.Fatal("Create must refuse to overwrite an existing log")
	}
}

func TestTamperingIsDetected(t *testing.T) {
	cases := map[string]func([]byte) []byte{
		"edited field": func(b []byte) []byte { return bytes.Replace(b, []byte("v1.md"), []byte("v9.md"), 1) },
		"deleted line": func(b []byte) []byte {
			lines := bytes.SplitAfter(b, []byte("\n"))
			return bytes.Join(append(lines[:1:1], lines[2:]...), nil)
		},
		"reordered lines": func(b []byte) []byte {
			lines := bytes.SplitAfter(b, []byte("\n"))
			lines[1], lines[2] = lines[2], lines[1]
			return bytes.Join(lines, nil)
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			l, path := newLog(t)
			mustAppend(t, l, "system", TypeContextLoaded, ContextLoaded{CompanyName: "X"})
			mustAppend(t, l, "system", TypeVersionIngested, VersionIngested{Doc: "v1.md"})
			mustAppend(t, l, "system", TypeVersionIngested, VersionIngested{Doc: "v2.md"})

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tamper(data), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "verification") {
				t.Fatalf("tampered log was accepted (err=%v)", err)
			}
		})
	}
}

func card(id string, escalate bool) ChangeAssessed {
	return ChangeAssessed{CardID: id, Result: assess.Result{
		Escalate: escalate,
		Actions:  []assess.Action{{ItemID: "OB-04", Action: "go quarterly"}},
	}}
}

func TestReplayReviewAndRollback(t *testing.T) {
	l, _ := newLog(t)
	mustAppend(t, l, "system", TypeContextLoaded, ContextLoaded{CompanyName: "X"})              // 1
	mustAppend(t, l, "system", TypeVersionIngested, VersionIngested{Doc: "v1.md"})              // 2
	mustAppend(t, l, "system", TypeChangeAssessed, card("C1", false))                           // 3
	mustAppend(t, l, "system", TypeChangeAssessed, card("C2", true))                            // 4
	mustAppend(t, l, "P-02", TypeReviewDecided, ReviewDecided{CardID: "C1", Decision: Approve}) // 5

	st, err := Replay(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	if st.Card("C1").Status != Approved || st.Card("C2").Status != NeedsExpert {
		t.Fatalf("statuses: C1=%s C2=%s", st.Card("C1").Status, st.Card("C2").Status)
	}
	if len(st.Tasks) != 1 || st.Tasks[0].ItemID != "OB-04" {
		t.Fatalf("approval should create a task, got %+v", st.Tasks)
	}

	mustAppend(t, l, "P-01", TypeRolledBack, RolledBack{ToSeq: 4, Note: "approved too early"}) // 6
	st, err = Replay(l.Events())
	if err != nil {
		t.Fatal(err)
	}
	if st.Card("C1").Status != Pending || len(st.Tasks) != 0 {
		t.Fatalf("rollback should undo the approval: C1=%s tasks=%d", st.Card("C1").Status, len(st.Tasks))
	}
	if st.AsOfSeq != 6 {
		t.Errorf("as of %d, want 6", st.AsOfSeq)
	}

	// Decide again after the rollback; the earlier, undone approval stays in the log.
	mustAppend(t, l, "P-02", TypeReviewDecided, ReviewDecided{CardID: "C1", Decision: Reject, Note: "wrong frequency"}) // 7
	st, _ = Replay(l.Events())
	if st.Card("C1").Status != Rejected || len(st.Card("C1").Decisions) != 1 {
		t.Fatalf("C1 = %+v", st.Card("C1"))
	}
	_, undone, _ := Effective(l.Events())
	if undone[5] != 6 || len(undone) != 1 {
		t.Errorf("undone = %v, want {5: 6}", undone)
	}
	if len(l.Events()) != 7 {
		t.Error("nothing may be removed from the log")
	}
}
