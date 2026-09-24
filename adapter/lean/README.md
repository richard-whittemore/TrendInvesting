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

The engine image is the run's own `lean_image` setting in `run.json`,
required and never defaulted: `quantconnect/lean@sha256:` followed by 64
lowercase hex characters, never a tag such as `:latest` or a dated build
tag. A LEAN run is evidence (ADR 0012, ADR 0017) — a moving tag lets
QuantConnect change the engine between one run and the next with no
recorded cause, including its fill modelling, its data-normalisation
implementation and its Python version, so a later divergence (including one
against `cmd/backtest`) could not be attributed. `algorithm.py`'s
`validate_lean_image` rejects anything else and stops the run at startup;
the accepted digest is logged as `adapter: lean_image=...` so every run's
own log records the engine that produced it.

**The launch command must use the same image the run declares.** `run.json`
only states which digest the run claims; nothing in the adapter can compel
`lean backtest` to actually launch that image, so the two must be kept in
step by hand. With the `lean` CLI, pass the digest to `--image`:

```sh
lean backtest <project> --image quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d
```

With a raw `docker run`, use the digest reference directly rather than a
tag, so Docker cannot silently resolve a different image locally:

```sh
docker run quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d ...
```

The current pinned digest is
`quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d`
(image created 2026-09-21; it is the one on this development machine).

**Upgrading LEAN is a deliberate change**, never an implicit one from a
moving tag:

1. Choose the tag to upgrade to and pull it: `docker pull
   quantconnect/lean:<tag>` (for example `latest`).
2. Read the digest of **that same tag**: `docker image inspect --format
   '{{json .RepoDigests}}' quantconnect/lean:<tag>`, and take its
   `quantconnect/lean@sha256:...` entry. Inspecting any other tag records,
   and then tests, a different image from the one chosen.
3. Update `run.json`'s `lean_image`, the launch command above, and this
   README to the new digest and its image-creation date.
4. Re-run the acceptance backtests against the new digest.
5. Record the result — pass/fail and any behavioural difference from the
   prior digest — in the pull request that bumps the digest.

