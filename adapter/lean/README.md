# LEAN adapter

The deliberately thin Python boundary to QuantConnect LEAN.

Built so far: `algorithm.py` is a `QCAlgorithm` that publishes one
`market.bar.completed` envelope per completed daily bar — warm-up bars
included — carrying both the split-adjusted and raw price views (ADR 0004),
continuing the Go engine's own input sequence (`cmd/engine/engine.go`'s
package doc, "Wire contract: the adapter's first bar must carry Sequence 2").
Account snapshots continue that same single sequence after the bar they
follow: configuration 1, bar 2, snapshot 3, bar 4, snapshot 5, including
warm-up. Both input types use the same reply validation for run identity,
sequence, causation, correlation and the hash of the exact payload bytes.
Warm-up is counted in bars, not calendar days, but every warm-up bar is still
sent: the reducer builds N and the Entry/Exit Channels from every completed
bar it receives (`internal/strategy/reducer.go`), so withholding LEAN's
warm-up bars would starve those figures rather than suppress a decision, and
which bars a strategy gets to see is itself a methodology choice this adapter
does not make. `client.py` is the transport, reused from the measured ADR
0014 spike (`spike/`); `publisher.py` maps a LEAN bar and its raw counterpart
to the wire payload and reports LEAN's portfolio without any methodology.

After each bar's decisions have been received, the adapter sends one
`account.snapshot` from `Portfolio.TotalPortfolioValue` (equity) and
`Portfolio.Cash` (available cash), in USD. Its `as_of`, `event_time` and
`recorded_at` equal that bar's own UTC `period_end`; snapshot times strictly
increase. The payload uses `event.AccountSnapshotSchemaVersion` (2), with
all four fields required by `event.AccountSnapshotPayload`. Invalid figures
or a failed bar or snapshot exchange stop the run.

LEAN's starting cash is the run's own `cash` setting in `run.json`, required
and never defaulted. Set it to the configuration's
`notional_account.starting_equity`. The first snapshot reports LEAN's equity
as the account's actual equity, and the engine's Notional Account measures
drawdown against the configured starting figure (ADR 0007). A run whose cash is
half the configured figure or less reads as a drawdown past the rule's 50%
asymptote on that first snapshot, and the engine halts, correctly.

There is no opening snapshot: warm-up bars precede StartDate, and the reducer
cannot size a Unit on its first bar because N and the channels use preceding
bars. The snapshot after bar 1 therefore arrives before any sizing is possible.
Under ADRs 0010 and 0020, a snapshot is eligible when its `as_of` is no later
than the decision bar's previous close, so bar t's close supplies the basis
for bar t+1. Warm-up follows exactly the same protocol. The adapter remains
backtest-only and submits no orders; LEAN end-to-end acceptance is separate.
See ADR 0020's 2026-09-24 producer amendment (#158).

**Delistings stop the run; they are never published.** LEAN reports a
delisting as the end of a ticker's map file, and its `Delisting` carries no
reason. A conversion therefore reads exactly like a delisting: in a real run
LEAN reported `GOOAV`, the when-issued class-C share that became `GOOG`, as
`DELISTED` on 2014-04-03. The engine treats a delisting as terminal (ADR 0009):
it closes any open Campaign at the last close and ignores the instrument
afterwards. So publishing an untrustworthy one could record a Delisting Exit
that never happened.

On `DELISTED` for its instrument, the adapter publishes that slice's bar and
snapshot, if the slice has one, and then stops the run, naming the instrument.
A delisting warning is logged, and the run continues. A ticker change
(`SymbolChangedEvents`) is logged and never published; the adapter holds
`instrument_id` constant, so a rename changes nothing for one instrument. This
is a backtest rule. In live trading the broker processes a delisting and the
system learns of it through reconciliation (ADR 0019, #113). Publishing
delistings needs a corporate-actions source that states *why* a security
stopped trading (#41, #112).

The adapter will still need to:

- send `account.cash-movement` events into the same input sequence (outside #158);
- normalize universe changes, corporate actions, connection changes, and brokerage events into versioned messages;
- validate returned trade proposals against current LEAN state;
- submit approved orders through LEAN, but never act on a decision that answers a warm-up bar — `OnData` already computes `warming = self.IsWarmingUp` once per bar, at exactly the point an order-submission step would sit, for #29 to check before acting on that bar's decision;
- return acknowledgements, rejections, cancellations, updates, and fills to Go; and
- enter safe mode and submit no new orders when Go is unavailable or state is uncertain.

It contains no methodology, position-sizing, pyramid, drawdown, or portfolio-risk rules — those stay in Go.

Unit tests (Python 3.9, plus the repository's Go toolchain for the snapshot
contract check):

```sh
cd adapter/lean && python3 -m unittest discover -s tests
```

The snapshot test feeds the emitted wire JSON to Go's actual envelope and
`AccountSnapshotPayload.Validate` checks and compares its type/schema with
the Go constants; it does not duplicate the Go contract in a Python validator.
