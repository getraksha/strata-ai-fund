// Package impact maps clause-level changes onto the company context.
//
// It is deterministic: no model calls. For each change it answers
// "which of our obligations, projects and documents does this touch, how do
// we know, and who should look at it?"
//
//  1. Direct: an obligation's source is the changed clause (old clause ID).
//     Its linked projects and documents are hit "via" that obligation.
//  2. Topic: only when nothing is linked directly. Topic keywords found in
//     the clause text (each with a citation) point to obligations that might
//     be related, their documents, and the people who cover that topic.
//  3. Gap: an added clause with no linked obligation, i.e. possibly a new
//     requirement the company does not track yet.
//
// It does NOT judge materiality or meaning; that is the next layer.
package impact

import (
	"errors"
	"fmt"
	"regexp"

	"strata/internal/cite"
	"strata/internal/clause"
	"strata/internal/company"
	"strata/internal/diff"
)

// How records why an item is considered affected.
type How string

const (
	Direct        How = "direct"          // obligation is sourced from the changed clause
	ViaObligation How = "via_obligation"  // project/document linked to a directly hit obligation
	Topic         How = "topic"           // topic keyword match only (weaker)
	Profile       How = "company_profile" // clause decides applicability; check company facts
)

// ApplicabilityTopic marks clauses that decide whether the rule applies at all.
const ApplicabilityTopic = "applicability"

// Hit is one affected company item.
type Hit struct {
	Kind  string `json:"kind"` // obligation | project | document | company
	ID    string `json:"id"`
	Title string `json:"title"`
	Owner string `json:"owner,omitempty"`
	How   How    `json:"how"`
	Via   string `json:"via"` // clause, obligation ID or topic that led here
}

// TopicMatch is a topic detected in the clause, with the exact words as evidence.
type TopicMatch struct {
	Topic    string        `json:"topic"`
	Evidence cite.Citation `json:"evidence"`
}

// Reviewer is a person the change should be routed to, with reasons.
type Reviewer struct {
	PersonID string   `json:"person_id"`
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Reasons  []string `json:"reasons"`
}

// Impact is one change plus everything it touches.
type Impact struct {
	Change    diff.Change  `json:"change"`
	Topics    []TopicMatch `json:"topics,omitempty"`
	Hits      []Hit        `json:"hits"`
	Gap       bool         `json:"gap"`
	Note      string       `json:"note,omitempty"`
	Reviewers []Reviewer   `json:"reviewers"`
}

// Report is the impact of one version transition on one company.
type Report struct {
	OldDoc  string   `json:"old_doc"`
	NewDoc  string   `json:"new_doc"`
	Docket  string   `json:"docket"`
	Company string   `json:"company"`
	Impacts []Impact `json:"impacts"`
}

type topicMatcher struct {
	id  string
	res []*regexp.Regexp
}

type mapper struct {
	ctx     *company.Context
	a, b    *clause.Document
	docket  string
	topics  []topicMatcher
	oldByID map[string]clause.Clause
	newByID map[string]clause.Clause
}

// Map computes the company impact of every change in res.
// Both documents must declare the same docket in their front matter.
func Map(res diff.Result, a, b *clause.Document, ctx *company.Context) (Report, error) {
	docket := a.Meta["docket"]
	if docket == "" {
		return Report{}, fmt.Errorf("%s: front matter has no docket", a.Name)
	}
	if b.Meta["docket"] != docket {
		return Report{}, fmt.Errorf("docket mismatch: %s is %q, %s is %q", a.Name, docket, b.Name, b.Meta["docket"])
	}
	m := &mapper{ctx: ctx, a: a, b: b, docket: docket, oldByID: byID(a), newByID: byID(b)}
	for _, t := range ctx.Topics {
		tm := topicMatcher{id: t.ID}
		for _, kw := range t.Keywords {
			// Leading word boundary, open end: "penalt" matches "penalties", "file" does not match "profile".
			tm.res = append(tm.res, regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(kw)))
		}
		m.topics = append(m.topics, tm)
	}

	rep := Report{OldDoc: a.Name, NewDoc: b.Name, Docket: docket, Company: ctx.Company.Name}
	for _, ch := range res.Changes {
		rep.Impacts = append(rep.Impacts, m.impact(ch))
	}
	return rep, nil
}

