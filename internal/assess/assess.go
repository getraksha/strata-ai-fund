// Package assess adds judgment to each change: did the rule materially change,
// does THIS company have to act, which way does it cut, what should the
// company do, and how sure are we.
//
// The model only interprets. Everything it claims is checked by plain code:
//   - every quote must be found verbatim in the clause it says it came from;
//     found quotes become real citations, missing ones force escalation
//   - every action must target an item the mapper found (or "NEW")
//   - a material judgment with no verified quote is escalated
//   - "unclear", "ambiguous", low confidence, model errors and malformed
//     output are escalated to the escalation reviewer, never guessed
//
// Renumbered clauses with identical text are decided by rule, without a model call.
package assess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"strata/internal/cite"
	"strata/internal/clause"
	"strata/internal/company"
	"strata/internal/diff"
	"strata/internal/impact"
	"strata/internal/llm"
)

// DefaultMinConfidence is the escalation threshold on model confidence.
const DefaultMinConfidence = 0.7

// NewItem is the action target for a proposed new obligation.
const NewItem = "NEW"

// Decision sources.
const (
	ByRule  = "rule"
	ByModel = "model"
	ByError = "error"
)

type Quote struct {
	Version string `json:"version"`
	Text    string `json:"text"`
}

type Action struct {
	ItemID string `json:"item_id"`
	Action string `json:"action"`
}

// modelChange is the exact shape the model must return.
type modelChange struct {
	Material        bool     `json:"rule_change_material"`
	ActionRequired  bool     `json:"company_action_required"`
	Direction       string   `json:"direction"`
	Summary         string   `json:"summary"`
	Rationale       string   `json:"rationale"`
	Quotes          []Quote  `json:"quotes"`
	Actions         []Action `json:"actions"`
	Confidence      float64  `json:"confidence"`
	Ambiguous       bool     `json:"ambiguous"`
	AmbiguityReason string   `json:"ambiguity_reason"`
}

type modelStatus struct {
	Status     string  `json:"status"`
	Quote      string  `json:"quote"`
	Confidence float64 `json:"confidence"`
}

// Result is the assessed version of one impact.
type Result struct {
	Impact         impact.Impact     `json:"impact"`
	DecidedBy      string            `json:"decided_by"`
	Material       bool              `json:"rule_change_material"`    // the rule's substance changed
	ActionRequired bool              `json:"company_action_required"` // this company must do something
	Direction      string            `json:"direction"`
	Summary        string            `json:"summary"`
	Rationale      string            `json:"rationale,omitempty"`
	Evidence       []cite.Citation   `json:"evidence"`
	QuotesRejected int               `json:"quotes_rejected"`
	Actions        []Action          `json:"actions"`
	Confidence     float64           `json:"confidence"`
	Escalate       bool              `json:"escalate"`
	Reasons        []string          `json:"escalation_reasons,omitempty"`
	Reviewers      []impact.Reviewer `json:"reviewers"`
	Raw            json.RawMessage   `json:"raw_model_output,omitempty"`
}

// StatusResult is the draft/final classification of one document.
type StatusResult struct {
	Doc        string          `json:"doc"`
	DecidedBy  string          `json:"decided_by"`
	Status     string          `json:"status"`
	Evidence   *cite.Citation  `json:"evidence,omitempty"`
	Confidence float64         `json:"confidence"`
	Escalate   bool            `json:"escalate"`
	Reasons    []string        `json:"escalation_reasons,omitempty"`
	Raw        json.RawMessage `json:"raw_model_output,omitempty"`
}

// Report is the assessment of one version transition.
type Report struct {
	OldDoc  string       `json:"old_doc"`
	NewDoc  string       `json:"new_doc"`
	Docket  string       `json:"docket"`
	Company string       `json:"company"`
	Model   string       `json:"model"`
	Status  StatusResult `json:"new_doc_status"`
	Results []Result     `json:"results"`
}

// Assessor runs the model and applies the guardrails.
type Assessor struct {
	Model         llm.Model
	Company       *company.Context
	MinConfidence float64 // 0 means DefaultMinConfidence
	Parallel      int     // concurrent model calls per Assess; 0 or 1 means sequential
}

// DefaultParallel is what the CLI and UI use.
const DefaultParallel = 4

