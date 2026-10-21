# Alerts, the daily summary and the dashboard

## What they do and why they exist

Two small programs watch an account's record of what the engine did and tell
you about it, so you do not have to go looking.

- `reporter` reads one account's log and sends your phone an alert whenever
  something needs your attention, and optionally a daily summary: what you
  hold, what the strategy wanted, the orders and fills, and whether your
  account is beating SPY.
- `dashboard` shows the same summary as a web page, one section per account,
  with a chart of your equity against SPY and a light or dark colour choice.
  It is read-only: you cannot click anything that changes the system. It shows
  the account's whole history, from its first day, across every restart and
  upgrade (see "The history across restarts" below).

Neither can place, change or cancel an order, and neither talks to the broker.
They only read.

## What they read

A run leaves one of two files, and both programs read either.

- The end-of-run journal, written when a run finishes (ADR 0017).
- The durable log a live engine streams as it works (ADR 0027). A live engine
  writes only this, so this is what you point the programs at on the server.
  Only records the engine has committed are reported; the unfinished end of a
  log from a crashed run holds nothing to trust and is ignored.

Before reporting anything the program checks the file's fingerprints, and
that every record names the same run (configuration and strategy version) as
the file's header. A file that has been edited or damaged, or that mixes
records from different runs, is refused whole, and no figure from it is
shown. The checking is the same code `journal-query` uses, so the three tools
cannot disagree about what a file holds.

Each record is then read the way `journal-query` reads it: only at a schema
version this build knows, and only if its values pass the same checks the
engine applies. That includes the configuration record that names the
account.

- A file that does not verify, a configuration that fails, or a record of
  what the engine decided (targets, orders planned or released, refusals, engine state) that
  fails is a sign of a damaged or mismatched file. The reporter then sends an
  urgent "the reporter cannot read the journal" alert naming the problem,
  still checks the heartbeat (unless the file is missing altogether or the calendar cannot answer, and then the alert says "The heartbeat is NOT checked" and why), sends nothing else from the file, and exits
  with the error. The dashboard says "journal does not verify" when the file's fingerprints fail, and
  "journal unavailable" for any other record it cannot read.
- An input that fails (a bar, an account snapshot, a fill) is skipped and
  counted. The log keeps inputs the engine itself refused, so stopping for one
  would silence every later alert. An account snapshot is also skipped when
  the engine would refuse it: one naming another account, or one no later
  than the last snapshot taken. A skipped input moves no figure, and the
  summary and the dashboard say "Inputs not read: N" when there are any.
- Bars are read in both forms the engines record: the Turtle engine's, and
  the Boost and momentum engines', which also carry a split- and
  dividend-adjusted price. The SPY side of the comparison grows by that
  adjusted price where the bars carry it, which is SPY with its dividends
  reinvested, and by the split-adjusted close where they do not (the older
  form), which leaves dividends out; the page and the summary say which.
  Your own account's figures never come from the adjusted price.
- The owner's deposits and withdrawals are read from the Flex reader's
  ledger (`-cash-ledger`, below; [reconciliation](reconciliation.md),
  "Deposits and withdrawals, read from IBKR"), not from the engine's log, so
  a transfer is counted once however it is also recorded.

## What triggers an alert

