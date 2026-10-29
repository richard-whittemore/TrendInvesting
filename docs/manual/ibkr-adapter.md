# The IBKR adapter: between the engine and IBKR

*For the owner. What the adapter is, what it does between an engine and
IBKR, what of it runs today and what does not, every stop it can make and
the command that settles each, and what can go wrong. The sending rules in
detail are in [paper trading](paper-trading.md) and
[re-delivery](redelivery.md); this chapter is the map.*

## What it is and why

The engines never talk to IBKR. Each engine has one **adapter**, a Python
program in `adapter/ibkr/` that sits between IB Gateway (IBKR's own
login-and-connection program, kept logged in on the server by IBC) and the
engine's private socket (ADR 0025; ADR 0014 for the socket). It has no
strategy logic. It turns IBKR's account figures and reports into the
engine's inputs, and the engine's released orders into IBKR orders.

It is separate from the engine because IBKR's own client library is Python
(`ibapi`), and because a fault on the IBKR side should stop one account's
link, not the program that holds the record (ADR 0025, plan section 3).

## What it does, piece by piece

| Piece (file) | What it does | Built? |
| --- | --- | --- |
| Configuration (`config.py`) | Host, port (4002 is IB Gateway's paper port), this engine's client ID, the IBKR account, the mode, the timing bounds (`commission_wait` among them) and `con_ids`, IBKR's contract number for each instrument. Every value is required but three: `paper_sending_enabled` (off when left out), and the IRA's two, `account_snapshot_version` (3, Boost's, when left out; 4 for an IRA) and `resolve_contracts` (off when left out; on for an IRA), so Boost's configuration reads exactly as before ("The IRAs' adapter", below). That includes `mark_timeout`, how long the adapter waits for IBKR's last prices (the example sets 15 seconds: IBKR sends the last price at once but only finishes the request at the end of an 11-second window, and the adapter takes a price only from a finished request, so a wait of 11 seconds or less can miss a price IBKR did send). A key that looks like a credential is refused: the adapter never holds the IBKR login, IBC does. A paper configuration must name a paper account (beginning `DU`), and `live` is refused | Built |
| Connection (`connection.py`) | Connects under the engine's own **client ID** (Boost 11, Traditional IRA 12, Roth IRA 13; 0 is refused). Set IB Gateway's **Master API client ID** to it (Configure > Settings > API > Settings): IBKR shows an API client other clients' and TWS's executions only then, and the adapter asks for every client's (`docs/runbooks/first-paper-order.md`, A4); its check of the holdings and cash since the previous close needs no client ID ([paper trading](paper-trading.md), "A paper day"). It notices a lost link (a closed socket, IBKR's codes 1100, 2110, 504, 1300, or no answer to a heartbeat) and a restored one (1101, 1102), after which it matches IBKR's orders and balances again before sending, and reconnects with growing pauses. When Gateway says it has lost IBKR's servers (1100 or 2110) it waits for Gateway to restore them (1101, 1102) for at most `restore_timeout`, and only while Gateway's connection stays open: if the connection closes during that wait, it reports the link lost at once and connects again straight away. Each new session records which accounts the IBKR login manages | Built |
| Account read (`account.py`, `snapshot.py`) | Reads positions, net liquidation, total cash, Excess Liquidity and every open order in one pass, bracketed by IBKR's clock, and builds the records the engine reads from one reading: the engine's **account snapshot** (positions and net liquidation, which orders are valued against), the **broker snapshot** the risk gate sizes sells from and the margin alarm watches, both stamped as of the session's close when the reading stands for it (your decision of 2026-10-18) or at the read time with the reason, the **mark** (the delivered bar's official close, IBKR's last price beside it, which the price band compares with when it is set), and the **reconciliation** the engine compares against its book. For an IRA (`account_snapshot_version` 4) the account snapshot is version 4: the same positions and net liquidation with IBKR's **settled cash** (`SettledCash`) and **total cash** (`TotalCashValue`) beside them, from the same reading; a reading without both gives no snapshot at all, never one with a zero. Anything IBKR did not deliver, or delivered in a form the engine cannot hold exactly, is marked **unreadable**, never sent as zero ([reconciliation](reconciliation.md), "What the adapter sends") | Built |
| Contracts (`snapshot.py`, `resolve_contracts`) | For an IRA only (`resolve_contracts`): asks IBKR, by name (a class share in IBKR's spelling, `BRK.B` as `BRK B`), for the contract of each instrument a delivery marks, as a US-dollar stock on SMART, and records the one contract IBKR answers (its contract number, SMART, its primary exchange, US dollars) with the mark. A name IBKR answers with no contract, more than one, or one that is not that name's own US-dollar stock gets no mark ("Contracts and marks for an IRA", below) | Built, against a stand-in for IB Gateway built from what IBKR's own `ibapi` hands the adapter |
| Same-day probe (`probe.py`) | `python3 -m adapter.ibkr probe ...`: reads one start's journal lines and the engine's durable log, talks to nothing, and says check by check whether the start worked ("The same-day probe", below) | Built; run on the server after every adapter install |
| Relay (`relay.py`) | Keeps the input waiting for the engine's reply in a small file on disk (the **pending file**), asks the engine for that reply again after either side restarts, and hands each released order marked `send` to the sender once, writing "about to send" before and "sent" after (the **hand-over** marks) ([re-delivery](redelivery.md)) | Built |
| Sender (`sender.py`) | Sends a released order to IB Gateway only in paper, on a paper account the login manages, while the link is up and matched, as a whole-share DAY limit order released in today's New York session, under IBKR's next order number (never reused). Dry-run never sends. Live is refused | Built, against a stand-in for IB Gateway only |
| Reports (`orders.py`, `reports.py`) | Turns IBKR's answers on this engine's orders into the engine's inputs: a status change (working, cancelling, cancelled, refused) and each fill with IBKR's commission and time. A fill is recorded only once its commission arrives. IBKR marks the end of its open-order list without saying which request it answers, so the adapter asks for one list at a time: a read waits for an earlier list to end (and drops the link if it never does) before it asks, so an earlier list's late orders are never read as a later one's. As a session starts, IB Gateway sends a list and an end-of-list mark nobody asked for; the adapter takes them before it asks for anything, waiting for that mark for at most 2 seconds after the last order Gateway sends unasked, and 10 seconds in all. If the connection closes, or IBKR sends an error that ends a connection (one under no order: IBKR's message about an order, under its order number, is kept and read as usual), during that wait, the connection attempt has failed and is tried again with the usual backoff; it is never reported connected. What is guaranteed at a start or reconnect: the open orders are taken as IBKR's whole list only when two reads in a row show the same orders in the same states, and the account read taken after them shows the same working orders; when it does not (an order filled, was cancelled or was placed while the adapter read), the orders and then the account are read again, up to four account reads. After each read the adapter waits until no open-order message has come for 2 seconds, and a read during or after which an order or an end-of-list mark arrived that no list was waiting for counts as no read, so two reads cut short the same way never pass. If the messages never pause for 2 seconds within 10 seconds, the link is dropped, made again and matched again, as after any drop; a link dropped during three reconciles in a row, with none completed between them, stops the adapter (`ReconcileIncomplete`). An execution in the account that arrives during these reads, and is not among the executions just read, has the orders and the account read again too; an order the engine released that fills meanwhile is then among them, and the start goes on. After three disagreements in a row, or four account reads that never agree with the orders, the adapter stops (`ReconcileIncomplete`) and nothing is sent. Outside a start or reconnect, the adapter also waits for that quiet before its next read whenever an order or an end-of-list mark arrived that no list was waiting for (IBKR's echo of an order the adapter itself just placed, matched to it by its order number, is expected and never makes a read wait), or a connection began without Gateway's unasked mark, so a link left one list behind is brought level before it is read again; a list that simply ends with no order (every list, while no order is open at IBKR) costs the next read no wait. What the first live IB Gateway must still confirm: that it sends its unasked mark before the adapter's first request, as the stand-in gateway does; an unasked mark that came later would cut one read short, which the quiet wait after it then catches | Built |
| Real IBKR client (`real_client.py`) | The only part that imports `ibapi` | Built; it places an order only in paper, on a paper account the login manages, with `paper_sending_enabled` switched on, and the paper-account test against IBKR is still to be run by hand |
| Main loop (`loop.py`) | At start: takes the pending file's lock, connects to the engine's socket, reads the engine's durable log, connects to IB Gateway, matches IBKR's orders against the orders the engine released, records IBKR's positions and cash for the engine to reconcile once the engine holds a book (read only once every replayed fill is recorded, sent before any market data, and tried at most three times in a row if IBKR does not give the positions or cash, then it stops for you), then asks again for any input a stop left waiting. It does the same after every reconnect. Each step reads IB Gateway's callbacks and, while the link is up and matched, sends the engine its waiting inputs in order ([re-delivery](redelivery.md)). It checks whether a reading after the close can stand for the close: read after it, on the close's own date, or on a later date before the next open with the Flex reader's executions proof showing nothing executed from the close to the midnight IBKR's own list starts at and no fill journalled since the close (your decision of 2026-10-27; until 9:00 New York a delivery waits for that proof rather than withhold; [reconciliation](reconciliation.md), "The executions proof"); no open order at IBKR, from any client; no execution at or since the close; every holding with an official close in the file and IBKR's own net liquidation positive; and the holdings and cash those of the previous close adjusted by the fills since (cash within 1% of net liquidation). The previous close is the last account snapshot of this run or of the earlier runs its approvals name, or the starting book you stated when approving the account's first run, so a fresh run's first reading is compared too; one that differs is never taken as the book ([paper trading](paper-trading.md), "A paper day"). When it stops it closes its connection to the engine, which takes one at a time | Built, against the stand-in gateway, a stand-in engine and, in the rehearsal, the real engine |
| Previous close's book (`book_basis.py`) | Finds the book a reading after the close is compared with, back through the chain of runs: the last account snapshot of this run or of the earlier runs its Run approvals name, or the starting book an approval states, whichever is later, with every fill since. It also keeps the figure behind `cash_drift` (the day's cash difference) and `unexplained_cash` (that difference added up since the cash was last explained; for Boost, since your starting book, as its book states no cash). The earlier runs' books never change, so they are kept once per run in a file beside the pending file (`.basis`); a kept file that does not read is only a warning, and is walked again and replaced. A log that cannot be read, or whose record hash chain is broken, stops it, naming the log. A starting book in another currency than the account's is never compared | Built, against a stand-in for IB Gateway and the engine |
| Market-data delivery (`delivery.py`) | Picks up each file the data job writes for the run (`--streams`), oldest first, and hands it to the engine through the outbox behind one fresh account reading: the account snapshot, the broker snapshot, a run's first reconciliation, then the file's lines with SPY's mark (the bar's official close) just before the last session's close; each input once, with the engine's own input numbers; refuses a file not as the data job writes it, not this run's, or older than one delivered ([market data](market-data.md), "What happens to the file"). **No decision needs IBKR market-data permissions**: nothing is sized from IBKR's last price; the price band compares with it when it is set. Every file carries a reconciliation of the same reading, ahead of its snapshots once the engine holds a book; in a run's first file it goes behind them when the reading matched the previous close or your starting book; when it did not it goes alone, the snapshots withheld, so the engine, holding no book, halts rather than take the difference as the book | Built, against a stand-in for IB Gateway and the engine, and rehearsed with the real engine |
| Engine link (`engine_client.py`) | The adapter's side of the engine's socket: one input at a time, and a reply is used only if it is the answer to that input from the run now running | Built, against a stand-in engine |
| `ibapi` pin (`ibapi_pin.py`) | Refuses to start unless the `ibapi` installed (`provision.sh twsapi`) is the version pinned with the adapter's code | Built |
| Start command (`cli.py`) | `python3 -m adapter.ibkr run ...`, what `ti-adapter-boost.service` runs, and for an IRA `ti-adapter@<instance>.service` (`--client-id` refuses a configuration naming another client ID than the instance's settings); reads the account number from the adapter's own account file and never logs it ([paper trading](paper-trading.md), "Starting the adapter") | Built; not yet run on the server |

### "Reconciled": what the adapter means by it

On every connect and reconnect the adapter must match IBKR's open orders,
trades and balances with its own records before it sends anything (ADR
0025, point 5). The code and the runbooks call this "reconciled". It is
**not** the engine's reconciliation, which compares IBKR's positions and
cash with the engine's book and can put the account in Degraded or Halted
([reconciliation](reconciliation.md)); the adapter sends the record for
that one from the same read, at a run's first delivery and at every later
start and reconnect. The
[glossary](glossary.md) lists all three meanings.

### How the balances are read

Each read of the account takes IBKR's positions, its open orders and three
balances in one pass: net liquidation, total cash and Excess Liquidity, in
US dollars. The balances come from IBKR's **account updates**: the adapter
subscribes to the account, takes every value IBKR sends for it until IBKR
says the download is finished, then unsubscribes. Each read is a fresh
download, and IBKR lets a program subscribe again as often as it likes.

The adapter used to read the balances from IBKR's **account summary**.
IBKR allows only two of those per connection. On 2026-10-25, with IB
Gateway 10.50, two reads worked and every later one was refused (IBKR's
code 322) for minutes, although the adapter had cancelled each, and the
adapter stopped. Why Gateway refused them is not confirmed. The account
summary is no longer used.

The rules have not changed:

- **Which figures.** Each balance is the figure IBKR sends for the whole
  account (its `NetLiquidation`, `TotalCashValue` and `ExcessLiquidity`)
  in the account's base currency, which must be US dollars. Total cash is
  `TotalCashValue`, the same figure the account summary gave. IBKR's
  separate figures for its securities and commodities segments (the
  `-S` and `-C` names) are not read.
- **What is unreadable.** Any of these makes that balance **unreadable**,
  never zero:
  - the balance in a currency other than US dollars;
  - the balance given in two currencies, so it is unclear which one is
    the total;
  - an extra row for it labelled `BASE` with a different amount;
  - any of its rows that is not a number;
  - two different amounts for it in one download. The same amount
    written two ways (`512.30` and `512.300`) is not a difference.
- **Cash in another currency.** IBKR also sends a cash line per currency
  the account holds (`CashBalance` and `TotalCashBalance`). The engine
  keeps its books in US dollars only. So if one of those lines shows a
  non-zero amount in any currency other than US dollars, total cash is
  unreadable, and the log names the currency (`total-cash unreadable:
  cash in EUR`). A line of zero, or the `BASE` total line, is fine. Net
  liquidation and Excess Liquidity are still read, since IBKR states
  them in US dollars.
- **Only this read's own download counts.** IBKR's account-update
  messages carry no request number, so the adapter draws the line itself:
  - values that arrive before it subscribes are thrown away;
  - so are values that arrive after IBKR says this download is
    finished. The download's figures stay as they were when it finished,
    even while the read waits for positions or open orders.
  - Before each read subscribes, the adapter waits until no account
    value has arrived for 2 seconds since the last read unsubscribed.
    A value that arrives during the last moment of that wait still
    counts: the adapter takes everything waiting before it calls the
    link quiet, and the 2 seconds start again from that value.
    That clears out anything IBKR sent just before the unsubscribe took
    effect, including IBKR's code 2100 ("API client has been unsubscribed
    from account data"), which can be IBKR's answer to the adapter's own
    unsubscribe. If values keep coming for 10 seconds, even while the
    adapter is taking everything waiting, the link is dropped and made
    again.
  - If the last read gave up before IBKR finished its download, the
    next read first waits for that download's late "finished" message.
    Only a "finished" naming this account counts: one for another
    account under the same login is ignored, and if this account's
    never comes the link is dropped and made again.
- **IBKR's errors.** Any IBKR error with no request number during the
  download makes the balances unreadable, 2100 among them. The only
  exceptions are the five notices IBKR's message-code list calls normal
  and about its data farms: 2104, 2106, 2107, 2108 and 2158. A data farm
  *disconnected* (2103, 2105) or a broken link to IBKR (2110) still fails
  the download.
- **A link lost** during the download voids the whole read, as it always
  has.
- **A download IBKR never finishes.** The adapter always unsubscribes,
  however the read ends. But IBKR's "finished" message carries no request
  number either, so a late one could cut a later read short. What happens
  next depends on when it happened:
  - **At start, or right after a reconnect** (the reconciliation before
    anything is sent): the read is incomplete, and the adapter **stops**
    with "could not be read in full". Start it again once IB Gateway is
    healthy (the stops table below).
  - **Later** (the reconciliation sent to the engine once the link is
    reconciled, the reading after the close, and the broker snapshot read
    with the market data): the link is dropped and made again, and reconciled
    from fresh reads before anything is sent, as for positions or open
    orders cut off the same way.
  - Either way, a later read on the same link first waits up to the
    request time limit for the late "finished". If it does not come, the
    link is dropped and made again, and that read is unread.

The broker snapshot is still read twice, with the marks read between the
two reads. Both reads must be complete, with no order report or
execution between them, and must show the same holdings, open orders and
cash (ADR 0019).

## The IRAs' adapter: one per account

Each IRA's momentum engine has its own adapter, an instance of one unit
template, `ti-adapter@<instance>.service`: `ti-adapter@trad-ira` for the
Traditional IRA (the Roth IRA's, `ti-adapter@roth-ira`, once that account has
a configuration). Boost keeps its own unit, `ti-adapter-boost.service`,
unchanged, and the template refuses to run as `ti-adapter@boost`. Each
instance has its own:

| | Traditional IRA (`ti-adapter@trad-ira`) |
| --- | --- |
| Client ID | 12, from `TI_ADAPTER_CLIENT_ID` in `/etc/trend-investing/engines/trad-ira.env`; the configuration must say the same or the adapter refuses to start |
| Configuration | `/etc/trend-investing/adapters/trad-ira.json`, created once from the example and never overwritten: Boost's values but client ID 12, `"account_snapshot_version": 4` and `"resolve_contracts": true` |
| Account number | the `IBKR_ACCOUNT_TRAD_IRA` line (named by `TI_ADAPTER_ACCOUNT_KEY`) of its own file, `accounts/trad-ira.env`, handed only to it |
| Engine socket and log | `/run/ti-engine-trad-ira/engine.sock`, `/var/lib/trend-investing/engines/trad-ira/<TI_RUN>.log` |
| Stream folder | `/var/lib/trend-investing/data/streams/trad-ira/` (the IRA's data job, `ti-data@trad-ira`) |
| Flex ledger and executions proof | `/var/lib/trend-investing/flex/trad-ira/` (its own Flex reader, `ti-flex@trad-ira`, at 05:45, 06:45 and 07:45) |
| Its own files | `/var/lib/trend-investing/adapters/trad-ira/` |

It is never started at boot and never restarted by systemd (`Restart=no`),
as Boost's: every start is yours, after its engine and, for a first run,
after the warm-up, and a failure pages you (`ti-unit-failed@`). The order and
the commands are in `docs/runbooks/first-dry-run-day.md`, "The Traditional
IRA". Everything else (the account read, the morning reading and its Flex
proof, the reconciliation, the delivery, dry-run never sending) is Boost's
adapter unchanged.

### Contracts and marks for an IRA

The momentum engine values every order it plans against a **mark** for that
stock as of the close, and its risk gate refuses an order with none
(`missing-mark`; ADR 0026, point 3). Boost's stream has a bar for everything
it marks. An IRA's compact stream does not: it carries bars only for SPY,
IEF, SHY and what you hold, and on a month's last session each ranked
stock's close history instead. A stock newly picked has no bar. So on that
session the adapter marks **every ranked stock**, at its close in that
session's universe snapshot (the same close the engine values it at), beside
the bars; a stock you hold keeps its bar's close.

Each of those marks also says which IBKR contract it is for. The adapter
asks IBKR for each name's contract (`resolve_contracts` on), by its name in
IBKR's spelling (a class share's dot becomes a space: `BRK.B` is asked as
`BRK B`, and only an answer IBKR itself spells `BRK B`, symbol and local
symbol alike, is taken; the mark keeps the name `BRK.B`), and the mark
records IBKR's contract number, SMART (where it was asked), the primary
exchange and US dollars (`market.mark.recorded` version 3); IBKR's last
price is beside it, as for Boost, when IBKR gives one (asked 50 at a time,
inside IBKR's market-data lines). A name gets **no mark** when IBKR answers
with no contract ("No security definition", IBKR's code 200), with more than
one (nothing tells which is meant, so none is taken), with one that is not
that name's own US-dollar stock, or with one whose number differs from
`con_ids` when `con_ids` names it. An answer IBKR has not finished within
`request_timeout` is not a refusal: the name is asked once more after the
rest, and gets no mark only if that second answer does not finish either.
IBKR's own refusal (code 200) is final and never asked again. The adapter
logs a warning for each (`ibkr contract for XYZ not resolved, so it has no
mark and the engine refuses any order for it (missing-mark): ...`), and the
engine then refuses any order for that stock before it is released; it is
never sized on a guess. A link lost while the contracts are asked for
voids the delivery, which is read again once the link is back. On a
month-end morning the adapter asks for about 500 contracts: allow a few
minutes. Every one of these requests, the market-data requests for IBKR's
last prices and their cancellations included, goes through one pacer: at
most 40 a second, under IBKR's limit of 50 messages a second, past which
IBKR warns and then disconnects the client. However fast IBKR answers or
refuses, no burst passes it.

At each connection the adapter also logs the server version the Gateway
negotiated (`ibkr gateway server_version=N contract_data=protobuf|text`):
`ibapi` reads IBKR's contract details from protobuf messages from version
205 on and from IBKR's older text messages below it. The adapter's test
doubles are built from both, as `ibapi`'s own decoder hands them over, so
either works; the probe reports which one the live Gateway uses.

### The same-day probe

After every adapter install, run the probe on the server the same day,
right after the adapter's first start (lessons L5 and L10: every server
defect of the Boost dry run appeared one per morning). It reads only two
files: the adapter's journal lines for that start and the engine's durable
log. The exact block is `docs/runbooks/first-dry-run-day.md`, I-8 ("The same-day probe"):

```sh
invocation=$(systemctl show -p InvocationID --value ti-adapter@trad-ira)
sudo journalctl _SYSTEMD_INVOCATION_ID="$invocation" --no-pager -o cat >/tmp/ti-adapter-trad-ira.journal
cd /opt/trend-investing/adapter
sudo -u ti-trader /opt/trend-investing/adapter-venv/bin/python3 -m adapter.ibkr probe \
  --journal /tmp/ti-adapter-trad-ira.journal \
  --engine-log /var/lib/trend-investing/engines/trad-ira/<TI_RUN>.log --client-id 12 --snapshot-version 4 \
  --expect-batch
```

What a good start prints, one line a check, last line `probe: passed`. Its
evidence is this start's own: the engine records it checks are the inputs
this start delivered, as its journal lines name them, never an earlier
start's or another run's, and each is checked field by field (a field that
is not valid, a negative net liquidation or a contract without its exchange,
fails naming it):

```text
probe: ok the adapter started: mode=dry-run client_id=12 sending=off ibapi=10.51.01
probe: Gateway server version 205 (contract details read from IBKR's protobuf messages)
probe: ok connected to IB Gateway as client_id=12 (1 time(s))
probe: ok reconciled client_id=12: positions=0 open_orders=0 executions=0
probe: ok queued run=<TI_RUN> files=trad-ira-<last session>.jsonl inputs=<n> (delivered: <n> answered, 0 already committed)
probe: ok account snapshot version 4 as of <time>: net_liquidation <n>, settled_cash <n>, total_cash <n>
probe: ok reconciliation journalled (1)
probe: ok <n> mark(s) at version 3, <n> session close(s), the last <time>
probe: ok no order released to be sent and none refused for a missing mark
probe: no decision yet: the first is made at the close of a month's last session, from the morning delivery after it
probe: passed
```

The adapter states each batch's outcome from its own accounting, in lines
beginning `ibkr stream batch files=<files>`. While a batch waits for its
reading it says `outcome=waiting reason=<the line the reading logged>`, once
for each change of reason: these are status lines, and there may be several
before the end. It ends with one final line: `inputs=N answered=A struck=S
outcome=delivered` once every input it queued is answered by the engine or
struck off as already committed (a retraction a later file repeats, say),
A + S = N, an input a stop left awaiting its reply and asked for again
included; or `outcome=dropped` when a fresh run strikes an earlier run's
batch off. A batch a stop left queued, its inputs still unsent, is taken up
by the next start, which says its queued line and, once they are settled,
its final line. The probe judges the latest batch by that line
alone: delivered with its count adding up passes; waiting, dropped, or no
outcome line at all fails, and a later batch waiting is never covered by an
earlier one delivered. A start that queued none passes only on the
adapter's own word that its stream is caught up: the adapter logs `ibkr
stream no new file since <file>; caught up` once every file of the run is
delivered, and the probe then says `probe: ok no new batch since start;
caught up (...)`. A start still waiting for its reading (`ibkr broker
snapshot waits ...`: the Flex proof, or between attempts) fails, naming
that line, and so does one with no word either way. Two forms, as the runbook's I-8 states: with
`--expect-batch` on the first day, after a data-job firing and on a
month-end morning (the start must deliver a batch); without it after a plain
restart or a binary install with no new file (the start must say it is
caught up). In
dry-run an order released marked to be sent is a `FAIL`; in paper the
probe only counts them (whether IBKR acknowledged each is in the order
reports). After a month-end the second-to-last line names the latest decision instead
(`latest decision: session <close> decided with <n> target(s)`, or
`withheld (<reason>: <detail>)`). For Boost, `--client-id 11
--snapshot-version 3` against `ti-adapter-boost`'s journal and Boost's log
(marks at version 2).

## What runs today, and what does not

What of the adapter is built, and what is not, is in the one table in the
[README](README.md#built-today-and-not-built-yet). The first-paper-order
runbook marks the steps that wait on unbuilt pieces.

## Every stop, and the command that settles it

The adapter is built to stop, and send nothing more, whenever it cannot be
sure what happened. A stop in a paper account costs nothing. Four rules
hold for every row below: do not place the order by hand at IBKR; stop the
adapter first (the commands refuse while it runs); never edit a log, a
`.handover` file or any number; if unsure, stop, look at IBKR and ask.

In the commands, `PENDING_FILE` is the adapter's pending file (named in the
stop message), `ENGINE_LOG` is the engine's durable log (its `-log` file)
and `CLIENT_ORDER_ID` is the order's tag, which IBKR shows as the order
reference. The commands are run from the repository: `python3
adapter/ibkr/relay.py ...`.

| The adapter stops with | What it means | What settles it |
| --- | --- | --- |
| "was being handed to the broker when the adapter stopped" | It stopped mid-send, and IBKR shows the order neither open nor executed. It may have arrived and been cancelled, or never arrived | Look at IBKR's order history for the reference. Seen at all: `resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "..."`. No trace, and released today: `--resend` instead, then start the adapter the same day |
| "was allowed to be sent again on" | A `--resend` permission from an earlier day: it expires with its day | Check IBKR's history for every day since, then `resolve ... --sent` |
| "was released for the session of ... not today's" with "its session is over" | No stop: the order was due on a day the link was down, its limit was for that day, so it is never sent. The adapter retires it by itself (the owner's decision of 2026-10-22), journals the retirement for the engine, and goes on; you get a routine "order ... retired (session over)" message | Nothing ([order lifecycle](order-lifecycle.md), "An order IBKR can never accept") |
| "was released for the session of ... not today's" with "settles it with relay.py resolve --never-sent" | The order's session could not be read, or is a later day than today: not a session that is over, so it is not retired | A fault: investigate. If the order is never to go, `resolve ... --never-sent --reason "..."`, then the engine side (below) |
| "can never be sent as it stands" | Not a limit order, a fraction of a share, or no contract number for the stock: IBKR can never accept it, so the adapter stops at it at every start; you are paged "order ... not acknowledged" | Find the cause (a missing contract: add it to `con_ids`, a new configuration; anything else is a fault), then `retire ... --by <your name> --reason "..."` as `ti-trader` ([operator](operator.md), "Retiring an order the adapter can never send") and start the adapter |
| "was settled as never sent, but IBKR shows" | An order you settled `--never-sent` is at IBKR after all | `resolve ... --sent` (the only decision accepted), then reconcile |
| "is not among the accounts this IBKR login manages" | The Gateway is logged in to another login than the configured account's; nothing is sent | Compare the configured account with the paper account the Gateway logged in to. A corrected account is a new configuration and a new run |
| "which is none of", "not an object of marks" or "is not JSON", naming the `.handover` file | The hand-over record holds something the adapter did not write | Restore it to the adapter's own marks, then settle each "handing-over" order with `resolve` ([re-delivery](redelivery.md)) |
| "is held by another adapter or by relay.py resolve", or "stop it first" | Another process has the pending file | Stop that process. Never delete the `.lock` file |
| "retried with other content" or "committed with different content" | The adapter sent a changed input under a used ID | A fault in the adapter: find why before starting again |
| A stop for IBKR correcting a trade (two execution IDs differing only at the end) | Recording the correction would count the shares twice; it stops at every start until you acknowledge it | `acknowledge-correction PENDING_FILE --journal ENGINE_LOG CORRECTED_EXEC_ID --corrects ORIGINAL_EXEC_ID --reason "..."`, then reconcile. If it says the original "was never recorded as a fill", reconcile by hand instead |
| "after the order was recorded as" or "after reporting it" (a trade on an order already ended) | IBKR reports a trade the engine cannot record, since the order ended | `acknowledge-late-fill PENDING_FILE --journal ENGINE_LOG EXEC_ID --after CLIENT_ORDER_ID --reason "..."`. The trade is never a fill, so the account then differs from the engine's book by its shares: follow `docs/runbooks/reconciliation.md` |
| "has waited ... for IBKR's commission report" | A trade happened but cannot be recorded without its commission | Check the trade at IBKR, then reconcile before starting again |
| "has not replayed that execution" | A cancellation held for a trade's commission, and IBKR did not repeat that trade after a restart | Check the order and its trades at IBKR, then reconcile. Leave the `.held` file alone |
| A report it cannot state exactly (part shares, a zero or odd price, a commission in another currency) | It will not record an approximation | A fault: investigate before starting again |
| "the stream file ... is refused" | A market-data file is not as the data job writes it, is another run's, or is older than one already delivered; nothing from it was sent | With the adapter stopped, move the file aside (never edit it), `sudo systemctl start ti-data`, then start the adapter ([market data](market-data.md), "What happens to the file") |
| "the run's stream folder ... does not exist" | The data job has not written this run's warm-up (after a crash resume: the first run's log is missing) | Run the warm-up, then start the adapter. If the log already holds committed delivered inputs, the adapter says so instead ("the log <path> already holds N committed delivered inputs beyond its configuration"): it is a crash resume, so restore the first run's log and do not run a warm-up ([market data](market-data.md), "A crash resume") |
| "already holds N committed delivered inputs beyond its configuration" | The log is a crash resume's (it holds copied history a fresh run's cannot; N counts delivered inputs only, market data and the adapter's broker snapshots, never operator, approval or configuration inputs), and the first run's log is not in the engine's folder, so the run's stream folder cannot be found. A stream folder with no log beside it, named in the message, is likely the first run's | Restore the first run's log, unchanged, from the backup and start again. Do not run a warm-up under the new name ([market data](market-data.md), "A crash resume") |
| "has more than one stream folder" | Two stream folders belong to the run, the one named after the running `TI_RUN` included (a warm-up run for a resumed `TI_RUN` while the first run's log was missing) | Move the extra folder out of `streams/boost/`, keep it, start the adapter |
| "nothing shows it is that run's crash resume" | The run shares the identity of the run whose folder it found but is not shown to be its resume. One of three cases: a dry-run first run started again without an approval (a paper engine refuses such a start itself); a resume of a first run that crashed having committed only its configuration; or a log that is not a copy of that run's records, record hash for record hash (one repeating its input IDs with other content, say) | For the first two, start a fresh run with its own warm-up (a dry run needs no approval; a paper run needs its approval); for the third, resume the crashed run's own log with `engine -resume` ([market data](market-data.md), "A crash resume") |
| "names this run in its first records but cannot be read" | The log beside a stream folder starts as this run's but cannot be read further (damaged) | Restore that log from the backup as the engine wrote it; never edit it ([market data](market-data.md), "A crash resume") |
| "does not follow its record hash chain" | A log compared to find the run's stream folder states record hashes its records do not give: it was changed after the engine wrote it | Restore that log from the backup as the engine wrote it; never edit it ([market data](market-data.md), "A crash resume") |
| "cannot be matched to any run" (the adapter stops) | A log beside a stream folder cannot be read from its first records (whether or not a folder of the run's own name exists): it may be this resumed run's own first log, damaged | After a crash resume, restore the first run's log from the backup; never run a warm-up for the new name. If the log is an old run's, once its run has ended move the folder out of `streams/boost/` and its log out of the engine's log folder (beside the running log), keeping both as evidence in a folder outside both ([market data](market-data.md), "A crash resume") |
| A warning: "stream folder ... skipped: its log's first records cannot be read, so it cannot be matched to any run" | The warning that comes with the stop in the row above | As that row |
| "the delivery record ... cannot be read" | The `.delivered` file beside the pending file holds something the adapter did not write | Restore it from the backup as it was; never edit it |
| IBKR error 326 | Another program holds this engine's client ID; the adapter does not retry or pick another | Stop the other program |
| Started in live mode | Refused until the limited-live gate | Use paper |
| "no order the engine released explains" or "IBKR shows N unexplained open order(s) or execution(s)" | At start or after a reconnect IBKR shows an open order or a trade of today that no run of this engine released: one placed by hand or by another program, or by an earlier run no longer read. The link is not treated as matched; nothing is sent | Look at each in IBKR (the message lists them by order reference, client ID and order number). Cancel each unexplained open order that should not be there; reconcile each unexplained execution, which cannot be cancelled ([reconciliation](../runbooks/reconciliation.md), "Find the cause"). Reconcile before starting again ([paper trading](paper-trading.md), "What can go wrong") |
| "could not be read in full" | IBKR did not answer every part of the account read at start or after a reconnect | Check IB Gateway is logged in, then start the adapter again |
| "the reconciliation owed after the start or reconnect could not be read in 3 attempts", or "the account could not be read in full for the broker snapshot in 4 attempts over N seconds" | IBKR gave neither the positions and cash for the reconciliation three times running, nor the whole account for the day's snapshot in four reads with waits of 5, 15 and 30 seconds between them; no stream input is sent. N is the time the four reads actually took: the 50 seconds of waits plus each request's own time, up to its timeout. The message ends with what became of the last download of the account values ("the last account values errored: IBKR code 2100: ...", "did not end within 10s", "ended; ExcessLiquidity missing") | See "The balances could not be read" under "What can go wrong" below, then start the adapter again; the file is delivered then ([market data](market-data.md), "What happens to the file") |
| "awaits its reply from an earlier engine run" | The adapter stopped while it waited for the engine's answer to an input, and the engine now running is a fresh run, not that run resumed | Resume the crashed run, or cancel at IBKR what the message names and any order the earlier run sent that is still open, confirm each cancelled, then with the adapter stopped settle with `resolve --run-ended ... --journal ENGINE_LOG --reason "..."` ([paper trading](paper-trading.md), "What can go wrong") |
| "carries other content than the input the engine committed", or "has no ... that can be read" naming a report | An input or report in the adapter's files is not as the adapter records it; it is neither sent nor dropped | Never edit the files; investigate against IBKR before starting again |
| "gave up after N attempts", "no handshake within connect_timeout", "Gateway closed the connection during the handshake" | The adapter could not connect to IB Gateway after its growing pauses (`max_connect_attempts`) | `docs/runbooks/gateway-login.md`, then start the adapter |
| "cannot connect to the engine's socket", "durable log ... cannot be read", "EngineUnavailable" or "the engine refused the input" | The engine is not running, has not finished starting (a fresh run records your Run approval before it listens), has stopped, or refused an input. Nothing is sent and the input stays where it is | Start or resume the engine ([the durable log runbook](../runbooks/durable-log.md), "Restarting an engine"), then start the adapter. A refusal is a fault: investigate |
| "refused: ..." at start | The configuration (unknown or missing keys, a value of the wrong kind, `paper_sending_enabled` outside paper, a key that looks like a credential), the account (named in both places or neither, or written with quotes or spaces), or the installed `ibapi` ("install the pinned build") | Correct what it names; for `ibapi`, `sudo deploy/provision.sh twsapi` and `adapter` ([paper trading](paper-trading.md), "Installing IBKR's `ibapi` on the server") |

### The settling commands at a glance

| Command | What it settles | Never use it |
| --- | --- | --- |
| `resolve ... --sent` | An order IBKR shows in any form (working, filled or cancelled): it is never sent again | — (it is always the safe answer once IBKR shows the order) |
| `resolve ... --resend` | An order committed **today** that IBKR has no trace of today: sent once more at its original limit, the same day only | For an order IBKR shows at all, or one from an earlier day (refused) |
| `resolve ... --never-sent` | The one order the adapter is refusing before it wrote "about to send", and always will: kept as a refusal with your reason; the engine is not told | For a link that is merely down today: that order goes out once the link is back. For an order IBKR can never accept (refused): `retire` is its only settlement, since it tells the engine |
| `retire PENDING_FILE CLIENT_ORDER_ID --by NAME --reason "..."` | The order the adapter stopped at because IBKR can never accept it as it stands: journalled for the engine as a refusal with your name and reason, and the adapter goes on. Owner only, as `ti-trader` | For any other stop (refused) |
| `acknowledge-correction` | IBKR's correction of a trade already recorded: passed over, never a second fill | When the original was never recorded as a fill (refused) |
| `acknowledge-late-fill` | A trade on an order already recorded as ended: passed over, left to reconciliation | For a trade the log holds as a fill (refused) |

Each command keeps your decision and reason in a file beside the pending
file: `.handover.decisions`, `.corrections` or `.late-fills`. The adapter's
other files there are `.lock` (never delete it), `.handover` (the
hand-over marks) and `.refused` (what the sender refused and why). The
held-reports file (`.held`), the delivery record (`.delivered`) and the kept
previous-close books (`.basis`) sit beside the pending file too.

### The engine side after `--never-sent`

The adapter passing over an order does not tell the engine. In the engine
the order stays released:

- **An IRA** withholds the next rebalance (`rebalance-in-progress`) until you
  clear it: `operator rebalance-cleared` naming the rebalance and the order
  ([operator](operator.md), "A rebalance clearance"). Clearing is safe here
  because IBKR never received the order.
- **Boost** lists the order as one IBKR has not ruled on
  (`undecided_orders`) in every decision until you clear it the same way:
  `operator rebalance-cleared` naming the decision that released it and
  the order. The clearance only takes it off that list, and Boost refuses it for an
  order IBKR has already named (that order was sent). A never-sent sell
  keeps counting as being sold, so every later exit in the run is smaller
  (as in dry-run: [engine](engine.md), "What dry-run simulates"), and a
  never-sent buy is not planned again, because the target in force has not
  changed. So clear it, reconcile, then start a fresh run you approve
  (`docs/runbooks/durable-log.md`, "Restarting an engine"): its first
  decision plans to the target from scratch.

## What can go wrong, and what the adapter does then

| What you see | What the program does | What you do |
| --- | --- | --- |
| A warning in the IRA adapter's journal: `ibkr contract for XYZ not resolved, so it has no mark and the engine refuses any order for it (missing-mark): ...` | IBKR did not answer the contract request for that name with exactly one US-dollar stock on SMART (its reason follows: error 200 "No security definition", N contracts, another currency or kind, no answer in time, or a contract number that differs from `con_ids`). The adapter delivers the batch without a mark for that name only; every other name is marked. The engine refuses any order for it (`missing-mark`) and goes on with the rest | Look the name up in IBKR (Client Portal, the contract search). A name that changed ticker or was delisted needs nothing: the next month ranks afresh. One that should resolve: tell the developer, with the warning. Nothing is sent in dry-run either way |
| The probe prints `probe: FAIL ...` and ends `probe: FAILED, <n> check(s)` | The probe found a check that did not hold in this start's journal or the engine's log: no start line or more than one, another client ID or mode, no connection or reconciliation, no batch queued, an account snapshot not at the version the engine reads or without its cash, a mark without its contract, an order sent in dry-run or refused for a missing mark, or a file it cannot read | Read the `FAIL` line: it says what is missing. A stop of the adapter shows as `FAIL the adapter stopped: ...`; settle it from "Every stop, and the command that settles it", start the adapter and run the probe again. Record the output in the ticket |
| `refused: the configuration ... names client_id 11, but the instance's settings give 12 (TI_ADAPTER_CLIENT_ID)` | An IRA adapter's configuration names another account's client ID (a copy of Boost's file, say); it does not connect | Set `"client_id": 12` in `/etc/trend-investing/adapters/trad-ira.json` (or restore it from `trad-ira.json.example`), then start the adapter |
| `ti-adapter@boost: the Boost account has its own adapter unit, ti-adapter-boost.service` and the unit failed | Someone started the template's boost instance; the template refuses it, so Boost never gets a second adapter | Nothing to settle: start Boost's adapter as `ti-adapter-boost`. `sudo systemctl reset-failed ti-adapter@boost` clears the failed instance |
| The link to IB Gateway drops | Reconnects under the same client ID; reads IBKR's open-order list until two reads in a row agree, then the account, which must show the same working orders (both are read again if the orders changed meanwhile), and matches IBKR's orders, trades and balances before anything is sent; never resends an order IBKR already has, and an order that is still working is matched, not reported missing. A drop during those reads, including one because IBKR's open-order messages never paused for 2 seconds within 10, is reconnected the same way; but if the link is dropped during three reconciles in a row with none completed between them (a Gateway that keeps its connection up while another client or IBKR's own app keeps working the account's orders), the adapter stops (`ReconcileIncomplete`, "the link to IB Gateway was dropped during 3 reconciles in a row (last: open-orders-never-quiet)"; "last: gateway-lost-ibkr" means IB Gateway kept losing IBKR's servers, not another client working the orders). An order placed by hand that fills while the adapter reads the account shows in no open-order list, so its execution has the orders and the account read again, and the start or reconnect then stops on it ("no order the engine released explains"). After three disagreements in a row, or four account reads that never agree with the orders, it stops (`ReconcileIncomplete`, "the link is not reconciled and nothing is sent") | Nothing, unless it gives up (`docs/runbooks/gateway-login.md`) or stops with `ReconcileIncomplete`. A stop is never restarted for you: the unit file says `Restart=no` (`deploy/systemd/ti-adapter-boost.service`), so `systemctl status ti-adapter-boost` shows the unit `failed` and `journalctl -u ti-adapter-boost` ends with "adapter stopped: ReconcileIncomplete: ..." and the reason. Nothing reaches the engine while it is stopped, so if you miss it, the reporter's "no engine heartbeat" alert pages you once 2 sessions have closed with the engine's log unchanged ([alerts-and-dashboard](alerts-and-dashboard.md)). Look at IBKR's open orders and at what else is logged in to the account (TWS, the mobile app, another API client): once nothing is being placed, filled or cancelled, start the adapter again (`sudo systemctl start ti-adapter-boost`) |
| IB Gateway loses IBKR's servers (IBKR's message 1100), then its connection closes | The adapter's link status says `connectivity-lost`, then `disconnected` the moment the connection closes; it connects again at once, with growing pauses, and matches everything again before sending. It never waits on a closed connection: the longest it waits for Gateway to restore IBKR's servers is `restore_timeout`, and only while the connection is still open. After `max_connect_attempts` failed tries it stops (`gave up after N attempts`) | Nothing, unless it gives up: `docs/runbooks/gateway-login.md` |
| The Gateway's nightly restart (11:45 PM New York) or Sunday re-login (09:00) | The link drops and comes back as above. An order IBKR accepted stays at IBKR, and is found again on reconnect | Approve the Sunday phone prompt. Not yet rehearsed against a real Gateway |
| Gateway is down when an order is due | The order is refused before "about to send" and waits; it goes out once the link is back the same New York day | Bring the Gateway back. Past the day, it is stale (above) |
| The adapter's unit failed, and you were paged: a page from the server (title ending `failed`, naming `ti-adapter-boost.service`, or an IRA's `ti-adapter@trad-ira.service`) with the adapter's last journal line as the reason, such as a stop for an incomplete reconciliation | The adapter stops itself on every condition in "Every stop, and the command that settles it" and systemd never restarts it (`Restart=no`). `ti-unit-failed@` paged you within a minute through PagerDuty and your own ntfy server, once for this failure (a restart that fails again before the adapter is seen well is the same failure, with a reminder at most once an hour); before this, nothing paged until the reporter's "no engine heartbeat" alert after two sessions. The reporter's firing pages it too if that page did not go, and resolves it once the adapter is no longer failed | Read the reason on the page, find it in "Every stop, and the command that settles it", run the settling command if it names one, then start the adapter ([paper trading](paper-trading.md), "Starting the adapter"). `journalctl -u ti-adapter-boost` (an IRA's: `journalctl -u ti-adapter@trad-ira`) has the whole reason |
| An IRA adapter's reading shows `settled-cash` unreadable (`ibkr broker snapshot read again ... settled-cash could not be read`), then, if it stays, `SnapshotRefused` | IBKR's `SettledCash` was missing, in another currency, restated with another amount in one download, not a number, or the account holds cash in another currency. The account snapshot the momentum engine reads needs it and the total cash both, from the same reading, so no snapshot is sent with either missing; the reading is tried again as for any balance (below) | As for the balances row below: check in IBKR that the account's base currency is US dollars and that it holds no other currency, then start the adapter again |
| The balances (net liquidation, total cash, Excess Liquidity) could not be read. On 2026-10-25, with the old way of reading them (IBKR's account summary), IB Gateway refused every read after the second on a connection with code 322 ("Maximum number of account summary requests exceeded; desubscribe to previous request first") although each had been cancelled, and the adapter stopped with `SnapshotRefused` | The balances now come from IBKR's account updates, a fresh download each read, so code 322 can no longer stop it ("How the balances are read" above). Each download logs what became of it: `ibkr account values ended`, or a warning `... errored: IBKR code C: IBKR's message` (account numbers shown as `<account>`), `... did not end within 10s`, `... the link to IB Gateway went before it ended`, or `ended; NetLiquidation in EUR` / `ExcessLiquidity missing` / `total-cash unreadable`. A download IBKR never finishes stops the adapter at start or right after a reconnect; later, it drops the link, which is made again and reconciled before anything is sent. A broker snapshot left incomplete is read again after 5, 15 and 30 seconds (`ibkr broker snapshot waits 15s before attempt 3 of 4`), each "read again" line naming the download's outcome. Only then does it stop (`SnapshotRefused`, as in the stops table, with the time the attempts took). A fill during any of these waits starts the reading again. A link lost during a read voids the whole read (positions, orders, executions and balances), even one IB Gateway restores within seconds; the link is reconciled again from fresh reads before anything is sent. If the outage cut the read off before IBKR finished its positions, open orders or account values (answers that carry no request number), the link is dropped and made again first. A late "finished" from the voided read can then never complete the next one | Read the last `ibkr account values` lines (`journalctl -u ti-adapter-boost`). Code 2101: another program on the same Gateway (TWS, a second API client) asked for this or another account's updates; close it. Code 2100 means IBKR ended this adapter's account-updates subscription; the adapter's own unsubscribe can cause it, and that one is taken and thrown away before the next download starts. Look for another program only when a 2100 fails a download that was under way, or one is known to be running, and then close it. Code 322 should never appear now; if it does, the installed adapter is older than this change, so run `sudo deploy/provision.sh adapter`. "did not end": check IB Gateway is logged in and has IBKR's servers. A balance in another currency, missing, or "total-cash unreadable" (with "cash in EUR" or another currency if the account holds some): check in IBKR that the account's base currency is US dollars and that it holds no cash in another currency (convert it to US dollars). Then start the adapter again (`sudo systemctl start ti-adapter-boost`) |
| The adapter stops mid-send | Stops with "was being handed to the broker" | The first row of the stops table |
| The engine restarts after a crash | On reconnect the adapter asks for its waiting input's reply again and receives the order decided before the crash, once | Nothing ([re-delivery](redelivery.md)) |
| IBKR refuses the order | Records the refusal with IBKR's reason; the engine reads the order as ended | Read the reason (`journal-query orders`). In paper it is usually an account setting |
| A sell order the program did not send (a sale you placed by hand) is working | The broker snapshot lists it among the open orders, and the reading after the close cannot stand for the close while it works (the session is withheld); at the adapter's next start or reconnect it stops on it ("no order the engine released explains"). The engine counts it as being sold, and the joint account's sale back to 98% is withheld (`unknown-working-sell`, CONTAINMENT REQUIRED) | Do not trade by hand while a program order is working ([states-and-kill-switch](states-and-kill-switch.md)) |

## What backs this

- ADR 0025 (the adapter, its points 3 to 6 and its amendments of 2026-10-16
  and 2026-10-18 on `ibapi`), ADR 0027 (journal before send, re-delivery on
  request), ADR 0022 (what IBKR's reports become), ADR 0026, points 2, 3
  and 8 (the order, the limits, one client ID per engine), ADR 0019
  (unreadable is never zero), and ADR 0019, ADR 0025 point 5 and ADR 0026
  point 1 as amended 2026-10-18 (the reading after the close stands for the
  close; the mark is the official close).
- `adapter/ibkr/README.md` lists every module's rules.
- Tests in `adapter/ibkr/tests/`: `test_config.py`, `test_connection.py`,
  `test_account.py`, `test_snapshot.py`, `test_book_basis.py`, `test_post_close.py`,
  `test_delivery.py`, `test_orders.py`,
  `test_sender.py`, `test_relay.py`, `test_paper.py`,
  `test_real_client.py`, `test_loop.py`, `test_engine_client.py`,
  `test_engine_log.py`, `test_ibapi_pin.py`, `test_cli.py` and
  `test_rehearsal.py`, all against the stand-in gateway (and, for the
  engine's side, a stand-in engine; the rehearsal runs the real one). The tests named
  row by row are in [paper trading](paper-trading.md), "The tests", and
  [re-delivery](redelivery.md), "The tests".
- The whole open-order list, against the stand-in gateway sending the list
  in parts, sending its unasked list and end-of-list mark after the
  connection is made (before or after the adapter's first request), with
  or without that mark, or sending an earlier list's orders and end late;
  two reads of the list that disagree, then agree, or disagree three
  times; two reads cut short the same way, a link one list behind, open
  orders still arriving at 10 seconds, and an empty list that ends cleanly
  costing the next read no wait:
  `test_orders.py` (`WholeOpenOrderListTests`,
  `SettledOpenOrderListTests`, among them
  `test_two_reads_cut_short_the_same_way_never_settle`,
  `test_a_link_one_list_behind_is_resynced_and_the_next_read_is_current`,
  `test_open_orders_still_arriving_at_ten_seconds_drop_the_link_however_short_the_last_gap` and
  `test_the_relays_read_after_an_empty_list_that_ended_cleanly_does_not_wait_for_quiet`),
  `test_snapshot.py`
  (`test_the_end_gateway_volunteers_on_a_reconnect_does_not_cut_the_order_list_short`)
  and `test_loop.py`
  (`test_a_reconnect_with_an_order_working_reads_ibkrs_whole_list_and_stays_reconciled`,
  `test_the_start_reads_the_open_orders_again_until_two_reads_agree`,
  `test_open_order_reads_that_disagree_three_times_leave_the_link_not_reconciled`,
  `test_an_account_read_that_never_agrees_with_the_settled_reads_leaves_the_link_not_reconciled`,
  `test_an_order_that_finishes_between_reads_on_a_healthy_link_still_settles`,
  `test_open_orders_never_quiet_during_the_starts_reconcile_reconnect_and_reconcile_the_new_link`,
  `test_open_orders_never_quiet_during_a_reconnects_reconcile_reconnect_and_never_end_the_adapter`).
  A connection that closes, or an error that ends a connection, while the
  adapter takes Gateway's unasked list: `test_connection.py`
  (`test_a_close_while_connect_takes_the_volunteered_list_fails_the_connect`,
  `test_a_fatal_error_while_connect_takes_the_volunteered_list_fails_the_connect`,
  `test_an_order_level_code_while_connect_takes_the_volunteered_list_is_kept_and_the_connect_stands`,
  `test_a_close_while_connect_takes_the_volunteered_list_is_retried_with_backoff`).
  A lost connection to IBKR's servers followed by a closed connection:
  `test_connection.py`
  (`test_connectivity_lost_then_a_socket_close_is_reported_and_reconnected_within_restore_timeout`
  and its neighbours).
- Not yet tested: anything against IBKR's real paper servers, including
  whether IB Gateway always sends its unasked end-of-list mark within the
  adapter's wait as a session starts. The first live Gateway must confirm
  it: until then, the guarantee is the two agreeing reads and the quiet wait
  after each, not the wait at connect.
