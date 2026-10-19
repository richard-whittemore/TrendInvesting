# The engine

*For the owner. What the engine does, why it is built this way, what backs
each claim, and what happens when something goes wrong.*

## What it is

The engine is one program, `ti-engine` (source: `cmd/engine`). It makes every
trading decision. It does not talk to IBKR: a separate program, the
[IBKR adapter](ibkr-adapter.md), feeds it market data and account figures
and carries out what it decides. The two talk over a private socket (a
local connection between two programs on the same server), one message at
a time (ADR 0014, ADR 0025).

The adapter is built: its main loop, its link to the engine's socket, the
real IBKR client, the paper sending switch (`paper_sending_enabled`, off as
shipped) and the delivery of the daily data job's stream (SPY's bar and the
session's close) behind a broker snapshot. Two inputs the Boost engine needs
before it decides a session are still produced by nothing: the account
snapshot as of the close and the recorded mark. So on the server today the
engine withholds every session (`stale-account-snapshot`, below) and
releases nothing ([paper trading](paper-trading.md)). `make
demo-boost-dry-run` shows a full run with an invented account.

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
   - the mode is live: no live order goes out before the limited-live gate,
     so only paper, bound to a paper account, releases orders to send (see
     [paper trading](paper-trading.md));
   - there is no durable log (`-log`) for a mode that can reach a broker;
   - the configuration is a momentum (IRA) one: this program composes
     only Boost (`compose.go`);
   - the durable log or the journal it is told to write already exists
     (a run name is never reused);
   - this is a fresh run after an earlier one and you have not approved it,
     or the approval leaves out the state in force (see
     [restart](restart.md));
   - a resume names a log that is not the latest in its folder.

   Every refusal, with the words it prints, is in "What can go wrong"
   below.
3. **It records the configuration first** in its durable log, so the log
   always says what it decided under, including the mode.
4. **Each session** the engine needs the SPY bar, an account snapshot
   (positions and value) as of the close, the current price ("mark") and
   "the session closed". Today the adapter delivers the bar and the close
   from the daily data job's file, behind a broker snapshot (IBKR's view of
   the account, which the sale back to 98% and the margin alarm read); the
   account snapshot as of the close and the recorded mark are not produced
   yet, so every session is withheld (see "What can go wrong"). On the
   close the engine decides Boost's target (98% invested, or 147% when the
   boost is on), plans the one order that moves the account there, and
   checks it against the hard limits: the 150% exposure cap, the maximum
   order size, the price band around the mark (ADR 0026, point 3). An order
   that passes is **released**; one that fails is **refused** (the reasons
   and what follows are in [states-and-kill-switch](states-and-kill-switch.md)).
5. **Before it answers, it writes.** Every input and every decision is
   written to the durable log and flushed to disk before the adapter is told
   anything (ADR 0027). A crash can lose at most the one input it was
   writing, and nothing from that input was ever released.
6. **In dry-run, every released order says `send: false`.** The adapter must
   never send it. Paper says `send: true` (live would too, but the engine
   refuses to start in live until the limited-live gate).
7. **When stopped** (SIGTERM, which `systemctl stop` sends, and which a
   normal shutdown of the server sends too), it also writes a journal of the
   whole run (ADR 0017) and prints `wrote <journal>: N record(s) over ...`.

### What dry-run simulates, and what it does not

Dry-run rehearses the decisions, not the trading. Nothing reaches a venue, so
no venue ever reports on an order. A momentum IRA plans its buys only once
every sell of the rebalance has ended (ADR 0026, point 2, "IRA execution"),
and would wait on an unsent sell for ever, withholding every later month as
`rebalance-in-progress`. So in dry-run, and only in dry-run, the engine
records its own **synthetic** terminal report for each order it releases:
an `execution.order.lifecycle` record at version 3, marked
`"synthetic": true`, status `canceled`, with no venue order id and a message
saying it was never sent. The rebalance then plans its buys at the same
close, and next month is decided normally.

