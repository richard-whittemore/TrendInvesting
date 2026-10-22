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

**In short.**

- **Normal**: every order that passes the limits goes.
- **Degraded**: something went wrong on named instruments (an order broke a
  limit, or the morning check found a difference it could not explain). No
  buys at all, nothing on those instruments; sells elsewhere still go.
- **Halted**: something the program cannot contain. Nothing goes.
- **Kill switch**: your own "stop". Nothing goes, but the state does not
  change, and turning it off puts things back.
- In every one of these, the joint margin account still makes **one sale
  back to 98% invested**, so it is never left borrowed. If that sale cannot
  be made safely, it is *withheld* and you may have to sell by hand: first
  stop the adapter, then the engine, as "Selling by hand safely" says, since
  the kill switch does not stop that sale.
- Only you bring an account back to Normal, through the
  [halted-account runbook](../runbooks/halted-account.md) and a fresh run you
  approve.

What is built today: the gate, the states, the kill switch input, the
margin-cushion alarm and the sale, all recorded in the journal and replayed
byte for byte, and the way you switch the kill switch on and off from the
server: `operator kill-switch` ([operator](operator.md)), run as `ti-operator`; the provisioner creates its inbox and the engine's service file passes it.

The reporter alerts you when a state changes, and repeats that alert every
30 minutes until you acknowledge it. It also alerts each change of the margin
alarm's level, each step of the sale back to 98% (sent, each fill, and IBKR
ending it unfilled), and a withheld sale you must make by hand; the daily
summary lists every withheld sale, lapses included. On the server it checks
every 5 minutes, around the clock, seven days a week, so these reach you
within 5 minutes, overnight and at weekends too
([alerts-and-dashboard](alerts-and-dashboard.md), "When the reporter runs").
Words like *net liquidation* (the account's value if everything were sold
today), *mark* (the latest recorded price) and *Excess Liquidity* (IBKR's
cushion before it would sell for you) are in the [glossary](glossary.md).

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

