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
session's close) behind one account reading after the close: the account
snapshot, the broker snapshot and the recorded mark (the bar's official
close), with a reconciliation of the same reading. A session is withheld
(`stale-account-snapshot`, below) only when that reading cannot stand for
the close, and a run's first reading that is not the book it is compared
with halts the account instead ([reconciliation](reconciliation.md)).
`make demo-boost-dry-run` shows a full run with an invented account.

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
   - another engine of the instance is running: before it reads your
     approval or any earlier run's log, and before it creates its own log,
     the engine locks `engine.lock` in its state folder (the folder of
     `-log`) and holds it for as long as it runs, whatever log it writes, a
     resume's new log included, so two never run side by side and the
     approval it checks against the latest run stays true while it runs;
     `-log` and `-out` may not name that file; and an
     engine that cannot take that lock at all (a filesystem without
     `flock`) does not start either, leaving no log behind. The IBKR
     adapter's `settle-ended-run` reads the same lock as its proof that a
     run has ended ([re-delivery](redelivery.md));
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
   "the session closed". The adapter delivers the bar and the close
   from the daily data job's file, behind one account reading after the
   close: the account snapshot and the broker snapshot (IBKR's view of the
   account, which the sale back to 98% and the margin alarm read), and the
   recorded mark. When the reading cannot be stamped as of the close, the
   session is withheld (see "What can go wrong"). On the
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
`rebalance-in-progress`; Boost would count an unsent sell as still being
sold, and size every later sell smaller. So in dry-run, and only in dry-run,
the engine, momentum or Boost, records its own **synthetic** terminal report
for each order it releases:
an `execution.order.lifecycle` record at version 3, marked
`"synthetic": true`, status `canceled`, with no venue order id and a message
saying it was never sent. The record comes straight after the release, in
the same decision. A momentum rebalance then plans its buys at the same
close, and next month is decided normally.

A synthetic report is never an execution. Nothing fills, so no proceeds,
commission or position change is counted, and the buys are budgeted from
the account's cash alone. The run's own orders never move the holdings, so
unless something else changes the account (a split, a deposit), each
month's dry-run decides from the same positions. `journal-query orders` shows the record as
`synthetic-lifecycle-canceled`. Paper and live never record one: there the
venue's own reports end each order. A synthetic report arriving as an input
is refused.

