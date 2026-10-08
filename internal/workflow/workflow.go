// Package workflow is the change-to-action loop on top of the audit log:
//
//	Init     record which company context the project uses (with its hash)
//	Ingest   add the next version: diff + map + assess, one card per change
//	Review   approve or reject a card, under routing rules
//	Rollback undo everything after a given event (itself an event)
//
// Every operation validates against the current state, then appends exactly
// the events that describe what happened. State is never written directly.
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"strata/internal/assess"
	"strata/internal/cite"
	"strata/internal/clause"
	"strata/internal/company"
	"strata/internal/diff"
	"strata/internal/impact"
	"strata/internal/llm"
	"strata/internal/state"
)

// LogFile is the audit log inside the project directory.
const LogFile = "events.jsonl"

// System is the actor for automated events.
const System = "system"

// Project is an open project: its log and the company context it was created with.
type Project struct {
	Dir      string
	Log      *state.Log
	Company  *company.Context
	Parallel int // concurrent model calls during Ingest (0 = sequential)
}

// Init creates a new project bound to a company context file.
func Init(dir, companyPath string) (*Project, error) {
	abs, err := filepath.Abs(companyPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	cc, err := company.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", companyPath, err)
	}
	log, err := state.Create(filepath.Join(dir, LogFile))
	if err != nil {
		return nil, err
	}
	if _, err := log.Append(System, state.TypeContextLoaded, state.ContextLoaded{
		CompanyPath: abs, CompanySHA256: sha(data), CompanyName: cc.Company.Name,
	}); err != nil {
		return nil, err
	}
	return &Project{Dir: dir, Log: log, Company: cc}, nil
}

// Open loads a project, verifies the audit log, and checks that the company
// context file is byte-for-byte the one the project was created with.
func Open(dir string) (*Project, error) {
	log, err := state.Open(filepath.Join(dir, LogFile))
	if err != nil {
		return nil, err
	}
	evs := log.Events()
	if len(evs) == 0 || evs[0].Type != state.TypeContextLoaded {
		return nil, errors.New("audit log does not start with context_loaded")
	}
	var cl state.ContextLoaded
	if err := json.Unmarshal(evs[0].Data, &cl); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(cl.CompanyPath)
	if err != nil {
		return nil, err
	}
	if sha(data) != cl.CompanySHA256 {
		return nil, fmt.Errorf("company context %s changed since the project was created; restore it or start a new project", cl.CompanyPath)
	}
	cc, err := company.Parse(data)
	if err != nil {
		return nil, err
	}
	return &Project{Dir: dir, Log: log, Company: cc}, nil
}

// State replays the log.
func (p *Project) State() (*state.State, error) { return state.Replay(p.Log.Events()) }

