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

Before reporting anything the program checks the file's fingerprints. A file
that has been edited or damaged is refused whole, and no figure from it is
shown. The checking is the same code `journal-query` uses, so the three tools
cannot disagree about what a file holds.

## What triggers an alert

| Alert | Urgency | Sent once per |
| --- | --- | --- |
| An order was refused | urgent | refused order |
| The engine went Degraded or Halted | urgent | change of state, reason and scope |
| A decision was withheld for missing data (bars, account snapshot, universe) | urgent | run of the same reason |
| A decision was withheld for another reason | routine | run of the same reason |
| No sign of life from the engine | urgent | silence, keyed by the last activity |
| The daily summary (if asked for) | routine | account and day |

"No sign of life" is the log file's last-modified time. A reporter running on
the engine's own machine cannot catch that machine dying; the outside
dead-man's switch ADR 0019 asks for is a separate service and is not built.

## Where alerts go

Alerts go to real channels. You choose them with `-channel`, comma separated.

| Channel | What it is | Secret (environment) |
| --- | --- | --- |
| `ntfy` | Push to your phone through ntfy | `TI_NTFY_TOPIC`, an unguessable topic name; optionally `TI_NTFY_SERVER` |
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
- The reporter remembers what each channel has taken in its own state file
  (`-state`). That file is the only thing it writes. If it is unreadable the
  reporter stops rather than starting over, because starting over would
  resend every alert.
- Secrets (the ntfy topic, the PagerDuty key) never appear in output or errors.

PagerDuty details: you sign up for PagerDuty and create a service with an
Events API v2 integration, which gives the routing key. The reporter triggers
an incident for each alert, using the alert's own key as PagerDuty's
de-duplication key, so a resend joins the incident it would have opened. It
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
(also send the day's summary); `-date YYYY-MM-DD` (default: the latest day in
the log); `-benchmark` (default SPY); `-heartbeat-timeout` (default 36 hours of
silence).

If it exits with an error, read it: it names the channel that failed or the
file it refused. Nothing is lost; the next run tries again.

## Running the dashboard

```
go run ./cmd/dashboard -journal joint.log -journal ira.log -addr 127.0.0.1:8088
```

Repeat `-journal` for each account. It will listen only on a loopback address
or a Tailscale address, so on the server it is reachable only over Tailscale.
Pages: `/` (the summaries), `/api/summary.json` and `/healthz`. Anything but a
read is answered "method not allowed". If a log does not verify, the page says
so without showing paths or figures.

## What the summary shows, and its limits

- Holdings come from the broker's account snapshot.
- Equity against SPY pairs the snapshot's net liquidation with SPY's closing
  price on the same day. It is price return only (no dividends) and needs two
  paired points before it shows a comparison.
- The fills list stays empty until the engine records fills for target-weight
  strategies.
