# Restarting an engine

*For the owner. When an engine stops, how it starts again, and why you are
the one who starts it.*

## The rule

You decided this on 2026-10-16:

- **After a crash, the engine resumes.** It reads its durable log, rebuilds
  exactly where it was, and carries on with the same run.
- **After anything else, it starts a fresh run, and only once you approve.**
  "Anything else" is: you stopped it, the server rebooted, you are taking
  the account out of Halted, you installed a new build, or you cleared a
  rebalance that stopped midway.

systemd never restarts an engine, and engines do not start at boot. Every
start is a step you take, written down in `docs/runbooks/durable-log.md`,
"Restarting an engine": you write `TI_RUN`, `TI_AS_OF` and
`TI_RESTART_ARGS` in the account's `run.env`, then `sudo systemctl start
ti-engine@<account>` ([engine](engine.md), "How it is started").

A reboot is a fresh run when the server shut down normally: shutdown stops
the engine with SIGTERM, which ends the run cleanly and writes its journal.
A power cut or a hard reset kills the engine without that, which is a
crash: resume it.

## Why

A crash is the one case where the log is the whole truth: the engine stopped
mid-stride and the log says exactly where. In every other case something
happened between the old run and the new one (a person decided something, a
new program, a reboot during which the market moved), so the engine must not
quietly carry on as if nothing had. A fresh run starts from what you have
checked and approved, and the approval is recorded as the run's own input,
so the record shows who allowed the restart, what they looked at, and what
state the account was in.

## What the approval says

A small file `operator approve` writes for you from the previous log
([operator](operator.md)), or that you write by hand (the durable log
runbook has the template). `operator` is installed on the server: run it as
`sudo -u ti-trader operator approve ...`. It holds:

- the account;
- the name of the previous run's log, and the fingerprint of its last
  record (its "final record hash"), so the approval is tied to the exact
  evidence you reviewed;
- the incident you are clearing, if any;
- who approved and when;
- the state in force: whether Boost's boost was on ("latched") when the
  previous run ended. The new run starts from that, rather than working it
  out again from prices;
- the incident still in force, if the previous run ended in one (otherwise
  `null`): whether it holds the account Degraded (and on which instruments)
  or Halted, so the new run starts under the same restrictions, and whether
  its one sale back to 98% was already attempted, with
  that sale's client order ID if it was sent. The rule is one attempt per
  incident (ADR 0019), so a new run told "attempted" never tries it again.

Every item must be written, even when it is "none" (`null`): the engine
refuses an approval that leaves one out rather than guess, so a forgotten
latch can never quietly switch the boost off.

## What can go wrong, and what the engine does then

Every check below is made before the engine opens its socket, so a refused
start decides nothing and sends nothing. Messages are quoted from what the
engine prints (after `engine:`); the rest names the files and values
concerned. Read them with `journalctl -u ti-engine@<account> -n 20`. The
steps for each case are in `docs/runbooks/durable-log.md` (the "durable log
runbook"); the section is named where there is one.

### Starting a fresh run

| What you see | What the program does | What you do |
| --- | --- | --- |
| `... already holds an earlier run's durable log, ..., so this is a fresh run, and a fresh run decides nothing until the owner's approval is recorded` | Refuses to start, naming the latest log. | Write the approval with `operator approve` and set `TI_RESTART_ARGS=-approval <file>` (durable log runbook, "Start a fresh run after an earlier one"). After a crash, resume instead. |
| `-approval names an earlier run, but ... holds no earlier run's durable log` | Refuses: a first run needs no approval. | Leave `TI_RESTART_ARGS` empty. |
| `-approval and -resume cannot both be given` | Refuses: a crash resume needs no approval. The one exception is a cut-short start (below). | Pass one or the other. |
| `the approval in ... is for account "..."; this configuration decides "..."` | Refuses. | Approve the right account's log. |
| `the approval in ... names ..., but the latest earlier run in ... is ...; approve the latest run` | Refuses, naming the latest run. | Approve that log. |
| `the approval in ... names ... with final record hash ..., but that log's final record hash is ...` | Refuses: the approval is not of the log as it stands. | Write the approval again from the log (`operator approve` reads the hash from it). |
| `the approval states no ...; every key is required, null where there is none`, or `the approval in ... is refused: ...` (for example `boost latched is required`, `incident in force: sale attempted is required`, or a schema version this build does not read) | Refuses rather than guess: a forgotten latch can never quietly switch the boost off. | Write every key, `null` where there is none; `operator approve` writes them all. |
| `...'s last decision, ..., has latched_after X, but the approval states latched Y`, or `... decided nothing, and the approval it started from, ..., states latched X` | Refuses, naming the latch the log ended with: the latch is restored, never changed by a restart. | State that latch. |
| `... ended degraded (or halted), but the approval neither clears that incident (incident_id) nor carries it in force (incident_in_force)` | Refuses: leaving Degraded or Halted is never automatic (ADR 0019). An incident the previous run was itself approved to continue counts, even when that run recorded no new state. | Clear it with `operator approve -clear-incident <id>` after the recovery in `docs/runbooks/reconciliation.md`, or carry it in force (leave `-clear-incident` out). |
| `... ended halted, but the approval carries its incident in force as degraded: a restart never loosens Halted`, or `... a restart never narrows a Degraded scope` | Refuses: a restart never loosens the state. | Carry it at least as tightly as the log ended. |
| `the approval clears incident "...", but ... ended ... in incident ...: an approval clears only that incident, by its ID`, or `the approval carries incident "..." in force, but ... the incident in force is that one, under its ID` | Refuses, naming the log's incident: the continued incident's ID, or else the ID of the first `strategy.engine.state` record out of Normal. | Use that ID. |
| `the approval clears incident "...", but ... records no incident: it ended normal`, or `the approval carries incident "..." in force, but ... records no incident: it ended normal` | Refuses: there is nothing to clear or continue. | Write `"incident_id": ""` and `"incident_in_force": null`. |
| `... records an attempt of incident ...'s one sale back to 98% ..., but the approval states it not attempted` | Refuses: the new run would try the sale again, and the rule is one attempt per incident (ADR 0019, amended 2026-10-15). Saying attempted is always accepted. | Change `sale_attempted` to `true` in the approval file and nothing else ([operator](operator.md), "An approval"); the order ID stays as the tool wrote it. |
| `... holds nothing past its configuration, and this run's configuration input is the one it opens with ...: resume ... with -resume, or declare a new start with -as-of` | Refuses: the latest log is a start cut short with this very start (configuration and `-as-of`), and a crash of this run before its approval was recorded could not be told apart from it. | Resume that log, or pick a new `TI_AS_OF`. |
| `... opens with this run's configuration input, so a crash of this run before its approval was recorded would leave a log no resume could tell from a resume of ... abandoned after its configuration` | Refuses: `TI_AS_OF` repeats the start of an earlier run in the folder. | Pick a new `TI_AS_OF`. |
| `... already exists: a durable log is recorded evidence and is never appended to or overwritten` | Refuses: `TI_RUN` names an existing log. | Set `TI_RUN` to a new name. |
| `recorded the owner's approval of a fresh run after ... at sequence 2; the adapter's first input carries sequence 3` | Accepted: the approval is the run's second entry, straight after the configuration. When the latest log holds nothing past its configuration (a start cut short), the approval still names that log and its fingerprint, but the latch and incident are checked against the latest earlier log that holds more. | Nothing. Start the adapter. |
| `... arrived before the owner's approval; a fresh run after an earlier one reads nothing but its configuration until the approval is recorded` | Refuses the input (a bar, a snapshot, a mark, a state, a kill switch, a fill or order report): nothing, not even a sale back to 98%, happens before the approval. An approval arriving after any other input is refused too. | Nothing in normal running: the engine records the approval before it opens its socket. If you see it, open an issue. |

