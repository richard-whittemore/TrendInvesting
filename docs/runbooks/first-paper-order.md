# Runbook: the first real paper order

*For the owner. A step-by-step checklist for the first order the program
ever sends to IBKR's paper servers: who does what, in what order, what to
look at afterwards, and what to do if it stops.*

The sending path has so far been tested only against a stand-in for IB
Gateway (a "fake gateway" that answers like IBKR, with invented accounts and
prices); the whole paper day is rehearsed against it with the real engine
deciding Boost (B3). This checklist is the hand-run paper-account test that ADR 0025
(Consequences) calls for, and the start of the plan's Phase 4 (IBKR paper
trading, `docs/live-trading/plan.md` section 9). Nothing here touches a live
account or real money, and AGENTS.md rule 5 still holds: the program sends
nothing live.

## What waits for what

Every piece the first paper order needs is now built. Each step below is
still tagged as it was written:

- **[NOW]** Could be done on what was first on `main`: the paper sender,
  the `twsapi` step of `provision.sh` and the manual chapter
  `docs/manual/paper-trading.md`.
- **[AFTER SECOND HALF]** Waited for the second half of the paper-sending
  work: the real IBKR client, installing IBKR's `ibapi` into the adapter's
  environment and the adapter's main loop, and then for the paper day's
  inputs: the data job's file, its delivery, and the account and broker
  snapshots and the mark made from one reading after each close. All of it
  is built; these steps can be done now, after the dry-run month (D14).

