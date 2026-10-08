// Command strata turns successive versions of a regulatory proceeding into
// cited, company-specific change assessments.
//
//	strata diff   [-json] OLD.md NEW.md
//	strata map    [-json] [-company FILE] OLD.md NEW.md
//	strata assess [-json] [-company FILE] [-model M] [-min-confidence X] OLD.md NEW.md
//	strata eval   [-json] [-company FILE] [-model M] [-expected FILE] [-dir DIR]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"strata/internal/assess"
	"strata/internal/clause"
	"strata/internal/company"
	"strata/internal/diff"
	"strata/internal/eval"
	"strata/internal/impact"
	"strata/internal/llm"
)

const usage = `usage:
  strata diff   [-json] OLD.md NEW.md
  strata map    [-json] [-company FILE] OLD.md NEW.md
  strata assess [-json] [-company FILE] [-model M] [-min-confidence X] OLD.md NEW.md
  strata eval   [-json] [-company FILE] [-model M] [-expected FILE] [-dir DIR] [-runs N]

project (living state + audit log, default dir .strata):
  strata init     [-project DIR] [-company FILE]
  strata ingest   [-project DIR] [-model M] FILE.md
  strata cards    [-project DIR]
  strata show     [-project DIR] CARD
  strata review   [-project DIR] -as PERSON (-approve | -reject) [-note TEXT] CARD
  strata rollback [-project DIR] -as PERSON -to SEQ -note TEXT
  strata log      [-project DIR]
  strata state    [-project DIR] [-json]
  strata serve    [-project DIR] [-addr HOST:PORT] [-docs DIR] [-model M]   web UI

assess/eval/ingest call OpenAI: set OPENAI_API_KEY, and -model or OPENAI_MODEL.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string, io.Writer, io.Writer) int{
		"diff": runDiff, "map": runMap, "assess": runAssess, "eval": runEval,
		"init": runInit, "ingest": runIngest, "cards": runCards, "show": runShow,
		"review": runReview, "rollback": runRollback, "log": runLog, "state": runState,
		"serve": runServe,
	}
	run, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	os.Exit(run(os.Args[2:], os.Stdout, os.Stderr))
}

// ---- commands ----

func runDiff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	_, _, res, err := loadAndDiff(fs.Arg(0), fs.Arg(1))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if *asJSON {
		return writeJSON(stdout, stderr, res)
	}
	printDiff(stdout, res)
	return 0
}

func runMap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	companyPath := fs.String("company", "testdata/company.json", "company context JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cc, err := company.Load(*companyPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	_, _, rep, err := pipeline(fs.Arg(0), fs.Arg(1), cc)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if *asJSON {
		return writeJSON(stdout, stderr, rep)
	}
	printMap(stdout, rep)
	return 0
}

func runAssess(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("assess", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON (includes raw model output)")
	companyPath := fs.String("company", "testdata/company.json", "company context JSON")
	model := fs.String("model", "", "OpenAI model name (default: $OPENAI_MODEL)")
	minConf := fs.Float64("min-confidence", assess.DefaultMinConfidence, "escalate below this model confidence")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cc, err := company.Load(*companyPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	m, err := llm.NewOpenAIFromEnv(*model)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	a, b, rep, err := pipeline(fs.Arg(0), fs.Arg(1), cc)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	as := &assess.Assessor{Model: m, Company: cc, MinConfidence: *minConf, Parallel: assess.DefaultParallel}
	out := as.Assess(context.Background(), rep, a, b)
	if *asJSON {
		return writeJSON(stdout, stderr, out)
	}
	printAssess(stdout, out)
	return 0
}

func runEval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print scorecards and full assessments as JSON")
	companyPath := fs.String("company", "testdata/company.json", "company context JSON")
	model := fs.String("model", "", "OpenAI model name (default: $OPENAI_MODEL)")
	minConf := fs.Float64("min-confidence", assess.DefaultMinConfidence, "escalate below this model confidence")
	expPath := fs.String("expected", "testdata/expected_changes.json", "answer key")
	dir := fs.String("dir", "testdata", "directory holding the version files")
	runs := fs.Int("runs", 1, "repeat the whole eval N times to measure stability")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *runs < 1 {
		fmt.Fprintln(stderr, "error: -runs must be at least 1")
		return 2
	}
	exp, err := eval.LoadExpected(*expPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cc, err := company.Load(*companyPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	m, err := llm.NewOpenAIFromEnv(*model)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	as := &assess.Assessor{Model: m, Company: cc, MinConfidence: *minConf, Parallel: assess.DefaultParallel}
	ctx := context.Background()

	// Diff + map are deterministic: compute once, reuse in every run.
	type prepared struct {
		tr   eval.Transition
		a, b *clause.Document
		rep  impact.Report
	}
	var preps []prepared
	isTarget := map[string]bool{}
	for _, tr := range exp.Transitions {
		a, b, rep, err := pipeline(filepath.Join(*dir, tr.From), filepath.Join(*dir, tr.To), cc)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		preps = append(preps, prepared{tr, a, b, rep})
		isTarget[tr.To] = true
	}
	// Versions that are never a transition target (the first draft) get their own status check.
	var extra []*clause.Document
	for _, v := range exp.Versions {
		if isTarget[v.File] {
			continue
		}
		d, err := load(filepath.Join(*dir, v.File))
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		extra = append(extra, d)
	}

	type output struct {
		Model       string                           `json:"model"`
		Runs        int                              `json:"runs"`
		Summaries   []eval.Summary                   `json:"summaries"`
		Statuses    map[string][]assess.StatusResult `json:"statuses_per_doc"`
		Assessments [][]assess.Report                `json:"assessments_per_run"`
	}
	out := output{Model: m.Name(), Runs: *runs, Statuses: map[string][]assess.StatusResult{}}
	cards := make([][]eval.Scorecard, len(preps))

	for run := 1; run <= *runs; run++ {
		var reports []assess.Report
		for _, d := range extra {
			fmt.Fprintf(stderr, "run %d/%d: status of %s ...\n", run, *runs, d.Name)
			out.Statuses[d.Name] = append(out.Statuses[d.Name], as.Status(ctx, d))
		}
		for i, p := range preps {
			fmt.Fprintf(stderr, "run %d/%d: assessing %s -> %s ...\n", run, *runs, p.tr.From, p.tr.To)
			ar := as.Assess(ctx, p.rep, p.a, p.b)
			reports = append(reports, ar)
			cards[i] = append(cards[i], eval.Score(p.tr, ar))
			out.Statuses[ar.Status.Doc] = append(out.Statuses[ar.Status.Doc], ar.Status)
		}
		out.Assessments = append(out.Assessments, reports)
	}
	for _, c := range cards {
		out.Summaries = append(out.Summaries, eval.Summarize(c))
	}

	if *asJSON {
		return writeJSON(stdout, stderr, out)
	}
	printEval(stdout, m.Name(), *runs, out.Summaries, out.Statuses, exp)
	return 0
}

// ---- pipeline ----

// loadAndDiff parses both documents, diffs them, and refuses to continue if
// any citation does not match the source.
func loadAndDiff(oldPath, newPath string) (*clause.Document, *clause.Document, diff.Result, error) {
	a, err := load(oldPath)
	if err != nil {
		return nil, nil, diff.Result{}, err
	}
	b, err := load(newPath)
	if err != nil {
		return nil, nil, diff.Result{}, err
	}
	res := diff.Compare(a, b)
	if err := diff.VerifyAll(res, a, b); err != nil {
		return nil, nil, diff.Result{}, fmt.Errorf("citation verification failed: %w", err)
	}
	return a, b, res, nil
}

// pipeline runs diff + map with citation verification at each stage.
func pipeline(oldPath, newPath string, cc *company.Context) (*clause.Document, *clause.Document, impact.Report, error) {
	a, b, res, err := loadAndDiff(oldPath, newPath)
	if err != nil {
		return nil, nil, impact.Report{}, err
	}
	rep, err := impact.Map(res, a, b, cc)
	if err != nil {
		return nil, nil, impact.Report{}, err
	}
	if err := impact.VerifyAll(rep, a, b); err != nil {
		return nil, nil, impact.Report{}, fmt.Errorf("citation verification failed: %w", err)
	}
	return a, b, rep, nil
}

func load(path string) (*clause.Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return clause.Parse(filepath.Base(path), string(data))
}

func writeJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// ---- text output ----

func printDiff(w io.Writer, r diff.Result) {
	fmt.Fprintf(w, "%s -> %s: %d change(s), %d clause(s) unchanged\n", r.OldDoc, r.NewDoc, len(r.Changes), r.Unchanged)
	fmt.Fprintln(w, "all citations verified against source text")
	for n, ch := range r.Changes {
		fmt.Fprintf(w, "\n[%d] %-19s %s", n+1, strings.ToUpper(string(ch.Type)), sections(ch))
		if ch.Type != diff.Added && ch.Type != diff.Deleted {
			fmt.Fprintf(w, "  (similarity %.2f)", ch.Similarity)
		}
		fmt.Fprintln(w)
		switch ch.Type {
		case diff.Added:
			fmt.Fprintf(w, "    + %s\n      @ %s\n", quote(ch.New.Quote), ch.New)
		case diff.Deleted:
			fmt.Fprintf(w, "    - %s\n      @ %s\n", quote(ch.Old.Quote), ch.Old)
		case diff.Renumbered:
			fmt.Fprintf(w, "    old @ %s\n    new @ %s\n", ch.Old, ch.New)
		default:
			for _, e := range ch.Edits {
				if e.Old != nil {
					fmt.Fprintf(w, "    - %s\n      @ %s\n", quote(e.Old.Quote), e.Old)
				}
				if e.New != nil {
					fmt.Fprintf(w, "    + %s\n      @ %s\n", quote(e.New.Quote), e.New)
				}
			}
		}
	}
}

func printMap(w io.Writer, r impact.Report) {
	fmt.Fprintf(w, "%s -> %s | docket %s | %s\n", r.OldDoc, r.NewDoc, r.Docket, r.Company)
	fmt.Fprintln(w, "all citations verified against source text")
	for n, im := range r.Impacts {
		ch := im.Change
		fmt.Fprintf(w, "\n[%d] %-19s %-14s %s\n", n+1, strings.ToUpper(string(ch.Type)), sections(ch), summary(ch))
		switch {
		case im.Gap:
			fmt.Fprintf(w, "    GAP: %s\n", im.Note)
		case im.Note != "":
			fmt.Fprintf(w, "    note: %s\n", im.Note)
		}
		if len(im.Topics) > 0 {
			parts := make([]string, len(im.Topics))
			for i, t := range im.Topics {
				parts[i] = fmt.Sprintf("%s (%q L%d)", t.Topic, t.Evidence.Quote, t.Evidence.LineStart)
			}
			fmt.Fprintf(w, "    topics: %s\n", strings.Join(parts, ", "))
		}
		for _, h := range im.Hits {
			fmt.Fprintf(w, "    %-7s %-10s %-30s %s\n", h.ID, h.Kind, string(h.How)+" "+h.Via, h.Title)
		}
		printReviewers(w, im.Reviewers)
	}
}

func printAssess(w io.Writer, r assess.Report) {
	fmt.Fprintf(w, "%s -> %s | docket %s | %s | model %s\n", r.OldDoc, r.NewDoc, r.Docket, r.Company, r.Model)
	fmt.Fprintf(w, "all quotes below were verified against the source text\n\n")
	printStatus(w, r.Status, "")

	for n, res := range r.Results {
		printResult(w, n+1, res)
	}
}

// printResult prints one assessed change. n > 0 adds a "[n]" prefix.
func printResult(w io.Writer, n int, res assess.Result) {
	ch := res.Impact.Change
	tag := "rule change: " + yn(res.Material) + ", company action: " + yn(res.ActionRequired)
	if res.Escalate {
		tag += ", ESCALATED"
	}
	prefix := ""
	if n > 0 {
		prefix = fmt.Sprintf("[%d] ", n)
	}
	fmt.Fprintf(w, "\n%s%-19s %-14s %s | %s | conf %.2f | by %s\n",
		prefix, strings.ToUpper(string(ch.Type)), sections(ch), res.Direction, tag, res.Confidence, res.DecidedBy)
	fmt.Fprintf(w, "    change:   %s\n", summary(ch))
	if res.Summary != "" {
		fmt.Fprintf(w, "    meaning:  %s\n", res.Summary)
	}
	for _, c := range res.Evidence {
		fmt.Fprintf(w, "    evidence: %s @ %s\n", quote(c.Quote), c)
	}
	for _, a := range res.Actions {
		fmt.Fprintf(w, "    action:   %-7s %s\n", a.ItemID, a.Action)
	}
	for _, reason := range res.Reasons {
		fmt.Fprintf(w, "    escalate: %s\n", reason)
	}
	printReviewers(w, res.Reviewers)
}

// printEval prints one table per transition. Verdicts are material/action/escalate.
// "ok runs" counts, per judgment, how many runs matched the answer key.
func printEval(w io.Writer, model string, runs int, sums []eval.Summary, statuses map[string][]assess.StatusResult, exp *eval.Expected) {
	fmt.Fprintf(w, "eval | model %s | runs %d | verdict = rule-change-material/company-action/escalate\n", model, runs)
	var checks, mat, act, esc, rows, stable, qv, qr int
	for _, s := range sums {
		fmt.Fprintf(w, "\n%s\n", s.Transition)
		fmt.Fprintf(w, "  %-3s %-24s %-13s %-14s %-6s %s\n", "", "change", "want", "ok runs m/a/e", "stable", "got per run")
		for _, r := range s.Rows {
			mark := "ok"
			if r.MaterialOK != runs || r.ActionOK != runs || r.EscalateOK != runs {
				mark = "XX"
			}
			fmt.Fprintf(w, "  %-3s %-24s %-13s %-14s %-6s %s\n", mark, r.Key, r.Want.String(),
				fmt.Sprintf("%d/%d/%d", r.MaterialOK, r.ActionOK, r.EscalateOK), yn(r.Stable), strings.Join(r.Got, " | "))
			if mark == "XX" {
				fmt.Fprintf(w, "      note: %s\n", r.Note)
			}
		}
		for _, u := range s.Unexpected {
			fmt.Fprintf(w, "  ??  %-24s not in answer key\n", u)
		}
		fmt.Fprintf(w, "  material %d/%d | action %d/%d | escalate %d/%d | stable %d/%d | quotes verified %d, rejected %d\n",
			s.MaterialCorrect, s.Checks, s.ActionCorrect, s.Checks, s.EscalateCorrect, s.Checks,
			s.Stable, len(s.Rows), s.QuotesVerified, s.QuotesRejected)
		checks, mat, act, esc = checks+s.Checks, mat+s.MaterialCorrect, act+s.ActionCorrect, esc+s.EscalateCorrect
		rows, stable, qv, qr = rows+len(s.Rows), stable+s.Stable, qv+s.QuotesVerified, qr+s.QuotesRejected
	}

	fmt.Fprintf(w, "\ndocument status\n")
	statusOK, statusTotal := 0, 0
	for _, v := range exp.Versions {
		results := statuses[v.File]
		ok := 0
		for _, s := range results {
			if s.Status == v.Status && !s.Escalate {
				ok++
			}
		}
		statusOK, statusTotal = statusOK+ok, statusTotal+len(results)
		mark := "ok"
		if ok != len(results) || len(results) == 0 {
			mark = "XX"
		}
		if len(results) > 0 {
			// show the last run's evidence; JSON output has every run
			printStatus(w, results[len(results)-1], fmt.Sprintf("  %-3s want %-6s %d/%d runs  ", mark, v.Status, ok, len(results)))
		}
	}

	fmt.Fprintf(w, "\nTOTAL material %d/%d | action %d/%d | escalate %d/%d | stable %d/%d | status %d/%d | quotes verified %d, rejected %d\n",
		mat, checks, act, checks, esc, checks, stable, rows, statusOK, statusTotal, qv, qr)
}

func printStatus(w io.Writer, s assess.StatusResult, prefix string) {
	ev := "(no verified evidence)"
	if s.Evidence != nil {
		ev = quote(s.Evidence.Quote) + " @ " + s.Evidence.String()
	}
	fmt.Fprintf(w, "%s%s: %s (conf %.2f) %s\n", prefix, s.Doc, strings.ToUpper(s.Status), s.Confidence, ev)
	for _, r := range s.Reasons {
		fmt.Fprintf(w, "%s    escalate: %s\n", strings.Repeat(" ", len(prefix)), r)
	}
}

func printReviewers(w io.Writer, rs []impact.Reviewer) {
	for _, rv := range rs {
		fmt.Fprintf(w, "    -> %s (%s): %s\n", rv.Name, rv.Role, strings.Join(rv.Reasons, "; "))
	}
}

// summary is a one-line description of what changed in a clause.
func summary(ch diff.Change) string {
	switch ch.Type {
	case diff.Added:
		return "+ " + heading(ch.New.Quote)
	case diff.Deleted:
		return "- " + heading(ch.Old.Quote)
	case diff.Renumbered:
		return "(text unchanged)"
	}
	var parts []string
	for i, e := range ch.Edits {
		if i == 2 {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(ch.Edits)-2))
			break
		}
		switch {
		case e.Old != nil && e.New != nil:
			parts = append(parts, quote(e.Old.Quote)+" -> "+quote(e.New.Quote))
		case e.New != nil:
			parts = append(parts, "+"+quote(e.New.Quote))
		default:
			parts = append(parts, "-"+quote(e.Old.Quote))
		}
	}
	return strings.Join(parts, "; ")
}

func heading(q string) string {
	if i := strings.IndexByte(q, '\n'); i >= 0 {
		q = q[:i]
	}
	return strings.TrimPrefix(q, "### ")
}

func sections(ch diff.Change) string {
	switch {
	case ch.OldID == "":
		return "§" + ch.NewID
	case ch.NewID == "":
		return "§" + ch.OldID
	case ch.OldID == ch.NewID:
		return "§" + ch.OldID
	default:
		return "§" + ch.OldID + " -> §" + ch.NewID
	}
}

// quote collapses whitespace for display only; citations keep the exact text.
func quote(s string) string {
	return `"` + strings.Join(strings.Fields(s), " ") + `"`
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
