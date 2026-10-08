package company

import (
	"path/filepath"
	"strings"
	"testing"
)

func ids[T any](items []*T, id func(*T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = id(it)
	}
	return out
}

func TestLoadTestdata(t *testing.T) {
	ctx, err := Load(filepath.Join("..", "..", "testdata", "company.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Obligations) != 9 || len(ctx.Projects) != 4 || len(ctx.Documents) != 5 || len(ctx.People) != 5 {
		t.Fatalf("unexpected sizes: %d obligations, %d projects, %d documents, %d people",
			len(ctx.Obligations), len(ctx.Projects), len(ctx.Documents), len(ctx.People))
	}

	obs := ids(ctx.ObligationsForClause("R.26-04-017", "4.2"), func(o *Obligation) string { return o.ID })
	if strings.Join(obs, ",") != "OB-04" {
		t.Errorf("§4.2 -> %v, want [OB-04]", obs)
	}
	docs := ids(ctx.DocumentsFor("OB-01"), func(d *Document) string { return d.ID })
	if strings.Join(docs, ",") != "DOC-01,DOC-03" {
		t.Errorf("OB-01 documents = %v", docs)
	}
	prjs := ids(ctx.ProjectsFor("OB-01"), func(p *Project) string { return p.ID })
	if strings.Join(prjs, ",") != "PRJ-01" {
		t.Errorf("OB-01 projects = %v", prjs)
	}
	if got := ctx.ObligationsForClause("OTHER-DOCKET", "4.2"); len(got) != 0 {
		t.Errorf("docket must be part of the clause link, got %v", got)
	}
}

func TestParseRejectsBrokenReferences(t *testing.T) {
	src := `{
	  "company": {"id": "X"},
	  "topics": [{"id": "t"}],
	  "people": [{"id": "P-1"}],
	  "routing": {"default_reviewer": "P-1", "escalation_reviewer": "P-9"},
	  "obligations": [
	    {"id": "OB-1", "owner": "P-2", "topics": ["nope"], "project_ids": ["PRJ-9"],
	     "sources": [{"docket": "D", "clause": "1"}]},
	    {"id": "OB-1", "owner": "P-1"}
	  ],
	  "projects": [],
	  "documents": [{"id": "DOC-1", "owner": "P-1", "obligation_ids": ["OB-7"]}]
	}`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("broken context accepted")
	}
	for _, want := range []string{
		`routing.escalation_reviewer: unknown owner "P-9"`,
		`OB-1: unknown owner "P-2"`,
		`unknown topic "nope"`,
		`unknown project "PRJ-9"`,
		`duplicate obligation OB-1`,
		`OB-1: no source clause`,
		`DOC-1: unknown obligation "OB-7"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got:\n%v", want, err)
		}
	}
}
