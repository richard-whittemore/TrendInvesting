# Alerts, the daily summary and the dashboard

## What they do and why they exist

Two small programs watch an account's record of what the engine did and tell
you about it, so you do not have to go looking.

- `reporter` reads one account's log and sends your phone an alert whenever
  something needs your attention, and optionally a daily summary: what you
  hold, what the strategy wanted, the orders and fills, and how your equity
  compares with SPY.
- `dashboard` shows the same summary as a web page, one section per account.
  It is read-only: you cannot click anything that changes the system.

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
  the Boost and momentum engines', which also carry a dividend-adjusted
  price. The comparison with SPY uses SPY's split-adjusted close from either.
  The dividend-adjusted price is for the strategies' signals only and never
  moves an equity figure.

## What triggers an alert

| Alert | Urgency | Sent once per |
| --- | --- | --- |
| An order was refused | urgent | refused order, except: a kill-switch refusal once per engagement (turning the switch off and on again is a new one), and a refusal for a too-old IBKR snapshot once per stale episode ([states-and-kill-switch](states-and-kill-switch.md)); a log written before the engine marked those first refusals alerts for every one |
| IBKR rejected two of the account's Boost orders in a row, whatever their targets (the page names the order whose rejection made it two, even an older order rejected late, and the rejection before it), or rejected an order after that (paged as another rejection while buys are stopped, with which one since the stop it is and the two rejections that stopped buys). The program sends no more buys for the account until you approve a fresh run (`operator approve`, adding `-clear-incident` if the run also ended in an incident). Its sells still go: Boost's exit, tried again at each decided session while due, and the margin alarm's sale back to 98%. So before any sale by hand, stop new orders (the kill switch where it exists, otherwise stop the engine), cancel any working program sale in IBKR's app, confirm none remains, and only then sell ([states](states-and-kill-switch.md#sells-that-keep-going-while-degraded)). Find the reason in IBKR's order log first ([boost](boost.md#a-refused-or-unfilled-order-is-planned-again-re-plan)). The rule was confirmed by the owner on 2026-10-18 | urgent | each rejected order |
| A sale back to 98% was not placed because the IBKR snapshot was too old, and it opened the stale episode (sell by hand if it is needed) | urgent | stale episode; the episode's later too-old refusals send nothing more |
| A sale back to 98% was not placed because IBKR's values could not be read (`broker-values-unreadable`) or a sell this program did not release is working at IBKR (`unknown-working-sell`, CONTAINMENT REQUIRED). The program will not try again: sell by hand | urgent | withheld sale |
| The margin-cushion alarm changed level ([margin warning](../runbooks/margin-warning.md)): warning (check the account the same day), urgent (the program sells back to 98% once) or clear | urgent for the urgent level; routine for warning and clear | change of level |
| A sale back to 98% was released to be sent, sized from the IBKR snapshot it names; in dry-run, released and not sent | routine | sale (its client order ID) |
| A sale back to 98% filled, in part or in full: the alert says how many shares are filled and how many are left | routine | fill (its fill ID; a resent fill is not counted again) |
| IBKR canceled or refused a sale back to 98% before it filled in full. The program will not try again: sell the rest by hand | urgent | sale |
| The engine went Degraded or Halted | urgent, repeated until acknowledged | change of state, reason and scope; the same state coming back after another is a new alert |
| A reconciliation did not match: an unexplained difference (Degraded) or evidence that could not be compared (Halted); see [reconciliation](reconciliation.md) | urgent, repeated until acknowledged | reconciliation; the state change it caused is part of the same alert, not a second one |
| A decision was withheld for missing data (bars, account snapshot, universe, the close histories of the stocks ranked) | urgent | run of the same reason |
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
| The daily summary (if asked for) | routine | account and day, once that day's session has closed in the log |

A sale back to 98% withheld for any other reason sends none of these
alerts: `non-positive-broker-values` is decided again from the next good
snapshot, `nothing-to-sell` has nothing to do, and a lapse (`lapsed`) goes in
the daily summary. A sale the gate refused alerts as a refused order, and one
IBKR never reported on alerts as an order not acknowledged. The sale alerts
read the records the engine already writes: the planned sale, its release, the
fills and IBKR's reports on it. A fill or report the engine could not read
raises no sale alert; an IBKR report of that kind alerts as "a report from
IBKR the engine did not read".

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
on the engine's output (`journalctl -u ti-engine@<account>`). Record the
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
its key, ntfy posts it twice); every other alert is not sent again. `cmd/reporter`'s unit test checks that the timer
runs around the clock, that its period divides the repeat interval and is at
most half the acknowledgement bound, and that a run is stopped before the
next firing.

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
that is not `KEY=VALUE`, stops the run with an error naming the line's number,
never its text.

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
  -secrets-file %d/secrets -channel ntfy,pagerduty -summary -account boost
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
alert; it must be positive).

If it exits with an error, read it: it names the channel that failed or the
file it refused. Nothing is lost; the next run tries again.

