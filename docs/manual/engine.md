# The engine

*For the owner. What the engine does, why it is built this way, what backs
each claim, and what happens when something goes wrong.*

## What it is

The engine is one program, `ti-engine` (source: `cmd/engine`). It makes every
trading decision. It does not talk to IBKR: a separate adapter (not built yet)
sends it market data and account figures and carries out what it decides.
The two talk over a private socket on the server, one message at a time
(ADR 0014, ADR 0025).

One engine runs per account. Today only the Boost account (the joint margin
account) has a configuration, and it runs in **dry-run**: the engine decides
and records everything, and marks every order "do not send".

## What it does, step by step

1. **It reads the account's configuration** (`deploy/etc/engines/boost.config`
   on the server, `/etc/trend-investing/engines/boost.config`). The
   configuration says which strategy, which account, which mode (dry-run,
   paper or live) and the hard limits. It is fingerprinted (a "configuration
   hash", ADR 0016); any change is a new run, never a silent edit.
2. **It checks it may start.** It refuses to start, with a reason, when:
   - the build is not identified ("unstamped"): see *Build identity* below;
   - the mode you started it with (`-mode`) is not the mode in the
     configuration;
   - a paper configuration names a live account, or a live configuration a
     paper account (IBKR paper account numbers begin "DU");
   - there is no durable log (`-log`) for a mode that can reach a broker;
   - this is a fresh run after an earlier one and you have not approved it,
     or the approval leaves out the state in force (see `restart.md`);
   - a resume names a log that is not the latest in its folder.
3. **It records the configuration first** in its durable log, so the log
   always says what it decided under, including the mode.
4. **Each session** the adapter sends the SPY bar, the account's positions
   and value, the current price ("mark") and "the session closed". On the
   close the engine decides Boost's target (98% invested, or 147% when the
   boost is on), plans the one order that moves the account there, and
   checks it against the hard limits: the 150% exposure cap, the maximum
   order size, the price band around the mark (ADR 0026, point 3). An order
   that passes is **released**; one that fails is **refused**, and the
   account becomes Degraded.
5. **Before it answers, it writes.** Every input and every decision is
   written to the durable log and flushed to disk before the adapter is told
   anything (ADR 0027). A crash can lose at most the one input it was
   writing, and nothing from that input was ever released.
6. **In dry-run, every released order says `send: false`.** The adapter must
   never send it. Paper and live say `send: true`.
7. **When stopped** (SIGTERM, which `systemctl stop` sends), it also writes a
   journal of the whole run (ADR 0017).

## Build identity

Every decision carries the build that made it. A build is "stamped" with the
git commit it came from by `make engine` or `deploy/build-engine.sh`
(`scripts/build-stamp.sh`: `git describe --always --dirty`, and for a tree
with uncommitted changes a fingerprint of the whole tree after "-dirty-", so
two different sets of changes are two builds). A plain `go build` is
unstamped ("dev"), and the engine refuses dry-run, paper and live on it,
because two different unstamped builds would look like one, and the rule
that a run never continues on a different build (ADR 0027, amendment of
2026-10-16) could not work. For the same reason a build stamped only
"-dirty" may run but is never resumed. `deploy/build-engine.sh` refuses a
dirty tree outright: the server only ever runs a committed build.

## How it is started

systemd starts it from `deploy/systemd/ti-engine@.service` with every setting
written out: `-mode`, `-config`, `-socket` (in `/run/ti-engine-<account>`, a
folder only the engine's user can open), `-log`, `-out` and `-as-of`. systemd
never restarts it by itself; restarting is your step (`restart.md`). The
engines are not started at boot, and the provisioner disables any engine an
earlier version of it had set to start at boot.

## Your instructions while it runs

Started with `-operator <folder>`, the engine reads your kill switch and
rebalance clearances from that folder, which `operator` writes, and records
each in its durable log before its next input from the adapter
([operator](operator.md)).

## What can go wrong, and what it does

| What happens | What the engine does |
| --- | --- |
| Started with the wrong mode, account, or an unstamped build | Refuses to start, says why, writes nothing. |
| The disk write fails | Stops deciding; nothing more is released (ADR 0027). |
| An order breaks a hard limit | Refuses the order and records Degraded (ADR 0026, point 3). |
| A session has no SPY bar | Withholds the decision; no order (ADR 0026, point 5). |
| The account figures are missing or stale | Withholds the decision; no order. |
| The process crashes | Stays stopped. You resume it from its log (`restart.md`). |

## Try it

`make demo-boost-dry-run` builds a stamped engine, plays the adapter with an
invented SPY series (never market data), prints one line per session (the
target, any order, "released (send false)"), then restarts a second engine
from the first one's log and checks the two logs are identical byte for byte.

## What backs this

- ADRs: 0014 (socket), 0016 (configuration hash and build), 0017 (journal),
  0019 (Degraded and Halted), 0025 (IBKR adapter), 0026 (target-weight
  strategies and the risk gate), 0027 (durable log, restart).
- Tests in `cmd/engine`: `TestADryRunEngineRunsBoostAndSendsNothing`,
  `TestTheDeclaredModeMustBeTheConfiguredMode`,
  `TestTheAccountMustMatchTheMode`,
  `TestAnUnstampedBuildRefusesEveryModeButBacktest`,
  `TestTheMakefileStampsTheBuildTheEngineRecords`,
  `TestTheServiceFileStartsTheEngineWithExplicitSettings`; in
  `cmd/boost-demo`: `TestTheDemoDrivesTheRealEngineInDryRun`.

## Not built yet

The IBKR adapter (so nothing can be sent at all yet), reconciliation against
IBKR on every start (#394), re-delivery of released orders after a restart
(#395), and the IRA accounts' momentum configuration.
