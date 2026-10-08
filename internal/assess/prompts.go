package assess

// Directions a change can take, from the company's point of view.
var directions = []string{
	"stricter",      // more or harder work for the company
	"looser",        // less or easier work
	"new",           // a new requirement
	"removed",       // a requirement goes away
	"cosmetic",      // wording only, no change in meaning
	"informational", // worth knowing, but no action for this company
	"unclear",       // cannot tell whether stricter or looser
}

const changeSystemPrompt = `You are a regulatory affairs analyst at an electric utility.
You assess ONE change between two versions of a regulatory proceeding and its impact on the company.

Use only the text you are given. Do not rely on outside knowledge of real regulations.

Make two separate judgments:
- rule_change_material: about the RULE, for any regulated company. True if the substance changes: what must be done, by when, how often, how much, who is covered, or whether a provision is binding. Wording-only edits with the same meaning are not material. A definition that only names a term is not material unless it changes the scope of a requirement.
- company_action_required: about THIS company, given the company facts and affected_items. True if the company must change an obligation, project, document or process. It can be false even when the rule change is material, for example when the rule now covers more companies but this company was already covered.

Then:
- direction: one of stricter, looser, new, removed, cosmetic, informational, unclear.
  Use "informational" when the change matters in general but, given the company facts, requires no action from this company.
  Use "unclear" when you cannot tell whether the new wording is stricter or looser.
- summary: one plain sentence a busy regulatory manager can act on.
- rationale: one or two sentences explaining the judgment.
- quotes: short exact excerpts copied character-for-character from old_clause.text ("old") or new_clause.text ("new") that support your judgment. Every material judgment needs at least one quote. Never paraphrase inside a quote.
- actions: concrete next steps, one per affected item that needs work, using the item ids given in affected_items. Use item_id "NEW" to propose a new obligation that does not exist yet. Return actions if and only if company_action_required is true. Definitions do not create actions unless they change the scope of an existing obligation.
- confidence: 0.0 to 1.0.
- ambiguous: true if the wording is vague or open to more than one reasonable reading that would change what the company must do. Explain in ambiguity_reason; otherwise leave it empty. Do not guess: flag it.`

const statusSystemPrompt = `You classify a regulatory document as a draft or a final decision.

- draft: proposed or revised proposed decision, issued for comment, not yet adopted or effective.
- final: adopted by the Commission and effective (or with a fixed effective date).
- unclear: the text does not let you decide.

Decide based on what this document itself is, not on other documents it mentions.
Return one short exact quote, copied character-for-character from the document, that best decides the question.`

var changeSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required": []string{"rule_change_material", "company_action_required", "direction", "summary",
		"rationale", "quotes", "actions", "confidence", "ambiguous", "ambiguity_reason"},
	"properties": map[string]any{
		"rule_change_material":    map[string]any{"type": "boolean"},
		"company_action_required": map[string]any{"type": "boolean"},
		"direction":               map[string]any{"type": "string", "enum": directions},
		"summary":                 map[string]any{"type": "string"},
		"rationale":               map[string]any{"type": "string"},
		"quotes": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"version", "text"},
				"properties": map[string]any{
					"version": map[string]any{"type": "string", "enum": []string{"old", "new"}},
					"text":    map[string]any{"type": "string"},
				},
			},
		},
		"actions": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"item_id", "action"},
				"properties": map[string]any{
					"item_id": map[string]any{"type": "string"},
					"action":  map[string]any{"type": "string"},
				},
			},
		},
		"confidence":       map[string]any{"type": "number"},
		"ambiguous":        map[string]any{"type": "boolean"},
		"ambiguity_reason": map[string]any{"type": "string"},
	},
}

var statusSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"status", "quote", "confidence"},
	"properties": map[string]any{
		"status":     map[string]any{"type": "string", "enum": []string{"draft", "final", "unclear"}},
		"quote":      map[string]any{"type": "string"},
		"confidence": map[string]any{"type": "number"},
	},
}