### Resuming after a crash

| What you see | What the program does | What you do |
| --- | --- | --- |
| `resumed N record(s) from ...` and `the adapter's next input carries sequence N` | Accepted: it rebuilt the run from the log. | Nothing. Start the adapter. |
| `left out a torn tail of N byte(s) at byte M of ...` | Accepted: the crash cut the last input's write short; none of its decisions was released, so the adapter sends it again. | Nothing (durable log runbook, "A torn tail"). |
| `-resume names ..., but the latest run in ..., the directory of -log, is "..."` | Refuses: only the crashed run, which is the latest run, is resumed. | Resume that log, or start a fresh run with an approval. |
| `-resume names ..., a resume of ... abandoned partway: its committed records are a prefix of that log's` | Refuses: a newer log holding an exact copy of the start of an older log's records is a resume that stopped partway (refused for divergence, or crashed), not a run. The engine skips it when finding the latest run and names the run it copied (the newest one, never another such copy). | Resume or approve the named run; keep the copy as evidence (ADR 0027 §6). |
| `-resume names ..., which holds no committed record` | Refuses: a log with only its header is an abandoned start (a fresh run or a resume that crashed before its first record), and nothing in it tells which. | Start the fresh run again with its `-as-of` and approval (a first run needs none); only if the latest run itself crashed, resume that. Keep the empty log. |
| `... holds nothing past its configuration, and ... holds an earlier run, ..., so its start was a fresh run whose approval was never recorded: resume it with -approval naming ...` | Refuses the resume without the approval: the fresh run crashed before recording it. | Resume with `-resume` and the same `-approval`; the engine checks it against the earlier run and records it. If that crashed too, resume the same cut-short log again with the same approval. |
| `the durable log opens with a different configuration input from the one this invocation composed; resume with the -config and -as-of the logged run started with` | Refuses. | Keep `TI_AS_OF` as the crashed run had it, and the configuration unchanged. |
| `the durable log was recorded by run ... / ..., not this run ... / ...` | Refuses: the configuration changed, so this is not the same run. | Start a fresh run you approve instead. |
| `the durable log was recorded by build "...", not this build "...", and a run does not resume across builds` | Refuses. | Durable log runbook, "Upgrading the build": stop at the end of a session, keep the old log, start a fresh run on the new build. |
| `-resume refuses build "...-dirty"` | Refuses: a stamp ending only "-dirty" cannot tell two different uncommitted changes apart. | Build from a committed tree (`deploy/build-engine.sh` refuses a dirty one). |
| `resuming from the durable log, event ... did not reproduce what the log records for it` | Refuses and stops: this build decides that input differently from the log. The new log holds a copy of the records before it, an abandoned resume the engine skips from then on. | Durable log runbook, "A resume that does not reproduce its log": keep both logs, do not retry, open an issue, and start a fresh run from a reconciled baseline. |

