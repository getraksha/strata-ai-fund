package state

import (
	"encoding/json"
	"fmt"
	"time"

	"strata/internal/assess"
	"strata/internal/cite"
)

// Event types.
const (
	TypeContextLoaded   = "context_loaded"
	TypeVersionIngested = "version_ingested"
	TypeChangeAssessed  = "change_assessed"
	TypeReviewDecided   = "review_decided"
	TypeRolledBack      = "rolled_back"
)

// Card statuses and review decisions.
const (
	Pending     = "pending"
	NeedsExpert = "needs_expert"
	Approved    = "approved"
	Rejected    = "rejected"

	Approve = "approve"
	Reject  = "reject"
)

// ---- event payloads ----

type ContextLoaded struct {
	CompanyPath   string `json:"company_path"`
	CompanySHA256 string `json:"company_sha256"`
	CompanyName   string `json:"company_name"`
}

// ObligationEffect is a status change for an obligation caused by a final decision.
type ObligationEffect struct {
	ObligationID  string         `json:"obligation_id"`
	Status        string         `json:"status"` // "effective"
	EffectiveDate string         `json:"effective_date,omitempty"`
	Due           string         `json:"due,omitempty"`
	DueEvidence   *cite.Citation `json:"due_evidence,omitempty"`
	Basis         string         `json:"basis"`
}

type VersionIngested struct {
	Doc      string              `json:"doc"`
	Path     string              `json:"path"`
	SHA256   string              `json:"sha256"`
	Docket   string              `json:"docket"`
	Issued   string              `json:"issued,omitempty"`
	Previous string              `json:"previous,omitempty"` // version it was diffed against
	Status   assess.StatusResult `json:"status"`
	Effects  []ObligationEffect  `json:"obligation_effects,omitempty"`
}

type ChangeAssessed struct {
	CardID     string        `json:"card_id"`
	Transition string        `json:"transition"`
	Result     assess.Result `json:"result"`
}

type ReviewDecided struct {
	CardID   string `json:"card_id"`
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
}

type RolledBack struct {
	ToSeq int    `json:"to_seq"`
	Note  string `json:"note"`
}

// ---- derived state ----

// Card is one change awaiting or past review.
type Card struct {
	ID         string        `json:"id"`
	Transition string        `json:"transition"`
	CreatedSeq int           `json:"created_seq"`
	Result     assess.Result `json:"result"`
	Status     string        `json:"status"`
	Decisions  []Decision    `json:"decisions,omitempty"`
}

type Decision struct {
	Seq      int       `json:"seq"`
	Time     time.Time `json:"time"`
	Reviewer string    `json:"reviewer"`
	Decision string    `json:"decision"`
	Note     string    `json:"note,omitempty"`
}

// Task is an action from an approved card, assigned to the affected item's owner.
type Task struct {
	CardID string `json:"card_id"`
	ItemID string `json:"item_id"`
	Action string `json:"action"`
	Owner  string `json:"owner"`
	Seq    int    `json:"seq"`
}

// State is the project as of the last event, after applying rollbacks.
type State struct {
	AsOfSeq     int                         `json:"as_of_seq"`
	Company     ContextLoaded               `json:"company"`
	Versions    []VersionIngested           `json:"versions"`
	Cards       []*Card                     `json:"cards"`
	Obligations map[string]ObligationEffect `json:"obligation_changes"` // only obligations changed by events
	Tasks       []Task                      `json:"tasks"`

	byCard map[string]*Card
}

// Card returns the card with id, or nil.
func (s *State) Card(id string) *Card { return s.byCard[id] }

// LastVersion returns the most recently ingested version, or nil.
func (s *State) LastVersion() *VersionIngested {
	if len(s.Versions) == 0 {
		return nil
	}
	return &s.Versions[len(s.Versions)-1]
}

// Effective returns the events that currently count, after rollbacks, and
// for each undone event the seq of the rollback that undid it.
// "Roll back to N" means: keep the events that counted at N, drop later ones.
func Effective(events []Event) ([]Event, map[int]int, error) {
	var effective []Event
	undoneBy := map[int]int{}
	for _, e := range events {
		if e.Type != TypeRolledBack {
			effective = append(effective, e)
			continue
		}
		var rb RolledBack
		if err := json.Unmarshal(e.Data, &rb); err != nil {
			return nil, nil, fmt.Errorf("event %d: %w", e.Seq, err)
		}
		var kept []Event
		for _, x := range effective {
			if x.Seq <= rb.ToSeq {
				kept = append(kept, x)
			} else {
				undoneBy[x.Seq] = e.Seq
			}
		}
		effective = kept
	}
	return effective, undoneBy, nil
}

// Replay rebuilds state from the full event history.
func Replay(events []Event) (*State, error) {
	eff, _, err := Effective(events)
	if err != nil {
		return nil, err
	}
	s := &State{Obligations: map[string]ObligationEffect{}, byCard: map[string]*Card{}}
	if n := len(events); n > 0 {
		s.AsOfSeq = events[n-1].Seq
	}
	for _, e := range eff {
		if err := s.apply(e); err != nil {
			return nil, fmt.Errorf("event %d (%s): %w", e.Seq, e.Type, err)
		}
	}
	return s, nil
}

func (s *State) apply(e Event) error {
	switch e.Type {
	case TypeContextLoaded:
		return json.Unmarshal(e.Data, &s.Company)

	case TypeVersionIngested:
		var v VersionIngested
		if err := json.Unmarshal(e.Data, &v); err != nil {
			return err
		}
		s.Versions = append(s.Versions, v)
		for _, eff := range v.Effects {
			s.Obligations[eff.ObligationID] = eff
		}

	case TypeChangeAssessed:
		var c ChangeAssessed
		if err := json.Unmarshal(e.Data, &c); err != nil {
			return err
		}
		if s.byCard[c.CardID] != nil {
			return fmt.Errorf("duplicate card %s", c.CardID)
		}
		card := &Card{ID: c.CardID, Transition: c.Transition, CreatedSeq: e.Seq, Result: c.Result, Status: Pending}
		if c.Result.Escalate {
			card.Status = NeedsExpert
		}
		s.Cards = append(s.Cards, card)
		s.byCard[card.ID] = card

	case TypeReviewDecided:
		var r ReviewDecided
		if err := json.Unmarshal(e.Data, &r); err != nil {
			return err
		}
		card := s.byCard[r.CardID]
		if card == nil {
			return fmt.Errorf("review of unknown card %s", r.CardID)
		}
		card.Decisions = append(card.Decisions, Decision{Seq: e.Seq, Time: e.Time, Reviewer: e.Actor, Decision: r.Decision, Note: r.Note})
		switch r.Decision {
		case Approve:
			card.Status = Approved
			for _, a := range card.Result.Actions {
				s.Tasks = append(s.Tasks, Task{CardID: card.ID, ItemID: a.ItemID, Action: a.Action, Owner: ownerFor(card, a.ItemID), Seq: e.Seq})
			}
		case Reject:
			card.Status = Rejected
		default:
			return fmt.Errorf("unknown decision %q", r.Decision)
		}

	default:
		return fmt.Errorf("unknown event type %q", e.Type)
	}
	return nil
}

// ownerFor is the affected item's owner; for NEW items, the first routed reviewer.
func ownerFor(card *Card, itemID string) string {
	for _, h := range card.Result.Impact.Hits {
		if h.ID == itemID && h.Owner != "" {
			return h.Owner
		}
	}
	if len(card.Result.Reviewers) > 0 {
		return card.Result.Reviewers[0].PersonID
	}
	return ""
}
