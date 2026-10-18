# The operator tool: your kill switch, your approval, your clearance

*For the owner. How you tell a running engine to stop ordering, approve a
fresh run, and clear a rebalance that is stuck, what each of these records,
why it works this way, and what can go wrong.*

## What it is

`operator` (source: `cmd/operator`) is a small command you run on the
server, **as the engine's user, `ti-trader`**: `sudo -u ti-trader operator
...`. It never talks to IBKR and never changes a running engine's state
by itself. It writes your instruction down, checked, in a place the engine
reads, and the engine records it in its durable log as one of its inputs
before acting on it. That is the rule for everything a person tells the
system (ADR 0026, point 3): an instruction is a recorded input, never a
setting the program glances at, so the log always shows who said what,
when, and what the engine did next, and a replay of the log does the same
thing again.

**Run it as `ti-trader`.** The tool writes each file readable by the user
that ran it only (mode `0600`), and the engine runs as `ti-trader`, so a file
written as you, or as root, is one the engine cannot read: the engine would
report it as waiting at every input and never act on it. So the tool checks
who owns the inbox (that is the engine's user) and refuses to write when it
is run as anyone else, saying `run it as the engine's user, sudo -u <engine
user> operator ...`. Approval files are read by the engine too: write them
as `ti-trader` as well, into a folder `ti-trader` can read. Every example
below leaves out the `sudo -u ti-trader` in front to keep it short.

It has three commands:

| Command | What it records | Where it goes | When the engine acts |
| --- | --- | --- | --- |
| `operator kill-switch` | the kill switch engaged or released | the engine's operator inbox | before the engine's next input from the adapter |
| `operator rebalance-cleared` | your clearance of a rebalance that is stuck | the engine's operator inbox | before the engine's next input from the adapter |
| `operator approve` | your approval of a fresh run | an approval file you pass to the engine with `-approval` | when that fresh run starts, as its second input |

## The operator inbox

Each engine is started with `-operator <folder>`, its inbox, a folder that
must exist; a backtest takes none. The server's service file does not pass it
yet: until it does, create `/var/lib/trend-investing/engines/<account>/operator`
(owned by `ti-trader`) and add `-operator` with that folder to
`TI_RESTART_ARGS` in the account's `run.env`. The tool
writes one small file per instruction there, named by the time and the
kind of instruction, for example `20261017T143005Z-operator.kill-switch.json`.
Each time the adapter sends the engine something (a price, an account
figure, a session close), the engine first reads the inbox. Every file it
has not yet recorded, in the order of the times the files state, becomes an input at the next place in
the run's numbered stream of inputs. The engine writes it to the durable log,
flushed to disk, and then handles the adapter's input. The engine only ever
checks an order while handling an input, so an instruction recorded before
the next input applies to every order checked from then on.

The time a file states is the one you gave with `-at`, or the time you ran
the tool. A file written later with an earlier `-at` sorts, and is
recorded, as of that earlier time.

The file stays in the inbox afterwards; the durable log, not the inbox, is
the evidence. Once the engine has flushed an instruction to the log, it
writes a marker beside the file, ending `.delivered`, saying where it was
recorded. An instruction with that marker, or already recorded in any log
in the engine's log folder (including the one a crash resume continues), is
never recorded again, so moving a damaged log into `quarantine`, or
starting a run with its log in another folder, does not replay old
instructions. Leave the markers in place.

Your instructions count in the order of the times they state (ADR 0019,
amendment of 2026-10-17, "operator instructions count in the order of the
times they state", a rule you are asked to confirm). A kill-switch
file stating a time earlier than the newest kill switch the engine has
applied for its account is recorded in the log and refused, with no effect
(and a `.refused` note naming the later one), so an old or backdated
`released` never undoes a later `engaged`, and the log still shows it was
tried. To change the switch, record it again with the current time.

An instruction of either kind, kill switch or clearance, stating a time
more than five minutes after the engine's own clock (a slip in `-at`) is
never recorded: it would put every instruction stating the true time before
it. The engine refuses it with a `.refused` note; record it again with the
current time. Up to five minutes ahead is allowed, since the tool and the
engine may run on machines whose clocks differ by that much.

A file the engine cannot read (hand-edited, damaged, or holding more than
one record) is never used.
One the engine refuses (for another account, backdated before a later kill
switch, or a clearance that does not match) is recorded with no effect. In
every case the engine writes a note beside the file, under the file's own
name, ending `.refused`, saying why, prints the same on its output
(`journalctl -u ti-engine@<account>`), and never tries that file again.

If the engine cannot write an instruction to the durable log (a disk fault
while it is being flushed, or a log that has reached the most records a run
holds), it was not recorded: the engine writes no note, stops the run with
the error on its output, and leaves the file as it was. Resume the run, or
after a full log start a fresh one (see [restart](restart.md)), and the
engine records it before the adapter's first input, so a kill switch is
never lost to a disk fault or a full log.

The adapter (not built yet) follows the engine's numbering: the decisions
the engine sends back carry the number of the last input it recorded, and
the adapter's next input carries the number after it. When the engine
records your instructions first and then refuses the adapter's input, the
refusal carries that number too, in its `sequence` field, and the adapter
continues from it. When one of your instructions could not be recorded,
or the engine could not record the adapter's input after yours (a log that
has reached the most records a run holds, or one input deciding more than a
run takes from one input), the refusal's `sequence` is the number of the
last instruction that was, says the adapter's input was not recorded, and
the run stops: a log missing a number could never be resumed, so the run
stops where its log ends, and resumed, the adapter's next input carries the
number after it. The same holds with no instruction recorded first: when
the engine cannot record the adapter's input for either of those reasons,
its number is used up, so the refusal says so and the run stops; resumed, the
adapter's next input carries the input's own number again. (Before this
release a run carried on after that refusal and left a gap in its log that
could not be resumed.) Either way the engine writes the refusal to the
adapter first and only then stops, so a connected adapter always receives
it; if the adapter has gone away, or the refusal cannot be delivered, the
run still stops, within a couple of seconds, with the same error. An input
carrying a number already used or out of date is different. An input sent
again under its own ID, at the sequence it was sent with, gets its committed
reply again. A different input under a used number, or the same input at
another sequence, is simply refused; the refusal uses up no number, and the
run carries on.

When the engine records your instructions before the adapter's input, the
input is recorded under a later number than the one it was sent with. If
the adapter sends that input again to ask for its reply, it is answered
under either number, and refused under any other. After a restart the log
no longer says which number it was sent with, so it is answered under any
number from the first of the instructions recorded just before it. After a crash resume the engine prints `the adapter's
next input carries sequence N`; a resume that also records an approval
prints the number once, on the approval's line.

## The kill switch

```
operator kill-switch -inbox /var/lib/trend-investing/engines/boost/operator \
  -account <IBKR account> -state engaged -by owner -detail "why"