func (as *Assessor) minConfidence() float64 {
	if as.MinConfidence > 0 {
		return as.MinConfidence
	}
	return DefaultMinConfidence
}

// Assess classifies the new document and assesses every change in rep.
func (as *Assessor) Assess(ctx context.Context, rep impact.Report, a, b *clause.Document) Report {
	out := Report{
		OldDoc: rep.OldDoc, NewDoc: rep.NewDoc, Docket: rep.Docket, Company: rep.Company,
		Model:  as.Model.Name(),
		Status: as.Status(ctx, b),
	}
	// Changes are independent: assess up to Parallel at a time, keeping order.
	workers := as.Parallel
	if workers < 1 {
		workers = 1
	}
	out.Results = make([]Result, len(rep.Impacts))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, im := range rep.Impacts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, im impact.Impact) {
			defer wg.Done()
			defer func() { <-sem }()
			out.Results[i] = as.assessChange(ctx, im, a, b)
		}(i, im)
	}
	wg.Wait()
	return out
}

// ---- change assessment ----

type clauseIn struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type editIn struct {
	Old string `json:"old"`
	New string `json:"new"`
}

type itemIn struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Link        string   `json:"link"`
	Requirement string   `json:"requirement,omitempty"`
	Status      string   `json:"status,omitempty"`
	Assumptions []string `json:"assumptions,omitempty"`
}

type companyIn struct {
	Name            string `json:"name"`
	Customers       int    `json:"customers"`
	HFTDTiersServed []int  `json:"hftd_tiers_served"`
}

type changeIn struct {
	Company       companyIn `json:"company"`
	ChangeType    string    `json:"change_type"`
	OldClause     *clauseIn `json:"old_clause"`
	NewClause     *clauseIn `json:"new_clause"`
	Edits         []editIn  `json:"edits"`
	Gap           bool      `json:"gap"`
	MapperNote    string    `json:"mapper_note,omitempty"`
	AffectedItems []itemIn  `json:"affected_items"`
}

func (as *Assessor) assessChange(ctx context.Context, im impact.Impact, a, b *clause.Document) Result {
	ch := im.Change
	r := Result{Impact: im, Evidence: []cite.Citation{}, Actions: []Action{}, Reviewers: copyReviewers(im.Reviewers)}

	if ch.Type == diff.Renumbered {
		r.DecidedBy, r.Direction, r.Confidence = ByRule, "cosmetic", 1
		r.Summary = fmt.Sprintf("§%s renumbered to §%s; text unchanged.", ch.OldID, ch.NewID)
		return r
	}

	user, err := json.MarshalIndent(as.buildInput(im), "", "  ")
	if err != nil {
		return as.fail(r, "building model input: "+err.Error())
	}
	raw, err := as.Model.Complete(ctx, llm.Request{
		Name: "change_assessment", System: changeSystemPrompt, User: string(user), Schema: changeSchema,
	})
	if err != nil {
		return as.fail(r, "model call failed: "+err.Error())
	}
	r.Raw = raw
	var mc modelChange
	if err := strictUnmarshal(raw, &mc); err != nil {
		return as.fail(r, "model output does not match schema: "+err.Error())
	}

	r.DecidedBy = ByModel
	r.Material, r.ActionRequired, r.Direction, r.Summary, r.Rationale, r.Confidence =
		mc.Material, mc.ActionRequired, mc.Direction, mc.Summary, mc.Rationale, mc.Confidence

	var reasons []string
	if !slices.Contains(directions, mc.Direction) {
		reasons = append(reasons, fmt.Sprintf("unknown direction %q", mc.Direction))
	}
	if mc.Confidence < 0 || mc.Confidence > 1 {
		reasons = append(reasons, fmt.Sprintf("confidence %v outside [0,1]", mc.Confidence))
	}

	// Guardrail 1: quotes must exist verbatim in the clause they claim.
	for _, q := range mc.Quotes {
		c, err := locateQuote(q, ch, a, b)
		if err != nil {
			r.QuotesRejected++
			reasons = append(reasons, err.Error())
			continue
		}
		r.Evidence = append(r.Evidence, c)
	}
	if mc.Material && len(r.Evidence) == 0 {
		reasons = append(reasons, "material judgment has no verified quote")
	}

	// Guardrail 2: actions may only target items the mapper found.
	allowed := map[string]bool{NewItem: true}
	for _, h := range im.Hits {
		allowed[h.ID] = true
	}
	for _, act := range mc.Actions {
		if !allowed[act.ItemID] {
			reasons = append(reasons, fmt.Sprintf("action targets unknown item %q", act.ItemID))
			continue
		}
		r.Actions = append(r.Actions, act)
	}
	// The two judgments must agree with the action list.
	if mc.ActionRequired && len(r.Actions) == 0 {
		reasons = append(reasons, "says company action is required but proposes no valid action")
	}
	if !mc.ActionRequired && len(mc.Actions) > 0 {
		reasons = append(reasons, "proposes actions but says no company action is required")
	}

	// Guardrail 3: do not guess.
	if mc.Direction == "unclear" {
		reasons = append(reasons, "model could not tell whether the change is stricter or looser")
	}
	if mc.Ambiguous {
		reasons = append(reasons, "model flagged ambiguity: "+mc.AmbiguityReason)
	}
	if mc.Confidence < as.minConfidence() {
		reasons = append(reasons, fmt.Sprintf("confidence %.2f below threshold %.2f", mc.Confidence, as.minConfidence()))
	}

	if len(reasons) > 0 {
		r.Escalate, r.Reasons = true, reasons
		r.Reviewers = as.addEscalation(r.Reviewers, reasons[0])
	}
	return r
}

