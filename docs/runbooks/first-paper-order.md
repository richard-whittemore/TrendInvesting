# Runbook: the first real paper order

*For the owner. A step-by-step checklist for the first order the program
ever sends to IBKR's paper servers: who does what, in what order, what to
look at afterwards, and what to do if it stops.*

The sending path has so far been tested only against a stand-in for IB
Gateway (a "fake gateway" that answers like IBKR, with invented accounts and
prices). This checklist is the hand-run paper-account test that ADR 0025
(Consequences) calls for, and the start of the plan's Phase 4 (IBKR paper
trading, `docs/live-trading/plan.md` section 9). Nothing here touches a live
account or real money, and AGENTS.md rule 5 still holds: the program sends
nothing live.

## What waits for what

Two pieces of work are not finished, and every step below is tagged with
the one it waits for:

- **[NOW]** Can be done today, on what is already merged.
- **[AFTER FIRST HALF]** Waits for the change that adds the paper sender, the
  `twsapi` step of `provision.sh` and the manual chapter
  `docs/manual/paper-trading.md`. It is open for review; the commands and
  files it adds do not exist on `main` until it merges. Until then, do not
  look for them.
- **[AFTER SECOND HALF]** Waits for the second half of the paper-sending
  work: enabling the real IBKR client (today it refuses to place any order),
  installing IBKR's `ibapi` into the adapter's environment, and the adapter's
  main loop (the part that keeps reading IB Gateway's messages and passing
  them on). Without it, no order can reach IBKR whatever you do. The
  work-item numbers are in "Related tickets" at the end.

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
- **Paper only.** The engine and the adapter both refuse to start in live
  mode, and refuse a paper configuration that names an account not beginning
  `DU` (see the manual's "What sends" in `docs/manual/paper-trading.md`).
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
| Checksummed download | `provision.sh gateway` and `provision.sh twsapi` fetch the pinned Linux files and refuse a mismatch | You download the Mac installers yourself and compare their SHA-256 to a value you compute and write down; nothing in the repository pins them |
| Counts as the ADR 0025 point 5 coverage test | Yes, if it ran on the pinned Gateway (10.50.1f) and API (10.51.01) | No: it proves the order path, not that IBKR's execution history covers the nightly restart on the pinned versions. That test must be repeated on the server |
| Cost and effort | Needs the instance (a purchase only you make) and the setup in the Lightsail runbook, including Tailscale | Nothing to buy; no firewall or Tailscale to set up |
| Weekly phone approval (2FA) | The Gateway logs out each Sunday and sends a prompt to your phone | Same prompt, and the Mac must not sleep: a sleeping Mac drops the link mid-session |
| Credentials | Root-only file on the server, handed to the Gateway only, rendered into memory (`deploy/libexec/render-ibc-config.sh`) | Typed into Gateway's login screen each time; keep it off the iCloud-synced Desktop |
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
  `docs/runbooks/gateway-login.md` **[AFTER FIRST HALF]**, or the "Reconnect
  drill" in the Lightsail runbook.
- **Mac mini.** Open IB Gateway, choose "IB API" and **Paper Trading**, and
  type the paper credentials into its login screen.

The program never sees these. The adapter's configuration refuses any key
containing "password", "secret" or "token".

### A4. Enable the API socket for paper [NOW]

IB Gateway only lets a program connect if you say so. In Gateway's
Configure > Settings > API > Settings:

| Setting | Value | Why |
| --- | --- | --- |
| Socket port | **4002** | IB Gateway's paper-trading port (4001 is the live one; the adapter's paper configuration must use 4002) |
| Enable ActiveX and Socket Clients | on | Without it the adapter's connection is refused |
| Read-Only API | **off** for this test | A read-only API refuses every order. On the server this is `TWS_READONLY_API=no` in `ibkr-login.env`; the shipped default is `yes`. Set it to `no` only for the paper test, and set it back to `yes` when the test ends |
| Trusted IPs | `127.0.0.1` only | The adapter runs on the same machine as the Gateway. Do not add another address, and do not open the port in any firewall: the server's firewall already allows inbound traffic on the Tailscale interface only |
| Allow connections from localhost only | on | Same reason |

Check from the machine that the Gateway listens on 4002, and nowhere you did
not intend: on the server, `ss -ltn | grep 4002` shows `127.0.0.1:4002`.
Also confirm the paper account was given trading permissions for US stocks
and ETFs in Account Management (SPY is one); a missing permission shows up
later as an IBKR refusal (part D).

### A5. Put the paper account number on the machine, and nowhere else [NOW to prepare; used AFTER FIRST HALF]

Find your paper account number (DU…) at the top of IB Gateway's window, or in
Account Management with the paper switch on. Then enter it on the machine,
by hand, in the places below. It is never committed, and the repository's
files never contain it, because the repository is public.