There is no opening snapshot: warm-up bars precede StartDate, and the reducer
cannot size a Unit on its first bar because N and the channels use preceding
bars. The snapshot after bar 1 therefore arrives before any sizing is possible.
Under ADRs 0010 and 0020, a snapshot is eligible when its `as_of` is no later
than the decision bar's previous close, so bar t's close supplies the basis
for bar t+1. Warm-up follows exactly the same protocol. The adapter remains
backtest-only; the orders it places are described under **Orders** below.
LEAN end-to-end acceptance is separate. See ADR 0020's 2026-09-24 producer
amendment (#158).

**Delistings stop the run; they are never published.** LEAN reports a
delisting as the end of a ticker's map file, and its `Delisting` carries no
reason. A conversion therefore reads exactly like a delisting: in a real run
LEAN reported `GOOAV`, the when-issued class-C share that became `GOOG`, as
`DELISTED` on 2014-04-03. The engine treats a delisting as terminal (ADR 0009):
it closes any open Campaign at the last close and ignores the instrument
afterwards. So publishing an untrustworthy one could record a Delisting Exit
that never happened.

On `DELISTED` for its instrument, the adapter publishes that slice's bar and
snapshot, if the slice has one, then sends `adapter.run.stopped` (reason
`delisted`, naming the instrument; `event.AdapterRunStoppedEventType`,
`internal/event/run_stopped.go`) immediately before `replay.run.completed`,
and finally stops the run. This is what lets a journal tell a run the adapter
deliberately stopped apart from one that simply reached its last bar (ADR
0012) — before this event existed, both cases ended a journal identically,
at a plain `replay.run.completed`. A delisting warning is logged, and the run
continues. A ticker change (`SymbolChangedEvents`) is logged and never
published; the adapter holds `instrument_id` constant, so a rename changes
nothing for one instrument. This is a backtest rule. In live trading the
broker processes a delisting and the system learns of it through
reconciliation (ADR 0019, #113). Publishing delistings needs a
corporate-actions source that states *why* a security stopped trading (#41,
#112).

**A startup failure sends no `adapter.run.stopped` event.** `Initialize`'s
own `except` block (`self.stop(...)`) can fire before `self.client` or
`self.publisher` exist at all — no connection, no sequence, and no run for
this event to belong to. That path is unchanged: it still just stops the
algorithm and logs, with nothing recorded in any journal, because there is no
journal yet to record it in.

**Orders.** `orders.py`'s `OrderDesk` turns the engine's decisions into LEAN orders once
both of a bar's exchanges (the bar and its snapshot) have been answered. It
validates each decision against LEAN's current state and places, amends or
cancels the one order the decision names. The engine decides every level,
quantity and N; the adapter computes none of them and never combines two
levels.

| Decision | LEAN order |
| --- | --- |
| `strategy.trade.proposed` | buy stop-market, **DAY**, at `entry_level`, for `quantity` |
| `strategy.add.proposed` | buy stop-market, **DAY**, at `level`, for `quantity` |
| `strategy.proposal.expired` (kind `entry` or `add`) | cancel that proposal's order if it is still working |
| `strategy.campaign.opened` | none: its frozen `campaign_n` is kept for its Exit Orders' slippage |
| `strategy.exit-order.set` | the Unit's one **good-till-cancelled** sell stop-market, at `level`, for the Unit's `quantity`; a later level for the same Unit **amends** that order and never adds a second |

- **The order tag is the decision id**: the trade or Add proposal's id, or, for
  an Exit Order, the id of the `strategy.exit-order.set` now in force (an
  amendment updates the tag with the level). A decision whose id is already
  on an order in LEAN's own order book, in any state, is not submitted again,
  so redelivering a proposal never opens a second position.
- **A proposal is valid only for the bar after the one that produced it**
  (ADR 0005's window; `EarliestFillAt`): its `period_end` must be the bar
  just published. Entries and Adds are DAY orders, so each is live for that
  one session. Anything else is rejected as stale.
- **Every rejection is logged with its reason** as `adapter: REJECTED <type>
  <id>: <reason>`: an instrument that isn't this run's symbol or isn't
  tradable, a quantity that isn't a positive whole number, a level that isn't
  a positive price, a direction other than long, a stale proposal, one already
  submitted, an order LEAN itself refuses, an Exit-Order level older than the
  one in force, and any new sell order that would take the working sell
  quantity past the holding. The totals are logged at the end of the run.
- **An Exit Order amendment LEAN doesn't acknowledge leaves the previous level
  in force**, and is logged (ADR 0019's amendment).
- **A decision answering a warm-up bar is never acted on**, and **nothing is
  submitted when Go is unreachable**: a failed exchange stops the run through
  the existing fail-closed path before any of that bar's decisions are read.
  State the adapter can't reconcile also stops the run: an unknown schema
  version of one of these decision types, an Exit Order for a Campaign whose
  frozen N was never sent, or an Exit-Order level for a Unit whose LEAN order
  is no longer working or sells a different quantity.

**Costs (ADR 0013).** Every order's fill slips by `slippage_n` × the N the
engine supplied with it: a trade proposal's `n`, an Add proposal's
`campaign_n`, and the Campaign's frozen `campaign_n` (from
`strategy.campaign.opened`) for an Exit Order, looked up by the order's tag.
An order with no supplied N raises rather than slipping by zero.
`slippage_n` is the run's own required setting in `run.json`, never
defaulted; set it to the configuration's `slippage_n` (0.05 in the Baseline),
as `cash` must equal `notional_account.starting_equity`. Commission uses
LEAN's `InteractiveBrokersFeeModel`; IBKR Pro Fixed is the working assumption
(#81).

**The startup fill-model report.** In a LEAN run LEAN's fills are the
evidence; `cmd/backtest` remains the reference implementation of ADR 0005, and
the two are compared rather than forced to agree. At startup the adapter logs
`adapter: fill model: ...` lines stating every respect in which LEAN's fills
depart from ADR 0005 and ADR 0013: gap-at-open behaviour, exact touches,
same-bar ambiguity, intrabar ordering, DAY-order lifetime, LEAN's default
equity slippage and the commission schedule. LEAN's source is not available
to the adapter, so the statements about LEAN's own behaviour are marked
unconfirmed; the LEAN acceptance run against the pinned image is what
confirms them.

**What works end to end now, and what waits for #30.** Returning fills and
order lifecycle to Go is #30. Until it lands:

- works now in a LEAN run: entries and Adds are placed as DAY stop orders
  from the engine's proposals, cancelled when their proposal expires,
  deduplicated from LEAN's order book, and filled by LEAN with the slippage
  and commission above;
- waits for #30: the engine never learns of a LEAN fill, so it never opens a
  Campaign, never emits `strategy.campaign.opened` or
  `strategy.exit-order.set`, and never proposes an Add. **No Exit Order is
  placed in a LEAN run yet**, so a LEAN position has no Protective Stop at
  the broker. The Exit-Order mirroring is built and unit-tested against
  fixture decisions only.

The adapter will still need to:

- send `account.cash-movement` events into the same input sequence (outside #158);
- normalize universe changes, corporate actions, connection changes, and brokerage events into versioned messages;
- return acknowledgements, rejections, cancellations, updates, and fills to Go (#30); and
- reconcile its orders and holdings with the broker (ADR 0019).

It contains no methodology, position-sizing, pyramid, drawdown, or portfolio-risk rules — those stay in Go.

Unit tests (Python 3.9, plus the repository's Go toolchain for the contract
checks):

```sh
cd adapter/lean && python3 -m unittest discover -s tests
```

The snapshot test feeds the emitted wire JSON to Go's actual envelope and
`AccountSnapshotPayload.Validate` checks and compares its type/schema with
the Go constants; it does not duplicate the Go contract in a Python validator.
The order tests (`tests/test_orders.py`) drive the algorithm with fixture
decisions and assert on the orders that reach LEAN's order book; their
fixtures are checked against the Go payload types' fields and schema versions
by `tests/testdata/order_decisions_contract.go`.
