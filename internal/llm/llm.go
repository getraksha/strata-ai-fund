// Package llm is the narrow seam between Strata and a language model.
//
// A Request carries a system prompt, a user message and a JSON schema; the
// model must answer with JSON that matches the schema. Everything Strata does
// with the answer (quote verification, escalation) happens outside this
// package, so swapping providers does not change the trust logic.
package llm

import (
	"context"
	"encoding/json"
)

// Request is one structured-output call.
type Request struct {
	Name   string         // schema name, e.g. "change_assessment"
	System string         // instructions
	User   string         // the case to assess (JSON)
	Schema map[string]any // JSON schema the answer must match (strict)
}

// Model returns raw JSON matching req.Schema.
type Model interface {
	Complete(ctx context.Context, req Request) (json.RawMessage, error)
	Name() string
}

// Func adapts a plain function to Model. Tests use it to script model
// behavior, including bad behavior (invented quotes, errors, malformed JSON).
type Func struct {
	ModelName string
	F         func(Request) (json.RawMessage, error)
}

func (f Func) Complete(_ context.Context, r Request) (json.RawMessage, error) { return f.F(r) }
func (f Func) Name() string                                                   { return f.ModelName }
