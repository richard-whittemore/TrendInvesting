# Paper trading: the one mode that sends orders, and only to a paper account

*For the owner. What the program sends in paper trading, what it never
sends, what you will see, and what it does when something goes wrong.*

**Where this stands.** The sending path, the adapter's main loop and its
start command are built and tested against a stand-in for IB Gateway (a
"fake gateway" that answers like IBKR, with invented accounts and prices),
and the whole paper day is rehearsed with the real engine deciding Boost
("The rehearsal", below). No order has yet been sent to IBKR's real paper
servers: that is the hand-run test in `docs/runbooks/first-paper-order.md`,
and it is yours to run. The real IBKR client places an order only when the
adapter's configuration switches sending on (`"paper_sending_enabled":
true`, below); it ships switched off. The adapter delivers each day's market
data to the engine behind one account reading after the close, which stands
for the account as of the close when nothing is working at IBKR, nothing
has executed since and the account is the previous close's adjusted by the
run's own fills (your decision of 2026-10-18; "A paper day", below), so
every input the Boost engine needs to decide a session is now produced.
A paper run starts only with your Run approval, the account's first with the
starting book you checked in IBKR (`operator approve -first-run`; "Starting
the adapter", below).

## What sends

Only an engine and an adapter both started in **paper** mode, on a **paper
account**, send anything. IBKR's paper account numbers begin `DU`; live
account numbers never do. The check is made in four places, and any one of
them stops the order:

1. **The engine** refuses to start if the mode you start it with is not the
   mode in its configuration, or if a paper configuration names an account
   that does not begin `DU`.
2. **The adapter's configuration** is refused the same way: paper must name a
   `DU` account.
3. **The adapter's sender** checks again before every order: it sends only
   on a connection set up from that paper configuration, only an order for
   that same account, and only if the IBKR login the adapter is connected
   to says it manages that account (IBKR sends that list as each session
   starts). If the login does not manage it, the adapter also refuses to
   treat the link as matched to IBKR, so nothing is sent at all.
4. **The real IBKR client** places an order only while the adapter's
   configuration is paper, on a `DU` account, has
   `"paper_sending_enabled": true`, and this session's IBKR login has named
   that account. Left out, the switch is off. It also checks that the
   connection to IB Gateway is open just before and just after it hands
   the order over; if it was closed at either moment the order is never
   counted as sent (see "IB Gateway closed the connection as an order went
   out" below). It must be written exactly
   `true` (not `1` or `"yes"`), and a configuration that sets it in any mode
   but paper is refused before anything starts.

When those all agree, one order goes out like this:

1. The engine decides, checks the order against the hard limits, writes it to
   its durable log, and only then hands it to the adapter marked `send: true`
   (see [engine](engine.md) and [re-delivery](redelivery.md)).
2. The adapter checks the order is one it can send right now: the link to IB
   Gateway is up, IBKR's open orders and today's trades have been matched to
   the orders the engine released (and its balances read in full) since the
   link last came up, the IBKR client in use is allowed to place
   orders (the real one only with `paper_sending_enabled` on), and the order
   is a whole-share limit order for the session (a DAY order) on a stock it
   knows IBKR's contract number for, released in **today's** session (New
   York date). If any of that fails, it sends nothing and records nothing
   about the order, and no order number is used. Whether it can go out
   later depends on why it was refused: see "The link is down" and the rows
   after it below.
3. It writes "about to send" for that order on disk, sends it to IB Gateway
   under IBKR's next order number, then writes "sent". Each order number is
   used once, even if the send failed partway.
4. IBKR's answers come back and are recorded for the engine:
   - when IBKR accepts the order (it is **working**), is **cancelling** it,
     has **cancelled** it, or has **refused** it, with IBKR's own reason;
   - each **fill**: shares, price, IBKR's commission and IBKR's time of the
     trade. A partly filled order shows one fill per execution. A fill is
     recorded only once IBKR's commission report for it arrives, which IBKR
     sends just after the trade. If it has not arrived within the
     configured wait (`commission_wait`, in seconds, in the adapter's
     configuration), the adapter stops rather than leave a trade unrecorded.

   Each report describes the order as IBKR holds it, not as the adapter
   meant it, so the engine can see if the two differ and refuse the report.

## Starting the adapter

Start the **engine first**: the adapter reads the engine's durable log (the
file the engine was given as `-log`) for the run it belongs to, and refuses
to start without it. In paper, **every run starts with your Run
approval**: the account's first run with `operator approve -first-run`
(it names no previous log and states the **starting book**, the holdings and
cash you checked in IBKR; [operator](operator.md), "An approval"), a fresh
run after an earlier one with `operator approve -log` naming the latest
earlier log. The engine is started with `-approval` and records the approval
in its log before it opens its socket (its log says "recorded the owner's
approval of a fresh run after ..."); without one it refuses to start (for a
first run: "decides nothing until the owner's approval states the starting
book"). Then start the adapter. The adapter connects to the engine's socket before it reads the
log or anything at IBKR, and stops ("cannot connect to the engine's
socket") if the engine is not listening yet, so it never reads a fresh
run's log before its approval is in it: read that way, the earlier run's
trades of today would look new and be recorded for the engine a second
time. On the server that is
`sudo systemctl start ti-adapter-boost` after `sudo systemctl start
ti-engine@boost`; like the engine, the adapter is never started or
restarted by systemd on its own, and every start is yours. Stop it with
`sudo systemctl stop ti-adapter-boost`. Why it stopped is in its log:
`journalctl -u ti-adapter-boost`. Account numbers are never written there
(a stop message shows `<account>` instead).

**A dry run the adapter feeds needs the first-run approval too.** The
engine starts a dry-run first run without one, but the adapter's first
reading of the account has nothing to be compared with, and the engine halts
on it ("reconciliation unverifiable"); the account named in the engine's
configuration must also be the one the adapter reads. The ordered steps are in
[the first dry-run day runbook](../runbooks/first-dry-run-day.md).

**Never start a run again without an approval.** In paper the engine
itself refuses: a start over an earlier run's log without `-approval` stops
with "already holds an earlier run's durable log ... decides nothing until
the owner's approval is recorded", and `-resume` is for a crash only. The
case the adapter has to guard against is a dry-run first run, which needs no
approval and is known to the adapter only by its configuration and its
as-of time (an approved run is known by its approval's ID). Started a second
time the same day from the same `run.env`, without `-approval`, it looks
exactly like the first run resumed, and the adapter would treat it so: an
input the first run left waiting for its answer would be asked of the
second run as if it were the same run. With market data delivered
(`--streams`), the adapter does stop such a start, naming both logs
("nothing shows it is that run's crash resume"): the second start's log is
not a copy of the first run's records, record hash for record hash, so it is
never handed the first run's stream folder, nor only the days since its
last file without the warm-up ([market data](market-data.md), "A crash
resume"). If the first run crashed, resume it ([the durable log
runbook](../runbooks/durable-log.md), "Restarting an engine"); any other
restart is a fresh run, started with its Run approval.

The command the unit runs, which you can also run by hand from the
directory that holds the adapter's code (`/opt/trend-investing/adapter` on
the server, the repository checkout on the Mac):

```sh
python3 -m adapter.ibkr run \
  --config /etc/trend-investing/adapters/boost.json \
  --secrets <account file> --account-key IBKR_ACCOUNT_BOOST \
  --pending /var/lib/trend-investing/adapters/boost/pending.json \
  --engine-log /var/lib/trend-investing/engines/boost/<TI_RUN>.log \
  --socket /run/ti-engine-boost/engine.sock \
  --streams /var/lib/trend-investing/data/streams/boost \
  --ibapi-record /opt/trend-investing/twsapi/INSTALLED \
  --versions /opt/trend-investing/adapter/versions.env