Do the owner steps in part A that are **[NOW]** first: they take the longest
(IBKR's login and phone approval) and block nothing else.

## Safety rules that apply to every step

- **The program never handles your IBKR login.** The username and password go
  only into IB Gateway's login screen (on the Mac mini) or into the
  root-only file `/etc/trend-investing/ibkr-login.env` on the server (ADR
  0025 point 6). Never paste them into an issue, a pull request, a chat with
  an agent, a shell command line or a screenshot.
- **This repository is public.** Your paper account number (it begins `DU`;
  this document calls it "your paper account (DU…)") is written only on the
  machine that runs the test, never in a file under version control, a
  ticket, a pull request or a chat. It is not a password, but together with
  other details it identifies you, and the rule is simpler than judging each
  case.
- **Paper only.** Today the engine refuses a paper configuration that names
  an account not beginning `DU`, and refuses a start mode that differs from
  its configuration's mode. **[NOW]** The adapter's configuration refuses the
  same mismatch, and the sender sends only to a paper account that your IBKR
  login manages (the manual's "What sends" in
  `docs/manual/paper-trading.md`). A live-mode configuration is a different
  thing and is not what this checklist builds: nothing here sets live mode.
- **Do not trade by hand in the paper account during the test.** A hand trade
  at IBKR looks to the program like a fill it did not make, and the morning
  reconciliation will say so.

## A. Steps only you can do

### A1. Choose the machine for the first test [NOW]

The plan puts the engines and IB Gateway on the Lightsail server
(`docs/live-trading/plan.md` section 8; ADR 0025 point 6; the server is set
up by `docs/runbooks/lightsail-deployment.md`). The Mac mini is the
alternative for a first look. The trade-offs:

| | Lightsail server (the plan) | Mac mini (first look) |
| --- | --- | --- |
| Matches what will run live | Yes: the same machine, users, service units and the Gateway and TWS API builds pinned in `deploy/versions.env` | No: IB Gateway for macOS and the TWS API for Mac are different downloads from the Linux builds the pins cover |
| Checksummed download | `provision.sh gateway` fetches the pinned Linux file and refuses a mismatch; `provision.sh twsapi` does the same for the TWS API | You download the Mac installers yourself and compare their SHA-256 to a value you compute and write down; nothing in the repository pins them |
| Counts as the ADR 0025 point 5 coverage test | Yes, if it ran on the pinned Gateway (10.50.1f) and the pinned API (10.51.01) | No: it proves the order path, not that IBKR's execution history covers the nightly restart on the pinned versions. That test must be repeated on the server |
| Cost and effort | Needs the instance (a purchase only you make) and the setup in the Lightsail runbook, including Tailscale | Nothing to buy; no firewall or Tailscale to set up |
| Weekly phone approval (2FA) | The Gateway logs out each Sunday and sends a prompt to your phone | Same prompt, and the Mac must not sleep: a sleeping Mac drops the link mid-session |
| Credentials | Root-only file on the server, handed to the Gateway only, rendered into a memory-only directory (`deploy/libexec/render-ibc-config.sh`) | Typed into Gateway's login screen each time; keep it off the iCloud-synced Desktop |
| Where the account number goes | `/etc/trend-investing/`, root-owned | A local file outside the repository checkout and outside any synced folder |

**Recommendation:** use the server if it exists by the time the second half
is ready, because only a server run can count as the coverage test and the
pins make it checksummed end to end. Use the Mac mini only to get an early
look at the Gateway and the paper account, and say so in the record of the
test. The rest of this checklist is written for the server; where the Mac
mini differs, a line says so. The program's engine and adapter are the same
on both.

### A2. Install IB Gateway from the pinned, checksummed download [NOW on the server; Mac: your own download]

**Server.** Nothing to download by hand. After the Lightsail runbook's setup
steps 1 to 6 (instance, Tailscale, firewall), on the server:

```sh
cd TrendInvesting
sudo deploy/provision.sh gateway    # IBC and IB Gateway, checksum-verified
```

The script downloads IBKR's Linux installer, compares its SHA-256 with the
one in `deploy/versions.env`, and stops with "checksum mismatch" if they
differ. A stop is the safe outcome. IBKR replaces its "stable" installer
without notice and publishes no checksum, so a mismatch usually means a new
Gateway build, not tampering; do not edit the checksum to make it pass. Follow
"Bumping a pinned version" in the Lightsail runbook, which requires repeating
this test on the new build.

**Mac mini.** Download IB Gateway (stable, macOS) from IBKR's own download
page, compute `shasum -a 256` of the file, write the value and the version
somewhere private, then run the installer. Do not run an installer you cannot
trace to IBKR's page.

### A3. Log in with the paper credentials [NOW]

Your IBKR paper account has its own username and password, separate from your
live ones (IBKR sends them when the paper account is opened; they are shown
in Account Management).

- **Server.** `sudo -e /etc/trend-investing/ibkr-login.env`, fill in
  `TWS_USERID` and `TWS_PASSWORD` with the **paper** username and password,
  leave `TWS_TRADING_MODE=paper`. Save. Start the Gateway:
  `sudo systemctl start ti-gateway`, and approve the IBKR Mobile prompt on
  your phone within three minutes. `journalctl -u ti-gateway -n 50` should
  show a login and no credential. If it will not log in, see
  `docs/runbooks/gateway-login.md`, or the "Reconnect drill" in the Lightsail
  runbook.
- **Mac mini.** Open IB Gateway, choose "IB API" and **Paper Trading**, and
  type the paper credentials into its login screen.

The program never sees these. The adapter's configuration refuses any key
containing "password", "secret" or "token".

### A4. Enable the API socket for paper [NOW]

IB Gateway only lets a program connect if you say so. In Gateway's
Configure > Settings > API > Settings (on the server there is no screen:
`render-ibc-config.sh` sets only the trading mode and the read-only flag, so
the rest are Gateway's own settings and the listening check below is how you
know they are right):

| Setting | Value | Why |
| --- | --- | --- |
| Socket port | **4002** | IB Gateway's paper-trading port (4001 is the live one; the adapter's paper configuration must use 4002) |
| Enable ActiveX and Socket Clients | on | Without it the adapter's connection is refused |
| Read-Only API | **on (leave it on) for now** | A read-only API refuses every order, which is what you want until the moment of the first order. On the server this is `TWS_READONLY_API=yes` in `ibkr-login.env`, the shipped value. You turn it off in B4, immediately before the first order, and back on afterwards (C) |
| Trusted IPs | `127.0.0.1` only | The adapter runs on the same machine as the Gateway. Do not add another address, and do not open the port in any firewall: the server's firewall already allows inbound traffic on the Tailscale interface only |
| Allow connections from localhost only | on | Same reason |
| Master API client ID | **11** (Boost's client ID) | IBKR shows an API client the executions of other clients, of TWS and of IBKR's own app only when its client ID is the Master API client ID. The adapter asks for every client's executions to check that nothing executed since the close (`docs/manual/paper-trading.md`, "A paper day"); with this set, its list has them all. On the server, set it once in Gateway's own settings as for the others (IBC does not set it); the adapter's check of the holdings and cash since the previous close does not depend on it |

Check that the Gateway is listening on 4002 and only on this machine. On the
server run `ss -ltn | grep 4002`; you should see one line whose local address
is `127.0.0.1:4002` (after the Gateway has logged in; it does not listen
before). On the Mac run `lsof -nP -iTCP:4002 -sTCP:LISTEN`; you should see
the Gateway's Java process listening on `127.0.0.1:4002` or `localhost:4002`.
If nothing is listening, the Gateway is not logged in yet or its API is off:
check the login (A3), then Gateway's API settings above (Enable ActiveX and
Socket Clients, port 4002), and run the check again. If the address is
`0.0.0.0` or `*` (every interface), "Allow connections from localhost only"
is off: turn it on and check again before going on.
Also confirm the paper account was given trading permissions for US stocks
and ETFs in Account Management (SPY is one); a missing permission shows up
later as an IBKR refusal (part D).

