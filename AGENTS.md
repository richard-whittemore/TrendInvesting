# TrendInvesting — agent instructions

A research and trading platform that evaluates three declared strategies on US equities: a source-verified **Turtle** control, a rules-based **Sublime** control, and predeclared **hybrid** experiments. Go owns strategy, risk, and replay; LEAN owns market data and brokerage; a thin Python adapter joins them.

**This system will trade real money.** Correctness and auditability outrank speed and cleverness in every trade-off.

## Non-negotiables

1. **TDD.** Write the failing test first, then the code (red–green–refactor). Golden scenarios come from primary sources — transcribe them, don't invent them.
2. **`make check` must pass** before any PR: format, vet, staticcheck, race-enabled tests, coverage floor, `govulncheck`, build.
3. **Cite the source of every strategy rule.** `docs/methodology/Methodology_Analysis.md` is the reference, with page and timestamp citations and provenance tags. Never restate a rule from memory. A rule tagged DISCLOSED for the baseline cannot be changed by an implementation — that is a specification change; stop and say so.
4. **Keep source rules separate from experiments.** Turtle baseline (½N adds, 2N stop) and Sublime/hybrid variants (1 ATR adds, 3×ATR stop) are distinct configurations, never silently blended.
5. **No live-order behaviour** before the paper-trading and limited-live gates. Determinism, replay, and fail-closed states come first.
6. **Preserve failed results.** Never rewrite or delete a recorded experiment configuration or its evidence.
7. **Domain packages stay pure** — no LEAN, database, or transport imports in `internal/`; `cmd/` is composition only.

Rules 4 and 7, and the determinism requirement, are enforced mechanically by `make lint` (`depguard` and `forbidigo` in `.golangci.yml`), not just by review. If a lint rule blocks you, the answer is almost never to add a `//nolint` — say why the rule is wrong instead.

## Working a ticket

Tickets are self-sufficient and are **living records**. Update the Work log as you go (what you did, how, how you tested it), edit Findings in place, and record any concern you discover. Close only when the acceptance criteria are met, testing evidence is recorded, durable findings are committed, and every concern is filed as its own issue. Full standard: `docs/agents/issue-tracker.md`.

## Orchestration

Planning, specs, ADRs, tickets, and reviews are done by the orchestrating session. Implementation is delegated by the ticket's `tier/*` label. Independent frontier tickets are worked **in parallel**, each in its own git worktree, and land as separate PRs.

This workspace runs inside **Herdr** (`herdr --skill` prints its CLI guide) — used for long-running agents that should be visible, and for cross-vendor work in the Codex pane.

## Agent skills

### Issue tracker

GitHub Issues in `richard-whittemore/TrendInvesting`, accessed only through the GitHub MCP (the `gh` CLI is not installed). See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical roles, unchanged (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` at the root, ADRs in `docs/adr/`. See `docs/agents/domain.md`.

## Further reading

- `docs/architecture.md` — LEAN ↔ adapter ↔ Go boundary and safety invariants
- `docs/development.md` — engineering principles, package boundaries, PR expectations
- `docs/dependency-policy.md` — when a runtime dependency is acceptable
- `docs/methodology/` — source analysis, webinar transcript, plan and backlog review
- `CONTRIBUTING.md` — toolchain and local workflow
