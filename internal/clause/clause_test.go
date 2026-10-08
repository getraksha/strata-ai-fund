package clause

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, name string) *Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(name, string(data))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseClauseCounts(t *testing.T) {
	for name, want := range map[string]int{
		"v1_proposed.md": 17,
		"v2_revised.md":  18,
		"v3_final.md":    18,
	} {
		if got := len(load(t, name).Clauses); got != want {
			t.Errorf("%s: got %d clauses, want %d", name, got, want)
		}
	}
}

func TestSpansPointAtSource(t *testing.T) {
	for _, name := range []string{"v1_proposed.md", "v2_revised.md", "v3_final.md"} {
		d := load(t, name)
		for _, c := range d.Clauses {
			if got := d.Source[c.TextSpan.Start:c.TextSpan.End]; got != c.Text {
				t.Errorf("%s §%s: text span mismatch: %q vs %q", name, c.ID, got, c.Text)
			}
			if got := d.Source[c.HeadingSpan.Start:c.HeadingSpan.End]; got != c.Heading {
				t.Errorf("%s §%s: heading span mismatch: %q vs %q", name, c.ID, got, c.Heading)
			}
			if !strings.HasPrefix(d.Source[c.Span.Start:c.Span.End], "### "+c.ID+" ") {
				t.Errorf("%s §%s: clause span does not start at its heading", name, c.ID)
			}
		}
	}
}

func TestTitleAndPreamble(t *testing.T) {
	d := load(t, "v3_final.md")
	if !strings.HasPrefix(d.Title, "DECISION ADOPTING") {
		t.Errorf("title = %q", d.Title)
	}
	if pre := d.Source[d.Preamble.Start:d.Preamble.End]; !strings.Contains(pre, "is adopted") {
		t.Errorf("preamble = %q", pre)
	}
}

func TestFrontMatterMeta(t *testing.T) {
	d := load(t, "v1_proposed.md")
	if d.Meta["docket"] != "R.26-04-017" || d.Meta["issued"] != "2026-04-15" {
		t.Errorf("meta = %v", d.Meta)
	}
}

func TestReflowedClauseHasSameKey(t *testing.T) {
	v2, v3 := load(t, "v2_revised.md"), load(t, "v3_final.md")
	find := func(d *Document, id string) Clause {
		for _, c := range d.Clauses {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("§%s not found in %s", id, d.Name)
		return Clause{}
	}
	a, b := find(v2, "3.2"), find(v3, "3.2")
	if a.Text == b.Text {
		t.Fatal("fixture should differ in line wrapping for §3.2")
	}
	if Key(v2.Tokens(a)) != Key(v3.Tokens(b)) {
		t.Error("reflowed §3.2 should normalize to the same key")
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := []struct {
		name, src, wantErr string
	}{
		{"no clauses", "# Title\n\nJust prose, no sections.\n", "no numbered clauses"},
		{"unnumbered clause", "# T\n### Scope\ntext\n", "section number"},
		{"duplicate id", "# T\n### 1.1 A\nx\n### 1.1 B\ny\n", "duplicate clause"},
		{"orphan text", "# T\n## 1. Group\nstray text\n### 1.1 A\nx\n", "outside any numbered clause"},
		{"unclosed front matter", "---\nfoo: bar\n# T\n### 1.1 A\nx\n", "front matter"},
		{"deep heading", "# T\n### 1.1 A\nx\n#### Sub\ny\n", "heading level 4"},
	}
	for _, tc := range cases {
		_, err := Parse(tc.name, tc.src)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: got err %v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}
