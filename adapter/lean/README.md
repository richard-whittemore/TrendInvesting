# LEAN adapter

The deliberately thin Python boundary to QuantConnect LEAN.

Built so far: `algorithm.py` is a `QCAlgorithm` that publishes one
`market.bar.completed` envelope per completed daily bar — warm-up bars
included — carrying both the split-adjusted and raw price views (ADR 0004;
see **Price views and raw accounting** below), continuing the Go engine's own input sequence (`cmd/engine/engine.go`'s
package doc, "Wire contract: the adapter's first bar must carry Sequence 2").
Each slice's bars are a Session (ADR 0021): after them the adapter sends
`market.session.closed`, naming those bars' instruments, in that same single
sequence. For example, in a run with no fills or order reports: configuration 1,
bar 2, session close 3, then the *previous* Session's `account.snapshot` at the
head of the next slice, before its own bar — 4, bar 5 — and so on, including
warm-up. Fills and order-change reports drained at the start of a slice take
the next numbers before that slice's snapshot and bar, so only the relative
order is fixed, not the numbers. The snapshot is not sent
inside the slice that closes the Session it reports: it is computed there and
held, then sent at the start of the following slice, before that slice's own
bar (`flush_snapshot`; see **Where each input falls** below for why). Once
orders are working, LEAN's fills and order changes join the same sequence
too, ahead of the bar for the identical reason (see **Fills and order
changes** below). Every input type uses the same reply validation for run
identity, sequence, causation, correlation and the hash of the exact payload
bytes.
Warm-up is counted in bars, not calendar days, but every warm-up bar is still
sent: the reducer builds N and the Entry/Exit Channels from every completed
bar it receives (`internal/strategy/reducer.go`), so withholding LEAN's
warm-up bars would starve those figures rather than suppress a decision, and
which bars a strategy gets to see is itself a methodology choice this adapter
does not make. `client.py` is the transport, reused from the measured ADR
0014 spike (`spike/`); `publisher.py` maps LEAN's raw bar and its
split-adjusted counterpart to the wire payload and reports LEAN's portfolio
without any methodology.

**Price views and raw accounting (ADR 0004).** LEAN trades, holds, prices
fills and charges commission in whatever view its subscription is in, so the
subscription is `DataNormalizationMode.Raw`: every LEAN order, fill, holding,
cash figure and commission is raw, as a broker's would be. The engine's
signals and sizing read the split-adjusted view, and ADR 0004's amendment
keeps a Campaign's money in the one view its fills are priced in, which is
split-adjusted until a split corporate action can adjust a held position. So
each figure crossing the boundary is in exactly one view:

| Figure | View |
| --- | --- |
| `market.bar.completed`'s `raw` | raw: LEAN's subscription bar |
| `market.bar.completed`'s `split_adjusted` | split-adjusted: LEAN's one-bar split-adjusted `History` for the same bar, never arithmetic in the adapter |
| a proposal's `entry_level` or `level`, and its `n` or `campaign_n` | split-adjusted: computed by the reducer from split-adjusted bars |
| a proposal's `quantity` | split-adjusted shares: sized from split-adjusted N and the split-adjusted previous close (ADRs 0003, 0020) |
| a `strategy.exit-order.set`'s `level` and `quantity` | split-adjusted: a Unit's quantity is its fill's |
| a LEAN order's quantity and stop price, and LEAN's holding and cash | raw |
| `execution.fill`'s `price`, `quantity`, `level` and `slippage_applied` | split-adjusted: LEAN's raw execution restated, so `quantity × price` is unchanged |
| `execution.fill`'s `commission`, and `account.snapshot` | cash: the same in both views; the commission is LEAN's charge on the raw shares it traded |
| `execution.order.lifecycle`'s `quantity` and `stop_price` | raw: the order as the venue states it, which reconciliation compares with the broker's book (ADR 0019) |

`internal/fills`, `cmd/backtest`'s reference, fills and prices everything in
the split-adjusted view, including its per-share commission.