**A pending cash read is not a state.** When an IRA's reconciliation finds
only its cash different, on a day the Flex reader has not yet read (a deposit
or withdrawal you made that day, which the reader records at 9:30 p.m.), the
account stays in the state it is in; you get one reconciliation alert,
`<account>: reconciliation pending-cash-read (<id>)`. With more cash than the
book (a deposit), buys carry on, sized without the new cash until the
reader's record explains it. With less (a withdrawal), every buy waits,
refused `pending-cash-decrease`, until it is settled (the first refused buy
pages once; the rest are recorded only); sells and exits go on,
as while Degraded, but nothing else of Degraded applies. It becomes Degraded on
cash only if the reader's record does not explain it once the reader has read
that day, or after more than two sessions, whichever comes first: it never
waits longer ([reconciliation](reconciliation.md), "A deposit or withdrawal
you make").

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

The same holds when IBKR has rejected two of Boost's orders in a row: the
program stops its buys until you start a fresh run, but Boost's exit and
the sale back to 98% still go, and an exit IBKR rejects is planned and sent
again at each decided close while it is due. So a check of IBKR's open
orders that finds none working is not enough: the next close may send the
exit again, sized without a hand sale IBKR has not yet listed. Sell by
hand only as this chapter's hand-sale rule says (below, and the failed-sale
bullet): first stop the adapter, then the engine (`sudo systemctl stop
ti-adapter-boost`, then `sudo systemctl stop ti-engine@boost`; the kill
switch is not enough), cancel
every open SPY order in IBKR's app, confirm in IBKR's app that none is open
and that no `working`, `pending` or `cancel-pending` sell remains, and only
then sell, starting again only as a fresh run
([boost](boost.md#a-refused-or-unfilled-order-is-planned-again-re-plan)).

## The kill switch

The kill switch is your own input, recorded in the journal as
`operator.kill-switch`. You record it with `operator kill-switch -state
engaged` (or `released`), run as `ti-operator` on the server (`sudo -u
ti-operator operator ...`); the engine records it before its next input from
the adapter ([operator](operator.md)). Only that user can write it: the
engine and the units that read data from outside run as `ti-trader`, which
cannot write the inbox, so a switch in the log is yours ([operator](operator.md),
"Who can write an instruction"). While it is on:

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
the Gateway is back the same day. Nor does the switch stop the margin
account's sale back to 98%: that sale goes through it, and while the switch
is on, a Boost exit it refuses fires that sale. So before you sell by hand,
follow "Selling by hand safely" below, not the kill switch alone.

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
  remaining than before, they are never counted. Do not sell by hand, or
  change a hand sale, while a sale of the program's is working or may still
  be decided; if you must, follow "Selling by hand safely" below.

The exit and the broker-holding check use this same rule, so a sell still
working is never sold again, and a sell that was cancelled stops holding the
exit back once a snapshot shows what it sold (ADR 0019, amended 2026-10-15;
ADR 0026, points 2.5 and 4).

**Once per incident.** An incident runs from the first move out of Normal
until recovery. Going from Degraded to Halted is the same incident. Under the
kill switch, it is once per engagement. For the urgent alarm, it is once per
alarm. A sale the adapter never sent and retired (its day was over, or you
retired it because IBKR could never accept it) used nothing up: nothing was
sent, so it is tried again at the next IBKR reading while the exit is still
refused or the alarm still urgent, as a new order with a client order ID of
its own ([order lifecycle](order-lifecycle.md), "An order IBKR can never
accept"). That holds for an urgent-alarm sale made while Halted, Degraded or
under the kill switch too, though it carries the incident's or the
engagement's tag. The new order's ID survives a restart: each decision
records the retired sale and its attempt number, and `operator approve`
writes how many of the incident's sales were retired into the fresh run's
approval (`sale_retired_attempts`), so no attempt ever reuses a retired
order's ID.

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
automatically for that incident, kill-switch engagement or alarm. The reporter's urgent "sale withheld, sell by hand" alert asks
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

A sale withheld for values that are not positive is paged all the same
("sale withheld, sell by hand", once per sale), since nothing sells until a
positive snapshot comes; and a sale the gate refuses as `broker-holding`
spends its attempt and is paged "sale back to 98% refused, sell by hand".
Both pages carry the steps of "Selling by hand safely" below.

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
1.19.0) reads only configuration version 7, so a journal written under an earlier rules
version will not replay. Only the margin account has them: an
IRA has no margin cushion, so its configuration leaves them out and it gets no
alarm. A change of level is recorded as
`risk.margin.alarm`. Turning urgent fires the sale back to 98%.

## Selling by hand safely

A sale you place by hand is known to the program only once an IBKR snapshot
lists it (above), so while the program can still sell, it can sell the same
shares again. The kill switch does not prevent that in the margin account:
it refuses Boost's orders but not the sale back to 98%, and a Boost exit it
refuses fires that sale. So before any sale by hand in the margin account:

1. **Stop the program.** Stop the adapter, then the engine, as two commands
   (one command per unit, so a failed adapter stop ends the procedure before
   the engine is stopped; the adapter unit is ordered after the engine's, and
   the engine's declares nothing about the adapter, so a single command would
   not stop on a failure):
   `sudo systemctl stop ti-adapter-boost`, then
   `sudo systemctl stop ti-engine@boost`. If the first fails, stop there and
   read `journalctl -u ti-adapter-boost` before going on. Stopping only the
   engine is not enough: an order it has already released waits in the
   adapter.
2. **Keep a released order from going.** An order the engine released that
   the adapter has not sent waits in the adapter. Do not start the adapter
   again until the fresh run's engine is running (step 6): a fresh run's
   adapter never sends an earlier run's order, but stops at start naming it
   ("awaits its reply from an earlier engine run"), and you settle it with
   `relay.py resolve --run-ended` ([paper trading](paper-trading.md), that
   row).
3. **Cancel what is working at IBKR**, in IBKR's own app: every open order on
   the instrument, the program's (its order reference begins `tw-`) and any
   of your own.
4. **Confirm nothing is working.** IBKR's app lists no open order on the
   instrument, cancel-pending included. That list is the check: with the
   adapter stopped, nothing IBKR does reaches the journal, and
   `journal-query orders` lists what the program planned, released and heard
   back, not what is open now ([journal-query](journal-query.md), "Is a sell
   of the program's still working?").
5. **Sell by hand**, the number the alert gives or back to 98% of net
   liquidation (`docs/runbooks/margin-warning.md`).
6. **Start again only once your sale has filled or been cancelled, as a
   fresh run you approve** ([restart](restart.md)), **whose approval states
   the book as it is after your sale.** Read the holdings and the total cash
   (`TotalCashValue`) in IBKR once the sale has filled, and type the
   approval right after: `operator approve -log <previous run's log>
   -account joint -by owner -positions SPY=<shares> -cash <total cash>
   -currency USD -out <approval file>` (`-positions ""` when nothing is
   held; add `-clear-incident <id>` only to clear the incident). The
   adapter compares the fresh run's first reading after a close with the
   previous run's last close adjusted by the fills the program recorded,
   and your sale is not one of them: approved without the book, that
   reading is not the book, and the account halts as unverifiable
   ([reconciliation](reconciliation.md), "What can go wrong"). **Never resume the
   stopped run** (`-resume`): a resumed engine hands the adapter the reply it
   had committed, so an order it released that was never sent goes to the
   adapter again, and the adapter sends it beside your sale
   ([redelivery](redelivery.md)). A fresh run's adapter never sends an
   earlier run's order (step 2). The approval carries the incident and
   whether its sale was attempted. `operator approve` writes that from the
   log. If your sale was the incident's (the account Halted, or Degraded with
   the instrument in its scope), add `-sale-attempted` to the command, with
   the book after the sale, which the tool requires with the flag
   ([operator](operator.md), "An approval"); it records
   `incident_in_force.sale_attempted: true`, which the engine always accepts,
   since you sold by hand, and which is the one recorded safeguard that stops
   the new run making that sale at all. If your sale was not the incident's
   (an instrument outside a Degraded incident's scope, or an engagement's or
   an alarm's sale), leave the flag out: stating it would spend the
   incident's one sale back to 98% and it would never be made, while an
   engagement's or an alarm's sale is a new sale in the fresh run whatever
   the approval says, sized from its first snapshot. Otherwise, if
   the program can still sell (each row of "The sale back to 98%" below
   says), the new run sizes that sale from its first readable, fresh IBKR
   snapshot, which shows your sale in the holding once it has filled, so it
   sells at most what is still above 98%. A hand sale still open then is an
   unknown working sell, and an incident's sale is withheld for it
   (CONTAINMENT REQUIRED).

Whether the program can still sell after a withheld or failed sale depends
on the reason. `broker-values-unreadable` and `unknown-working-sell` spend
the attempt, as does a sale that was released (sent, filled in part, or
ended by IBKR) or refused at the gate (`broker-holding`, `sale-attempted`):
the program never makes that sale again for that incident, kill-switch
engagement or alarm. `stale-broker-snapshot`, `non-positive-broker-values`
and `nothing-to-sell` do not: the sale is decided again at the next
readable, positive snapshot while what fired it is in force, and `lapsed`
leaves the attempt unspent too (ADR 0019, amendment of 2026-10-17, reading
1; `event.SaleWithheldPayload.Attempted`). Even a spent attempt holds only
for the incident, engagement or alarm the sale belongs to, which the
program picks afresh each time the sale fires, in this order:

- while the account is Halted, or Degraded with the instrument in its
  scope, the sale is the incident's: once per incident;
- otherwise, while the kill switch is engaged, it is the engagement's: once
  per engagement, whether Boost's refused exit or an urgent margin alarm
  fired it (an alarm turning urgent again under the same engagement is not
  a new sale);
- otherwise, with the switch off, a margin alarm turning urgent fires that
  alarm's sale: once per alarm.

So a new incident, a new engagement of the kill switch, or, with neither in
force, a new urgent alarm is a new sale. Stopping the adapter and the engine
stops those too.

An IRA makes no sale back to 98%, so there the kill switch ([operator](operator.md))
stops the program's new orders.

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

Every refusal reaches you as an urgent alert,
`<account>: order refused (...)` with the reason filled in, within 5 minutes
([alerts-and-dashboard](alerts-and-dashboard.md)). The kill switch, a stale
IBKR snapshot and a pending cash decrease are the exceptions: they alert once
per engagement, outage or pending difference. `journal-query refusals <log>`
lists every refusal, state change and withheld sale, with its detail. In the tables, `joint` is the margin
account's name; an IRA's alerts carry its own.

### Orders the gate refuses

| What you see | What the program does | What you do |
| --- | --- | --- |
| `kill-switch`. Alert: `joint: order refused (kill-switch)`, for the first refusal of each engagement only | Refuses every order except the margin account's one sale back to 98%. The engine state does not change. A refused Boost order is planned again at the first close that decides after you release the switch | Nothing, if you meant it. Release it with `sudo -u ti-operator operator kill-switch -inbox /var/lib/trend-investing/engines/boost/operator -account joint -state released -by owner -detail "why"` ([operator](operator.md)). The switch does not cancel an order already working at IBKR: cancel that in IBKR's own app |
| `engine-degraded`. Alert: `joint: order refused (engine-degraded)` | The account is Degraded: every buy, and every order on an instrument in the scope, is refused. Sells elsewhere still go | Work the [halted-account runbook](../runbooks/halted-account.md) for the Degraded alert that came first |
| `pending-cash-decrease`. Alert: `<account>: order refused (pending-cash-decrease)` | An IRA's reconciliation found less cash than the book on a day the Flex reader has not yet read (a pending cash read): it may be a withdrawal, so every buy is refused until the reader's record explains it or it is decided (once more than two sessions have closed since it: at most three decisions). Every refusal is recorded; only the first of the pending difference pages, once. Sells and exits go on. The engine state does not change. A refused buy is planned again at the next decision | Nothing, if you withdrew money that day: it clears at the first reconciliation after the reader has covered the day. If you did not, check IBKR's activity for what moved the cash ([reconciliation](reconciliation.md), "A deposit or withdrawal you make") |
| `engine-halted`. Alert: `joint: order refused (engine-halted)` | The account is Halted: every order but the sale back to 98% is refused | The [halted-account runbook](../runbooks/halted-account.md) |
| `undecided-order`. Alerts: `joint: order refused (undecided-order)` and `joint: engine halted (risk-limit-breached)` | An order (or a sale) named no recorded decision. That is a defect in the program's order path, so the account is Halted at once | Do not recover until the cause is found: keep the log, and report it as a defect. Then the [halted-account runbook](../runbooks/halted-account.md) |
| `order-count`. Alerts: `joint: order refused (order-count)` and `joint: engine degraded (risk-limit-breached)` | More orders that are not risk-reducing were planned at one decision than the limit allows. The buys are refused and the instrument is added to the Degraded scope; risk-reducing sells are never counted | Check the plan in `journal-query orders`; then the [halted-account runbook](../runbooks/halted-account.md) |
| `order-size`. Alerts: `joint: order refused (order-size)` and the Degraded alert | The order was a larger share of net liquidation than the limit. Refused, not cut down; the instrument is frozen (Degraded) | Check the order and the account's value at IBKR; then the runbook |
| `price-band`. Alerts: `joint: order refused (price-band)` and the Degraded alert | The order's price was further from the latest mark than the band allows (5% for Boost). Refused; the instrument is frozen | Check the price data (a bad bar or a bad mark); then the runbook |
| `missing-mark`. Alerts: `joint: order refused (missing-mark)`, `<account>: no recorded mark for <instrument>` and the Degraded alert | No mark was recorded for the instrument at or after the session's close, so the price band could not be checked. Refused; the instrument is frozen. A risk-reducing sell needs no mark and is never refused for this | Check the adapter is receiving prices (`journalctl -u ti-adapter-boost`); then the runbook |
| `unvalued-holding`. Alerts: `joint: order refused (unvalued-holding)` and the Degraded alert | An order that adds exposure, while a holding has no price, so the account's exposure cannot be worked out. Refused; the instrument is frozen | Check the holding named in the detail has prices; then the runbook |
| `sell-exceeds-holding`. Alerts: `joint: order refused (sell-exceeds-holding)` and the Degraded alert | A sell larger than the account's recorded holding, net of the sells already working. Refused, never cut down; the instrument is frozen | Compare the holding at IBKR with `journal-query positions`; then the runbook |
| `broker-holding`. Alert: `joint: order refused (broker-holding)` | Three cases. (1) A sell larger than IBKR's latest holding net of working sells: refused, and the instrument is frozen (Degraded alert too). (2) While Degraded, a sell with no readable IBKR snapshot to size it from: refused, no state change. (3) The sale back to 98% itself, larger than IBKR's holding when checked: refused, no state change, and its one attempt is spent, so the program cannot make that sale again in this incident, engagement or alarm; you are also paged `<account>: sale back to 98% refused, sell by hand (<instrument>)` (the row below) | (1) and (2): check IBKR is connected and its positions match; then the runbook. (3): check the account at IBKR and, if it is still above 98%, sell back to 98% by hand, following "Selling by hand safely" (stop the adapter, then the engine, first: step 1) |
| `sale-attempted`. Alert: `joint: order refused (sale-attempted)` | The one sale back to 98% of this incident, engagement or alarm came to the gate a second time. Refused: the attempt is spent, so the program cannot make that sale again in this incident, engagement or alarm. No state change | Check `journal-query orders` for the first attempt and what IBKR did with it; if the account is still above 98%, sell the rest by hand, following "Selling by hand safely" |
| `stale-broker-snapshot` (a refused sell). Alert: `joint: order refused (stale-broker-snapshot)`, once per outage | A sell checked against an IBKR snapshot older than `MaxSnapshotAge` (30 minutes for Boost), or taken after the sell was planned. Refused, no state change; planned again at the next decision. A fresh snapshot ends the outage | Check the Gateway and the adapter ([gateway-login runbook](../runbooks/gateway-login.md)). If snapshots come back, nothing more |
| A refusal for exposure above the margin account's limit, or for an IRA's exposure or cash | Refused, and the instrument frozen, as above | See [boost](boost.md) and [momentum](momentum.md), "What can go wrong", which own these reasons |

### The account's state

| What you see | What the program does | What you do |
| --- | --- | --- |
| `<account>: engine <state> (<reason>)`, urgent | The account left Normal (Degraded or Halted), or Degraded widened, or became Halted. The body says since when, why, the scope, and what still goes. It is sent again every 30 minutes, day and night, until you acknowledge it | Acknowledge it with the key at the end of its text (`operator acknowledge`, [operator](operator.md)), then work the [halted-account runbook](../runbooks/halted-account.md). Acknowledging changes nothing else |
| `risk-limit-breached`. Alert: `joint: engine degraded (risk-limit-breached)` or `joint: engine halted (risk-limit-breached)` | An order broke a hard limit (Degraded, its instrument frozen) or named no decision (Halted). It never returns to Normal by itself, not after a clean check and not after a crash | The [halted-account runbook](../runbooks/halted-account.md); recovery ends in a fresh run approved with `operator approve -clear-incident` ([restart](restart.md)) |
| Degraded or Halted after a reconciliation | See [reconciliation](reconciliation.md), "What can go wrong", which owns those reasons | As that chapter says |
| A pending cash read: alert `<account>: reconciliation pending-cash-read (<id>)` after you deposited or withdrew money | No state change: Normal stays Normal. With more cash than the book, buys carry on, sized without the new cash until it is explained; with less, every buy is refused (`pending-cash-decrease`) until it is settled, and sells and exits go on. The engine checks the difference again at each reconciliation; the reader's record of your transfer explains it, usually at the next one. If it does not once the reader has read that day, or after more than two sessions, the account goes Degraded on cash: the `engine degraded (reconciliation-unexplained)` alert follows. The alert's text says the account stays in its state, how much the cash differs and which way, on what day, whether buys continue, and when it becomes Degraded | Acknowledge it. Nothing else if you moved money that day; otherwise check IBKR's activity ([reconciliation](reconciliation.md), "What can go wrong"). Do not approve a fresh run while it is pending |
| A pending cash read turned Degraded after two sessions with the Flex reader set aside | Degraded on cash: buys stop, sells carry on | Fix the reader (its status file and `journalctl -u ti-flex@<instance>`), check the ledger now records the transfer, then recover with a fresh run that clears the incident ([halted-account runbook](../runbooks/halted-account.md)) |
| The account is Halted and Boost wants to buy | Refused (`engine-halted` above). Only the sale back to 98% goes | Recover as above |
| The engine crashes mid-incident | It replays its durable log and resumes in the same state, scope and incident. It does not repeat a sale it already sent | Resume it ([restart](restart.md)) |
| You upgrade the program mid-incident | A new build never resumes an old log. It starts a fresh run, only after your recorded approval, which carries the incident unless you clear it | Approve with `operator approve` ([operator](operator.md)) |

### The sale back to 98%

| What you see | What the program does | What you do |
| --- | --- | --- |
| `<account>: sale back to 98% sent (<instrument>)`, routine | The sale was released to be sent in paper or live. The log is written before the send, so this means "send requested" | Check IBKR for the order whose order reference is the client order ID in the alert. Do not trade that instrument by hand while it works |
| `<account>: sale back to 98% released, not sent (<mode>)`, routine | In dry-run (the server's mode today) the sale is recorded, never sent. Nothing reaches IBKR. The release spent the attempt, so the program cannot make that sale again in this incident, engagement or alarm | If the account really is borrowed and needs it, sell back to 98% by hand at IBKR, following "Selling by hand safely" |
| `<account>: sale back to 98% filled <filled> of <wanted> (<instrument>)`, routine, one per fill | Records each fill; the body says how many are still to fill | Nothing, unless it stops partway (next row) |
| `<account>: sale back to 98% failed (<status>, <instrument>)`, urgent | IBKR ended the sale (`canceled` or `invalid`, a rejection) before it filled in full. The body says how many filled of how many, and the number left to sell. The attempt is spent, so the program cannot make that sale again in this incident, engagement or alarm; the account stays in its state | Check the account at IBKR. If it is still above 98%, sell by hand the number the body gives, following "Selling by hand safely": stop the adapter, then the engine, as step 1 says (the kill switch does not stop the sale back to 98%), cancel anything working in IBKR's app, confirm in IBKR's app that nothing is working, then sell (`docs/runbooks/margin-warning.md`) |
| `<account>: sale of <instrument> withheld, sell by hand (<reason>)`, urgent | The sale was withheld for a reason that counts as its one attempt (`broker-values-unreadable` or `unknown-working-sell`, the two rows below), once per withheld sale: the attempt is spent, so the program cannot make that sale again in this incident, engagement or alarm. Or it was withheld for IBKR values that are not positive (`non-positive-broker-values`, below), once per sale: not an attempt, but nothing sells until a positive snapshot comes. The page carries the steps of "Selling by hand safely": stop the adapter, then the engine (step 1, with its guard), cancel every open order on the instrument in IBKR's app, confirm none is open, sell, and start again as a fresh run whose approval states the book after your sale | Act by hand at IBKR now: sell back to 98% of net liquidation, following the steps the page gives ("Selling by hand safely"; `docs/runbooks/margin-warning.md`) |
| `<account>: sale back to 98% refused, sell by hand (<instrument>)`, urgent, beside `<account>: order refused (broker-holding)` | The risk gate refused the sale back to 98% itself because it is larger than IBKR's holding net of working sells. The refusal spends the attempt, so the program cannot make that sale again in this incident, engagement or alarm. Once per sale. The page names the order and the gate's detail and carries the steps of "Selling by hand safely" | Check the holding at IBKR. If the account is still above 98%, sell back to 98% by hand, following the steps the page gives: stop the adapter, then the engine (step 1, with its guard), cancel every open order on the instrument, confirm none is open, sell, and start again as a fresh run whose approval states the book after your sale |
| `broker-values-unreadable`. Alert: `joint: sale of SPY withheld, sell by hand (broker-values-unreadable)` | IBKR's holding, net liquidation, open orders or mark could not be read, or no IBKR snapshot is recorded yet. Nothing is guessed; the attempt is spent, so the program cannot make that sale again in this incident, engagement or alarm. Degraded sells are refused too | Check the Gateway ([gateway-login runbook](../runbooks/gateway-login.md)). Sell back to 98% by hand if still needed, following "Selling by hand safely" |
| `unknown-working-sell`. Alert: `joint: sale of SPY withheld, sell by hand (unknown-working-sell)`; the body says CONTAINMENT REQUIRED | A sell is working at IBKR that this program did not release (one you placed by hand, say). The attempt is spent, so the program cannot make that sale again in this incident, engagement or alarm | Follow "Selling by hand safely": stop the adapter, then the engine (step 1, with its guard), cancel or settle that order and anything else working at IBKR, confirm nothing is working, then sell back to 98% by hand if still needed; then recover |
| `stale-broker-snapshot` (a withheld sale). Alert: `<account>: sale of <instrument> withheld (<reason>)`, urgent, for the first withheld sale of an outage only, its reason always `stale-broker-snapshot` (for example `joint: sale of SPY withheld (stale-broker-snapshot)`) | The latest IBKR snapshot was too old to size the sale. Not an attempt: no state change, and **the program can still sell**: the sale is decided again from the next readable, positive snapshot while what fired it is still in force; otherwise it lapses | Check the Gateway and the adapter. If snapshots stay old and you must sell by hand, follow "Selling by hand safely" first: stop the adapter, then the engine (step 1, with its guard), or the sale decided from the next good snapshot can go out beside yours |
| `non-positive-broker-values`. Alert: `joint: sale of SPY withheld, sell by hand (non-positive-broker-values)`, urgent, once per sale (the same sale withheld again at the next snapshot sends nothing more); also listed in `journal-query refusals` and the daily summary | IBKR reported a net liquidation or mark of zero or less, so the shares to keep cannot be worked out. Not an attempt, so **the program can still sell**: the sale is decided again from the next positive snapshot; until one comes, nothing sells. Other sells are not affected | Check the account at IBKR. If the next snapshots stay wrong and you must sell by hand, follow the steps the page gives ("Selling by hand safely") first: stop the adapter, then the engine (step 1, with its guard), or the sale decided again from the next good snapshot can go out beside yours |
| `nothing-to-sell`. No alert; listed in `journal-query refusals` and the daily summary | The account is already at or below 98%, net of working sells. Nothing is sent | Nothing |
| `lapsed`. No urgent alert; listed in the daily summary and `journal-query refusals` | What needed a withheld sale has ended (the exit no longer refused or planned, or the alarm no longer urgent), so it will not be decided again. The attempt stays unspent | Nothing, unless the account is still borrowed above Boost's target: then check why with `journal-query` |
| You sold by hand after the "sell by hand" alert (IBKR's values unreadable, or an unknown working sell), or after the sale failed, was refused at the gate or was released in dry-run | The attempt is spent: the program never makes that sale again in that incident, engagement or alarm. A new incident, a new engagement of the kill switch, or, with neither in force, a new urgent alarm is a new sale ("Selling by hand safely" has the rule) | Start the engine again only once your sale has filled or been cancelled, as a fresh run whose approval states the book after your sale (`-positions ... -cash ... -currency USD`, typed right after reading the book in IBKR), or the fresh run halts on its first reading ("Selling by hand safely", step 6) |
| You sold by hand after a sale withheld for a stale IBKR snapshot, for values that are not positive or with nothing to sell, or one that lapsed | The attempt is unspent: the sale is decided again from the next readable, fresh, positive IBKR snapshot while what fired it is in force. If your sale is not yet in that snapshot, the program can sell the same shares again | If you stopped the adapter, then the engine, first, as "Selling by hand safely" says, start them again only once your sale has filled or been cancelled, as a fresh run whose approval states the book after your sale (`-positions ... -cash ... -currency USD`, typed right after reading the book in IBKR; step 6), never a resume. If the approval carries the incident in force and the sale was the incident's (the account Halted, or Degraded with the instrument in its scope), approve with `-sale-attempted` (always accepted, since you sold by hand; step 6), so the new run never makes that sale; otherwise, and for an engagement's or an alarm's sale, the new run sizes the sale from a snapshot that shows yours. If the engine is still running, stop the adapter, then the engine, now (step 1, with its guard), and check IBKR for a sale of the program's |
| The kill switch is on and the margin alarm turns urgent | The sale still goes. That is intended: the switch never keeps you borrowed | Watch for the sale's alerts |

### The margin-cushion alarm

| What you see | What the program does | What you do |
| --- | --- | --- |
| `<account>: margin cushion <level>`: `warning` and `clear` routine, `urgent` urgent; one alert per change of level (a run starts at clear, so a healthy cushion sends nothing) | Records the level (`risk.margin.alarm`). Turning urgent fires the one sale back to 98% for that alarm; the body says the reading or that there was none | Warning: check the account at IBKR the same day. Urgent: watch for the sale's alerts above, and act by hand if it is withheld. Clear: nothing (`docs/runbooks/margin-warning.md`) |
