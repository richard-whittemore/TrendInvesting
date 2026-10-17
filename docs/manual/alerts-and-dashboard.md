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
| An order was refused | urgent | refused order |
| The engine went Degraded or Halted | urgent | change of state, reason and scope; the same state coming back after another is a new alert |
| A decision was withheld for missing data (bars, account snapshot, universe) | urgent | run of the same reason |
| A momentum decision was withheld because no stock with a target was affordable (`all-unaffordable`; the net liquidation or prices may be wrong) | urgent | run of the same reason |
| A decision was withheld for another reason | routine | run of the same reason |
| An order to be sent was not acknowledged by IBKR within `-ack-within` of its commit ([order lifecycle](order-lifecycle.md)) | urgent | order (its client order ID) |
| An order already alerted as unacknowledged has since been acknowledged | routine | order |
| A report from IBKR in the log that the engine did not read: it does not validate, or does not match the order it names ([order lifecycle](order-lifecycle.md)) | urgent | that report (its input ID) |
| No sign of life from the engine | urgent | silence, keyed by the last activity |
| The reporter cannot read the journal (its text says "The heartbeat is NOT checked" and why, whenever liveness is unmonitored) | urgent | account and problem |
| The reporter cannot check the heartbeat (the calendar does not cover the span) | urgent | account and problem |
| The daily summary (if asked for) | routine | account and day, once that day's session has closed in the log |

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

## Where alerts go

Alerts go to real channels. You choose them with `-channel`, comma-separated.

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

From the repository root, once per timer tick:

```
TI_NTFY_TOPIC=... TI_PAGERDUTY_ROUTING_KEY=... \
  go run ./cmd/reporter -journal /var/lib/trend-investing/joint.log \
  -state /var/lib/trend-investing/joint.reporter.json \
  -channel ntfy,pagerduty -summary
```

Flags: `-journal` and `-state` are required; `-channel` (default `stdout`,
which is refused on its own, so always name a real channel); `-summary`
(also send the day's summary); `-date YYYY-MM-DD` (default: the latest day
whose session has closed in the log; a stated day whose session has not
closed is an error); `-benchmark` (default SPY); `-heartbeat-sessions`
(default 2 NYSE sessions with nothing written); `-account` (the name for
alerts when the log cannot name its account; default the log file's name);
`-send-timeout` (default 30 seconds per send); `-ack-within` (default 10
minutes: how long an order to be sent may go unacknowledged by IBKR, from its
commit, before an alert; it must be positive).

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
- The fills list stays empty until the engine records fills for target-weight
  strategies.