### A5. Put the paper account number on the machine, and nowhere else [NOW]

Find your paper account number (DU…) at the top of IB Gateway's window, or in
Account Management with the paper switch on. Then enter it on the machine,
by hand, in the places below. It is never committed, and the repository's
files never contain it, because the repository is public.

| Where | What | Why |
| --- | --- | --- |
| `/etc/trend-investing/accounts/boost.env`, the line `IBKR_ACCOUNT_BOOST=` (use `sudo -e`; root-only, mode 0600) | your paper account (DU…), with no quotes and no spaces | The shipped file holds the name only. It is the only file the Boost adapter is given: no other account's number and no alert URL. Do not put the account in `/etc/trend-investing/secrets.env`: no program reads an `IBKR_ACCOUNT_*` line from that file (the IRA lines there are names kept for later engines), so a number there is used by nothing |
| The Boost engine's paper configuration, `/etc/trend-investing/engines/boost.config`: a copy of the shipped dry-run file with `"mode": "paper"`, `"account_id"` set to your paper account (DU…), and `"warm_up_start": "2025-01-02"` | the mode, the account and the warm-up start | The engine refuses to start if the mode in this file is not the mode you start it with, or if a paper configuration names an account not beginning `DU`. The file is hashed into the run (ADR 0016), so a change is a new run. `warm_up_start` must lie within Massive's two-year window, or the data job's warm-up cannot fetch the history: the shipped 2024-01-02 is outside it. 2025-01-02 is the recommended value (your choice: any session inside the window with at least 200 sessions before the first paper day); it stays inside the window until 2 January 2027. It changes the paper configuration's hash only (`docs/manual/market-data.md`, "The one-time warm-up") |
| `/etc/trend-investing/engines/boost.env`: set `TI_MODE=paper` | the engine's start mode | It must equal the mode in the configuration |
| The adapter's configuration, `/etc/trend-investing/adapters/boost.json`: `"mode": "paper"`, `"paper_sending_enabled": true`, `host` 127.0.0.1, `port` 4002, the engine's `client_id` (Boost is 11), the timing values and `con_ids` with SPY's IBKR contract number. No `account_id`: the account comes from `accounts/boost.env` only, and naming it in both places is refused | the adapter's side of the binding | See B2 |

Two cautions:

- `sudo deploy/provision.sh services` copies the repository's `*.env` and
  `*.config` files over `/etc/trend-investing/engines/`. Running it again
  would put the dry-run files back and erase your edits. After the first
  paper edit, do not re-run `services` without redoing them; if the file
  changed, the engine refuses to start in paper mode, which is the safe
  outcome.
- Check that `git status` in the checkout shows nothing new under `deploy/`
  before every commit or pull. The edits above are made in `/etc`, not in the
  checkout.

On the Mac mini, the same values go in files in a directory outside the
repository checkout (not on the iCloud-synced Desktop), and the engine and
adapter are pointed at them.

### A6. Put the Massive key on the machine [NOW]

The daily data job fetches SPY's prices from Massive with your own key. On
the server, `sudo -e /etc/trend-investing/secrets.env` and fill in the line
`MASSIVE_API_KEY=` (add the line if the file predates it). Like the account
number, it is never committed, pasted into an issue or typed on a command
line.

## B. Steps the program and the adapter take

Most of these are commands you start, but nothing in this part needs a
decision from you.

### B1. Fetch IBKR's TWS API [NOW]

```sh
sudo deploy/provision.sh twsapi
```

