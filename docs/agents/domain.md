# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root — the glossary of domain terms.
- **`docs/adr/`** — read ADRs that touch the area you're about to work in.
- **`docs/methodology/`** — for anything touching strategy rules, `Methodology_Analysis.md` is the source-grounded reference: it carries the Turtle and Sublime rules with page/timestamp citations, provenance tags (DISCLOSED / RECONSTRUCTED / PROXY / EXCLUDED), and the conflict ledger. Do not restate a strategy rule from memory; cite it.

If any of these files don't exist, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The `/domain-modeling` skill (reached via `/grill-with-docs` and `/improve-codebase-architecture`) creates them lazily when terms or decisions actually get resolved.

## File structure

This is a **single-context** repo:

```
/
├── AGENTS.md                       ← standing instructions (CLAUDE.md imports this)
├── CONTEXT.md                      ← glossary
├── docs/
│   ├── adr/
│   │   └── 0001-modular-monolith.md
│   ├── agents/                     ← this directory: skill configuration
│   ├── methodology/                ← source analysis, transcript, plan review
│   ├── architecture.md
│   ├── development.md
│   └── dependency-policy.md
├── internal/                       ← domain packages (no LEAN, DB, or transport imports)
├── cmd/
└── adapter/lean/                   ← thin Python boundary (reserved)
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in `CONTEXT.md`. Don't drift to synonyms the glossary explicitly avoids.

This matters more than usual here: `N`, `Unit`, `campaign`, `notional account`, `risk`, and `ATR` all have precise, easily-confused meanings, and conflating them is a known source of real bugs (see the conflict ledger in `docs/methodology/Methodology_Analysis.md` §4.2).

If the concept you need isn't in the glossary yet, that's a signal: either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/domain-modeling`).

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0007 (event-sourced orders), but worth reopening because…_

The same applies to a strategy rule: if your implementation would depart from what `Methodology_Analysis.md` records as DISCLOSED for the baseline, say so and stop — that is a specification change, not an implementation detail.