The adapter converts with one figure, the **split ratio**: the whole number of
split-adjusted shares one raw share is, equal to the bar's raw close over its
split-adjusted close. A level or N is multiplied by it on the way into LEAN; a
fill's price, level and slippage are divided by it on the way out; share
counts go the other way. LEAN's factor files round the cumulative split factor
(AAPL's 1/56 is `0.0178571`), so the two closes are 56.000134 apart; a ratio
within 1e-4 of a whole number of at least 1 is taken as that number, and any
other stops the run, since no whole share of one view would then be a whole
number of shares of the other (so an instrument with a 3-for-2 or a reverse
split after the bar cannot be run yet). An entry or Add is **rounded down to whole raw shares**, never
up, so it never risks more than the Unit the engine sized (ADR 0003); one
smaller than a raw share is rejected. Its fill reports what executed, which the
reducer accepts as a Unit of at most the proposal's quantity: up to one raw
share short of what `cmd/backtest` would fill. An Exit Order whose quantity is
not whole raw shares stops the run. Every bar's ratio must equal the one in
force; a different one with no split reported is taken only while LEAN holds
nothing and works no order, and otherwise stops the run.

**Which splits are carried across.** Only an **n-for-1 split**, n a whole
number of at least 2 that divides the split ratio in force, is carried across
a run: every raw share becomes exactly n, and the ratio falls to ratio ÷ n,
still whole. Any other split reported during the run — a 3-for-2 (for
example from 3 split-adjusted shares a raw share to 2), a reverse split, or
an n that does not divide the ratio — stops the run in the split's own slice,
whether or not LEAN holds anything, and even where the quantities happen to
divide. The engine's split-adjusted view is adjusted for every split, later
ones included, so a supported split changes none of its levels, quantities or
N, only the split ratio. A split while flat only changes the ratio.

**A split while holding or with orders working** is carried across, and
checked before the next session can fill anything. LEAN, under Raw
normalisation, applies the split itself, in the split's own time step
(observed on the pinned image for AAPL's 2-for-1 of 2005-02-28):

1. it splits the holding (and pays any fractional share as cash) before the
   split's slice reaches `OnData`, whose tickets are still unadjusted;
2. after `OnData` returns, it divides each open order's quantity by the
   factor, multiplies its stop by it and rounds it to the cent, and reports
   each through `OnOrderEvent` as `UpdateSubmitted`, its ticket already
   changed;
3. it then fires the adapter's scheduled event at 00:01, before that
   session's fills at its close.

So the adapter checks in three places. In the split's slice (`OnData`), it
takes the new ratio and requires LEAN's split holding to be exactly the sum
of the engine's Units and every stored Exit Order to be still working. At the
00:01 scheduled check (`verify_split`), reading each ticket from LEAN's order
book, it requires the same, and every working order to be its split-adjusted
quantity at the new ratio, resting within one cent of its split-adjusted
level at it. At the next slice's start, it repeats that check as a second line.
Any failure stops the run: in the first two places before LEAN can fill
anything in the next session (a `Quit` at 00:01 was observed to prevent that
session's fills), and always before the next bar reaches the engine. No
corporate-action contract exists yet (ADR 0004's amendment) to carry any
other outcome. This rests on the
split-adjusted view being adjusted for splits after the run's end, which a
backtest's factor file provides and a live run cannot; live trading needs the
corporate-action contract first.

After each bar's decisions have been received, the adapter reads one
`account.snapshot` from `Portfolio.TotalPortfolioValue` (equity) and
`Portfolio.Cash` (available cash), in USD. Its `as_of`, `event_time` and
`recorded_at` equal that bar's own UTC `period_end`; snapshot times strictly
increase. The payload uses `event.AccountSnapshotSchemaVersion` (2), with
all four fields required by `event.AccountSnapshotPayload`. Invalid figures
or a failed bar or snapshot exchange stop the run. The figures are read at
the close, before that slice's decisions act, but the snapshot is **sent** at
the start of the next slice, after LEAN's reports of that slice's fills and
before its bar (`flush_snapshot`); the reason is under **Fills and order
changes**. The last snapshot is sent before the end of the stream, or before
a deliberate stop.

**Account type (ADR 0010).** The Baseline never borrows: "no partial Units,
no borrowing". LEAN's default equity account is margin, which lets a fill
cost more than the cash held — observed on a 2003–2014 AAPL run, where a gap
fill on 2011-07-20 drove cash negative and the run stopped, because
`account.snapshot` refuses a negative figure. So `Initialize` calls
`SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage,
AccountType.Cash)`, which must precede the explicit `SetSlippageModel` and
`SetFeeModel` calls: calling it after was observed to reset the security's
slippage model back to LEAN's own default (`NullSlippageModel`; see
**Observed LEAN behaviour** below), and the adapter's `InteractiveBrokersFeeModel`
and `NSlippageModel` must survive it. Two cases, both observed on the pinned
image with a deliberately undersized cash balance:

- **At submission,** an order the cash account cannot fund at its own level
  is refused outright: LEAN reports it `Invalid` with a message naming the
  required and free margin ("Insufficient buying power to complete orders").
  This is the same path as any other order LEAN refuses (`_propose`'s
  `ticket.Status == Invalid` branch): rejected and logged, not stopped — the
  engine's next proposal for the same instrument still reaches LEAN.
- **At fill, on a gap above the level,** the cash account does **not**
  refuse the fill and does **not** clamp the cost to what is held: an order
  affordable at its own level when submitted (2 shares at 490, cash 1000)
  filled anyway at a gapped-open price costing more than the account held (2
  x 547.95 = 1095.90), leaving `Portfolio.Cash` **negative** (-96.90). A cash
  account therefore narrows the failure (an order LEAN can already see is
  unaffordable at its own level is refused before it can ever fill) but does
  not remove it: a large enough gap still leaves cash negative, and the run
  must still stop, exactly as it does today, because `account.snapshot`
  refuses to report a negative figure. Since the owner's decision of
  2026-09-24 (ADR 0005 and ADR 0020, as amended), the engine reserves every
  entry's and Add's worst-case cost when it proposes it, at its **price cap**
  (level + 1N) with slippage and commission, and the Baseline's order is a
  **stop-limit** limited at that cap. A gap above the cap then does not fill
  at all, so a fill cannot cost more than was reserved; the account type is
  not what bounds it. Only the declared Variant `uncapped`, whose orders are
  stop-market, keeps the gap exposure described here.

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

**Orders.** `orders.py`'s `OrderDesk` turns the engine's decisions into LEAN
orders once the bar's exchanges (the bar and its Session close), or a fill's,
have been answered. It validates each decision against LEAN's current state
and places, amends or cancels the one order the decision names. The engine
decides every level, quantity and N; the adapter computes none of them and
never combines two levels.

| Decision | LEAN order |
| --- | --- |
| `strategy.trade.proposed` | buy **stop-limit**, **good-till-cancelled**, stop at `entry_level`, limit at `price_cap` rounded **down** to the tick, for `quantity`, each converted to raw (**Price views and raw accounting**); a **stop-market** order at `entry_level` when `order_type` is `stop-market` (the declared Variant `uncapped`) |
| `strategy.add.proposed` | the same, at `level` and `price_cap`, for `quantity`, each converted to raw |
| `strategy.proposal.expired` (kind `entry` or `add`) | cancel that proposal's order if it is still working |
| `strategy.campaign.opened` | none: its frozen `campaign_n` is kept for its Exit Orders' slippage, and its `fill_id` as Unit 1's opening fill |
| `strategy.campaign.unit-added` | none: its `fill_id` is kept as that Unit's opening fill |
| `strategy.exit.proposed` | none: its id is kept as the Campaign's outstanding exit proposal |
| `strategy.exit-order.set` | the Unit's one **good-till-cancelled** sell stop-market, at `level`, for the Unit's `quantity`, each converted to raw; a later level for the same Unit **amends** that order and never adds a second |
| `strategy.campaign.units-stopped`, `strategy.campaign.exited` | none: the closed Units' Exit Orders, and the Campaign's exit proposal, are forgotten |

- **Entries and Adds are good-till-cancelled, never DAY.** At daily
  resolution LEAN expires a DAY order before it evaluates the next session's
  fill: DAY buy stops whose next bar crossed their level expired unfilled
  (see **Observed LEAN behaviour**). The engine expires an unfilled proposal
  at the instrument's next bar (ADR 0011) and the adapter then cancels its
  order, so each still works for exactly one session.
- **The order tag is the decision id**: the trade or Add proposal's id, or, for
  an Exit Order, the id of the `strategy.exit-order.set` now in force (an
  amendment updates the tag with the level). A decision whose id is already
  on an order in LEAN's own order book, in any state, is not submitted again,
  so redelivering a proposal never opens a second position. A cancellation
  passes no tag, because LEAN's `Cancel(tag)` overwrites the order's own.
- **A proposal is valid only for the session after the bar that produced
  it** (ADR 0005's window; `EarliestFillAt`): its `period_end` must be the
  bar the decisions answer. A proposal in a fill's reply answers the session
  the fill executed in, so an Add the engine chains from a fill, measured
  against the bar that signalled the entry, is stale: that session has
  already traded in LEAN. Anything stale is rejected.
- **Every rejection is logged with its reason** as `adapter: REJECTED <type>
  <id>: <reason>`: an instrument that isn't this run's symbol or isn't
  tradable, a quantity that isn't a positive whole number or is under one raw
  share, a level that isn't a positive price, a direction other than long, a
  stale proposal, one already
  submitted, an entry or Add order LEAN itself refuses, and an Exit-Order
  level older than the one in force. The totals are logged at the end of the
  run. A rejected entry or Add is a proposal the engine re-issues on a later
  bar, not a protection gap.
- **An Exit Order amendment LEAN doesn't acknowledge leaves the previous level
  in force**, and is logged (ADR 0019's amendment).
- **A decision answering a warm-up bar is never acted on**, and **nothing is
  submitted when Go is unreachable**: a failed exchange stops the run through
  the existing fail-closed path before any of that bar's decisions are read.
  State the adapter can't reconcile also stops the run:
  - an unknown schema version of one of these decision types;
  - an Exit Order for a Campaign whose frozen N was never sent;
  - an Exit-Order level for a Unit whose LEAN order is no longer working or
    sells a different quantity — including a redelivered level whose tagged
    order is filled, cancelled or invalid, since the engine still sets it but
    no working order protects the Unit. The one exception is an order LEAN
    has already filled in the slice being reported, whose fill is still to be
    sent: that fill states what executed, so the amendment is skipped and
    logged (ADR 0005 rule 3: a bar that fills an Add and a stop enters, then
    stops);
  - an Exit Order LEAN refuses (status `Invalid`);
  - a cancellation LEAN refuses when a proposal expires, or one it has not
    confirmed by the start of the next slice, since the order could still
    fill into a holding the engine doesn't expect. LEAN answers a cancel with
    `CancelPending` and reports `Canceled` after the slice; the cancellation
    is logged as requested, then as done once LEAN confirms it;
  - a trade proposal arriving while LEAN already holds the instrument: the
    engine proposes an entry only when it holds no Campaign there.
- **Reconciliation before trading** (`docs/architecture.md`: reconcile before
  any executor submits). At startup, and again immediately before the run's
  first order, LEAN must hold no position and have no open order for the
  run's instrument; otherwise the run stops with a reason stating both. A
  backtest starts flat, so this passes there; it is checked anyway.
- **An Exit Order that would sell more than LEAN holds stops the run.** The
  working sell quantity never exceeds the holding; if a Unit's new Exit Order
  would take it past (including when LEAN holds nothing at all), LEAN's
  holding and the engine's Exit Orders disagree. Refusing the order and
  carrying on would leave that Unit silently without a stop, so the run stops
  through the fail-closed path, with a reason naming the instrument, the
  Campaign and Unit, the working sell quantity and the holding. Containment is
  then a person's decision (ADR 0019's amendment), not the system's.
- **An Exit Order the adapter can't place also stops the run:** another
  instrument, a quantity that isn't a positive whole number of raw shares, a
  level that isn't a positive price, or an unreadable `as_of`. Unlike an entry
  or Add, it isn't re-issued, and it would leave its Unit without a stop.
- **A split ratio the adapter can't trust stops the run**: one that isn't
  whole, one that changes with no split while LEAN holds or works an order,
  and a split that leaves LEAN's position or orders other than the engine's
  (see **Price views and raw accounting**).
- **LEAN's own API, as the pinned image has it.** `StopMarketOrder`'s fourth
  argument is the bool `asynchronous`, so the tag and order properties are
  the fifth and sixth; the order-ticket collections are enumerables with no
  length or indexing, so they are read with `list()`. `StopLimitOrder` is
  called as `(symbol, quantity, stop, limit, asynchronous, tag, properties)`,
  by analogy: **unconfirmed**, not yet observed on the pinned image.
- **A proposal's order must be placeable exactly as stated** (ADR 0005, as
  amended 2026-09-24): `order_type` `stop-limit` with a positive `price_cap`
  at or above its level, or `stop-market` with `price_cap` 0. Anything else is
  rejected and logged, like any other malformed proposal. The adapter computes
  no cap. It places the engine's, rounded **down** to LEAN's tick so the limit
  LEAN holds never exceeds the cap the engine's hold was computed from. After a
  split, a working stop-limit's limit is checked against its cap at the new
  ratio at the 00:01 pre-session check and again at the next slice's start.
  A limit LEAN rounded **down** is kept if it is within a tick of the cap. A
  limit it rounded **above** the cap is amended down to the cap floored to the
  tick (`UpdateOrderFields.LimitPrice`) before the session can fill; if LEAN
  does not acknowledge that, or the limit is still above the cap, the run
  stops. That LEAN splits a limit as it splits a stop, rounding either way, is
  also **unconfirmed**.

**Fills and order changes.** `OnOrderEvent` sends nothing: LEAN raises it in
the middle of placing an order (a submission is reported before
`StopMarketOrder` returns) and before `OnData` for a session's fills, so each
report is read once (`OrderDesk.observe`) and queued. `drain_order_events`
sends the queue at three points: at the start of each slice, before anything
else; after the slice's decisions have been acted on; and before the stream
ends or a deliberate stop. Acting on a fill's decisions places or amends
orders, and LEAN reports those too, so the queue is drained until it is
empty.

- **A fill becomes one `execution.fill`**, shaped exactly as the reducer
  expects for its order's kind, mirroring `internal/fills`:

  | LEAN order filled | `kind` | `proposal_id` | `campaign_id` | `unit_ids` |
  | --- | --- | --- | --- | --- |
  | an entry | `entry` | the trade proposal | empty | empty |
  | an Add | `add` | the Add proposal | the proposal's Campaign | empty |
  | an Exit Order the engine last set at its protective stop | `stop` | empty | the Unit's Campaign | the Unit's opening fill |
  | every Exit Order the engine last set at the Exit Channel, filled at one instant | `exit`, **one fill for them all** | the Campaign's exit proposal | the Campaign | empty |

  The kind of an Exit Order is the `source` of the `strategy.exit-order.set`
  in force; nothing compares a price with a level. An exit closes the whole
  remaining holding (`applyExitFill`), so every Unit resting at the Exit
  Channel must have filled at the same instant, level, price and slippage, or
  the run stops. `price` and `quantity` are LEAN's raw execution restated in
  the split-adjusted view (price ÷ the split ratio, quantity ×), `filled_at`
  is the event's `UtcTime`, `level` the order's raw stop price ÷ the ratio,
  `slippage_applied` what the adapter's slippage model charged that order ÷
  the ratio, and `commission` LEAN's `OrderFee` on the raw shares, unchanged
  (summed for an exit). Each is logged raw first, as `adapter: LEAN filled
  order <id> ...: <n> raw shares @ <price> raw, commission <fee> USD (split
  ratio <k>)`. `fill_id` is `lean:<order id>:<event id>`,
  joined with `+` for an exit. One instant's fills are sent in ADR 0005's
  order: the buy first, then stop fills worst price first, then the exit.
- **Every other change becomes one `execution.order.lifecycle`**
  (`internal/event/order_lifecycle.go`), in the order LEAN reported it:
  `Submitted` (LEAN's acknowledgement) as `submitted`, `UpdateSubmitted` as
  `updated`, `CancelPending` as `cancel-pending`, `Canceled` (by the adapter,
  or LEAN's own expiry) as `canceled`, and `Invalid` (LEAN refused it) as
  `invalid`, each with the order's id, tag, signed quantity and stop price as
  LEAN states them (raw), time and LEAN's message. The engine records them and decides nothing from them;
  they are journalled for reconciliation (ADR 0019). Any other status stops
  the run.
- **A partial fill stops the run.** The reducer accepts one fill per order: a
  second partial of an entry is refused, and a partial stop or exit cannot
  close its Units. Until successive partial fills accumulate into one Unit
  (#67), the adapter invents no position logic; a `PartiallyFilled` event, or
  a `Filled` one for less than the order, stops the run with the order, tag,
  quantities and price in the reason.
- **Where each input falls.** A slice is: reports LEAN made after the last
  slice (confirmed cancellations and amendments); this session's fills, and
  what acting on them placed; the previous Session's `account.snapshot`; this
  Session's bar and `market.session.closed`; then reports of what acting on
  those decisions placed or cancelled. So:
  - **a session's fills precede its bar.** A proposal stays outstanding until
    the instrument's next bar expires it (ADR 0011); a fill sent after that
    bar would name a proposal the engine no longer offers, and the engine
    refuses it (`TestAFillFromTheNextSessionPrecedesThatSessionsBar`);
  - **the previous Session's snapshot follows those fills.** The engine
    attributes an Add it chains from a fill to the bar that signalled the
    entry, and checks it against cash known at that bar's previous close
    (ADR 0021, section 7). A snapshot sent before the fill is later than that
    close, so the engine would find no eligible cash basis and stop; this
    happened on the first entry of the first acceptance attempt. The
    snapshot's figures are read at its own close, so they are unchanged by
    when it is sent, and it still precedes the bar whose sizing it is the
    basis for (ADR 0020).

**Costs (ADR 0013).** Every order's fill slips by `slippage_n` × the N the
engine supplied with it: a trade proposal's `n`, an Add proposal's
`campaign_n`, and the Campaign's frozen `campaign_n` (from
`strategy.campaign.opened`) for an Exit Order, looked up by the order's tag.
LEAN prices the fill in raw, so it slips by that N × the split ratio in force
when LEAN fills the order. An order with no supplied N raises rather than
slipping by zero.
`slippage_n` is the run's own required setting in `run.json`, never
defaulted; set it to the configuration's `slippage_n` (0.05 in the Baseline),
as `cash` must equal `notional_account.starting_equity`. Commission uses
LEAN's `InteractiveBrokersFeeModel`; IBKR Pro Fixed is the working assumption
(#81), and LEAN's model differs from it (see below).

**The startup fill-model report.** In a LEAN run LEAN's fills are the
evidence; `cmd/backtest` remains the reference implementation of ADR 0005, and
the two are compared rather than forced to agree. At startup the adapter logs
`adapter: fill model: ...` lines stating every respect in which LEAN's fills
depart from ADR 0005 and ADR 0013: the two price views and the rounding of a
Unit to raw shares, the cash account (ADR 0010) and what it does and does not
prevent, gap-at-open behaviour, the cent tick, exact touches, same-bar
ambiguity, amendments, intrabar ordering, entry timing, order lifetime,
LEAN's default equity slippage, the commission schedule, partial fills and
the price cap. Each statement about LEAN's own behaviour was observed on the
pinned image, as follows, **except the price cap's**, which is marked
unconfirmed in the report itself (see **The stop-limit and ADR 0005**, below).

**The stop-limit and ADR 0005** (unconfirmed: nothing below has been observed
on the pinned image). ADR 0005's amended fill model handles a triggered buy
stop-limit with stop `level` and limit `cap`, against a bar with reference
(open) `R` and low `L`, in three cases:

- if `max(level, R)` is at or below `cap`, it fills there;
- if `R` is above `cap` and the bar trades back down to it (`L ≤ cap`), it
  fills at `cap`;
- otherwise it does not fill.

Slippage is added in every case, so a fill never exceeds `cap` plus slippage,
which is exactly what the engine's hold reserved. LEAN's own stop-limit fill
model, read from its source rather than observed, differs:

- it triggers only when the bar's high **exceeds** the stop;
- it fills only if the bar's **close** is below the limit, at the lower of the
  bar's high and the limit;
- it charges no slippage on a limit fill, so `slippage_applied` would be
  reported as zero.

If that reading holds, LEAN skips some bars ADR 0005 fills (a touch, or a bar
closing above the cap), fills others at a different price, and never pays
more than the limit. Every one of these must be observed in a probe on the
pinned image, and the table below extended, before any paper-trading gate.

**Observed LEAN behaviour** (image
`quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d`;
a probe algorithm on AAPL, BAC and SPY daily bars for 2–11 January 2013, and
the acceptance run below):

| Question | Observed |
| --- | --- |
| Does a DAY order fill in the next session at daily resolution? | **No.** LEAN expires it first: DAY buy stops at 549.66 and 500.00 placed after the 2 January bar were `Canceled` ("The order has expired.") at the 3 January close, though that bar's high was 549.66 and its open 547.95. The same orders good-till-cancelled filled. |
| Gap at the open | Matches ADR 0005: a buy stop at 500.00 filled at the 547.95 open plus slippage; a sell stop at 540.00 filled at the 537.15 open less slippage, each with LEAN's "unfavorable gap" message. In the split-adjusted acceptance run all 119 fills, 55 of them gaps, were priced at max/min(level, open) ± slippage to within 2e-15; in the raw one all 68, 33 of them gaps, in raw prices to within 1.5e-14. |
| An exact touch | Fills: a buy stop at 549.66, that bar's exact high, filled at 549.76; a sell stop at 525.83, that bar's exact low, filled at 525.73 (0.10 slippage). The acceptance run had no exact touch. |
| Is a cancel synchronous? | **No.** `Cancel()` returns success with the order `CancelPending`; `Canceled` is reported after `OnData` returns, before the next slice. In the acceptance run all 15 cancellations were confirmed that way. |
| Does `SetBrokerageModel` reset a security's own models? | **Yes, if called after them.** A security whose slippage model was set, then had `SetBrokerageModel(InteractiveBrokersBrokerage, AccountType.Cash)` called, had its slippage model reset to `NullSlippageModel`; calling `SetBrokerageModel` first and the explicit `SetSlippageModel`/`SetFeeModel` after left both as set. |
| Cash account at submission | An order the account cannot fund at its own level is refused outright: 1,000 AAPL shares at a stop of 274.52 with $1,000 cash was reported `Invalid` ("Insufficient buying power to complete orders (Value:[274520]) ... Initial Margin: 274520, Free Margin: 1000"), the same path as any other order LEAN refuses. |
| Cash account at a gap fill | **LEAN fills it anyway and leaves cash negative; it does not refuse the fill or clamp its cost.** 2 AAPL shares at a stop of 490 (affordable at submission: 2 × 490 = 980 ≤ $1,000) filled at the next session's gapped-open price of 547.95 (2 × 547.95 = 1,095.90, more than the account held); `Portfolio.Cash` went to **-96.90** after the fill and commission. |
| LEAN's default equity slippage | Zero (`NullSlippageModel`): a gapped SPY buy with no slippage model filled exactly at the 145.99 open. |
| IB fee tier | $0.005 per share, $1.00 minimum (500 shares: $2.50; 50 shares: $1.00), capped at 0.5% of the order's value at LEAN's market price, not Pro Fixed's 1%; the minimum wins over the cap (1 BAC share at $12.01: $1.00). It is charged on the shares LEAN trades: the cap bound on 42 of the split-adjusted acceptance run's 119 fills, whose share counts were up to 56 times raw, and on none of the raw run's 68, every one of which was charged exactly $0.005 × its raw shares (10,998 shares: $54.99). |
| Amendments | An amended order is evaluated against the bar it was amended after: a sell stop raised to 530.00 after a bar whose low was 525.83 filled at 529.90 in that same slice. A new order never fills against the bar it was placed after. |
| When are fills reported? | Before `OnData` for the session they executed in, stamped at its close (`UtcTime` = the bar's end), with the holding already moved. |
| Early closes | LEAN's one-bar `History` returned two rows on 2002-12-24 (13:00 close); `split_adjusted_view` takes the row ending with the bar. |
| Both views from one run | With a Raw subscription, a one-bar `History(..., dataNormalizationMode=SplitAdjusted)` returns the split-adjusted bar: AAPL on 2005-02-22 closed at 85.38 raw and 1.524639198 split-adjusted, 56.000134 apart, the factor file's rounded 1/56. |
| Tick | LEAN rounds every raw stop price to the cent, including a split's adjustment of an open stop, logging "To meet brokerage precision requirements, order StopPrice was rounded to 15.30 from 15.29996328" for the first only: all 193 order changes in the raw run are at whole cents. |
| A split under Raw normalisation | For AAPL's 2-for-1 of 2005-02-28 (factor 0.4999986): `SplitType.Warning` in a bar-less slice on the 25th, then `SplitOccurred` in a bar-less slice at midnight on the 28th, when the holding has already doubled (1,000 → 2,000, the average price halved, and a fractional share paid as cash: $0.25 on 1,000 shares). Each open stop's quantity doubles and its price halves, rounded to the cent (62.23 → 31.11 for a sell, 115.57 → 57.78 for a buy), but only after that slice's `OnData` returns, whose tickets are still unadjusted; each is then reported through `OnOrderEvent` as `UpdateSubmitted` with its ticket already changed, still at midnight. A scheduled event at 00:01 then fires with every ticket adjusted, before the session's fills at 16:00, and a `Quit` there prevents those fills (a sell stop that filled that session did not). `OnEndOfTimeStep` is never called for a Python algorithm. In the raw run 4 Units (11,056 raw shares) and their 4 Exit Orders were carried across it: 22,112 shares, each stop at half its level, and $2.75 of fractional-share cash. |

**The acceptance run.** A one-instrument backtest on the pinned image, with
the engine in its own container on a shared named volume (ADR 0014). It uses
only the LEAN data already on the machine and pulls nothing. From a LEAN
workspace whose `data/` holds US equity daily data, with a project directory
holding `algorithm.py` as `main.py`, `client.py`, `orders.py`,
`publisher.py` and a `run.json` naming `/run/adapter/private/s.sock`:

```sh
# Build for the Docker server's own architecture (arm64 or amd64), so the LEAN
# image runs the engine natively.
GOOS=linux GOARCH="$(docker version --format '{{.Server.Arch}}')" CGO_ENABLED=0 go build -o <stage>/engine ./cmd/engine
docker volume create trend30sock
docker run -d --name trend30-engine -v trend30sock:/run/adapter -v <stage>:/stage \
  --entrypoint /stage/engine quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d \
  -socket /run/adapter/private/s.sock -config /stage/configuration.json -out /stage/journal.jsonl \
  -as-of 2003-01-01T00:00:00Z
lean backtest <project> --image quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d \
  --no-update --extra-docker-config '{"volumes": {"trend30sock": {"bind": "/run/adapter", "mode": "rw"}}}'
docker stop trend30-engine   # the engine writes the journal on SIGTERM
go run ./cmd/backtest -verify <stage>/journal.jsonl
go run ./cmd/backtest -replay <stage>/journal.jsonl
```

The required `-as-of` is the run's declared start in RFC 3339: this example
uses `run.json`'s `"start": "2003-01-01"` as midnight UTC. Change both
together. Missing, invalid or zero times refuse engine startup before the
socket opens. The configuration input's `event_time` and `recorded_at` both
use this value, keeping identical runs' journals comparable (ADRs 0012, 0017).

Configuration time need not precede the first bar's period end: the reducer
does not read it, and bar/session chronology is checked separately. LEAN's
warm-up bars before `start` are accepted. The journal span remains the earliest
and latest input event times (ADR 0017), so `span_start` equals `-as-of` when
no input predates it, or the earliest warm-up input when one does.

`run.json`'s `configuration_hash` and `strategy_version` must be the
engine's own for that configuration and build. The journal and the market
data are never committed. The first acceptance run, under the earlier
split-adjusted subscription, is recorded on its pull request: AAPL, 2003-01-01
to 2010-12-31 with 60 warm-up bars, the Baseline's 55/20 channels; 119 fills
(26 entries, 48 Adds, 32 stops, 13 exits) and 325 order changes; 9,520
records, a complete run, a verified chain and a byte-identical replay.

The raw acceptance run: AAPL, 2003-01-01 to 2006-12-31 with 60 warm-up bars,
55/20 channels, a 0.5% Unit and $1,000,000, across the 2-for-1 of 2005-02-28
with four Units held. 1,067 bars; 68 fills (13 entries, 29 Adds, 18 stops, 8
exits) and 193 order changes; 4,931 records, a complete run, a verified chain
and a byte-identical replay. Every fill's raw quantity × the split ratio is
the journal's quantity, and its raw price ÷ the ratio the journal's price, so
the two differ in money by at most 6e-11; every raw price is ADR 0005's
max/min(level, open) ± slippage against the raw bar; every commission is
$0.005 × raw shares with the $1.00 minimum, $2,453.56 in all, where the same
orders in split-adjusted shares would have been charged $116,726.68 before
the cap. Each Unit is up to one raw share short of the engine's quantity (a
2003 proposal of 615,898 split-adjusted shares filled as 10,998 raw, 615,888).
The ten rejected decisions are all Adds chained from a fill, which are stale
in LEAN.

The adapter will still need to:

- send `account.cash-movement` events into the same input sequence (outside #158);
- normalize universe changes, corporate actions, connection changes, and brokerage events into versioned messages;
- accumulate partial fills into one Unit (#67), rather than stopping on one;
- recover which LEAN order is each Unit's Exit Order after a restart (#207); and
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
decisions and a fake of LEAN's order book that behaves as the pinned image was
observed to (the order-event timing, `CancelPending`, the enumerables and
`StopMarketOrder`'s signature), and assert on the orders that reach it and the
inputs that reach the engine. Their decision fixtures are checked against the
Go payload types' fields and schema versions by
`tests/testdata/order_decisions_contract.go`, and every fill and lifecycle
input the adapter builds is checked against Go's own validators by
`tests/testdata/execution_contract.go`.

`tests/test_end_to_end.py` runs the algorithm against the real `cmd/engine`
over a real socket, with the fake order book filling orders under ADR 0005's
rules against a synthetic (non-market) series: an entry, Adds including one
the engine chains from a fill, and an Exit-Channel exit of every Unit as one
fill. It then runs `cmd/backtest -verify` and `-replay` on the engine's
journal and requires a complete, verified run that replays byte-identically.
It needs the Go toolchain and nothing else: no LEAN, docker or network.

## Coverage: issue #31's acceptance criteria

Issue #31 ("Adapter contract fixtures and fault cases") asks for a pinned
fixture at every event type's current schema version, and a test proving
every way the boundary can fail does so without trading. This table maps each
criterion to the test(s) that satisfy it; most already existed, and are
listed here rather than duplicated.

| Criterion | Test(s) |
| --- | --- |
| A fixture exists for every input and decision event type at the current schema version | `tests/testdata/*_contract.go`, one per input shape (`bar_contract.go`, `session_contract.go`, `snapshot_contract.go`, `execution_contract.go` for fills and lifecycle reports, `run_stopped_contract.go`, `run_completed_contract.go`) and `order_decisions_contract.go` for every decision type, acted on (`TestFixtureFieldsAndSchemaVersionsMatchTheGoPayloads`) or not (`test_orders.py`'s `IgnoredDecisionTests`, `FixtureContractTests`). `tests/testdata/wire_coverage.go` and `test_wire_coverage.py`'s `test_every_go_event_type_is_covered_or_explicitly_excused` parse `internal/event`'s own source for every `EventType`/`SchemaVersion` pair and fail if one has no adapter fixture and no documented reason it doesn't cross the boundary yet (`strategy.configuration`, `account.cash-movement`, `market.corporate-action`) |
| An unsupported schema version fails closed with a clear error | `test_orders.py`'s `test_an_unknown_schema_version_stops_the_run` (a decision type the adapter acts on); `test_algorithm.py`'s `test_bad_snapshot_reply_stops_run` and `test_bad_bar_reply_sends_no_snapshot`/`test_bad_session_close_reply_sends_no_snapshot` (the wire reply's own schema/type/sequence/causation, including `schema_version`, via `publisher.py`'s `_publish`); `test_client.py`'s `test_undecodable_json_reply_fails_closed_without_reuse` (a reply that is not even parseable JSON, wrapped as a clear `Unavailable` by `client.py`, not a bare `json.JSONDecodeError`) |
| Go unreachable: no orders submitted | `test_client.py`'s `test_connect_fails_closed_when_absent`; `test_algorithm.py`'s `test_absent_engine_quits_at_startup`; `test_orders.py`'s `test_go_unreachable_means_nothing_is_submitted` |
| Go unreachable: safe mode entered, alert-worthy event emitted | **Not implemented, and this is stated rather than assumed.** ADR 0019's Degraded/Halted states and its Alerting section are design-only ("Proposed", "This is a design, not code yet") and gated on #151 (engine journal durability); no alerting channel exists. The adapter's actual, tested contract for "Go unreachable" is the strongest available response given that: the run stops (`self.stop`/`Quit`), submitting nothing further, which is `docs/architecture.md`'s safety invariant ("If the Go decision engine is unavailable, the adapter submits no new orders") — not a degraded-but-continuing mode. There is deliberately no fabricated "alert" event standing in for the real one ADR 0019 describes |
| Go crash mid-run: no duplicate orders on restart | `test_orders.py`'s `test_a_redelivered_proposal_creates_no_second_order` and `test_duplicate_detection_reads_leans_order_book_not_adapter_memory` (a decision id already on an order in LEAN's book — filled, so not even working — is never resubmitted by an adapter instance whose own memory holds nothing, which is exactly the property a restart needs: LEAN's order book, not this process's memory, is the record); `test_a_redelivered_exit_order_set_changes_nothing` (idempotent Exit Order amendments too) |
| Go crash mid-run: reconciliation performed | `test_orders.py`'s `StartupReconciliationTests` (`require_flat`): every run reconciles before its first order, today to "LEAN holds nothing and works no order" — the only state a backtest-only adapter can start from. Full broker-vs-engine reconciliation (ADR 0019: positions, cash, open orders, explained differences) is design-only and needs a live broker integration (#81), a durable engine journal (#151) and recovering which LEAN order is each Unit's Exit Order after a real process restart (#207); none of that exists to test yet |
| Duplicate, out-of-order, and corrupted messages are each rejected without trading | Duplicate bar: `test_publisher.py`'s `test_fail_closed_on_duplicate_bar_or_wrong_engine_identity`. Duplicate decision: see "no duplicate orders" above. Out-of-order (wrong causation/sequence): `test_client.py`'s `test_decide_causation_mismatch_raises_out_of_order`; `test_publisher.py`'s `test_reply_with_another_runs_correlation_id_is_rejected`; `test_algorithm.py`'s field-by-field reply tests. Corrupted (bad payload hash): `test_client.py`'s `test_payload_that_does_not_match_its_hash_is_refused`. Corrupted (undecodable JSON): `test_client.py`'s `test_undecodable_json_reply_fails_closed_without_reuse`. Every case above also proves the abandoned connection is never reused (`test_decide_broken_connection_abandoned`) |
| Protective orders survive an adapter fault | `test_orders.py`'s `ProtectiveOrderSurvivalTests`: a working Exit Order's LEAN ticket is untouched (no `Cancel`, no `Update`) after an unrelated fault stops the run, both for an unsupported schema version and for Go becoming unreachable. `OrderDesk` in fact has no code path that ever cancels an Exit Order at all — `_expire` only ever cancels an entry or Add's own buy order — so this is the existing contract, now pinned rather than merely implicit |
| `make check` green | Enforced by CI/the working agreement; verified locally for this change |

Timeout (`docs/architecture.md`'s failure modes; ADR 0014's measured failure
table) is covered by `Client`'s own `socket.settimeout` plus
`test_client.py`'s general "no reply within the deadline" path exercised
through the same abandoned-connection tests above: a timed-out exchange and a
closed connection are both `Unavailable`, handled identically by
`algorithm.py`'s fail-closed `except` blocks, and neither is retried on the
same connection.
