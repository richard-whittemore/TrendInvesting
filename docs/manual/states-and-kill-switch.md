# Degraded, Halted and the kill switch

## What this chapter covers

The target-weight engine (Boost in the joint margin account, momentum in the
IRAs) checks every order it plans against hard limits before anything can be
sent. Those checks live outside the strategy, in the risk gate. When something
goes wrong, the gate moves the account out of **Normal** into **Degraded** or
**Halted**. Separately, you can turn on the **kill switch** yourself.

This chapter says what each of these does, what still happens while they are
on, the one sale back to 98% that the margin account still makes, and what can
go wrong. The rules come from ADR 0019 (amended 2026-09-24, 2026-10-15,
2026-10-16 and 2026-10-17) and ADR 0026, points 3 to 5.

What is built today: the gate, the states, the kill switch input, the
margin-cushion alarm and the sale, all recorded in the journal and replayed
byte for byte. The way you will switch the kill switch on and off from the
server, and the alerts that tell you a state changed, come with the composed
live engine. They are not in this build.

## The three states

| State | What causes it | What still goes | What stops |
| --- | --- | --- | --- |
| **Normal** | the starting state | every order that passes the limits | orders that break a limit |
| **Degraded** | an order broke a limit (too large, outside the price band, no mark, too much exposure, a sell larger than the broker's holding) | sells on instruments the problem does not name, sized from the broker's latest holding | every buy; every order on a named instrument |
| **Halted** | something the gate cannot contain, such as an order that names no recorded decision, which means a defect in the order path | only the one sale back to 98% (below) | every other order |

Each move out of Normal is written to the journal as a
`strategy.engine.state` record. It says which state, why, and for Degraded
which instruments are frozen (the *scope*). A second breach on another
instrument widens the scope; it never narrows. Degraded can become Halted.
Halted never becomes Degraded again.

**Leaving Degraded or Halted is never automatic.** A later clean check does
not bring the account back to Normal. Only you can, by recovery. A fresh run
starts only after you record an approval (`operator.run.approved`, decided
2026-10-17).

**A crash does not clear the state.** After a crash, the engine replays its
durable log (ADR 0027) and comes back in the same state, scope and incident.
It sends nothing it already sent.

### Sells that keep going while Degraded

A *risk-reducing* sell moves the account toward 98% invested or lower and is
no larger than what the broker says you hold, minus every sell already
working. It is exempt from the order count, the order-size limit and the price
band. One late price mark cannot block Boost's exit. While Degraded, these are
the only orders that go, and only on instruments the problem does not name. A
Degraded sell needs a broker snapshot to size from. Without one it is refused
(`broker-holding`). A sell larger than the broker's holding is refused, not cut
down, and the instrument is added to the scope.

## The kill switch

The kill switch is your own input, recorded in the journal as
`operator.kill-switch`. While it is on:

- every order is refused (`kill-switch`), routine sells and the IRAs'
  included;
- the margin account's one sale back to 98% still goes, so you never stay
  borrowed because the switch is on;
- the engine state does not change. Turning the switch off puts things back
  as they were. Turning it on twice is still one engagement.

Each input must say `engaged` true or false. An input that leaves it out, or
sets it to null, is rejected rather than read as "off", so a malformed input
can never turn the switch off.

The kill switch stops new orders from this program. To stop everything,
including orders already at the broker, stop the gateway or use IBKR's own
app.

## The one sale back to 98%

Boost borrows to hold up to 150% of the account in SPY. If the account is
stuck in a state that refuses Boost's exit, it would stay borrowed. So the
margin account gets one sale that brings it back to the unlevered weight, the
configured invested fraction of 98%, and never below.

**When it fires**, in the margin account only, never in an IRA:

- Boost's exit is refused because the account is Halted, Degraded on SPY, or
  under the kill switch; or
- the margin-cushion alarm turns urgent (below).

**How it is sized.** From one IBKR snapshot read together: your holding, net
liquidation, the mark and every open order. It keeps
ceil(0.98 × net liquidation ÷ mark) shares. It sells the smallest of three
amounts: what reaches 98%; your holding minus every working sell (so it can
never sell shares you do not have); and that minus the shares to keep (so,
together with a sell of ours still working, it never takes you below 98%).
"Every working sell" is every sell the program counts as still being sold,
by the rule below, not only the ones that IBKR snapshot lists: a sell it
released a moment ago that IBKR does not list yet still counts.
Boost's own exit is sized from the account snapshot, and is reduced the same
way, so an urgent sale and the exit in one session never sell the same shares
twice. Which sells count is set by one rule, below.
Worked example (ADR 0019): 147 SPY, $4,700 borrowed, mark $90, so net
liquidation is $8,530. It keeps 93 shares and sells 54.

**When a sell stops counting.** A sell this program sent is *pending* from
the moment it is released until the shares are known filled or a
cancellation report ends it. Pending shares are the shares sent minus the
shares known filled. One rule decides what is known filled:

- A fill report shows the shares it filled. A partial fill leaves the rest
  pending.
- A sell IBKR lists as working has filled all but the quantity IBKR says
  remains.
- A fall in your holding, seen by an account snapshot or an IBKR snapshot
  against the snapshot before it, that the fills already known do not
  explain counts as filled by the sells still pending on that fund. An
  account snapshot first sets aside the shares of any unknown sell IBKR
  listed, since that order may be what sold. A sale you make by hand while
  one of ours is working looks the same, so it is taken as ours until IBKR
  lists ours at its true remaining quantity.
- A fall counts once for the fund's sells together, never once per sell. If
  two sells are working, A for 60 and B for 40 of 100, and the account
  snapshot shows 70, the 30 are counted filled; when B's fill report for 30
  arrives, it is the same 30, so 70 shares (A's 60 and B's 10) are still
  being sold, not 40.
- A fill learned in more than one way is counted once. If the account
  snapshot already showed the 54 shares of the sale sold (147 down to 93),
  the IBKR snapshot and the fill report that follow add nothing, and the
  next exit is not cut down by them a second time.
- A snapshot shows only the fills that had happened when it was taken,
  whatever order things are recorded in. Fill reports arrive as the fills
  happen, but snapshots are taken on a schedule, so a fill report can be
  recorded before a snapshot taken a moment before the fill. If the urgent
  sale of 54 fills at 16:00:02 and its report is recorded before an account
  snapshot taken at 16:00:00 that still shows 147, the 54 still count as
  being sold against that snapshot; otherwise the session's exit could sell
  another 54 and leave 39 of 147. A fill reported after a snapshot taken
  after the fill is one the snapshot already showed, so it counts once.
- A fill in the same second as a snapshot is assumed to have come after it.
  IBKR stamps fills to the whole second, and a snapshot has no IBKR time of
  its own, so a fill stamped 16:00:02 may have happened just after a
  snapshot stamped 16:00:02 read its holdings. The gate takes the safe
  reading: against that snapshot, the 54 still count as being sold. A fill
  stamped 16:00:01 is one the snapshot shows. A cancellation in the same
  second as a snapshot is treated the same way: the shares the sale had sold
  by then still count as being sold against that snapshot. A fill stamped
  after a cancellation (IBKR can report a late fill) counts only from its
  own time: a snapshot taken between the cancellation and that fill still
  counts its shares as being sold.
- A snapshot taken earlier than one already seen, but recorded after it
  (account and IBKR snapshots each keep their own order), is judged by its
  own time too. An IBKR snapshot from 15:59:55 that still shows 147,
  recorded after the 16:00 account snapshot that showed the 54 sold, is
  still checked with the 54 counted as working against its 147, so a sell
  of 117 is refused rather than sold short. An account snapshot from 15:02,
  recorded after a 15:03 IBKR snapshot, still shows a fill reported for
  15:01, so the next exit is not cut down by it a second time.

Only a cancellation or refusal report ends what has not filled. A sell IBKR
no longer lists, with your holding unchanged, stays pending: it may simply
not have reached IBKR yet. So an urgent sale of 54 that a snapshot a minute
later does not list still holds the same session's exit back; otherwise both
could fill and sell 108 of 147, far below 98%. The sell is settled by a
later snapshot that lists it, a fall in the holding, a fill report or a
cancellation report. A sell waiting to be cancelled (cancel-pending) may
still fill, so it stays pending in full until the cancellation is confirmed.

The exit and the broker-holding check use this same rule, so a sell still
working is never sold again, and a sell that was cancelled no longer holds
the exit back (ADR 0019, amended 2026-10-15; ADR 0026, points 2.5 and 4).

**Once per incident.** An incident runs from the first move out of Normal
until recovery. Going from Degraded to Halted is the same incident. Under the
kill switch, it is once per engagement. For the urgent alarm, it is once per
alarm.

**When it does not go.** It is *withheld*, and recorded as
`risk.sale.withheld`, when:

- IBKR's values cannot be read (`broker-values-unreadable`); or
- a working sell is at IBKR that this program did not release (an unknown
  order, marked **CONTAINMENT REQUIRED**, `unknown-working-sell`); or
- the latest IBKR snapshot is older, at the trigger, than the configured
  `MaxSnapshotAge` (`stale-broker-snapshot`); or
- the latest IBKR snapshot reports a net liquidation or a mark that is not
  strictly positive (`non-positive-broker-values`): the shares to keep
  would be zero or negative, and the sale could sell into a short; or
- there is nothing to sell, because you are already at or below 98%
  (`nothing-to-sell`).

In the first two cases the sale counts as attempted and is never tried again
automatically. The alert will ask you to act by hand, and a later automatic
sale could duplicate yours. No IBKR snapshot recorded yet counts as
unreadable. In the other three nothing was attempted: the engine state does
not change, and the sale is decided again at the next IBKR snapshot that is
readable and positive, while what fired it is still in force:

- for Boost's exit, until the next decision no longer plans that sell, or
  the account no longer refuses the exit (no longer Halted, Degraded on SPY
  or under the kill switch);
- for the alarm, while it stays urgent. A snapshot with no Excess Liquidity
  reading leaves the alarm where it was, so a later snapshot that can size
  the sale still sends it, even without that reading.

Otherwise the withheld sale lapses, its attempt unspent, and the program
records one more `risk.sale.withheld`, with reason `lapsed`, so the journal
shows it will not be decided again. A lapse goes in the daily summary, not
an urgent alert: what needed the sale has ended. Boost's exit and the alarm
can fire the same sale, under the same incident or engagement. Once either
of them plans it, it is not tried again for the other and never recorded as
lapsed; withheld for both, it lapses once, when neither is still in force. The incident or the
engagement only decides the tag, picked afresh each time. The sale is always
sized from the latest snapshot, never an older one. A snapshot both stale and
not positive is withheld as `stale-broker-snapshot`: freshness is checked
first.

**`MaxSnapshotAge`.** Every account's configuration sets it, the IRAs'
included, in minutes, in the execution section
(`execution.max_snapshot_age_minutes`). It has no default: a configuration
without it, with zero or less, or with more than 390 (one regular session,
09:30 to 16:00) is rejected. For the sale, the age is the
triggering input's journalled time (the session's close for the exit, the
snapshot's own time for the urgent alarm) minus the IBKR snapshot's time;
for any other sell, it is measured from that sell's `strategy.order.planned`
time. A snapshot taken after that time is stale too, never fresh, and the
record says by how much. The wall clock is never read.

For any other sell, the IBKR snapshot gates only the broker-holding check. A
sell checked against a stale latest snapshot is refused
(`stale-broker-snapshot`) with no state change. The sell is planned again at
the next decision.

**One alert per stale episode.** The first stale refusal or stale withhold
for the account opens a *stale episode* and carries its one urgent alert
(the record's `alert` flag). Further stale refusals and withholds in the
episode are recorded without it. The episode ends at the first IBKR snapshot
recorded (fresh by its own time) or the first check, refusal, withhold or
release whose latest snapshot is fresh, whichever comes first. A later
outage opens a new episode and alerts again, even on the same snapshot.
Episodes follow the order the inputs were recorded in, so a replay or a
restart rebuilds them exactly and does not resend the alert. Net liquidation and the mark are read only by the sale's shares to
keep, so they never refuse another sell. The state and kill-switch refusals
come first, so a stale snapshot never stops Boost's exit from firing the
sale.

## The margin-cushion alarm

Each broker snapshot (`account.broker.snapshot`) is checked for the margin
account's Excess Liquidity as a share of net liquidation:

| Level | When |
| --- | --- |
| clear | 40% or more |
| warning | below 40% |
| urgent | below 20%, or no reading for 30 minutes while the market is open |

With no reading yet in a run, the 30 minutes run from the run's first IBKR
snapshot, so a run started mid-session is not urgent at once. A net
liquidation of zero or less, with Excess Liquidity readable, is a reading,
and the worst one: there is no ratio, but it is below every threshold, so
the alarm is urgent at once, market open or not (cause
`non-positive-net-liquidation`).

Every IBKR snapshot must say whether the market was open and list its open
orders; a snapshot that leaves either out is refused as invalid. "No open
orders" is an empty list. When IBKR cannot supply the orders, the snapshot
marks them unreadable instead: the sale back to 98% is then withheld as
`broker-values-unreadable` (and counts as attempted), and Degraded sells are
refused, because working sells could not be checked.

The thresholds are in the configuration (version 3 of
`strategy.target-weight.configuration`). Only the margin account has them: an
IRA has no margin cushion, so its configuration leaves them out and it gets no
alarm. A change of level is recorded as
`risk.margin.alarm`. Turning urgent fires the sale back to 98%.

## Reading it afterwards

`journal-query refusals` lists each refusal with its reason and each state
change. The new reasons are `kill-switch`, `broker-holding`,
`sale-attempted` and `stale-broker-snapshot`. Each withheld sale is listed
too (event `sale-withheld`) with its reason: `broker-values-unreadable`,
`unknown-working-sell`, `nothing-to-sell`, `stale-broker-snapshot`,
`non-positive-broker-values` or `lapsed`.
A sell exempt as risk-reducing records `risk_reducing`, with or without an
IBKR snapshot, so the journal shows why its mark, band and size were not
checked. A planned sale is version 2 of `strategy.order.planned`.
Releases and refusals with broker figures or one of the new reasons are
version 3 of `risk.order.released` and `risk.order.refused` (version 2 is the
account-wide exposure and the IRA's cash check). The tool reads every version. See [journal-query](journal-query.md).

## What can go wrong

- **The broker snapshot is missing or unreadable.** Degraded sells are refused
  and the sale back to 98% is withheld. Nothing is guessed. Sell by hand if the
  alert asks you to.
- **The broker snapshot is old.** The sale is withheld and other sells are
  refused, with no state change and one urgent alert per stale episode; the
  next fresh snapshot ends the episode and decides again.
- **The broker snapshot reports a net liquidation or mark of zero or less.**
  The sale is withheld, with no state change; the next positive snapshot
  decides it again. Other sells are not affected.
- **An order you placed by hand is still working at IBKR.** The program does
  not recognise it, so the sale is withheld (CONTAINMENT REQUIRED). Cancel or
  settle it, then recover.
- **You sold by hand after a withheld sale.** The program never repeats the
  attempt in that incident, so it cannot sell a second time.
- **The sale fills only partly, or IBKR rejects it.** It is not retried. The
  account stays in its state until you recover.
- **The kill switch is on and the margin alarm turns urgent.** The sale still
  goes. That is intended: the switch never keeps you borrowed.
- **The account is Halted and Boost wants to buy.** Refused. Only the sale
  goes until you recover.
- **The engine crashes mid-incident.** It replays its durable log and resumes
  in the same state. It does not repeat a sale it already sent.
- **You upgrade the program mid-incident.** A new build never resumes an old
  log. It starts a fresh run, and only after your recorded approval.
