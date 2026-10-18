# The momentum universe: how the IRAs' engine sees the market

## What it does and why it exists

Each month the IRAs' momentum engine ranks the 500 most-traded US common
stocks (by one session's dollar volume, at $5 or more), scores each on its
last 253 sessions of prices, and picks its book from them (ADR 0026, point
1). It needs two things for that: the whole market's prices on the last
session of the month, to rank, and a year of prices for the 500 it ranked,
to score.

The first way built fed it a daily price bar for every one of about 5,000
common stocks, every session. That works, but it is about 1.3 million
records a year and makes a new engine replay a year of every stock before it
can decide. The owner chose a smaller shape on 2026-10-16:

- **one universe snapshot per session**: every common stock's raw close and
  volume, and its sector label, in a single record;
- **daily bars only for what the engine trades or watches every day**: SPY
  (the trend), IEF and SHY (risk-off and bonds), and whatever is held;
- **at each month's last session, one price history per ranked stock**: the
  500 stocks the engine will rank from that day's snapshot, each with its last
  253 sessions of split- and dividend-adjusted closes, in one record.

The engine never reads a bar for every listed stock in a session.

## The configuration says which shape the run reads

The IRA's configuration (version 6) must say which of the two shapes its
stream is, in the momentum section's `universe_input`:

- `"compact"`: the shape above, written by `encode --ranker`;
- `"bars"`: a daily bar for every stock ranked, written by `encode
  --universe-members`.

A configuration with no `universe_input`, or any other value, is refused
before the run starts, and the value is part of the configuration hash. The
engine never guesses the shape from what arrives. `encode` run with neither
`--ranker` nor `--universe-members` writes a bars stream with bars only for
the instruments it names, so most ranked stocks have no bar: it is not a
stream to decide from (see below). `universe-rank`
refuses a configuration that says `"bars"`, since a bars run reads no
histories.

## How the 500 are chosen without ranking twice

Ranking is a rule, and rules live in Go (ADR 0026, point 1). The market-data
step that writes the histories is Python, so it does not rank: it asks a
small Go command, `universe-rank`, which runs the engine's own ranking code
on the snapshot and reads the IRA's own configuration. The engine then ranks
the same snapshot again when the histories arrive and refuses them if they
name any other set of stocks. A ranker built from older code, or run with the
wrong configuration, therefore stops the run instead of changing what it
trades.

## The same decision either way

The histories carry exactly the closes the stock's daily bars would have
carried, adjusted the same way from the same base, and a session missing from
a stock's bars (bad data, a day it did not trade) is missing from its history
too. So the engine decides the same thing from either stream. Two tests hold
this in place:

- `TestTheCompactStreamDecidesAsTheFullBarsDo`
  (`internal/targetweight/momentum_universe_test.go`) runs two months of
  invented prices through the engine both ways, with orders filled at the
  venue, and requires every decision, order and release to be byte for byte
  the same;
- `test_the_momentum_engine_decides_the_same_from_the_compact_stream_as_from_full_bars`
  (`adapter/marketdata/tests/test_compact.py`) encodes the same invented
  recordings both ways with the real Go ranker and feeds each to a fresh
  engine (`tests/testdata/decision_equivalence.go`).

`TestTheCompactStreamCarriesNoBarPerUniverseMember` and the encoder's
`test_counted_per_session_no_input_is_written_per_listed_stock` count the
inputs per session.

A ranked stock's raw price for orders comes from its universe row, the same
unadjusted close its bar would carry. A ranked stock you hold has a bar too,
and the bar's raw close and the universe row's close must agree; the engine
stops the run if they do not. Any other stock you hold (one the snapshot
leaves out, or ranks below the 500) is priced from its bar alone.

In a run configured `"compact"`, the engine expects histories at every
month's last session, from the very first. If a month's last session closes
with its universe snapshot but no history for the stocks it ranks (a ranker
that wrongly said it was not a decision session, or named no stocks, or a
stream written without `--ranker`), the engine does not decide from the few
stocks it has bars for: it journals that month's decision withheld, with the
reason `histories-missing`, and alerts you as missing data. In a run
configured `"bars"`, a history is refused as it arrives, and a month's last
session whose snapshot ranks any stock without a bar that session is
withheld as `missing-bars` the same way, its detail saying how many of the
ranked stocks have no bar. Every stock ranked needs its bar: a stream
carrying only the bars of SPY, IEF, SHY and the stocks you hold is withheld,
even when those stocks rank. A stock with bad data does not cause this,
since `encode` leaves it out of that session's universe snapshot as well as
its bars ([market data](market-data.md)), so it is not ranked that month.

Tests: `TestTheFirstCompactDecisionSessionWithoutHistoriesIsWithheld`,
`TestAUniverseOnlyCompactSessionIsWithheld`,
`TestADecisionSessionWithoutItsHistoriesIsWithheld`,
`TestABarsRunReadsNoCloseHistory`,
`TestABarsDecisionSessionWithNoRankedMemberBarIsWithheld` and
`TestABarsDecisionSessionWithOnlyTheHeldNamesBarsIsWithheld`
(`internal/targetweight/momentum_universe_test.go`).

## Stocks with no sector label

The book holds at most 3 stocks from any one sector. Each stock's sector
comes with it in the universe snapshot: the GICS sector from iShares' IWV
holdings where IWV lists the stock, otherwise Nasdaq's
([market data](market-data.md)). Some stocks have neither, mostly companies
based abroad and recent listings.

Such a stock is ranked like any other and can be picked. It counts toward
the 25 picks, but toward no sector's limit of 3, so any number of unlabelled
stocks can be picked if their scores put them there. The decision records it
`picked`, `beyond-top-n`, or with any other status a stock can have, and its
sector as empty.

This is what the backtests did: QuantConnect's universe held stocks with no
Morningstar sector (about 4% of the ranked names, 1999-2026), and the
backtest left them uncapped. Live trading at first left them out instead,
which made the live book differ from the backtest's in most months the
parity check looked at (see [parity](parity.md)). The owner chose on
2026-10-18 to do exactly what the backtest did, so the strategy traded is
the one that was tested (ADR 0026, point 8; target-weight rules version
1.13.0). Which stocks lack a label still differs, since live trading's labels
come from iShares and Nasdaq, not Morningstar; that is measured during
dry-run and paper trading.

Tests: `TestSelectBookPicksNamesWithNoSectorAsTheBacktestDoes`
(`internal/targetweight/momentum/book_test.go`) and
`TestAMomentumRunPicksAnUnlabelledNameUncapped`
(`internal/targetweight/momentum_handler_test.go`).

## A session the exchange cancels

If the exchange closes on a day its normal rules call a session, record it
once with `marketdata retract` (see [market-data](market-data.md)). The
stream then carries a `market.session.retracted` for that day, and the
engine removes the day from its calendar: it is not a gap in anyone's prices,
so the 253-session windows reach one session further back and the engine
keeps deciding. Without it, every window that reached that day would be
short a session for a year, and every month would be withheld.

If the cancelled day was the last of its month, the day before becomes the
month's decision session. The retraction can arrive after that day has
already closed, when it was not yet a decision session:

- in a run configured `"bars"`, the engine already holds every price it
  needs and makes that day's decision at once (withheld as `missing-bars` if
  any stock it ranks had no bar that day);
