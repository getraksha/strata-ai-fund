# Strata — Technical Design

Go 1.21+, standard library only. ~4.2k lines of Go, ~1.4k lines of tests (55 test cases, all offline).
LLM: OpenAI Chat Completions with strict JSON schema, called over plain `net/http`.

## 0. Architecture

```
               testdata/v1..v3.md                    testdata/company.json
                      │                                       │
              ┌───────▼────────┐                      ┌───────▼────────┐
              │ clause         │ parse, byte offsets   │ company        │ load, validate links
              └───────┬────────┘                      └───────┬────────┘
              ┌───────▼────────┐                              │
              │ diff           │ align clauses, word edits    │
              └───────┬────────┘                              │
              ┌───────▼────────┐◄─────────────────────────────┘
              │ impact         │ change → obligations/projects/docs → reviewers   (deterministic)
              └───────┬────────┘
              ┌───────▼────────┐        ┌─────────┐
              │ assess         │◄──────►│ llm     │  judgment only; guardrails in assess
              └───────┬────────┘        └─────────┘
              ┌───────▼────────┐
              │ workflow       │ ingest / review / rollback rules
              └───────┬────────┘
              ┌───────▼────────┐
              │ state          │ append-only hash-chained event log + replay
              └───────┬────────┘
          ┌───────────┴───────────┐
     cmd/strata (CLI)        cmd/strata serve (web UI, same project)
                                                   cite: Citation + Verify, used by every layer
                                                   eval: scores assess against the answer key
```

Design rule: **deterministic code owns facts, the model owns interpretation.** Where text came from, what changed, what is linked to what and who may decide are computed by code. The model only answers "what does this mean and what should we do", and everything it claims is checked by code before it is shown or stored.

Two layers of commands share the engine:

- `diff`, `map`, `assess`, `eval`: stateless, one stage each, for inspection and measurement.
- `init`, `ingest`, `cards`, `show`, `review`, `rollback`, `log`, `state`, `serve`: the product workflow on a persistent project. `ingest` = diff + map + assess + persist.

## 1. Ingestion

- Input: markdown with YAML-style front matter (`docket`, `agency`, `issued`), `#` title, `##` groups, `###` numbered clauses (`### 4.2 Inspection Reporting`).
- `clause.Parse` produces clauses with exact byte spans for heading, body and whole clause. Front matter keys go to `Document.Meta`.
- **Failure path:** documents that break the format are rejected, not half-parsed: no numbered clauses, duplicate clause numbers, text outside any clause, unnumbered `###` headings, heading level 4+, unclosed front matter. Each has a test.
- On ingest the file's SHA-256 is recorded. When the next version is diffed, the previous file is re-read and its hash must match, so a version edited after ingest is refused.
- Both versions must declare the same docket.

**Divergence from production:** no PDF/HTML parsing or docket scraping. A production ingester would convert filings to the same `Clause` model (paragraph-level fallback for unnumbered text); everything downstream is format-independent.

## 2. Version diffing

Clause alignment in four passes (`internal/diff`):

1. **Identical text** (whitespace-insensitive): same number → unchanged; different number → `renumbered`.
2. **Same number**, similarity ≥ 0.5 or same heading → `modified`.
3. **Different number**, similarity ≥ 0.6, best pairs first → `renumbered_modified`.
4. Leftovers → `deleted` / `added`.

Similarity is word-level LCS: `2·LCS / (len(a)+len(b))`. Within a matched pair, the LCS walk groups changed words into edits, each citing the exact source span on both sides (`"annually."` → `"quarterly."`).

Handled cases (all in the test fixtures): renumbering caused by an insertion (§2.3 → §2.4), line reflow with identical text (§3.2, reported as unchanged), word swaps, modal shifts (`should` → `shall`), insertions, deletions, cosmetic rewording.

Complexity: pass 3 compares remaining clause pairs (O(N·M) LCS computations). Fine for hundreds of clauses; at larger scale, block candidates with shingling/MinHash before LCS.

## 3. Evidence-linked extraction

The prototype does not extract an obligation register from regulation text; the register is supplied as company context (§4). What is extracted and evidence-linked:

| Output | Evidence |
|---|---|
| Each change | Old and new clause spans + per-edit spans (deterministic) |
| Topic match for unlinked changes | The keyword occurrence in the clause, cited |
| Draft/final status | Model quote, located in the document |
| Materiality / action / direction | Model quotes, located in the old or new clause |
| Obligation due date | Regex match "within N days of the effective date" in the obligation's source clause, cited |
| New requirement with no obligation | Proposed as an action on item `NEW`, routed by topic |

## 4. Company-context data model

`testdata/company.json`, loaded and validated by `internal/company`:

```
Company profile   customers, HFTD tiers served           → applicability reasoning
Obligation        id, requirement, sources[{docket, clause, as_of}], owner, topics, status,
                  project_ids, document_ids
Project           id, owner, due, obligation_ids, assumptions[]   ← what a change can break
Document          id, owner, type, obligation_ids
Person            id, role, topics                        → routing
Topic             id, keywords                             → fallback matching
Routing           default_reviewer, escalation_reviewer
```

