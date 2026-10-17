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
| The previous log ended Degraded or Halted and the approval neither clears that incident nor carries it in force at least as tightly | Refuses: a restart never loosens Degraded or Halted (ADR 0019). |
| A resume names a log that is not the latest in the folder | Refuses, names the latest: only the crashed run, which is the latest, is resumed. |
| Resuming on a build stamped only "-dirty" | Refuses: such a stamp cannot tell two different uncommitted changes apart. |
| The fingerprint does not match the log | Refuses: the log is not the one you approved. |
| The approval is for another account | Refuses. |
| An approval and a resume together | Refuses: a crash resume needs no approval. |
| A resume of a log holding nothing past its configuration, in a folder with an earlier run | Refuses without the approval: the fresh run crashed before recording it. Resume with the same approval; the engine checks it against the earlier run and records it. |
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
`TestABoostRunDoesNotResumeAcrossBuilds`,
`TestADirtyBuildThatDoesNotIdentifyItsTreeNeverResumes` (`cmd/engine`);
`TestAnApprovedFreshRunStartsFromTheApprovedLatch`, `TestApprovalInputRules`,
`TestAFreshRunNeverAttemptsAnIncidentsSaleTwice` (`internal/targetweight`);
`TestAnIncidentsSaleIsAttemptedOnce` (`internal/risklimit`);
`TestOperatorRunApprovedValidate`, `TestAnApprovalStatesBoostsLatchExplicitly`,
`TestAnApprovalCarriesTheIncidentSaleAttempt`,
`TestDecodeOperatorRunApprovedRequiresEveryKey` (`internal/event`).

The engine on this branch plans no sale back to 98% yet; it records the
approval's attempt in its risk gate, which refuses a second attempt, and the
sale planner checks that before planning one.
