package main

// Project commands: a living, auditable change-to-action workspace.
//
//	strata init     [-project DIR] [-company FILE]
//	strata ingest   [-project DIR] [-model M] FILE.md
//	strata cards    [-project DIR]
//	strata show     [-project DIR] CARD
//	strata review   [-project DIR] -as PERSON (-approve | -reject) [-note TEXT] CARD
//	strata rollback [-project DIR] -as PERSON -to SEQ -note TEXT
//	strata log      [-project DIR]
//	strata state    [-project DIR] [-json]

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"strata/internal/assess"
	"strata/internal/llm"
	"strata/internal/state"
	"strata/internal/workflow"
)

const defaultProject = ".strata"

func projectFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs, fs.String("project", defaultProject, "project directory (holds the audit log)")
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "error:", err)
	return 1
}

func runInit(args []string, stdout, stderr io.Writer) int {
	fs, dir := projectFlags("init", stderr)
	companyPath := fs.String("company", "testdata/company.json", "company context JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p, err := workflow.Init(*dir, *companyPath)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "created project %s for %s\n", *dir, p.Company.Company.Name)
	return 0
}

func runIngest(args []string, stdout, stderr io.Writer) int {
	fs, dir := projectFlags("ingest", stderr)
	model := fs.String("model", "", "OpenAI model name (default: $OPENAI_MODEL)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: strata ingest [-project DIR] [-model M] FILE.md")
		return 2
	}
	p, err := workflow.Open(*dir)
	if err != nil {
		return fail(stderr, err)
	}
	m, err := llm.NewOpenAIFromEnv(*model)
	if err != nil {
		return fail(stderr, err)
	}
	p.Parallel = assess.DefaultParallel
	events, err := p.Ingest(context.Background(), fs.Arg(0), m)
	if err != nil {
		return fail(stderr, err)
	}
	for _, e := range events {
		fmt.Fprintf(stdout, "#%-3d %s\n", e.Seq, eventSummary(e))
	}
	return 0
}

func runCards(args []string, stdout, stderr io.Writer) int {
	fs, dir := projectFlags("cards", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, st, code := openState(*dir, stderr)
	if code != 0 {
		return code
	}
	if len(st.Cards) == 0 {
		fmt.Fprintln(stdout, "no cards yet: ingest at least two versions")
		return 0
	}
	for _, c := range st.Cards {
		r := c.Result
		var who []string
		for _, rv := range r.Reviewers {
			who = append(who, rv.Name)
		}
		fmt.Fprintf(stdout, "%-4s %-13s %-19s %-14s %-13s action:%-3s -> %s\n",
			c.ID, c.Status, strings.ToUpper(string(r.Impact.Change.Type)), sections(r.Impact.Change),
			r.Direction, yn(r.ActionRequired), strings.Join(who, ", "))
	}
	return 0
}

func runShow(args []string, stdout, stderr io.Writer) int {
	fs, dir := projectFlags("show", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: strata show [-project DIR] CARD")
		return 2
	}
	_, st, code := openState(*dir, stderr)
	if code != 0 {
		return code
	}
	c := st.Card(fs.Arg(0))
	if c == nil {
		return fail(stderr, fmt.Errorf("no card %s", fs.Arg(0)))
	}
	fmt.Fprintf(stdout, "%s | %s | %s | created by event #%d\n", c.ID, c.Status, c.Transition, c.CreatedSeq)
	printResult(stdout, 0, c.Result)
	for _, d := range c.Decisions {
		fmt.Fprintf(stdout, "    decided: #%d %s %s by %s %s\n", d.Seq, d.Time.Format("2006-01-02 15:04"), d.Decision, d.Reviewer, d.Note)
	}
	return 0
}

func runReview(args []string, stdout, stderr io.Writer) int {
	fs, dir := projectFlags("review", stderr)
	as := fs.String("as", "", "reviewer person id, e.g. P-02")
	approve := fs.Bool("approve", false, "approve the card")
	reject := fs.Bool("reject", false, "reject the card (needs -note)")
	note := fs.String("note", "", "reason for the decision")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *approve == *reject {
		fmt.Fprintln(stderr, "usage: strata review [-project DIR] -as PERSON (-approve | -reject) [-note TEXT] CARD")
		return 2
	}
	p, err := workflow.Open(*dir)
	if err != nil {
		return fail(stderr, err)
	}
	decision := state.Approve
	if *reject {
		decision = state.Reject
	}
	e, err := p.Review(fs.Arg(0), *as, decision, *note)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "#%-3d %s\n", e.Seq, eventSummary(e))
	return 0
}

func runRollback(args []string, stdout, stderr io.Writer) int {
	fs, dir := projectFlags("rollback", stderr)
	as := fs.String("as", "", "person id performing the rollback")
	to := fs.Int("to", 0, "keep events up to and including this sequence number")
	note := fs.String("note", "", "reason for the rollback")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p, err := workflow.Open(*dir)
	if err != nil {
		return fail(stderr, err)
	}
	e, err := p.Rollback(*to, *as, *note)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "#%-3d %s\n", e.Seq, eventSummary(e))
	return 0
}