Links may be declared from either side (obligation → project or project → obligation) and are unioned. Every reference is validated; unknown owners, projects, documents, topics or duplicate ids are errors listing every problem. The file's SHA-256 is bound to the project at `init`; if the file changes afterwards, every project command refuses to run.

## 5. Citation verification

`cite.Citation{doc, clause_id, start, end, line_start, line_end, quote}`; `cite.Verify` re-reads `source[start:end]` and requires byte equality with `quote`.

Verification points:

| Stage | Check |
|---|---|
| diff | Every change and edit citation verified before output (`diff.VerifyAll`); CLI refuses to print otherwise |
| map | Topic evidence verified (`impact.VerifyAll`) |
| assess | Each model quote must be found in the clause it claims (`old` or `new`). Exact match first, then whitespace-flexible match; the stored citation is always the real source text. Quotes under 3 characters are rejected. A failed quote is dropped and the change escalates. A material judgment with no verified quote escalates. |
| status | The deciding quote must exist in the document |
| state | Due-date evidence is a citation into the final version |

Result on real runs: 58 model quotes over 3 eval runs, 58 verified, 0 rejected. Tests prove rejection of invented quotes, quotes attributed to the wrong version, tampered quotes and out-of-range offsets.

## 6. Confidence and escalation

The model returns `rule_change_material`, `company_action_required`, `direction` (stricter / looser / new / removed / cosmetic / informational / unclear), `summary`, `rationale`, `quotes`, `actions`, `confidence`, `ambiguous`, `ambiguity_reason` under a strict JSON schema; unknown fields are rejected.

A change escalates (card status `needs_expert`, counsel added to reviewers) if **any** of these hold:

- direction is `unclear` or the model sets `ambiguous`
- confidence < 0.7 (configurable)
- any quote fails verification, or a material claim has no verified quote
- an action targets an item the mapper did not find (only mapped ids or `NEW` allowed)
- `company_action_required` disagrees with the action list
- the model call fails, is refused, is truncated, or returns output that does not match the schema — **fail closed**: treated as material and action-required

Renumbered clauses with identical text are decided by rule, with no model call.

**Observed:** self-reported confidence is uncalibrated (0.87–1.00 on everything). Escalation therefore does not rely on it alone; the ambiguity flag and the deterministic checks do the work. On the eval, the only escalated change was the planted ambiguous one, in every run.

## 7. Reviewer routing

Computed deterministically in `internal/impact`:

1. **Direct**: obligations whose `sources` include the changed clause (old id) → obligation owner. Their projects and documents are hit `via_obligation` → those owners too. One change can route to several people (§4.2 → Raj for the obligation, Dana for the reporting calendar).
2. **Topic** (only when nothing is linked directly): topic keywords found in the clause → obligations and documents with that topic (marked `topic`, a weaker link) → people covering the topic.
3. **Gap**: an added clause with no linked obligation is flagged as a possible new requirement.
4. **Default**: no owner found → default reviewer.
5. **Escalation**: assess adds the escalation reviewer with the reason.

Decision rules, enforced server-side in `workflow.Review` (the UI mirrors them to explain disabled buttons):

- Reviewer must be a known person; a decided card cannot be decided again (roll back instead).
- `needs_expert` cards: escalation reviewer only.
- Other cards: a routed reviewer or the escalation reviewer.
- Rejection requires a note.
- Approval turns the card's actions into tasks for each affected item's owner.

## 8. Audit history and rollback

**Storage:** `<project>/events.jsonl`, one JSON event per line, opened `O_APPEND`, fsynced per event.

**Event types:** `context_loaded`, `version_ingested` (status, evidence, obligation effects), `change_assessed` (full assessment including raw model output), `review_decided`, `rolled_back`.

**Tamper evidence:** each event stores `hash = SHA-256(seq, time, actor, type, prev_hash, data)` and the previous event's hash. Every project command and every UI request re-opens the log and verifies sequence and chain; an edited, deleted or reordered line fails with the offending event number (tested for all three).

**State = replay.** No separate state file. `state.Replay` applies events in order; `state.Effective` applies rollbacks.

**Rollback** is an event: `rolled_back{to_seq, note}`. Events after `to_seq` that were effective stop counting and are marked "undone by #N"; nothing is deleted. Card ids are never reused after a rollback, so an id means one thing everywhere in the log.

**Final decisions drive state:** when a version is classified final (and not escalated), every obligation sourced from the docket becomes `effective`; if its source clause states "within N days of the effective date", a due date is computed and cited (OB-01: due 2026-11-09 per v3 §3.3).