| Where | What | Why |
| --- | --- | --- |
| `/etc/trend-investing/secrets.env`, the line `IBKR_ACCOUNT_BOOST=` (use `sudo -e`; root-only, mode 0600) | your paper account (DU…) | The shipped file holds names only; each engine reads only its own line |
| The Boost engine's paper configuration, `/etc/trend-investing/engines/boost.config`: a copy of the shipped dry-run file with `"mode": "paper"` and `"account_id"` set to your paper account (DU…) | the mode and the account | The engine refuses to start if the mode in this file is not the mode you start it with, or if a paper configuration names an account not beginning `DU`. The file is hashed into the run (ADR 0016), so a change is a new run |
| `/etc/trend-investing/engines/boost.env`: set `TI_MODE=paper` | the engine's start mode | It must equal the mode in the configuration |
| The adapter's configuration: `mode` paper, `account_id` your paper account (DU…), `host` 127.0.0.1, `port` 4002, the engine's `client_id` (Boost is 11), the timing values and `con_ids` with SPY's IBKR contract number | the adapter's side of the binding | See B2 |

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

## B. Steps the program and the adapter take

Most of these are commands you start, but nothing in this part needs a
decision from you.

### B1. Fetch IBKR's TWS API [AFTER FIRST HALF]

```sh
sudo deploy/provision.sh twsapi
```

