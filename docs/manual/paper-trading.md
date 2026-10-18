# Paper trading: the one mode that sends orders, and only to a paper account

*For the owner. What the program sends in paper trading, what it never
sends, what you will see, and what it does when something goes wrong.*

**Where this stands.** The sending path, the adapter's main loop and its
start command are built and tested against a stand-in for IB Gateway (a
"fake gateway" that answers like IBKR, with invented accounts and prices)
and a stand-in for the engine. No order has yet been sent to IBKR's real
paper servers: that is the hand-run test in
`docs/runbooks/first-paper-order.md`, and it is yours to run. The real IBKR
client places an order only when the adapter's configuration switches
sending on (`"paper_sending_enabled": true`, below); it ships switched off.
One piece is still missing before the engine can release an order in
paper: nothing yet sends the engine each session's inputs (the day's bar,
the broker snapshot, the mark and the session's close). Until that is
built, the adapter passes IBKR's reports to the engine but the engine has
nothing to decide on.

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
to start without it. For a fresh run after an earlier one, the **Run
approval comes first**: the engine is started with `-approval` and records
your approval in its log before it opens its socket (its log says
"recorded the owner's approval of a fresh run after ..."). Then start the
adapter. The adapter connects to the engine's socket before it reads the
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
  --ibapi-record /opt/trend-investing/twsapi/INSTALLED \
  --versions /opt/trend-investing/adapter/versions.env
