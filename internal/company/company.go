// Package company loads the synthetic company context: who the company is,
// what it must do (obligations), what it is working on (projects), what it has
// written down (documents), and who is responsible (people).
//
// In the prototype this is a hand-authored JSON file. A real deployment would
// import it from GRC, document-management and project systems.
package company

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
)

type Profile struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	Customers       int    `json:"customers"`
	HFTDTiersServed []int  `json:"hftd_tiers_served"`
	Overhead        bool   `json:"overhead_facilities"`
}

type Topic struct {
	ID       string   `json:"id"`
	Keywords []string `json:"keywords"`
}

type Person struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Role   string   `json:"role"`
	Topics []string `json:"topics"`
}

type Routing struct {
	DefaultReviewer    string `json:"default_reviewer"`
	EscalationReviewer string `json:"escalation_reviewer"`
}

// Source links an obligation to the regulatory clause it comes from.
type Source struct {
	Docket string `json:"docket"`
	Clause string `json:"clause"`
	AsOf   string `json:"as_of"`
}

type Obligation struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Requirement string   `json:"requirement"`
	Sources     []Source `json:"sources"`
	Status      string   `json:"status"`
	Owner       string   `json:"owner"`
	Topics      []string `json:"topics"`
	ProjectIDs  []string `json:"project_ids"`
	DocumentIDs []string `json:"document_ids"`
}

type Project struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Owner         string   `json:"owner"`
	Due           string   `json:"due"`
	ObligationIDs []string `json:"obligation_ids"`
	Assumptions   []string `json:"assumptions"`
}

type Document struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Type          string   `json:"type"`
	Owner         string   `json:"owner"`
	ObligationIDs []string `json:"obligation_ids"`
}

// Context is the whole company model plus lookup indexes.
type Context struct {
	Company     Profile      `json:"company"`
	Topics      []Topic      `json:"topics"`
	People      []Person     `json:"people"`
	Routing     Routing      `json:"routing"`
	Obligations []Obligation `json:"obligations"`
	Projects    []Project    `json:"projects"`
	Documents   []Document   `json:"documents"`

	people      map[string]*Person
	topics      map[string]*Topic
	obligations map[string]*Obligation
	projects    map[string]*Project
	documents   map[string]*Document
	byClause    map[string][]*Obligation // docket|clause -> obligations
	obProjects  map[string][]*Project    // obligation id -> projects (either side may declare the link)
	obDocuments map[string][]*Document   // obligation id -> documents
}

// Load reads and validates a company context file.
func Load(path string) (*Context, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ctx, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ctx, nil
}

// Parse decodes and validates a company context. Broken references
// (unknown owner, project, document, topic) are errors, not silent gaps.
func Parse(data []byte) (*Context, error) {
	var ctx Context
	if err := json.Unmarshal(data, &ctx); err != nil {
		return nil, err
	}
	if err := ctx.index(); err != nil {
		return nil, err
	}
	return &ctx, nil
}

