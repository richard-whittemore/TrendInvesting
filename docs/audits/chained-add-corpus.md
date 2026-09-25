### Decision corpus 1.10.0 → 1.11.0

All 363 scenario keys are preserved: 241 hashes are byte-identical and 122 change. Full canonical decision records were captured by instrumenting `recordDecision` in isolated copies of the parent baseline and the new code, running the complete strategy package in each, and comparing every field. The harness already excludes `strategy_version` from its hash. Normalize only Add proposals' schema 2→3, their new `valid_for_sessions` field, and the consequent payload hash: 121 of the 122 changed scenario records then match exactly, including envelope identities, ordering, quantities, levels, expiry emissions, and run errors. Across the new records, Add proposals comprise 178 ordinary one-session proposals and 23 fill-chained two-session proposals (107 scenarios contain only ordinary Adds, four only chained Adds, and 11 both).

The sole remaining semantic difference is `TestAFillWhoseCostCannotBeStatedFailsClosed/add`. Its affordable opening fill on day(56) proposes rung 2. Previously day(57) expired that proposal and emitted a replacement; now the original survives day(57). Accordingly the corpus drops the day(57) expiry and replacement Add, and changes the campaign-evaluation output sequence from 64 to 63. The fixture now names the surviving day(56) proposal when reporting the day(57) Add fill. The fill still fails closed with exactly the same unrepresentable-cash-cost error (ADR 0020). This is the intended extra-session behavior and the fixture's necessary reference correction, not a changed cash rule. No other corpus decision changes beyond schema propagation.

The new window regression tests are in-package reducer tests and do not call the external `stream.run` corpus hook; hence no added corpus keys. That includes the tests pinning the extension's limits (an exit proposed at the intervening bar, or a Campaign a stop has already closed, expires the chain at that bar), which were added after this comparison and left the regenerated corpus byte-identical.

The count was re-derived independently by dumping every scenario's decision records from a checkout of the parent commit and from this branch, normalising only Add proposals' schema version, `valid_for_sessions` and payload hash: 363 scenarios each, one differing (`TestAFillWhoseCostCannotBeStatedFailsClosed/add`).

### `cmd/backtest` goldens

The three golden journals (default, `profit-protecting-stop`, `uncapped`) keep their record counts (152, 263 and 151 lines) and their configuration hashes. Normalising the strategy version string (1.10.0 → 1.11.0), Add proposals' schema version and `valid_for_sessions`, and the payload and record hashes that follow from them, every record is identical to the 1.10.0 golden. Only `profit-protecting-stop` has Add proposals: three, all fill-chained with `valid_for_sessions` 2, all filled inside the bar that covered them, none expired. The two registry `golden.json` files change only their `strategy_version` and `final_record_hash`. No trading decision changed, as `TestProposalsAreAlwaysCoveredByTheBarThatRaisedThem` predicts.

### Exhaustive changed-key classification

Semantic window change plus fixture correction:

- `TestAFillWhoseCostCannotBeStatedFailsClosed/add`

Add schema/window/hash propagation only (all 121):