This downloads IBKR's TWS API 10.51.01 installer, verifies its SHA-256
against `deploy/versions.env`, and stops on a mismatch (ADR 0025 point 4,
amended). It is its own step and is not part of `all`, so a change at IBKR
never blocks the Gateway or the services. The file is not stored in the
repository (IBKR's licence terms) and the step only fetches and verifies it.

**Installing `ibapi` into the adapter's environment [AFTER SECOND HALF]**:
the second half adds that step and its command. Until then the real client
cannot connect.

### B2. The configuration binds paper mode to the DU account [AFTER FIRST HALF]

Three checks, and any one stops the order (the manual's "What sends"):

1. the engine refuses a paper configuration whose account does not begin
   `DU`, or a start mode that differs from the configuration's mode;
2. the adapter's configuration refuses the same mismatch;
3. before every order, the sender checks the order is for that account, the
   connection came from that paper configuration, and IBKR's `managedAccounts`
   list (sent at the start of each session) includes the account. If your
   login does not manage it, the link is never treated as matched to IBKR and
   nothing is sent.

### B3. Rehearse against the fake with the real account-ID shape [AFTER FIRST HALF]

Before pointing anything at IBKR, prove the checks accept your account number
and the adapter's suite is green on this machine. It sends nothing to IBKR.
In a checkout (the account number is typed at a hidden prompt, so it is not
saved in shell history or the screen):

```sh
make adapter-test
read -s -p "paper account (DU...): " ACC && echo && export ACC
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

### B4. Start the engine and the adapter in paper mode [AFTER SECOND HALF]

Order of starting, each on the server, each an explicit step of yours:

1. The Gateway is running and logged in (A3).
2. Write the run file the engine reads, exactly as the "Restarting an engine"
   section of `docs/runbooks/durable-log.md` says: a new `TI_RUN`, a
   `TI_AS_OF`, and an empty `TI_RESTART_ARGS` for a first run. A log is never
   reused.
3. Start the adapter, then the engine: `sudo systemctl start ti-engine@boost`.
   Engines are never started by systemd on their own and never restarted by
   it; every start is yours.
4. Confirm the engine's log begins with the paper configuration, and that the
   adapter says it connected on 4002, and matched IBKR's open orders, trades
   and balances ("reconciled") before anything else. The adapter does this on
   every start and reconnect (ADR 0025 point 5).

The adapter's startup command and service unit are added by the second half;
this document does not guess them.

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
| The adapter's own files beside its pending file | `.handover` (the "about to send" and "sent" marks), `.handover.decisions` (every decision you make with `relay.py resolve`), `.refused` (an order the sender refused and why), `.held` (a cancellation waiting for a fill's commission), `.corrections` and `.late-fills` (your acknowledgements) |

You never edit any of these files by hand. The adapter reads them strictly
and stops on a value it does not know.

### B7. How a reconnect is proven not to resend [AFTER SECOND HALF]

The rule is in the manual (`docs/manual/paper-trading.md`) and in
`docs/manual/redelivery.md`: on every reconnect the adapter reconnects with
the same client ID and matches IBKR's open orders, trades and balances to its
own records **before** anything else is sent. An order already marked "sent",
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
first paper order is done. Then set `TWS_READONLY_API=yes` again if you are
stopping here, and record the result: which machine, which Gateway and API
builds, the date, and what you saw. Phase 4 continues from here with the
daily checks (plan section 9).

## D. What can go wrong, and what to do

The adapter fails closed: whenever it cannot be sure what happened, it stops
and sends nothing more. The full table is "What can go wrong, and what the
program does" in `docs/manual/paper-trading.md`; the stops you may meet on a
first order, and what to do, are below. In every case: **do not place the
order by hand**, stop the adapter before using the commands (they refuse
while it runs), and never edit a journal, a `.handover` file or any number to
make a state look right. `PENDING_FILE` is the adapter's pending file;
`ENGINE_LOG` is the engine's `-log` file.

| What you see | What it means | What to do |
| --- | --- | --- |
| **Never sent.** A stop or refusal that says the order "was released for the session of ... not today's", or a refusal kept in `PENDING_FILE.refused` | The order was refused before "about to send" was written, so IBKR never received it. A link that is down or not yet matched on the same day waits and goes out when it is back. After the day ends it is stale and can never go out | If the link is simply down the same day, do nothing; do not settle it. If the day has passed: stop the adapter, confirm in IBKR (orders, including cancelled ones) that no order carries that client order ID as its reference, then `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --never-sent --reason "released for <day>, refused as stale"`. The engine then keeps the order working, so the next rebalance is withheld until you clear it with `operator rebalance-cleared` (`docs/manual/operator.md`; Boost reads no clearance, so reconcile and report it instead), reconcile, and start again. The manual's rows on a stale order and on an order the adapter can never send (such as a missing contract number in `con_ids`) have the detail |
| **Handover unconfirmed.** "was being handed to the broker when the adapter stopped" | It stopped while sending and IBKR shows the order neither open nor executed. It may have been taken and cancelled, or never arrived. The adapter will not guess | Look in IBKR for an order whose reference is the client order ID. If it is there at all (working, filled or cancelled): `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "what IBKR showed"`. Only if IBKR has no trace of it today and it was committed today: `--resend`, then start the adapter the same day. Never `--resend` an order IBKR shows at all (`docs/manual/redelivery.md`, "What can go wrong") |
| **Correction.** The adapter stops because IBKR reported the same trade again under an execution ID that differs only in its last part | Recording it would count the shares twice. It stops at every start until acknowledged | Check the trades in IBKR. With the adapter stopped: `python3 adapter/ibkr/relay.py acknowledge-correction PENDING_FILE --journal ENGINE_LOG CORRECTED_EXEC_ID --corrects ORIGINAL_EXEC_ID --reason "what IBKR shows"` (both IDs are in the stop message). It is never recorded as a fill. Reconcile the account before starting again. If it says the original "was never recorded as a fill", acknowledging cannot help: reconcile by hand first |
| **Late fill.** A trade or status reported on an order the adapter already recorded as cancelled or refused | A contradiction the engine will not record. For a trade the adapter stops at every start that day | Check the order and its trades in IBKR. With the adapter stopped: `python3 adapter/ibkr/relay.py acknowledge-late-fill PENDING_FILE --journal ENGINE_LOG EXEC_ID --after CLIENT_ORDER_ID --reason "what IBKR shows"`. The trade is never a fill; the position difference is its shares, settled by reconciling by hand (`docs/runbooks/reconciliation.md`) |
| **Unmanaged account.** "is not among the accounts this IBKR login manages" | The login the Gateway used does not manage the account in the configuration, so the link is never treated as matched and nothing is sent | Check the number in the configurations against the paper account the Gateway logged in to (A3, A5). Most often the live login was used, or the number has a typo. A wrong account is a new configuration and a new run |
| **Link down.** The adapter cannot reach the Gateway, or the Gateway cannot reach IBKR | Nothing goes out and nothing comes back. The engines withhold decisions that need fresh data; an order waits and goes out when the link is back the same day | `docs/runbooks/gateway-login.md` (a login, 2FA or "existing session" problem) **[AFTER FIRST HALF]**, and the Lightsail runbook's reconnect drill. Do not retry a bad password in a loop: IBKR locks the login. After a drop the adapter reconnects, matches IBKR, and does not resend (B7) |
| **A client-ID conflict** (IBKR code 326) | Another program is connected under the same client ID | The adapter does not retry or pick another ID. Stop the other program; each engine has one ID (Boost 11) |
| **IBKR refuses the order** | The refusal and IBKR's reason are recorded and the engine reads the order as ended | Read the reason in `journal-query orders`. A margin, permission or price refusal in paper usually means a paper-account setting (trading permissions for stocks, A4) |
| **Commission never arrives.** "has waited ... for IBKR's commission report" | A trade happened at IBKR but cannot be recorded without its commission | Check the trade in IBKR, then reconcile the account before starting again |
| **A report the adapter cannot state exactly** (part shares, a zero or odd price, a commission in another currency) | It stops rather than record an approximation | A fault to investigate: check the order in IBKR, and do not start again until it is understood |
| **The account left Normal** (Degraded or Halted) | The risk gate or reconciliation moved it | `docs/runbooks/halted-account.md` and `docs/runbooks/reconciliation.md` |

If you are unsure, the safe action is always the same: stop the adapter,
look in IBKR, change nothing, and ask. A stopped adapter costs nothing in a
paper account.

## The rules and their sources

- AGENTS.md rule 5: no live-order behaviour before the paper-trading and
  limited-live gates.
- ADR 0025 point 3 and point 5: a paper configuration names a paper account;
  on every start and reconnect IBKR's orders, trades and balances are
  matched before any order is accepted. Point 4, amended: IBKR's `ibapi` is
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
  chapter, the runbooks marked **[AFTER FIRST HALF]**).