```

`python3 -m adapter.ibkr run --help` lists every option. What each names:

| Option | What it is |
| --- | --- |
| `--config` | The adapter's configuration, `/etc/trend-investing/adapters/boost.json` on the server, created once from `deploy/etc/adapters/boost.json.example` and never overwritten. It ships in **dry-run with sending off** and with no contract numbers (`"con_ids": {}`). For the paper test set `"mode": "paper"`, `"paper_sending_enabled": true`, and `"con_ids": {"SPY": <SPY's contract number from IBKR's contract search>}`. It holds no account number |
| `--secrets`, `--account-key` | Where the account number comes from: the `IBKR_ACCOUNT_BOOST=` line of the adapter's own account file, `/etc/trend-investing/accounts/boost.env` (root-only, mode 0600; the unit hands the adapter a private copy of that file and nothing else, so it never sees another account's number or the alert URLs). Write it `IBKR_ACCOUNT_BOOST=DU...` with no quotes and no spaces: a value with either is refused ("is not KEY=VALUE with a bare value"). The account is named in exactly one place: either here, or as `account_id` in the configuration (then leave both options out); both, or neither, is refused |
| `--pending` | **The pending file**, `/var/lib/trend-investing/adapters/boost/pending.json` on the server. This is the `PENDING_FILE` every `relay.py` command in this manual and in [re-delivery](redelivery.md) takes. The adapter's other files sit beside it, named after it: `.handover` (the "about to send" and "sent" marks), `.handover.decisions` (your decisions), `.refused`, `.not-handed-over`, `.run` (which engine run the waiting input was sent to), `.run-ended` (inputs you settled as left by an earlier run), `.corrections`, `.late-fills`, `.lock`, `.outbox` (inputs recorded for the engine and not yet sent; anything that adds to it, the adapter or another program, does so only under the lock on `.outbox.lock`, so neither loses the other's input) and `.held` (a cancellation waiting for a fill's commission). Never edit them |
| `--engine-log` | **The engine's durable log**, `/var/lib/trend-investing/engines/boost/<TI_RUN>.log`, the `ENGINE_LOG` the acknowledgement commands take. The unit fills `<TI_RUN>` in from the same `run.env` the engine starts from |
| `--socket` | The engine's socket, `/run/ti-engine-boost/engine.sock` |
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
   a fill starts normally. Only the runs of today's session (New York
   date) are read: the adapter follows the approvals back and stops at the
   first earlier run whose last input is from an earlier day, since IBKR's
   lists hold nothing of it, and it never reads the logs before that one.
   Each approval names the earlier log's final record hash, and the
   adapter checks it against that log ("final record hash"): a log that
   differs is not the one you approved.
   Only then does it treat the link as matched ("reconciled" in its log).
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

### Installing IBKR's `ibapi` on the server

```sh
sudo deploy/provision.sh twsapi    # fetch, verify, install ibapi, record it
sudo deploy/provision.sh adapter   # install the adapter's code and its pin file
sudo deploy/provision.sh services  # the unit, the adapter's directory and configuration
```

`twsapi` downloads IBKR's TWS API 10.51.01 installer, refuses it unless its
SHA-256 matches `deploy/versions.env`, runs it as an unprivileged user
(never as root) into a scratch directory, copies only its Python client
into a root-owned directory, installs that client into the adapter's own
environment (`/opt/trend-investing/adapter-venv`, offline), checks that it
imports, and only then writes `/opt/trend-investing/twsapi/INSTALLED`. Run
`adapter` again after every pull you mean to run. If the pin in the
adapter's code ever differs from what was installed, or the `ibapi` the
adapter's Python actually imports is another version (or does not import
at all), the adapter refuses to start ("install the pinned build with
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
one), then runs the adapter's own start command against the fake gateway
and a stand-in engine, with sending switched on, and prints a `PASS` line
for each of: the account read before the order; one SPY whole-share DAY
limit order for your account, tagged with its client order ID; IBKR's
acceptance and two fills reaching the engine; a dropped link reconnected
and matched again without sending the order again; and a restart that
sends and records nothing twice. It ends "rehearsal passed: nothing was
sent to IBKR", or exits with `FAIL:` and the reason. Nothing reaches IBKR
and your account number is never printed.

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
later day it is stale (the row "The link stayed down past the end of the
order's day" below).

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

| What happens | What the program does | What to do |
| --- | --- | --- |
| The link to IB Gateway drops after an order was sent | It reconnects with the same client ID, then matches IBKR's open orders, trades and balances before anything else is sent. The order already sent is not sent again. | Nothing, unless the reconnect gives up; then see `docs/runbooks/lightsail-deployment.md`. |
| The adapter stops after IBKR took the order but before it wrote "sent", and the order is still working or has traded | On restart it finds the order in IBKR's open orders or trades and does not send it again. | Nothing. |
| The adapter stops after IBKR took the order but before it wrote "sent", and IBKR cancelled the order (or it expired) before the restart | A cancelled order is in neither IBKR's open orders nor its trades, so on restart the adapter cannot tell it from an order that never arrived. It stops with "was being handed to the broker when the adapter stopped" and sends nothing more. | Look in IBKR's order history (Client Portal or the mobile app, switched to the paper account: orders, including cancelled ones) for an order whose order reference is the client order ID named. If it is there, cancelled, it reached IBKR: settle it with `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "cancelled at IBKR"`, and the next rebalance places a fresh order if one is still wanted. Never use `--resend` for an order IBKR shows at all. The full procedure is in [re-delivery](redelivery.md), "What can go wrong". |
| The adapter stops mid-send and IBKR shows no trace of the order | It stops with "was being handed to the broker when the adapter stopped" and sends nothing more. | Follow [re-delivery](redelivery.md), "What can go wrong": look in IBKR first, then settle it with the resolve command. |
| The link is down, or not yet matched to IBKR, when an order is due (including a connection IB Gateway closed that the adapter has not yet heard about) | The order is refused before "about to send" is written, and nothing goes out. The refusal and its reason are kept in the file `PENDING_FILE.refused` until the input is settled. It waits: the adapter asks for it again on every exchange. That check is the only link check before "about to send"; a connection IB Gateway closes in the instant after it is caught by the real client's own check just before it hands the order over, which stops the adapter instead (the row "IB Gateway closed the connection as an order went out", closed before the hand-over). | Nothing, if the link is back and matched the same day (New York date): it goes out then. Do not settle a link-down refusal with `--never-sent`: the order goes out once the link is back. |
| The link stayed down past the end of the order's day | The order was for the session it was released in; its limit and the engine's checks were made for that day. On any later day it is refused ("was released for the session of ... not today's") before "about to send" is written. It is never sent, and the adapter cannot get past it by itself: it stops on that order at every exchange. | Stop the adapter. Confirm in IBKR (orders, including cancelled ones) that no order carries that client order ID as its order reference: nothing was sent. Then settle it with `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --never-sent --reason "released for <day>, refused as stale"`. The command accepts only the order the adapter is refusing (it names it if you give another). This is kept as a refusal with your reason, never as a cancel at IBKR, and the adapter goes on to its next input when started again, after checking IBKR once more: if an order or trade carrying that client order ID shows up after all, it stops with "was settled as never sent, but IBKR shows"; then mark it `--sent` (see [re-delivery](redelivery.md)). The order stays **working** in the engine, because nothing at IBKR will ever report on it, so the engine withholds the next rebalance (`rebalance-in-progress`) until you clear it with `operator rebalance-cleared` naming that rebalance and the order ([operator](operator.md), "A rebalance clearance"). Clearing is safe here because IBKR never received the order. Then reconcile and start again. Boost reads no clearance: its never-sent order stays released in its log; reconcile and report it. |
| An order the adapter can never send as it stands: no IBKR contract number configured for the stock, or a request that is not a whole-share DAY limit order for the configured account | It is refused before "about to send" is written, every time it is asked for again, so the adapter makes no progress until a person acts. | For a missing contract number: add the stock to `con_ids` in the adapter's configuration (the engine's instrument to IBKR's contract number, looked up in IBKR's contract search, never guessed by name; a change of configuration is a new run) and start the adapter again the same day; on a later day the order is stale (the row above). Any other shape refusal is a fault in the engine or the adapter: stop and investigate. If the order is never to go out, settle it with `--never-sent` and clear it in the engine, as in the row above. |
| The adapter stops: "no order the engine released explains" | At start or after a reconnect, IBKR shows an open order or a trade of today in the account that is not one of the orders the engine released under its own client ID: one you placed by hand, one placed by another program or client ID, or one no run of this engine released. An order or trade of an earlier run of this engine is explained, not a stop: after a fresh run starts the same day (a reboot, a build upgrade), the adapter reads the durable logs of the runs the Run approvals name, back to the first run of an earlier day, and records none of their reports again. The link is not treated as matched, and nothing is sent. The stop message lists each one by its order reference, client ID and IBKR order number (never the account number). | Look at each in IBKR. Cancel any order that should not be there, and reconcile the account ([reconciliation](../runbooks/reconciliation.md)) before starting again. Do not place orders by hand in the engine's account while it runs. |
| IB Gateway closed the connection as an order went out | The real client checks the connection just before and just after it hands the order to IB Gateway. Either way the order is not counted as sent: "about to send" stays written, and the adapter stops and sends nothing more. Closed before ("IB Gateway's socket was closed before placeOrder, so nothing was handed to IB Gateway"): the order never left the adapter, so IBKR cannot have it, and the adapter records that in `PENDING_FILE.not-handed-over`. Closed during ("... closed during placeOrder, so whether IBKR has the order is not known"): it may or may not have reached IBKR, and nothing is recorded. | Closed before, the same day (New York date) the order was committed, if it should still go out: with the adapter stopped, `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --resend --reason "socket closed before placeOrder"`; it goes out once the link is back and matched. Closed before, on a later day (a resend is refused then, because the order's day has passed), or if it should not go out: look in IBKR's order history (orders, including cancelled ones) and confirm no order carries that client order ID as its order reference, then `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --not-handed-over --reason "socket closed before placeOrder; IBKR shows no order"`. The command accepts it only for the order the adapter recorded in `PENDING_FILE.not-handed-over` for the input it stopped on, and refuses it for an order stopped mid-send. It is kept as never sent, and the adapter goes on when started again after checking IBKR once more, as for `--never-sent` (if IBKR shows the order after all it stops with "was settled as never sent, but IBKR shows"; then mark it `--sent`). The engine still holds the order as released, as in the row "The link stayed down past the end of the order's day": clear it there the same way. Never use `--never-sent` here: it is refused for an order marked "about to send". Closed during: as for any stop mid-send, look in IBKR for an order whose order reference is the client order ID named, then settle it with the resolve command ([re-delivery](redelivery.md), "What can go wrong"). |
| The adapter stops: "carries other content than the input the engine committed" | An input waiting in `PENDING_FILE.outbox` has the same ID as one the engine already holds, but different content. It is neither sent nor dropped. | A fault in whatever recorded the input (an ID reused for another input): compare the two, stop and investigate before starting again. |
| IBKR's login does not manage the configured account | The adapter refuses to treat the link as matched to IBKR ("is not among the accounts this IBKR login manages"), so nothing is sent. | Check the account number in the adapter's configuration against the paper account the Gateway logs in to. A wrong account is a new configuration and a new run. |
| IBKR refuses the order | It records the refusal with IBKR's reason, and the engine reads the order as ended. | Read IBKR's reason. A margin or permission refusal in paper usually means a paper account setting to change. |
| IBKR reports on the order in a form the engine cannot read exactly (part shares, a price that is zero or not a number, a commission in another currency, negative, not a number, or as large as the order itself, an order it never showed) | The adapter stops rather than record an approximation. | A fault to investigate before starting again; check the order in IBKR. |
| IBKR corrects a fill (it reports the same trade again under an execution ID that differs only in its last part) | The adapter stops rather than record the correction as a second fill, which would count the shares twice. IBKR repeats the day's trades every time the adapter starts, so it stops again at every start until you acknowledge the correction. | Check the order's trades in IBKR. Then, with the adapter stopped, acknowledge it: `python3 adapter/ibkr/relay.py acknowledge-correction PENDING_FILE --journal ENGINE_LOG CORRECTED_EXEC_ID --corrects ORIGINAL_EXEC_ID --reason "what IBKR shows"` (both IDs are in the stop message). `ENGINE_LOG` is the engine's durable log, the file given to the engine as `-log`: the engine writes it as it runs, so the engine need not stop. The command reads only the fills the engine has committed to that log. It refuses a file it cannot read as the engine writes it ("cannot be read at line"), including the journal the engine writes when it stops (`-out`): a log it cannot read is never taken as holding no fills. It also refuses if the log does not hold the original as a fill ("was never recorded as a fill"): that happens when IBKR's correction arrived before the original's commission. Then no fill records those shares, so acknowledging cannot help: reconcile the order by hand ([reconciliation](../runbooks/reconciliation.md)) before starting again. The adapter then passes over that correction. It is never recorded as a fill: the engine keeps the original fill's price and shares, so reconcile the account ([reconciliation](../runbooks/reconciliation.md)) before starting again. Any further correction stops it again. |
| IBKR cancels or refuses an order while one of its trades still waits for its commission, and the adapter stops before the commission arrives | The cancellation is held until every trade of that order reported before it is recorded, and the hold is written to the file ending `.held` beside the journal before it is accepted; a further trade IBKR reports on that order while the cancellation is held is added to the file before it is accepted. On restart IBKR repeats the trades and their commissions (not the cancelled order's status); the adapter records the fills, then the cancellation, once each. If writing the cancellation fails, the adapter stops with the cause and keeps the hold for the next start. Only the adapter holding the lock on `PENDING_FILE.lock` reads or writes the `.held` file: a second adapter started beside it is refused before it reads it. | Nothing, unless it stops with "has not replayed that execution" (IBKR did not repeat the trade within `commission_wait` seconds of the link being up and matched): then check the order and its trades in IBKR and reconcile before starting again. Leave the `.held` file alone: it is read strictly, and a hand edit stops the adapter. |
| IBKR reports a trade, or another status, on an order after the adapter recorded it cancelled or refused (or while that cancellation is held, any status but the cancellation itself) | The adapter stops ("after the order was recorded as", "after reporting it"): the engine reads nothing on an order that has ended, so a fill after a cancellation is a contradiction it does not record. For a trade, IBKR repeats the day's trades every time the adapter starts, so it stops again at every start that day until you acknowledge the trade; a restart alone does not get past it. The engine refuses a fill on an order already ended, so the trade's shares never reach the engine as a fill: it holds the position without them. The next start's reconciliation (each start and reconnect matches IBKR's positions and cash before any order is accepted, ADR 0025) reports those shares as an unexplained difference, and the engine goes Degraded then (no new buys, ADR 0019); the morning reconciliation shows it too. That Degraded is this trade, not a new fault. | Check the order and its trades in IBKR. For a trade, with the adapter stopped, acknowledge it: `python3 adapter/ibkr/relay.py acknowledge-late-fill PENDING_FILE --journal ENGINE_LOG EXEC_ID --after CLIENT_ORDER_ID --reason "what IBKR shows"` (the execution ID and client order ID are in the stop message; `ENGINE_LOG` is the engine's `-log` file, as for `acknowledge-correction`). It refuses a log it cannot read, a trade the log holds as a fill ("is recorded as a fill"), and an order recorded as cancelled or refused neither in the log nor as the input awaiting its reply in `PENDING_FILE` ("neither in the engine's durable log nor as the input awaiting its reply"; the cancellation the adapter recorded just before it stopped counts, though the engine has not committed it yet). If it refuses that way, check `--after` is the order the stop message names; if it is, the adapter and the engine disagree on the order: leave the adapter stopped and reconcile by hand ([reconciliation](../runbooks/reconciliation.md)). It keeps your acknowledgement and reason in `PENDING_FILE.late-fills`, which is the record of the trade for reconciliation; the adapter then passes over that trade at every start. Then start the adapter: its start reconciliation reports the position difference (that trade's shares) and the engine goes Degraded. Follow the recovery in [reconciliation](../runbooks/reconciliation.md) (ADR 0019) to settle it and clear Degraded. Any other trade on that order, or a correction of this one, stops it again. For a status, there is nothing to acknowledge: IBKR does not repeat the status of an order no longer open when the adapter starts, so a start goes on; but IBKR showed an ended order changing, which is a fault to investigate and reconcile before starting again. |
| A trade's commission report never arrives | After `commission_wait` seconds the adapter stops ("has waited ... for IBKR's commission report"): the trade happened at IBKR but cannot be recorded without its commission. | Check the trade in IBKR, then reconcile the account before starting again. |
| The adapter refuses to start: "refused: ..." | Nothing was started or sent. It names the reason: the configuration (unknown key, a value of the wrong kind, `paper_sending_enabled` on outside paper), the account (named in both places or in neither, the account file's line empty, or written with quotes or spaces), or the installed `ibapi` ("install the pinned build": not the pinned build, not importable, another version than the one imported, or an `ibapi` from outside the adapter's own environment, `/opt/trend-investing/adapter-venv`, such as a system-wide or hand-installed copy, whatever version it reports). An `ibapi` whose order or contract lacks a field the adapter sets stops it at start too ("has no field"), before any order. | Correct what it names. For `ibapi`, run `sudo deploy/provision.sh twsapi` and `sudo deploy/provision.sh adapter` from the same checkout. |
| The adapter stops at start: "cannot connect to the engine's socket" | The engine is not running, or has not finished starting (a fresh run records its Run approval before it opens its socket). Nothing was read or sent. | Start the engine first and wait for its start line in `journalctl -u ti-engine@boost`, then start the adapter. |
| The adapter stops at start: "durable log ... cannot be read" | The engine is not running yet, or `--engine-log` names another run's file. Nothing was sent. | Start the engine first, check `TI_RUN` in `run.env`, then start the adapter. |
| The adapter stops: "cannot read the engine's durable log" naming an earlier run's log, or "final record hash" | A Run approval names an earlier run's log that is missing, unreadable, or not the log you approved (its last commit is not the one the approval names). The adapter reads that log to tell whether the earlier run was of today's session, so it cannot go on without it. Nothing was sent. Logs of runs before the first earlier run of an earlier day are never read, so archiving those stops nothing. | Leave the adapter stopped. Restore the named log, unchanged, from your backup into the engine's state directory (`/var/lib/trend-investing/engines/boost/`), and start the adapter again. If it cannot be restored or does not match, do not edit any log: follow [the durable log runbook](../runbooks/durable-log.md) for a log that cannot be read, and reconcile the account by hand ([reconciliation](../runbooks/reconciliation.md)) before starting again. |
| The adapter stops: "could not be read in full" | IBKR did not answer every part of the account read (positions, balances, open orders) in time, at start or after a reconnect. Nothing is sent on a partial picture. | Check IB Gateway is logged in and connected (the runbook's A4 check), then start the adapter again. |
| The adapter stops at start: "awaits its reply from an earlier engine run" | The adapter stopped last time while it waited for the engine's answer to one input, and the engine now running is a fresh run, not that run resumed. A fresh run starts from the account as it is and decides afresh, so it never takes the earlier run's input and never sends an order the earlier run decided. The message names the input, the earlier run's log, whether that run recorded an answer, and any order in that answer the adapter never sent (one "being handed over" may have reached IBKR). Nothing is sent. | If you meant to resume a crashed engine, stop it and resume the crashed run instead ([the durable log runbook](../runbooks/durable-log.md), "Restarting an engine"): the adapter then asks that run for its answer. Otherwise check IBKR for every order the message names. Then, with the adapter stopped, settle the input with the command the message gives: `python3 adapter/ibkr/relay.py resolve --run-ended PENDING_FILE INPUT_ID --journal ENGINE_LOG --reason "a fresh run after ..."`, where `ENGINE_LOG` is the running engine's `-log` file. The input and those orders are recorded as refused, "run ended", in `PENDING_FILE.run-ended`: they are never sent, now or later. The command refuses an input of the run now running ("belongs to the run now running"), another input than the one waiting, and a missing `--journal`. Start the adapter: it goes on with the fresh run's own inputs, and its start reconciliation and the morning reconciliation show the account as it is (ADR 0025 point 5). |
| The adapter stops: "EngineUnavailable" or "the engine refused the input" | The engine stopped, or refused an input. The input stays in the pending file; nothing is lost and nothing is sent. | For a stopped engine, follow `docs/runbooks/durable-log.md` ("Restarting an engine") to resume it, then start the adapter: it asks for that input again first. A refusal is a fault to investigate before starting again. |
| IB Gateway's Read-Only API is still on when an order goes out | Not yet observed against IBKR. IBKR refuses API orders while Read-Only is on, with its error 321 ("Error validating request ... Read-Only mode") under the order's number. The adapter has written "sent" by then, so it never sends it again; it records IBKR's refusal for the engine as the order refused (`invalid`) with IBKR's reason, so the engine reads the order as ended. The same holds for IBKR's other codes that refuse a newly placed order before it works (103 duplicate order number, 110 price off the minimum tick, 200 no security definition, 203 security not allowed for the account), but only for an order this adapter placed in this session that IBKR has not yet shown working. If the adapter stops after it placed the order but before it read IBKR's refusal, no report is recorded: IBKR does not repeat error messages, and the order, already written "sent", is not sent again, so the engine holds it as released with no report. | Read IBKR's reason in the engine's record. If no report was recorded (the adapter stopped first), the reporter's not-acknowledged alert shows the order: confirm in IBKR that it was refused or that no order carries that client order ID, and settle it by hand as an order IBKR never took ([re-delivery](redelivery.md), "What can go wrong"). Otherwise, confirm in IBKR that no order carries that client order ID. Turn Read-Only off before the next order; the next rebalance places a fresh order if one is still wanted. |
| You start the engine or adapter in live mode | It refuses to start and says the limited-live gate has not been passed. | Use paper. |
| A paper configuration names an account not beginning `DU` | It refuses to start. | Correct the account number in the configuration (a new configuration and a new run). |

## The rules and their sources

- AGENTS.md rule 5: no live-order behaviour before the paper-trading and
  limited-live gates.
- ADR 0025 point 3 and the engine's configuration rule: a paper
  configuration names a paper account, and a live one does not.
- ADR 0025 point 5: on every start and reconnect, IBKR's orders, trades and
  balances are matched before any order is accepted.
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
  - `test_a_release_left_waiting_past_its_session_is_never_sent` and
    `test_a_stale_release_settled_as_never_sent_lets_the_adapter_go_on_and_reports_nothing`;
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
- `adapter/ibkr/tests/test_rehearsal.py`: `deploy/rehearse-paper.sh`'s
  rehearsal passes, and fails when the adapter stops.

Not yet tested: sending to IBKR's real paper servers, installing `ibapi`
on the server, and Gateway 10.50.1f with API 10.51.01. They are the
hand-run test in `docs/runbooks/first-paper-order.md`. When you run it,
also confirm from IBKR's own view of the order (its order window in Client
Portal) that it is a plain DAY limit order: no display size (iceberg),
minimum quantity, percent offset, discretion, sweep, block, "not held" or
cash quantity. Those are the defaults the paper test confirms; `ibapi`'s
order has other fields the adapter neither sets nor lists, and this check
is what shows they left the order plain.