This downloads IBKR's TWS API 10.51.01 installer, verifies its SHA-256
against `deploy/versions.env`, and stops on a mismatch (ADR 0025 point 4,
amended). It is its own step and is not part of `all`, so a change at IBKR
never blocks the Gateway or the services. The file is not stored in the
repository (IBKR's licence terms) and the step only fetches and verifies it.

**Installing `ibapi` into the adapter's environment [NOW]**: the same
`provision.sh twsapi` step installs the verified build's Python client into
the adapter's own environment and records it; then install the adapter's
code and pin file with `sudo deploy/provision.sh adapter` from the same
checkout ([paper trading](../manual/paper-trading.md), "Starting the
adapter").

### B2. The configuration binds paper mode to the DU account [NOW]

Three checks, and any one stops the order (the manual's "What sends"):

1. the engine refuses a paper configuration whose account does not begin
   `DU`, or a start mode that differs from the configuration's mode;
2. the adapter's configuration refuses the same mismatch;
3. before every order, the sender checks the order is for that account, the
   connection came from that paper configuration, and IBKR's `managedAccounts`
   list (sent at the start of each session) includes the account. If your
   login does not manage it, the link is never treated as matched to IBKR and
   nothing is sent.

### B3. Rehearse against the fake with the real account-ID shape [NOW]

Before pointing anything at IBKR, prove the checks accept your account number
and the adapter's suite is green on this machine, then rehearse the paper
days with the real engine. None of it sends anything to IBKR. In a checkout
(the account number is typed at a hidden prompt, so it is not saved in shell
history or the screen):

```sh
make adapter-test
printf 'paper account (DU...): '; read -r -s ACC; echo; export ACC
python3 - <<'PY'
import os, sys
sys.path.insert(0, "adapter/ibkr")
from config import is_paper_account
print("shaped like a paper account:", is_paper_account(os.environ["ACC"]))
PY
unset ACC
```

The suite includes `adapter/ibkr/tests/test_paper.py`, which sends one SPY
order to the fake gateway and journals its answers. The second command prints
`True` only for an ID beginning `DU`. Do not paste the number into the
command line or into a file in the checkout. The rehearsal's value is that a
typo in the number is found here, not in front of IBKR.

Then run the rehearsal of the paper days, which builds the real engine and
runs it in paper mode, deciding Boost (it asks for the account at a hidden
prompt too):

```sh
deploy/rehearse-paper.sh --account
```

It must end "rehearsal passed: nothing was sent to IBKR", with a `PASS`
line for each step ([paper trading](../manual/paper-trading.md), "The
rehearsal"): the warm-up, the day's reading as of the close, the engine's
decision and its one order sent once, the reconnect and restart reconciled,
a day withheld because a fill came after the close, a crash resume, and a
holding no order explains found at the next start.

### B4. Start the engine and the adapter in paper mode [AFTER SECOND HALF]

Order of starting, each on the server, each an explicit step of yours:

1. The Gateway is running and logged in (A3).
2. **Turn the Read-Only API off, now, and not before.** On the server:
   `sudo -e /etc/trend-investing/ibkr-login.env`, set
   `TWS_READONLY_API=no`, then `sudo systemctl restart ti-gateway` and
   approve the phone prompt if it comes; wait until the check in A4 shows
   port 4002 listening again. On the Mac: untick Read-Only API in
   Gateway's API settings. From here until you turn it back on (C), the
   Gateway accepts orders from the adapter.
3. Write the run file the engine reads, exactly as the "Restarting an engine"
   section of `docs/runbooks/durable-log.md` says: a new `TI_RUN`, a
   `TI_AS_OF`, and an empty `TI_RESTART_ARGS` for a first run. A log is never
   reused.
4. Start the engine, then the adapter: `sudo systemctl start ti-engine@boost`,
   then, once the engine's log (`journalctl -u ti-engine@boost`) shows its
   start, `sudo systemctl start ti-adapter-boost`. The adapter refuses to
   start until the engine's socket is open and its durable log exists; for a
   fresh run the engine records the Run approval before it opens its socket.
   For a run's first paper day the order is: the engine; then the data
   job's one-time warm-up (`docs/manual/market-data.md`, "The one-time
   warm-up"), which writes the run's first file,
   `/var/lib/trend-investing/data/streams/boost/<TI_RUN>/boost-<date>.jsonl`;
   check it exists (`ls` that folder); **only then** start the adapter. The
   adapter refuses to start until the run's stream folder exists ("the
   run's stream folder ... does not exist"), and once started it delivers
   the warm-up to the engine behind one account reading, with the
   start-of-run reconciliation. **Do this the evening before the first paper
   day, or that morning**: the first day's reading must come after the
   warm-up's, and a warm-up delivered after the first day's 17:05 file is
   due withholds that day. From then on the timer writes each day's file at
   17:05 and the adapter, left running, delivers it within seconds behind
   one reading after the close, which stands for the account as of the
   close when nothing is working at IBKR and nothing has executed since
   (`docs/manual/paper-trading.md`, "A paper day"). Keep the adapter
   running through the evening: a file delivered the next morning is read
   on a later date than its close, so that session is withheld, and an
   order released the evening before is for a session already past, which
   is never sent.
   Neither is started or restarted by systemd on its own; every start is
   yours.
   **IBKR market data is not needed.** SPY's mark is the day's official
   close from the data job; IBKR's last price is recorded beside it, and
   the price band compares a buy with it when it is there. If the paper
   login has no market data for SPY, the band compares the close with
   itself and always passes, and nothing else changes.
5. Confirm the engine's log begins with the paper configuration, and that the
   adapter says it connected on 4002, read the account's balances in full
   and matched IBKR's open orders and today's trades to the orders the
   engine released ("[reconciled](../manual/glossary.md)", the adapter's matching) before anything else. The adapter does this on
   every start and reconnect (ADR 0025 point 5), and, once the engine
   holds an account snapshot, sends IBKR's positions and cash for the
   engine to reconcile against its book: `ibkr reconciliation recorded for
   the engine` in its log, and a `risk.reconciliation.result` of
   `continue` in the engine's journal.

The adapter's startup command, its options and its service unit are in
[paper trading](../manual/paper-trading.md), "Starting the adapter".

### B5. What the first order is [AFTER SECOND HALF]

One order, from the Boost engine on the paper account:

- **One whole-share SPY limit order**, time in force **DAY** (good for the
  session it was released in). No market orders, no part shares, no other
  time in force (ADR 0022; ADR 0026 point 2).
- **Its size** is whatever the strategy decides on the paper account's
  balance, then limited by the hard limits (ADR 0026 point 3). A new paper
  account has a large simulated balance, so the size in shares may look big
  next to your real accounts; it is still simulated cash. Read the balance
  before the run so the size is no surprise.
- **It is sent at most once.** The engine writes the order to its durable log
  first and releases it marked `send: true`. The adapter then writes "about to
  send" on disk, sends it under IBKR's next order number, then writes "sent".
  The order's client order ID travels to IBKR as its order reference
  (`orderRef`).
- **When it goes out:** an order is for the session it was released in (New
  York date). If the link is down or not yet matched, it waits and goes out
  when the link is back the same day. An order that cannot go out in that
  session is refused as stale and never sent (part D).

### B6. What gets journalled [AFTER SECOND HALF]

| Where | What you will find |
| --- | --- |
| The engine's durable log (`-log`, in `/var/lib/trend-investing/engines/boost/`) | the order released with `mode: paper` and `send: true`; then each report IBKR sent, as the engine read it |
| IBKR's reports, as records | an order-lifecycle record each time the order's state changes (working, cancelling, cancelled or refused, with IBKR's reason), and one fill record for each execution, with shares, price, IBKR's commission and IBKR's time. A fill is recorded only once IBKR's commission report arrives |
| The adapter's own files beside its pending file (the file's name followed by a suffix) | `.handover` (the "about to send" and "sent" marks), `.handover.decisions` (every decision you make with `relay.py resolve`), `.refused` (an order the sender refused and why), `.corrections` and `.late-fills` (your acknowledgements) |
| The adapter's held-reports file, `PENDING_FILE.held` (the pending file's name followed by `.held`; on the server `/var/lib/trend-investing/adapters/boost/pending.json.held`) | a cancellation or refusal waiting for a fill's commission before it is recorded |

You never edit any of these files by hand. The adapter reads them strictly
and stops on a value it does not know.

### B7. How a reconnect is proven not to resend [AFTER SECOND HALF]

The rule is in the manual (`docs/manual/paper-trading.md`) and in
`docs/manual/redelivery.md`: on every reconnect the adapter reconnects with
the same client ID, reads the account's balances in full and matches IBKR's
open orders and today's trades to its own records **before** anything else is
sent. An order already marked "sent",
or one IBKR shows as open or executed, is never sent again. The code's tests
(`adapter/ibkr/tests/test_paper.py`, against the fake) include a reconnect
that never resends and a crash between the send and its mark that never
resends.

Prove it for real, once, in paper, after the first order has been accepted:

1. Note the order's number of appearances: IBKR's order history shows the
   order once, with your client order ID as its order reference.
2. Cause a reconnect: `sudo systemctl restart ti-gateway` (this drops the
   link; approve the phone prompt if it comes), or stop and start the adapter.
3. Wait until the adapter says it has matched IBKR again.
4. Look again: IBKR's order history still shows **one** order with that
   reference, and the journal shows the order released once and no second
   "about to send" for it. Both must hold. If either does not, stop the
   adapter and go to part D.

## C. What to check afterwards

Check in this order, and compare the three: they must agree.

1. **IBKR's own order history.** In Client Portal or the mobile app, switched
   to the paper account: Orders (include cancelled ones). Find the SPY order:
   side, quantity, limit price, DAY, status, and the order reference. The
   order reference must equal the client order ID in the engine's log.
2. **The journal, with `journal-query`** (`docs/manual/journal-query.md`), on
   the engine's durable log:

   ```sh
   go run ./cmd/journal-query orders  <durable log>   # planned, released, IBKR's lifecycle changes
   go run ./cmd/journal-query fills   <durable log>   # each fill: quantity, price, commission
   go run ./cmd/journal-query refusals <durable log>  # nothing expected
   go run ./cmd/journal-query positions <durable log> # holding rebuilt from the fills
   ```

   The order's quantity and limit match IBKR's; the fills add up to the
   quantity IBKR shows executed; the commission on each fill is IBKR's own
   figure; `refusals` shows nothing for this order.
3. **The reporter's alerts** (`docs/manual/alerts-and-dashboard.md`). An
   urgent alert "order … not acknowledged" means IBKR had not answered within
   the allowed time (`docs/manual/order-lifecycle.md`); one "order …
   acknowledged" follows when it does. Any urgent alert about a refusal, a
   state change or a report the engine could not read during this test is a
   finding: read it and part D before the next session.
4. **The morning reconciliation** (`docs/manual/reconciliation.md`): the next
   morning IBKR's positions and cash match the book. A difference puts the
   account in Degraded or Halted; follow `docs/runbooks/reconciliation.md`.

When the order is filled, reconciled and the reconnect proof (B7) passed, the
first paper order is done. Then turn the Read-Only API back on: on the server
set `TWS_READONLY_API=yes` in `ibkr-login.env` and `sudo systemctl restart
ti-gateway`; on the Mac tick Read-Only API in Gateway's settings. Do this even
if you plan to run again soon, so a stray start cannot send. Record the
result: which machine, which Gateway and API builds, the date, and what you
saw. Phase 4 continues from here with the
daily checks (plan section 9).

## D. What can go wrong, and what to do

This part is written for someone who does not read code. The program is
built to stop and send nothing more whenever it cannot be sure what
happened. A stopped program costs nothing in a paper account, so a stop is
not an emergency. The full list is "What can go wrong, and what the program
does" in `docs/manual/paper-trading.md`.

**Four rules for every case below:**

1. Do not place the order by hand at IBKR.
2. Stop the adapter first (the commands below refuse to run while it is
   running).
3. Never edit a journal, a `.handover` file or any number to make things
   look right.
4. If you are unsure: stop, look in IBKR, change nothing, and ask.

Three names are used in the commands. **PENDING_FILE** is the adapter's
pending file: its path is named in the adapter's stop message and in its
start command (added by the second half). **ENGINE_LOG** is the engine's
`-log` file, the `<TI_RUN>.log` in `/var/lib/trend-investing/engines/boost/`
(B4). **CLIENT_ORDER_ID** is the order's tag, shown in the stop message and
in IBKR as the order's reference. Replace these capitals with the real
values and leave everything else exactly as written. The `relay.py`
commands below are on `main` now **[NOW]**.

### D1. The order was never sent

- **What you will see:** a message that the order "was released for the
  session of ... not today's", or a line for it in the file
  `PENDING_FILE.refused`.
- **What it means:** the program refused the order before it wrote "about
  to send", so IBKR never received it. If the connection was only down
  during the same trading day, the order simply goes out when the
  connection is back. After the day ends it is out of date and can never go
  out.
- **What to do:** if the day has not ended, do nothing. If it has: stop the
  adapter, look in IBKR (orders, including cancelled ones) and confirm no
  order has that CLIENT_ORDER_ID as its reference, then run this, replacing
  the capitals:

  ```sh
  python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --never-sent --reason "released for DAY, refused as stale"
  ```

  Boost reads no operator clearance (`docs/manual/operator.md`, "A
  rebalance clearance"), so after this reconcile the account
  (`docs/runbooks/reconciliation.md`), then start a fresh run as
  `docs/runbooks/durable-log.md` ("Restarting an engine") says. The manual's
  rows on a stale order and on an order the adapter can never send (such as
  a missing contract number in the adapter's `con_ids`) have the detail.

### D2. The program stopped while sending

- **What you will see:** a message "was being handed to the broker when the
  adapter stopped".
- **What it means:** the adapter stopped at the moment of sending and IBKR
  shows the order neither open nor executed. It may have been taken and
  cancelled, or never arrived. The program will not guess.
- **What to do:** in IBKR, look for an order with that CLIENT_ORDER_ID as
  its reference. If you find it at all (working, filled or cancelled), run:

  ```sh
  python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "what IBKR showed"
  ```

  Only if IBKR has no trace of it, and it was released today, run the same
  command with `--resend` in place of `--sent`, then start the adapter the
  same day. Never use `--resend` for an order IBKR shows in any form
  (`docs/manual/redelivery.md`, "What can go wrong").

### D3. IBKR reported the same trade twice

- **What you will see:** the adapter stops at every start, with a message
  naming two execution IDs that differ only at the end.
- **What it means:** IBKR corrected a trade it already reported. Recording
  it would count the shares twice, so the program refuses until you say you
  have looked.
- **What to do:** with the adapter stopped, check the trades in IBKR, then
  run this (both IDs are in the stop message; replace ENGINE_LOG,
  CORRECTED_EXEC_ID and ORIGINAL_EXEC_ID):

  ```sh
  python3 adapter/ibkr/relay.py acknowledge-correction PENDING_FILE --journal ENGINE_LOG CORRECTED_EXEC_ID --corrects ORIGINAL_EXEC_ID --reason "what IBKR shows"
  ```

  The repeat is never recorded as a fill. Reconcile the account before
  starting again. If the message says the original "was never recorded as a
  fill", acknowledging cannot help: reconcile by hand first.

### D4. A trade arrived for an order already closed

- **What you will see:** the adapter stops (at every start that day, for a
  trade) after IBKR reports a trade or status on an order the program had
  already recorded as cancelled or refused.
- **What it means:** IBKR's report contradicts the program's record, and the
  engine will not record a contradiction.
- **What to do:** with the adapter stopped, check the order and its trades
  in IBKR, then run (replace EXEC_ID, the trade's execution ID from the stop
  message):

  ```sh
  python3 adapter/ibkr/relay.py acknowledge-late-fill PENDING_FILE --journal ENGINE_LOG EXEC_ID --after CLIENT_ORDER_ID --reason "what IBKR shows"
  ```

  The trade is never recorded as a fill. The account then differs from the
  program's book by that trade's shares; settle it by reconciling
  (`docs/runbooks/reconciliation.md`).

### D5. The login does not manage the account

- **What you will see:** "is not among the accounts this IBKR login
  manages".
- **What it means:** the Gateway is logged in to a different IBKR account
  than the one in the configuration, so the program treats the link as not
  matched and sends nothing.
- **What to do:** compare the account number in the configurations with the
  paper account the Gateway logged in to (A3, A5). Most often the live login
  was used, or the number has a typo. A corrected account is a new
  configuration and a new run. No command is needed.

### D6. The connection to the Gateway is down

- **What you will see:** nothing goes out and nothing comes back; the
  engines hold their decisions until fresh data returns.
- **What it means:** the adapter cannot reach the Gateway, or the Gateway
  cannot reach IBKR (often the weekly phone approval).
- **What to do:** follow `docs/runbooks/gateway-login.md` and the Lightsail
  runbook's "Reconnect drill". Do not retry a bad password in a loop: IBKR
  locks the login. After a drop the adapter reconnects, matches IBKR's
  records, and does not resend (B7).

### D7. Another program uses the same client ID

- **What you will see:** IBKR error code 326.
- **What it means:** two programs are connected under one client ID. The
  adapter does not retry or pick another.
- **What to do:** stop the other program. Each engine has one ID (Boost is
  11).

### D8. IBKR refused the order

- **What you will see:** an order that ended as refused, with IBKR's reason.
- **What it means:** IBKR said no (margin, permission or price). In a paper
  account this usually means a paper-account setting, such as trading
  permission for stocks (A4).
- **What to do:** read the reason with
  `go run ./cmd/journal-query orders ENGINE_LOG`, fix the setting at IBKR,
  and ask before trying again.

### D9. A commission report never arrived

- **What you will see:** "has waited ... for IBKR's commission report".
- **What it means:** a trade happened at IBKR but cannot be recorded
  without its commission.
- **What to do:** check the trade in IBKR, then reconcile the account
  before starting again.

### D10. A report the program cannot state exactly

- **What you will see:** a stop mentioning part shares, a zero or odd
  price, or a commission in another currency.
- **What it means:** the program stops rather than record an approximation.
- **What to do:** treat it as a fault to investigate. Check the order in
  IBKR and do not start again until it is understood.

### D11. The account is Degraded or Halted

- **What you will see:** the dashboard or an alert says Degraded or Halted.
- **What it means:** the risk gate or the morning reconciliation moved the
  account out of Normal.
- **What to do:** `docs/runbooks/halted-account.md` and
  `docs/runbooks/reconciliation.md`.

### D12. The engine refuses to start

- **What you will see:** `sudo systemctl start ti-engine@boost` fails, and
  `journalctl -u ti-engine@boost -n 20` shows the reason. Nothing was
  decided or sent.
- **What it means and what to do**, by the reason it gives:
  - **An unstamped build** ("refuses an unstamped build"): the binary was
    built without its version. Build it again from a clean checkout with
    `deploy/build-engine.sh` (or `make engine` on the Mac) and install it
    (`docs/runbooks/lightsail-deployment.md`).
  - **The mode does not match**: `TI_MODE` in `engines/boost.env` is not
    the `"mode"` in `engines/boost.config` (A5). Make both say `paper`. If
    you change the configuration, that is a new run.
  - **No `run.env`**: systemd cannot load
    `/var/lib/trend-investing/engines/boost/run.env`. Create it from
    `deploy/etc/engines/run.env.example` (B4), with `TI_RUN`, `TI_AS_OF` and,
    for a first run, an empty `TI_RESTART_ARGS`.
  - **A repeated start** (the `TI_AS_OF` and configuration an earlier log in
    the folder already opens with): pick a new `TI_AS_OF`, or, if the latest
    log holds nothing past its configuration, resume that log instead
    (`docs/runbooks/durable-log.md`, "Restarting an engine").
  - **Any other reason** (a paper configuration naming an account not
    beginning `DU`, a fresh run without your approval, a resume naming a log
    that is not the latest): `docs/manual/engine.md` and
    `docs/manual/restart.md` list each one and what it needs.

### D13. The data job's warm-up fails

- **What you will see:** the warm-up command (`docs/manual/market-data.md`,
  "The one-time warm-up") prints `ti-data refused:` and a reason, and exits
  1. No file is written, so the engine reads nothing half-made, and every
  recording already fetched is kept: running the same command again fetches
  only what is missing.
- **What to do**, by the reason it gives: an empty `MASSIVE_API_KEY=` line
  (A6); a date that is not the configuration's `warm_up_start` (use that
  date exactly); a history shorter than the indicators need, or a
  `warm_up_start` outside Massive's two-year window (set a later
  `warm_up_start` in your paper configuration, A5: a new configuration, so a
  new run); the engine not started yet, or its log not readable (start the
  engine once first, B4). The full list is "What can go wrong" in
  `docs/manual/market-data.md`. Do not start paper trading until the warm-up
  has written the run's first file.

### D14. The dry-run month is not done yet

- **What you will see:** nothing stops you: the program does not check the
  plan's gates.
- **What it means:** this runbook starts the plan's Phase 4 (IBKR paper
  trading). Phase 3's gate comes first: a clean dry-run month for Boost on
  Lightsail (`docs/live-trading/plan.md` section 9).
- **What to do:** finish that month before the first paper order. The
  **[NOW]** steps in part A that only set up IBKR can be done earlier.

### D15. A session withheld after the close

- **What you will see:** an urgent page, "decision withheld
  (stale-account-snapshot)", adding "The adapter's reading after the close
  could not stand for the close (broker snapshot ...)" and a reason. In the
  engine's journal the session's `strategy.targets.decided` is `withheld`,
  and the broker snapshot before it has `as_of_close: false` and the reason
  in `not_as_of_close`.
- **What it means:** the adapter's one reading after the close could not
  stand for the account as of the close (your decision of 2026-10-18), so it
  was stamped at its read time and the engine decided nothing for that
  session. The reasons: IBKR showed an order still working for the account
  (from any client), IBKR reported an execution in the account since the
  close, a holding had no official close in the day's file, the holdings
  were not the previous close's plus or minus the run's own fills (a trade
  IBKR did not show the adapter, a transfer, a split), the cash moved by
  more than 0.5% of net liquidation beyond those fills, the reading was on
  a later date than the close (the adapter was not running that evening),
  or the engine already held a snapshot stamped at or after that close.
  Nothing was ordered; the target in force is unchanged.
- **What to do:** check the account in IBKR for the order, trade, holding
  or cash the reason names. Holdings the run's fills do not explain also
  reach the engine in that day's reconciliation, which takes the account
  out of Normal: settle them as `docs/runbooks/reconciliation.md` says. A trade you placed by hand is reported again at the next
  start's reconciliation; settle it as `docs/runbooks/reconciliation.md`
  says. Otherwise nothing: the next session whose reading passes decides as
  usual. Keep the adapter running through the evening so the reading is
  taken on the close's own date.

## The rules and their sources

- AGENTS.md rule 5: no live-order behaviour before the paper-trading and
  limited-live gates.
- ADR 0025 point 3 and point 5: a paper configuration names a paper account;
  on every start and reconnect IBKR's orders, trades and balances are
  matched before any order is accepted, and IBKR's positions and cash are
  sent for the engine to reconcile.
- ADR 0019, ADR 0025 point 5 and ADR 0026 point 1, amended 2026-10-18 (your
  decision): one reading after the close, with nothing working at IBKR and
  no execution since, and holdings and cash the previous close's adjusted
  by the run's fills, is the account as of that close, valued at the
  close; the mark is the official close, and the price band compares
  IBKR's last price with the reference price when IBKR gives one. Point 4, amended: IBKR's `ibapi` is
  pinned by checksum, fetched and verified, never committed. Point 6: IB
  Gateway runs under IBC, credentials only on the server.
- ADR 0027 and `docs/manual/redelivery.md`: journal before send, each order
  sent once.
- ADR 0022 and ADR 0026 points 2, 3 and 8: what IBKR's reports become, the
  whole-share DAY limit order, the hard limits, one client ID per engine.
- `docs/live-trading/plan.md` section 9, Phase 4: IBKR paper trading.

## Related tickets

- #330: the owner's paper account (opened).
- #397: live slice 5, paper sending; its second half (enabling the real
  client, installing `ibapi`, the first order against IBKR's paper gateway,
  the Gateway and API pairing) is what **[AFTER SECOND HALF]** waits for.
- PR #467: the first half (paper sender, `provision.sh twsapi`, the manual
  chapter), merged to `main` as d40b78b.
