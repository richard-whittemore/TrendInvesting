# Owner's manual

Plain-language chapters for the owner, written for someone who owns the
money but did not write the code. Each says what a piece does, why it
exists, which decisions (ADRs, plan sections) and tests back it, and what
can go wrong and what the program does then. Read them in the order below,
from "what is this program" to "my first paper order".

## What this program is

The program is designed to trade three IBKR accounts automatically, each
with its own rule (`docs/live-trading/plan.md`, sections 1 to 3). As of
2026-10-18 only Boost runs, in dry-run; the engine refuses a momentum
configuration and no IRA configuration exists, so the IRA rows below are not
runnable yet:

| Account | Rule | Chapter |
| --- | --- | --- |
| Joint taxable account, on margin | **Boost**: SPY at 98%, raised to 147% for a few days after a sharp drop in an uptrend | [boost](boost.md) |
| Roth IRA and Traditional IRA | **Momentum**: each month, 25 large US stocks that rose most over the past year, with a bond sleeve that grows with your age | [momentum](momentum.md), [glide path](glide-path.md) |

Everything runs on one Lightsail server ([server](server.md)): IB Gateway
logged in to IBKR, a daily data job that fetches prices from Massive, and
one **engine** per account that makes every decision and writes it down
before anything is sent. A separate **adapter** carries the engine's orders
to IBKR and IBKR's answers back. A **reporter** and a **dashboard** tell
you what happened. Nothing trades with real money until the limited-live
gate (plan section 9, Phase 5).

## Built today and not built yet

This is the one list for the whole program. Other chapters link here
rather than keep their own. As of `main` on 2026-10-18, after the adapter
began delivering the data job's daily file to the engine:

| Piece | Built today | Not built yet |
| --- | --- | --- |
| Boost rule and its order | Yes; parity passed on every session compared | — |
| Momentum rule, order planner, minimum lot, glide path | Yes; parity checked for the rule (passed once the owner's 2026-10-18 decisions were applied: `docs/research/parity-2026-10.md`) | Parity of the glide path, the minimum lot, the IRA buy delay and buy scaling; the engine program refuses a momentum configuration; no IRA configuration exists |
| Risk gate: hard limits, Normal, Degraded, Halted, kill switch, sale back to 98%, margin alarm | Yes | The adapter's second check of the limits before calling IBKR |
| Durable log, crash resume, approved fresh run | Yes ([restart](restart.md)) | — |
| Engine on the server | Boost in dry-run, started by hand (`ti-engine@boost`) | Paper: the adapter is built and installed, but nothing has run against IBKR yet |
| Daily data job | Boost's stream (`ti-data` and its timer), written for the adapter to deliver | A job for the IRAs |
| IBKR adapter | Connection, account reads, the main loop, the link to the engine's socket, a start command and unit (`ti-adapter-boost`), the real client, the paper sending switch, re-delivery and the settling commands, and delivering the data job's daily file behind a broker snapshot that carries IBKR's last price for each holding; all run against a stand-in gateway, and a whole synthetic paper day in `deploy/rehearse-paper.sh` | The paper-account test against IBKR, run by hand; a real server run with `ibapi` installed; the account snapshot as of the close (`account.snapshot`) and the recorded mark (`market.mark.recorded`) the Boost engine decides from, so in paper every session is withheld today ([paper trading](paper-trading.md), "Not built yet") |
| Morning reconciliation | The engine's comparison, states and alert | The adapter's reconciliation record (not sent until an account snapshot exists); the checks at startup, after fills and before orders; open orders; the joint account's cash; any command to record a missed event |
| Reporter and dashboard | Both programs, built with the engine and installed on the server. The reporter runs as `ti-reporter@boost` every 5 minutes around the clock, sends every alert in [alerts and dashboard](alerts-and-dashboard.md) (the margin alarm, the sale back to 98% and the sell-by-hand alerts included) and the daily summary (withheld and lapsed sales, Boost fills). The dashboard listens on `127.0.0.1:8088` | The outside heartbeat (a dead-man's switch: nothing reads `HEALTHCHECK_URL`); reporters for the IRAs (no IRA engine yet) |
| Operator tool and journal-query | Both programs, built with the engine and installed on the server, and the engine's operator inbox, created by the provisioner and passed by the service file | — |
| Server | Provisioning, the Gateway unit, the firewall, pinned downloads | Backups; a real server run |
| Day-trade counter for the joint account | — | Counted by hand ([margin warning](../runbooks/margin-warning.md)) |

**Current versions** (each chapter's "since version x" is history; these
are what this build runs):

| What | Version |
| --- | --- |
| Target-weight rules (Boost and momentum), `RulesVersion` | 1.17.0 |
| Target-weight configuration schema | 7 |
| Decision record (`strategy.targets.decided`) | 5 |
| Fresh-run approval (`operator.run.approved`) | 2 |
| IB Gateway, pinned | 10.50.1f |
| IBKR TWS API (`ibapi`), pinned | 10.51.01 |
| IBC, pinned | 3.24.2 |
| Go | 1.27.3 |

## Reading order

1. **Words.** [glossary](glossary.md): every term the manual uses,
   including the three meanings of "reconciled" and the four names one
   account goes by.
2. **The rules.**
   - [boost](boost.md): the joint account's rule end to end, its one order,
     and one Boost day from the data to IBKR.
   - [momentum](momentum.md): the IRAs' rule, the order planner, the hard
     limits in plain words, and one IRA month.
   - [momentum-universe](momentum-universe.md): how the IRAs' engine sees
     the market: the universe snapshot, price histories for the 500 it
     ranks, stocks with no sector label, a held stock with a missing day, a
     cancelled session.
   - [momentum-minimum-lot](momentum-minimum-lot.md): whole shares only: a
     stock too dear for its target is replaced, a held one is kept.
   - [glide-path](glide-path.md): how the IRAs move into bonds as you age.
3. **One day, end to end.** [One Boost day](boost.md#one-boost-day-from-the-data-to-ibkr-and-back)
   and [one IRA month](momentum.md#one-ira-month-end-to-end), each with a
   "stops here today" mark where the pieces are not yet joined.
4. **The machinery.**
   - [market-data](market-data.md): fetching, recording and encoding the
     prices the engines read, and the daily data job.
   - [engine](engine.md): the program that decides, how it starts and what
     it refuses.
   - [restart](restart.md): resuming after a crash, and the fresh run you
     approve after anything else.
   - [redelivery](redelivery.md): how an order reaches IBKR exactly once.
   - [order-lifecycle](order-lifecycle.md): reading an order's state
     (committed, released, acknowledged) from the log.
   - [ibkr-adapter](ibkr-adapter.md): what sits between the engine and IBKR,
     what runs today, every stop and the command that settles it.
   - [paper-trading](paper-trading.md): the one mode that sends, and only
     to a paper account.
5. **Safety.**
   - [states-and-kill-switch](states-and-kill-switch.md): Degraded, Halted,
     the kill switch, the margin account's one sale back to 98%, the
     margin-cushion alarm.
   - [reconciliation](reconciliation.md): the morning check of IBKR's
     positions and cash against the engine's book.
6. **Telling you, and your controls.**
   - [alerts-and-dashboard](alerts-and-dashboard.md): the reporter's alerts
     and daily summary, and the dashboard.
   - [operator](operator.md): your kill switch, approval, clearance and
     acknowledgement.
   - [journal-query](journal-query.md): asking a run's log what happened.
7. **The server.** [server](server.md): what runs where, the units and
   timers, configuration and secrets, backups, what you start by hand.
8. **Trust.** [parity](parity.md): the check that the Go code decides what
   the backtests decided.
9. **Runbooks**, for when something has happened (in `docs/runbooks/`):
   - [durable-log](../runbooks/durable-log.md): restarting an engine, a
     refused restart, a build upgrade;
   - [halted-account](../runbooks/halted-account.md): a Degraded or Halted
     account;
   - [reconciliation](../runbooks/reconciliation.md): when IBKR and the
     journal disagree;
   - [margin-warning](../runbooks/margin-warning.md): the joint account's
     margin cushion, and day trades;
   - [missed-rebalance](../runbooks/missed-rebalance.md): an IRA month
     withheld or stuck;
   - [gateway-login](../runbooks/gateway-login.md): the Gateway will not
     log in;
   - [lightsail-deployment](../runbooks/lightsail-deployment.md): setting up
     the server, and the restart and reconnect drills;
   - last, [first-paper-order](../runbooks/first-paper-order.md): the
     checklist for the first order the program sends to IBKR's paper
     servers.

The plan these chapters build towards is `docs/live-trading/plan.md`; the
decisions are the ADRs in `docs/adr/`.
