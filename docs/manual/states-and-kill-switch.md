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
Worked example (ADR 0019): 147 SPY, $4,700 borrowed, mark $90, so net
liquidation is $8,530. It keeps 93 shares and sells 54.

**Once per incident.** An incident runs from the first move out of Normal
until recovery. Going from Degraded to Halted is the same incident. Under the
kill switch, it is once per engagement. For the urgent alarm, it is once per
alarm.

**When it does not go.** It is *withheld*, and recorded as
`risk.sale.withheld`, when:

- IBKR's values cannot be read; or
- a working sell is at IBKR that this program did not release (an unknown
  order, marked **CONTAINMENT REQUIRED**); or
- there is nothing to sell, because you are already at or below 98%.

In the first two cases the sale counts as attempted and is never tried again
automatically. The alert will ask you to act by hand, and a later automatic
sale could duplicate yours. In the third case nothing was attempted.

## The margin-cushion alarm

Each broker snapshot (`account.broker.snapshot`) is checked for the margin
account's Excess Liquidity as a share of net liquidation:

| Level | When |
| --- | --- |
| clear | 40% or more |
| warning | below 40% |
| urgent | below 20%, or no reading for 30 minutes while the market is open |

The thresholds are in the configuration (version 3 of
`strategy.target-weight.configuration`). Only the margin account has them: an
IRA has no margin cushion, so its configuration leaves them out and it gets no
alarm. A change of level is recorded as
`risk.margin.alarm`. Turning urgent fires the sale back to 98%.

## Reading it afterwards

`journal-query refusals` lists each refusal with its reason and each state
change. The new reasons are `kill-switch`, `broker-holding` and
`sale-attempted`. A planned sale is version 2 of `strategy.order.planned`.
Releases and refusals with broker figures or one of the new reasons are
version 3 of `risk.order.released` and `risk.order.refused` (version 2 is the
account-wide exposure and the IRA's cash check). The tool reads every version. See [journal-query](journal-query.md).

## What can go wrong

- **The broker snapshot is missing or unreadable.** Degraded sells are refused
  and the sale back to 98% is withheld. Nothing is guessed. Sell by hand if the
  alert asks you to.
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