For Boost, the synthetic report is what keeps a dry-run's sizing honest.
The risk gate counts a released sell as still being sold until it ends, and
sizes every later Boost sell net of the shares still being sold (ADR 0026,
point 2, step 5). The synthetic report ends each dry-run order at its
release, so nothing of the run's own is ever counted as being sold. Each
exit is then sized from the account as the snapshot shows it, exactly as the
paper account would size it from the same holding once its earlier orders
had ended: the holding less the target, nothing netted. The same goes for
the broker-holding check, and the sale back to 98% nets nothing either,
though it is sized from the latest broker snapshot by its own rule. Nothing
of the run's own fills, so its orders never move the holding; each exit is
sized from the latest snapshot, which, with no split, deposit or other
change to the account, still shows the holding the run started with. A
fill or venue report on a dry-run order is refused, since no venue received
it. A dry-run Boost order is never watched for
a re-plan either: nothing can fill it, so its synthetic cancel is not an
order "ended unfilled" and plans nothing again. A run resumed in the middle
of a dry-run exit decides exactly as the run that was never stopped. (Before
target-weight rules version 1.22.0 Boost recorded no synthetic report, so a
dry-run's first exit sell stayed pending for the rest of the run and every
later exit was smaller than paper's, or nothing.)

Nor does a dry-run Boost run wait for a venue's verdict on its orders: it
lists none as awaiting one (`undecided_orders`), and each decision records
`no_venue: "dry-run"` instead, so its rejection count never moves
([boost](boost.md)).

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
written out: `-mode` (from `/etc/trend-investing/engines/<instance>.env`,
`TI_MODE`), `-config`, `-socket` (in `/run/ti-engine-<instance>`, a folder
only the engine's user can open), `-log` and `-out` (named by `TI_RUN`),
`-as-of` (`TI_AS_OF`) and, for a restart, `-approval` or `-resume`
(`TI_RESTART_ARGS`). You write `TI_RUN`, `TI_AS_OF` and `TI_RESTART_ARGS`
before every start in `/var/lib/trend-investing/engines/<instance>/run.env`
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
`journalctl -u ti-engine@<instance>`.

## Your instructions while it runs

Started with `-operator <folder>`, the engine reads your kill switch, alert
acknowledgements and rebalance clearances from that folder, which `operator`
writes, and records each in its durable log before its next input from the
adapter ([operator](operator.md)).

The server's service file passes it: `-operator
/var/lib/trend-investing/engines/<instance>/operator`, the folder the
provisioner creates, owned by `ti-operator` in the engine's group, mode
`0750`. The engine refuses to start if the folder is missing, if it could
write it itself, or if it cannot read it ([operator](operator.md), "Who can
write an instruction"). The kill switch refuses Boost's orders but not the
sale back to 98%, so before you sell by hand stop the adapter, then the
engine
(`sudo systemctl stop ti-adapter-boost && sudo systemctl stop ti-engine@boost`)
and follow [states-and-kill-switch](states-and-kill-switch.md), "Selling by hand
safely". Stopping the engine alone is not enough: an order it has released
waits in the adapter, and goes out once the Gateway is back the same day. A
fresh run's adapter never sends an earlier run's order: it stops naming it,
and you settle it with `relay.py resolve --run-ended` ([paper
trading](paper-trading.md), "What can go wrong"); one the adapter refuses
for good is settled `--never-sent` ([server](server.md), "Reading the logs
and the kill switch on the server").

## Your deposits and withdrawals

Started with `-cash-ledger <file>`, the engine also reads the Flex reader's
ledger of the account's deposits and withdrawals, and the reader's status
beside it, and records each new record and status in its durable log before
its next input from the adapter, as it does your instructions. The server's
service file passes the account's own: `-cash-ledger
/var/lib/trend-investing/flex/<instance>/cash-movements.jsonl` (through
`TI_FLEX_DIR`). The engine only reads it; a recorded transfer explains the
cash it moved at the next reconciliation ([reconciliation](reconciliation.md),
"Deposits and withdrawals, read from IBKR", which also lists what can go wrong
with it).

## What can go wrong, and what the program does then

Messages are quoted from the start of what the engine prints (after
`engine:`); the rest of each message names the file or value concerned.
Read them with `journalctl -u ti-engine@<instance> -n 20`.

### When it starts

| What you see | What the program does | What you do |
| --- | --- | --- |
| `-mode dry-run refuses an unstamped build ("dev")` (or `paper`, `live`) | Refuses to start; writes nothing. A plain `go build` is unidentified, so its decisions could not be told from another build's. | Build with `deploy/build-engine.sh` (or `make engine` on the Mac) from a committed tree and install it with `deploy/provision.sh`. |
| `-resume refuses build "...-dirty"` | Refuses to resume on a build that names uncommitted changes without its tree fingerprint. | Rebuild from a committed tree; see [restart](restart.md). |
| `-mode X does not match the configuration's mode "Y"` | Refuses: the mode is part of the hashed configuration. | Make `TI_MODE` in `/etc/trend-investing/engines/<instance>.env` match the configuration's `mode`. Both are repository files: change them in the repository and run `provision.sh`; a changed configuration is a new run. |
| `-mode live is refused until the limited-live gate` | Refuses: no live order goes out before that gate. | Nothing. Use dry-run or paper ([paper trading](paper-trading.md)). |
| `the configuration in ... is invalid: ... mode paper names account "...", but an IBKR paper account ID begins "DU"` (or the reverse for live) | Refuses: a paper run is bound to a paper account and a live run to a live one. | Correct the configuration's `account_id` or `mode` in the repository, then provision. |
| `the configuration in ... names strategy_id "momentum", a target-weight strategy this engine does not compose` | Refuses: this program runs Boost only; the IRAs' momentum strategy has no engine composition yet. | Nothing; the IRA engines have no configuration on the server ([momentum](momentum.md)). |
| `read the configuration`, `decode the target-weight configuration in ...`, or `the configuration in ... is invalid` | Refuses: the file is missing, is not JSON, has a key the engine does not know, or breaks a rule (each broken rule is listed). | Fix the configuration in the repository and provision it again. |
| `-mode is required`, `-mode must be one of ...`, `-socket is required`, `-config is required`, `-out is required`, `-as-of is required` or `-log is required in -mode ...` | Refuses: there is no default for any of these. The service file always passes them, so on the server this means `run.env` or the account's `.env` lacks a value. | Fill in `TI_RUN`, `TI_AS_OF` (and `TI_MODE` in the `.env`) as the durable log runbook says. |
| The unit fails at once and `systemctl status` says `Failed to load environment files` | The engine never ran: `run.env` is missing. The unit requires it and does not guess. | Write `/var/lib/trend-investing/engines/<instance>/run.env` (`docs/runbooks/durable-log.md`, "Restarting an engine"). |
| `systemctl status` says the unit was skipped because a condition was not met | The engine never ran: the binary, the account's `.env` or its `.config` is not installed. | Run `deploy/build-engine.sh`, then `deploy/provision.sh`. For an IRA this is expected: it has no configuration. |
| `-as-of must be an RFC 3339 time` or `-as-of must be nonzero` | Refuses. | Write `TI_AS_OF` as `2026-10-20T20:00:00Z`, say. |
| `-log and -out both name ...` | Refuses: the log and the journal must be two files. | Nothing on the server (the unit names them `<TI_RUN>.log` and `<TI_RUN>.journal`). |
| `-log names ...` or `-out names ..., the instance lock an engine holds in the folder of -log` | Refuses before it writes anything: the log or the journal would be `engine.lock`, the file the engine locks for as long as it runs, so the journal could never be installed when it stops. | Nothing on the server (`TI_RUN` names `<TI_RUN>.log` and `<TI_RUN>.journal`); by hand, choose another file name. |
| `... already exists: a durable log is recorded evidence and is never appended to or overwritten`, or `... already exists: a journal is recorded evidence and is never overwritten` | Refuses: `TI_RUN` names a run that already has a log or journal. | Set `TI_RUN` to a new name. Never delete or move the old files. |
| `another engine of this instance runs: it holds .../engine.lock, the instance lock ...` | Refuses before it writes anything: another engine of the same account is running from the same state folder. | Check `systemctl status ti-engine@<instance>` and any engine you started by hand; stop the other one before starting this one. Never delete `engine.lock`. |
| `take the instance lock .../engine.lock: ...: the engine does not start without it ...` (`no locks available`, `operation not supported`), or `open the instance lock ...` | Refuses before it writes anything: the state folder's filesystem cannot hold the lock (a network or unusual mount), or the folder cannot be written. No log is left behind, so the same `TI_RUN` can be used again. | Keep `/var/lib/trend-investing/engines/<instance>/` on the server's local disk with its usual owner and mode (`provision.sh`), then start again. |
| `... so this is a fresh run, and a fresh run decides nothing until the owner's approval is recorded` | Refuses: the folder holds an earlier run, so this start needs your approval. | Write one with `operator approve` and pass it as `-approval` in `TI_RESTART_ARGS` ([restart](restart.md)). |
| `... is the one it opens with ...: resume ... or declare a new start with -as-of`, or `... opens with this run's configuration input ...: declare a new start with -as-of` | Refuses: `TI_AS_OF` repeats the start of an earlier run in the folder. | Choose a new `TI_AS_OF`, or resume the cut-short log; see [restart](restart.md). |
| Any other refusal naming an approval, a resume, a log in the folder, a build or a latch | Refuses to start, naming what does not match. | See [restart](restart.md), "What can go wrong", and `docs/runbooks/durable-log.md`. |
| `the operator inbox ...` or `the operator inbox ... is not a directory` | Refuses: the `-operator` folder is missing, cannot be read, or the engine could write it itself. | Run `provision.sh` again, or fix the folder as [operator](operator.md), "Who can write an instruction", says (`sudo chown ti-operator:ti-trader <inbox>`, mode `0750`). |
| `ti-data refused: ... no bars for trading day ...` or `HTTP <code> from ...` in `journalctl -u ti-data` during the warm-up | The configuration's `warm_up_start` is earlier than the history Massive (the market-data provider) gives this account's plan, so the data job writes no stream and the engine gets no bar to decide from. A bar earlier than `warm_up_start` that does reach the engine is refused (`targetweight: bar ... precedes the warm-up start`). | Move `warm_up_start` (in the repository's `boost.config`) to an NYSE session inside Massive's history, still long enough for the indicators (`ti-data` refuses a history shorter than the longest window). That is a new configuration, so a new run: provision, approve a fresh run, run the warm-up with `ti-data --backfill-from <new date>` ([market data](market-data.md)). |
| `ti-data refused: warm_up_start ... is not an NYSE session` | The data job refuses the configuration. | Pick a trading day for `warm_up_start`, as above. |

### While it runs

| What you see | What the program does | What you do |
| --- | --- | --- |
| `stale-account-snapshot`. Alert: `<account>: decision withheld (stale-account-snapshot)`, urgent, sent once for each run of sessions withheld for this reason | No target is decided for the session and no order is planned: there is no account snapshot as of the session's close, or only one taken after it, and orders are valued at that close (ADR 0026, point 2). Boost's latch (whether the boost is on) still moves, so a change withheld one session is ordered the next. The same holds for a momentum IRA. It happens when the adapter's reading after the close cannot stand for the close; the broker snapshot's `not_as_of_close` names every condition that failed: read before the close or on a later New York date than the close; an open order for the account at IBKR from any client (working, pending or cancel-pending); IBKR's executions not read in full, or one at or since the close; a holding with no official close in the file; IBKR's own net liquidation not above zero; holdings that are not the previous close's adjusted by the fills recorded since; cash more than 1% of the net liquidation at the close (IBKR's own at the read when the account cannot be valued at the close) from the previous close's adjusted by those fills; or a snapshot the engine already holds as of this close or later. Both snapshots are then stamped at the read time. A run's first reading that is not the book it was compared with is different: no snapshot is sent at all, only its reconciliation, and the account **halts** as unverifiable (next row). | Check why in the adapter's log (`journalctl -u ti-adapter-boost`), and that IBKR answered the account read. A session withheld for working orders or a late execution decides normally the next day. |
| A run's first reading is not the book. Alert: `<account>: reconciliation unverifiable (<id>)`, the adapter's log saying the holdings or cash are not the previous close's (or your starting book's) adjusted by the fills | The reading is not the book it was compared with, so the adapter sends its reconciliation alone and no snapshot; the engine, holding no book, finds it unverifiable and the account goes **Halted**. Nothing of that reading is taken as the book. | Read the book in IBKR and start a fresh run whose approval clears the incident and states that book, typed right after reading it (`operator approve -log <this run's log> -account <account> -by owner -clear-incident <id> -positions ... -cash ... -currency USD -out <file>`); never a resume. See [reconciliation](reconciliation.md), "What can go wrong". |
| A close snapshot holds shares the book and the fills since it do not explain (a trade during the session the engine did not place, a fill it was never given, or a journalled fill IBKR does not show). Alert: `<account>: reconciliation unexplained (close-snapshot:<snapshot ID>)` | Before every account snapshot after a run's first becomes the book, the engine compares its holdings with the book in force plus the fills, dividends and splits journalled since. A difference goes **Degraded** on that stock (no buys, the stock frozen) and is recorded with the snapshot as its input; the snapshot is still the book the next decision sizes from. A delisting, merger or symbol change journalled since the book cannot be applied, so the check is unverifiable and the account goes **Halted**, as for any unverifiable reconciliation. Cash is not compared. When the delivery's reconciliation already reported the same holdings, with no fill or corporate action dated between the reconciliation's reading and the snapshot's (whichever came first), nothing more is sent; a Halt is sent unless the account is already Halted for the same change. | Find the trade in IBKR, then recover as for any unexplained difference (Halted: a fresh run whose approval clears the incident, `docs/runbooks/halted-account.md`, "Recover"): [reconciliation](reconciliation.md), "What can go wrong", and `docs/runbooks/reconciliation.md`. |
| `missing-bars`. Alert: `<account>: decision withheld (missing-bars)`, urgent, sent once for each run of sessions withheld for this reason | A session closed without the bar the decision needs, so nothing is decided. **Boost** withholds that session only and decides again at the next session with a bar, its readings computed over the closes it has (the missing close is not filled in), naming the gap (`gap_sessions`); one page per gap ([boost](boost.md)). A **momentum IRA** withholds when a held or targeted name, a ranked name or the trend instrument has no bar for the decision session, or a decision session never closed. | Find why the bar is missing: `journalctl -u ti-data` and the data job's stream ([market data](market-data.md)). For Boost nothing more: the run recovers by itself, so do not start a fresh run for a gap. For a momentum IRA, see [momentum](momentum.md). |
| An order breaks a hard limit, or the kill switch or a Degraded or Halted state stops it | The risk gate refuses it. | See [states-and-kill-switch](states-and-kill-switch.md), "What can go wrong". |
| An input refused with a message starting `targetweight:` (a bar out of order, for another instrument, on another price base, or before the warm-up start) | Nothing is decided from that input; the adapter is told why. | Read the adapter's journal: it is a fault in what was delivered, not in the decision. See [IBKR adapter](ibkr-adapter.md) and [market data](market-data.md). |
| `... was given sequence N and not recorded; the run stops, so its log ends at sequence N-1` | The durable log could not be written (the disk is full, or failing). It releases nothing more and stops: a decision is never answered before it is on disk (ADR 0027). The process exits with an error. | Free space (`df -h /var/lib/trend-investing`). The log is intact up to the last recorded input, so resume it as after a crash ([restart](restart.md)); the adapter sends the unrecorded input again. |
| `this decision's deadline passed while it was still running ...; no further decisions will be made` | A decision took longer than `-decision-timeout` (5 seconds). The engine can no longer vouch for its state, so it decides nothing more. | Stop the engine and start a fresh run you approve ([restart](restart.md)). Open an issue: no decision should take that long. |
| `the journal was not written; this run's decisions are not on disk` when it stops | The end-of-run journal could not be written (often a full disk). The durable log still holds every decision. | Free space. The journal is missing, so treat it as a crash: resume from the log ([restart](restart.md)). |
| The engine's unit failed, and you were paged: a page from the server (title ending `failed`, naming `ti-engine@boost.service`) with the failed run's last journal line as the reason | systemd shows the unit failed and never restarts it. `ti-unit-failed@` paged you within a minute through PagerDuty and your own ntfy server, once for this failure (a restart that fails again before the engine is seen well is the same failure, with a reminder at most once an hour); the reporter's every firing pages it too if that page did not go, and resolves it once the engine is no longer failed. Orders already at IBKR are unaffected | Read the reason on the page and `journalctl -u ti-engine@boost`. A crash is resumed from its log; any other stop is a fresh run you approve ([restart](restart.md); `docs/runbooks/durable-log.md`, "Restarting an engine"). The page's server-side detail is in [alerts and dashboard](alerts-and-dashboard.md), "The reporter itself", the row `<host>: <unit> failed`, and [server](server.md), "A failed unit" |
| The process crashes | Stays stopped; nothing restarts it. | Resume it from its log ([restart](restart.md)). |
| The server reboots | A normal shutdown sends SIGTERM, so the engine stops cleanly and writes its journal; it is not started at boot. A power cut or hard reset is a crash instead: no journal is written. | After a normal reboot, a fresh run you approve. After a power cut (`systemctl status` shows it was killed, no journal), resume ([restart](restart.md)). |
| The configuration file is changed while it runs | Nothing changes for the running run: it decides under the configuration it recorded first. The next resume is refused (`the durable log was recorded by run ... not this run`, or `... opens with a different configuration input ...`). The data job refuses too (`the Boost configuration ... is not the one the engine's durable log ... holds`) when the instrument, `warm_up_start` or an indicator length changed. Note that `provision.sh` overwrites the configuration with the repository's. | Change a configuration only in the repository, as a new run: stop at the end of a session, provision, approve a fresh run ([restart](restart.md)). |
| You recorded a kill switch, acknowledgement or clearance and nothing changed | The engine reads the inbox only before its next input from the adapter, so nothing is read while it is stopped or the adapter sends nothing. | Look for the record's `.delivered` marker in the engine's notes folder ([operator](operator.md), "What can go wrong"). To stop the program ordering now, stop the adapter and the engine and cancel anything at IBKR in IBKR's app, as "Your instructions while it runs" says; then settle any order the engine had released as [server](server.md) says. |

The reporter's own alerts about a silent engine are in
[alerts-and-dashboard](alerts-and-dashboard.md).

## Try it

`make demo-boost-dry-run` builds a stamped engine, plays the adapter with an
invented SPY series (never market data), prints one line per session (the
target, any order, "released (send false)" and the engine's "canceled
(synthetic, never sent)" after it), then restarts a second engine from the
first one's log and checks the two logs are identical byte for byte. Its
simulated account starts holding 14 SPY bought on margin ($4,000 of net
liquidation, 140% invested) and, since a dry-run sends nothing, nothing
ever fills: the first session sells down to 98%, and every Boost exit after
it sells again, sized from the 14 shares held.

## What backs this

- ADRs: 0014 (socket), 0016 (configuration hash and build), 0017 (journal),
  0019 (Degraded and Halted; the instance lock, "A sale an ended run never
  sent"), 0025 (IBKR adapter), 0026 (target-weight strategies and the risk
  gate), 0027 (durable log, restart).
- Tests of the instance lock, in `cmd/engine`:
  `TestTheEngineHoldsItsInstanceLockWhileItRunsWhateverLogItWrites`,
  `TestTheEngineRefusesToStartWhileItsInstanceLockIsHeld`,
  `TestASecondEngineRefusesAtTheInstanceLockBeforeItReadsTheApprovalOrTheRuns`,
  `TestTheJournalAndTheDurableLogNeverNameTheInstanceLock`,
  `TestAnInstanceLockTheFilesystemCannotTakeRefusesTheStartAndLeavesNoLog`.
- The synthetic report: ADR 0026, point 3, "Mode" (amended 2026-10-17);
  introduced for momentum in target-weight rules version 1.11.0 and for
  Boost in 1.22.0 (ADR 0016; the rules are at 1.25.0 now: 1.23.0 also reads a retired order, [order lifecycle](order-lifecycle.md), 1.24.0 keeps a sale back to 98% IBKR has named across a restart, and 1.25.0 keeps one that had ended, so a late fill of it is read, [states and the kill switch](states-and-kill-switch.md)), so a dry-run
  journal written under an earlier rules version does not replay.
- Tests of the withheld sessions, in `internal/targetweight`:
  `TestAMissingBarIsSkippedAndBoostDecidesAcrossTheGap`,
  `TestAGapWhileBoostedNeverBlocksTheExit`,
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
  `TestLimitOrderLifecycleIsReadAtBothItsVersions`. For Boost, in
  `internal/targetweight`: `TestADryRunBoostExitIsSizedAsPaperWouldSizeIt`,
  `TestADryRunBoostRecordsEachUnsentReleaseCanceled`,
  `TestADryRunBoostRestartedMidExitDecidesAsTheFullRun`,
  `TestRulesVersionDeclaresTheDryRunBoostSyntheticReports`.
- Tests in `cmd/engine`: `TestADryRunEngineRunsBoostAndSendsNothing`,
  `TestTheDeclaredModeMustBeTheConfiguredMode`,
  `TestTheAccountMustMatchTheMode`,
  `TestAnUnstampedBuildRefusesEveryModeButBacktest`,
  `TestTheMakefileStampsTheBuildTheEngineRecords`,
  `TestTheServiceFileStartsTheEngineWithExplicitSettings`; in
  `cmd/boost-demo`: `TestTheDemoDrivesTheRealEngineInDryRun`.

## Not built yet

- A paper run against IBKR's paper servers: the account snapshot as of the
  close, the recorded mark, the adapter's main loop, its link to the
  engine's socket, the real IBKR client, the paper sending switch and the
  delivery of the data job's stream are built and rehearsed against a
  stand-in gateway ([IBKR adapter](ibkr-adapter.md)).
- The reconciliations ADR 0019 also asks for after every fill, before every
  order and on a timer; a comparison of open orders; and the joint
  account's cash. The engine's reconciliation does not compare it, because
  Boost's book states none after the first reading; the adapter's own check
  compares it only on a run's first reading, with the stated book or the
  previous close, and afterwards reports the daily cash as drift and
  unexplained cash. The
  ones at every start, reconnect and delivery are built
  ([reconciliation](reconciliation.md), "What is not built yet").
- The IRAs: this program refuses a momentum configuration (`compose.go`), and
  no IRA configuration exists ([momentum](momentum.md)).

Re-delivery of released orders after a restart is built: see
[re-delivery](redelivery.md). The README's
[built today / not built yet](README.md#built-today-and-not-built-yet) table
is the one list for the whole program.