// fail records a failed assessment. Fail closed: treat as material and
// action-required, escalate.
func (as *Assessor) fail(r Result, reason string) Result {
	r.DecidedBy, r.Material, r.ActionRequired, r.Direction = ByError, true, true, "unclear"
	r.Summary = "Automatic assessment failed; needs expert review."
	r.Escalate, r.Reasons = true, []string{reason}
	r.Reviewers = as.addEscalation(r.Reviewers, reason)
	return r
}

func (as *Assessor) buildInput(im impact.Impact) changeIn {
	ch := im.Change
	c := as.Company.Company
	in := changeIn{
		Company:       companyIn{Name: c.Name, Customers: c.Customers, HFTDTiersServed: c.HFTDTiersServed},
		ChangeType:    string(ch.Type),
		Edits:         []editIn{},
		Gap:           im.Gap,
		MapperNote:    im.Note,
		AffectedItems: []itemIn{},
	}
	if ch.Old != nil {
		in.OldClause = &clauseIn{ID: ch.OldID, Text: ch.Old.Quote}
	}
	if ch.New != nil {
		in.NewClause = &clauseIn{ID: ch.NewID, Text: ch.New.Quote}
	}
	for _, e := range ch.Edits {
		var ed editIn
		if e.Old != nil {
			ed.Old = e.Old.Quote
		}
		if e.New != nil {
			ed.New = e.New.Quote
		}
		in.Edits = append(in.Edits, ed)
	}
	for _, h := range im.Hits {
		it := itemIn{ID: h.ID, Kind: h.Kind, Title: h.Title, Link: string(h.How) + " " + h.Via}
		switch h.Kind {
		case "obligation":
			if ob := as.Company.Obligation(h.ID); ob != nil {
				it.Requirement, it.Status = ob.Requirement, ob.Status
			}
		case "project":
			if p := as.Company.Project(h.ID); p != nil {
				it.Assumptions = p.Assumptions
			}
		}
		in.AffectedItems = append(in.AffectedItems, it)
	}
	return in
}

// ---- document status ----