func (m *mapper) impact(ch diff.Change) Impact {
	im := Impact{Change: ch, Hits: []Hit{}}
	rv := &reviewerSet{ctx: m.ctx}
	seen := map[string]bool{}
	add := func(h Hit) bool {
		k := h.Kind + "|" + h.ID
		if seen[k] {
			return false
		}
		seen[k] = true
		im.Hits = append(im.Hits, h)
		return true
	}

	// 1. Direct links from the old clause ID.
	direct := 0
	if ch.OldID != "" {
		for _, ob := range m.ctx.ObligationsForClause(m.docket, ch.OldID) {
			direct++
			add(Hit{Kind: "obligation", ID: ob.ID, Title: ob.Title, Owner: ob.Owner, How: Direct, Via: "§" + ch.OldID})
			rv.add(ob.Owner, fmt.Sprintf("owns %s, linked to §%s", ob.ID, ch.OldID))
			for _, p := range m.ctx.ProjectsFor(ob.ID) {
				if add(Hit{Kind: "project", ID: p.ID, Title: p.Name, Owner: p.Owner, How: ViaObligation, Via: ob.ID}) {
					rv.add(p.Owner, fmt.Sprintf("owns %s, via %s", p.ID, ob.ID))
				}
			}
			for _, d := range m.ctx.DocumentsFor(ob.ID) {
				if add(Hit{Kind: "document", ID: d.ID, Title: d.Title, Owner: d.Owner, How: ViaObligation, Via: ob.ID}) {
					rv.add(d.Owner, fmt.Sprintf("owns %s, via %s", d.ID, ob.ID))
				}
			}
		}
	}

	if ch.Type == diff.Renumbered {
		if direct == 0 {
			im.Note = "renumbered only, text unchanged; no company impact"
		} else {
			im.Note = "renumbered only, text unchanged; obligation links must move to §" + ch.NewID
		}
		if rv.empty() {
			rv.add(m.ctx.Routing.DefaultReviewer, "no linked owner; default reviewer")
		}
		im.Reviewers = rv.list()
		return im
	}

	// 2. Topics: always recorded (with citations), but only used to find
	//    affected items when nothing is linked directly.
	im.Topics = m.detectTopics(ch)
	if direct == 0 {
		if ch.Type == diff.Added {
			im.Gap = true
			im.Note = "new clause; no existing obligation covers it"
		} else {
			im.Note = "no obligation is linked to this clause"
		}
		for _, tm := range im.Topics {
			if tm.Topic == ApplicabilityTopic {
				c := m.ctx.Company
				add(Hit{Kind: "company", ID: c.ID, How: Profile, Via: tm.Topic,
					Title: fmt.Sprintf("%s: %d customers, HFTD tiers %v", c.Name, c.Customers, c.HFTDTiersServed)})
			}
			for _, ob := range m.ctx.ObligationsWithTopic(tm.Topic) {
				add(Hit{Kind: "obligation", ID: ob.ID, Title: ob.Title, Owner: ob.Owner, How: Topic, Via: tm.Topic})
				// Follow topic-matched obligations to their documents (procedures,
				// playbooks) so a new requirement can be placed in the right one.
				// Still marked "topic": a possible hit, not a confirmed one.
				for _, d := range m.ctx.DocumentsFor(ob.ID) {
					add(Hit{Kind: "document", ID: d.ID, Title: d.Title, Owner: d.Owner, How: Topic, Via: ob.ID + " (topic " + tm.Topic + ")"})
				}
			}
			for _, p := range m.ctx.PeopleWithTopic(tm.Topic) {
				rv.add(p.ID, "covers topic "+tm.Topic)
			}
		}
	}

	if rv.empty() {
		rv.add(m.ctx.Routing.DefaultReviewer, "no linked owner; default reviewer")
	}
	im.Reviewers = rv.list()
	return im
}

// detectTopics scans the clause (new version if it exists, else old) for
// topic keywords and cites the first match per topic.
func (m *mapper) detectTopics(ch diff.Change) []TopicMatch {
	doc, cl := m.b, m.newByID[ch.NewID]
	if ch.NewID == "" {
		doc, cl = m.a, m.oldByID[ch.OldID]
	}
	text := doc.Source[cl.Span.Start:cl.Span.End]
	var out []TopicMatch
	for _, t := range m.topics {
		for _, re := range t.res {
			if loc := re.FindStringIndex(text); loc != nil {
				out = append(out, TopicMatch{
					Topic:    t.id,
					Evidence: cite.New(doc.Name, cl.ID, doc.Source, cl.Span.Start+loc[0], cl.Span.Start+loc[1]),
				})
				break
			}
		}
	}
	return out
}

// VerifyAll re-checks every citation in the report against the source documents.
func VerifyAll(rep Report, a, b *clause.Document) error {
	docs := map[string]*clause.Document{a.Name: a, b.Name: b}
	var errs []error
	var changes []diff.Change
	for _, im := range rep.Impacts {
		changes = append(changes, im.Change)
		for _, t := range im.Topics {
			d, ok := docs[t.Evidence.Doc]
			if !ok {
				errs = append(errs, fmt.Errorf("topic evidence cites unknown document %s", t.Evidence.Doc))
				continue
			}
			if err := cite.Verify(d.Source, t.Evidence); err != nil {
				errs = append(errs, err)
			}
		}
	}
	errs = append(errs, diff.VerifyAll(diff.Result{Changes: changes}, a, b))
	return errors.Join(errs...)
}

type reviewerSet struct {
	ctx   *company.Context
	order []string
	byID  map[string]*Reviewer
}

func (r *reviewerSet) add(personID, reason string) {
	if r.byID == nil {
		r.byID = map[string]*Reviewer{}
	}
	rv, ok := r.byID[personID]
	if !ok {
		rv = &Reviewer{PersonID: personID}
		if p := r.ctx.Person(personID); p != nil {
			rv.Name, rv.Role = p.Name, p.Role
		}
		r.byID[personID] = rv
		r.order = append(r.order, personID)
	}
	for _, x := range rv.Reasons {
		if x == reason {
			return
		}
	}
	rv.Reasons = append(rv.Reasons, reason)
}

func (r *reviewerSet) empty() bool { return len(r.order) == 0 }

func (r *reviewerSet) list() []Reviewer {
	out := make([]Reviewer, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, *r.byID[id])
	}
	return out
}

func byID(d *clause.Document) map[string]clause.Clause {
	m := make(map[string]clause.Clause, len(d.Clauses))
	for _, c := range d.Clauses {
		m[c.ID] = c
	}
	return m
}
