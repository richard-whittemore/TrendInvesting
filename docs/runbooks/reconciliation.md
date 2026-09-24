# Runbook: when reconciliation fails

This is the procedure for when the system alerts that it has left **Normal**, under ADR 0019's 2026-09-24 amendment. It is written to be followed under pressure. Read the whole alert first, because it states what the system is **still doing** and what it has **stopped doing**.

> **Status:** written ahead of the live integration. The broker-activity sources, the exact commands and the alert channels named below are placeholders until the broker integration (#29/#30) and the alert channels are chosen. Everything marked *(to confirm)* must be filled in and rehearsed in paper trading before live money.

## First, in every case

1. **Acknowledge the alert.** Acknowledgement is recorded, and it stops the repeat reminders. It does not change the system's state.
2. **Read the state:**
   - **Degraded:** no new positions and no Adds. Positions the alert does not name are still managed: stops are raised and exits taken. Named positions are frozen, but keep their stop at the broker.
   - **Halted:** no order changes at all. Every position keeps its good-till-cancelled stop at the broker, and those stops still fill if price reaches them.
3. **If the alert says CONTAINMENT REQUIRED, do this first.** The broker has more shares in working and pending sell orders for an instrument than you hold, for example an order nobody recognises alongside our stop. If both filled, you'd be short. At the broker, cancel orders until the working sell-stops are exactly the position's own, one per Unit at its journalled level, and together cover no more than the holding, and **wait for each cancellation to be confirmed.** Record what you cancelled and why.
4. **Check every position's Units each have exactly one working stop at the broker, for the right quantity.** Use the broker's own order list, not ours. The alert says, for each affected position, whether the system **restored** its stop (with the order id), **failed** to, or has **none** to restore (UNPROTECTED, meaning no *journalled* stop).
   - **The right quantity:** each Unit's stop covers that Unit's shares, and the stops together cover exactly the number of shares the broker says you hold now, including any partial fills. Fewer leaves shares unprotected; more would sell shares you don't have. If they don't add up, amend the stops. Don't add another one.
   - **Restored:** don't place another. Two stops would both fill and sell more than you hold.
   - **Work Unit by Unit, never by instrument.** Each Unit's stop covers only that Unit's shares, so a working stop on one Unit does **not** protect another. Match each working sell-stop to its Unit by level and quantity (the alert gives each Unit's journalled level and shares).
   - **Before placing a stop by hand for a Unit**, look at the broker's working orders for that instrument:
     - If a sell-stop is already working **for that Unit** (a restoration the system submitted, or one placed earlier by hand, at its level for its quantity), **don't add another.** Verify it and record it.
     - If a restoration order for that Unit is **pending** (submitted but not yet working or rejected), **cancel it and wait for the broker to confirm the cancellation** before placing your own.
     - Place a stop by hand **only for a Unit with no working or pending sell-stop of its own**, even if other Units of the same position still have theirs.
     - Afterwards, check again that all the stops together cover exactly the holding (the quantity rule above).
   - **Failed, or Halted** (the system doesn't restore while Halted): for each Unit the broker shows without a stop, place one at that Unit's journalled level (from the alert). **Size it from the broker, never from the alert:** first read the broker's *current* holding for the instrument, subtract the shares already covered by the other Units' working stops, and use the smaller of that remainder and the Unit's journalled quantity. The alert's quantity can be stale after a partial fill, and a stop larger than the shares actually held could sell shares you no longer own. If the remainder is zero or negative, place nothing and treat it as CONTAINMENT REQUIRED (step 3). The system attempts restoration only once per incident, so it won't place another over yours. Record each stop straight away (step 3 of *Recover*).
   - **UNPROTECTED:** there's no journalled level. If the broker shows no stop, decide a level yourself and place it.

   Then record anything placed or cancelled by hand (step 3 of the recovery below).
5. **Do not "fix" the account by editing numbers anywhere.** The system never adopts a balance (ADR 0019). Every repair is a recorded event.

## Find the cause

Compare the broker's **activity statement** (trades, order history, cash activity) *(source to confirm)* against the reconciliation's named differences. Every class below has a characteristic signature.

| What the alert names | Likely cause | Where to look |
|---|---|---|
| A position quantity differs by a fill's size | a fill report the system missed or received malformed | the broker's trade history for that instrument |
| A position or order the system has never seen | a manual trade or order placed outside the system | the order history: who or what placed it |
| A position reduced to zero, or a large sale | a broker liquidation (margin), or a corporate action | broker messages; the corporate-actions notice |
| A Protective Stop missing or cancelled | cancelled at the broker (manually, or by the broker, e.g. at a corporate action) | order history for the stop's order id |
| Share count changed with no trade | a split, a merger, a conversion or a delisting | the corporate-actions notice |
| Cash differs, positions match | a dividend, interest, a fee, a transfer, or a missed commission | the cash activity statement |
| **Halted, unverifiable** | the broker's data was stale, incomplete, or unavailable | the integration's connection and status; the broker's service status |

## Recover

1. **Record the cause as the event that really happened.** Never write it as a correction to our numbers:
   - **missed fill:** recover the broker's actual fill report and record it;
   - **manual trade or order:** record it as what it was. It never becomes a new Campaign by adoption (ADR 0019); decide separately whether to close it;
   - **corporate action:** record it through the corporate-action path. If that kind is not yet supported (#38, #113), the instrument stays frozen until it is;
   - **cash difference:** record the dividend, fee, interest or transfer as its own cash event.
2. **Decide what happens to each affected order,** such as cancelling a stray order or keeping a stop, and record the decision.
3. **Record anything done by hand at the broker,** such as a stop you placed or an order you cancelled in steps 3–4 of *First*. Whatever the broker holds must be explainable by recorded events, or the next reconciliation fails again.
4. **Approve the restart.** A recovery run rebuilds state from the journal plus the events just recorded, then runs a fresh reconciliation. **Only a passing reconciliation returns the system to Normal.** If it fails, the new alert names what is still unexplained; go back to *Find the cause*. *(Command to confirm.)*
5. **Keep the failed run.** It is evidence, and it is never deleted or overwritten (ADRs 0012, 0017, 0018).

## If the system itself is silent

If the **heartbeat alert** fires, the system is down or wedged and cannot report its own state. The stops at the broker are still working.

1. Check every position has a stop, on the broker's own screen.
2. Restart through the normal startup path. The system comes back **in the state it was in**. If it was Degraded or Halted before it went down, it stays so, and a passing startup reconciliation doesn't return it to Normal; only the recovery above does. Startup reconciliation runs before any decision, and it alerts if anything changed while the system was down.

## Before live money

- Choose the alert channels (at least two, independent) and the alert, heartbeat and timeout intervals.
- **Provision the external heartbeat monitor** (outside the system and its host), and prove in paper trading that it pages: stop the process, then wedge it (alive but not processing), and confirm each triggers the heartbeat alert on both channels within the configured timeout.
- Prove every alert **repeats until acknowledged**, and that a failed delivery on one channel is retried on the other.
- Fill in every *(to confirm)* above.
- **Rehearse each row of the cause table in paper trading**, by deliberately creating that discrepancy, and time the recovery.