A synthetic report is never an execution. Nothing fills, so no proceeds,
commission or position change is counted, and the buys are budgeted from
the account's cash alone. The holdings never move, so each month's dry-run
decides from the same positions. `journal-query orders` shows the record as
`synthetic-lifecycle-canceled`. Paper and live never record one: there the
venue's own reports end each order. A synthetic report arriving as an input
is refused.

Boost records none, and that has a consequence. It has no buy phase to wait
for, but the gate still counts each released Boost sell as pending until a
venue reports a fill or a cancellation on it, and every later Boost sell is
sized net of the shares still pending (ADR 0026, point 2, step 5). In a
dry-run against an adapter that sends nothing, no report ever comes, so after
Boost's first exit sell (147% back to 98%) that sell's shares stay pending
for the rest of the run: the next exit's sell is smaller than paper would
make it, and none at all once the pending shares reach the holding. The
broker-holding check and the sale back to 98% net the same phantom sell. So
a dry-run rehearses one Boost exit faithfully; read any later exit in the
same run as under-sized (a resumed run replays the same pending sell). (`make
demo-boost-dry-run` does not show this: its simulated account reports a fill
for every released order at the next close, which ends each sell.)

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
written out: `-mode` (from `/etc/trend-investing/engines/<account>.env`,
`TI_MODE`), `-config`, `-socket` (in `/run/ti-engine-<account>`, a folder
only the engine's user can open), `-log` and `-out` (named by `TI_RUN`),
`-as-of` (`TI_AS_OF`) and, for a restart, `-approval` or `-resume`
(`TI_RESTART_ARGS`). You write `TI_RUN`, `TI_AS_OF` and `TI_RESTART_ARGS`
before every start in `/var/lib/trend-investing/engines/<account>/run.env`
(the example is `deploy/etc/engines/run.env.example`; the steps are in
`docs/runbooks/durable-log.md`, "Restarting an engine"). The unit is skipped
until the stamped binary, the account's `.env` and its `.config` are
installed, so the two IRA engines, which have no configuration, never start.

systemd never restarts it by itself (`Restart=no`); restarting is your step
([restart](restart.md)). The engines are not started at boot, and the
provisioner disables any engine an earlier version of it had set to start at
boot. Start the engine before its adapter: the adapter reads the engine's
durable log and refuses to start without it.

When it is up it prints `listening on <socket>`; a fresh run after an earlier
one first prints `recorded the owner's approval of a fresh run after <log>
...`, and a resume `resumed N record(s) from <log>`. Read these with
`journalctl -u ti-engine@<account>`.

## Your instructions while it runs

Started with `-operator <folder>`, the engine reads your kill switch, alert
acknowledgements and rebalance clearances from that folder, which `operator`
writes, and records each in its durable log before its next input from the
adapter ([operator](operator.md)).

**The server's service file does not pass `-operator` yet.** So on the
server, what you record with `operator kill-switch`, `operator acknowledge`
or `operator rebalance-cleared` stays in the folder and never reaches the
engine. Until the unit passes it, the kill switch does not work on the
server: to stop the program ordering, stop the engine
(`sudo systemctl stop ti-engine@<account>`) or act in IBKR's own app
(`docs/runbooks/durable-log.md` names the same gap).

## What can go wrong, and what the program does then

Messages are quoted from the start of what the engine prints (after
`engine:`); the rest of each message names the file or value concerned.
Read them with `journalctl -u ti-engine@<account> -n 20`.

### When it starts

| What you see | What the program does | What you do |
| --- | --- | --- |
| `-mode dry-run refuses an unstamped build ("dev")` (or `paper`, `live`) | Refuses to start; writes nothing. A plain `go build` is unidentified, so its decisions could not be told from another build's. | Build with `deploy/build-engine.sh` (or `make engine` on the Mac) from a committed tree and install it with `deploy/provision.sh`. |
| `-resume refuses build "...-dirty"` | Refuses to resume on a build that names uncommitted changes without its tree fingerprint. | Rebuild from a committed tree; see [restart](restart.md). |
| `-mode X does not match the configuration's mode "Y"` | Refuses: the mode is part of the hashed configuration. | Make `TI_MODE` in `/etc/trend-investing/engines/<account>.env` match the configuration's `mode`. Both are repository files: change them in the repository and run `provision.sh`; a changed configuration is a new run. |
| `-mode live is refused until the limited-live gate` | Refuses: no live order goes out before that gate. | Nothing. Use dry-run or paper ([paper trading](paper-trading.md)). |
| `the configuration in ... is invalid: ... mode paper names account "...", but an IBKR paper account ID begins "DU"` (or the reverse for live) | Refuses: a paper run is bound to a paper account and a live run to a live one. | Correct the configuration's `account_id` or `mode` in the repository, then provision. |
| `the configuration in ... names strategy_id "momentum", a target-weight strategy this engine does not compose` | Refuses: this program runs Boost only; the IRAs' momentum strategy has no engine composition yet. | Nothing; the IRA engines have no configuration on the server ([momentum](momentum.md)). |
| `read the configuration`, `decode the target-weight configuration in ...`, or `the configuration in ... is invalid` | Refuses: the file is missing, is not JSON, has a key the engine does not know, or breaks a rule (each broken rule is listed). | Fix the configuration in the repository and provision it again. |
| `-mode is required`, `-mode must be one of ...`, `-socket is required`, `-config is required`, `-out is required`, `-as-of is required` or `-log is required in -mode ...` | Refuses: there is no default for any of these. The service file always passes them, so on the server this means `run.env` or the account's `.env` lacks a value. | Fill in `TI_RUN`, `TI_AS_OF` (and `TI_MODE` in the `.env`) as the durable log runbook says. |
| The unit fails at once and `systemctl status` says `Failed to load environment files` | The engine never ran: `run.env` is missing. The unit requires it and does not guess. | Write `/var/lib/trend-investing/engines/<account>/run.env` (`docs/runbooks/durable-log.md`, "Restarting an engine"). |
| `systemctl status` says the unit was skipped because a condition was not met | The engine never ran: the binary, the account's `.env` or its `.config` is not installed. | Run `deploy/build-engine.sh`, then `deploy/provision.sh`. For an IRA this is expected: it has no configuration. |
| `-as-of must be an RFC 3339 time` or `-as-of must be nonzero` | Refuses. | Write `TI_AS_OF` as `2026-10-20T20:00:00Z`, say. |
| `-log and -out both name ...` | Refuses: the log and the journal must be two files. | Nothing on the server (the unit names them `<TI_RUN>.log` and `<TI_RUN>.journal`). |
| `... already exists: a durable log is recorded evidence and is never appended to or overwritten`, or `... already exists: a journal is recorded evidence and is never overwritten` | Refuses: `TI_RUN` names a run that already has a log or journal. | Set `TI_RUN` to a new name. Never delete or move the old files. |
| `... so this is a fresh run, and a fresh run decides nothing until the owner's approval is recorded` | Refuses: the folder holds an earlier run, so this start needs your approval. | Write one with `operator approve` and pass it as `-approval` in `TI_RESTART_ARGS` ([restart](restart.md)). |
| `... is the one it opens with ...: resume ... or declare a new start with -as-of`, or `... opens with this run's configuration input ...: declare a new start with -as-of` | Refuses: `TI_AS_OF` repeats the start of an earlier run in the folder. | Choose a new `TI_AS_OF`, or resume the cut-short log; see [restart](restart.md). |
| Any other refusal naming an approval, a resume, a log in the folder, a build or a latch | Refuses to start, naming what does not match. | See [restart](restart.md), "What can go wrong", and `docs/runbooks/durable-log.md`. |
| `the operator inbox ...` or `the operator inbox ... is not a directory` | Refuses: the `-operator` folder cannot be read. | Create the folder, owned by `ti-trader`, or correct the path ([operator](operator.md)). |
| `ti-data refused: ... no bars for trading day ...` or `HTTP <code> from ...` in `journalctl -u ti-data` during the warm-up | The configuration's `warm_up_start` is earlier than the history Massive (the market-data provider) gives this account's plan, so the data job writes no stream and the engine gets no bar to decide from. A bar earlier than `warm_up_start` that does reach the engine is refused (`targetweight: bar ... precedes the warm-up start`). | Move `warm_up_start` (in the repository's `boost.config`) to an NYSE session inside Massive's history, still long enough for the indicators (`ti-data` refuses a history shorter than the longest window). That is a new configuration, so a new run: provision, approve a fresh run, run the warm-up with `ti-data --backfill-from <new date>` ([market data](market-data.md)). |
| `ti-data refused: warm_up_start ... is not an NYSE session` | The data job refuses the configuration. | Pick a trading day for `warm_up_start`, as above. |

### While it runs

| What you see | What the program does | What you do |
| --- | --- | --- |
| `stale-account-snapshot`. Alert: `<account>: decision withheld (stale-account-snapshot)`, urgent, sent once for each run of sessions withheld for this reason | No target is decided for the session and no order is planned: there is no account snapshot as of the session's close, or only one taken after it, and orders are valued at that close (ADR 0026, point 2). Boost's latch (whether the boost is on) still moves, so a change withheld one session is ordered the next. The same holds for a momentum IRA. **On the server today this is every session**, because nothing produces the account snapshot as of the close yet. | Today, nothing: it is the expected state until the snapshot is built ([paper trading](paper-trading.md)). Once it is built, check the adapter is running and connected (`journalctl -u ti-adapter-boost`) and that IBKR answered the account read. |
| `missing-bars`. Alert: `<account>: decision withheld (missing-bars)`, urgent, sent once for each run of sessions withheld for this reason | A session closed without the bar the decision needs, so nothing is decided. **Boost** withholds that session and every later one in the run: its indicators are never computed across the gap. A **momentum IRA** withholds when a held or targeted name, a ranked name or the trend instrument has no bar for the decision session, or a decision session never closed. | Find why the bar is missing: `journalctl -u ti-data` and the data job's stream ([market data](market-data.md)). For Boost the run cannot recover by itself: stop the engine at the end of a session and start a fresh run you approve ([restart](restart.md)), after the data job has the bar. |
| An order breaks a hard limit, or the kill switch or a Degraded or Halted state stops it | The risk gate refuses it. | See [states-and-kill-switch](states-and-kill-switch.md), "What can go wrong". |
| An input refused with a message starting `targetweight:` (a bar out of order, for another instrument, on another price base, or before the warm-up start) | Nothing is decided from that input; the adapter is told why. | Read the adapter's journal: it is a fault in what was delivered, not in the decision. See [IBKR adapter](ibkr-adapter.md) and [market data](market-data.md). |
| `... was given sequence N and not recorded; the run stops, so its log ends at sequence N-1` | The durable log could not be written (the disk is full, or failing). It releases nothing more and stops: a decision is never answered before it is on disk (ADR 0027). The process exits with an error. | Free space (`df -h /var/lib/trend-investing`). The log is intact up to the last recorded input, so resume it as after a crash ([restart](restart.md)); the adapter sends the unrecorded input again. |
| `this decision's deadline passed while it was still running ...; no further decisions will be made` | A decision took longer than `-decision-timeout` (5 seconds). The engine can no longer vouch for its state, so it decides nothing more. | Stop the engine and start a fresh run you approve ([restart](restart.md)). Open an issue: no decision should take that long. |
| `the journal was not written; this run's decisions are not on disk` when it stops | The end-of-run journal could not be written (often a full disk). The durable log still holds every decision. | Free space. The journal is missing, so treat it as a crash: resume from the log ([restart](restart.md)). |
| The process crashes | Stays stopped; nothing restarts it. | Resume it from its log ([restart](restart.md)). |
| The server reboots | A normal shutdown sends SIGTERM, so the engine stops cleanly and writes its journal; it is not started at boot. A power cut or hard reset is a crash instead: no journal is written. | After a normal reboot, a fresh run you approve. After a power cut (`systemctl status` shows it was killed, no journal), resume ([restart](restart.md)). |
| The configuration file is changed while it runs | Nothing changes for the running run: it decides under the configuration it recorded first. The next resume is refused (`the durable log was recorded by run ... not this run`, or `... opens with a different configuration input ...`). The data job refuses too (`the Boost configuration ... is not the one the engine's durable log ... holds`) when the instrument, `warm_up_start` or an indicator length changed. Note that `provision.sh` overwrites the configuration with the repository's. | Change a configuration only in the repository, as a new run: stop at the end of a session, provision, approve a fresh run ([restart](restart.md)). |
| You recorded a kill switch, acknowledgement or clearance and nothing changed | The server's engine is started without `-operator`, so it never reads them (above). | Stop the engine, or act in IBKR's app. |

The reporter's own alerts about a silent engine are in
[alerts-and-dashboard](alerts-and-dashboard.md).

## Try it

`make demo-boost-dry-run` builds a stamped engine, plays the adapter with an
invented SPY series (never market data), prints one line per session (the
target, any order, "released (send false)"), then restarts a second engine
from the first one's log and checks the two logs are identical byte for byte.

## What backs this

- ADRs: 0014 (socket), 0016 (configuration hash and build), 0017 (journal),
  0019 (Degraded and Halted), 0025 (IBKR adapter), 0026 (target-weight
  strategies and the risk gate), 0027 (durable log, restart).
- The synthetic report: ADR 0026, point 3, "Mode" (amended 2026-10-17);
  introduced in target-weight rules version 1.11.0 (ADR 0016; the rules are
  at 1.17.0 now), so a dry-run journal written under an earlier rules
  version does not replay.
- Tests of the withheld sessions, in `internal/targetweight`:
  `TestAMissingBarWithholdsAndNeverComputesAcrossTheGap`,
  `TestAStaleSnapshotWithholdsButTheLatchAdvances`,
  `TestASnapshotAfterTheCloseWithholds`.
- Tests of the synthetic reports, in `internal/targetweight`:
  `TestRulesVersionDeclaresTheDryRunSyntheticReports`,
  `TestADryRunRebalanceThatSellsPlansItsBuysInTheSameRun`,
  `TestOnlyADryRunRecordsSyntheticReports`,
  `TestADryRunCountsNoProceedsOrCommissionFromASyntheticReport`,
  `TestADryRunJournalReplays`; in `internal/journalquery`:
  `TestASyntheticReportIsShownAndCountsNoFill`; in `internal/report`:
  `TestASyntheticReportAcknowledgesNothing`; in `internal/event`:
  `TestLimitOrderLifecycleIsReadAtBothItsVersions`.
- Tests in `cmd/engine`: `TestADryRunEngineRunsBoostAndSendsNothing`,
  `TestTheDeclaredModeMustBeTheConfiguredMode`,
  `TestTheAccountMustMatchTheMode`,
  `TestAnUnstampedBuildRefusesEveryModeButBacktest`,
  `TestTheMakefileStampsTheBuildTheEngineRecords`,
  `TestTheServiceFileStartsTheEngineWithExplicitSettings`; in
  `cmd/boost-demo`: `TestTheDemoDrivesTheRealEngineInDryRun`.

## Not built yet

- The account snapshot as of the close and the recorded mark for Boost:
  nothing produces them, so the engine withholds every session and releases
  nothing ([paper trading](paper-trading.md)). The adapter's main loop, its
  link to the engine's socket, the real IBKR client, the paper sending
  switch and the delivery of the data job's stream are built
  ([IBKR adapter](ibkr-adapter.md)).
- `-operator` in the server's service file: without it your kill switch,
  acknowledgements and clearances do not reach the engine on the server.
- The engine's own reconciliation against IBKR at every start (ADR 0019).
  The adapter matches IBKR's open orders and executions against the
  released orders before it sends anything, but the engine does not compare
  positions and cash at start. The morning reconciliation's engine side is
  built, but nothing delivers its input yet
  ([reconciliation](reconciliation.md)).
- The IRAs: this program refuses a momentum configuration (`compose.go`), and
  no IRA configuration exists ([momentum](momentum.md)).

Re-delivery of released orders after a restart is built: see
[re-delivery](redelivery.md). The README's
[built today / not built yet](README.md#built-today-and-not-built-yet) table
is the one list for the whole program.
