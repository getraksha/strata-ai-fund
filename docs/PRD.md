# Strata — Product Requirements

Citation-grade regulatory change-to-action workspace for regulated enterprises.
Prototype scope: one state rulemaking docket, one utility, three versions (proposed → revised → final).

---

## 1. Problem

Regulated companies already know *when* a docket changes. Docket alerts and RSS feeds tell them a new filing exists. The work that remains is manual and slow:

1. What changed between this version and the last one, clause by clause?
2. Is this still a draft, or is it final and binding?
3. Which of *our* obligations, projects and internal documents does it affect?
4. Who must review it, and what do we do next?

Today a regulatory affairs manager answers these by reading two PDFs side by side, redlining by hand, and emailing subject-matter experts. The answers live in inboxes and spreadsheets, without a durable link back to the source text.

## 2. First user

**Regulatory Affairs Manager at a mid-size investor-owned electric utility** (100k–1M customers, operating in one or more states with wildfire-risk territory).

- Tracks 10–30 active proceedings at the state commission (PUC) at any time.
- Accountable for knowing what each new version means for the company and getting the right experts to act.
- Signs off on the company's interpretation; counsel is pulled in for ambiguous language.

Secondary users in the same workflow:

| Role | In the prototype | Needs |
|---|---|---|
| Subject-matter owner (vegetation, PSPS operations) | Raj Patel, Lena Brooks | Only the changes that touch their obligations, with the exact text |
| Compliance officer | Priya Nair | Audit trail of who decided what, and when |
| Regulatory counsel | Sam Whitfield | Only the genuinely ambiguous items, flagged with a reason |

## 3. Why alerts and search are not enough

| Tool | What it gives | What it does not give |
|---|---|---|
| Docket alerts | "Docket R.26-04-017 has a new filing" | What changed, whether it is final, what it affects |
| Search | Documents that mention a term | The delta between versions; links to the company's own obligations |
| Generic AI summary | A readable paragraph | Verifiable citations; a decision record; a defined behavior when the model is unsure |
| Manual redline | Accurate diff | Speed; consistency; a durable link from each change to obligations, owners and actions |

The gap is not detection. It is a **trusted, cited, auditable path from a changed clause to an assigned action**. Expert users will not adopt a tool whose claims they cannot check in one click, or that guesses when the language is ambiguous.

## 4. Wedge workflow (what the prototype does)

```
ingest version ──► clause-level diff ──► draft/final ──► map to company ──► assess ──► review queue ──► project state
   (v1, v2, v3)     every change cited    cited quote     obligations,       material?   routed owner      obligations effective,
                                                          projects, docs,    action?     approves/rejects  deadlines, tasks,
                                                          owners             escalate?   counsel for       audit log, rollback
                                                                                         escalations
```

1. **Ingest** the next version of the proceeding.
2. **Diff** it against the previous version at clause level. Renumbered clauses are recognized as moves, not rewrites. Line-wrap changes are ignored.
3. **Classify the version** as draft or final, with a quoted sentence as evidence.
4. **Map** each change to the company's obligations (by source clause), and from there to projects, documents and owners. Changes with no linked obligation are matched by topic or flagged as a gap.
5. **Assess** each change: did the rule materially change; does this company have to act; which way does it cut; what are the concrete actions. Every claim carries a quote that is verified against the source.
6. **Escalate** to counsel instead of guessing when the wording is ambiguous, the evidence does not check out, or the model fails.
7. **Review**: each change becomes a card routed to the owners of the affected items. Approving a card turns its actions into tasks.
8. **Maintain project state**: when a final decision is ingested, obligations become effective and deadlines are computed from the cited clause. Every step is an event in an append-only, tamper-evident log; any point can be rolled back.

### Mandatory behaviors → where they live