// Status classifies a document as draft or final, with a verified quote.
func (as *Assessor) Status(ctx context.Context, d *clause.Document) StatusResult {
	sr := StatusResult{Doc: d.Name}
	fail := func(reason string) StatusResult {
		sr.DecidedBy, sr.Status, sr.Escalate = ByError, "unclear", true
		sr.Reasons = append(sr.Reasons, reason)
		return sr
	}
	raw, err := as.Model.Complete(ctx, llm.Request{
		Name: "document_status", System: statusSystemPrompt,
		User: "Document " + d.Name + ":\n\n" + d.Source, Schema: statusSchema,
	})
	if err != nil {
		return fail("model call failed: " + err.Error())
	}
	sr.Raw = raw
	var ms modelStatus
	if err := strictUnmarshal(raw, &ms); err != nil {
		return fail("model output does not match schema: " + err.Error())
	}
	sr.DecidedBy, sr.Status, sr.Confidence = ByModel, ms.Status, ms.Confidence

	if start, end, ok := locate(d.Source, 0, len(d.Source), ms.Quote); ok {
		c := cite.New(d.Name, sectionAt(d, start), d.Source, start, end)
		sr.Evidence = &c
	} else {
		sr.Reasons = append(sr.Reasons, fmt.Sprintf("status quote not found in %s: %q", d.Name, ms.Quote))
	}
	if ms.Status != "draft" && ms.Status != "final" {
		sr.Reasons = append(sr.Reasons, fmt.Sprintf("status is %q", ms.Status))
	}
	if ms.Confidence < as.minConfidence() {
		sr.Reasons = append(sr.Reasons, fmt.Sprintf("confidence %.2f below threshold %.2f", ms.Confidence, as.minConfidence()))
	}
	sr.Escalate = len(sr.Reasons) > 0
	return sr
}

// ---- helpers ----

// locateQuote finds q in the old or new clause and returns a verified citation.
func locateQuote(q Quote, ch diff.Change, a, b *clause.Document) (cite.Citation, error) {
	var doc *clause.Document
	var whole *cite.Citation
	switch q.Version {
	case "old":
		doc, whole = a, ch.Old
	case "new":
		doc, whole = b, ch.New
	default:
		return cite.Citation{}, fmt.Errorf("quote has unknown version %q", q.Version)
	}
	if whole == nil {
		return cite.Citation{}, fmt.Errorf("quote cites the %s clause, which does not exist for this change: %q", q.Version, q.Text)
	}
	start, end, ok := locate(doc.Source, whole.Start, whole.End, q.Text)
	if !ok {
		return cite.Citation{}, fmt.Errorf("quote not found in %s §%s: %q", doc.Name, whole.ClauseID, q.Text)
	}
	c := cite.New(doc.Name, whole.ClauseID, doc.Source, start, end)
	if err := cite.Verify(doc.Source, c); err != nil {
		return cite.Citation{}, err
	}
	return c, nil
}

// locate finds quote inside src[from:to]. It tries an exact match, then a
// whitespace-flexible match (models often collapse line breaks). Either way
// the returned span is the real source text, so the citation stays exact.
func locate(src string, from, to int, quote string) (start, end int, ok bool) {
	q := strings.TrimSpace(quote)
	if len(q) < 3 {
		return 0, 0, false
	}
	text := src[from:to]
	if i := strings.Index(text, q); i >= 0 {
		return from + i, from + i + len(q), true
	}
	fields := strings.Fields(q)
	for i, f := range fields {
		fields[i] = regexp.QuoteMeta(f)
	}
	re, err := regexp.Compile(strings.Join(fields, `\s+`))
	if err != nil {
		return 0, 0, false
	}
	if loc := re.FindStringIndex(text); loc != nil {
		return from + loc[0], from + loc[1], true
	}
	return 0, 0, false
}

// sectionAt names the part of the document containing byte offset off.
func sectionAt(d *clause.Document, off int) string {
	for _, c := range d.Clauses {
		if off >= c.Span.Start && off < c.Span.End {
			return c.ID
		}
	}
	switch {
	case off >= d.TitleSpan.Start && off < d.TitleSpan.End:
		return "title"
	case off >= d.Preamble.Start && off < d.Preamble.End:
		return "preamble"
	}
	return "document"
}

func (as *Assessor) addEscalation(revs []impact.Reviewer, reason string) []impact.Reviewer {
	id := as.Company.Routing.EscalationReviewer
	why := "escalated: " + reason
	for i := range revs {
		if revs[i].PersonID == id {
			revs[i].Reasons = append(revs[i].Reasons, why)
			return revs
		}
	}
	rv := impact.Reviewer{PersonID: id, Reasons: []string{why}}
	if p := as.Company.Person(id); p != nil {
		rv.Name, rv.Role = p.Name, p.Role
	}
	return append(revs, rv)
}

func copyReviewers(in []impact.Reviewer) []impact.Reviewer {
	out := make([]impact.Reviewer, len(in))
	for i, r := range in {
		r.Reasons = append([]string(nil), r.Reasons...)
		out[i] = r
	}
	return out
}

func strictUnmarshal(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
