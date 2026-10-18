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
byte for byte, and the way you switch the kill switch on and off from the
server: `operator kill-switch` ([operator](operator.md)). The reporter alerts
you when a state changes, and repeats the alert until you acknowledge it. It
also alerts each change of the margin alarm's level, each step of the sale
back to 98% (sent, each fill, and IBKR ending it unfilled), and a withheld
sale you must make by hand. On the server it checks every 5 minutes, around
the clock, seven days a week, so these reach you within 5 minutes, overnight
and at weekends too ([alerts-and-dashboard](alerts-and-dashboard.md), "When
the reporter runs").

## The three states

| State | What causes it | What still goes | What stops |
| --- | --- | --- | --- |
| **Normal** | the starting state | every order that passes the limits | orders that break a limit |
| **Degraded** | an order broke a limit (too large, outside the price band, no mark, too much exposure, a sell larger than the broker's holding), or the morning reconciliation found a difference no journalled event explains ([reconciliation](reconciliation.md)) | sells on instruments the problem does not name, sized from the broker's latest holding | every buy; every order on a named instrument |
| **Halted** | something the gate cannot contain, such as an order that names no recorded decision, which means a defect in the order path, or a reconciliation whose evidence could not be compared | only the one sale back to 98% (below) | every other order |

Each move out of Normal is written to the journal as a
`strategy.engine.state` record. It says which state, why, and for Degraded
which instruments are frozen (the *scope*). A reconciliation's scope can also
name the account's cash (`total-cash`) when only the cash differs: then no
instrument is frozen, every buy is refused and sells carry on. A second breach
on another instrument widens the scope; it never narrows. Degraded can become Halted.
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
`operator.kill-switch`. You record it with `operator kill-switch -state
engaged` (or `released`); the engine records it before its next input from
the adapter ([operator](operator.md)). While it is on:

- every order is refused (`kill-switch`), routine sells and the IRAs'
  included;
- the margin account's one sale back to 98% still goes, so you never stay
  borrowed because the switch is on;
- the engine state does not change. Turning the switch off puts things back
  as they were. Turning it on twice is still one engagement.
- A Boost order refused while the switch was on is planned again at the
  next decision, after the next close: Boost orders the difference between
  what the account holds and its target, even though the target has not
  changed. So if the switch refused the first buy to 98%, the account buys
  at the first close after you turn the switch off that decides (a withheld
  close waits), while the account is Normal; while the switch stays on,
  each close is refused again, but only the engagement's first refusal
  sends you an alert. If the refused order was a sell, a later engagement
  may send one more small sale back to 98% for the share or two left over.
  A run restarted from the last decision of an earlier run picks the
  re-plan up from that decision. This holds for any refusal that leaves the engine
  Normal, a too-old IBKR snapshot included; a refusal while Degraded or
  Halted follows those states' rules below (owner's decision, 2026-10-18).
  The IRAs plan afresh at every decision time.
- A fresh run starts with the switch off. `operator approve` warns you when
  the previous run ended with it on, so you can record it again.

Each input must say `engaged` true or false. An input that leaves it out, or
sets it to null, is rejected rather than read as "off", so a malformed input
can never turn the switch off.

The kill switch stops new orders from this program. To cancel orders already
at the broker, cancel them in IBKR's own app. Stopping the Gateway is not a
cancel: it only delays an order the engine has released, which goes out once
the Gateway is back the same day.

## The one sale back to 98%

Boost borrows to hold 147% of the account in SPY while its boost is on (98%
x 1.5, its own target: [boost](boost.md)); 150% is the separate hard limit no
buy may exceed. If the account is
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

**Which sells count: one rule.** Every sell is sized from a snapshot of
your holding, net of the shares already being sold that the snapshot cannot
show: the shares sold since it was taken, and the shares that may still
sell. The program works that out with one rule, from everything recorded,
in whatever order it arrived:

1. It looks at every snapshot that read your holding: each account
   snapshot, and each IBKR snapshot that read both your positions and your
   open orders.
2. For each, it adds up the most that can still be sold after it was taken:
   - for each sell the program sent, the shares sent minus the shares known
     filled by then, or nothing once a cancellation or refusal report has
     ended it in an earlier second;
   - for each sell IBKR lists that the program did not send (a sale you
     placed by hand), what IBKR's latest listing before that snapshot says
     remains, or nothing once IBKR no longer lists it. A snapshot is passed
     over when IBKR lists a hand sale after it that its latest listing did
     not, or lists one with more remaining than the listing before (you
     raised its size): what that sale sold since cannot be known from it.
3. That snapshot's holding minus that sum is a floor: once every working
   sell has filled, you will still hold at least that much. The program
   takes the highest of these floors, never below zero.
4. What a snapshot cannot show is its holding minus that floor. Boost's exit
   is sized from the account snapshot net of it; the sale back to 98% and
   the broker-holding check from the IBKR snapshot net of it.

A sell's shares *known filled* by a snapshot are the most of two things:
its fill reports stamped before the snapshot's second; or what an IBKR
listing at or before it showed (the shares sent minus the quantity IBKR
said remains), plus the fills reported in the seconds between that
listing's second and the snapshot's.

For example, two sells are working, A for 60 and B for 40 of 100. An
account snapshot shows 70, and B's fill report for 30 arrives after it. The
snapshot before the fills gives a floor of 100 - 100 = 0; the one showing
70 gives 70 - (60 + 10) = 0. So 70 shares (A's 60 and B's 10) are still
being sold against the 70, never 40.

What follows from the rule:

- A fill learned in more than one way is counted once. Nothing is added up
  from separate sources: every share sold between two snapshots is in the
  difference between their holdings, whoever sold it. If the account
  snapshot already showed the 54 shares of the sale sold (147 down to 93),
  the IBKR snapshot and the fill report that follow add nothing, and the
  next exit is not cut down by them a second time.
- A drop in your holding is put down to no sale in particular. Say IBKR
  lists a hand sale of 30, the program sends a sell of 60 against 100, and
  the account snapshot shows 60. Whether yours or the program's sold the 40,
  you will hold 100 - 30 - 60 = 10 once both fill, so 50 of the 60 are
  still being sold.
- A snapshot shows only the fills that had happened when it was taken.
  Fill reports arrive as the fills happen, but snapshots are taken on a
  schedule, so a fill report can be recorded before a snapshot taken a
  moment before the fill. If the urgent sale of 54 fills at 16:00:02 and
  its report is recorded before an account snapshot taken at 16:00:00 that
  still shows 147, the 54 still count as being sold against that snapshot;
  otherwise the session's exit could sell another 54 and leave 39 of 147.
- A fill or a cancellation in the same second as a snapshot is assumed to
  have come after it. IBKR stamps fills to the whole second, and a snapshot
  has no IBKR time of its own, so a fill stamped 16:00:02 may have happened
  just after a snapshot stamped 16:00:02 read your holding. Against that
  snapshot, the 54 still count as being sold; a fill stamped 16:00:01 is one
  it shows. Snapshots stamped in the same second are each taken alone:
  neither is credited with a fill or a listing the other saw, and each one's
  holding gives its own floor. Every snapshot and fill time is a whole
  second; a time with a fraction of a second is refused when it is
  recorded.
- A cancelled sale stops counting once a snapshot taken in a later second
  than the cancellation reads your holding. Until then, every share it may
  have sold before the cancellation still counts, its fill reported or not,
  because a fill can be reported late and the holding is the only sure
  sign. A sell of 60 of 100 is cancelled at 15:00:02, and an IBKR snapshot
  of 15:00:02 holds 70: the other 30 may have sold after that snapshot's
  read, so they still count against it, and all 60 against an account
  snapshot of 15:00:00 that showed 100. The IBKR snapshot of 15:00:03 shows
  what it finally sold. A fill report stamped after a cancellation (IBKR can
  report a late fill) moves the cancellation's time to that fill's.
- Only a cancellation or refusal report ends a sell. A sell IBKR no longer
  lists, with your holding unchanged, stays counted: it may simply not have
  reached IBKR yet. So an urgent sale of 54 that a snapshot a minute later
  does not list still holds the same session's exit back; otherwise both
  could fill and sell 108 of 147, far below 98%. A sell waiting to be
  cancelled (cancel-pending) may still fill, so it counts in full until the
  cancellation is confirmed.
- One limit is deliberate: a sell is counted until its report arrives, even if
  the holding is later bought back to where it was, because a fall in the
  holding is never assumed to be the program's sale. The error is only ever
  toward selling less, and it clears when IBKR's fill report or the end of
  the order arrives, usually within seconds.
- An IBKR snapshot that cannot read the open orders is not one of the
  snapshots in step 1, and the hand sales are taken from the last IBKR
  snapshot that could read them, never from it. A hand sale IBKR listed
  before it is still counted at its remaining quantity, so its fill is never
  taken as the program's.
- A snapshot taken earlier than one already seen, but recorded after it
  (account and IBKR snapshots each keep their own order), is judged by the
  same rule. An IBKR snapshot from 15:59:55 that still shows 147, recorded
  after the 16:00 account snapshot that showed the 54 sold, still has the
  54 counted as working against its 147, so a sell of 117 is refused rather
  than sold short.
- A sale you make by hand is known to the program only from IBKR's list of
  open orders. One placed after the last IBKR snapshot that could read that
  list is not counted until a snapshot lists it, and if it fills first it
  never is: its shares sold look like fills of the program's working sales,
  so the program can count them as further along than they are and size the
  next exit from shares you have already sold. The same holds for a hand
  sale whose size you raise: the extra shares count from the first IBKR
  listing that shows more remaining than the one before, and if fills
  between two listings outweigh the raise, so IBKR never lists more
  remaining than before, they are never counted. Do not sell by hand, or change a hand sale, while a sale
  of the program's is working; if you must, engage the kill switch
  first, which stops every new order but the sale back to 98%.

The exit and the broker-holding check use this same rule, so a sell still
working is never sold again, and a sell that was cancelled stops holding the
exit back once a snapshot shows what it sold (ADR 0019, amended 2026-10-15;
ADR 0026, points 2.5 and 4).

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
automatically. The reporter's urgent "sale withheld, sell by hand" alert asks
you to act by hand, and a later automatic
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
shows it will not be decided again. A lapse is meant for the daily summary,
not an urgent alert, since what needed the sale has ended. The daily summary
lists lapses; `journal-query refusals` does too. Boost's exit and the alarm
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
the next decision: for Boost, the difference between the holding and its
target, even when the target has not changed.

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

The thresholds are in the configuration. They were introduced in version 5 of
`strategy.target-weight.configuration`, carried unchanged in versions 6 and the
current 7, and read from rules version 1.5.0 on. This build (rules version
1.17.0) reads only configuration version 7, so a journal written under an earlier rules
version will not replay. Only the margin account has them: an
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
account-wide exposure and the IRA's cash check). The tool, the reporter and
the dashboard read every version. See [journal-query](journal-query.md).

## What can go wrong

- **The broker snapshot is missing or unreadable.** Degraded sells are refused
  and the sale back to 98% is withheld. Nothing is guessed. If the withhold is
  `broker-values-unreadable`, the program will not try again: sell back to
  98% by hand at IBKR if the sale is still needed; the reporter sends an
  urgent "sell by hand" alert for it.
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
  account stays in its state until you recover. The reporter sends an urgent
  "sale back to 98% failed" alert saying how many shares filled. Check the
  account at IBKR; if it is still above 98%, engage the kill switch (it does not cancel orders
  already at IBKR), cancel any remaining sale in IBKR, and confirm that no
  `working`, `pending` or `cancel-pending` sell remains before selling the rest
  by hand (`docs/runbooks/margin-warning.md`).
- **The kill switch is on and the margin alarm turns urgent.** The sale still
  goes. That is intended: the switch never keeps you borrowed.
- **You release the kill switch after it refused a Boost order.** Nothing
  to do: the next close plans the difference to Boost's target and, the
  account still Normal, releases it. Check it with `journal-query`.
- **The account is Halted and Boost wants to buy.** Refused. Only the sale
  goes until you recover.
- **The engine crashes mid-incident.** It replays its durable log and resumes
  in the same state. It does not repeat a sale it already sent.
- **You upgrade the program mid-incident.** A new build never resumes an old
  log. It starts a fresh run, and only after your recorded approval.