### A log in the folder the engine cannot use

Every start reads every log in the folder to find the latest run, so these
refuse a fresh run and a resume alike.

| What you see | What the program does | What you do |
| --- | --- | --- |
| `...; every log in the directory is read to find the latest run, so move ..., unchanged, into .../quarantine and start again, never editing or deleting it` | Refuses every start: a log is damaged, or a stray file ends in `.log`. The engine reads only the folder itself, never `quarantine`. | Durable log runbook, "An unrecoverable log". |
| `... could not be read, as the engine is not permitted to read it` | Refuses every start; the engine never skips it. | Fix the file's owner or mode (it belongs to `ti-trader`) and start again, leaving the log where it is. |
| `... could not be read, a fault in reading it, such as the disk, ... so it is or may be the latest run, an intact crashed run` | Refuses every start. | Fix the disk and start again, leaving the log where it is, never in `quarantine`. If the disk is written off, follow the give-up steps in the durable log runbook, "A log that cannot be read" (below). |
| `... could not be read, a fault in reading it, ...; if it persists, ..., a log holding a record, was modified after it` | Refuses every start: an older log cannot be read, and a newer one holding a record exists. | Fix the disk if you can; if not, check the service journal as the runbook says ("A log that cannot be read") before moving the file, unchanged, into `quarantine`. |

**Giving up an unreadable latest log.** Move it, unchanged, into
`quarantine` as evidence, and start a fresh run from a reconciled baseline
with an approval naming the latest run that can still be read. The engine
can check that approval only against that older log, so the approval is
your statement of what you checked at IBKR before the start: positions and
cash against the last readable journal; open orders cancelled or recorded;
the incident that log ended in carried in force (never loosened, cleared
only by its ID through the recorded recovery); and the Boost latch that log
ended with. An incident or latch change recorded only in the lost log
cannot be expressed in the approval, and decisions recorded only there are
gone. The kill switch is not part of the approval: a fresh run starts with
it released. If you believe the lost log opened an incident, settle the
positions by hand at IBKR first: the engine's own reconciliation does not
run yet ([reconciliation](reconciliation.md)), so nothing else will catch a
difference.

