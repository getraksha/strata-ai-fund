package impact

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"strata/internal/clause"
	"strata/internal/company"
	"strata/internal/diff"
)

func loadDoc(t *testing.T, name string) *clause.Document {
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

func run(t *testing.T, oldName, newName string) Report {
	t.Helper()
	a, b := loadDoc(t, oldName), loadDoc(t, newName)
	ctx, err := company.Load(filepath.Join("..", "..", "testdata", "company.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Map(diff.Compare(a, b), a, b, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAll(rep, a, b); err != nil {
		t.Fatalf("citation verification failed: %v", err)
	}
	return rep
}

// find returns the impact whose new clause is id (or old clause, for deletions).
func find(t *testing.T, rep Report, id string) Impact {
	t.Helper()
	for _, im := range rep.Impacts {
		if im.Change.NewID == id || (im.Change.NewID == "" && im.Change.OldID == id) {
			return im
		}
	}
	t.Fatalf("no impact for §%s", id)
	return Impact{}
}

func hitIDs(im Impact, how How) []string {
	var out []string
	for _, h := range im.Hits {
		if h.How == how {
			out = append(out, h.ID)
		}
	}
	sort.Strings(out)
	return out
}

func reviewerIDs(im Impact) []string {
	var out []string
	for _, r := range im.Reviewers {
		out = append(out, r.PersonID)
	}
	sort.Strings(out)
	return out
}

type want struct {
	section   string
	direct    []string
	via       []string
	reviewers []string
	gap       bool
	company   bool // company profile hit expected
}

func check(t *testing.T, rep Report, cases []want) {
	t.Helper()
	for _, w := range cases {
		im := find(t, rep, w.section)
		eq := func(what string, got, exp []string) {
			sort.Strings(exp)
			if strings.Join(got, ",") != strings.Join(exp, ",") {
				t.Errorf("§%s %s: got %v, want %v", w.section, what, got, exp)
			}
		}
		eq("direct", hitIDs(im, Direct), w.direct)
		eq("via", hitIDs(im, ViaObligation), w.via)
		eq("reviewers", reviewerIDs(im), w.reviewers)
		if im.Gap != w.gap {
			t.Errorf("§%s gap = %v, want %v", w.section, im.Gap, w.gap)
		}
		if got := len(hitIDs(im, Profile)) > 0; got != w.company {
			t.Errorf("§%s company profile hit = %v, want %v", w.section, got, w.company)
		}
	}
}

func TestProposedToRevised(t *testing.T) {
	rep := run(t, "v1_proposed.md", "v2_revised.md")
	check(t, rep, []want{
		{section: "3.3", direct: []string{"OB-01"}, via: []string{"PRJ-01", "DOC-01", "DOC-03"}, reviewers: []string{"P-01"}},
		{section: "4.2", direct: []string{"OB-04"}, via: []string{"DOC-03"}, reviewers: []string{"P-01", "P-02"}},
		{section: "4.3", direct: []string{"OB-05"}, via: []string{"PRJ-03", "DOC-02"}, reviewers: []string{"P-02"}},
		{section: "5.3", reviewers: []string{"P-03"}, gap: true},
		{section: "6.2", direct: []string{"OB-09"}, via: []string{"PRJ-04", "DOC-05"}, reviewers: []string{"P-05"}},
	})

	// The new medical-baseline clause has no linked obligation, but topic
	// matching must still surface the PSPS playbook where it belongs.
	gap := find(t, rep, "5.3")
	if got := strings.Join(hitIDs(gap, Topic), ","); !strings.Contains(got, "DOC-04") {
		t.Errorf("§5.3 topic hits = %s, want DOC-04 included", got)
	}

	renum := find(t, rep, "2.4")
	if len(renum.Hits) != 0 || !strings.Contains(renum.Note, "no company impact") {
		t.Errorf("renumbered §2.3->§2.4 should have no impact, got %+v", renum)
	}
}

func TestRevisedToFinal(t *testing.T) {
	rep := run(t, "v2_revised.md", "v3_final.md")
	check(t, rep, []want{
		{section: "1.2", reviewers: []string{"P-01"}, company: true},
		{section: "4.1", direct: []string{"OB-03"}, via: []string{"PRJ-02", "DOC-02"}, reviewers: []string{"P-02"}},
		{section: "5.1", direct: []string{"OB-06"}, via: []string{"DOC-04"}, reviewers: []string{"P-03"}},
		{section: "7.1", reviewers: []string{"P-01"}, company: true},
	})
}

func TestTopicEvidenceQuotesTheKeyword(t *testing.T) {
	rep := run(t, "v1_proposed.md", "v2_revised.md")
	im := find(t, rep, "5.3")
	got := map[string]string{}
	for _, tm := range im.Topics {
		got[tm.Topic] = tm.Evidence.Quote
	}
	if !strings.EqualFold(got["psps"], "PSPS") {
		t.Errorf("psps evidence = %q", got["psps"])
	}
	if got["customer-notification"] == "" {
		t.Errorf("customer-notification topic not detected in §5.3: %v", got)
	}
}

func TestDocketMismatchIsAnError(t *testing.T) {
	a, b := loadDoc(t, "v1_proposed.md"), loadDoc(t, "v2_revised.md")
	b.Meta["docket"] = "R.99-99-999"
	ctx, err := company.Load(filepath.Join("..", "..", "testdata", "company.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Map(diff.Compare(a, b), a, b, ctx); err == nil {
		t.Fatal("diffing two different dockets should fail")
	}
}