func (c *Context) index() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	c.people = indexBy(c.People, func(p *Person) string { return p.ID }, "person", bad)
	c.topics = indexBy(c.Topics, func(t *Topic) string { return t.ID }, "topic", bad)
	c.obligations = indexBy(c.Obligations, func(o *Obligation) string { return o.ID }, "obligation", bad)
	c.projects = indexBy(c.Projects, func(p *Project) string { return p.ID }, "project", bad)
	c.documents = indexBy(c.Documents, func(d *Document) string { return d.ID }, "document", bad)

	person := func(id, ref string) {
		if _, ok := c.people[id]; !ok {
			bad("%s: unknown owner %q", ref, id)
		}
	}
	person(c.Routing.DefaultReviewer, "routing.default_reviewer")
	person(c.Routing.EscalationReviewer, "routing.escalation_reviewer")

	c.byClause = map[string][]*Obligation{}
	c.obProjects = map[string][]*Project{}
	c.obDocuments = map[string][]*Document{}

	for i := range c.Obligations {
		ob := &c.Obligations[i]
		person(ob.Owner, ob.ID)
		if len(ob.Sources) == 0 {
			bad("%s: no source clause", ob.ID)
		}
		for _, s := range ob.Sources {
			k := clauseKey(s.Docket, s.Clause)
			c.byClause[k] = append(c.byClause[k], ob)
		}
		for _, t := range ob.Topics {
			if _, ok := c.topics[t]; !ok {
				bad("%s: unknown topic %q", ob.ID, t)
			}
		}
		for _, id := range ob.ProjectIDs {
			if p, ok := c.projects[id]; ok {
				c.obProjects[ob.ID] = appendOnce(c.obProjects[ob.ID], p)
			} else {
				bad("%s: unknown project %q", ob.ID, id)
			}
		}
		for _, id := range ob.DocumentIDs {
			if d, ok := c.documents[id]; ok {
				c.obDocuments[ob.ID] = appendOnce(c.obDocuments[ob.ID], d)
			} else {
				bad("%s: unknown document %q", ob.ID, id)
			}
		}
	}
	for i := range c.Projects {
		p := &c.Projects[i]
		person(p.Owner, p.ID)
		for _, id := range p.ObligationIDs {
			if _, ok := c.obligations[id]; ok {
				c.obProjects[id] = appendOnce(c.obProjects[id], p)
			} else {
				bad("%s: unknown obligation %q", p.ID, id)
			}
		}
	}
	for i := range c.Documents {
		d := &c.Documents[i]
		person(d.Owner, d.ID)
		for _, id := range d.ObligationIDs {
			if _, ok := c.obligations[id]; ok {
				c.obDocuments[id] = appendOnce(c.obDocuments[id], d)
			} else {
				bad("%s: unknown obligation %q", d.ID, id)
			}
		}
	}
	for _, ps := range c.obProjects {
		sort.Slice(ps, func(i, j int) bool { return ps[i].ID < ps[j].ID })
	}
	for _, ds := range c.obDocuments {
		sort.Slice(ds, func(i, j int) bool { return ds[i].ID < ds[j].ID })
	}
	return errors.Join(errs...)
}

// Person returns the person with id, or nil.
func (c *Context) Person(id string) *Person { return c.people[id] }

// Obligation returns the obligation with id, or nil.
func (c *Context) Obligation(id string) *Obligation { return c.obligations[id] }

// Project returns the project with id, or nil.
func (c *Context) Project(id string) *Project { return c.projects[id] }

// Document returns the document with id, or nil.
func (c *Context) Document(id string) *Document { return c.documents[id] }

// ObligationsForClause returns obligations whose source is docket §clause.
func (c *Context) ObligationsForClause(docket, clause string) []*Obligation {
	return c.byClause[clauseKey(docket, clause)]
}

// ProjectsFor returns projects linked to an obligation.
func (c *Context) ProjectsFor(obligationID string) []*Project { return c.obProjects[obligationID] }

// DocumentsFor returns documents linked to an obligation.
func (c *Context) DocumentsFor(obligationID string) []*Document { return c.obDocuments[obligationID] }

// ObligationsWithTopic returns obligations tagged with topic.
func (c *Context) ObligationsWithTopic(topic string) []*Obligation {
	var out []*Obligation
	for i := range c.Obligations {
		if contains(c.Obligations[i].Topics, topic) {
			out = append(out, &c.Obligations[i])
		}
	}
	return out
}

// PeopleWithTopic returns people who cover topic.
func (c *Context) PeopleWithTopic(topic string) []*Person {
	var out []*Person
	for i := range c.People {
		if contains(c.People[i].Topics, topic) {
			out = append(out, &c.People[i])
		}
	}
	return out
}

func clauseKey(docket, clause string) string { return docket + "|" + clause }

func indexBy[T any](items []T, id func(*T) string, kind string, bad func(string, ...any)) map[string]*T {
	m := make(map[string]*T, len(items))
	for i := range items {
		it := &items[i]
		k := id(it)
		if _, dup := m[k]; dup {
			bad("duplicate %s %s", kind, k)
		}
		m[k] = it
	}
	return m
}

func appendOnce[T comparable](list []T, v T) []T {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
