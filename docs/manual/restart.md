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
"Restarting an engine".

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

A small file you write (the runbook has the template):

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

## What the engine checks

| Situation | What happens |
| --- | --- |
| The log folder already holds an earlier run and there is no approval | Refuses to start, names the latest log. |
| The approval names an older log than the latest | Refuses, names the latest. |
| The approval leaves out the latch or the incident in force, or adds an item the engine does not know | Refuses, names it. |
| The approval's latch is not the one the previous log ended with | Refuses, names the previous log's latch. |
| The previous log ended Degraded or Halted and the approval neither clears that incident nor carries it in force at least as tightly | Refuses: a restart never loosens Degraded or Halted (ADR 0019). An incident the previous run was itself approved to continue counts, even when that run recorded no new state. |
| The approval clears, or carries in force, an incident whose ID is not the one the previous log ended in | Refuses, names the log's incident: an approval clears or continues only that incident, by its ID (the continued incident's ID, or else the ID of the first `strategy.engine.state` record out of Normal). |
| The approval clears an incident, or carries one in force, but the previous log ended Normal | Refuses: there is no incident to clear or continue; write `"incident_id": ""` and `"incident_in_force": null`. |
| The latest log holds nothing past its configuration (a start cut short) and a fresh run approves it | The approval still names that log and its fingerprint, but the latch and incident are checked against the latest earlier log that holds more: a cut-short start decided nothing, so the run before it is the state still in force. |
| A newer log holds an exact copy of the start of an older log's records: the configuration only, or more | It is a resume that stopped partway through copying (refused for divergence, or crashed), not a run: the engine skips it when finding the latest run, refuses to resume it or have it approved, and names the run it copied (the newest run it is a copy of the start of, never another such copy: two resumes of one crashed run that both stopped partway name that run). Resume or approve that run; keep the copy as evidence (ADR 0027 §6). A log holding only its configuration that copies no older log is a start cut short, and is still a run. |
| A log holds no record at all, only its header | A fresh run that crashed before its first record, or a resume that crashed before copying anything, leaves such a log, and nothing in it tells which. It is an abandoned start, not a run: the engine skips it when finding the latest run and refuses to resume it. Start the fresh run again with its `-as-of` and approval (a first run needs none); only if the latest run itself crashed, resume that instead. Keep the empty log as evidence. |
| A resume names a log that is not the latest run in the folder | Refuses, names the latest: only the crashed run, which is the latest run, is resumed. |
| Resuming on a build stamped only "-dirty" | Refuses: such a stamp cannot tell two different uncommitted changes apart. |
| The fingerprint does not match the log | Refuses: the log is not the one you approved. |
| The approval is for another account | Refuses. |
| An approval and a resume together | Refuses: a crash resume needs no approval. |
| A resume of a log holding nothing past its configuration, in a folder with an earlier run | Refuses without the approval: the fresh run crashed before recording it. Resume with the same approval; the engine checks it against the earlier run and records it. If that resume crashed too before recording it, it left a copy of the cut-short log, which the engine treats as an abandoned attempt: resume the same cut-short log again, with the same approval. |
| A fresh run whose start (configuration and `-as-of`) is the very one an earlier log in the folder opens with | Refuses: a crash of it before its approval was recorded would look like an abandoned resume of that log. Pick a new `-as-of` (or, if the latest log holds nothing past that start, resume it). |
| A log in the folder holds something the engine cannot recover (damaged, or a stray file ending in `.log`) | Every start is refused, naming the step: move the file, unchanged, into the `quarantine` folder beside the logs, never edit or delete it. The engine reads only the folder itself, never `quarantine`. |
| A log in the folder cannot be opened or read (its permissions, or a disk fault) | Every start is refused, saying the log could not be read; the engine never skips it. A permission fault: fix the file's owner or mode and start again. A disk fault on a log that no log holding a record was modified after: it is or may be the latest run, an intact crashed run, so fix the disk and start again, leaving the log where it is, never in `quarantine`. A disk fault on an older log, when the message names a newer log holding a record: fix the disk if you can; if not, check the service journal for a start after it was last written that finished its resume or recorded an approval (the runbook gives the command), and only then move the file, unchanged, into `quarantine`. |
| Resuming a log made by another build | Refuses, and names the upgrade procedure (ADR 0027). |
| The approval is accepted | Records it as the run's second entry, straight after the configuration, then starts. |

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
`TestAnApprovalNeverLoosensThePreviousRunsState`,
`TestAnInheritedIncidentSurvivesARunThatRecordsNoStateChange`,
`TestAFreshRunAfterACutShortStartIsCheckedAgainstTheRunBeforeIt`,
`TestAResumeAbandonedPartwayIsNeverTheLatestRun`,
`TestAResumeAbandonedBeforeItCopiedPastItsConfigurationIsNeverTheLatestRun`,
`TestAnUnrecoverableLogIsSetAsideInTheQuarantineFolder`,
`TestABoostRunDoesNotResumeAcrossBuilds`,
`TestADirtyBuildThatDoesNotIdentifyItsTreeNeverResumes` (`cmd/engine`);
`TestAnApprovedFreshRunStartsFromTheApprovedLatch`, `TestApprovalInputRules`,
`TestAFreshRunNeverAttemptsAnIncidentsSaleTwice`,
`TestARecoveredDecisionNeverFollowsAnApproval` (`internal/targetweight`);
`TestAnIncidentsSaleIsAttemptedOnce` (`internal/risklimit`);
`TestOperatorRunApprovedValidate`, `TestAnApprovalStatesBoostsLatchExplicitly`,
`TestAnApprovalCarriesTheIncidentSaleAttempt`,
`TestDecodeOperatorRunApprovedRequiresEveryKey` (`internal/event`).

The engine on this branch plans no sale back to 98% yet; it records the
approval's attempt in its risk gate, which refuses a second attempt, and the
sale planner checks that before planning one.