## Running the dashboard

On the server it runs as `ti-dashboard.service`:

```
/opt/trend-investing/bin/ti-dashboard \
  -journal /var/lib/trend-investing/engines/boost/${TI_RUN}.log -addr 127.0.0.1:8088
```

Port 8088 is the program's default, the unit's, and the one the Lightsail
runbook publishes to your tailnet (`sudo tailscale serve --bg 8088`). `TI_RUN`
is read when the dashboard starts, so after a fresh run restart it
(`sudo systemctl restart ti-dashboard`) to show the new log. It reads no
secret and is given none.

On the Mac:

```
go run ./cmd/dashboard -journal joint.log -journal ira.log -addr 127.0.0.1:8088
```

Repeat `-journal` for each account. It will listen only on a loopback address
or an address this machine's own Tailscale interface holds (`tailscale ip`
prints it), so on the server it is reachable only over Tailscale. An address
that merely looks like a Tailscale one but belongs to another network is
refused.
Pages: `/` (the summaries, with each alert's full text under its title),
`/api/summary.json` and `/healthz`. Anything but a
read is answered "method not allowed". If a log does not verify the page says
"journal does not verify"; if it cannot be read for any other reason it says
"journal unavailable". Neither shows paths or figures.

## What the summary shows, and its limits

- Holdings come from the broker's account snapshot.
- Each released order says "released (send requested)" or "released (not
  sent)". The engine records a release before it sends, so a release is only
  possibly sent; the summary does not yet say a broker confirmed it. A dry-run
  releases orders without sending them.
- Equity against SPY pairs the snapshot's net liquidation with SPY's closing
  price on the same day. It is price return only (no dividends) and needs two
  paired points before it shows a comparison. The comparison names the dates
  it runs between; the last date can be earlier than the summary's day when
  that day has no paired snapshot and close.
- The day's change compares the summary's day with the paired point before
  it, and names both dates, so a gap of missing days shows as a longer span.
  When the summary's day has no paired point it says "Day: not available"
  rather than showing an earlier day's change as today's.
- Fills are read in both forms the engines record: the Turtle engine's and
  the Boost and momentum engines' (which name the side and the client order
  ID). Neither is counted in "Inputs not read".
- Every sale back to 98% withheld that day is listed under "Sales back to 98%
  withheld", with its reason, what fired it and the engine's detail. A lapse
  (`lapsed`: what fired the sale ended before a good snapshot came) appears
  only here, never as an alert.

## What can go wrong

| What you see | What it means | What you do |
| --- | --- | --- |
| Nothing at all for a long time | The timer is not running, the binary is not installed, or every run fails. Nothing outside the server notices: the outside dead-man's switch is not built | `systemctl list-timers ti-reporter@boost.timer` shows the next firing; `systemctl status ti-reporter@boost` and `journalctl -u ti-reporter@boost` show the last run and its error |
| The unit failed: "run.env is missing or empty" | The Boost engine has never been started on this server | Start it by `docs/runbooks/durable-log.md`, "Restarting an engine" |
| The unit failed: "the ntfy channel needs TI_NTFY_TOPIC" (or the PagerDuty key) | That line of `/etc/trend-investing/secrets.env` is empty | Fill it in with `sudo -e /etc/trend-investing/secrets.env`; the next firing uses it |
| The unit failed: "ntfy: server answered 4xx" or "pagerduty: server answered 400" | A wrong topic, server or routing key | Correct the line in `secrets.env`. Only that channel is retried, at the next firing |
| PagerDuty shows incidents but the phone is silent | PagerDuty's own notification rules | Check the PagerDuty app's notification settings; the reporter's delivery was accepted |
| "the state file is unreadable and is not reset" | The state file in `/var/lib/trend-investing/reports` is damaged | Move it aside and run again: every alert in the log is sent once more, which is safer than none |
| The dashboard says "journal unavailable" or "journal does not verify" | The log cannot be read, or its fingerprints fail | The reporter sends "the reporter cannot read the journal" with the reason; check the file with `journal-query` |
| The dashboard shows the previous run | It was not restarted after a fresh run | `sudo systemctl restart ti-dashboard` |

## What backs this

- ADR 0019, "Alerting" (amended 2026-09-24 and 2026-10-17): what alerts, once
  per incident, repeats until acknowledged, the stale episode.
- ADR 0027: the durable log both programs read.
- `docs/live-trading/plan.md` section 3 and decision D6 (the sale's steps),
  section 5 (the margin-cushion alarm) and section 7 (the channels).
- Tests: `internal/report` (each alert and the summary, read from synthetic
  logs), `cmd/reporter` (delivery, repeats, the secrets file, and the unit
  and timer as shipped, run against a synthetic log), `cmd/dashboard` (the
  page and the unit), and `deploy/rehearse.sh`, which installs the programs
  in a clean Ubuntu container and runs the reporter with its unit's own
  command line.