func runLog(args []string, stdout, stderr io.Writer) int {
	fs, dir := projectFlags("log", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p, err := workflow.Open(*dir) // Open verifies the hash chain
	if err != nil {
		return fail(stderr, err)
	}
	events := p.Log.Events()
	_, undone, err := state.Effective(events)
	if err != nil {
		return fail(stderr, err)
	}
	for _, e := range events {
		mark := ""
		if by, ok := undone[e.Seq]; ok {
			mark = fmt.Sprintf("  [undone by #%d]", by)
		}
		fmt.Fprintf(stdout, "#%-3d %s  %-6s %s%s\n", e.Seq, e.Time.Local().Format("2006-01-02 15:04:05"), e.Actor, eventSummary(e), mark)
	}
	fmt.Fprintf(stdout, "\n%d events, hash chain verified (last %s)\n", len(events), short(events[len(events)-1].Hash))
	return 0
}

func runState(args []string, stdout, stderr io.Writer) int {
	fs, dir := projectFlags("state", stderr)
	asJSON := fs.Bool("json", false, "print state as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p, st, code := openState(*dir, stderr)
	if code != 0 {
		return code
	}
	if *asJSON {
		return writeJSON(stdout, stderr, st)
	}

	fmt.Fprintf(stdout, "%s | as of event #%d\n\nversions\n", st.Company.CompanyName, st.AsOfSeq)
	for _, v := range st.Versions {
		ev := ""
		if v.Status.Evidence != nil {
			ev = quote(v.Status.Evidence.Quote)
		}
		fmt.Fprintf(stdout, "  %-16s %-6s issued %s  %s\n", v.Doc, strings.ToUpper(v.Status.Status), v.Issued, ev)
	}

	counts := map[string]int{}
	for _, c := range st.Cards {
		counts[c.Status]++
	}
	fmt.Fprintf(stdout, "\ncards: %d total | %d pending | %d needs_expert | %d approved | %d rejected\n",
		len(st.Cards), counts[state.Pending], counts[state.NeedsExpert], counts[state.Approved], counts[state.Rejected])

	fmt.Fprintln(stdout, "\nobligations")
	for _, ob := range p.Company.Obligations {
		if eff, ok := st.Obligations[ob.ID]; ok {
			line := fmt.Sprintf("  %-6s %-10s since %s", ob.ID, eff.Status, eff.EffectiveDate)
			if eff.Due != "" {
				line += fmt.Sprintf(", due %s per %s @ %s", eff.Due, quote(eff.DueEvidence.Quote), eff.DueEvidence)
			}
			fmt.Fprintln(stdout, line)
		} else {
			fmt.Fprintf(stdout, "  %-6s %-10s (no change yet)\n", ob.ID, ob.Status)
		}
	}

	fmt.Fprintln(stdout, "\ntasks (from approved cards)")
	if len(st.Tasks) == 0 {
		fmt.Fprintln(stdout, "  none")
	}
	for _, t := range st.Tasks {
		owner := t.Owner
		if per := p.Company.Person(t.Owner); per != nil {
			owner = per.Name
		}
		fmt.Fprintf(stdout, "  %-14s %-7s %s  (%s)\n", owner, t.ItemID, t.Action, t.CardID)
	}
	return 0
}

func openState(dir string, stderr io.Writer) (*workflow.Project, *state.State, int) {
	p, err := workflow.Open(dir)
	if err != nil {
		return nil, nil, fail(stderr, err)
	}
	st, err := p.State()
	if err != nil {
		return nil, nil, fail(stderr, err)
	}
	return p, st, 0
}

// eventSummary is a one-line description of an event for the audit log view.
func eventSummary(e state.Event) string {
	switch e.Type {
	case state.TypeContextLoaded:
		var d state.ContextLoaded
		_ = json.Unmarshal(e.Data, &d)
		return fmt.Sprintf("%-17s %s (sha256 %s)", e.Type, d.CompanyName, short(d.CompanySHA256))
	case state.TypeVersionIngested:
		var d state.VersionIngested
		_ = json.Unmarshal(e.Data, &d)
		s := fmt.Sprintf("%-17s %s %s", e.Type, d.Doc, strings.ToUpper(d.Status.Status))
		if d.Status.Escalate {
			s += " (status escalated)"
		}
		if len(d.Effects) > 0 {
			s += fmt.Sprintf(", %d obligations now effective", len(d.Effects))
		}
		return s
	case state.TypeChangeAssessed:
		var d state.ChangeAssessed
		_ = json.Unmarshal(e.Data, &d)
		ch := d.Result.Impact.Change
		s := fmt.Sprintf("%-17s %s %s %s %s", e.Type, d.CardID, strings.ToUpper(string(ch.Type)), sections(ch), d.Result.Direction)
		if d.Result.Escalate {
			s += " NEEDS EXPERT"
		}
		return s
	case state.TypeReviewDecided:
		var d state.ReviewDecided
		_ = json.Unmarshal(e.Data, &d)
		s := fmt.Sprintf("%-17s %s %s", e.Type, d.CardID, d.Decision)
		if d.Note != "" {
			s += " " + quote(d.Note)
		}
		return s
	case state.TypeRolledBack:
		var d state.RolledBack
		_ = json.Unmarshal(e.Data, &d)
		return fmt.Sprintf("%-17s to #%d %s", e.Type, d.ToSeq, quote(d.Note))
	}
	return e.Type
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