## What the engine cannot tell

It cannot tell a crashed log from one a clean stop ended. Choosing "resume"
is your judgement: `systemctl status` shows whether the process failed or was
stopped.

## What backs this

ADR 0027 and its amendment of 2026-10-17 (accepted); ADR 0026, point 5
(state recovered from the record, never recomputed); ADR 0019 (recovery from
Degraded and Halted; one sale back to 98% per incident). Tests:
`TestAFreshRunAfterAPriorRunNeedsTheOwnersApproval`,
`TestAnApprovalMustNameTheLatestRun`, `TestAnApprovalFileStatesEveryKey`,
`TestACrashBeforeTheApprovalIsRecordedResumesWithIt`,
`TestARepeatedCrashBeforeTheApprovalIsRecordedResumesWithTheSameApproval`,
`TestACutShortFirstRunResumesWithoutAnApprovalAfterRepeatedCrashes`,
`TestAFreshRunCannotRepeatAStartThatRecordedNothing`,
`TestAFreshRunCannotRepeatTheStartOfAnEarlierRun`,
`TestAFreshRunAfterACutShortStartIsCheckedAgainstTheRunBeforeIt`,
`TestAResumeAbandonedPartwayIsNeverTheLatestRun`,
`TestAResumeAbandonedBeforeItCopiedPastItsConfigurationIsNeverTheLatestRun`,
`TestAnUnrecoverableLogIsSetAsideInTheQuarantineFolder`,
`TestABoostRunDoesNotResumeAcrossBuilds`,
`TestADirtyBuildThatDoesNotIdentifyItsTreeNeverResumes`,
`TestAnApprovalCannotUnderstateTheIncidentsSaleAttempt` (`cmd/engine`);
`TestAnApprovalNeverLoosensThePreviousRunsState`,
`TestAnInheritedIncidentSurvivesARunThatRecordsNoStateChange`,
`TestAnUndecodableRecordRefusesTheIncidentCheck`,
`TestAnApprovedLatchIsTheOneTheApprovedLogEndsWith`,
`TestAnApprovalNeverUnderstatesTheSaleAttempt`,
`TestALogDirectoryIsClassifiedIntoRunsAttemptsAndEmptyStarts`,
`TestAnEmptyDirectoryHasNoLatestRun`,
`TestAStartCutShortHoldsNothingPastItsConfiguration`,
`TestAnAttemptAtAStartIsALogOfThatStartHoldingNothingPastIt`,
`TestAReadFaultIsToldApartFromTheLatestRun` (`internal/restart`, where
these rules live; `cmd/engine` only finds, orders and reads the logs);
`TestAnApprovedFreshRunStartsFromTheApprovedLatch`, `TestApprovalInputRules`,
`TestAFreshRunNeverAttemptsAnIncidentsSaleTwice`,
`TestARecoveredDecisionNeverFollowsAnApproval`,
`TestAFreshRunReadsNothingBeforeItsApproval`,
`TestAnApprovalIsTheFirstInputAfterTheConfiguration` (`internal/targetweight`);
`TestAnIncidentsSaleIsAttemptedOnce` (`internal/risklimit`);
`TestOperatorRunApprovedValidate`, `TestAnApprovalStatesBoostsLatchExplicitly`,
`TestAnApprovalCarriesTheIncidentSaleAttempt`,
`TestDecodeOperatorRunApprovedRequiresEveryKey` (`internal/event`).

The engine writes the approval's attempt into its risk gate's one record
of the incident's sale, the same record the sale back to 98%
([states-and-kill-switch](states-and-kill-switch.md)) checks before
planning one and updates when it does: an incident whose sale was attempted
is never sold again, and one whose sale was not attempted is sold once,
tagged with that incident (`TestARestoredIncidentsEarlierAttemptIsNotRepeated`,
`internal/risklimit`).
