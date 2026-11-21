# ADR 0029: Strategies plug in through one interface; Turtle and the LEAN backtest path are retired to history

- Status: Accepted (2026-11-21, by the owner)
- Date: 2026-11-21
- Retires: ADR 0002, ADR 0003, ADR 0005, ADR 0006, ADR 0007, ADR 0008, ADR 0009, ADR 0010, ADR 0011, ADR 0012, ADR 0013, ADR 0020
- Amends: ADR 0001, ADR 0004, ADR 0014, ADR 0016, ADR 0017, ADR 0018, ADR 0019, ADR 0021, ADR 0022, ADR 0023, ADR 0024, ADR 0025, ADR 0026, ADR 0027

## In plain English

*A summary. Where it differs from the text below, the text governs.*

The program began as research into three trend strategies: Turtle, Sublime
and hybrids of the two. That research found no edge in Turtle. What it did
find was Boost and Momentum, and those are what run today: Boost on SPY in a
paper account, and Momentum in the Traditional IRA in dry run.

Turtle is still in the code, though, and in the middle of it. The engine falls
back to Turtle when a configuration names no other strategy, Turtle's sizing
types sit in the shared event code, and most engine tests use Turtle as their
example. Someone reading the code meets Turtle first, and has to be told the
history to see what actually runs.

So Turtle, and the LEAN backtest path built for it, are retired. Nothing is
thrown away: the git tag `turtle-final` keeps every line of Turtle's code, its
configurations and its evidence. The records that decided Turtle's rules stay
in `docs/adr/`, marked retired. The research that produced Boost and Momentum
stays where it is.

## Context

Boost and Momentum are target-weight strategies (ADR 0026). They trade
through the IBKR adapter (ADR 0025), are checked by reconciliation
(ADR 0019), and write a durable log (ADR 0027). None of that needs Turtle.

Turtle needs the opposite: its own reducer, sizing, indicators and universe;
`cmd/backtest` with its fill simulator and run registry; and the LEAN
adapter, which feeds backtests over the socket of ADR 0014. These were built
for a strategy that is no longer a candidate. They still pass `make check`,
and still shape shared code: the engine's fallback, the shared event types,
and the engine tests' main fixture.

The owner's goal (2026-11-21) is a codebase that is clean and makes sense at
first look, without needing its history explained, and that can easily take
future strategies, while keeping everything useful and the research that led
to Boost and Momentum.

## Decision

1. **Strategies plug in through one interface.** A strategy reads the
   engine's inputs and returns its decisions. The engine, the order planner,
   the limits, reconciliation and the journal know no strategy's rules
   (ADR 0026 already says this for target-weight strategies). A new strategy
   is a new module behind that interface, with its own ADR and its own
   chapter in `docs/manual/`.
2. **The engine selects from an explicit registry of live strategies.** A
   configuration names its strategy by id. The engine looks the id up in a
   fixed list of live strategies, today Boost and Momentum, and refuses to
   start on any id the list does not hold. There is no fallback strategy.
   This is a later change. Until it lands, the engine behaves as it does now.
3. **Turtle is retired to history.** The tag `turtle-final` marks the last
   commit that holds Turtle. It preserves the full Turtle code, its
   configurations, its journals and run registry, its fixtures and its
   evidence, so any of it can be checked out, read or rerun. The tag is never
   moved or deleted.
4. **What leaves the live tree later.** In separate changes, after this one:
   - the Turtle reducer, and its sizing, indicators and universe;
   - `cmd/backtest`, with its fill simulator and its run registry;
   - the LEAN adapter (`adapter/lean`);
   - the transport spike;
   - `runs/`, the Turtle run registry.

   Each removal keeps `make check` green and changes no Boost or Momentum
   decision. Whatever stays in shared code (the socket of ADR 0014, the
   envelope, the journal) stays because a live strategy uses it.
5. **Research.** The research that produced Boost and Momentum stays where it
   is: `research/edge-search/` and the reports in `docs/research/`. The
   Turtle, Sublime and futures research (`research/qc-cloud/`,
   `research/qc-cloud-sublime/`, `research/qc-cloud-futures/`,
   `research/futures-local/`, `docs/methodology/` and `docs/experiments/`)
   moves to a history folder with an index that says what each item was
   and what it found.
6. **Retired ADRs are kept.** An ADR that governed only Turtle gets the
   status line "Retired by ADR 0029 (2026-11-21): kept as history", and its
   text is otherwise unchanged. An ADR that still governs a live strategy,
   but has Turtle clauses, gets a dated amendment note saying which clauses
   are now moot. ADR 0021, ADR 0023 and ADR 0024 are in the second group:
   their Turtle rules are moot, but the `market.session.closed` input and the
   `market.corporate-action` event they define are read by the live engine.

## Consequences

- AGENTS.md describes the live platform. Its rule 3 names the live rule
  references, its rule 4 is stated for strategies in general, and its rule 6
  says evidence is preserved in history and never rewritten, but need not
  stay on the build path. The rule numbers do not change, because code
  comments cite them.
- Source comments that cite a retired ADR stay correct as history. A
  comment in live code that cites one only for a convention it shares (the
  declared run start of ADR 0012, cash movements of ADR 0007) should cite
  the live rule instead when that code is next changed.
- The journal readers still read Turtle's event versions: `execution.fill`
  v4, `market.bar.completed` v1 and the others the reducer used. Whether a
  reader keeps decoding them, so a Turtle journal stays readable from the
  live tree, or refuses them and leaves old journals to the tag, is decided
  in the change that removes the reducer.
- `-mode backtest` exists in `cmd/engine` for Turtle configurations and for
  the LEAN adapter's end-to-end test. The change that removes them decides
  whether the mode stays, and what for.
- The owner's manual describes Boost and Momentum. It names Turtle only
  where `journal-query` and the dashboard read Turtle's fill and bar
  versions (`docs/manual/journal-query.md`,
  `docs/manual/alerts-and-dashboard.md`); those lines change with the
  readers, in the same change.
- Running a retired strategy again means checking out `turtle-final`. It
  does not mean restoring the code to the live tree, which would be a new
  decision.