- `TestACampaignsProtectiveStopIsNotAlwaysTheFirstUnitsOwn`
- `TestAChainedAddIsCheckedAgainstCashLessTheFillsBeforeIt`
- `TestAPartialStopOutEndsTheStoppedUnitsExitOrders`
- `TestAProposalCarriesItsPriceCap`
- `TestAProposalOutstandingWhenTheStreamEndsReachesATerminalEvent/an_add_proposal_raised_by_the_last_bar`
- `TestASessionCloseAddIsCheckedAgainstCashLessAFillTheSnapshotPredates`
- `TestASessionCloseMustNameExactlyTheBarsReceived/a_bar_for_a_Session_already_closed`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ADD_BREAK_ENTRY_EXIT]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ADD_BREAK_EXIT_ENTRY]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ADD_ENTRY_BREAK_EXIT]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ADD_ENTRY_EXIT_BREAK]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ADD_EXIT_BREAK_ENTRY]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ADD_EXIT_ENTRY_BREAK]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[BREAK_ADD_ENTRY_EXIT]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[BREAK_ADD_EXIT_ENTRY]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[BREAK_ENTRY_ADD_EXIT]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[BREAK_ENTRY_EXIT_ADD]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[BREAK_EXIT_ADD_ENTRY]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[BREAK_EXIT_ENTRY_ADD]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ENTRY_ADD_BREAK_EXIT]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ENTRY_ADD_EXIT_BREAK]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ENTRY_BREAK_ADD_EXIT]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ENTRY_BREAK_EXIT_ADD]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ENTRY_EXIT_ADD_BREAK]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[ENTRY_EXIT_BREAK_ADD]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[EXIT_ADD_BREAK_ENTRY]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[EXIT_ADD_ENTRY_BREAK]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[EXIT_BREAK_ADD_ENTRY]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[EXIT_BREAK_ENTRY_ADD]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[EXIT_ENTRY_ADD_BREAK]`
- `TestASessionOrdersExitsThenAddsThenEntriesAcrossInstruments/[EXIT_ENTRY_BREAK_ADD]`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#10`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#11`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#12`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#13`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#14`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#15`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#16`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#17`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#18`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#19`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#2`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#20`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#21`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#22`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#23`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#24`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#3`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#4`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#5`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#6`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#7`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#8`
- `TestASessionsDecisionsDoNotDependOnTheOrderItsBarsArrived#9`
- `TestAStopFillRecordsTheLowestLevelAcrossTheUnitsItCloses`
- `TestAddFillForAFullCampaignFailsClosed`
- `TestAddFillNamingAnUnknownProposalIsRejected`
- `TestAddFillOverExecutionIsRejected`
- `TestAddFillPredatingTheBarTheAddOrderCouldHaveExecutedInIsRejected/at_the_previous_bar's_period_end`
- `TestAddFillPredatingTheBarTheAddOrderCouldHaveExecutedInIsRejected/long_before_the_proposal_existed`
- `TestAddFillReusingAFillIDWithDifferentContentsIsRejected`
- `TestAddFillWithMismatchedDirectionIsRejected`
- `TestAddLadderOneRungPerBarUpToFourUnitsThenNoFifth`
- `TestAddProposalExpiresWhenNextBarArrivesWithoutAFill`
- `TestAddWithinTheBreakoutBarItself`
- `TestAllFourUnitsAddedInOneDayFromTheBreakoutBarItself`
- `TestAnAddFillNamingADifferentCampaignThanTheOpenOneIsRejected`
- `TestAnAddFillThatWouldLeaveTheNewUnitUnprotectedIsRejected`
- `TestAnExitFillPredatingAnEarlierClosingFillIsRejected`
- `TestAnExitLevelBetweenUnitsStopsMovesOnlyTheUnitsBelowIt`
- `TestBarPredatingAnAddFillFailsClosed`
- `TestBarPredatingAnAddFillFailsClosed#2`
- `TestByteIdenticalReplayOfAFixtureContainingASkip`
- `TestByteIdenticalReplayOfAFixtureContainingASkip#2`
- `TestCampaignAddRungOverflowPropagatesFromBothFillKinds/add`
- `TestCampaignAggregateRiskOverflowIsRefused`
- `TestCampaignStopRaiseOverflowIsRefused`
- `TestCampaignUnrepresentableResultHaltsAtTheFill/exit/realised_result_in_unit_n#2`
- `TestCampaignUnrepresentableResultHaltsAtTheFill/stop/realised_result_in_unit_n#2`
- `TestChainedAddFillWithEarlierTimestampThanPreviousUnitIsRejected`
- `TestDelistingAfterAPartialStopAggregatesTheWholeLife`
- `TestDelistingCancelsAPendingAddProposal`
- `TestDelistingEffectiveAtBeforeAnEarlierPartialStopFailsClosed`
- `TestDuplicateAddFillIsAnIdempotentNoOp`
- `TestDuplicateStopFillForASubsetOfUnitsIsAnIdempotentNoOp`
- `TestEarlierUnitStopsAreRaisedThroughASameBarChainFromTheEntry`
- `TestExitFillAfterAPartialStopAggregatesTheWholeLife`
- `TestExitOrdersCoverAPartiallyFilledAdd`
- `TestFifthAddIsDeclinedForTheInstrumentCap`
- `TestFiniteAddFillCanProduceAnInvalidUnit`
- `TestFiniteStopRaiseCanRoundBackToItsPreviousLevel`
- `TestFourUnitsAddedWithinOneBarViaTheSameBarChain`
- `TestLateAddFillAfterExpiryIsRejected`
- `TestMultiUnitCampaignExitsViaExitChannelWithAggregatedQuantityAndResult`
- `TestNoPartialUnitIsEverEmittedUnderAnyCashSkipFixture`
- `TestPartialAddFillAcceptedForFilledQuantity`
- `TestPartialStopInsideTheAddProposalsOwnBarCancelsItWithAValidExpiry`
- `TestPendingAddIsCancelledByAPartialStopAndItsFillIsRejected`
- `TestProfitProtectingCampaignStopExitsCleanly/exit`
- `TestProfitProtectingCampaignStopExitsCleanly/stop`
- `TestRemainingCampaignRiskOverflowIsRefusedAtAPartialStop`
- `TestReplayingASessionFixtureTwiceYieldsByteIdenticalEmissions`
- `TestReplayingASessionFixtureTwiceYieldsByteIdenticalEmissions#2`
- `TestReplayingTheExitOrderFixtureTwiceYieldsByteIdenticalEmissions`
- `TestReplayingTheExitOrderFixtureTwiceYieldsByteIdenticalEmissions#2`
- `TestReplayingTheFourUnitAddFixtureTwiceYieldsByteIdenticalEmissions`
- `TestReplayingTheFourUnitAddFixtureTwiceYieldsByteIdenticalEmissions#2`
- `TestReplayingTheGapFixtureTwiceYieldsByteIdenticalEmissions`
- `TestReplayingTheGapFixtureTwiceYieldsByteIdenticalEmissions#2`
- `TestStopFillClosingOneUnitLeavesTheCampaignOpenWithNoFurtherAdds`
- `TestStopFillClosingTheRemainingUnitsExitsWithTheAggregatedResult`
- `TestStopFillNamingAnAlreadyClosedUnitFailsClosed`
- `TestStopFillNamingAnUnknownUnitFailsClosed`
- `TestStopFillTimestampMustNotRegress/earlier_timestamp_is_rejected`
- `TestStopFillTimestampMustNotRegress/the_same_instant_is_accepted`
- `TestStopLadderGapCaseKeepsEarlierUnitsAtTheStandardRaise`
- `TestStopLadderRaisesEveryEarlierUnitByHalfNOnEachAdd`
- `TestStopMultipleOneRaisesUnitOneAboveItsEntry`
- `TestTheStopLadderMovesEachUnitsExitOrder`
- `TestUnaffordableThirdRungFollowedByAnAffordableFourth`
