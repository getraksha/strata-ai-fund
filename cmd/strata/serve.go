package main

// strata serve: a small local web UI over the same project (audit log) the
// CLI uses. Every request re-opens the project, so the hash chain is verified
// on each call and CLI and UI can be used side by side.
//
// Prototype scope: binds to localhost, no authentication; the "acting as"
// picker stands in for login. Mutations are serialized with a mutex.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"strata/internal/assess"
	"strata/internal/cite"
	"strata/internal/company"
	"strata/internal/llm"
	"strata/internal/state"
	"strata/internal/workflow"
)

//go:embed web/index.html
var indexHTML []byte

type server struct {
	project  string    // project directory
	docs     string    // folder of version files offered for ingest
	model    llm.Model // nil when no model is configured
	modelErr string
	mu       sync.Mutex
}

func runServe(args []string, stdout, stderr io.Writer) int {
	fs, dir := projectFlags("serve", stderr)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	companyPath := fs.String("company", "testdata/company.json", "company context, used only to create a new project")
	docs := fs.String("docs", "testdata", "folder of version files offered for ingest")
	model := fs.String("model", "", "OpenAI model name (default: $OPENAI_MODEL)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if _, err := os.Stat(filepath.Join(*dir, workflow.LogFile)); errors.Is(err, os.ErrNotExist) {
		p, err := workflow.Init(*dir, *companyPath)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "created project %s for %s\n", *dir, p.Company.Company.Name)
	} else if _, err := workflow.Open(*dir); err != nil {
		return fail(stderr, err)
	}

	s := &server{project: *dir, docs: *docs}
	if m, err := llm.NewOpenAIFromEnv(*model); err != nil {
		s.modelErr = err.Error()
		fmt.Fprintf(stderr, "warning: ingest disabled: %v\n", err)
	} else {
		s.model = m
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/ingest", s.handleIngest)
	mux.HandleFunc("/api/review", s.handleReview)
	mux.HandleFunc("/api/rollback", s.handleRollback)

	fmt.Fprintf(stdout, "Strata UI: http://%s  (project %s, docs %s)\n", *addr, *dir, *docs)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// ---- handlers ----

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		replyErr(w, http.StatusMethodNotAllowed, errors.New("GET only"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply(w)
}

func (s *server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	if s.model == nil {
		replyErr(w, http.StatusBadRequest, errors.New("ingest needs a model: "+s.modelErr))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := workflow.Open(s.project)
	if err != nil {
		replyErr(w, http.StatusInternalServerError, err)
		return
	}
	st, err := p.State()
	if err != nil {
		replyErr(w, http.StatusInternalServerError, err)
		return
	}
	// Only files from the docs folder, by name: no arbitrary paths from the browser.
	if !slices.Contains(s.available(st), req.File) {
		replyErr(w, http.StatusBadRequest, fmt.Errorf("%q is not an available version", req.File))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	p.Parallel = assess.DefaultParallel
	if _, err := p.Ingest(ctx, filepath.Join(s.docs, req.File), s.model); err != nil {
		replyErr(w, http.StatusBadRequest, err)
		return
	}
	s.reply(w)
}

func (s *server) handleReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Card     string `json:"card"`
		Reviewer string `json:"reviewer"`
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := workflow.Open(s.project)
	if err != nil {
		replyErr(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := p.Review(req.Card, req.Reviewer, req.Decision, req.Note); err != nil {
		replyErr(w, http.StatusBadRequest, err)
		return
	}
	s.reply(w)
}

func (s *server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To    int    `json:"to"`
		Actor string `json:"actor"`
		Note  string `json:"note"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := workflow.Open(s.project)
	if err != nil {
		replyErr(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := p.Rollback(req.To, req.Actor, req.Note); err != nil {
		replyErr(w, http.StatusBadRequest, err)
		return
	}
	s.reply(w)
}

// ---- view model ----

type uiCard struct {
	*state.Card
	Section       string `json:"section"`
	ChangeSummary string `json:"change_summary"`
}

type uiObligation struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Requirement   string         `json:"requirement"`
	Owner         string         `json:"owner"`
	Status        string         `json:"status"`
	EffectiveDate string         `json:"effective_date,omitempty"`
	Due           string         `json:"due,omitempty"`
	DueEvidence   *cite.Citation `json:"due_evidence,omitempty"`
	Basis         string         `json:"basis,omitempty"`
}

type uiEvent struct {
	Seq      int       `json:"seq"`
	Time     time.Time `json:"time"`
	Actor    string    `json:"actor"`
	Type     string    `json:"type"`
	Summary  string    `json:"summary"`
	Hash     string    `json:"hash"`
	UndoneBy int       `json:"undone_by,omitempty"`
}

type uiState struct {
	Company            company.Profile         `json:"company"`
	People             []company.Person        `json:"people"`
	DefaultReviewer    string                  `json:"default_reviewer"`
	EscalationReviewer string                  `json:"escalation_reviewer"`
	AsOf               int                     `json:"as_of"`
	Versions           []state.VersionIngested `json:"versions"`
	Cards              []uiCard                `json:"cards"`
	Obligations        []uiObligation          `json:"obligations"`
	Tasks              []state.Task            `json:"tasks"`
	Events             []uiEvent               `json:"events"`
	Available          []string                `json:"available"`
	Model              string                  `json:"model"`
	ModelError         string                  `json:"model_error,omitempty"`
}

func (s *server) reply(w http.ResponseWriter) {
	v, err := s.snapshot()
	if err != nil {
		replyErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *server) snapshot() (*uiState, error) {
	p, err := workflow.Open(s.project)
	if err != nil {
		return nil, err
	}
	st, err := p.State()
	if err != nil {
		return nil, err
	}
	events := p.Log.Events()
	_, undone, err := state.Effective(events)
	if err != nil {
		return nil, err
	}
	cc := p.Company
	out := &uiState{
		Company: cc.Company, People: cc.People,
		DefaultReviewer: cc.Routing.DefaultReviewer, EscalationReviewer: cc.Routing.EscalationReviewer,
		AsOf: st.AsOfSeq, Versions: st.Versions, Tasks: st.Tasks,
		Cards: []uiCard{}, Obligations: []uiObligation{}, Events: []uiEvent{},
		Available: s.available(st), ModelError: s.modelErr,
	}
	if s.model != nil {
		out.Model = s.model.Name()
	}
	if out.Versions == nil {
		out.Versions = []state.VersionIngested{}
	}
	if out.Tasks == nil {
		out.Tasks = []state.Task{}
	}
	for _, c := range st.Cards {
		ch := c.Result.Impact.Change
		out.Cards = append(out.Cards, uiCard{Card: c, Section: sections(ch), ChangeSummary: summary(ch)})
	}
	for _, ob := range cc.Obligations {
		u := uiObligation{ID: ob.ID, Title: ob.Title, Requirement: ob.Requirement, Owner: ob.Owner, Status: ob.Status}
		if eff, ok := st.Obligations[ob.ID]; ok {
			u.Status, u.EffectiveDate, u.Due, u.DueEvidence, u.Basis = eff.Status, eff.EffectiveDate, eff.Due, eff.DueEvidence, eff.Basis
		}
		out.Obligations = append(out.Obligations, u)
	}
	for _, e := range events {
		out.Events = append(out.Events, uiEvent{
			Seq: e.Seq, Time: e.Time, Actor: e.Actor, Type: e.Type,
			Summary: eventSummary(e), Hash: short(e.Hash), UndoneBy: undone[e.Seq],
		})
	}
	return out, nil
}

// available lists *.md files in the docs folder that are not ingested yet.
func (s *server) available(st *state.State) []string {
	matches, _ := filepath.Glob(filepath.Join(s.docs, "*.md"))
	done := map[string]bool{}
	for _, v := range st.Versions {
		done[v.Doc] = true
	}
	out := []string{}
	for _, m := range matches {
		if name := filepath.Base(m); !done[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ---- helpers ----

func decodePost(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		replyErr(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		replyErr(w, http.StatusBadRequest, fmt.Errorf("bad request: %w", err))
		return false
	}
	return true
}

func replyErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