```

`-state` must be `engaged` or `released`; anything else is refused, so the
switch is never turned off by a typing slip. `-at` sets the time (RFC 3339);
without it the tool uses the current time, to the second. It prints:

```
recorded operator.kill-switch:<account>:2026-10-17T14:30:05Z (engaged) in <inbox>/20261017T143005Z-operator.kill-switch.json; the engine delivers it before the adapter's next input and journals it in its durable log
```

What the switch does is in [states-and-kill-switch](states-and-kill-switch.md):
every order is refused except the margin account's one sale back to 98%,
and the engine state does not change.

**Boost does not re-try a refused order.** Boost orders only when its target
changes (ADR 0026, point 2, "Boost"). An order refused under the switch is
not planned again when you release it. The account stays where it was until
Boost's target next changes. If you engaged the switch while Boost wanted
to buy, the account stays at 98% invested; if Boost wanted to sell, the sale
back to 98% has gone anyway. The momentum IRAs plan afresh at every decision
time, so their next rebalance orders as usual.

## A rebalance clearance

An IRA's monthly rebalance sends its sells, then its buys once the sells
are done. While any order of a rebalance is still working, the next
month's decision is held back (`rebalance-in-progress`) and nothing more is
sent (ADR 0026, point 5). Normally IBKR reports every order finished. If an
order is stuck, you deal with it at IBKR (cancel it, or confirm it is gone)
and then clear the rebalance:

```
operator rebalance-cleared -inbox <inbox> -account <IRA account> \
  -decision targets-decided:<account>:2026-09-30T20:00:00Z \
  -orders <client order id>,<client order id> -by owner -detail "cancelled both at IBKR"
