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

> **On the server today, neither runs.** Both programs are built, but
> neither is installed, and their units pass none of the settings below: the
> reporter's unit gives no `-journal`, `-state` or `-channel`, and does not
> set the `TI_NTFY_TOPIC` or `TI_PAGERDUTY_ROUTING_KEY` the reporter reads;
> the dashboard's gives no `-journal` and names a port (8080) the program
> does not read. As shipped, **no alert would reach your phone.** And the
> reporter's timer runs it **once a weekday, at 17:30**, so what this
> chapter says about repeats "every 30 minutes" and an order unacknowledged
> "within 10 minutes" holds only for a reporter run that often; on the
> timer as shipped, a repeat or an unacknowledged order is noticed at the
> next 17:30 run at the earliest ([server](server.md)). Fixing the units
> and the timer is the reporter follow-up's work.

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
  what the engine decided (targets, orders, refusals, engine state) that
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
| A sale back to 98% was not placed because the IBKR snapshot was too old, and it opened the stale episode (sell by hand if it is needed) | urgent | stale episode; the episode's later too-old refusals send nothing more |
| The engine went Degraded or Halted | urgent, repeated until acknowledged | change of state, reason and scope; the same state coming back after another is a new alert |
| A reconciliation did not match: an unexplained difference (Degraded) or evidence that could not be compared (Halted); see [reconciliation](reconciliation.md) | urgent, repeated until acknowledged | reconciliation; the state change it caused is part of the same alert, not a second one |
| A decision was withheld for missing data (bars, account snapshot, universe, the close histories of the stocks ranked) | urgent | run of the same reason |
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

### Alerts the reporter does not send

**Not built yet.** Some pages of this manual and the plan describe these,
but the reporter does not read the records behind them, so nothing reaches
you. Until they are built (tracked with the reporter follow-up on missing
alerts), check them yourself with `journal-query refusals` and `grep` on
the durable log:

- the margin-cushion alarm, warning or urgent (`risk.margin.alarm`);
- the sale back to 98% being sent, filling or failing;
- a sale back to 98% withheld for you to act by hand
  (`broker-values-unreadable`, or `unknown-working-sell`, CONTAINMENT
  REQUIRED). Only a withhold that opens a stale episode alerts;
- a withheld sale that lapsed, in the daily summary;
- Boost's fills, in the daily summary (below).

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
each `-repeat-interval` (default 30 minutes), for as long as you have not
acknowledged it. Every other alert is sent once. ADR 0019 asks for "a fixed
interval" and leaves its length to you; 30 minutes is the program's
default, not a recorded decision. A repeat can only come when the reporter
runs, so on the server's once-a-day timer it comes once a day (above).

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
you see a `.refused` note beside the file saying why in the engine's inbox and
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

## Where alerts go

Alerts go to real channels. You choose them with `-channel`, comma-separated.

The secrets are read from the reporter's **environment**, under the names
below. The server's `/etc/trend-investing/secrets.env` has other names
(`NTFY_URL`, `HEALTHCHECK_URL`) that nothing reads, and no PagerDuty line
([server](server.md), "Where the secrets live").

| Channel | What it is | Secret (environment) |
| --- | --- | --- |
| `ntfy` | Push to your phone through ntfy (the public ntfy.sh relay unless you run your own server) | `TI_NTFY_TOPIC`, an unguessable topic name; optionally `TI_NTFY_SERVER` |
| `pagerduty` | The second channel, chosen by the owner: PagerDuty, through its Events API | `TI_PAGERDUTY_ROUTING_KEY`, the integration key; optionally `TI_PAGERDUTY_URL` |
| `stdout` | Prints to the terminal or the system log | none |

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

From the repository root, once per timer tick. On the server, the engine's
log is `/var/lib/trend-investing/engines/<account>/<TI_RUN>.log`, and the
reporter's unit may write only under `/var/lib/trend-investing/reports/`, so
its state file goes there:

```
TI_NTFY_TOPIC=... TI_PAGERDUTY_ROUTING_KEY=... \
  go run ./cmd/reporter -journal /var/lib/trend-investing/engines/boost/<TI_RUN>.log \
  -state /var/lib/trend-investing/reports/boost.reporter.json \
  -channel ntfy,pagerduty -summary
```

A new run writes a new log, so the `-journal` path changes with every
`TI_RUN`: the reporter follows the one file it is given.

Flags: `-journal` and `-state` are required; `-channel` (default `stdout`,
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
- The fills list shows the Turtle engine's fills (version 4) only. The engine
  now records the target-weight strategies' fills (version 5), but the
  summary does not read them yet: each Boost or IRA fill is counted under
  "Inputs not read" and the fills list stays empty for those accounts. **Not
  built yet** (tracked in the issue "Reporter summary counts newer snapshot
  and fill versions as rejected inputs").