| Alert | Urgency | Sent once per |
| --- | --- | --- |
| An order was refused | urgent | refused order, except: a kill-switch refusal once per engagement (turning the switch off and on again is a new one), and a refusal for a too-old IBKR snapshot once per stale episode ([states-and-kill-switch](states-and-kill-switch.md)); a log written before the engine marked those first refusals alerts for every one |
| IBKR rejected two of the account's Boost orders in a row, whatever their targets (the page names the order whose rejection made it two, even an older order rejected late, and the rejection before it), or rejected an order after that (paged as another rejection while buys are stopped, with which one since the stop it is and the two rejections that stopped buys). The program sends no more buys for the account until you approve a fresh run (`operator approve`, adding `-clear-incident` if the run also ended in an incident). Its sells still go: Boost's exit, tried again at each decided session while due, and the margin alarm's sale back to 98%. So before any sale by hand, stop the adapter, then the engine (the kill switch is not enough: it does not stop the sale back to 98%), cancel any working program sale in IBKR's app, confirm none remains, and only then sell; start again only once your sale has filled, as a fresh run whose approval states the book as it is after your sale (`-positions ... -cash ... -currency USD`, typed right after reading the book in IBKR), or the fresh run halts on its first reading as unverifiable ([states](states-and-kill-switch.md#selling-by-hand-safely)). Find the reason in IBKR's order log first ([boost](boost.md#a-refused-or-unfilled-order-is-planned-again-re-plan)). The rule was confirmed by the owner on 2026-10-18 | urgent | each rejected order |
| A sale back to 98% was not placed because the IBKR snapshot was too old, and it opened the stale episode (sell by hand if it is needed) | urgent | stale episode; the episode's later too-old refusals send nothing more |
| A sale back to 98% was not placed because IBKR's values could not be read (`broker-values-unreadable`) or a sell this program did not release is working at IBKR (`unknown-working-sell`, CONTAINMENT REQUIRED). The program will not try again: sell by hand. The page carries the steps of [Selling by hand safely](states-and-kill-switch.md#selling-by-hand-safely): stop the adapter, then the engine, cancel every open order on the instrument, confirm none is open, sell, and start again as a fresh run whose approval states the book after your sale | urgent | withheld sale |
| A sale back to 98% was not placed because IBKR reported a net liquidation or mark of zero or less (`non-positive-broker-values`). It is decided again from the next positive snapshot, but until one comes nothing sells; the page carries the same hand-sale steps, so a sale you make cannot go out beside the program's | urgent | sale (the same sale withheld again at the next snapshot sends nothing more) |
| The risk gate refused the sale back to 98% itself as larger than IBKR's holding (`broker-holding`), beside the refused-order alert. The refusal spends the attempt, so the program will not try again: sell by hand, with the same hand-sale steps | urgent | sale |
| The margin-cushion alarm changed level ([margin warning](../runbooks/margin-warning.md)): warning (check the account the same day), urgent (the program sells back to 98% once) or clear | urgent for the urgent level; routine for warning and clear | change of level |
| A sale back to 98% was released to be sent, sized from the IBKR snapshot it names; in dry-run, released and not sent | routine | sale (its client order ID) |
| A sale back to 98% filled, in part or in full: the alert says how many shares are filled and how many are left | routine | fill (its fill ID; a resent fill is not counted again) |
| IBKR canceled or refused a sale back to 98% before it filled in full. The program will not try again: sell the rest by hand | urgent | sale |
| The engine went Degraded or Halted | urgent, repeated until acknowledged | change of state, reason and scope; the same state coming back after another is a new alert |
| A reconciliation did not match: an unexplained difference (Degraded) or evidence that could not be compared (Halted); see [reconciliation](reconciliation.md) | urgent, repeated until acknowledged | reconciliation; the state change it caused is part of the same alert, not a second one |
| A close snapshot's holdings are not the book adjusted by the fills since it (a trade the engine did not place, a fill it was never given, or a journalled fill IBKR does not show): the same reconciliation alert, its ID `close-snapshot:<snapshot ID>`, the account Degraded on the stock, or Halted when a delisting, merger or symbol change journalled since the book cannot be applied (the check is unverifiable); see [reconciliation](reconciliation.md) | urgent, repeated until acknowledged | snapshot found to differ; none when the reconciliation from the same reading already reported the same holdings and no fill or corporate action is dated between the reconciliation's reading and the snapshot's, whichever came first (a Halt is sent unless the account is already Halted for the same change) |
| A decision was withheld for missing data (bars, account snapshot, universe, the close histories of the stocks ranked) | urgent | run of the same reason |
| The same page, for a Boost session withheld as `stale-account-snapshot` because the IBKR adapter's reading after the close could not stand for the close: it adds "The adapter's reading after the close could not stand for the close (broker snapshot ...)" and every reason the snapshot states (for example an order still working at IBKR from any client, an execution since the close, a reading on a later date than the close, holdings or cash that are not the previous close's adjusted by the fills; the full list is in [market data](market-data.md), "What happens to the file"). A run's first reading that is not the book pages as an unverifiable reconciliation instead (the row above). See [paper trading](paper-trading.md), "A paper day" | urgent | run of the same reason: one page, however many sessions it covers |
| Boost had no SPY bar for a session. The page names the first missing session and says Boost skipped it and traded nothing, and decides again at the next session with a bar, its readings computed over the closes it has (the missing close is not filled in). Check the daily data job ([market data](market-data.md)); the run carries on by itself ([boost](boost.md#a-session-with-no-spy-bar)) | urgent | the gap: one page however many sessions in a row are missing; a later gap pages again |
| A momentum decision was withheld because no stock with a target was affordable (`all-unaffordable`; the net liquidation or prices may be wrong) | urgent | run of the same reason |
| A momentum decision was withheld because more than the configured share (20%) of the ranked stocks had no sector label (`labels-missing`; a sector-label source or its mapping may have failed, see [momentum universe](momentum-universe.md)) | urgent | run of the same reason |
| A decision was withheld for another reason | routine | run of the same reason |
| An order to be sent was not acknowledged by IBKR within `-ack-within` of its commit ([order lifecycle](order-lifecycle.md)) | urgent | order (its client order ID) |
| An order already alerted as unacknowledged has since been acknowledged | routine | order |
| A report from IBKR in the log that the engine did not read: it does not validate, or does not match the order it names ([order lifecycle](order-lifecycle.md)) | urgent | that report (its input ID) |
| No sign of life from the engine | urgent | silence, keyed by the last activity |
| The reporter cannot read the journal (its text says "The heartbeat is NOT checked" and why, whenever liveness is unmonitored) | urgent | account and problem |
| The reporter cannot check the heartbeat (the calendar does not cover the span) | urgent | account and problem |
| The reporter set one delivery channel aside (its secret is empty or malformed, or its name is unknown); sent on the channels that still work | urgent | account, channel and problem |
| The server's clock failed its check (the reporter runs `clock-check.sh` with `-clock-check`, after the other alerts of each firing). The page gives the measured skew, or says none was measured, and the remedy ([server](server.md), "The clock"). Sent after two failing checks in a row; not repeated | urgent | the host: one page for the whole server, whichever account's reporter saw it; a passing check resolves it, and the next failure after that is another alert |
| The daily summary (if asked for) | routine | account and day, once that day's session has closed in the log |

A sale back to 98% withheld for any other reason sends none of these
alerts: `nothing-to-sell` has nothing to do, and a lapse (`lapsed`) goes in
the daily summary. A sale the gate refused alerts as a refused order (and,
refused as `broker-holding`, with the "sell by hand" page above), and one
IBKR never reported on alerts as an order not acknowledged. The sale alerts
read the records the engine already writes: the planned sale, its release, the
fills and IBKR's reports on it. A fill or report the engine could not read
raises no sale alert; an IBKR report of that kind alerts as "a report from
IBKR the engine did not read".

A trade the momentum engine skipped (one share costs more than its target,
or it waits for a working sell: [momentum minimum lot](momentum-minimum-lot.md))
sends no alert. The daily summary and the dashboard list the day's skipped
trades under "Trades skipped", and `journal-query orders` lists each as
`skipped` ([journal-query](journal-query.md)).

"No sign of life" is measured in NYSE sessions, not hours: the alert fires
once `-heartbeat-sessions` sessions (default 2, minimum 2) have closed since the log
file last changed. Weekends and exchange holidays close no session, so a
quiet weekend or a long holiday weekend never pages you. With the default,
an engine that stops after writing Monday's session is reported at
Wednesday's close. The calendar is the same NYSE calendar the market-data
adapter uses, and covers 2022 to 2030; outside those years the reporter
exits with an error rather than guess, and also sends an urgent "the reporter
cannot check the heartbeat" alert, once per distinct problem, so a timer run
never fails silently. The minimum is 2 because a session's close is counted
before the engine writes that session's records, so 1 would page every
trading day. A reporter running on the engine's
own machine cannot catch that machine dying; the outside dead-man's switch
ADR 0019 asks for is a separate service and is not built.

If the log cannot be read, the alert is named by the account the log names,
or by `-account`, or by the log file's name.

## Repeated until you acknowledge it

A Degraded, Halted or reconciliation alert is sent again, on every channel,
each `-repeat-interval` (default 30 minutes, your setting under ADR 0019),
for as long as you have not acknowledged it, overnight and at weekends too
(below, "When the reporter runs"). Every other alert is sent once. ADR 0019
asks for "a fixed interval" and leaves its length to you; 30 minutes is the
program's default and the server's setting, not a recorded decision of yours.
Change it with `-repeat-interval` in the unit if you want another.

The alert's text ends with its **alert key**. You acknowledge it by recording
an `operator.alert.acknowledged` input naming that key, which the engine
writes to the log like any other input; the reporter reads it there and
stops repeating that alert. Acknowledging changes no state: the account stays
Degraded or Halted until recovery (`docs/runbooks/halted-account.md`, and
`docs/runbooks/reconciliation.md` for a reconciliation).
Record it on the server with `operator acknowledge`, pasting the key from
the alert ([operator](operator.md), "Acknowledging an alert"). The engine
journals it only when it takes its next input from the adapter, so with the
engine stopped the repeats continue until it runs; the reporter's first run
after that sends the alert no more.

Only an acknowledgement for the account counts. One recorded for another
account has no effect, so the alert keeps coming until an acknowledgement for
this account is in the log. One the engine refuses (its time is more than five
minutes ahead of the engine's clock, or it cannot be read) is never recorded;
you see a `.refused` note saying why in the engine's notes folder beside its inbox (`operator-notes`) and
on the engine's output (`journalctl -u ti-engine@<instance>`). Record the
acknowledgement again with the right account and the current time.

On both channels, ntfy and PagerDuty, repeats come every `-repeat-interval` (default 30 minutes). On PagerDuty, each repeat opens a **new incident**: the first alert's
incident is named by the alert key, and each repeat's by the alert key with
`#repeat-1`, `#repeat-2` and so on, while the alert key itself is shown in
the incident's details. So until the acknowledgement is recorded in the
journal you will see one more incident, and one more page, every
`-repeat-interval`. Acknowledging or resolving a PagerDuty incident on your
phone stops that incident's escalation only; it does not stop the repeats,
which follow the log. Once the journal records the acknowledgement, no further
incidents open; resolve the ones already open in PagerDuty yourself.

A repeat is timed per channel from when that channel last took the alert, and
recorded in the state file with how many repeats that channel has taken. A
state file written by an earlier reporter has no send times, so its
unacknowledged alerts repeat once at the next run.

## When the reporter runs

The reporter checks the log only when it runs. On the server
`ti-reporter@boost.timer` runs it **every 5 minutes, around the clock, seven
days a week** (New York time). So, at any hour:

- an alert is sent at most 5 minutes after the engine writes its record;
- a repeat comes every 30 minutes (a multiple of 5, so on time);
- an order IBKR has not acknowledged within `-ack-within` (10 minutes) of
  its commit is reported at most 5 minutes after that, so within 15 minutes;
- the daily summary goes at the first run after the log records that day's
  session closed.

An alert that arises overnight or at a weekend (a reconciliation at the
reconnect after the Gateway's nightly restart, a lost heartbeat, a refused
journal) pages you then, and **its repeats continue overnight, every 30
minutes, until you acknowledge it**. To silence one, acknowledge it with
`operator acknowledge` ([operator](operator.md), "Acknowledging an alert");
acknowledging changes no state. If you want quiet hours, that is your
decision to make: narrow the timer's `OnCalendar=` and record the choice
here. A run that takes more than 4 minutes is stopped, so the next firing is
never skipped; each delivery is saved as it is made, so a run stopped
between a channel taking an alert and the save sends that one alert again on
that one channel (delivery is at-least-once: PagerDuty folds the repeat by
its key, ntfy posts it twice); every other alert is not sent again. `TestTheTimerMakesTheRepeatAndAcknowledgementBoundsTrue`
(`cmd/reporter`) checks that the timer runs around the clock, that its
period divides the repeat interval and is at most half the acknowledgement
bound, and that a run is stopped before the next firing.

## Where alerts go

Alerts go to real channels. You choose them with `-channel`, comma-separated.

| Channel | What it is | Secret |
| --- | --- | --- |
| `ntfy` | Push to your phone through ntfy (the public ntfy.sh relay unless you run your own server) | `TI_NTFY_TOPIC`, an unguessable topic name; optionally `TI_NTFY_SERVER` |
| `pagerduty` | The second channel, chosen by the owner: PagerDuty, through its Events API | `TI_PAGERDUTY_ROUTING_KEY`, the integration key; optionally `TI_PAGERDUTY_URL` |
| `stdout` | Prints to the terminal or the system log | none |

Each secret is read from the environment, or, when `-secrets-file` names a
file of `KEY=VALUE` lines, from that file for any name the environment leaves
empty. On the server the secrets go in `/etc/trend-investing/secrets.env`
(root only, mode 0600; the names are in `deploy/etc/secrets.env.example`),
which systemd hands the reporter as a private credential file; nothing puts
them in its environment. A secrets file that cannot be read, or a line in it
that is not `KEY=VALUE`, is reported in the run's error, naming the line's
number, never its text; every line that reads is still used.

Each channel is checked on its own. A channel whose secret is empty or
malformed (an empty topic or routing key, a server or endpoint that is not a
URL), or whose name is unknown, is set aside: the other channels still get
every alert, an urgent alert "the reporter cannot deliver to" that channel
goes to them, the daily summary ends with "Channels not delivering:" and the
channel's name and why, and the run ends with an error naming it. Only when
no channel that reaches you is left does the run stop before sending
anything.

`stdout` is only an echo for whoever is watching the terminal. It reaches no
one else, so it never counts as a delivery: `-channel stdout` alone is refused,
and a run that has only echoed an alert will send it again next time.

The delivery rules:

- An alert counts as delivered only when every real channel you named has
  taken it. One channel working does not make up for another failing.
- A channel that failed is retried on the next run, and only that channel.
  A channel that already took the alert is not sent it again.
- Each send has a time limit (`-send-timeout`, default 30 seconds). A channel
  that does not answer in time has failed and is retried next run, so one
  stalled channel cannot hold up the other alerts.
- On the public ntfy.sh relay, a reconciliation alert is sent in a shorter
  form that names what is in doubt (the instruments and the cash) but no
  figure; the figures go to PagerDuty and to your own ntfy server. When the
  reconciliation could not be verified (Halted), the short form says only
  that, and that you should look at the private channel: the reasons it could
  not compare can carry amounts and share counts, so they go to PagerDuty and
  your own ntfy server alone.
- The daily summary carries your balances, so it never goes through a public
  relay. With ntfy on the public ntfy.sh server the summary skips ntfy and goes
  only to PagerDuty or your own ntfy server. If no such channel is named, the
  incident alerts still go out and the run ends with an error saying the
  summary had nowhere private to go.
- The summary for a day is sent only once the log records that day's session
  closed. The day's decision and orders are written with the close, so a
  summary sent earlier would be incomplete, and, being sent once per day,
  would never be replaced. Before the close the summary is simply not due.
- If the summary cannot be built (a decision record the program cannot read),
  the incident alerts still go out and are remembered, and the run ends with
  an error naming the record.
- The reporter remembers what each channel has taken in its own state file
  (`-state`). That file is the only thing it writes, and it is flushed to disk
  before the run ends, so a power cut cannot leave it empty. If it is
  unreadable the reporter stops rather than starting over, because starting
  over would resend every alert.
- Secrets (the ntfy topic, the PagerDuty key) never appear in output or errors.

PagerDuty details: you sign up for PagerDuty and create a service with an
Events API v2 integration, which gives the routing key. The reporter triggers
an incident for each alert, using the alert's own key as PagerDuty's
de-duplication key, so a resend joins the incident it would have opened.
PagerDuty refuses a key over 255 characters, so a longer one (an engine state
naming many instruments or orders) is sent as its first part followed by a
fingerprint of the whole key; the full key is in the incident's details. It
reads PagerDuty's answer back: only an "accepted, success" answer is a
delivery. A refusal, a throttle or an answer it cannot read is a failure. That
is the machine acknowledgement. Acknowledging the incident yourself on your
phone is PagerDuty's job (its escalation rules), not the reporter's, because
the reporter has no way to listen for it.

## Running the reporter

On the server it runs as `ti-reporter@boost.service`, started by its timer,
with:

```
/opt/trend-investing/bin/ti-reporter \
  -journal /var/lib/trend-investing/engines/boost/${TI_RUN}.log \
  -state /var/lib/trend-investing/reports/boost.reporter.json \
  -secrets-file %d/secrets -channel ntfy,pagerduty -summary -account boost \
  -cash-ledger /var/lib/trend-investing/flex/boost/cash-movements.jsonl
```

`TI_RUN` comes from the engine's `run.env`, which systemd reads again at
every firing, so after a fresh run the next firing reads the new log. The
state file is per account, not per run, so an alert already delivered is not
sent again from a later log. The reporter only reads the engine's directory
and writes only in `/var/lib/trend-investing/reports`. Check a run with
`systemctl status ti-reporter@boost` and `journalctl -u ti-reporter@boost`.
Only the Boost account's timer is enabled: the IRA engines have no
configuration yet.

On the Mac, from the repository root, against a copy of a log:

```
TI_NTFY_TOPIC=... TI_PAGERDUTY_ROUTING_KEY=... \
  go run ./cmd/reporter -journal boost.log -state boost.reporter.json \
  -channel ntfy,pagerduty -summary
```

Flags: `-journal` and `-state` are required; `-secrets-file` (a
`KEY=VALUE` file read for any secret the environment leaves empty); `-channel` (default `stdout`,
which is refused on its own, so always name a real channel); `-summary`
(also send the day's summary); `-date YYYY-MM-DD` (default: the latest day
whose session has closed in the log; a stated day whose session has not
closed is an error); `-benchmark` (default SPY); `-heartbeat-sessions`
(default 2 NYSE sessions with nothing written); `-account` (the name for
alerts when the log cannot name its account; default the log file's name);
`-send-timeout` (default 30 seconds per send); `-repeat-interval` (default 30
minutes; how often an unacknowledged Degraded, Halted or reconciliation alert
is sent again; must be positive); `-ack-within` (default 10 minutes: how long
an order to be sent may go unacknowledged by IBKR, from its commit, before an
alert; it must be positive); `-cash-ledger` (the Flex reader's ledger of the
account's deposits and withdrawals, `/var/lib/trend-investing/flex/<instance>/cash-movements.jsonl`,
the same file the reader writes; without it, or until the reader has written
it, the summary says the comparison assumes no transfer. It is accepted only
from the account's own folder, `flex/<instance>`, where `<instance>` is the
name of the engine folder the `-journal` is in; any other is not read, and
the summary says "ledger not accepted: it belongs to <x>, not <y>").

If it exits with an error, read it: it names the channel that failed or the
file it refused. Nothing is lost; the next run tries again.

## Running the dashboard

On the server it runs as `ti-dashboard.service`:

```
/opt/trend-investing/bin/ti-dashboard \
  -engine-dir /var/lib/trend-investing/engines/boost \
  -cash-ledger /var/lib/trend-investing/flex/boost/cash-movements.jsonl \
  -addr 127.0.0.1:8088
```

Port 8088 is the program's default, the unit's, and the one the Lightsail
runbook publishes to your tailnet (`sudo tailscale serve --bg 8088`). It reads
no secret and is given none.

`-engine-dir` names the folder the engine keeps its logs in. On every request
the page finds the account's current run there, the newest link of the chain
of runs (ties decided by the rule the engine uses to find the latest run at a
start), so after a restart or an upgrade of the engine the next page load
already shows the new run. You never restart the dashboard to follow
the engine. (Restart it only to run a new build of the dashboard itself:
`provision.sh services` does that for you at the upgrade step of the Lightsail
runbook, which is why that step already ends with a dashboard restart.)

`-cash-ledger` names the Flex reader's ledger of the account's deposits and
withdrawals, read afresh on every request like the log. With one
`-engine-dir` the path alone is enough; with several, or with any
`-journal`, write each as `SOURCE=PATH`, where SOURCE is exactly the
`-engine-dir` or `-journal` it belongs to, for example
`-cash-ledger /var/lib/trend-investing/engines/trad-ira=/var/lib/trend-investing/flex/trad-ira/cash-movements.jsonl`.
A ledger that names no account given, or a second ledger for one account, is
refused at start: a deposit mirrored on the wrong account would misstate
both. A ledger is also accepted only from the account's own folder,
`flex/<instance>`, where `<instance>` is the name of the `-engine-dir` (for
a `-journal`, of the engine folder it is in), the layout the server keeps;
a ledger from another folder is not read, and the card says "ledger not
accepted: it belongs to <x>, not <y>". An account given no ledger is
compared assuming no transfer, and its card says so.

### The history across restarts

Every restart or upgrade of the engine is a new run with its own log, and each
new run's approval names the run before it and that log's final fingerprint.
The page follows those names back to the account's first run, checking each
log against the fingerprint the approval named, and draws the equity
comparison from the first run's first close: the chart, the returns since the
start and the "first paired point" date are the real first day, not the day of
the last restart. Everything else on the page (holdings, engine state, the
day's orders and fills, alerts, "Inputs not read") is the current run's, as
before. The daily summary the reporter sends follows the chain the same way.

Each earlier log must also be the same account's as the current one, so
another account's log never joins the history. A run with no approval counts
as a first run only in a dry run, whose first run starts without one; in paper
or live a log with no approval states no starting book, so it is a broken
link. An earlier dry-run log holding nothing past its configuration is a
broken link too, since it cannot be told from a start cut short before its
approval was recorded.

The current run is different: no other log names it, so a current log holding
only its configuration is a run still starting (a dry run's first run waits
there for its first input). The equity card says "the run is starting; no
history yet", and nothing is alerted: there is nothing to restore (the daily
summary is not due until a session closes). In paper or live the engine
records the approval itself at once, straight after the configuration, so a
log that stays at its configuration is a start that crashed before its
approval was recorded: the card then says "the run started but recorded no
approval; if the engine is not running, resume it with its approval (see
"Restarting an engine")". Resume it with `-resume` and the `-approval` it was
started with ([restart](restart.md)).

The mode (dry-run, paper or live) may change along the chain: the engine
accepts a paper run approved against a dry-run log, and a live run against a
paper log, so such a link is followed and checked like any other and is not a
break. The equity comparison is one mode's, though: it holds only the runs in
the current run's mode, from the earliest of them since the last change of
mode, and the card and the daily summary say "history since <date>, the first
<mode> run" (the date that run started), so a dry run's or a paper run's
figures never join a live run's. If the mode changed and then came back (paper,
then live, then paper again), the comparison starts at the latest change: the
line says "history since <date>, the first paper run since the mode last
changed; the paper runs before that change are not included", and those
earlier paper days are left out.

The current run is the log no other log in the folder names as its previous
log, the newest link of the chain. The page finds it from each log's first
records, its configuration and approval, so it reads no more of the other logs
than that. The one exception is a tie: after a resume, the resumed run's log
and the log it copied begin with the same records, so neither names the other,
and the page reads both whole to pick the one the engine would. The copied log
is then kept and read again only when it changes, so after the first request
only the current log is read whole. An earlier run's log is read whole once,
for its fingerprint and its share of the equity history, and read again only
when its size or modification time changes; the current log is read afresh on
every request.

If a link cannot be followed (an earlier log is missing, was changed, is
another account's, or the approvals loop back on themselves) the history
never costs you an alert or a figure of the current run. The page still shows
the current run's holdings, engine state, alerts and day's figures; only the
equity card says "history unavailable: the chain of runs is broken between
<log A> and <log B>", naming the two logs the broken link joins, and draws the
comparison from the first intact run it includes, saying so (the first
intact run of the current mode, when the intact runs also cross a change of
mode). The reporter still sends every alert and the daily summary, whose
equity section gives that line and then the comparison over the intact runs,
"Equity vs SPY, from <log>, the first intact run it includes, <date> to
<date>, time-weighted: ...", the same figures the card shows,
and tells you of the break once, in a routine alert, "<account>: the history
of earlier runs is unavailable".

The remedy: restore the named earlier log from a backup of it as the engine
wrote it, beside the others, **keeping its modification time, and before any
restart of the engine**. The engine finds the latest run by modification
time, so a log restored with the time of the restore looks like the newest
run, and the engine's next start would take it for the latest run and ask you
to approve a new run against it. It must also be owned by `ti-trader`, the
user the engine, the reporter and the dashboard run as: run as root, `rsync -a`
and `cp -p` keep the backup's owner, and a log they cannot read is still a
broken link. Restore it with a command that keeps the time and sets the owner,
for example (the backup's path is yours):

```
sudo rsync -a --chown=ti-trader:ti-trader /path/to/backup/run1.log /var/lib/trend-investing/engines/boost/
```

or `sudo cp -p /path/to/backup/run1.log /var/lib/trend-investing/engines/boost/`
followed by `sudo chown ti-trader:ti-trader /var/lib/trend-investing/engines/boost/run1.log`
(`chown` leaves the time alone). Then check the times and the owner:

```
ls -l --time-style=full-iso /var/lib/trend-investing/engines/boost/
```

The restored log must be older than the current one, and its owner and group
must read `ti-trader ti-trader`, like the other logs. If it is not older, the page
shows "<restored log> looks newer than the current run <log>; fix its
modification time before the next restart" above the account's figures, and
the reporter sends that once in a routine alert, "<account>: a restored log
looks newer than the current run". Restore it again with `rsync -a --chown=ti-trader:ti-trader`
(or `cp -p` then `chown`) from the backup, or set its time back with
`sudo touch -d '<the backup's own time>' <restored log>`, where the backup's own time is the
modification time `ls -l --time-style=full-iso /path/to/backup/run1.log` shows for the backup
copy, for example `sudo touch -d '2026-10-01 12:00:00.000000000 -0400' <restored log>`. That
time must fall between the times of the logs on either side of it, so that the folder is in
its true order: list the folder sorted by time, oldest first, with
`ls -lrt --time-style=full-iso /var/lib/trend-investing/engines/boost/`, and check the
restored log stands where its run belongs, older than every log written after it, not only
the current one. Check again before you restart anything.

If the engine was already restarted after such a restore, it refuses to start
with "the approval in <file> names <current log>, but the latest earlier run
in <folder> is <restored log>; approve the latest run". **Do not approve the
restored log**: that would start a new run against an old one and leave the
runs after it out of the chain. Fix the restored log's time instead, with
`sudo touch -d '<the backup's own time>' <restored log>` (or
restore it again with `sudo rsync -a --chown=ti-trader:ti-trader`), check with
`ls -lrt --time-style=full-iso` that it now stands before every log written after it, and
start the engine again with the approval you already have, the one naming the
current log.

If you restarted the engine with `-resume` instead, after the same bad restore,
it refuses with "-resume names <current log>, but the latest run in <folder> is
<restored log>: only a crash is resumed, and the crashed run is the latest one;
resume that log, or start a fresh run with -approval". **Do not resume the
restored log either**: that would copy an old run's records into a new log and
continue from its state, leaving every run after it out of the chain. Fix the
restored log's time the same way first, then resume the current log again.

If it cannot be recovered, the history starts at the first intact run from
then on, and the page says so. Never put back a log the engine told you to
move into `quarantine/`: the engine reads every log in its folder at a start
and would refuse to start again. The earlier logs are never edited.

On the Mac:

```
go run ./cmd/dashboard -journal joint.log -journal ira.log -addr 127.0.0.1:8088
```

Repeat `-journal` for each account (it follows the chain of runs back from that
log), or give `-engine-dir` a folder of logs to show the current run in it. It will listen only on a loopback address
or an address this machine's own Tailscale interface holds (`tailscale ip`
prints it), so on the server it is reachable only over Tailscale. An address
that merely looks like a Tailscale one but belongs to another network is
refused.
Pages: `/` (the summaries, with each alert's full text under its title),
`/api/summary.json` and `/healthz` (which answers "ok"). Anything but a
read (GET or HEAD) is refused with "405 Method Not Allowed" and the text
"read-only dashboard". Every request reads and checks the log afresh. If a log
does not verify the page says "journal does not verify"; if it cannot be read
for any other reason (missing, not readable, a record that fails its checks)
it says "journal unavailable". Neither shows paths or figures. With several
`-journal` files one bad file blanks the whole page, not just its section. An
`-engine-dir` account's trouble shows on its own card, named for the folder
("<account>: journal unavailable", "<account>: journal does not verify", "no
run yet" or "recorded nothing past its header"), and every other account still
shows in full. In `/api/summary.json` such an account carries only `Name` and
`Message`: it has no `Summary` and no `Alerts`, so it cannot be taken for an
account with a zero balance.

### Reading the page

Nothing on the page can be clicked to change anything; the links only move you
around the page, to the two data pages, or switch the colours. From the top:

- **The header** says "Read-only" and holds the colour switch. Choose Light or
  Dark and the page reloads in that colour scheme (the address gains
  `?theme=light` or `?theme=dark`). With no choice the page follows your
  device's own light or dark setting. The choice lives only in the address,
  so it is not remembered: bookmark the address with the choice if you want
  it every time. The page runs no scripts and loads nothing from anywhere
  else.
- **The left column** lists each account with its engine state (normal,
  degraded or halted, written out as well as coloured) and links to
  `/api/summary.json` and `/healthz`. On a narrow screen it moves above the
  rest.
- **The row of figures** gives each account's net liquidation (what the
  broker says the account is worth) and its engine state. Click one to jump to
  that account.
- **One section per account.** Its first card is the equity comparison.
  Below it come the day's holdings, the targets the strategy decided, the
  orders, the refusals, the fills, and, only on a day that has them, the sales
  back to 98% that were withheld and the trades skipped. "Inputs not read"
  appears when the program left out an input that failed its checks. The
  account's alerts close the section.

**The equity card** compares your account with SPY over every date on which
the program has both your account's value and SPY's close (a *paired day*).
How the SPY side is built is in "How the comparison with SPY works", below.
From the top:

- **The verdict**, one line in words: "Beating SPY by 1.3 points since
  2027-03-01", "Behind SPY by 0.8 points since 2027-03-01" ("1.0 point"
  when it is exactly that), or "Level with SPY since 2027-03-01" when the
  two returns are less than 0.1 point apart.
  The date is the first paired day. It is green with an up arrow when you are
  ahead, red with a down arrow when behind, and plain with "=" when level, so
  colour is never the only cue. Beside it, the **dollar gap**: your net
  liquidation less the SPY shadow account's value on the latest paired day.
  When your recorded withdrawals took the SPY shadow account to zero, the
  verdict stays (it does not depend on the shadow's dollars) but the dollar
  gap and the dollar line are replaced by a sentence saying so (below).
- Your net liquidation and when it was read.
- The three chips: your time-weighted return, SPY's, and the difference
  between them in points (the number the verdict rounds).
- **The dollar line**: "Account 16000.00 · SPY shadow account 16000.00 · gap
  +0.00 on 2027-03-04", what each is worth in dollars on the latest paired
  day.
- The whole-period and the latest-day changes in words. The day's change is
  from the paired day before to the latest, with any transfer that day taken
  out.
- A line chart of the two time-weighted growth lines, each the growth of the
  starting balance, rebased to 100 on the first paired day, so a deposit or a
  withdrawal does not bend either line. Your account is the solid line with
  circles, SPY the dashed line with squares, so the two can be told apart
  without colour. The figures at the right-hand end are where each line
  stands on the latest paired day, which can be older than today when
  today's snapshot or close is missing; the dates under the chart are its
  first and last paired dates. The chart spaces paired dates evenly, by
  paired point and not by calendar, so a missing day leaves no gap.
- The number of paired points, the first and last dates, then one sentence
  saying exactly what is counted (below).
- The first, previous and latest points with your net liquidation and SPY's
  close (the price as quoted), and what the SPY shadow account is worth.

**The change-between-paired-points card** draws, for each pair of consecutive
paired dates, a pair of bars: your account's change on the left and SPY's on
the right, each time-weighted, so a deposit is not drawn as a rise. A bar is the change from one paired date to the next, which can
span several sessions when a snapshot or close is missing, so it is not always
one day's move. Like the line chart, it spaces paired dates evenly, by paired
point and not by calendar. It shows at most the most recent 120 changes (121
paired points), so the bars stay wide enough to see; the dates under it are
the first and last of those points. Bars above
the centre line are rises and bars below are falls, on one scale that is
printed at the top and bottom. Your account's bars and SPY's are in the
account and benchmark colours and are told apart by position, left and right;
the sign is whether a bar sits above or below the centre line. Green and red,
with up and down arrows, appear only on the return chips above the line chart,
and are never the only signal there either.

If there are fewer than two paired dates, for example early in a run, a chart
cannot be drawn. The card says "The chart needs two paired points" instead of
drawing something misleading.

### How the comparison with SPY works

The question the card answers is: **would the same money have done better
in SPY?** "The same money" means the balance you started with and every
deposit and withdrawal you made since, each at the moment you made it.

**The SPY shadow account.** On the first paired day the program imagines a
second account holding your whole net liquidation in SPY at that day's
closing price. From then on:

- each deposit you make buys SPY in the shadow account for the same amount,
  and each withdrawal sells SPY for the same amount, at the official close of
  the New York day the transfer is dated (IBKR usually dates a transfer by
  its day alone; that day's close is used). A transfer dated on a weekend or
  holiday is mirrored at the next session's close. A transfer on or before
  the first paired day is already in the starting balance;
- SPY's dividends are reinvested in SPY on their ex-date. This comes from
  the split- and dividend-adjusted price the market-data job records
  ([market data](market-data.md), "Adjusting"): its day-to-day ratios are
  exactly SPY held with each dividend bought back in SPY.

Fees, commissions and interest are not mirrored: they are part of running
the strategy, so they stay in your account's own result.

The shadow account cannot sell SPY it does not hold. If a withdrawal is
larger than the shadow account is worth that day (your account did much
better than SPY, then you took most of it out), the shadow account stops at
zero on that day. A withdrawal of exactly its value does the same. The
verdict and the time-weighted returns are still shown, because they do not
depend on the shadow's dollars, but the dollar gap and the dollar line are
withheld and replaced by "the recorded withdrawals reached or exceeded the SPY shadow's
value on <date>; dollar figures are not meaningful after that day". The
shadow account restarts at the next deposit you make: that deposit buys SPY
at that day's close, the dollar figures return from that day, and the
sentence becomes "the recorded withdrawals reached or exceeded the SPY shadow's value
on <date>; dollar figures restart from <date of the deposit>". The restarted
shadow holds only what you deposited since, so its dollars stand for that
money, not for the original starting balance.

**Time-weighted returns.** Both percentages are time-weighted: each stretch
between paired days counts only the market's move, with the money you added
or took out that day removed, and the stretches are multiplied together.
That way a deposit is never a gain, a withdrawal never a loss, and the two
returns compare fairly whatever the size or timing of your transfers. The
dollar line shows the other view, what each account is actually worth; the
two can disagree in sign when a large deposit came just before a fall or a
rise, which is why both are shown.

**What the sentence says.** It states exactly what is counted:

- "SPY with dividends reinvested" when every day-to-day step of the period
  uses the adjusted price; "SPY price return only for <from>–<to>; dividends
  reinvested for the rest" when some steps do not, naming each stretch that
  is price only (more than one is listed, "a–b and c–d"): a stretch of bars
  of the older form, or the one step where the adjusted price's base
  changes, at a fresh run with another warm-up start, since levels on two
  bases cannot be compared; and "SPY price return only: dividends not
  counted" when no step uses it;
- "SPY close on <date> is not positive: that session is left out" for a
  close that cannot be a price; the step runs from the session before it to
  the one after;
- "your deposits and withdrawals mirrored as recorded by the Flex reader (N
  in this period: X in, Y out)" when the ledger was read, followed by how far
  the reader has recorded: "transfers recorded through <day>", the
  `covered_to` of its status file beside the ledger
  ([reconciliation](reconciliation.md), "The status file"). The reader runs
  at 21:30 New York, after the close's summary is sent, and IBKR may post a
  day's transfer only the next evening, so when the summary's day is later
  than that it adds "deposits or withdrawals from the last two trading
  sessions may not be recorded yet; a transfer appears in the summary two
  trading sessions after it posts (a Friday transfer on Tuesday's)". A
  transfer posted on day D is first in the statement the reader reads on the
  evening of the next session, after that day's summary is sent, so the
  summary two sessions after D is the first to include it. A session is a
  New York Stock Exchange trading day: weekends and exchange holidays are not
  sessions and are skipped, so a Friday (or weekend) transfer is in Tuesday's
  summary, a Thursday one in Monday's, and one posted before a holiday waits
  for the extra session. When the status file gives no
  `covered_to` (no reading yet, or the last one running or set aside) it
  says "how far the Flex reader has recorded is not known; deposits or
  withdrawals from the last two trading sessions may not be recorded yet". A
  reading that found no transfer writes no ledger; its status file still
  counts, and the ledger is taken as read and empty;
- "ledger not accepted: it belongs to <x>, not <y>; the comparison assumes
  none ..." when the ledger named is in another account's folder;
- "deposits and withdrawals not yet recorded: the comparison assumes none,
  so money you add or take out counts as the account's own gain or loss"
  when no ledger is given or the reader has not written one yet, and
  "deposits and withdrawals not readable (<why>): the comparison assumes
  none ..." when it exists but cannot be read. A transfer then shows as the
  account's own gain or loss until the ledger is read: the comparison never
  silently assumes none.

**A worked example** (made-up numbers). You start on Monday 2027-03-01 with
10,000.00, all in SPY at a close of 100.00, so the shadow account holds 100
shares.

| Day | What happens | SPY close | Your account | Shadow account | Your return | SPY's return | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Mon 03-01 | Start | 100.00 | 10,000.00 | 100 shares, 10,000.00 | | | |
| Tue 03-02 | SPY rises 10% | 110.00 | 11,000.00 | 100 shares, 11,000.00 | +10.00% | +10.00% | Level |
| Wed 03-03 | You deposit 5,000.00 | 110.00 | 16,000.00 | buys 45.4545 shares: 145.4545 shares, 16,000.00 | +10.00% | +10.00% | Level |
| Thu 03-04 | SPY goes ex-dividend, paying 1.00 a share | 109.00 | 16,000.00 | the 145.45 paid buys 1.3344 shares: 146.7889 shares, 16,000.00 | +10.00% | +10.00% | Level with SPY since 2027-03-01, gap +0.00 |

On Wednesday your account jumped from 11,000.00 to 16,000.00, but 5,000.00
of that is your own money: the time-weighted return takes it out, (16,000 −
5,000) / 11,000, no change that day, so you are still +10.00%. Had the deposit
been counted as a gain, the card would have said you were up 60% and beating
SPY by 50 points. On Thursday SPY's price fell by the dividend it paid, 110.00
to 109.00; your account, holding SPY, received the cash, so its value did not
move. Counted as price only, SPY would show +9.00% and the card would say you
were beating SPY by 1.0 point; with the dividend reinvested SPY's holder is
level too, and the verdict says so.

## What the summary shows, and its limits

- Holdings come from the broker's account snapshot.
- Each released order says "released (send requested)" or "released (not
  sent)". The engine records a release before it sends, so a release is only
  possibly sent; the summary does not yet say a broker confirmed it. A dry-run
  releases orders without sending them.
- Equity against SPY pairs the snapshot's net liquidation with SPY's closing
  price on the same day, and needs two paired points before it shows a
  comparison. The summary then opens with the same verdict line as the card,
  with the dollar gap and both accounts' values beside it ("Beating SPY by
  1.3 points since 2027-03-01; dollar gap +130.00 (account 13130.00, SPY
  shadow 13000.00)"). The returns are time-weighted, against the SPY shadow
  account ("How the comparison with SPY works", above), and the sentence of
  what is counted follows them. The comparison names the dates it runs
  between; the last date can be earlier than the summary's day when that day
  has no paired snapshot and close.
- The day's change compares the summary's day with the paired point before
  it, with that day's transfers taken out, and names both dates, so a gap of
  missing days shows as a longer span.
  When the summary's day has no paired point it says "Day: not available"
  rather than showing an earlier day's change as today's.
- Fills are read in both forms the engines record: the Turtle engine's and
  the Boost and momentum engines' (which name the side and the client order
  ID). Neither is counted in "Inputs not read".
- Every sale back to 98% withheld that day is listed under "Sales back to 98%
  withheld", with its reason, what fired it and the engine's detail. A lapse
  (`lapsed`: what fired the sale ended before a good snapshot came) appears
  only here, never as an alert.
- Every trade the program sized but did not order that day (ADR 0026, point
  8) is listed under "Trades skipped", with its client order ID, side,
  instrument, the shares the rule asked for (to at most six decimals; a smaller size is shown in scientific notation) and its reason
  (`below-one-share`, `scaled-below-one-share` or `held-by-working-sells`;
  [momentum minimum lot](momentum-minimum-lot.md)). The dashboard shows the
  same list.
- When the reporter set a channel aside, the summary it sends on the others
  ends with "Channels not delivering:" and each such channel and why.
- With the IBKR adapter, the summary shows the cash checks of the latest
  reading after the close (ADR 0019, amended 2026-10-18): "Cash drift since
  the previous close", the day's difference the adapter accepted (at most 1%
  of net liquidation), and "unexplained cash since the cash was last
  explained", that difference added up since a reconciliation the engine
  explained, or since the latest starting book you approved (for Boost
  always that starting book, since its book carries no cash; approving a
  fresh run with a new starting book starts the figure again from it). A figure that keeps growing is a posting no trade
  explains: check the account in IBKR. When the latest reading was stamped
  at its read time it says "Cash drift: not checked".

## Which chapter explains each alert

Find the alert's title here, then read that chapter's "What can go wrong"
table for what the program did and what you do. `<account>` is the account's
name; the other words in angle brackets are filled in by the reporter.

| Alert title | Urgency | Explained in |
| --- | --- | --- |
| `<account>: order refused (<reason>)` | urgent | this chapter, below; the reason, in the next table |
| `<account>: decision withheld (<reason>)` | urgent or routine, by reason | this chapter, below; the reason, in the table after next |
| `<account>: no engine heartbeat` | urgent | this chapter, below |
| `<account>: the reporter cannot read the journal` | urgent | this chapter, below |
| `<account>: the reporter cannot check the heartbeat` | urgent | this chapter, below |
| `<account>: the history of earlier runs is unavailable` | routine | this chapter, below |
| `<account>: a restored log looks newer than the current run` | routine | this chapter, below |
| `<account>: engine <state> (<reason>)` (Degraded or Halted) | urgent, repeated until acknowledged | [states and kill switch](states-and-kill-switch.md); a reconciliation's own reasons in [reconciliation](reconciliation.md) |
| `<account>: margin cushion <level>` | urgent at the urgent level, otherwise routine | [states and kill switch](states-and-kill-switch.md), and the [margin warning](../runbooks/margin-warning.md) runbook |
| `<account>: sale back to 98% sent (<instrument>)` and `... released, not sent (<mode>)` | routine | [states and kill switch](states-and-kill-switch.md) |
| `<account>: sale back to 98% filled <filled> of <wanted> (<instrument>)` | routine | [states and kill switch](states-and-kill-switch.md) |
| `<account>: sale back to 98% failed (<status>, <instrument>)` | urgent | [states and kill switch](states-and-kill-switch.md) |
| `<account>: sale of <instrument> withheld (<reason>)` | urgent | [states and kill switch](states-and-kill-switch.md) |
| `<account>: sale of <instrument> withheld, sell by hand (<reason>)` | urgent | [states and kill switch](states-and-kill-switch.md) |
| `<account>: sale back to 98% refused, sell by hand (<instrument>)` | urgent | [states and kill switch](states-and-kill-switch.md) |
| `<account>: the reporter cannot deliver to <channel>` | urgent | this chapter, "What can go wrong" |
| `<account>: no recorded mark for <instrument>` | urgent | [states and kill switch](states-and-kill-switch.md) |
| `<account>: reconciliation <classification> (<id>)` | urgent, repeated until acknowledged | [reconciliation](reconciliation.md) |
| `<account>: venue report <id> not read` | urgent | [order lifecycle](order-lifecycle.md) |
| `<account>: order <id> not acknowledged` | urgent | [order lifecycle](order-lifecycle.md) |
| `<account>: order <id> acknowledged` | routine | [order lifecycle](order-lifecycle.md) |
| `<host>: the server's clock failed its check` (the server's name, not an account's) | urgent | this chapter, below, and [server](server.md), "The clock" |
| `<account>: daily summary <date>` | routine | this chapter, "What the summary shows, and its limits" |

**Why an order was refused.** The reason in `order refused (<reason>)`:

| Reason | Explained in |
| --- | --- |
| `kill-switch`, `engine-degraded`, `engine-halted`, `sale-attempted`, `broker-holding`, `sell-exceeds-holding`, `stale-broker-snapshot`, `undecided-order`, `order-count`, `order-size`, `price-band`, `missing-mark`, `unvalued-holding` | [states and kill switch](states-and-kill-switch.md) |
| `margin-exposure` | [Boost](boost.md) |
| `ira-exposure`, `insufficient-cash` | [momentum](momentum.md) |

**Why a decision was withheld.** The reason in `decision withheld (<reason>)`,
and whether the alert is urgent:

| Reason | Urgency | Explained in |
| --- | --- | --- |
| `missing-bars`, `stale-account-snapshot` | urgent (missing data) | [engine](engine.md) |
| `missing-universe`, `histories-missing` | urgent (missing data) | [momentum universe](momentum-universe.md) |
| `labels-missing` | urgent | [momentum universe](momentum-universe.md) |
| `all-unaffordable` | urgent | [momentum minimum lot](momentum-minimum-lot.md) |
| `no-eligible-names`, `insufficient-trend-history` | routine | [momentum](momentum.md) |
| `rebalance-in-progress` | routine | [momentum](momentum.md) |

**Reasons that come in other titles, or in no alert:**

- A sale back to 98% withheld: `stale-broker-snapshot` (in `sale of
  <instrument> withheld (<reason>)`), `broker-values-unreadable`,
  `unknown-working-sell` and `non-positive-broker-values` (in `sale of
  <instrument> withheld, sell by hand (<reason>)`); `nothing-to-sell` and
  `lapsed` send no alert (a lapse is in the daily summary). All in
  [states and kill switch](states-and-kill-switch.md).
- An engine state's reason: `risk-limit-breached` in
  [states and kill switch](states-and-kill-switch.md);
  `reconciliation-unexplained` and `reconciliation-unverifiable` in
  [reconciliation](reconciliation.md).
- A skipped trade: `below-one-share`, `scaled-below-one-share` and
  `held-by-working-sells` send no alert; they are listed under "Trades
  skipped" in the summary and the dashboard; see
  [momentum minimum lot](momentum-minimum-lot.md).

## What can go wrong, and what the program does then

`journalctl -u ti-reporter@<instance>` shows every reporter run's error;
`journalctl -u ti-dashboard` the dashboard's. On the server `<instance>` is
`boost`.

### Alerts this chapter explains

| What you see | What the program does | What you do |
| --- | --- | --- |
| `<account>: order refused (<reason>)` | The risk gate refused one order before anything was sent to IBKR. The reporter sends this urgent alert once per refused order (once per kill-switch engagement, and once per stale-snapshot episode). It is not repeated. The body names the order, its side, size, limit price and the gate's detail | Look the reason up in "Which chapter explains each alert", above; that chapter's table says what to do |
| `<account>: decision withheld (<reason>)` | The engine decided no new targets for that session. Positions and orders already in place are unchanged. One alert per run of the same reason; a decided session ends the run, and the reason coming back later is a new alert. Urgent when an input is missing, no stock is affordable or sector labels are missing; routine otherwise | Look the reason up in the table above; that chapter says what to do |
| `<account>: no engine heartbeat` | The log has not changed while `-heartbeat-sessions` NYSE sessions (default 2) closed. One urgent alert per silence; a later silence is another. Orders already at IBKR are unaffected | `systemctl status ti-engine@<instance>` and `journalctl -u ti-engine@<instance>`. If it crashed, resume it; otherwise start a fresh run you approve ([restart](restart.md); runbook `docs/runbooks/durable-log.md`, "Restarting an engine") |
| `<account>: the history of earlier runs is unavailable` | The chain of runs behind the current log is broken: the body names the two logs and why. Only the equity comparison is affected: every other alert and the daily summary are sent as before, and the summary's equity section reads "history unavailable: the chain of runs is broken between <log A> and <log B>", then gives the comparison over the intact runs, as the dashboard does. Once per account, break and reason: the same link broken for another reason (a restored log that is not the one approved) is another alert, and once a firing finds the chain whole the break is forgotten, so a later break alerts again | Before any restart, restore the named earlier log from a backup of it as the engine wrote it, keeping its modification time and owned by `ti-trader` (`sudo rsync -a --chown=ti-trader:ti-trader`, or `sudo cp -p` then `sudo chown ti-trader:ti-trader`), and check with `ls -l --time-style=full-iso` that it is older than the current log and owned by `ti-trader` ("The history across restarts", above). If it cannot be recovered, the history starts at the first intact run from then on. Never put back a log moved into `quarantine/` |
| `<account>: a restored log looks newer than the current run` | A log the chain of runs names as an earlier run has a newer modification time than the current run, usually a log restored from a backup with the time of the restore. The body reads "<restored log> looks newer than the current run <log>; fix its modification time before the next restart". The running engine is unaffected, but the engine finds the latest run by modification time, so its next start would take the restored log for the latest run. Once per account and pair of logs | Before any restart, restore the log again from its backup with `sudo rsync -a --chown=ti-trader:ti-trader` (or `sudo cp -p` then `sudo chown ti-trader:ti-trader`), or set its time back with `sudo touch -d '<a time before the current log's>' <restored log>`, and check with `ls -l --time-style=full-iso` that it is older than the current log ("The history across restarts", above). If the engine already refused a start saying the latest earlier run is the restored log, do not approve that log: fix its time and start again with the approval naming the current log |
| `<account>: the reporter cannot read the journal` | The log does not verify, mixes runs, is missing, or holds a decision record that fails its checks. No other alert from that log is sent and no summary; the heartbeat is still checked unless the body says "The heartbeat is NOT checked" and why. Once per account and problem; the run exits with the error | Check the file with `sudo -u ti-trader journal-query` ([journal-query](journal-query.md)); never edit it. Runbook `docs/runbooks/durable-log.md`, "A log that cannot be read". Until it reads, nothing in it reaches you: watch the account in IBKR's app |
| `<account>: the reporter cannot check the heartbeat` | The NYSE calendar does not cover the span (it covers 2022 to 2030), so a silent engine would go unreported. Once per problem; the run exits with the error | Install a build whose calendar covers the year. Until then check the engine yourself each trading day |
| `<host>: the server's clock failed its check` | The reporter runs the clock check (`/usr/local/libexec/trend-investing/clock-check.sh`) at every firing, after it has sent the firing's other alerts, so a slow check never holds them back. A clock behind IBKR's makes every engine on the server count the account reading as stale and withhold every sell (ADR 0019). **When it pages:** after two checks in a row fail, one timer period apart, so within 10 minutes of the clock going wrong; a single failing check (a reference briefly unreachable, say) pages nothing, and one passing check starts the count again. A check recorded more than 150 seconds ahead of the time now means the clock was set back, and that counts as a failed check in its own right (the alert then says the clock stepped back by about so long), so a clock that keeps stepping back pages on the second firing that finds it. **Once per server:** the clock is the server's, so every account's reporter shares one record of it, `/var/lib/trend-investing/reports/host-clock.json` (`-clock-state`), under a lock: one check per 5-minute period, one page per fault, whichever account's reporter made the check. The alert gives the time of the check, the skew it measured (this clock minus the reference), or says none was measured (the clock not synchronised, no reference reachable, the check missing, not executable or timed out), or that its output could not be read, and what the check said. It is not repeated; when a later check passes the PagerDuty incident is resolved and the failure forgotten, channel by channel, so drifting again pages again. A channel that works forgets it at once. If PagerDuty is set aside at that moment (a bad routing key), its record is kept and the incident is closed by the first firing in which PagerDuty works again; ntfy, which has nothing to close, is not held back by it. The same holds if PagerDuty's resolve fails (its service down, or its routing key invalid since the trigger): only PagerDuty's record is kept, to retry the resolve, and a new fault still pages ntfy. A routing key changed to another valid key is accepted by PagerDuty, so the resolve succeeds, closes nothing and the record is forgotten at once: the incident opened under the old key stays open, and it must be resolved by hand in PagerDuty. A channel taken out of `-channel` has its record dropped by the next passing check, but the program cannot reach its incident: if PagerDuty was removed, resolve the open `clock-drift:<host>` incident by hand in PagerDuty. Reporters on one host naming different `-channel` lists share this record, so keep the lists the same | Run the check by hand and fix the time syncing, or on the Mac mini turn on the Mac's automatic date and time ([server](server.md), "The clock"). If the alert says the check **is missing**, run `sudo deploy/provision.sh clock`, which installs it. If it says the check **is not executable by this user**, the reporter's user (`ti-trader`) cannot run it: `sudo chmod 0755 /usr/local/libexec/trend-investing/clock-check.sh` (or run `provision.sh clock` again). If it says the check **timed out after 40s**, the check hung (a reference or `timedatectl` did not answer): run it by hand to see which, and check the network and `systemctl status systemd-timesyncd`. If it says the output **could not be read**, read what the check said in the alert; the check's wording and the reporter disagree, so install a matching build. Until the clock is right, expect sales to be withheld as `stale-broker-snapshot` |

The daily summary's last line, "Clock check: ok at <time> (...)" or "Clock check:
FAILED at <time> (...)", is the check the reporter ran in the same firing that
sent the summary, or, when another account's reporter was checking at that
moment, the last check recorded for the server; it appears only when the unit
passes `-clock-check`.

### The reporter itself

| What you see | What the program does | What you do |
| --- | --- | --- |
| Nothing at all for a long time | Possible causes: the timer is not running, the binary is not installed, or every run stops before it sends (the rows below). Nothing outside the server notices: the outside heartbeat (a dead-man's switch) is not built, and nothing reads a `HEALTHCHECK_URL` line | `systemctl list-timers ti-reporter@boost.timer` shows the next firing; `systemctl status ti-reporter@boost` and `journalctl -u ti-reporter@boost` show the last run and its error |
| The unit failed: "ti-reporter: /var/lib/trend-investing/engines/boost/run.env is missing or empty: start the boost engine once first ..." | The reporter does not run, so it sends nothing | Start the engine by `docs/runbooks/durable-log.md`, "Restarting an engine"; the next firing follows its log |
| `<account>: the reporter cannot deliver to <channel>`, urgent, on the channels that still work | One channel's secret is empty or malformed, or its name in `-channel` is unknown, so that channel is set aside; every alert still goes to the others, the daily summary ends with "Channels not delivering:" naming it, and the run ends with an error naming it (the rows below). Once per channel and problem; nothing reaches the channel set aside until it is fixed | Fix that channel's line in `secrets.env` (the rows below say which), or the unit's `-channel`; the next firing delivers to it everything it missed |
| "reporter: the ntfy channel needs TI_NTFY_TOPIC ..." or "reporter: the pagerduty channel needs TI_PAGERDUTY_ROUTING_KEY ..." | That channel is set aside and the other still delivers (the row above). With no channel left that reaches you, the whole run is refused before anything is sent | Fill the line in with `sudo -e /etc/trend-investing/secrets.env`; the next firing uses it |
| "reporter: TI_NTFY_SERVER must be an absolute URL such as https://ntfy.example.org" or "reporter: TI_PAGERDUTY_URL must be an absolute URL such as https://events.pagerduty.com/v2/enqueue" | That channel is set aside and the other still delivers; with none left, the run is refused before anything is sent | Correct the line, or leave it empty for the public ntfy.sh relay or PagerDuty's own endpoint |
| "reporter: the secrets file cannot be read; check -secrets-file names the credential file", or "reporter: the secrets file's line N is not KEY=VALUE" | Reported in the run's error, which never shows a secret's text. Every line that reads is still used, so only a channel whose secret that leaves unset is set aside; with no channel left, nothing is sent | Correct that line of `secrets.env` (`KEY=VALUE`, no spaces round `=`) with `sudo -e` |
| "ntfy: server answered 403 Forbidden" (or another status), "pagerduty: server answered 400 Bad Request", "pagerduty: the answer did not acknowledge the event", or "post failed" | That channel failed, so the alert is not delivered, even if the other channel took it. Only the failed channel is retried, at every firing, until it takes the alert | A wrong topic, server or routing key: correct `TI_NTFY_TOPIC`, `TI_NTFY_SERVER` or `TI_PAGERDUTY_ROUTING_KEY` in `secrets.env`. A network fault clears by itself |
| PagerDuty shows incidents, but your phone shows nothing | Both channels accepted the alert, so the reporter counts it delivered. What your phone does with it is up to the PagerDuty and ntfy apps | Check the PagerDuty app's notification rules, and that the ntfy app is subscribed to exactly the topic (and server) in `secrets.env` |
| "reporter: the state file is unreadable and is not reset, since a reset resends every alert: ..." | The run stops before anything is sent, every firing | Move the file aside: `sudo -u ti-trader mv /var/lib/trend-investing/reports/boost.reporter.json /var/lib/trend-investing/reports/boost.reporter.json.bad`. The next firing sends every alert in the log once more, which is safer than none |
| "reporter: write state: ..." | Alerts may have gone, but were not recorded, so the next firing sends them again | Free disk space, or fix the folder's owner (`ti-trader`) |
| "the daily summary carries balances and never goes through a public relay such as ntfy.sh; ..." | Every alert still goes; the summary does not, because no private channel was named | Keep `pagerduty` in the unit's `-channel`, or set `TI_NTFY_SERVER` to your own ntfy server |
| "reporter: the daily summary was not sent: ..." | Every alert still goes and is remembered; the summary is tried again at the next firing | Read the record it names with `journal-query`; if the record fails its checks, the log is damaged (row above on reading the journal) |
| The repeats keep coming after you ran `operator acknowledge` | The engine records an acknowledgement only from its operator inbox, at its next input from the adapter, so with the engine stopped the repeats continue until it runs. One the engine refused leaves a `.refused` note in its notes folder (`operator-notes`, beside the inbox); one for another account, or for a key with a typing slip, is recorded and does nothing | Check the engine is running and read any `.refused` note; record the acknowledgement again with the right account, the current time and the key copied from the alert ([operator](operator.md), "Acknowledging an alert") |

### The dashboard

| What you see | What the program does | What you do |
| --- | --- | --- |
| The page says "journal unavailable" | The log could not be read: missing, not readable, or a record that fails its checks. The page and `/api/summary.json` show no figure | `systemctl status ti-dashboard`; check the engine's log folder (`/var/lib/trend-investing/engines/boost`) holds the logs. The reporter's "cannot read the journal" alert names the reason when a record is bad |
| An account's card says "<account>: no run yet: the engine has not started a run" | The account's engine folder holds no log: its first run has not started. The other accounts show as usual | Nothing, if you have not started that engine. Otherwise check the folder is the engine's (`/var/lib/trend-investing/engines/<instance>`) and start it ([restart](restart.md)) |
| An account's card says "<account>: the engine started a run but recorded nothing past its header" | The folder holds only logs with no record past their header: a start that crashed before it recorded its configuration, not a first start. Nothing is alerted for it | Check the engine (`systemctl status ti-engine@<instance>`). If it is not running, start it again ([restart](restart.md)) |
| An account's card says "<account>: journal unavailable" or "<account>: journal does not verify" | The same causes as the whole-page messages below, for that account's current log only | As for the whole-page messages below |
| The page says "journal does not verify" | The log's fingerprints fail, or its records name different runs. No figure is shown | Never edit the log. Runbook `docs/runbooks/durable-log.md`, "A log that cannot be read" |
| The equity card says "history unavailable: the chain of runs is broken between <log A> and <log B>" | The page follows each run's approval back to the first run, and one link fails: an earlier log is missing, is not the log the approval named, is another account's, holds no approval outside a dry run, is a dry-run log holding nothing past its configuration, or the approvals loop. The rest of the page is the current run's, as always; the equity comparison is drawn from the first intact run, and the card says where it starts | Before any restart, restore the named earlier log from a backup of it as the engine wrote it, beside the others, keeping its modification time and owned by `ti-trader` (`sudo rsync -a --chown=ti-trader:ti-trader`, or `sudo cp -p` then `sudo chown ti-trader:ti-trader`; "The history across restarts", above, and `docs/runbooks/durable-log.md`), and check with `ls -l --time-style=full-iso` that it is older than the current log and owned by `ti-trader`. If it cannot be recovered, the history starts at the first intact run from then on, and the page says so. Never put back a log moved into `quarantine/`, and never edit or delete a log |
| The page says "<restored log> looks newer than the current run <log>; fix its modification time before the next restart" | A log the chain names as an earlier run is newer by modification time than the current run, usually restored with the time of the restore. The page still shows the current run, found by the chain; the engine, which finds the latest run by modification time, would take the restored log for the latest run at its next start | Before any restart, restore it again with `sudo rsync -a --chown=ti-trader:ti-trader` (or `sudo cp -p` then `sudo chown ti-trader:ti-trader`) from the backup, or `sudo touch -d '<the backup's own time>' <restored log>` (the time `ls -l --time-style=full-iso` shows for the backup copy, so the folder is back in its true order), and check with `ls -lrt --time-style=full-iso` |
| The equity card says "history since <date>, the first <mode> run" | The chain of runs holds runs of another mode (a dry run before paper, or paper before live) before the current mode's. The chain is whole; the comparison holds only the current mode's runs, from the first of them | Nothing: a live account's figures are compared from its first live run |
| The equity card says "history since <date>, the first <mode> run since the mode last changed; the <mode> runs before that change are not included" | The mode changed and came back (paper, then live, then paper again). The chain is whole; the comparison holds only the current mode's runs since the latest change, so the earlier runs of the same mode, before the run of another mode, are left out | Nothing, if you meant to return to that mode: the comparison starts again at the return |
| The equity card says "the run is starting; no history yet" | The current log holds only its configuration: a dry run still starting, such as its first run waiting for its first input. No other log names it, so it is no break, and nothing is alerted | Nothing: the line goes once the run records its next input. If it stays, check the engine and the adapter are running (`systemctl status`) |
| The equity card says "the run started but recorded no approval; if the engine is not running, resume it with its approval (see the manual's "Restarting an engine" chapter, docs/manual/restart.md)" | In paper or live the engine records the run's approval itself, straight after its configuration, so a log that stays at its configuration is a start that crashed before its approval was recorded. Nothing is alerted for it | Check the engine (`systemctl status`). If it is not running, resume it with `-resume` and the `-approval` it was started with ([restart](restart.md)) |
| The page shows a previous run after a restart | It should not: the page finds the current run, the newest link of the chain, on every request. If it does, the dashboard is an older build that still reads `TI_RUN` at start | Install the current build and restart the dashboard once (`sudo systemctl restart ti-dashboard`); after that no restart is needed |
| The equity card or the summary says "deposits and withdrawals not yet recorded: the comparison assumes none ..." | No ledger was given (`-cash-ledger`), or the Flex reader has not written one yet. The comparison runs, but a deposit or withdrawal counts as the account's own gain or loss | If you have moved no money since the first paired day, nothing. Otherwise turn the Flex reader on ([reconciliation](reconciliation.md), "Deposits and withdrawals, read from IBKR") and check the unit passes `-cash-ledger` with the reader's path |
| The equity card or the summary says "deposits and withdrawals not readable (<why>): the comparison assumes none ..." | The ledger exists but cannot be read (not readable by `ti-trader`), or a line of it is not a cash movement as the Flex reader writes it, or does not end with a whole line. Nothing of it is used, never a part | The Flex reader stops on the same ledger: follow its status file ([reconciliation](reconciliation.md), "What can go wrong"). Never edit the ledger by hand |
| The equity card or the summary says "SPY price return only: dividends not counted" (or "price return only for <from>–<to>") | Some or all of SPY's bars in the period are of the older form, which carries no dividend-adjusted price, or the adjusted price's base changed between two runs on that step, so the SPY side leaves those dividends out | Nothing to do for a period already recorded; bars from the current market-data job carry the adjusted price. If new days still say so, check the data job ([market data](market-data.md)) |
| The equity card or the summary says "ledger not accepted: it belongs to <x>, not <y>" | The `-cash-ledger` named is in another account's `flex/<instance>` folder than the engine folder it is paired with. Nothing of it is read, and the comparison assumes no transfer | Correct the unit's `-cash-ledger` to the account's own `/var/lib/trend-investing/flex/<instance>/cash-movements.jsonl` ("Running the dashboard", above) |
| The equity card or the summary says "deposits or withdrawals from the last two trading sessions may not be recorded yet; a transfer appears in the summary two trading sessions after it posts (a Friday transfer on Tuesday's)" (or "how far the Flex reader has recorded is not known ...") | The Flex reader has not yet read the summary's day: it runs at 21:30 New York, after the close's summary, and IBKR may post a day's transfer only the next evening. A transfer from the last two trading sessions is not in the comparison yet; it is in the summary two trading sessions after it posts (a Friday transfer on Tuesday's) | Nothing, if a later line says the day is recorded. If "not known" stays, check the reader's status file ([reconciliation](reconciliation.md), "What can go wrong") |
| The equity card or the summary says "the recorded withdrawals reached or exceeded the SPY shadow's value on <date>; dollar figures are not meaningful after that day" (or "... dollar figures restart from <date>") | Your recorded withdrawals took the SPY shadow account to zero that day, so its dollar figures (the gap and the dollar line) mean nothing until your next deposit restarts it; the verdict, which is time-weighted, stays | Nothing to fix: it is a true statement of the record. If the withdrawal was not yours, check the ledger's transfers against IBKR's activity ([reconciliation](reconciliation.md)) |
| It will not start: "dashboard: -cash-ledger ... must be SOURCE=PATH ...", "... names ..., which is no -journal or -engine-dir", "... is given twice for ..." or "... names no ledger file" | It refuses to guess which account a ledger belongs to; a `-journal`'s ledger is always named | Name each ledger as `SOURCE=PATH` with SOURCE exactly the `-engine-dir` or `-journal` it belongs to ("Running the dashboard", above) |
| It will not start: "dashboard: ... is neither loopback nor a Tailscale address ..." or "... is not an address of this host's Tailscale interface" | It refuses to listen anywhere but loopback or this machine's own Tailscale address | Use `-addr 127.0.0.1:8088` (the unit's) and publish it with `sudo tailscale serve --bg 8088` |

## What backs this

- ADR 0019, amendment of 2026-09-24, "Alerting": what alerts, once per
  incident, repeats until acknowledged, the outside heartbeat; its amendment
  of 2026-10-17 (the stale episode, and the sale back to 98%).
- ADR 0027: the durable log both programs read.
- `docs/live-trading/plan.md` section 3 and decision D6 (the sale's steps),
  section 5 (the margin-cushion alarm) and section 7 (the channels).
- Tests in `internal/report`, read from synthetic logs, among them
  `TestEveryRefusalAlertsOnceUrgently`,
  `TestAKillSwitchOrStaleRefusalAlertsOnlyWhenFlagged`,
  `TestAWithheldRunIsOneIncidentAndADecisionEndsIt`,
  `TestAnAllUnaffordableWithholdIsUrgent`, `TestALabelsMissingWithholdIsUrgent`,
  `TestHeartbeatLossIsCountedInSessionsAndAlertsOncePerSilence`,
  `TestEachMarginAlarmLevelChangeIsAlerted`,
  `TestAWithheldSaleTheOwnerMustMakeByHandIsAlertedUrgently`,
  `TestANonPositiveWithheldSalePagesSellByHand`,
  `TestASaleRefusedAsBrokerHoldingPagesSellByHand`,
  `TestAChannelTheReporterCannotUseIsAnUrgentAlertOnTheOthers` and
  `TestTheSummaryListsTheDaysSkippedTrades`; for the SPY comparison,
  `TestADepositMidSeriesIsNotOutperformance`,
  `TestAWithdrawalMidSeriesIsNotUnderperformance`,
  `TestADividendExDateLiftsTheBenchmarkSide`,
  `TestTheVerdictSaysWhetherTheAccountIsBeatingSPY`,
  `TestTheAssumptionNamesALedgerThatIsAbsentOrUnreadable`,
  `TestAVersion1SeriesSaysDividendsAreNotCounted` and
  `TestATransferIsMirroredAtTheCloseOfItsNewYorkDayOrTheNextSession`.
- Tests in `cmd/reporter`, among them
  `TestAJournalTheReporterCannotReadStillAlertsAndChecksTheHeartbeat`,
  `TestAFailureOnOneChannelIsNeverADeliveryAndOnlyThatChannelIsRetried`,
  `TestAnUnacknowledgedHaltRepeatsOnBothChannelsUntilAcknowledged`,
  `TestTheDailySummaryNeverGoesThroughThePublicRelay`,
  `TestTheSecretsFileSuppliesTheChannelSecrets`,
  `TestOneChannelsBadSecretNeverSilencesTheOther`,
  `TestAMalformedSecretsLineSetsAsideOnlyWhatItLeavesUnset`,
  `TestTheServiceFileRunsTheReporterOnTheEnginesLog`,
  `TestTheDailySummaryGivesTheVerdictWithTheFlexLedgersTransfersMirrored` and
  `TestTheTimerMakesTheRepeatAndAcknowledgementBoundsTrue`.
- Tests in `cmd/dashboard`, among them `TestEveryRouteRefusesEveryWritingMethod`,
  `TestDashboardShowsTheDaysSkippedTrades`,
  `TestDashboardRefusesAJournalThatDoesNotVerify`,
  `TestListenAddressMustBeLoopbackOrThisHostsTailscaleAddress`,
  `TestTheEquityCardOpensWithTheVerdictInWordsAndColour`,
  `TestTheChartDrawsGrowthSoADepositDoesNotBendIt`,
  `TestTheDashboardReadsTheFlexLedgerForEachEngineDirectory`,
  `TestTheCashLedgerFlagNamesTheAccountItBelongsTo` and
  `TestTheServiceFileServesTheBoostLogOnTheOnePort`.
- `deploy/rehearse.sh`, which installs the programs in a clean Ubuntu
  container and runs the reporter with its unit's own command line.
