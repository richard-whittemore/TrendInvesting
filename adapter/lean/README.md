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
AccountType.Cash)`, which must precede the explicit `SetSlippageModel`,
`SetFillModel` and `SetFeeModel` calls: calling it after was observed to
reset the security's slippage model back to LEAN's own default
(`NullSlippageModel`; see **Observed LEAN behaviour** below), and the
adapter's `InteractiveBrokersFeeModel`, `NSlippageModel` and ADR 0005 fill
model must survive it. Two cases, both observed on the pinned
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
  **stop-limit** limited at that cap, filled by the adapter's ADR 0005 fill
  model (**The stop-limit and ADR 0005**, below). A gap above the cap then
  fills at the cap only if the bar trades back down to it, and otherwise not
  at all, so no fill exceeds the cap plus slippage; the account type is not
  what bounds it. That no fill costs more
  than its hold is **exact in `cmd/backtest`**, whose commission is ADR 0013's
  schedule. In a LEAN run it holds only **up to the difference between LEAN's
  fee model and that schedule**: `InteractiveBrokersFeeModel` charges a $1.00
  minimum per order, and the hold reserved ADR 0013's charge. For example, a
  one-share order at $5 reserves about $0.05 of commission and LEAN charges
  $1.00 (see **Observed LEAN behaviour**, IB fee tier; tracked in #81). Only
  the declared Variant `uncapped`, whose orders are stop-market, keeps the
  gap exposure described here.

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
  at the instrument's next bar for entries and ordinary Adds, or one bar
  later for fill-chained Adds (ADR 0011, amended 2026-09-24; still at the
  next bar if that bar proposes an exit), and the adapter then cancels its
  order. A stop fill that closes the Campaign, in part or in full, expires a
  pending Add in its own reply, so the adapter cancels that order before the
  next Session can fill it.
- **The order tag is the decision id**: the trade or Add proposal's id, or, for
  an Exit Order, the id of the `strategy.exit-order.set` now in force (an
  amendment updates the tag with the level). A decision whose id is already
  on an order in LEAN's own order book, in any state, is not submitted again,
  so redelivering a proposal never opens a second position. A cancellation
  passes no tag, because LEAN's `Cancel(tag)` overwrites the order's own.
- **Entries and ordinary Adds retain their one-bar window.** An entry's
  `period_end` must be the bar the decisions answer. Add schema 3 carries
  `valid_for_sessions`: 1 for an ordinary Add, 2 for a fill-chained Add
  (ADR 0011 and ADR 0021 §7, amended 2026-09-24). A chained Add can therefore
  be placed from a fill's reply in the first following session and fill in
  LEAN's next slice. The adapter remembers the last two actual instrument
  bar ends to validate the window; fill timestamps and deferred snapshots
  never advance that history, and weekends or holidays are not calendar-day
  increments. A missing, malformed or wider window is rejected. A proposal
  older than its declared window is still stale and rejected.
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
    is logged as requested, then as done once LEAN confirms it. A
    cancellation a fill's reply requests at the start of a slice (a stop
    fill that closes the Campaign expires its pending Add) is therefore
    checked at the start of the following slice, not before this slice's
    bar. That check runs before any of the following slice's fills is sent,
    counting a `Canceled` report LEAN queued for it as confirmation;
  - a fill, or partial fill, of an order whose cancellation the adapter has
    requested, confirmed or not: the engine has already expired its
    proposal, so the fill is never sent and the run stops (ADR 0019);
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
  called as `(symbol, quantity, stop, limit, asynchronous, tag, properties)`:
  **confirmed** by a probe on the pinned image, whose bound method's own
  `__doc__` states exactly this order (see **Observed LEAN behaviour**).
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
  stops. Kept or amended, the limit must also stay at or above the working
  stop. When the split rounds the stop above the cap floored to the tick (it
  can when `gap_buffer_n` is 0), no limit can satisfy both, and the run stops
  rather than rest an order that a touch of its stop could not fill. The stop
  is never moved, because it is the engine's level. That LEAN splits a limit
  the same way it splits a stop is **confirmed** by a probe on the pinned
  image (see **Observed LEAN behaviour**): both are multiplied by the split
  factor and rounded to the cent in the one report that also adjusts the
  quantity. The probe's own factor rounded up both times; the exact tie-break
  for a value landing precisely on a half-cent was not observed, so the code
  above still defends both directions relative to the engine's cap.

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
- **A fill above a stop-limit's own cap plus its slippage stops the run.**
  ADR 0005 and ADR 0020 (as amended 2026-09-24) both depend on a capped
  order never executing above its limit before slippage, so never costing
  more per share than the limit plus slippage, which is what its hold
  reserved. If LEAN ever reports a fill of an entry's or Add's stop-limit
  order priced above that order's own `LimitPrice`, raw, plus the raw
  slippage the adapter's model charged on it, the adapter's fill model
  (**The stop-limit and ADR 0005**, below) was not the one that priced it.
  Rather than trust such a fill, the adapter raises `Uncertain` naming the
  order, the fill price, the limit and the slippage, and the fill is never
  sent to the engine.
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
slipping by zero. LEAN charges it on a stop-market fill itself; a
stop-limit's fill is priced by the adapter's ADR 0005 fill model, which
charges the same `NSlippageModel` explicitly, because LEAN's native
stop-limit fill applies no slippage at all (observed).
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
the price cap, which is now priced by the adapter's ADR 0005 fill model in
place of LEAN's native stop-limit fill. Every statement about LEAN's own
behaviour, and about the adapter's fill model running inside it, was
observed on the pinned image, as follows.

**The stop-limit and ADR 0005: the adapter's ADR 0005 fill model.** ADR
0005's amended fill model handles a triggered buy stop-limit with stop
`level` and limit `cap`, against a bar with reference (open) `R` and low
`L`, in three cases:

- if `max(level, R)` is at or below `cap`, it fills there;
- if `R` is above `cap` and the bar trades back down to it (`L ≤ cap`), it
  fills at `cap`;
- otherwise it does not fill.

It triggers when the bar reaches the level (`high ≥ level`, an exact touch
included), and slippage is added in every case, so a fill never exceeds
`cap` plus slippage, which is exactly what the engine's hold reserved.

LEAN's own stop-limit fill is a different mechanism (below, kept as
history), so the owner's decision of 2026-09-25 replaced it for the
adapter's capped buys: `algorithm.py` sets **the adapter's ADR 0005 fill
model** (`orders.adr_0005_fill_model`) on the security, after the brokerage
model. It is a Python subclass of LEAN's `EquityFillModel`, the model LEAN
fills equities with on the pinned image (before and after
`SetBrokerageModel`), and overrides one method, `StopLimitFill(asset,
order)`, which LEAN calls through its `FillModelPythonWrapper` and which
returns an `OrderEvent`:

- **for a buy**, it reads the bar LEAN is evaluating from the inherited
  `GetPricesCheckingPythonWrapper(asset, order.Direction)` (the daily
  `TradeBar`'s open, high and low), the order's `StopPrice` and
  `LimitPrice`, and its N from `NSlippageModel` (which records the charge),
  and prices it with `stop_limit_buy_fill_price`, the Python twin of
  `internal/fills.ExecuteStopLimit`. A fill is the whole order,
  `OrderStatus.Filled` at that price; otherwise the event is returned
  unfilled. As LEAN's own fills do, it fills nothing for a cancelled order,
  while the exchange is closed (`IsExchangeOpen`), or against a bar that
  ended at or before the order was placed.
- **each bar decides afresh.** The order fills in the bar that triggers it
  or not at all in that bar; nothing is carried to a later bar. Expiry stays
  the adapter's: it cancels the order when the engine expires the proposal
  (ADR 0011), after one session, or two for a fill-chained Add, and that
  second session is priced from its own open exactly as `internal/fills`
  prices it.
- **every other order keeps `EquityFillModel`'s own fill**: a sell
  stop-limit (the adapter places none) through the base method, and every
  stop-market order (the Variant `uncapped`'s entries and Adds, and every
  Exit Order) because nothing else is overridden. Those were observed to
  match ADR 0005 already (the gap and touch rows below). Subclassing the
  generic `FillModel` instead would not: it filled stop-market orders at
  the bar's **close** plus or minus slippage and ignored exact touches
  (observed).
- **it fails closed.** LEAN catches an exception raised in a fill model,
  logs it as an order error ("Transaction model failed to fill") and leaves
  the order unfilled, so a raised error would not stop the run. The model
  instead records the first thing it cannot price (a non-positive or
  non-finite price, a cap below the stop, a bar whose high is below its low,
  an order with no N) in `failure` and leaves the order unfilled, and
  `algorithm.py` stops the run on it before any queued report is sent: at
  the start of `OnData`, before each report at every drain (LEAN rescans
  every working order after one is placed or amended, so a failure can be
  recorded mid-slice), and at the end of the run.

**Two implementations, kept in step.** `internal/fills.ExecuteStopLimit`
and the adapter's `stop_limit_buy_fill_price` are two implementations of
one rule, so both answer one shared table,
`tests/testdata/stop_limit_fill_cases.json` (bar OHLC, level, cap, N,
`slippage_n`, and the fill or no-fill with its price), to within 1e-9:
`TestExecuteStopLimitAnswersTheSharedCases` in `internal/fills` and
`SharedCaseTests` here. Changing the rule in one place and not the other
fails one of the two suites. The table's bars are **synthetic**, invented
round numbers: market data is never committed. On the pinned image, a probe
run locally on real AAPL daily bars placed every case the table covers as a
real `StopLimitOrder` with the adapter's models: each LEAN fill matched
`internal/fills`' price to 1.1e-13, or did not fill where it does not
(**Observed LEAN behaviour**).

**What still differs from `cmd/backtest`.** The tick: LEAN's stop is the
level rounded to the cent and its limit the cap rounded down to the cent,
so a LEAN fill can differ from `cmd/backtest`'s by that rounding. Entry
timing (the next session). And total cost: LEAN's
`InteractiveBrokersFeeModel` charges a $1.00 minimum per order where the
hold reserved ADR 0013's schedule (**Costs**, above, and the fee-model gap
#81 tracks), so a fill never costs more than the hold reserved (ADR 0020)
up to that same fee-model difference, not exactly. As a safety net against
a fill this model did not price, the adapter refuses rather than trusts a
fill LEAN reports above its own `LimitPrice`, raw, plus the slippage
charged on it (**Fills and order changes**, above).

**LEAN's native stop-limit fill (history, observed before the adapter's
model replaced it).** Now observed rather than read from source, and no
longer used for any order the adapter places:

- **the trigger is `high > level`, strictly.** An exact touch (`high == level`)
  does **not** trigger, unlike a plain stop-market order's (which does; see
  the exact-touch row below). Bracketed a cent either side of a bar's exact
  high on the pinned image.
- **once triggered, the order stays triggered**, and fills on the first bar
  from the trigger bar onward whose low is at or below the limit: at that
  bar's open when the bar is **not** the one that triggered it and the open
  is at or below the limit (a favorable gap, LEAN logs it as such), otherwise
  at `min(high, limit)`. So **on the triggering bar itself, gap or no gap,
  the fill is `min(high, limit)`** whenever that bar's low reaches the limit:
  a plain inside-the-bar trigger with no gap at all — the case ADR 0005 fills
  at `level` — instead fills at the bar's **high**, bounded by the limit, a
  worse (more expensive) price for the buyer than ADR 0005's, though never
  above the limit. A gap that trades back to the limit fills at the limit,
  matching ADR 0005's rule 3. A bar whose low never reaches the limit leaves
  the order resting, unfilled and still armed for the next bar, where it
  could fill at a favorable-gap open below its own stop, a price ADR 0005
  never produces.
- **no slippage is applied to a stop-limit fill of either kind**:
  `slippage_applied` was always zero, where ADR 0013 requires it on every
  fill.
- **`k = 0`** (limit equal to stop after tick flooring) matched ADR 0005's
  pre-slippage price only for a fill on the triggering bar itself, and never
  on an exact touch, which did not trigger.
- **a split adjusts the limit exactly as it adjusts the stop**: both are
  multiplied by the split factor and rounded to the cent, reported in the one
  `UpdateSubmitted` event that also halves the quantity. This is LEAN's
  order handling, not its fill model, and still applies.

In the 2003–2014 acceptance run under the adapter's model (below), 84 of
the 92 capped buy fills were priced differently from what LEAN's native
`min(high, limit)` would have charged.

**Observed LEAN behaviour** (image
`quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d`;
a probe algorithm on AAPL, BAC and SPY daily bars for 2–11 January 2013, a
`StopLimitOrder` probe on AAPL for the same window and across the 2005-02-28
split, a probe of the adapter's ADR 0005 fill model on real AAPL daily bars
run locally, and the acceptance runs below). The rows on a stop-limit's
trigger, its fill inside the bar, its gaps, `k = 0` and its slippage record
LEAN's native stop-limit fill, which the adapter's model has replaced for
every order the adapter places; they are kept as history:

| Question | Observed |
| --- | --- |
| Does a DAY order fill in the next session at daily resolution? | **No.** LEAN expires it first: DAY buy stops at 549.66 and 500.00 placed after the 2 January bar were `Canceled` ("The order has expired.") at the 3 January close, though that bar's high was 549.66 and its open 547.95. The same orders good-till-cancelled filled. |
| Gap at the open | Matches ADR 0005: a buy stop at 500.00 filled at the 547.95 open plus slippage; a sell stop at 540.00 filled at the 537.15 open less slippage, each with LEAN's "unfavorable gap" message. In the split-adjusted acceptance run all 119 fills, 55 of them gaps, were priced at max/min(level, open) ± slippage to within 2e-15; in the raw one all 68, 33 of them gaps, in raw prices to within 1.5e-14. |
| An exact touch (plain stop-market/stop order) | Fills: a buy stop at 549.66, that bar's exact high, filled at 549.76; a sell stop at 525.83, that bar's exact low, filled at 525.73 (0.10 slippage). The acceptance run had no exact touch. |
| `StopLimitOrder`'s signature | `(symbol, quantity, stop_price, limit_price, asynchronous, tag, order_properties)`, read directly from the bound method's own `__doc__` via pythonnet (three overloads, for `Decimal`/`Double`/`Int32` quantity, all in this order) and confirmed by placing orders with it: the resulting ticket's tag, quantity, stop and limit were exactly as passed. |
| A stop-limit's trigger, exact touch | **Strict: `high > stop`, not `≥`.** A stop-limit at 549.65 (Jan 3's exact high 549.66, less a cent) triggered and filled; the same order at 549.66 (the exact high) and at 549.67 (a cent above) did not trigger at all, all week. This differs from a plain stop order's exact-touch fill (row above). |
| A stop-limit trigger inside the bar, no gap | **Fills at the bar's high, bounded by the limit — not at the stop's own level.** Open 547.95 < stop 548.50 < high 549.66, limit 560.00 (well clear): filled at 549.66 (the high), not 548.50 (ADR 0005's `level`). |
| A stop-limit gap within the limit | **Also fills at the bar's high, bounded by the limit — not at the open.** Stop 500.00, limit 560.00, against Jan 3 (open 547.95, high 549.66): filled at 549.66 (`min(high, limit)`), not 547.95 (ADR 0005's `max(level, open)`). |
| A stop-limit gap above the limit, with a trade-back | Matches ADR 0005: stop 530.00, limit 533.00, against Jan 4 (open 537.15 > limit, high 538.59, low 525.83 ≤ limit): filled **on that same (triggering) bar** at 533.00, the limit. |
| A stop-limit gap above the limit, no trade-back that bar | **Does not fill that bar, but stays armed rather than expiring** (ADR 0005 would expire the proposal): stop 500.00, limit 520.00, against Jan 3 (open 547.95, high 549.66, low 541.00 > limit): no fill on Jan 3. The same order, still resting (triggered once, permanently), filled 4 sessions later (Jan 7: low 515.20 ≤ 520 ≤ high 529.30, open 522.05 above the limit so no favorable gap) at exactly 520.00 — even though that day's **close**, 524.16, ended up above the limit, confirming the fill needs the bar's low at or below the limit and never the close. A companion order (stop 500.00, limit 525.00) armed on Jan 4, whose low (525.83) missed its limit by 0.83, similarly stayed resting and filled 3 sessions later, on Jan 7, at that day's **open** (522.05, at or below the 525.00 limit): a "favorable gap" fill, logged as such — the same favorable-gap rule an ordinary resting limit order uses, available only once already triggered (row above: a gap on the triggering bar itself does not take this branch). |
| `k = 0` (limit equal to stop) | LEAN accepts the order and fills it at exactly that shared price when triggered, in both a gap (armed at 530.00/530.00 against a bar opening at 537.15: filled at 530.00) and a non-gap trigger (armed at 548.50/548.50 against Jan 3: filled at 548.50). |
| Slippage on a stop-limit fill | **Zero, always.** A marker slippage model returning a fixed, distinguishable 0.11 on every order was attached to the security; none of 7 stop-limit fills across both probes showed that offset — every fill matched the unslipped `min(high, limit)`, cap, or open exactly. |
| Which fill model LEAN gives an equity | `EquityFillModel`, both at `AddEquity` and after `SetBrokerageModel(InteractiveBrokersBrokerage, AccountType.Cash)`. |
| Overriding a stop-limit's fill from Python | A Python subclass of `EquityFillModel` set with `SetFillModel` is wrapped in LEAN's `FillModelPythonWrapper`, which calls the subclass's `StopLimitFill(asset, order)` (`Security`, `StopLimitOrder`) and takes the `OrderEvent` it returns; `super().StopLimitFill` reaches LEAN's own. The protected helpers `IsExchangeOpen(asset, False)` and `GetPricesCheckingPythonWrapper(asset, order.Direction)` (a `Prices` with the daily bar's `Open`, `High`, `Low`, `Close`, `EndTime`) are callable from the subclass. .NET `DateTime`s (`order.Time`, `Prices.EndTime` after `Extensions.ConvertToUtc`) arrive as Python `datetime`s, with no `Ticks`. `StopLimitOrder.StopTriggered` has an internal setter, so the subclass leaves it alone. A Python float assigned to `OrderEvent.FillPrice` is accepted, and LEAN's fee model still charges the fill ($1.00 on each 1-share fill). |
| The adapter's ADR 0005 fill model against the shared cases | **Matches `internal/fills` in every case**, on real AAPL daily bars run locally (the bars are not committed): a 1-share `StopLimitOrder` for each case the shared table covers (a trigger inside the bar, an exact touch, a gap within the cap, an open exactly at the cap, a gap above the cap with a trade-back, one whose low is exactly the cap, one that closes above the cap, one with no trade-back, a level never reached, the `k = 0` variants and a second slippage figure), placed after the bar before its session and cancelled after it, with the adapter's own models (IB cash account, `NSlippageModel`, the fill model, `InteractiveBrokersFeeModel`). Every LEAN fill price equalled `internal/fills.ExecuteStopLimit`'s for the same bar to 1.1e-13, and every case `internal/fills` does not fill did not fill. |
| Stop-market orders and sell stops alongside it | **Match ADR 0005** (`internal/fills.Execute`), unchanged by the adapter's model, in the same probe: buy stops inside the bar, on an exact touch of the high, gapped through (with LEAN's "unfavorable gap" message) and never reached; sell stops inside the bar, on an exact touch of the low, gapped through and never reached. Each filled at `internal/fills`' price, or did not fill where it does not. |
| Subclassing the generic `FillModel` instead | **Diverges from ADR 0005 for stop-market orders**, so the adapter's model subclasses `EquityFillModel`: with the generic base, a gapped-through buy stop and sell stop each filled at the bar's **close** plus or minus slippage rather than the open, a buy stop triggered inside the bar filled at its level with no slippage, and neither exact touch filled. |
| An exception in a fill model | **Swallowed**: LEAN logs "Order Error: id: <n>, Transaction model failed to fill for order type: StopLimit with error: ...", leaves the order unfilled, and the algorithm carries on. The adapter's model therefore records it instead: a stop-limit tagged with no N was left unfilled on a bar that reached it, with `failure` naming the order, and no LEAN error. |
| A split and a resting stop-limit's limit | **Adjusted exactly like the stop.** A stop-limit armed at 95.00/97.00 for 10 shares before AAPL's 2005-02-28 2-for-1 (factor 0.49999860000056007) was reported, in the one post-split `UpdateSubmitted` event, as 20 shares at stop 47.50 and limit 48.50 — both the raw halved values (47.49986, 48.49986) rounded to the nearest cent, the same way and the same direction. |
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
The ten rejected decisions in this recorded run were all Adds chained from
fills, rejected under the lifetime rule then in force. This historical result
is preserved. ADR 0011's 2026-09-24 amendment now allows their extra session;
the real-engine synthetic test verifies a chained Add is placed and filled,
but the pinned-image acceptance run has not been repeated under that rule.

The acceptance run under the adapter's ADR 0005 fill model, and under ADR
0011's amended lifetime for fill-chained Adds: AAPL from 2003-01-01 to
2014-12-31 with 60 warm-up bars, the Baseline's 55/20 channels, a 0.5% Unit,
$1,000,000 and stop-limit buys capped at 1N (`gap_buffer_n` 1), by the
recipe above with `-as-of 2003-01-01T00:00:00Z`. It ran 2,937 bars, to
2014-06-06, and stopped at the 7-for-1 split of 2014-06-09: LEAN's rounded
split factor (0.1428572) left it holding 27,509 raw shares where the
engine's Units were 27,510, and the adapter's split check stopped the run
(#222). 147 fills (34 entries, 58 Adds, 37 stops, 18 exits), none with zero
slippage, and 655 order changes (287 submitted, 162 updated, 103 cancelled);
14,173 records, a verified chain, a byte-identical replay, and `-verify`
reporting the run incomplete, as a stopped run must be. Every one of the 92
capped buy fills is `stop_limit_buy_fill_price` of LEAN's raw stop, its
tick-floored limit and the raw bar, to 5.7e-14 (27 triggered inside the bar,
58 gapped within the cap, 7 gapped above it and traded back); 84 of them
would have been priced differently by LEAN's native `min(high, limit)`.
Every one of the 55 stop and exit fills is `min(level, open)` less
slippage, to 1.1e-13. Of 195 buy proposals (39 entries, 156 Adds), 92
filled, and every other one was placed and is accounted for by ADR 0005:
80 (2 entries, 78 Adds) gapped above their cap without trading back and
were skipped, and 23 (3 entries, 20 Adds) never reached their level; each
was cancelled when its proposal expired. All 13 fill-chained Adds were
placed and 10 filled. The engine declined 645 Add proposals before any
order was placed (477 at the instrument's Unit cap, 168 for insufficient
cash), and the adapter rejected none. Cash never went negative: the lowest
available cash reported was $125,586.58, and the last snapshot, at
2014-06-06, had $170,081.45 available of $2,707,171.55 equity. The Exit
Channel partial fill that stopped the previous attempt on 2005-04-15 (#233)
did not occur in this run; that issue remains open.

**Live-trading fill delivery.** The owner's 2026-09-24 clarification requires
fills to reach the engine intraday, while subsequent orders can still work
in that session (ADR 0021 §7). The current daily backtest queues LEAN's fill
reports until `OnData`; a live integration must drain them promptly at a safe
transport boundary without interleaving a request already in progress.
Waiting for the daily close is not an acceptable live implementation. This
requirement does not enable live trading or replace its paper and limited-live
gates.

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