// Ingest adds the next version of the proceeding. The first version only
// gets a draft/final check; later versions are diffed against the previous
// one, mapped and assessed, producing one card per change.
func (p *Project) Ingest(ctx context.Context, path string, m llm.Model) ([]state.Event, error) {
	st, err := p.State()
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	doc, digest, err := loadDoc(abs, "")
	if err != nil {
		return nil, err
	}
	for _, v := range st.Versions {
		if v.Doc == doc.Name || v.SHA256 == digest {
			return nil, fmt.Errorf("%s is already ingested", doc.Name)
		}
	}
	if doc.Meta["docket"] == "" {
		return nil, fmt.Errorf("%s: front matter has no docket", doc.Name)
	}

	as := &assess.Assessor{Model: m, Company: p.Company, Parallel: p.Parallel}
	v := state.VersionIngested{Doc: doc.Name, Path: abs, SHA256: digest, Docket: doc.Meta["docket"], Issued: doc.Meta["issued"]}
	var cards []state.ChangeAssessed

	if prev := st.LastVersion(); prev == nil {
		v.Status = as.Status(ctx, doc)
	} else {
		prevDoc, _, err := loadDoc(prev.Path, prev.SHA256)
		if err != nil {
			return nil, fmt.Errorf("previous version: %w", err)
		}
		res := diff.Compare(prevDoc, doc)
		if err := diff.VerifyAll(res, prevDoc, doc); err != nil {
			return nil, err
		}
		rep, err := impact.Map(res, prevDoc, doc, p.Company)
		if err != nil {
			return nil, err
		}
		ar := as.Assess(ctx, rep, prevDoc, doc)
		v.Status, v.Previous = ar.Status, prevDoc.Name
		next := nextCardNumber(p.Log.Events())
		for i, r := range ar.Results {
			cards = append(cards, state.ChangeAssessed{
				CardID:     fmt.Sprintf("C%d", next+i),
				Transition: prevDoc.Name + " -> " + doc.Name,
				Result:     r,
			})
		}
	}
	if v.Status.Status == "final" && !v.Status.Escalate {
		v.Effects = obligationEffects(doc, p.Company, v)
	}

	var out []state.Event
	e, err := p.Log.Append(System, state.TypeVersionIngested, v)
	if err != nil {
		return nil, err
	}
	out = append(out, e)
	for _, c := range cards {
		e, err := p.Log.Append(System, state.TypeChangeAssessed, c)
		if err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Review records a decision on a card. Rules:
//   - only a known person can review
//   - a decided card cannot be decided again (roll back instead)
//   - an escalated card can only be decided by the escalation reviewer
//   - otherwise the reviewer must be one the card was routed to (or the escalation reviewer)
//   - a rejection needs a note
func (p *Project) Review(cardID, reviewer, decision, note string) (state.Event, error) {
	st, err := p.State()
	if err != nil {
		return state.Event{}, err
	}
	card := st.Card(cardID)
	if card == nil {
		return state.Event{}, fmt.Errorf("no card %s", cardID)
	}
	if p.Company.Person(reviewer) == nil {
		return state.Event{}, fmt.Errorf("unknown reviewer %q", reviewer)
	}
	if decision != state.Approve && decision != state.Reject {
		return state.Event{}, fmt.Errorf("decision must be %q or %q", state.Approve, state.Reject)
	}
	if decision == state.Reject && strings.TrimSpace(note) == "" {
		return state.Event{}, errors.New("a rejection needs a note explaining why")
	}
	expert := p.Company.Routing.EscalationReviewer
	switch card.Status {
	case state.Approved, state.Rejected:
		return state.Event{}, fmt.Errorf("card %s is already %s; roll back to change the decision", cardID, card.Status)
	case state.NeedsExpert:
		if reviewer != expert {
			return state.Event{}, fmt.Errorf("card %s needs expert review; only %s can decide it", cardID, p.personLabel(expert))
		}
	default:
		routed := routedTo(card)
		if reviewer != expert && !slices.Contains(routed, reviewer) {
			return state.Event{}, fmt.Errorf("card %s is routed to %s, not %s", cardID, strings.Join(routed, ", "), reviewer)
		}
	}
	return p.Log.Append(reviewer, state.TypeReviewDecided, state.ReviewDecided{CardID: cardID, Decision: decision, Note: note})
}

// Rollback undoes every event after toSeq. The undo is itself an event;
// nothing is deleted from the log.
func (p *Project) Rollback(toSeq int, actor, note string) (state.Event, error) {
	if p.Company.Person(actor) == nil {
		return state.Event{}, fmt.Errorf("unknown person %q", actor)
	}
	if strings.TrimSpace(note) == "" {
		return state.Event{}, errors.New("a rollback needs a note explaining why")
	}
	n := len(p.Log.Events())
	if toSeq < 1 || toSeq >= n {
		return state.Event{}, fmt.Errorf("can only roll back to an event between 1 and %d", n-1)
	}
	return p.Log.Append(actor, state.TypeRolledBack, state.RolledBack{ToSeq: toSeq, Note: note})
}

func (p *Project) personLabel(id string) string {
	if per := p.Company.Person(id); per != nil {
		return per.Name + " (" + id + ")"
	}
	return id
}

func routedTo(card *state.Card) []string {
	var ids []string
	for _, r := range card.Result.Reviewers {
		ids = append(ids, r.PersonID)
	}
	return ids
}

// reDeadline finds deadlines expressed relative to the effective date.
var reDeadline = regexp.MustCompile(`(?i)within (\d+) days of the effective date`)

// obligationEffects makes every obligation sourced from this docket effective,
// and computes a due date where its source clause states one.
//
// Assumption (prototype): the effective date is the decision's issued date,
// which holds for "This decision is effective today." Clause links are
// matched by the clause id recorded on the obligation.
func obligationEffects(doc *clause.Document, cc *company.Context, v state.VersionIngested) []state.ObligationEffect {
	clauses := map[string]clause.Clause{}
	for _, c := range doc.Clauses {
		clauses[c.ID] = c
	}
	effective, dateErr := time.Parse("2006-01-02", v.Issued)
	basis := "final decision " + doc.Name
	if v.Status.Evidence != nil {
		basis += ": " + strings.Join(strings.Fields(v.Status.Evidence.Quote), " ")
	}

	var out []state.ObligationEffect
	for _, ob := range cc.Obligations {
		for _, src := range ob.Sources {
			if src.Docket != v.Docket {
				continue
			}
			e := state.ObligationEffect{ObligationID: ob.ID, Status: "effective", Basis: basis}
			if dateErr == nil {
				e.EffectiveDate = v.Issued
			}
			if c, ok := clauses[src.Clause]; ok && dateErr == nil {
				text := doc.Source[c.Span.Start:c.Span.End]
				if m := reDeadline.FindStringSubmatchIndex(text); m != nil {
					days, _ := strconv.Atoi(text[m[2]:m[3]])
					e.Due = effective.AddDate(0, 0, days).Format("2006-01-02")
					ev := cite.New(doc.Name, c.ID, doc.Source, c.Span.Start+m[0], c.Span.Start+m[1])
					e.DueEvidence = &ev
				}
			}
			out = append(out, e)
			break // one effect per obligation
		}
	}
	return out
}

// nextCardNumber never reuses an id, even for cards undone by a rollback,
// so a card id means the same thing everywhere in the audit log.
func nextCardNumber(events []state.Event) int {
	highest := 0
	for _, e := range events {
		if e.Type != state.TypeChangeAssessed {
			continue
		}
		var c state.ChangeAssessed
		if json.Unmarshal(e.Data, &c) == nil {
			if n, err := strconv.Atoi(strings.TrimPrefix(c.CardID, "C")); err == nil && n > highest {
				highest = n
			}
		}
	}
	return highest + 1
}

// loadDoc reads and parses a version file. If wantSHA is set, the file must
// be unchanged since it was ingested.
func loadDoc(path, wantSHA string) (*clause.Document, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	digest := sha(data)
	if wantSHA != "" && digest != wantSHA {
		return nil, "", fmt.Errorf("%s changed since it was ingested", path)
	}
	doc, err := clause.Parse(filepath.Base(path), string(data))
	if err != nil {
		return nil, "", err
	}
	return doc, digest, nil
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