```

`python3 -m adapter.ibkr run --help` lists every option. What each names:

| Option | What it is |
| --- | --- |
| `--config` | The adapter's configuration, `/etc/trend-investing/adapters/boost.json` on the server, created once from `deploy/etc/adapters/boost.json.example` and never overwritten. It ships in **dry-run with sending off** and with no contract numbers (`"con_ids": {}`). For the paper test set `"mode": "paper"`, `"paper_sending_enabled": true`, and `"con_ids": {"SPY": <SPY's contract number from IBKR's contract search>}`. It holds no account number |
| `--secrets`, `--account-key` | Where the account number comes from: the `IBKR_ACCOUNT_BOOST=` line of the adapter's own account file, `/etc/trend-investing/accounts/boost.env` (root-only, mode 0600; the unit hands the adapter a private copy of that file and nothing else, so it never sees another account's number or the alert URLs). Write it `IBKR_ACCOUNT_BOOST=DU...` with no quotes and no spaces: a value with either is refused ("is not KEY=VALUE with a bare value"). The account is named in exactly one place: either here, or as `account_id` in the configuration (then leave both options out); both, or neither, is refused |
| `--pending` | **The pending file**, `/var/lib/trend-investing/adapters/boost/pending.json` on the server. This is the `PENDING_FILE` every `relay.py` command in this manual and in [re-delivery](redelivery.md) takes. The adapter's other files sit beside it, named after it: `.handover` (the "about to send" and "sent" marks), `.handover.decisions` (your decisions), `.refused`, `.not-handed-over`, `.run` (which engine run the waiting input was sent to), `.run-ended` (inputs you settled as left by an earlier run), `.corrections`, `.late-fills`, `.lock`, `.delivered` (the data files handed over, [market data](market-data.md)), `.basis` (the earlier runs' closing books, kept so each reading after the close need not walk their logs again; a file that does not read is only a warning in the adapter's log and is walked again and replaced), `.outbox` (inputs recorded for the engine and not yet sent; anything that adds to it, the adapter or another program, does so only under the lock on `.outbox.lock`, so neither loses the other's input), `.outbox.set-aside` (queued entries the adapter could not have written that you set aside with `resolve --run-ended --set-aside-malformed`, [re-delivery](redelivery.md); kept, never sent) and `.held` (a cancellation waiting for a fill's commission). Never edit them |
| `--engine-log` | **The engine's durable log**, `/var/lib/trend-investing/engines/boost/<TI_RUN>.log`, the `ENGINE_LOG` the acknowledgement commands take. The unit fills `<TI_RUN>` in from the same `run.env` the engine starts from |
| `--socket` | The engine's socket, `/run/ti-engine-boost/engine.sock` |
| `--streams` | The folder of the data job's Boost streams, `/var/lib/trend-investing/data/streams/boost`. The adapter delivers the run's own folder inside it, named `<TI_RUN>` like the engine's log, and refuses to start until that folder exists (the data job's warm-up writes it). After a crash resume, a new `TI_RUN` carrying the crashed run's identity, it keeps delivering the folder of the run that began it, as the data job keeps writing it, and carries on with the batch it had written down ([market data](market-data.md), "A crash resume"). Left out, no market data is delivered ("A paper day", below) |
| `--ibapi-record`, `--versions` | The adapter starts only if IBKR's `ibapi` installed by `provision.sh twsapi` is the build pinned in `versions.env` (below) |

What the adapter does, in order:

1. Checks the configuration, the account number and the `ibapi` build;
   refuses to start on any of them.
2. Connects to IB Gateway with the engine's own client ID, reads the
   account (positions, cash, net liquidation, excess liquidity and every
   open order) and IBKR's open orders and today's executions. If any of
   that could not be read, it stops ("could not be read in full") rather
   than send anything on a partial picture. It then matches every open
   order and every trade of today in the account against the orders the
   engine released to be sent, by client order ID under the engine's own
   client ID. One that none of them explains (an order you placed by hand,
   one from another program or client ID, or one no run of this engine
   released) stops it ("no order the engine released explains") before
   anything is sent. The orders of the earlier runs that the Run approvals
   name count as the engine's own, so a fresh run started the same day as
   a fill starts normally. Only the runs IBKR's lists can still show are
   read: the adapter follows the approvals back and stops at the first
   earlier run whose last input is from before yesterday's session (New
   York date; yesterday's session is the weekday before today, so Friday's
   on a Saturday or a Monday) and every order it released has ended (a
   cancellation or refusal recorded, or fills for its whole quantity, in
   its log or a later run's, or settled by the adapter's own records: an
   order you settled as never sent or not handed over, one the adapter or
   you retired, one a `--run-ended`
   settlement left unsent (one it names as being handed over may have
   reached IBKR: if IBKR still shows it once its run is no longer read, the
   adapter stops on it, "no order the engine released explains"), or one
   whose cancellation or fills were settled with that input or struck off
   at that settlement), and it never reads the logs before that one.
   Every run of today's or yesterday's session is read, even one that
   released nothing, and an earlier day's run with an order not ended is
   read too, and the walk goes on past it: a DAY order sent after the
   close works the next session. A dry-run run's own made-up reports
   ("synthetic") are never taken as IBKR's.
   Each approval names the earlier log's final record hash, and the
   adapter checks it against that log ("final record hash"): a log that
   differs is not the one you approved.
   Only then does it treat the link as matched ("[reconciled](glossary.md)" in its log: the adapter's matching, not the morning reconciliation).
   What this does not check: a released order IBKR no longer shows (a DAY
   order ended or filled on an earlier day is in neither list, and the
   order about to be sent is checked again at IBKR on its own), and
   positions and cash, which it reads in full but leaves to the engine's
   reconciliation. It does the same after every reconnect.
3. Asks the engine again for the reply to any input it was waiting on when
   it last stopped, before anything new.
4. Then, until you stop it: reads IB Gateway's messages one at a time,
   checks after each one whether a trade's commission report is overdue
   (`commission_wait`; it stops on time, not only when IBKR next writes),
   and sends the engine each report it records, oldest first. A released
   order marked to send goes out then, once. While it waits for the
   engine's reply it keeps reading IB Gateway's messages, so a slow reply
   neither drops a healthy link nor makes a commission that has arrived
   look missing.
5. With `--streams`, also delivers each new file the data job writes for
   the run, behind a fresh broker snapshot ("A paper day", below).

### Installing IBKR's `ibapi` on the server

```sh
sudo deploy/provision.sh twsapi    # fetch, verify, install ibapi, record it
sudo deploy/provision.sh adapter   # install the adapter's code and its pin file
sudo deploy/provision.sh services  # the unit, the adapter's directory and configuration
```

`twsapi` downloads IBKR's TWS API 10.51.01 installer and the pinned
protobuf 5.29.5 wheel (`ibapi` imports it; a dependency proposed to the
owner 2026-10-20), refuses either unless its SHA-256 matches
`deploy/versions.env` (nothing is installed on a mismatch), runs it as an unprivileged user
(never as root) into a scratch directory, copies only its Python client
into a root-owned directory, installs that client into the adapter's own
environment (`/opt/trend-investing/adapter-venv`, offline, protobuf first),
checks that both import, and only then writes `/opt/trend-investing/twsapi/INSTALLED`. Run
`adapter` again after every pull you mean to run. If the pin in the
adapter's code ever differs from what was installed, or the `ibapi` the
adapter's Python actually imports, or its protobuf, is another version
(or does not import at all), the adapter refuses to start ("install the pinned build with
`sudo deploy/provision.sh twsapi`"). Neither step has been run on the
server yet; if `twsapi` stops with "has not exactly one
source/pythonclient", IBKR changed the installer's layout: stop and report
it, do not install `ibapi` by hand.

### The rehearsal: run it before the first real order

```sh
deploy/rehearse-paper.sh --account
```

From a checkout, on the machine that will run the test. It asks for your
paper account at a hidden prompt (leave `--account` off for a synthetic
one), builds the real engine (`make engine`) and runs synthetic paper days
with it, in paper mode, deciding Boost: the data job writes the run's
warm-up and each day's file from invented Massive prices, and the adapter's
own start command, with sending switched on, delivers them to the engine
through the fake gateway. It prints a `PASS` line for each of:

- the account's first run beginning with your approval stating its starting
  book (the engine's second input), as a paper first run must;
- the data job's warm-up, stamped with the engine's own identity;
- the warm-up delivered the evening before the first day behind one
  account reading as of that evening's close, with the start-of-run
  reconciliation, which the engine matches;
- the day's account and broker snapshots as of the close (read after it),
  its bar, its mark from the bar's official close, then its close;
- the engine's decision and its release of one buy, sent once, only after
  it, as a whole-share SPY DAY limit order for your account tagged with its
  client order ID, and IBKR's acceptance reaching the engine;
- a dropped link and a restart, each reconciled again and matched by the
  engine, sending nothing twice;
- the next day's fills, after the close, so that day's reading is stamped
  at its read time with the reason and the engine withholds the session
  (`stale-account-snapshot`), ordering nothing;
- a crash resume of the engine under a new run name, which takes the next
  day's file from the run's own folder, once, and decides it;
- a holding no order explains, found at the next start's reconciliation,
  taking the engine out of Normal (Degraded on SPY).

It ends "rehearsal passed: nothing was sent to IBKR", or exits with `FAIL:`
and the reason. Nothing reaches Massive or IBKR and your account number is
never printed.

### Turning sending on, and off again

The order reaches IBKR only when all of these hold: the configuration is
paper with `"paper_sending_enabled": true`, the engine runs in paper, and
IB Gateway's **Read-Only API is off**. Turn Read-Only off just before the
first order and back on afterwards, as the runbook says. To stop sending
without stopping the engine: stop the adapter, set
`"paper_sending_enabled": false`, and start it again. An order released
while sending is off is refused before "about to send" is written ("does
not place orders"), and the adapter stops on it at every start: switch
sending back on and start the adapter the same day, and it goes out; on a
later day its session is over and the adapter retires it by itself (the row
"The link stayed down past the end of the order's day" below).

## A paper day

What happens on a trading day once the engine, the adapter and the data
job are running (New York times):

| When | What happens | Where you see it |
| --- | --- | --- |
| All day | The adapter keeps the link to IB Gateway up and passes IBKR's reports on any working order to the engine | `journalctl -u ti-adapter-boost` |
| 4:00 p.m. (1 p.m. on an early close) | The market closes | |
| Next morning, 5:30 a.m. (again at 6:30 and 7:30) | The Flex reader asks IBKR for the account's cash transactions and its Trade Confirmations, and writes the **executions proof**: every execution IBKR lists, up to midnight ([reconciliation](reconciliation.md), "The executions proof") | `journalctl -u ti-flex@boost`; `executions.json` beside the Flex ledger |
| 6:05 a.m. (again at 7:05 and 8:05) | The data job records SPY's settled session from Massive (its free plan serves a day's prices only the next morning) and writes the day's file, `boost-<date>.jsonl`, in the run's stream folder | `journalctl -u ti-data` |
| Within seconds | The adapter reads the account at IBKR once, checks that the reading can stand for the close (below), and sends the engine, in this order: its account snapshot, the broker snapshot, the day's SPY bar, SPY's mark, then the session's close ([market data](market-data.md), "What happens to the file"). Until 9:00 it waits for the executions proof if it is not there yet | the adapter's log: `ibkr broker snapshot read_at=... as_of=...`, `ibkr stream queued ...`, then one `answered with` line per input |
| At the close's input | The engine decides the session. If it releases an order, the adapter sends it to IB Gateway once, as a DAY limit order for the day it is sent (the morning's session), as before | the adapter's log: `ibkr sent client_order_id=...`; the engine's journal (`journal-query orders`) |
| Next session | IBKR works the order; its acceptance and fills reach the engine as they happen | `journal-query orders`, `journal-query fills` |

**The reading after the close stands for the close** (your decision of
2026-10-18; ADR 0019, ADR 0025 point 5 and ADR 0026 point 1). The engine
decides a session from the account as it was at that session's close. The
adapter reads the account after the close, when the day's file arrives:
the next morning, on Massive's free plan.
Between the close and the next open the account changes only through an
order still working, an execution, a corporate action IBKR books overnight
(a split, say) or a cash posting (interest, a fee, a dividend). The adapter
checks for each, so when, at the moment of the read:

- the read is after the close and on the same New York date (IBKR's list of
  executions reaches back only to midnight, so only then does it cover
  everything since the close), **or** (your decision of 2026-10-27) on a
  later date before the next session's open, with the Flex reader's
  **executions proof** listing every execution of the account from the
  session's day up to the midnight that starts the reading's day and none at
  or after the close, and no fill recorded at or after the close, in this
  run or any earlier run its approvals name: the
  proof covers the close to midnight, IBKR's own list midnight to the read
  ([reconciliation](reconciliation.md), "The executions proof");
- IBKR shows no open order for the account, from any client (working,
  pending or cancel-pending), in either read of the account or in the last
  list of open orders it returns;
- IBKR reports no execution in the account at or since the close, from
  any client it shows to the adapter (see the Master API client ID, below);
- every holding has the day's official close in the file, so the account
  can be valued at the close, and IBKR's own net liquidation is above zero;
  and
- **with no help from any client ID**, the holdings are exactly the
  previous close's plus or minus the fills since, and the cash is the
  previous close's adjusted by those fills, give or take at most **1% of
  net liquidation** (room for a day's interest, a fee or a dividend; the
  difference is recorded on the broker snapshot as `cash_drift`, and that
  difference added up since the cash was last explained as
  `unexplained_cash`: for Boost, whose book states no cash, since your
  starting book; the daily summary shows both, so a small posting day
  after day is still seen). A split, a transfer, a large posting or a trade
  made by a client IBKR does not show the adapter fails this check. The
  previous close is the last account snapshot of this run or of the earlier
  runs its approvals name, or the **starting book** you stated when you
  approved the account's first run (`operator approve -first-run`), so a
  fresh run's first delivery is compared with the last run's close and the
  first run's with your book; a first delivery that does not match is never
  taken as the book (the engine halts on its reconciliation instead),

the one reading is the account as of the close. The adapter then states
both snapshots "as of" the close (4:00 p.m.) and records the real read time
beside the broker snapshot as evidence (`read_at`). Otherwise it stamps
both with the real read time and writes every reason into the broker
snapshot (`not_as_of_close`); the engine finds no snapshot as of the close,
withholds the session (`stale-account-snapshot`) and orders nothing, and
the reporter pages you once, naming the reason
([alerts](alerts-and-dashboard.md)). The target in force stays as it was;
the next session that passes decides as usual.

One residual you cover by a setting: a sale and a repurchase of the same
shares by another client after the close leave the holdings unchanged and
move the cash by little, so only IBKR's list of executions shows them, and
IBKR shows other clients' executions only to the Gateway's Master API
client ID. Set it to the adapter's client ID (below, and the runbook's A4);
the unexplained cash in the daily summary is your check on it.

**Set the Gateway's Master API client ID to the adapter's.** IBKR shows an
API client the executions of other clients, and of TWS or IBKR's own app,
only when that client's ID is the Gateway's "Master API client ID". Set it
to Boost's client ID, 11 (the [first paper order runbook](../runbooks/first-paper-order.md),
step A4, on the Mac mini and the server; [the IBKR adapter](ibkr-adapter.md)), so the list the adapter
reads, which asks for every client's executions, has them all. The cash and
holdings check above does not depend on it: a trade the adapter cannot see
still changes the holdings, and the reading is then stamped at its read time.

**What the snapshots say, and at what prices.** One price basis per rule
(ADR 0019, amended 2026-10-18). A reading as of the close is valued **at the
close**: the engine's account snapshot holds IBKR's positions and a net
liquidation of the account's cash plus SPY at the day's official close, the
price Boost's orders are valued at; the broker snapshot holds the same
holdings marked at that close, the same net liquidation, IBKR's own net
liquidation at the read beside it (`read_net_liquidation`), Excess
Liquidity as IBKR read it, every open order, and whether the market was open
at its stamp (at the close it is not). The sale back to 98% is sized from
the broker snapshot's net liquidation and mark, both at the close; the
margin cushion compares Excess Liquidity with IBKR's own net liquidation at
the read, both IBKR's figures from the same moment. A reading stamped at its
read time is IBKR's own reading throughout: its net liquidation, and each
holding marked at IBKR's last price at the read (no mark when IBKR gives
none).

**The mark is the day's official close.** SPY's mark input
(`market.mark.recorded`) is the delivered bar's official close: the number
the backtest marks at. IBKR's last traded price is read too and recorded
beside it (`broker_last`). The engine's price band compares a buy's
reference price, the previous close, with IBKR's last price when IBKR gave
one, and with the official close otherwise, and says which on the order
(`mark_source`). **The band has teeth only when IBKR provides a price**:
without one it compares the close with itself and passes, and the order's
limit price is the only bound on the fill. This is the orchestrator's reading
of your decision, put to you (ADR 0026, point 3, amended 2026-10-18).
Nothing is sized from IBKR's price, so **no decision needs IBKR market-data
permissions** on the paper login.

**Reconciliation with every reading.** Every day's delivery also sends
IBKR's positions and cash from the same reading. Once the engine holds a
book it goes first, so the engine compares IBKR's holdings with the book it
holds and the fills recorded since **before** the day's snapshot replaces
that book: shares that no fill explains are unexplained, never quietly
taken over. At a run's first delivery, before the engine holds a book, it
goes behind the snapshots, for the engine to compare against the book it has
just recorded. At every later start of the adapter and every reconnect it
sends them again, read only once every trade IBKR replayed has its
commission and its fill has reached the engine, and before any of the day's
market data. A difference no journalled fill or corporate action explains
takes the account out of Normal (ADR 0019: Degraded on the instrument, or
Halted when it cannot be compared), and you are paged
([reconciliation](reconciliation.md)).

**Keep the adapter and the Flex reader running.** The day's file arrives
the next morning, so the reading is on a later date than the close: it
stands for the close only with the executions proof, which needs the Flex
reader's Trade Confirmation query ([reconciliation](reconciliation.md),
"What you set up once", step 4). Without the proof by 9:00 New York the
session is withheld and nothing is ordered for it, and the page says which
proof was missing. An order the engine releases is sent the morning it is
released, for that day; one not sent that day is never sent: its session is
over, so the adapter retires it (the row "The link stayed down past the end
of the order's day", below).

**The first paper day.** Start in this order: the engine; the data job's
one-time warm-up ([market data](market-data.md), "The one-time warm-up"),
which writes the run's first file and its folder; then the adapter, which
refuses to start until that folder exists ("the run's stream folder ...
does not exist"). It delivers the warm-up straight away, behind one
reading, with the start-of-run reconciliation. Do this **the evening before
the first paper day** (any time before that day's file, written the
next morning from 6:05 a.m.): the warm-up's reading is stamped with its
own close, or its read time, and the first day's snapshot must come after
it. From then on the timer writes each day's file the next morning, from
6:05 a.m.

**What can go wrong.** Each of these stops the adapter with a message
naming the file, and nothing from that file is sent; the settling steps are
in [market data](market-data.md), "What happens to the file": a file not as
the data job writes it, a file of another run or configuration ("it is not
this run's"), a file older than one already delivered, two waiting files
that overlap, or the adapter's record of delivered files
(`pending.json.delivered`) changed by something else. An account IBKR
cannot read in full for the snapshot three times running stops the adapter
too, with the file left in place. A link that drops while the snapshot is
read stops nothing: the adapter waits, and delivers once the link is back
and matched.

**Not built yet.** Nothing further is needed before the first real paper
order. Still to come, and not needed for it:

- the after-fill, pre-order and scheduled reconciliations (ADR 0019): only
  the start, reconnect and daily-delivery ones are sent;
- the IRA accounts' inputs: only Boost's stream is delivered.

## What never sends

- **Dry-run.** Every order is marked "do not send" and the adapter never
  sends it.
- **Live.** The engine refuses to start in live mode, and the adapter refuses
  to build a live sender, until the limited-live gate (plan section 9,
  Phase 5). That is a separate change, reviewed on its own.
- **A paper configuration pointed at a live account**, or the reverse: both
  are refused before anything starts.
- **An order sent before.** The adapter never sends the same order (its
  client order ID, which IBKR shows as `orderRef`) twice, after a reconnect
  or a restart; see below.
- **An order released in another mode** than the adapter's, or one not
  marked to be sent.

## What you will see

- In the engine's log: each order released with `mode: paper` and
  `send: true`.
- In the adapter's records: the order handed over, then IBKR's reports on it
  (working, cancelling, cancelled or refused) and each fill with its
  commission. The engine reads these the same way it will read live ones.
- In IBKR's paper account (Client Portal or the mobile app, switched to the
  paper account): the order, with your client order ID in its order
  reference.

## What can go wrong, and what the program does

| What you see | What the program does | What you do |
| --- | --- | --- |
| The adapter refuses to start: "the time zone US/Eastern that IBKR's execution times name is not installed" | IBKR states every execution time in `US/Eastern`, a name Ubuntu 24.04 ships only in the `tzdata-legacy` package. The adapter checks it can read that zone before it connects, so it refuses to start rather than stop at the first trade. Nothing is sent. | Run `sudo deploy/provision.sh base` (it installs the package; safe to repeat), then start the adapter again. |
| The link to IB Gateway drops after an order was sent | It reconnects with the same client ID, then matches IBKR's open orders, trades and balances before anything else is sent. The order already sent is not sent again. | Nothing, unless the reconnect gives up; then see `docs/runbooks/lightsail-deployment.md`. |
| The adapter stops after IBKR took the order but before it wrote "sent", and the order is still working or has traded | On restart it finds the order in IBKR's open orders or trades and does not send it again. | Nothing. |
| The adapter stops after IBKR took the order but before it wrote "sent", and IBKR cancelled the order (or it expired) before the restart | A cancelled order is in neither IBKR's open orders nor its trades, so on restart the adapter cannot tell it from an order that never arrived. It stops with "was being handed to the broker when the adapter stopped" and sends nothing more. | Look in IBKR's order history (Client Portal or the mobile app, switched to the paper account: orders, including cancelled ones) for an order whose order reference is the client order ID named. If it is there, cancelled, it reached IBKR: settle it with `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "cancelled at IBKR"`, and the next rebalance places a fresh order if one is still wanted. Never use `--resend` for an order IBKR shows at all. The full procedure is in [re-delivery](redelivery.md), "What can go wrong". |
| The adapter stops mid-send and IBKR shows no trace of the order | It stops with "was being handed to the broker when the adapter stopped" and sends nothing more. | Follow [re-delivery](redelivery.md), "What can go wrong": look in IBKR first, then settle it with the resolve command. |
| The link is down, or not yet matched to IBKR, or IBKR has not yet given the session an order number, when an order is due (including a connection IB Gateway closed that the adapter has not yet heard about) | The order is refused before "about to send" is written, and nothing goes out. The refusal and its reason are kept in the file `PENDING_FILE.refused` until the input is settled. It waits: the adapter asks for it again on every exchange. That check is the only link check before "about to send"; a connection IB Gateway closes in the instant after it is caught by the real client's own check just before it hands the order over, which stops the adapter instead (the row "IB Gateway closed the connection as an order went out", closed before the hand-over). | Nothing, if the link is back and matched the same day (New York date): it goes out then. Do not settle a link-down refusal with `--never-sent`: the order goes out once the link is back. |
| The link stayed down past the end of the order's day | The order was for the session it was released in; its limit and the engine's checks were made for that day. On any later day it is refused before "about to send" is written ("was released for the session of ... not today's ... its session is over"), so it is never sent. By the owner's decision of 2026-10-22 the adapter then **retires it by itself**: once the link is back and matched, it checks IBKR, and if IBKR shows nothing under the order's client order ID it records an `execution.order.retired` input for the engine (reason `session over`, retired by `adapter`), records the retirement in `PENDING_FILE.handover.decisions`, and goes on to its next order and input. It is a refusal of an order that never left the adapter, never a cancel at IBKR. The engine reads it as an order that ended with nothing filled and re-plans at its next decision; a Boost sale back to 98% retired so is tried again by its own rule, as a new order, since its one attempt was never used. You get one routine message, "order ... retired (session over)" ([order lifecycle](order-lifecycle.md)); you are not paged for the retirement (the order may already have been paged as "not acknowledged" while the link was down). If IBKR does show an order under that client order ID, it was sent after all: the adapter takes it as sent and retires nothing. | Nothing for the order. Find out why the link stayed down (see "The link is down", above). If IBKR later shows an order under that client order ID, the adapter stops with "was settled as never sent, but IBKR shows": mark it `--sent` as [re-delivery](redelivery.md) says and reconcile. |
| An order the adapter can never send as it stands: not a limit order, a fraction of a share, or no IBKR contract number configured for the stock ("can never be sent as it stands") | It is refused before "about to send" is written, and the adapter **stops at that order**, every time it is started, sending nothing more: it will not cure itself. It records the order in `PENDING_FILE.refused`. Because the order never reaches IBKR, the reporter pages you urgently, "order ... not acknowledged", within about 10 minutes ([order lifecycle](order-lifecycle.md)), and the adapter's stop names the order and the command to run. | Find the cause. For a missing contract number: add the stock to `con_ids` in the adapter's configuration (the engine's instrument to IBKR's contract number, looked up in IBKR's contract search, never guessed by name); a change of configuration is a new run, so the order stuck here still has to be retired. A market order or a fraction of a share is a fault in the engine or the adapter: report it. Then, with the adapter stopped, retire the order: `sudo -u ti-trader python3 adapter/ibkr/relay.py retire PENDING_FILE CLIENT_ORDER_ID --by <your name> --reason "what was wrong and what you changed"` ([operator](operator.md), "Retiring an order the adapter can never send"). It records an `execution.order.retired` input for the engine with your name and reason (reason `owner retired`), and when you start the adapter again it goes on to its next input, after checking IBKR once more. The engine re-plans at its next decision; you get one routine "order ... retired (owner retired)" message. If the command stopped part way (a crash, a full disk), start the adapter anyway: it finds the retirement begun and finishes it before anything else, and never sends that order, even once the cause is fixed. `resolve ... --never-sent` is refused for this order, because it would not tell the engine. Any other refusal of an order (the login does not manage the account, IBKR has given no order number, sending is off) is not retired: fix it as its own row says. |
| The adapter stops: "no order the engine released explains" | At start or after a reconnect, IBKR shows an open order or a trade of today in the account that is not one of the orders the engine released under its own client ID: one you placed by hand, one placed by another program or client ID, or one no run of this engine released. An order or trade of an earlier run of this engine is explained, not a stop: after a fresh run starts the same day (a reboot, a build upgrade), the adapter reads the durable logs of the runs the Run approvals name, back to, but not including, the first run from before yesterday's session (the weekday before today) whose orders have all ended (an earlier day's run with an order still working is read), and records none of their reports again. The link is not treated as matched, and nothing is sent. The stop message lists each one by its order reference, client ID and IBKR order number (never the account number). | Look at each in IBKR. Cancel any order that should not be there, and reconcile the account ([reconciliation](../runbooks/reconciliation.md)) before starting again. Do not place orders by hand in the engine's account while it runs. One gap: "yesterday's session" is the weekday before today, not the exchange's calendar, so on the day after a weekday holiday (a Tuesday after a Monday holiday, say) an order an earlier run released after the close of the session before the holiday can still be working while its run is no longer read, and it is listed here although this engine released it. Its order reference is a client order ID in that earlier run's durable log (search the log for it). If it is there, cancel it at IBKR (it is a DAY order from the session before; the fresh run has no record of it) and start the adapter again. |
| IB Gateway closed the connection as an order went out | The real client checks the connection just before and just after it hands the order to IB Gateway. Either way the order is not counted as sent: "about to send" stays written, and the adapter stops and sends nothing more. Closed before ("IB Gateway's socket was closed before placeOrder, so nothing was handed to IB Gateway"): the order never left the adapter, so IBKR cannot have it, and the adapter records that in `PENDING_FILE.not-handed-over`. Closed during ("... closed during placeOrder, so whether IBKR has the order is not known"): it may or may not have reached IBKR, and nothing is recorded. | Closed before, the same day (New York date) the order was committed, if it should still go out: with the adapter stopped, `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --resend --reason "socket closed before placeOrder"`; it goes out once the link is back and matched on that same day. If the link is back only on a later day, the adapter stops again (the permission is dated: "resend" is allowed only on the day it was given) and the order is settled with `--not-handed-over`, below. Closed before, on a later day (a resend is refused then, because the order's day has passed), or if it should not go out: look in IBKR's order history (orders, including cancelled ones) and confirm no order carries that client order ID as its order reference, then `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --not-handed-over --reason "socket closed before placeOrder; IBKR shows no order"`. The command accepts it only for the order the adapter recorded in `PENDING_FILE.not-handed-over` for the input it stopped on, and refuses it for an order stopped mid-send. It is kept as never sent, and the adapter goes on when started again after checking IBKR once more, as for `--never-sent` (if IBKR shows the order after all it stops with "was settled as never sent, but IBKR shows"; then mark it `--sent`). The engine still holds the order as released, because nothing at IBKR will ever report on it: a momentum engine withholds the next rebalance (`rebalance-in-progress`) until you clear it with `operator rebalance-cleared` naming that rebalance and the order ([operator](operator.md), "A rebalance clearance"), which is safe because IBKR never received it; Boost reads that clearance only to stop waiting for IBKR's word on the order, so for Boost reconcile the account ([reconciliation](../runbooks/reconciliation.md)) and start a fresh run as [the durable log runbook](../runbooks/durable-log.md), "Restarting an engine", says. Never use `--never-sent` here: it is refused for an order marked "about to send". Closed during: as for any stop mid-send, look in IBKR for an order whose order reference is the client order ID named, then settle it with the resolve command ([re-delivery](redelivery.md), "What can go wrong"). |
| The adapter stops: "carries other content than the input the engine committed" | An input waiting in `PENDING_FILE.outbox` has the same ID as one the engine already holds, but different content. It is neither sent nor dropped. | A fault in whatever recorded the input (an ID reused for another input): compare the two, stop and investigate before starting again. |
| IBKR's login does not manage the configured account | The adapter refuses to treat the link as matched to IBKR ("is not among the accounts this IBKR login manages"), so nothing is sent. | Check the account number in the adapter's configuration against the paper account the Gateway logs in to. A wrong account is a new configuration and a new run. |
| IBKR refuses the order | It records the refusal with IBKR's reason, and the engine reads the order as ended. | Read IBKR's reason. A margin or permission refusal in paper usually means a paper account setting to change. |
| IBKR reports on the order in a form the engine cannot read exactly (part shares, a price that is zero or not a number, a commission in another currency, negative, not a number, or as large as the order itself, an order it never showed) | The adapter stops rather than record an approximation. | A fault to investigate before starting again; check the order in IBKR. |
| IBKR corrects a fill (it reports the same trade again under an execution ID that differs only in its last part) | The adapter stops rather than record the correction as a second fill, which would count the shares twice. IBKR repeats the day's trades every time the adapter starts, so it stops again at every start until you acknowledge the correction. | Check the order's trades in IBKR. Then, with the adapter stopped, acknowledge it: `python3 adapter/ibkr/relay.py acknowledge-correction PENDING_FILE --journal ENGINE_LOG CORRECTED_EXEC_ID --corrects ORIGINAL_EXEC_ID --reason "what IBKR shows"` (both IDs are in the stop message). `ENGINE_LOG` is the engine's durable log, the file given to the engine as `-log`: the engine writes it as it runs, so the engine need not stop. The command reads only the fills the engine has committed to that log. It refuses a file it cannot read as the engine writes it ("cannot be read at line"), including the journal the engine writes when it stops (`-out`): a log it cannot read is never taken as holding no fills. It also refuses if the log does not hold the original as a fill ("was never recorded as a fill"): that happens when IBKR's correction arrived before the original's commission. Then no fill records those shares, so acknowledging cannot help: reconcile the order by hand ([reconciliation](../runbooks/reconciliation.md)) before starting again. The adapter then passes over that correction. It is never recorded as a fill: the engine keeps the original fill's price and shares, so reconcile the account ([reconciliation](../runbooks/reconciliation.md)) before starting again. Any further correction stops it again. |
| IBKR cancels or refuses an order while one of its trades still waits for its commission, and the adapter stops before the commission arrives | The cancellation is held until every trade of that order reported before it is recorded, and the hold is written to the file ending `.held` beside the pending file before it is accepted; a further trade IBKR reports on that order while the cancellation is held is added to the file before it is accepted. On restart IBKR repeats the trades and their commissions (not the cancelled order's status); the adapter records the fills, then the cancellation, once each. If writing the cancellation fails, the adapter stops with the cause and keeps the hold for the next start. Only the adapter holding the lock on `PENDING_FILE.lock` reads or writes the `.held` file: a second adapter started beside it is refused before it reads it. | Nothing, unless it stops with "has not replayed that execution" (IBKR did not repeat the trade within `commission_wait` seconds of the link being up and matched): then check the order and its trades in IBKR and reconcile before starting again. Leave the `.held` file alone: it is read strictly, and a hand edit stops the adapter. |
| IBKR reports a trade, or another status, on an order after the adapter recorded it cancelled or refused (or while that cancellation is held, any status but the cancellation itself) | The adapter stops ("after the order was recorded as", "after reporting it"): the engine reads nothing on an order that has ended, so a fill after a cancellation is a contradiction it does not record. For a trade, IBKR repeats the day's trades every time the adapter starts, so it stops again at every start that day until you acknowledge the trade; a restart alone does not get past it. The engine refuses a fill on an order already ended, so the trade's shares never reach the engine as a fill: it holds the position without them. No check at the adapter's start compares positions: the next start's reconciliation matches IBKR's open orders and trades with the orders the engine released (the acknowledged trade is passed over), and reads positions and balances without matching them. Positions are compared by the engine: the [reconciliation](reconciliation.md) sent with each delivery compares IBKR's holdings with the previous close's book plus the fills recorded since, and the engine checks every close snapshot's holdings against the same book and fills before taking it as the book (ADR 0019, amended 2026-10-21). The trade's shares have no fill, so the evening after it, whether it was made overnight or during the session, they show as an unexplained difference and the account goes Degraded on the stock (no new buys; `<account>: reconciliation unexplained (...)`, urgent); that Degraded is this trade, not a new fault. Your acknowledgement records why the adapter passed over the trade; the Degraded state is the engine's own record of the shares. | Check the order and its trades in IBKR. For a trade, with the adapter stopped, acknowledge it: `python3 adapter/ibkr/relay.py acknowledge-late-fill PENDING_FILE --journal ENGINE_LOG EXEC_ID --after CLIENT_ORDER_ID --reason "what IBKR shows"` (the execution ID and client order ID are in the stop message; `ENGINE_LOG` is the engine's `-log` file, as for `acknowledge-correction`). It refuses a log it cannot read, a trade the log holds as a fill ("is recorded as a fill"), and an order recorded as cancelled or refused neither in the log nor as the input awaiting its reply in `PENDING_FILE` ("neither in the engine's durable log nor as the input awaiting its reply"; the cancellation the adapter recorded just before it stopped counts, though the engine has not committed it yet). If it refuses that way, check `--after` is the order the stop message names; if it is, the adapter and the engine disagree on the order: leave the adapter stopped and reconcile by hand ([reconciliation](../runbooks/reconciliation.md)). It keeps your acknowledgement and reason in `PENDING_FILE.late-fills`, which is the record of the trade for reconciliation; the adapter then passes over that trade at every start. Then start the adapter: it goes on, and nothing at its start reports the position difference. Reconcile the account by hand for that trade's shares ([reconciliation](../runbooks/reconciliation.md), ADR 0019); when the reconciliation or the close-snapshot check has sent the account Degraded on it, follow the recovery there to settle it and clear Degraded. Any other trade on that order, or a correction of this one, stops it again. For a status, there is nothing to acknowledge: IBKR does not repeat the status of an order no longer open when the adapter starts, so a start goes on; but IBKR showed an ended order changing, which is a fault to investigate and reconcile before starting again. |
| A trade's commission report never arrives | After `commission_wait` seconds the adapter stops ("has waited ... for IBKR's commission report"): the trade happened at IBKR but cannot be recorded without its commission. | Check the trade in IBKR, then reconcile the account before starting again. |
| The adapter refuses to start: "refused: ..." | Nothing was started or sent. It names the reason: the configuration (unknown key, a value of the wrong kind, `paper_sending_enabled` on outside paper), the account (named in both places or in neither, the account file's line empty, or written with quotes or spaces), or the installed `ibapi` ("install the pinned build": not the pinned build, not importable, another version than the one imported, or an `ibapi` from outside the adapter's own environment, `/opt/trend-investing/adapter-venv`, such as a system-wide or hand-installed copy, whatever version it reports). An `ibapi` whose order or contract lacks a field the adapter sets stops it at start too ("has no field"), before any order. | Correct what it names. For `ibapi`, run `sudo deploy/provision.sh twsapi` and `sudo deploy/provision.sh adapter` from the same checkout. |
| The adapter stops at start: "cannot connect to the engine's socket" | The engine is not running, or has not finished starting (a fresh run records its Run approval before it opens its socket). Nothing was read or sent. | Start the engine first and wait for its start line in `journalctl -u ti-engine@boost`, then start the adapter. |
| The adapter stops at start: "durable log ... cannot be read" | The engine is not running yet, or `--engine-log` names another run's file. Nothing was sent. | Start the engine first, check `TI_RUN` in `run.env`, then start the adapter. |
| The adapter stops: "cannot read the engine's durable log" naming an earlier run's log, or "final record hash" | A Run approval names an earlier run's log that is missing, unreadable, or not the log you approved (its last commit is not the one the approval names). The adapter reads that log to tell whether the earlier run was of today's session, so it cannot go on without it. Nothing was sent. Logs of runs before the first earlier run from before yesterday's session whose orders have all ended (at IBKR, or settled by the adapter's own records) are never read, so archiving those stops nothing; keep every log of today's and yesterday's session, the first run before them, and every log back to a run with an order still working. | Leave the adapter stopped. Restore the named log, unchanged, from your backup into the engine's state directory (`/var/lib/trend-investing/engines/boost/`), and start the adapter again. If it cannot be restored or does not match, do not edit any log: follow [the durable log runbook](../runbooks/durable-log.md) for a log that cannot be read, and reconcile the account by hand ([reconciliation](../runbooks/reconciliation.md)) before starting again. |
| The adapter stops: "could not be read in full" | IBKR did not answer every part of the account read (positions, balances, open orders) in time, at start or after a reconnect. Nothing is sent on a partial picture. | Check IB Gateway is logged in and connected (the runbook's A4 check), then start the adapter again. |
| The adapter stops at start: "awaits its reply from an earlier engine run" | The adapter stopped last time while it waited for the engine's answer to one input, and the engine now running is a fresh run, not that run resumed. A fresh run starts from the account as it is and decides afresh, so it never takes the earlier run's input and never sends an order the earlier run decided. The message names the input, the earlier run's log, whether that run recorded an answer, and any order in that answer the adapter never sent (one "being handed over" may have reached IBKR). Nothing is sent. | If you meant to resume a crashed engine, stop it and resume the crashed run instead ([the durable log runbook](../runbooks/durable-log.md), "Restarting an engine"): the adapter then asks that run for its answer. Otherwise check IBKR for every order the message names, and **cancel at IBKR** any of them still working, and any order the earlier run sent that is still open (orders under the engine's client ID, open orders view), and confirm each is cancelled before the adapter is started: the fresh run has no record of such an order, and a later fill of it would be recorded into a run that never released it. Then, with the adapter stopped, settle the input with the command the message gives: `python3 adapter/ibkr/relay.py resolve --run-ended PENDING_FILE INPUT_ID --journal ENGINE_LOG --reason "a fresh run after ..."`, where `ENGINE_LOG` is the running engine's `-log` file. The input and those orders are recorded as refused, "run ended", in `PENDING_FILE.run-ended`: they are never sent, now or later. So is every report waiting in `PENDING_FILE.outbox` on an order an earlier run released (IBKR's reports recorded while the earlier run's engine decided): it belongs to that run, so it is struck off unsent and kept in the same record, never sent to the fresh run. So is every retirement waiting there of an order the fresh run did not release (an earlier run's, however long ago): the fresh run's engine never released that order and would refuse it. The command reads only the fresh run's log for those, so an archived log older than the runs the adapter reads at start never blocks it; a missing log the adapter does read at start refuses it, with nothing changed (see "cannot read the engine's durable log", above). Anything else waiting there stays and is sent to the fresh run when the adapter starts: another program, such as the session's close, may have recorded it for the fresh run while the adapter was stopped, and an input that may be the running run's is never thrown away. The command refuses an input of the run now running ("belongs to the run now running"), another input than the one waiting, a missing `--journal`, an input whose recorded time cannot be read ("has no recorded_at that can be read": the pending file is not as the adapter writes it, and the adapter stops at start with the same message; leave it stopped and investigate, never edit the file), and an input with no record of the engine run it was sent to ("has no record of the run it was sent to": see [re-delivery](redelivery.md), "What can go wrong"). Start the adapter: it goes on with the fresh run's own inputs, and its start reconciliation and the morning reconciliation show the account as it is (ADR 0025 point 5). |
| The adapter stops: "EngineUnavailable" or "the engine refused the input" | The engine stopped, or refused an input. The input stays in the pending file; nothing is lost and nothing is sent. | For a stopped engine, follow `docs/runbooks/durable-log.md` ("Restarting an engine") to resume it, then start the adapter: it asks for that input again first. A refusal is a fault to investigate before starting again. |
| IB Gateway's Read-Only API is still on when an order goes out | Not yet observed against IBKR. IBKR refuses API orders while Read-Only is on, with its error 321 ("Error validating request ... Read-Only mode") under the order's number. The adapter has written "sent" by then, so it never sends it again; it records IBKR's refusal for the engine as the order refused (`invalid`) with IBKR's reason, so the engine reads the order as ended. The same holds for IBKR's other codes that refuse a newly placed order before it works (103 duplicate order number, 110 price off the minimum tick, 200 no security definition, 203 security not allowed for the account), but only for an order this adapter placed in this session that IBKR has not yet shown working. If the adapter stops after it placed the order but before it read IBKR's refusal, no report is recorded: IBKR does not repeat error messages, and the order, already written "sent", is not sent again, so the engine holds it as released with no report. | Read IBKR's reason in the engine's record. If no report was recorded (the adapter stopped first), the reporter's not-acknowledged alert shows the order: confirm in IBKR that it was refused or that no order carries that client order ID, and settle it by hand as an order IBKR never took ([re-delivery](redelivery.md), "What can go wrong"). Otherwise, confirm in IBKR that no order carries that client order ID. Turn Read-Only off before the next order; the next rebalance places a fresh order if one is still wanted. |
| You are paged: a decision withheld as stale-account-snapshot, "The adapter's reading after the close could not stand for the close" | A condition of the reading after the close failed, each named in the broker snapshot's `not_as_of_close`: for example an open order at IBKR from any client after the close, something executed in the account since the close, the reading taken on a later date than the close without the Flex reader's executions proof (no proof by 9:00, one set aside, a statement that does not reach midnight, or an execution after the close in it; [reconciliation](reconciliation.md), "The executions proof"), or after the next open, or holdings or cash that are not the previous close's adjusted by the fills (the full list: [market data](market-data.md), "What happens to the file"). A run's first reading that is not the book halts the account as unverifiable instead ([reconciliation](reconciliation.md), "What can go wrong"). The reading was stamped at its read time and the session withheld; nothing was ordered and the target in force is unchanged | Read the reason in the page. Check the account in IBKR: an order still working, or a trade you placed after the close, explains it. Nothing to do if the next session's reading passes: it decides as usual. If the reason is a trade you made by hand, the next start's reconciliation will report it ([reconciliation](reconciliation.md)) |
| The engine refuses to start: "holds no earlier run, so this is the account's first run", "already holds an earlier run's durable log", "names an earlier run, ... but ... holds no earlier run's durable log" or "is a first run's, but ... holds an earlier run" | Nothing starts and nothing is written: in paper a run decides nothing without your Run approval, and the approval must be the right kind for the folder. | A first run: `sudo -u ti-trader operator approve -first-run -account <account> -by <name> -positions <SYM=N,...> -cash <amount> -currency USD -out /var/lib/trend-investing/engines/<instance>/<file>` (the holdings and cash you read in IBKR; no `-log`, no `-clear-incident`), then start the engine with `-approval <file>` ([operator](operator.md), "An approval"). A fresh run after an earlier one: `operator approve -log <latest run>.log ...`. After a crash: `-resume`, not `-approval` ([the durable log runbook](../runbooks/durable-log.md), "Restarting an engine"). |
| The adapter stops: "the reconciliation owed after the start or reconnect could not be read in 3 attempts" | IBKR did not give the positions or the cash in three reads in a row, so no stream input is sent: a reconciliation with values IBKR did not give would compare nothing. The file stays where it is. | Check IB Gateway's link and that the account holds whole US-dollar shares and reports its balances in US dollars (`journalctl -u ti-gateway`), then start the adapter again. |
| The adapter stops: "the account could not be read in full for the broker snapshot in 4 attempts over N seconds" | The same, for the day's snapshot, after four reads with waits of 5, 15 and 30 seconds between them (N is the time they actually took, each read's own waits included): nothing is written down or sent, since a snapshot missing a figure would withhold the sale back to 98% for nothing. The message ends with what became of the last account summary (IBKR's error code and message, or that it did not end). | As the row above, and [the IBKR adapter](ibkr-adapter.md), "What can go wrong" (the account summary row); the file is delivered when the adapter starts again ([market data](market-data.md), "What happens to the file"). |
| The adapter stops: "the report ... has no ... that can be read" | A report in the pending file or the outbox has no time that can be read, so it is not as the adapter records one and is never taken as an order's status. | Do not edit the file. Check the order against IBKR and treat it as a fault to investigate before starting again ([re-delivery](redelivery.md)). |
| The adapter stops at start or after a drop: "gave up after N attempts", "no handshake within connect_timeout", "Gateway closed the connection during the handshake" or "socket connect failed" | It retried with growing pauses, up to `max_connect_attempts`, and then stopped. Nothing is sent. | Bring IB Gateway back: `docs/runbooks/gateway-login.md`. Then start the adapter. |
| The adapter stops: IBKR error 326, "client_id ... is already connected" | Another program holds the engine's client ID. The adapter does not retry or pick another. | Find and stop the other program (a second adapter, a test script, TWS). |
| The adapter stops: "the stream file ... is refused" | A market-data file is not as the data job writes it, is not this run's, or is older than one already delivered. Nothing from it was sent | [Market data](market-data.md), "What happens to the file": move the file aside, run the data job again, start the adapter |
| The adapter stops at start: "the run's stream folder ... does not exist" | The data job has not written this run's warm-up yet. Nothing was read or sent. After a crash resume this means the first run's log, through which the run's folder is found, is missing from the engine's folder | Run the warm-up ([market data](market-data.md), "The one-time warm-up"), then start the adapter. After a crash resume, do not: the adapter then says "the log <path> already holds N committed delivered inputs beyond its configuration" instead (N counts delivered inputs only: market data and the adapter's broker snapshots); restore the first run's log, unchanged, from the backup ([market data](market-data.md), "A crash resume") |
| The adapter stops at start: "nothing shows it is that run's crash resume" | The run has the identity of the run whose stream folder it found but is not shown to be its crash resume. One of three cases: a dry-run first run started again without an approval (a paper engine refuses such a start itself); a resume of a first run that crashed having committed only its configuration; or a log that is not a copy of that run's records, record hash for record hash. Nothing was read or sent | For the first two, start a fresh run with its own warm-up (a dry run needs no approval; a paper run needs its approval); for the third, resume the crashed run's own log with `engine -resume` ([market data](market-data.md), "A crash resume") |
| The adapter stops at start: "has more than one stream folder" | Two stream folders belong to the run, the one named after the running `TI_RUN` included (a warm-up was run for a resumed `TI_RUN` while the first run's log was missing, say). Nothing was read or sent | Move the folder that should not be there out of `streams/boost/` (keep it), then start the adapter ([market data](market-data.md), "A crash resume") |
| You start the engine or adapter in live mode | It refuses to start and says the limited-live gate has not been passed. | Use paper. |
| A paper configuration names an account not beginning `DU` | It refuses to start. | Correct the account number in the configuration (a new configuration and a new run). |

## The rules and their sources

- AGENTS.md rule 5: no live-order behaviour before the paper-trading and
  limited-live gates.
- ADR 0025 point 3 and the engine's configuration rule: a paper
  configuration names a paper account, and a live one does not.
- ADR 0025 point 5: on every start and reconnect, IBKR's orders, trades and
  balances are matched before any order is accepted, and the engine is sent
  IBKR's positions and cash to reconcile against its book.
- ADR 0019, ADR 0025 point 5 and ADR 0026 point 1, amended 2026-10-18 (your
  decision): one reading after the close, with no order working or pending
  at IBKR and no execution since the close, is the account as of that
  close, when the account is also the previous close's adjusted by the
  fills since (cash within 1% of net liquidation; the previous close found
  back through the chain of runs, or your starting book), valued at the close;
  the mark is the delivered bar's official close, with IBKR's last price
  beside it, which the price band compares with when it is set.
- ADR 0027 and [re-delivery](redelivery.md): journal before send, and each
  order sent once.
- ADR 0022 and ADR 0026 point 2: what IBKR's reports become, and the order
  as a whole-share DAY limit order.
- ADR 0025 point 4, amended 2026-10-18: IBKR's own TWS API client (`ibapi`
  10.51.01) is pinned by its checksum and fetched and verified on the
  server, never stored in the repository.

## The tests

- `cmd/engine`, `TestSendingIsEnabledOnlyInPaperUntilTheLimitedLiveGate`: a
  live engine on a live account refuses to start and leaves no log.
- `cmd/engine`, `TestTheAccountMustMatchTheMode`: a paper configuration on a
  live account, or a live one on a paper account, is refused; paper on a
  paper account starts.
- `adapter/ibkr/tests/test_config.py`,
  `test_the_account_is_bound_to_the_mode`: the adapter's configuration
  refuses the same mismatches.
- `adapter/ibkr/tests/test_sender.py`: paper sends one order to the gateway
  and records it; live is refused; paper is refused on any connection that is
  not the paper configuration's; nothing is sent before reconciliation or
  without the link, or for another account, a market order, part shares or
  any time in force but DAY; an order released for another session, an
  account the login does not manage and a client that cannot place orders
  are all refused before any order number is used; an order number is never
  reused after a failed send.
- `adapter/ibkr/tests/test_paper.py`, against the fake gateway:
  - `test_one_spy_order_is_sent_and_its_status_and_fills_are_journalled_as_the_engine_reads_them`:
    one SPY order, accepted, filled in two parts; the records are
    `tests/testdata/paper_reports.json`;
  - `test_a_rejection_is_journalled_as_invalid_with_ibkrs_reason` and
    `test_a_cancellation_is_journalled_as_cancel_pending_then_canceled`;
  - `test_a_crash_between_the_send_and_its_mark_never_resends`: IBKR took
    the order and the adapter stopped before writing "sent"; after a restart
    it is not sent again;
  - `test_a_crash_before_ibkr_shows_the_order_stops_rather_than_resend`;
  - `test_an_order_the_link_cannot_take_yet_leaves_no_mark_and_goes_out_once_reconciled`;
  - `test_a_reconnect_never_resends_an_order_already_sent`;
  - `test_a_restart_after_reports_and_fills_were_journalled_writes_none_of_them_again`:
    IBKR replays each working order's status and each trade's commission
    after a restart, and nothing is recorded twice;
  - `test_a_corrected_execution_stops_for_a_person_and_is_never_a_second_fill`;
  - `test_an_execution_whose_commission_never_arrives_stops_for_a_person`;
  - `test_a_release_left_waiting_past_its_session_is_retired_and_never_sent` and
    `test_an_order_ibkr_can_never_accept_stops_until_the_owner_retires_it`;
  - `test_the_relay_checks_with_the_sender_before_the_mark_whatever_the_mapper`;
  - `test_an_acknowledged_correction_lets_the_adapter_start_again_and_is_never_a_fill`
    and `test_a_correction_of_an_execution_never_recorded_as_a_fill_is_not_acknowledged`;
  - `test_a_cancellation_held_for_a_fill_survives_a_stop_before_the_fills_commission`:
    the adapter stops while a cancellation waits for a fill's commission;
    after the restart the fill, then the cancellation, are recorded once;
    with `test_a_failed_write_of_a_held_cancellation_stops_and_keeps_it_for_the_restart`
    and its neighbours on the `.held` file;
  - `test_an_execution_reported_after_a_held_cancellation_is_recorded_before_it_across_a_restart`:
    a second trade reported after the cancellation was held; after a stop
    and a restart, both fills, then the cancellation, once each;
  - `test_a_second_adapter_journals_nothing_held_while_the_first_holds_the_lock`;
  - `test_a_client_that_cannot_place_orders_leaves_no_mark` and
    `test_a_login_that_does_not_manage_the_account_is_never_reconciled_or_sent_on`.
- `internal/event`,
  `TestTheIBKRAdaptersPaperReportsAreReadByOrderReports`: the engine reads
  every record the adapter's paper test wrote, by the same rule it reads
  every venue report, to the order's end; and
  `TestAPaperOrderTheAdapterNeverSentIsWorkingUntilTheOwnersClearance`:
  that release, with no report at all, stays working until the owner's
  clearance names it.

- `adapter/ibkr/tests/test_config.py`,
  `test_paper_sending_is_off_unless_the_configuration_switches_it_on`.
- `adapter/ibkr/tests/test_real_client.py`, with a stand-in `ibapi`:
  `OrderMappingTests` (the request onto ibapi's `Contract` and `Order`,
  field by field; every field that changes what is sent set to its plain
  value whatever ibapi's default, the overnight session and IBKR's price
  management off; the defaults the paper test confirms listed; an ibapi
  without a field refused when the client is built, before any order; any
  other request refused) and `PlacingOrdersTests` (an order placed only
  with mode, `DU` account, managed account and switch all in place, on the
  current session; a connection IB Gateway closed, before or during the
  hand-over, never taken as sent).
- `adapter/ibkr/tests/test_ibapi_pin.py`: the adapter starts only on the
  pinned, recorded `ibapi`, and only if the `ibapi` it imports is that
  version.
- `adapter/ibkr/tests/test_engine_client.py`: the engine socket's replies
  are used only when they answer the input sent, byte for byte, in the
  envelope and schema versions the adapter reads.
- `adapter/ibkr/tests/test_orders.py`: IBKR's refusal of an order this
  adapter placed (error 321 and the other placement codes) recorded as
  `invalid` with IBKR's reason, and the same codes on anything else not.
- `adapter/ibkr/tests/test_sender.py`: a link down or not yet matched is
  reported as such, before the sending switch is asked.
- `adapter/ibkr/tests/test_engine_log.py`: the run's identity and the
  committed inputs, read from the engine's durable log.
- `adapter/ibkr/tests/test_loop.py`, the main loop against the fake: one
  order sent once and its status and fills reaching the engine as
  `tests/testdata/paper_reports.json`; nothing sent before the account is
  read and matched; a partial read, an unmanaged account and an open order
  or trade no released order explains stop it; a missing commission stops
  it on time; a reconnect and a restart send and record nothing twice; a
  link lost while the engine decides holds the order until it is back; a
  slow engine reply does not drop a healthy link; IBKR's Read-Only refusal
  reaches the engine as `invalid`; an input awaiting its reply is asked
  for again first, byte for byte; an input the engine already holds is
  never sent again, and one under its ID with other content stops it; the
  outbox is changed only under its lock; a dry run sends nothing.
- `adapter/ibkr/tests/test_cli.py`: the start command, its refusals, the
  account read from one place and never shown, and the server's unit and
  shipped configuration checked against it.
- `adapter/ibkr/tests/test_book_basis.py`: the previous close's book found
  back through the chain of runs and your starting book (the latest wins),
  the cash drift and the unexplained cash, a starting book in another
  currency refused, and the kept `.basis` file read strictly, replaced when
  it does not read or names another approval.
- `adapter/ibkr/tests/test_morning.py`, the next morning's reading with
  the Flex reader's executions proof (your decision of 2026-10-27): as of the
  close when the proof reaches the reading's midnight with nothing after the
  close; withheld with no proof, one set aside or "none", a statement ending
  on the session's day or asked for too soon after midnight, one starting
  after the session, an execution at or after the close in it (an early
  close's too), one in IBKR's own list since midnight, a fill recorded after
  the close (an earlier run's included) or with a time that has no zone, IB Gateway's connection time or an
  execution IBKR reported in a zone other than New York's (or a connection
  time it cannot read; a reconnect to a Gateway set right is trusted
  again), a fill two runs back found past an empty run, another account's proof, or a reading after
  the next open; Monday's delivery of Friday, the day after a holiday; and
  the wait for the proof until 9:00, a proof rewritten during the read
  included.
- `adapter/ibkr/tests/test_post_close.py`, the reading after the close
  against the fake gateway and the stand-in engine: the account and broker
  snapshots as of the close with the read time beside it, then the bar,
  the mark from the bar's official close (IBKR's last price beside it),
  then the close; a reading as of the close valued at the official close
  with IBKR's own net liquidation beside it, one at its read time IBKR's
  own; no market-data permission changes nothing decided; a catch-up batch
  marks and stamps only its last session; an order working at IBKR, one in
  the last open-order list, an execution since the close, a holding with no
  close, holdings no fill explains, cash beyond the drift bound, a reading
  on a later date or before the close stamps the read time and says why; a
  fill of the run explains holdings and cash, a small posting is recorded
  as drift; an execution during the last read reads again; a later batch
  follows the last stamp the engine holds; the first batch carries the
  start-of-run reconciliation, every later batch its reconciliation ahead
  of its snapshots, and every later start and reconnect reconciles once
  the engine holds a book, only after a replayed fill's commission.
- `adapter/ibkr/tests/test_delivery.py`, the day's market data against the
  fake gateway and the stand-in engine: a file reaches the engine after the
  account and broker snapshots, in file order, as written but for the
  engine's sequence; IBKR's last price asked by the contract, and a holding
  with no bar stated without a mark; files in session order, once
  each, across a restart, each later batch behind its own snapshot; a file
  stopped partway resumes at its next line; a stop between the record and
  the outbox write, or after the write and before the record closes,
  delivers once; a stop after the engine committed and before it answered
  asks again under the same ID; a file of another run, configuration or
  build, a file that fails the strict read in any of ten ways, and a file
  older than one delivered are refused and nothing from them is sent; a
  link lost during the snapshot sends and records nothing until it is
  back; the delivery record is read strictly; an earlier run's lines left
  in the outbox are dropped unsent; the market's hours by the NYSE
  calendar.
- `adapter/ibkr/tests/test_snapshot.py`, `MarkTests`: IBKR's last price
  (evidence beside the mark) is its LAST tick, and an error, a price not
  finite and positive, a previous close or no answer give none;
  `PostCloseReadingTests`: the payloads, each the contract file
  `internal/event`'s `TestTheIBKRAdapterPayloadsDecodeAndValidate` reads.
- `internal/targetweight`, `TestAPostCloseReadingDecidesAsAReadingAtTheClose`
  (decisions identical to readings taken at the close),
  `TestThePriceBandComparesIBKRsLastPriceWhenIBKRGivesOne` and
  `TestAReadingStampedAtItsReadTimeWithholds`; `internal/risklimit`,
  `TestAPostCloseSnapshotsAlarmIsOnIBKRsOwnReading`.
- `internal/report`, `TestAStaleWithholdSaysWhyThePostCloseReadingWasNotTheClose`.
- `adapter/ibkr/tests/test_rehearsal.py`: `deploy/rehearse-paper.sh`'s
  rehearsal of the paper days with the real engine passes, and fails when
  the adapter stops or refuses a stream.

Not yet tested: sending to IBKR's real paper servers, installing `ibapi`
on the server, and Gateway 10.50.1f with API 10.51.01. They are the
hand-run test in `docs/runbooks/first-paper-order.md`. When you run it,
also confirm from IBKR's own view of the order (its order window in Client
Portal) that it is a plain DAY limit order: no display size (iceberg),
minimum quantity, percent offset, discretion, sweep, block, "not held" or
cash quantity. Those are the defaults the paper test confirms; `ibapi`'s
order has other fields the adapter neither sets nor lists, and this check
is what shows they left the order plain.