| Behavior | Implementation |
|---|---|
| Ingest successive versions + company context | `strata ingest`, `testdata/company.json` |
| Detect material changes | `internal/diff` (what changed), `internal/assess` (`rule_change_material`) |
| Distinguish draft from final | `assess.Status`, with verified quote; final drives state |
| Cite exact passages behind every claim | `internal/cite`; every quote verified before display |
| Map changes to obligations, projects, documents | `internal/impact` |
| Recommend action with reviewer routing | `assess` actions + `impact` reviewers + `workflow.Review` rules |
| Living, auditable project state | `internal/state` event log + replay |
| Escalate low-confidence interpretations | `assess` guardrails → `needs_expert` cards, counsel-only |

## 5. Trust and adoption metrics

Expert users adopt on trust first, speed second. Metrics in priority order:

**Trust (measured by the eval harness today)**

| Metric | Definition | Target | Prototype result (gpt-6-luna, 3 runs) |
|---|---|---|---|
| Citation validity | Model quotes found verbatim in source / all model quotes | 100% (anything else escalates) | 58 / 58 verified, 0 rejected |
| Escalation recall | Ambiguous changes escalated / ambiguous changes | 100% | 3 / 3 (§5.1 every run) |
| Escalation precision | Escalated changes that were truly ambiguous | ≥ 80% | 100% (no false escalations) |
| Materiality accuracy | Agreement with expert answer key | ≥ 95% | 36 / 36 |
| Run-to-run stability | Changes with identical verdict across runs | ≥ 90% | 11 / 12 (the unstable one was always escalated) |
| Draft/final accuracy | Agreement with expert | 100% | 9 / 9, including the "revised proposed decision" wording trap in the final |

**Trust (measured in production)**

- **Reviewer override rate**: share of cards rejected or materially edited by the routed owner. Rising rate = model or mapping drift.
- **Routing acceptance**: share of cards decided by the person they were routed to, without reassignment.
- **Missed-change incidents**: changes found later by a human that the system did not surface. Target 0.

**Adoption**

- Time from new filing to routed cards (target: minutes; today: days of manual redlining).
- Time from card created to decision, per role.
- Share of proceedings tracked in Strata vs. email/spreadsheets.
- Weekly active reviewers per company; share of obligations with source-clause links.

## 6. Out of scope for the prototype

- PDF/HTML ingestion and docket scraping (input is markdown with numbered clauses).
- Extracting a company's obligation register from scratch (it is supplied as company context).
- Multi-docket and multi-jurisdiction views.
- Authentication and role administration (an "acting as" picker stands in for login).
- Integrations with GRC, document management and ticketing systems.

## 7. Expansion beyond utilities

The core object — *a versioned regulatory text, linked clause-by-clause to a company's obligations, with cited and reviewed decisions* — is not utility-specific.

| Next segment | Source of change | What carries over | What must be added |
|---|---|---|---|
| Multi-state utilities (gas, water) | Other state PUCs | Everything | Per-state topic taxonomy; parallel dockets on one obligation |
| Energy (FERC, NERC reliability standards) | Federal orders, standard versions | Diff, citations, routing | Standard-ID based clause model; federal/state obligation layering |
| Healthcare providers / payers | CMS rules, state health departments | Draft→final logic (proposed rule → final rule) | Federal Register XML ingestion; much larger documents |
| Financial services | Banking and securities rulemakings | Obligation register mapping, audit trail | Control-library integration (GRC tools); stricter data isolation |
| Pharma / medical devices | FDA draft vs. final guidance | Draft/final distinction is central | Guidance-document structure; product-line mapping |

Sequencing: deepen in utilities (multi-state, multi-docket) → adjacent energy regulators → other sectors with the same "proposed → final" rulemaking pattern. The product does not change shape; the ingestion formats, topic taxonomy and company-context connectors do.

## 8. Risks

| Risk | Mitigation in the design |
|---|---|
| Model invents or paraphrases evidence | Every quote verified verbatim; failures escalate |
| Model guesses on ambiguous language | Explicit "unclear/ambiguous" outputs route to counsel; low confidence escalates |
| Experts distrust automated decisions | System proposes; humans decide; every decision is attributable and reversible |
| Company context goes stale | Context file is hash-bound to the project; any change is detected and blocks the run |
| Silent edits to history | Hash-chained, append-only log; rollback is itself an event |
