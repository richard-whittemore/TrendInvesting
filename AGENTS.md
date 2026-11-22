# TrendInvesting — agent instructions

A trading platform that runs declared strategies on US equities through IBKR. Two strategies are live, both target-weight (ADR 0026): **Boost** (SPY, in the joint account, on paper) and **Momentum** (the Traditional IRA, in dry run). Each plugs into the engine through one strategy interface (ADR 0029). Go owns strategy, risk, and replay; the IBKR adapter owns live market data and brokerage (ADR 0025); a thin Python adapter joins it to Go. Turtle, Sublime and the LEAN backtest path are retired to history (ADR 0029, the `turtle-final` tag).

**This system will trade real money.** Correctness and auditability outrank speed and cleverness in every trade-off.

## Non-negotiables

1. **TDD.** Write the failing test first, then the code (red–green–refactor). Golden scenarios come from primary sources — transcribe them, don't invent them.
2. **`make check` must pass** before any PR: format, vet, staticcheck, race-enabled tests, coverage floor, `govulncheck`, build, and the LEAN adapter's Python suite (`make adapter-test`).
3. **Cite the source of every strategy rule.** A live strategy's rules are in its ADR and its manual chapters: ADR 0026 for Boost and Momentum, with `docs/manual/boost.md`, `docs/manual/momentum.md`, `docs/manual/momentum-universe.md`, `docs/manual/momentum-minimum-lot.md` and `docs/manual/glide-path.md`. The research they were backtested in is `docs/research/` and `research/edge-search/`. Never restate a rule from memory. An implementation cannot change a rule — that is a specification change; stop and say so. Source comments follow `docs/development.md`'s comment standard: what it does, the rule and its citation, any invariant — never a ticket number, PR number, or review round.
4. **Keep strategies separate.** Each strategy is its own module behind the strategy interface (ADR 0029). Two strategies share no rule and no state, and are never silently blended. A changed rule is a new rules version of its own strategy, decided in its ADR.
5. **No live-order behaviour** before the paper-trading and limited-live gates. Determinism, replay, and fail-closed states come first.
6. **Preserve evidence.** Never rewrite or delete a recorded configuration, result or its evidence, failed ones included. Evidence of retired work is kept in history — the `turtle-final` tag, the retired ADRs and the research history — and need not stay on the build path.
7. **Domain packages stay pure** — no LEAN, database, or transport imports in `internal/`; `cmd/` is composition only.

Rule 7 and the determinism requirement are enforced mechanically by `make lint` (`depguard` and `forbidigo` in `.golangci.yml`), not just by review; rule 4 is enforced by review. If a lint rule blocks you, the answer is almost never to add a `//nolint` — say why the rule is wrong instead.

## Working a ticket

Tickets are self-sufficient and are **living records**. Update the Work log as you go (what you did, how, how you tested it), edit Findings in place, and record any concern you discover. Close only when the acceptance criteria are met, testing evidence is recorded, durable findings are committed, and every concern is filed as its own issue. Full standard: `docs/agents/issue-tracker.md`.

## Orchestration

Planning, specs, ADRs, tickets, and reviews are done by the orchestrating session. Implementation is delegated by the ticket's `tier/*` label. Independent frontier tickets are worked **in parallel**, each in its own git worktree, and land as separate PRs.

This workspace runs inside **Herdr** (`herdr --skill` prints its CLI guide) — used for long-running agents that should be visible, and for cross-vendor work in the Codex pane.

## Agent skills

### Issue tracker

GitHub Issues in `richard-whittemore/TrendInvesting`, accessed with the `gh` CLI (installed and logged in on the Mac mini) or the GitHub MCP. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical roles, unchanged (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` at the root, ADRs in `docs/adr/`. See `docs/agents/domain.md`.

## Further reading

- `docs/architecture.md` — LEAN ↔ adapter ↔ Go boundary and safety invariants
- `docs/manual/` — the owner's manual: what each live piece does, and what to do when it goes wrong
- `docs/development.md` — engineering principles, package boundaries, source comment standard, PR expectations
- `docs/running-a-backtest.md` — running a Turtle fixture end to end (retired, ADR 0029)
- `docs/dependency-policy.md` — when a runtime dependency is acceptable
- `docs/methodology/` — the Turtle and Sublime source analysis, plan and backlog review (retired, ADR 0029). Third-party source material (the recording, its transcript, vendor PDFs) is deliberately **not** in this repository; the analysis names where it lives.
- `CONTRIBUTING.md` — toolchain and local workflow