- in a run configured `"compact"`, no history was sent for that day, since it was
  not the month's last when it closed. The engine waits for them: once a
  history has arrived for every stock it ranks from that day's snapshot, it
  makes the decision, exactly as it would have at that day's close had the
  calendar never listed the cancelled day. If the next session closes first,
  it journals that decision withheld as `histories-missing` instead, so no
  month passes without a decision (ADR 0026, point 1). `encode` writes a
  day's histories only with that day's session, so a stream encoded from the
  next session on does not carry them: until something sends them, expect
  the withheld decision, and an alert, in that month. A run fed a stream
  encoded after the retraction was recorded, from before that day, reads the
  retraction first and decides the day at its own close.

Tests: `TestARetractedSessionIsNoGap` (a retracted day decides exactly as a
calendar that never listed it, while the same day left in place withholds
January), `TestALateRetractionWaitsForTheLastSessionsHistories` (a late
retraction decides exactly as a calendar that never listed the day, from
either stream), `TestALateRetractionWithoutHistoriesIsWithheldAtTheNextClose`,
`TestALateRetractionBeforeAnyHistoryWaitsForThem`,
`TestALateRetractionInABarsRunWithNoRankedMemberBarIsWithheld`,
`TestAPendingDecisionThatCompletesDecides` (the decision ranks its own day's
snapshot even when the next day's has arrived),
`TestRetractingTheNextSessionDecidesTheLastOneClosed`,
`TestSessionRetractionRules` and the encoder's `RetractionTest`.

## What can go wrong, and what the engine does

The engine stops the run with a message, and decides nothing from the input,
when:

- a history names a stock the engine did not rank, or one it ranked has no
  history;
- a history arrives before its session's universe snapshot, for a session
  that is not the last of its month, for a session that has already closed
  (unless a late retraction left that session's decision waiting for it), or
  twice for one stock;
- a universe snapshot arrives while the histories of the one before it are
  still waiting for their session to close;
- a history arrives in a run configured `"bars"`;
- a ranked stock with both a bar and a history has a bar whose raw close
  differs from its universe row's close;
- a history has a price on a day the calendar does not list, or a price that
  disagrees with one the engine already holds for that stock and day, or was
  adjusted from a different base;
- a retraction names a day not on the calendar, or any day on or before the
  last session closed (a retraction must name a session after it; a day
  before it that never closed cannot be repaired by retracting it), a day
  whose bars, universe snapshot or histories have arrived (it traded), or
  would leave the calendar with no session after the last one closed.

A second copy of the same retraction changes nothing, and a later calendar
that still lists the cancelled day is read without it.

A month's last session with no history for the stocks it ranks in a compact
run, or a ranked stock without its bar in a bars run, is not a stop: the decision is
withheld as `histories-missing` or `missing-bars` (above).

A stock whose history is short or has a gap is recorded in the decision as
`short-history` or `gap` and left out of that month, exactly as with daily
bars (ADR 0026, point 5).

## What backs it

- ADR 0026, points 1 (inputs, ranking in Go, decision times) and 5 (missing
  bars fail closed); ADR 0015 (the two new record types start at version 1;
  the configuration's `universe_input` is its version 6, and version 5 is
  refused); ADR 0016 (the value is part of the configuration hash; rules
  version 1.6.0);
  ADR 0004 and the adjusted-bar rule in `internal/event/bar_adjusted.go` (how
  closes are adjusted).
- `internal/event/universe_inputs.go`: `market.close.history` and
  `market.session.retracted`.
- `internal/targetweight/momentum_history.go` and `momentum_retraction.go`:
  the engine side; `cmd/universe-rank`: the ranker.
- `adapter/marketdata/encode.py`: the compact stream and retractions.
