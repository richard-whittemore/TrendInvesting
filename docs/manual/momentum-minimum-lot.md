# Whole shares in the IRAs: the minimum-lot rule (D7-L)

## What it does and why it exists

The IRAs buy whole shares only. IBKR does not take fractional orders through
the program's connection, and a one-share order costs a commission whatever
its size. A small account can therefore be handed a target it cannot buy: the
momentum book might ask for $300 of a stock that costs $450 a share.

The minimum-lot rule, called D7-L in the design (ADR 0026, point 2, and the
2026-10-17 amendment to it), deals with this before any order is planned. For
each stock the momentum book would hold, it compares the price of one share
(the previous session's raw close) with the dollar value that stock would get:
its weight, times the momentum share of the account (1 minus the bond share
from the glide path), times the account's net liquidation value.

If one share costs more than that value, the stock is **skipped** and the
next-ranked stock that is eligible, and within the sector cap, takes its
place. Weights are then worked out again for the new picks, and the check is
repeated until every stock in the book is affordable. A skipped stock is
never reconsidered in the same decision.

The decision records each skipped stock with the status `unaffordable`, the
price of one share and the target value it exceeded.

## When nothing is affordable

The rule only compares a stock whose target is above zero. Three cases follow
(ADR 0026 amendment, 2026-10-17, which the owner is asked to confirm):

- **(a) Fully risk-off by design.** In a risk-off month with the risk-off
  fraction at 1, every stock's target is zero, so nothing is compared. The
  decision is made: the whole invested fraction goes to the risk-off
  instrument and every held stock is sold, as the risk-off rule asks. The
  decision's detail says so.
- **(b) Every stock with a target is unaffordable.** If no stock is picked and
  at least one was skipped as unaffordable, the decision is **withheld** with
  the reason `all-unaffordable`, and the account keeps the book it has. This
  is the same in a risk-on month and a partly risk-off month. A wrong net
  liquidation value or wrong prices would look exactly like this, and
  selling every stock on a wrong figure is the worse mistake. Every skipped
  stock is recorded `unaffordable` with its one-share price and target value,
  and the detail names the lot (one share), the momentum scale and the net
  liquidation value used. It also counts the candidates valued and found
  unaffordable, and the candidates left out for another reason (no sector,
  or zero volatility), so a left-out name is never reported as valued. The
  reporter sends this as an urgent alert.
- **(c) Some stocks are affordable.** The book is the affordable stocks after
  substitution, and the decision is made as usual.

A book with no stock and nothing skipped (nothing eligible) is withheld as
`no-eligible-names`.

## Orders that round to nothing: `strategy.order.skipped`

Whole shares also mean that a small trade can round down to zero shares. The
program records every such trade as a `strategy.order.skipped` event instead
of dropping it silently. It names the instrument, the shares the rule asked
for and one of three reasons:

- `below-one-share`: the trade the planner sized is less than one share. For
  a stock already held this is a trade past the rebalance band; a buy of a
  stock not held has no band.
- `scaled-below-one-share`: a buy was scaled down to fit the budget (the fee
  allowance) and is left with less than one share.
- `held-by-working-sells`: a sell finds its whole holding already committed to
  sells that are working.

## What is recorded, and when

Each momentum decision (`strategy.targets.decided`) carries the **MinimumLot
figures**: the net liquidation value and the momentum scale the stocks were
valued against. They are recorded exactly when D7-L valued the stocks: a book
the momentum rule decided under it, or one it withheld as `all-unaffordable`.
If the run then withholds a decided book (for example because bars are
missing or a rebalance is still in progress) the figures stay on the withheld
decision beside the unaffordable stocks they value. A decision the rule
withheld for any other reason compared no stock and records none.

The rule is not applied at all when there is no account snapshot as of the
previous close (the decision is withheld whatever the picks), or when the bond
share is 1 and there is no momentum sleeve.

## Rules version 1.4.0

Applying D7-L changes which stocks the momentum rule picks, so the rules
version moved to **1.4.0**. The decision record is version 4 and the
configuration version 4 (share quantity `minimum-lot`). A journal written by
an earlier build was made without the substitution, so replaying it here
would pick different stocks. Such journals do not replay under this build and
the build refuses to claim an earlier version.

## Tests that back it

In `internal/targetweight/momentum/minimum_lot_test.go`:
`TestMinimumLotSkipsAPickWhoseOneShareExceedsItsTarget`,
`TestMinimumLotFreesTheSectorSlot`,
`TestMinimumLotIteratesUntilEveryPickIsAffordable`,
`TestMinimumLotUsesTheMomentumSleevesScale`,
`TestMinimumLotWithEveryPickAffordableIsTheBook`,
`TestMinimumLotWithNoAffordablePickPicksNothing`,
`TestMinimumLotRefusesBadInputs`, `TestDecideRecordsTheMinimumLot`,
`TestDecideWithholdsAMixedBookOfDearAndUnlabelledNames`,
`TestDecideExplainsAFullyRiskOffLiquidation`,
`TestMinimumLotLeavesAFullyRiskOffBookAsSelectBookMakesIt`,
`TestDecideWithholdsAnUnaffordablePartialRiskOffBook` and
`TestDecideWithNoCandidateUnderTheLotIsStillWithheld`.

In `internal/targetweight/momentum_d7_test.go`:
`TestMinimumLotSubstitutesAnUnaffordablePick`,
`TestMinimumLotValuesTheMomentumSleeveAfterTheGlide`,
`TestATradeRoundedToNoShareIsRecorded`,
`TestMinimumLotRunReplaysByteIdentically`,
`TestMinimumLotDoesNotApplyWithNoMomentumSleeve`,
`TestMinimumLotMovesAFullyRiskOffBookToTheRiskOffInstrument`,
`TestMinimumLotIsNotAppliedFromALateSnapshot`,
`TestMinimumLotFiguresOnlyWhenPicksWereValued` and
`TestMinimumLotWithholdsAnAllUnaffordableBook`.

`TestRulesVersionDeclaresMinimumLot` (in `rules_version_test.go`) pins the
version, `TestOrderSkippedValidate` and `TestUnaffordableNamesValidate` (in
`internal/event`) pin the recorded payloads, and
`TestAnAllUnaffordableWithholdIsUrgent` (in `internal/report`) pins the
alert.

## What can go wrong and what the program does then

- **No stock with a target is affordable.** The decision is withheld as
  `all-unaffordable` and the account keeps its book; an urgent alert is
  sent. Check the net liquidation value in the detail against the broker and
  the one-share prices of the `unaffordable` names against the market. If the
  figures are right, the account is too small for the book; if not, fix the
  data and the next decision runs on it.
- **A stock is skipped and the book holds fewer or different names than the
  research.** That is intended. The decision keeps whether the backtest would
  have picked each stock, so the difference can be measured.
- **A trade rounds to no shares.** It is recorded as `strategy.order.skipped`
  with its reason; nothing is sent.
- **No account snapshot as of the previous close.** The rule is not applied
  and the decision is withheld as a stale account snapshot; nothing is
  traded until the next decision.
- **Missing bars, or an earlier rebalance still working.** The decision is
  withheld, as before; the MinimumLot figures stay on the withheld record.
- **An old journal will not replay.** Journals from before rules version 1.4.0
  are history only. Start a new run from the current build.