```

The clearance must name the rebalance's decision and **every** order of it
that is still open, no more and no fewer. The engine refuses anything else
(a different decision, a missing or extra order, nothing in progress), so a
clearance never means more than you said. Once it is recorded, those orders
count as finished, the rest of that rebalance (its buys, if still waiting)
is never sent, and the next decision time decides afresh. `journal-query
refusals` and `journal-query orders` show which orders are open; the
[missed-rebalance runbook](../runbooks/missed-rebalance.md) has the steps.

The momentum engine is not composed into the running program yet (only
Boost is), so today a clearance is read by the momentum strategy and its
tests only. Clear only after IBKR shows the order finished. A later IBKR
report on a cleared order is refused, which stops the engine until you
reconcile. The reporter reads the clearance the same way, so that report
also raises the urgent "venue report not read" alert.

## An approval

A fresh run (anything but a crash resume: see [restart](restart.md)) starts
only with your approval. `operator approve` writes it from the previous
log itself, so you do not copy hashes and states by hand:

```
operator approve -log /var/lib/trend-investing/engines/boost/<latest run>.log \
  -account <IBKR account> -by owner -out /var/lib/trend-investing/engines/boost/approval-<new run>.json
```

It reads the log and states what it ends in: its file name and the
fingerprint of its last record, Boost's latch, and the incident it ended
in (Degraded or Halted; which instruments, and `total-cash` when a
[reconciliation](reconciliation.md) found cash it could not explain; and
whether that incident's one sale back to 98% was already attempted). That incident is carried into the
new run unless you clear it with `-clear-incident <incident id>`, which is
the step ADR 0019's recovery procedure ends with, after you have reconciled
the account. It prints what the approval says and the next step:

```
run-2026-10-20.log ends with Boost latched false; the approval carries incident engine-state:joint:… in force, halted, its one sale back to 98% attempted true
wrote <file>: start the fresh run with -approval <file> (docs/runbooks/durable-log.md, "Restarting an engine")
```

If the kill switch was engaged when the previous run ended, it also says so:
a fresh run starts with the switch released, so record it again with
`operator kill-switch -state engaged` before you start the engine. The
engine reads that record before the fresh run's first input from the
adapter, before any order is checked.

The engine checks the approval against the same log when the fresh run
starts (see [restart](restart.md), "What the engine checks"). One more check
is new: an approval that carries the incident in force and says its sale
was **not** attempted is refused when the log records an attempt, because the
new run would otherwise try the sale a second time (ADR 0019, amended
2026-10-15). Saying it **was** attempted is always accepted, since you may
have sold by hand.

The tool refuses a log it cannot read, one holding nothing past its
configuration (resume that one instead), an incident to clear that is not
the one the log ended in, and an approval file that already exists. It never
overwrites anything.

## Seeing what was recorded

`journal-query operator <log or journal> [...]` lists every operator input:
the time to the second, the account, which input, what it did, who recorded
it and why:

```
journal  seq  time                  account  input              action                                     by     detail
run.log  57   2026-10-17T14:30:05Z  U1234    kill-switch        engaged                                    owner  news before the open
run.log  2    2026-10-21T13:00:00Z  U1234    run-approved       after run-1.log, latched false; no incident  owner  -
```

`-account`, `-from`, `-to` and `-format csv|json` work as for the other
reports ([journal-query](journal-query.md)).

## What can go wrong

| What happens | What the system does | What you do |
| --- | --- | --- |
| You run the tool as yourself or as root, not `ti-trader` | Refused, nothing written: the engine could not read the file | Run it again with `sudo -u ti-trader` |
| A kill switch backdated with `-at` before one already applied | Recorded with no effect, with a `.refused` note naming the later one | Record it again with the current time |
| A kill switch or clearance with `-at` more than five minutes in the future | Never recorded; a `.refused` note says so | Record it again with the current time |
| The disk fails while the engine records an instruction | No note; the run stops with the error; the file waits | Resume the run; the instruction is recorded first |
| The log is full (the most records a run holds) when an instruction arrives | No note; the run stops with the error; the file waits | Start a fresh run; the instruction is recorded first |
| You record the switch while the engine is stopped | The engine records it before its first input from the adapter after it starts | Nothing more |
| A typing slip in `-state`, or a missing `-by` or `-detail` | Refused, nothing written | Run it again |
| Two instructions of one kind in the same second | The second is refused (the file name exists); nothing is overwritten | Run it again a second later |
| Someone edits a file in the inbox | The engine refuses it, writes `<file>.refused` saying why, and never uses it | Record the instruction again with the tool |
| A clearance that names the wrong orders | Recorded with no effect; `.refused` note says which orders are open | Clear again naming exactly those |
| The engine is restarted fresh while the switch was on | The fresh run starts with the switch off; `operator approve` warns you | Record the switch again before starting it |
| A refused Boost order after you release the switch | Not retried until Boost's target changes | Wait, or rebalance by hand at IBKR |
| The engine cannot list the inbox (permissions) | It says so on its output and tries again at the next input; nothing is recorded meanwhile | Fix the folder's owner or mode (`ti-trader`) |

The kill switch stops new orders from this program only. To stop everything,
including orders already at IBKR, stop the gateway or use IBKR's own app.

## What backs this

ADR 0026, points 3 and 5 (the kill switch and the rebalance in progress as
recorded inputs); ADR 0027 and its amendment of 2026-10-17 (the durable log,
the fresh-run approval); ADR 0019, amended 2026-10-15 (one sale per
incident); ADR 0015 (each new input type is versioned: `operator.rebalance.cleared`
is version 1).

Tests: `TestTheKillSwitchIsRecordedInTheInbox`,
`TestTheClearanceIsRecordedInTheInbox`,
`TestAnInputTheToolCannotRecordIsRefused`,
`TestTheApprovalStatesWhatThePreviousLogEndsIn`,
`TestTheApprovalWarnsOfAKillSwitchLeftEngaged`,
`TestTheApprovalWarnsOfTheSwitchThisAccountLeftEngaged`,
`TestTheInboxAndApprovalDirectoriesAreFlushed`,
`TestEachClearedOrderIDIsTrimmed`,
`TestARecordTheEngineCouldNotReadIsRefused` (`cmd/operator`, what the
tool records); `TestTheOperatorsKillSwitchStopsAndReleasesOrders`,
`TestAnOperatorInputIsDeliveredOnce`,
`TestAnOperatorRecordTheEngineRefusesIsNoted`,
`TestALogFailureWhileDeliveringAnOperatorRecordLeavesItForTheNextStart`,
`TestADeliveredRecordIsMarkedSoAMovedLogCannotRedeliverIt`,
`TestAKillSwitchOlderThanTheOneInForceIsRefused`,
`TestADeliveredRecordIsNotReadAgain`,
`TestARefusedInputAfterOperatorInputsStatesItsSequence`,
`TestARecorderRefusalAfterOperatorInputsStopsTheRun`,
`TestAResumedLogEndingInOperatorInputsAnswersTheNextInput`,
`TestAnInputSentAgainIsCheckedAgainstTheSequenceItWasSentWith`,
`TestAResumeWithAnApprovalStatesOneNextSequence`,
`TestTheOperatorInboxIsForARunThatCanReachABroker`,
`TestAnApprovalCannotUnderstateTheIncidentsSaleAttempt` (`cmd/engine`, how
the engine responds); `TestTheOwnersClearanceEndsARebalanceInProgress`,
`TestAClearanceMustNameTheRebalanceInProgress`, `TestBoostReadsNoClearance`
(`internal/targetweight`); `TestReadEndStateFindsWhatAFreshRunMustBeToldOf`,
`TestAnApprovalNeverUnderstatesTheSaleAttempt`,
`TestProposeApprovalStatesTheEndStateOrClearsTheIncident`
(`internal/restart`); `TestADecodedRecordIsExactlyOneTheToolWrites`,
`TestARecordIsDeliveredAsAnOperatorInput` (`internal/operator`);
`TestTheOperatorReportListsEveryOperatorInput` (`internal/journalquery`);
`TestARefusalRecordedAtAnotherSequenceStatesIt` (`transport`);
`TestRebalanceClearedValidate` (`internal/event`).

## Not built yet

The IBKR adapter, which will carry the engine's inputs and follow its
numbering; the inbox in the server's service file and provisioner, and the
tool installed on the server beside the engine; the momentum engines in the
running program; a day-trade
counter for the margin account (counted by hand for now: see the
[margin-warning runbook](../runbooks/margin-warning.md)).