Verified on a real UI session: 18 events; v3 FINAL with 9 obligations effective; C11 escalated; C4 approved by Raj (#17) and undone by his rollback (#18) with the note "c4 approval was by mistake".

## 9. Evals

**Answer key:** `testdata/expected_changes.json`, hand-written: for each planted change, `material`, `action`, `escalate`; for each version, draft/final.

**Harness:** `strata eval -model M -runs N` runs diff and map once, then assess N times, and scores each judgment per change, plus draft/final per version, plus quotes verified/rejected, plus stability (same verdict in every run).

**Results**

| Model | Runs | Material | Action | Escalate | Stable | Status | Quotes rejected |
|---|---|---|---|---|---|---|---|
| gpt-6-luna | 3 | 36/36 | 32/36 | 36/36 | 11/12 | 9/9 | 0 of 58 |
| gpt-4o | 1 (earlier schema) | — | — | 8/8 (v1→v2) | — | correct | 0 |

The 4 action misses: §7.1 (3/3 runs; the clause alone does not show that deadlines start — handled by project state, §8) and §5.1 (1 run; always escalated, so a human decides). gpt-4o, on the earlier single-field schema, produced actions for a pure definition (§2.3) and amended an existing obligation instead of proposing a new one (§5.3); gpt-6-luna got both right. Model choice is a flag; the guardrails are identical across models.

**Iteration log (what the eval changed):** the first eval showed §1.2 flipping between material and not material across runs. Cause: one "material" field mixed "did the rule change" with "does this company need to act". Splitting it into two fields made §1.2 stable 3/3.

**Unit tests (offline, `go test ./...`):** parser rejections; diff against the answer key; citation tamper detection; company-context validation; mapper against the expected impact table; model client against a local HTTP stub (strict schema sent, retries, refusals, non-JSON); assess guardrails with a scripted model returning bad answers (invented quote, wrong version, unknown item, inconsistency, low confidence, ambiguity, API error, malformed output); log tampering (edit, delete, reorder); full ingest → review → final → rollback → re-ingest loop.

## 10. Data isolation

- One project = one directory = one company context = one audit log. Nothing is shared between projects.
- The company context file is bound by hash; a project cannot silently start using another company's data.
- Model calls send only what one change needs: the old/new clause text, the edits, the affected items' titles, requirements and assumptions, and three company facts. No other company data, no other changes.
- Raw model output is stored with each assessment for audit.

Production: per-tenant storage and encryption keys; tenant id on every event; model provider configured for zero data retention; no cross-tenant caching of prompts or results.

## 11. Security

- Web UI binds to `127.0.0.1` by default. No authentication: the "acting as" picker stands in for login. All rules are enforced server-side regardless of what the UI sends.
- Ingest accepts only file names that exist in the configured docs folder (no paths from the browser; tested against `../go.mod`).
- Request bodies limited to 1 MB; unknown JSON fields rejected.
- API key read from the environment only.
- Regulation text is untrusted input to the model (prompt injection). Mitigations: strict output schema; every quote verified; actions restricted to known item ids; the model cannot write state — it only proposes; a person decides.

Production: SSO and role-based permissions (including who may roll back), CSRF protection, TLS, and anchoring the log head hash outside the file (database row or signed checkpoint), since a local hash chain detects edits but not a full rewrite of the file.

## 12. Divergences from the PRD and known limitations

| Item | Status |
|---|---|
| PDF/HTML ingestion | Not built; markdown with numbered clauses only |
| Obligation register extraction | Supplied as company context; new requirements surface as `NEW` actions |
| Effective date | Taken as the decision's issued date; correct here ("effective today"), not in general |
| Clause renumbering vs obligation links | Links use the clause id recorded on the obligation; not carried forward after renumbering |
| Rollback permissions | Any known person can roll back (seen in the demo: the vegetation lead rolled back); production restricts to the regulatory manager/admin |
| Concurrency | Single user; UI serializes writes; no file locking between CLI and UI |
| Scale | State is rebuilt from the full log on every command; snapshots needed for large histories |
| Log integrity | Detects edits, deletions, reordering; not a full-file rewrite (needs external anchor) |
| Confidence | Model self-reported; uncalibrated (§6) |
| Eval size | 12 planted changes, 3 versions, 1 docket; enough to prove the mechanism, not to estimate production accuracy |

## 13. Running it

Step-by-step test instructions for both front ends are in the [README](../README.md). Summary:

```
go test ./...                                  # offline: scripted model, no API calls
go build -o strata ./cmd/strata
export OPENAI_API_KEY=... OPENAI_MODEL=gpt-6-luna
```

| Workflow | Commands |
|---|---|
| Web UI | `./strata serve -project ui-test`, open http://127.0.0.1:8080, then ingest v1 → v2 → v3, review cards as different people, roll back from the audit log |
| CLI (same project model) | `init`, `ingest`, `cards`, `show`, `review -as P-0x -approve/-reject`, `state`, `log`, `rollback -as P-0x -to N -note ...`, all with `-project DIR` |
| Engine stages | `diff`, `map`, `assess` on two version files: one stage at a time, stateless |
| Evals | `./strata eval -runs 3`: scorecard against `testdata/expected_changes.json` |
