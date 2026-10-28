# Alerts, the daily summary and the dashboard

## What they do and why they exist

Two small programs watch an account's record of what the engine did and tell
you about it, so you do not have to go looking.

- `reporter` reads one account's log and sends your phone an alert whenever
  something needs your attention, and optionally a daily summary: what you
  hold, what the strategy wanted, the orders and fills, and whether your
  account is beating SPY. The two go to different places on purpose: an alert
  is a **page**, and a page always means an incident; the summary is routine
  and arrives only on your own ntfy server (see "Where alerts go").
- `dashboard` shows the same summary as a web page, one section per account,
  with a chart of your equity against SPY and a light or dark colour choice.
  It is read-only: you cannot click anything that changes the system. It shows
  the account's whole history, from its first day, across every restart and
  upgrade (see "The history across restarts" below).

Neither can place, change or cancel an order, and neither talks to the broker.
They only read.

A third, the triage job on the Mac mini, reads each alert issue and writes
one comment analysing it, with Claude Code ("Alerts analysed by Claude",
below). It too places nothing and only reads the server.

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
| An order was refused | urgent | refused order, except: a kill-switch refusal once per engagement (turning the switch off and on again is a new one), a refusal for a too-old IBKR snapshot once per stale episode, and a `pending-cash-decrease` refusal once per pending cash difference ([states-and-kill-switch](states-and-kill-switch.md)); a log written before the engine marked those first refusals alerts for every one |
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
| An order to be sent was not acknowledged by IBKR within `-ack-within` of its commit ([order lifecycle](order-lifecycle.md)). A report that the engine reads counts as the acknowledgement. In a Boost run a fill is read by the risk gate's own rule, so a fill that names the order and fits it (part or whole, even one that arrives after IBKR canceled the order) acknowledges it, and a fill the gate refuses (past the order's size, for another account or side, or a fill number reused for a different execution) does not. A refused fill on an order already acknowledged is paged as a report the engine did not read (two rows down) | urgent | order (its client order ID) |
| An order already alerted as unacknowledged has since been acknowledged | routine | order |
| An order the adapter never sent was retired: its day ended before it could be sent, or you retired it because IBKR could never accept it ([order lifecycle](order-lifecycle.md)) | routine, never a page | order; it also answers an earlier unacknowledged-order page for it |
| A report from IBKR in the log that the engine did not read: it does not validate, is at a version this build does not read (its client order ID may then be unknown, so the alert names the report itself), or does not match the order it names ([order lifecycle](order-lifecycle.md)); in a Boost run, also a fill the risk gate's rule refuses on an order already acknowledged | urgent | that report (its input ID) |
| No sign of life from the engine | urgent | silence, keyed by the last activity |
| The reporter cannot read the journal (its text says "The heartbeat is NOT checked" and why, whenever liveness is unmonitored) | urgent | account and problem |
| The run's settings file (`run.env`) is missing or empty although the account has run: the reporter cannot tell which log to read | urgent | account and sign of the run |
| The reporter cannot check the heartbeat (the calendar does not cover the span) | urgent | account and problem |
| The reporter set one delivery channel aside (its secret is empty or malformed, or its name is unknown); sent on the channels that still work | urgent | account, channel and problem |
| "state at reporter start": a channel began with this reporter after the log's earlier events (a new or lost state file, or a channel just named), and something is in force. The page names the engine state when Degraded or Halted, with its scope (an incident a fresh run's approval carried in force included), Boost's stopped buys (with the orders, and whether the boost is on, as soon as the second rejection is reported), the kill switch when engaged, a pending cash read, the margin-cushion alarm when not clear, and each open alert that follows it; each open alert is then sent marked "state at reporter start" and repeats as usual until acknowledged (see "The delivery rules") | urgent | channel: once, when the channel begins |
| "the daily summary has no channel: name your own ntfy server": `-summary` is set but no ntfy server of your own takes the summary (see "The delivery rules") | routine | account: once, until your own server takes the summary; again if it is later left out or set aside |
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
own machine cannot catch that machine dying, so every firing that finishes
without a fault also pings an outside service (Healthchecks), which pages you
through Pushover when the pings stop: that page comes from Healthchecks, not
from the reporter, names your check and no account, and means no clean firing
for 20 minutes: the server, the timer or the reporter's firings, or, most
likely, an engine or adapter left failed for 20 minutes, which first paged you
`<host>: <unit> failed`; act on that page, and this one clears at the first
clean firing after the unit runs again ([server](server.md), "The outside
heartbeat"). A trading unit
that stops (the engine, the adapter, the reporter) pages you at once through
the reporter's own channels ("The reporter itself", below, the row
`<host>: <unit> failed`).

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

On both channels, ntfy and Pushover, repeats come every `-repeat-interval` (default 30 minutes). On Pushover, each repeat is a **new emergency message**:
until the acknowledgement is recorded in the journal you will get one more
page, every `-repeat-interval`, and each page, if you do not acknowledge it in
the Pushover app, itself repeats on your phone every 60 seconds for 30 minutes
(the page's `retry` and `expire`, below). Acknowledging a page in the Pushover
app stops that message's own ringing only; it does not stop the reporter's
repeats, which follow the log. Once the journal records the acknowledgement, no
further message is sent, and the ones already sent expire on their own.
(The optional `pagerduty` channel opens a new incident for each repeat, named
by the alert key with `#repeat-1`, `#repeat-2` and so on, and you resolve
those in PagerDuty yourself.)

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
that one channel (delivery is at-least-once: ntfy and Pushover post it twice); every other alert is not sent again. `TestTheTimerMakesTheRepeatAndAcknowledgementBoundsTrue`
(`cmd/reporter`) checks that the timer runs around the clock, that its
period divides the repeat interval and is at most half the acknowledgement
bound, and that a run is stopped before the next firing.

## Where alerts go

Alerts go to real channels. You choose them with `-channel`, comma-separated.

| Channel | What it is | Secret |
| --- | --- | --- |
| `ntfy` | Push to your phone through ntfy. With `TI_NTFY_SERVER` set to **your own ntfy server** (on the Lightsail box, reachable only over Tailscale; [server](server.md), "Your own ntfy server") it carries every alert **and the daily summary**. With it empty, or naming the public ntfy.sh relay, it carries incident alerts only | `TI_NTFY_TOPIC`, an unguessable topic name; `TI_NTFY_SERVER`, your own server's URL |
| `pushover` | The second channel, chosen by the owner: Pushover, through its messages API. A page (an urgent alert) is an **emergency** (priority 2): the Pushover app repeats it every 60 seconds until you acknowledge it in the app or 30 minutes pass; any other alert is **high priority** (priority 1). **Incidents only**: the daily summary never goes here | `TI_PUSHOVER_APP_TOKEN`, the application's API token; `TI_PUSHOVER_USER_KEY`, your user key; optionally `TI_PUSHOVER_URL` |
| `pagerduty` | An optional channel the server no longer names (Pushover replaced it, because PagerDuty's sign-up refuses personal email addresses): PagerDuty, PagerDuty, through its Events API. **Incidents only**: the daily summary never goes here | `TI_PAGERDUTY_ROUTING_KEY`, the integration key; optionally `TI_PAGERDUTY_URL` |
| `github-issues` | One GitHub Issue per alert, readable on your phone in the GitHub app. **Incidents only**: the daily summary never goes here ("Alerts as GitHub Issues", below) | `TI_GITHUB_ISSUES_TOKEN`, an issues-only fine-grained token; `TI_GITHUB_ISSUES_REPO`, the repository as `owner/name`; optionally `TI_GITHUB_API_URL` (https, or http for a listed loopback host such as `127.0.0.1`; any host other than `api.github.com` must also be named in `TI_GITHUB_API_ALLOW_HOST`, with its port when the URL has one, so the token is never sent anywhere you did not name; the reporter never follows a redirect, so the token goes nowhere else) |
| `stdout` | Prints to the terminal or the system log | none |

Each secret is read from the environment, or, when `-secrets-file` names a
file of `KEY=VALUE` lines, from that file for any name the environment leaves
empty. On the server the secrets go in `secrets.env` (stored encrypted under
`/etc/trend-investing`; edit it with `sudo ti-secrets edit secrets.env`; the
names are in `deploy/etc/secrets.env.example`), whose unlocked copy systemd
hands the reporter as a private credential file; nothing puts them in its
environment. A secrets file that cannot be read, or a line in it
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
  figure; the figures go to Pushover and to your own ntfy server. When the
  reconciliation could not be verified (Halted), the short form says only
  that, and that you should look at the private channel: the reasons it could
  not compare can carry amounts and share counts, so they go to Pushover and
  your own ntfy server alone.
- **A page always means an incident.** Pushover pages you for every
  alert it is sent, so it is sent incidents only. The daily summary is routine
  and carries your balances, so it goes to your own ntfy server and nowhere
  else: never to Pushover or PagerDuty, and never to the public ntfy.sh relay, which
  anyone who learns the topic can read. A summary never opens an incident and
  is never resolved.
- With no ntfy server of your own taking the summary (ntfy absent, only the
  public ntfy.sh relay, or your server's channel set aside for a bad secret),
  the summary is not sent. The reporter tells you once, as a routine message
  on the incident channels, "<account>: the daily summary has no channel:
  name your own ntfy server" (on Pushover it is a high-priority message, not an emergency),
  and in the unit's output (`journalctl -u ti-reporter@boost`). It is said
  only in a run whose journal reads, since it names the account. Incident
  alerts are unaffected. When the route is simply absent (ntfy not named, or
  only the public relay), the run is not an error; when your server's channel
  is set aside for a bad secret, the run still ends with an error naming that
  channel, as for any channel set aside. Once your server takes the summary,
  the notice is forgotten (a PagerDuty incident, if that channel is named, is
  resolved first, and if PagerDuty is set aside then, or its resolve fails,
  its record is kept and the resolve retried at a later firing). It tells you again only if your
  server is later left out or set aside.
- ntfy takes a message of at most 4096 bytes. A longer one (an alert naming
  many instruments, or a long summary) is sent as numbered parts, each titled
  with the alert's title and "(1/3)", "(2/3)" and so on; read them in order.
  The parts together are the whole message. The time limit
  (`-send-timeout`) covers all the parts of one message together, not each
  part, so a message that cannot be sent within it fails the ntfy channel
  only and the other channels are sent the alert without waiting. If one part
  fails, or the time runs out, the whole message is sent again at the next
  firing, so you may see the earlier parts twice.
- The summary's channel failing (your server unreachable, say) fails the run
  with the channel named, and retries the summary at the next firing. It never
  holds back an incident page: the incidents are sent first, to every channel
  that works.
- The summary for a day is sent only once the log records that day's session
  closed. The day's decision and orders are written with the close, so a
  summary sent earlier would be incomplete, and, being sent once per day,
  would never be replaced. Before the close the summary is simply not due.
- If the summary cannot be built (a decision record the program cannot read),
  the incident alerts still go out and are remembered, and the run ends with
  an error naming the record.
- A **new or lost state file replays no history, but pages what is in
  force.** The first time the reporter sees a channel with no record in the
  state file (the file is new, was lost, or you have just named the channel
  in `-channel`), it records that moment for that channel in the file
  ("baselines"), and writes the file before it sends anything, so a run
  stopped midway cannot lose it. That channel is sent none of the routine
  history the log holds from events before it (old refusals, fills, withheld
  decisions); only events after it are sent. What is still in force is not
  history: the channel is sent, once, an urgent page "<account>: state at
  reporter start: Halted" (or Degraded, or Normal) naming the engine state and
  its scope, Boost's buys if two orders the venue rejected in a row have
  stopped them (with the count and the orders, and whether the boost is on;
  counted by the engine's own rule, from the latest decision's recorded
  count and the venue's reports and fills since it, so named from the
  report of the second rejection, before the next decision records it; an
  invalid report is a rejection; a fill the engine's fill rule accepts, or
  any report but "submitted", is the venue accepting the order ("submitted"
  is not enough: IBKR's PreSubmitted maps to it and can still be followed
  by a rejection); a fill the rule refuses, a report that does not describe
  the order as released, and a report on an order cleared as never sent
  count for nothing, as the engine refuses them; on a Boost account, a
  clearance the engine's Boost rule refuses (it names an order this run
  never released, one the venue has already named, or another account's) is paged on every
  channel as an urgent "operator clearance <id> refused" alert naming its
  orders, and named on the page; a clearance record the reporter cannot read
  or validate (a record that names an order twice does not validate), which
  the engine also refuses, is paged the same way as "operator clearance <id>
  not read", and its record ID is also shown on the page with "cannot be
  read"; a Boost
  account is the one whose configuration names Boost, so a clearance
  journalled after a fresh run's approval and before its first decision is
  judged the same way; a momentum account's clearances are never read
  this way, since that engine applies them through its own order reports; a decision record the reporter cannot
  read is named "cannot be read: treat the buy stop as possibly in force"
  beside any stop already known, and on a momentum account, which has no
  Boost stop, only as a record that cannot be read), the kill
  switch if it is engaged, a pending cash read, the margin-cushion alarm if it
  is not clear, and every open alert. Each open
  alert (a Degraded, Halted or reconciliation alert you have not
  acknowledged) is then sent to it, its title beginning "state at reporter
  start:", and repeats every `-repeat-interval` until you acknowledge it, as
  on any channel. This is per channel: adding Pushover to an account that has
  run for weeks starts Pushover at that moment with the state in force, and
  ntfy, which already has its record, is not sent anything again. A channel
  set aside for a bad secret is recorded too, so what it missed while set
  aside, and the state in force, reach it once it works. A channel that
  already has alerts recorded in a state file written by an earlier reporter
  is not new. If the reporter cannot write the file at that moment, the
  baseline is dropped and that channel is sent every alert the log holds,
  acknowledged Halted and Degraded incidents included, once each as it would
  have been at the time (an emergency on Pushover); and since nothing could be
  recorded, the same happens again at every firing until the file can be
  written (ntfy and Pushover show each again). This is noisy but never silent, and
  each such run ends with an error naming the write (the "reporter: write
  state" row below). The daily summary is keyed by
  its day, not an event, so a new state file sends the latest closed day's
  summary once.
- The reporter remembers what each channel has taken in its own state file
  (`-state`). That file is the only thing it writes, and it is flushed to disk
  before the run ends, so a power cut cannot leave it empty. If it is
  unreadable the reporter stops rather than starting over, because starting
  over would resend every alert.
- Secrets (the ntfy topic, the Pushover token and user key, the PagerDuty key) never appear in output or errors.

Pushover details: you buy the Pushover app once, create an application named
"Trend server" (its API token is `TI_PUSHOVER_APP_TOKEN`) and copy your user
key from the dashboard (`TI_PUSHOVER_USER_KEY`); [server](server.md), "The
pager", has the steps. The reporter posts a form to
`https://api.pushover.net/1/messages.json` (`TI_PUSHOVER_URL` names another
endpoint, which must be https, or http on this machine's own loopback address
for a stand-in; the reporter never follows a redirect, which could carry the
token elsewhere, and treats a 3xx answer as a failure) with the token, the user key, the
alert's title (at most 250 characters), a message and a priority. The message
is the alert's text followed by its key and account, within Pushover's 1024
characters: when it is too long the text is cut first, with an ellipsis, and
the key and account that end the message are kept whole up to their own
limits (the key at 512 characters, the account at 128; a longer one is cut
with an ellipsis). One alert is always one
message, so a long alert never opens a second emergency. A page carries priority 2 with `retry` 60
(seconds between the phone's repeats; Pushover's minimum is 30) and `expire`
1800 (when it stops; Pushover's maximum is 10800); anything else is priority 1.
It carries what the PagerDuty payload carries and nothing more: no account
number and no secret. The reporter reads Pushover's answer back, in full
and within a size limit: only a 2xx answer whose whole body is one JSON object
with `"status":1` is a delivery. Any other answer, a cut-off or oversized one,
or none, is a failure that is retried at the next firing, and the error is a
fixed message with the HTTP status code, or the kind of network fault (timeout,
DNS, refused connection, TLS), never the token, the user key or the address. Pushover has no de-duplication key, so the
rule against a second emergency is the reporter's own: an alert Pushover
accepted is never sent again except by the repeat rule above, and a message
Pushover did not accept opens nothing. The one exception is an answer lost
after Pushover accepted the message (the connection dropped, or a run stopped
between the send and the save): the next firing then sends it once more.
An emergency's answer carries a receipt, which the reporter keeps in its state
file (never printed or logged). Where PagerDuty would resolve an incident (the
clock check passing again, a failed unit seen well, a cleared notice), the
reporter cancels that alert's emergencies through Pushover's receipt API, so
the phone stops repeating a page whose fault has cleared. A cancel that fails
is kept and tried again at the next firing until the emergency has run its
30 minutes (plus 5), when Pushover has stopped it and the record is dropped.
Every firing drops the receipts that are past that, whatever alert they were
for, so a page that repeats for days does not fill the state file; the age is
read from the server's clock, and a receipt that looks newer than the clock
(the clock went back) is kept. If the clock was wrong when the page was sent
and has since been corrected, a receipt can be dropped early: the page then
runs until you acknowledge it or it expires. The clock check pages only a
drift that lasts through two checks in a row, so a clock put right between
two checks can go unremarked, and no clock page is promised. A receipt is kept, not dropped, when the incident clears while the
Pushover channel is set aside for a missing secret, and is cancelled when the
secret is back. If the channel is still away when the receipt passes its
expiry and margin, the receipt is dropped anyway, together with the record of
the cleared incident, so nothing is left in the state file. A receipt Pushover no longer knows (HTTP 400 whose answer says
the receipt was not found) has nothing left to cancel; any other 400, such as an
invalid application token, is a failure that is retried. An emergency whose
answer carries no usable receipt is still a delivery (the phone rings), but
the reporter prints a warning that it cannot be cancelled and it runs until you
acknowledge it or it expires. An answer with anything after its one JSON object
(a stray bracket, a second object) is not a delivery, and only a space, tab, newline or carriage return may follow the object. The journal's acknowledgement does not
cancel a page, as it does not resolve a PagerDuty incident: it stops the
reporter's repeats, and the page already sent expires on its own or when you
acknowledge it in the app.

PagerDuty details (the optional channel): you sign up for PagerDuty and create a service with an
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

## Alerts as GitHub Issues

The server's reporter and failed-unit hook name `github-issues` in
`-channel`, so once the token and repository are filled in, every alert also
becomes an issue in the repository named by `TI_GITHUB_ISSUES_REPO`, with the label `alert`. It is a
third place to read the alert, with the context beside it, in the GitHub app
on your phone. The alerts repository is **private**; only the code and the
fix PRs are public (see "Two repositories", below). Pushover still pages you, and ntfy still pushes; the issue is
not a replacement for either, and a fault in it never holds them back: every
alert of a firing goes to Pushover and ntfy before any goes to GitHub, and
so does every reminder of a failed unit. A firing's work on GitHub, every
request of every alert together, is bounded by one `-send-timeout`, counted
only while GitHub is being asked (the firing's own work between two alerts,
such as the unit and clock checks, does not use it up), and when GitHub
cannot list the alert issues, runs out of that time, limits the rate, answers with a server error (5xx), or cannot be
reached or stops answering half-way, it is not asked again in that firing:
the firing's other alerts fail on it at once and are all sent again at the
next firing.

**What an issue holds.** The title is the alert's title. The body is the
alert's text, then "Last journal records (redacted)": the journal's last ten
records, one line each, then the output of three `journal-query` reports over
the journal (`reconcile`, which is the reconciliation detail, `refusals` and
`orders`), each cut to its last ten rows. A line or a table cell longer than 4,096
bytes is cut there and ends " [cut]". A journal over 64 MiB is not read for
the issue: the issue carries no journal part, only the line "evidence
withheld: journal over the limit", and so does every later issue while the
journal stays over it (the journal itself is on the server). A hidden line at the end ties the
issue to the alert's key; leave it alone, and leave the `alert` label on the
issue, since the reporter finds its issues by that label. GitHub takes at
most 65,536 characters in an issue or a comment, so a longer one is cut: the
journal part first, then the alert's text, ending "[cut to fit GitHub's
limit; the rest is in the journal on the server]".

**One issue per alert.** An alert is one issue however many times it is sent.
An alert that repeats until you acknowledge it ("Repeated until you
acknowledge it") adds one comment per repeat to the same open issue, never a
second issue, so your phone buzzes at each repeat as it does for the other
channels. A failed unit's hourly reminder is a comment on its issue too. A
delivery the reporter sends again, because GitHub's answer was lost, writes
nothing more: each comment carries a hidden line naming its repeat or its
reminder, and one already there is not posted twice. Only the reporter's own
comments (those of the token's user) count: a hidden line someone else
pasted into a comment does not stop a repeat. A comment GitHub names no
author of could be the reporter's, so then nothing is written and the
delivery fails (it is tried again at the next firing). An alert that comes
back after it resolved is a new incident and opens a new issue.

**What is kept out.** The alerts repository is private, so only you (and anyone
you add to it) can read an issue, but it can be shared or made public later,
so the redaction holds regardless.
The reporter therefore writes only the alert's *public* text when it has one,
the text that names what is in doubt and carries no balance (the same text
the public ntfy.sh relay gets), and it removes, from every part of the issue,
each secret in the secrets file, every IBKR account id (the letters U or DU
and digits, or a paper account's DU with letters among its digits, such as
DUX1234567, removed whole), any GitHub token, any Tailscale host name (`...ts.net`) or
address (`100.64.0.0/10`), this server's own names, and every figure. A
secret or name shorter than six characters is removed where it stands as a
word. The issue carries no quantity, price, limit, fill, share count,
position, size or P&L, in any part of it:

- in the journal records, the value of any field that holds cash, equity,
  liquidation value, margin, an amount, a cost, an account, a quantity, a
  price, a limit, a fill, shares, a position, a size, a weight or a P&L is
  replaced wherever it sits, and so is every other number;
- in the query output, the same rule leaves out those columns;
- in the alert's text and in every other piece of text, each number is
  replaced: a digit of any script, and every other numeral (a superscript, a
  fraction, a circled number, a roman-numeral character), whatever stands
  beside it (a letter, a currency sign, a dash, a colon, `#`), unless the
  whole word it sits in is on this list, and only this list:
  - an ISO date `YYYY-MM-DD`, in a year from 1900 to 2099, on the calendar
    (a real month and a day that month has: `2024-02-29` is kept,
    `2026-02-31` is not), alone or followed by `T` and a time `hh:mm` or
    `hh:mm:ss` on the 24-hour clock (`00:00` to `23:59:59`), with no
    fraction of a second, optionally ending `Z` or an offset `+hh:mm` or
    `-hh:mm`, such as `2024-01-02T21:00:00Z`; a date followed by a `T` with
    no time after it is not kept, and neither is a time of day on its own
    (`21:00`);
  - an ADR's number, four digits after `ADR `, such as `ADR 0019`.

  Everything else goes: a record number, an order or fill name such as
  `tw-17`, a duration such as `30m` or `5s`, a version such as `v2`. The
  issue needs none of them; the hidden line ties it to its alert.

Each is shown as `[redacted]`. A secret is removed also in the escaped form
a journal record writes it in. So an issue tells you what happened and where
to look, and the figures stay on the server: read them with `journal-query`
or on the dashboard. The daily summary carries your balances and is never
sent here.

**How an issue closes.** The reporter closes it, with a closing comment, when
the alert resolves:

- an alert that repeats until you acknowledge it closes when the journal
  records your acknowledgement (`operator acknowledge`); it is not opened
  again;
- a page the reporter resolves for the other channels, such as a failed unit
  that recovers or a clock check that passes again, closes its issue in the
  same firing;
- a broken history that is whole again, or a restored-log alert whose folder
  no longer holds a newer log, closes its issue at the end of the firing that
  finds it so, so the same fault arising again opens a new issue that
  notifies you. If GitHub refuses that close, it is tried again at the end
  of each of the next firings, whether or not the fault is back; after 3
  firings it is given up, and the fault's next incident opens a new issue
  that begins by naming the earlier one, left open, for you to close by
  hand. Only a close GitHub was asked to make and refused counts toward the
  3; a firing in which GitHub could not be reached, or had no time left, does
  not.

**Design note: a resolved issue leaves the `alert` label.** When the reporter
resolves an issue it gives it the label `alert-resolved` and takes the `alert`
label off, so resolved incidents leave the reporter's list of at most 1,000
issues. The same step takes them out of every view, saved search or job that
selects issues by the label `alert`: any job that needs to find resolved
incidents (a triage job, say) must poll by `alert-resolved`, or by the hidden
key line and the issue's state, never by `alert` alone. An issue you close
by hand keeps the `alert` label, and counts toward the 1,000, until its alert
resolves, when the reporter swaps the label for `alert-resolved`; the issue of
a single-event alert keeps it until you remove it yourself.

An alert that reports a single event, such as one refused order, has nothing
that resolves it: its issue stays open until you close it yourself in the app
once you have read it. A close the reporter could not make, because GitHub
could not be reached, is tried again at the next firing; its closing comment
is posted once, however many tries the close takes. An issue with more than
1,000 comments is closed with no closing comment, since the reporter cannot
read them all to see whether one is there; once the reporter has
closed it, it counts as done, and the alert's next incident opens a new
issue. That new issue names the earlier one and says why only while the
earlier issue still has the `alert` label (you closed it yourself and left
the label on); an issue the reporter resolved has lost the label, so the new
issue carries no link to it, and the closed issue is found by the label
`alert-resolved`.

If you close an issue yourself, the reporter leaves it closed. An alert that
goes on repeating after that, because it is not yet acknowledged in the
journal, neither reopens that issue nor opens another, and nor does a
first delivery the reporter tries again: its repeats still reach Pushover
and ntfy. Only once the alert has resolved (acknowledged, cleared or
recovered, as above; the reporter then adds its closing comment to your
closed issue) and then happens again is it a new incident with a new issue.
An issue whose `alert` label you remove, or that you delete, is out of the
reporter's sight: a repeat or reminder of it opens no new issue.

**If issues stop appearing.** Pushover and ntfy still get every alert, but
a firing whose GitHub work failed ends with an error: the reporter's unit
fails, so you get its urgent `<host>: ti-reporter@boost.service failed` page
(and its hourly reminders while every firing fails), and the firing sends no
outside heartbeat ping, so Healthchecks pages too if it goes on (rows below).
Expect both until GitHub recovers, or stop using the channel for now with a
systemd drop-in (the unit files themselves are replaced by every
`provision.sh services`, so never edit them): run
`sudo systemctl edit --drop-in=no-github-issues.conf ti-reporter@boost`, put this in
the editor and save:

```
[Service]
ExecStart=
ExecStart=/opt/trend-investing/bin/ti-reporter -run-env /var/lib/trend-investing/engines/%i/run.env -journal ${TI_STATE_DIR}/${TI_RUN}.log -state ${TI_REPORTS_DIR}/%i.reporter.json -secrets-file %d/secrets -channel ntfy,pushover -summary -account %i -clock-check /usr/local/libexec/trend-investing/clock-check.sh -clock-state ${TI_REPORTS_DIR}/host-clock.json -cash-ledger /var/lib/trend-investing/flex/%i/cash-movements.jsonl -units ti-engine@%i.service,ti-adapter-%i.service -resolve-units ti-data.service -self-unit ti-reporter@%i.service
```

then run `sudo systemctl edit --drop-in=no-github-issues.conf ti-unit-failed@.service`
and put this in the editor for the failed-unit hook:

```
[Service]
ExecStart=
ExecStart=/opt/trend-investing/bin/ti-reporter notify-unit-failed -state /var/lib/trend-investing/reports/unit-failures.json -secrets-file %d/secrets -channel ntfy,pushover %i
```

(each copies the installed unit's `ExecStart=` line with `github-issues`
taken out of `-channel`; compare with `systemctl cat ti-reporter@boost` and
`systemctl cat ti-unit-failed@.service` first, because a later unit file may
differ). **Warning:** while these drop-ins exist they keep that exact command
over the unit file that every later `provision.sh services` installs, so a
newer release's changed command line is silently ignored. Remove them as soon
as GitHub works again, and before or right after any upgrade. To go back,
remove exactly these two files and nothing else (your other overrides stay;
`systemctl revert` would remove all of a unit's drop-ins):

```
sudo rm /etc/systemd/system/ti-reporter@boost.service.d/no-github-issues.conf
sudo rm /etc/systemd/system/ti-unit-failed@.service.d/no-github-issues.conf
sudo systemctl daemon-reload
```

`systemctl cat` shows the override while it is in place. First read the run's error: a failed firing names `github-issues:` and why. The
usual causes are in the table "What can go wrong", below: the token expired or
was revoked ("GitHub answered 401 Unauthorized"), the token cannot write the
repository's issues ("403 Forbidden" or "404 Not Found"), GitHub was
unreachable, or `TI_GITHUB_ISSUES_TOKEN` or `TI_GITHUB_ISSUES_REPO` is empty
("reporter: the github-issues channel needs ..."). The alert is not lost: it
is sent again at the next firing, and an issue is made then.

**The token.** Create it once, and again when it expires:

1. On GitHub, open Settings, Developer settings, Personal access tokens,
   Fine-grained tokens, Generate new token.
2. Name it `ti-alerts`, set an expiry (90 days is a good length), and under
   "Repository access" choose "Only select repositories" and pick the one
   repository.
3. Under "Repository permissions" set **Issues** to "Read and write", and
   leave every other permission at "No access". (Metadata read-only is added
   by GitHub and is fine.) An issues-only token cannot read code, change
   settings or trade; if it leaks, the most it can do is write issues.
4. On the server, `sudo ti-secrets edit secrets.env` and fill in
   `TI_GITHUB_ISSUES_TOKEN=` (the token) and `TI_GITHUB_ISSUES_REPO=`
   (`owner/name` of the private alerts repository). **Do this before the unit
   files that name `github-issues` are installed** (`sudo deploy/provision.sh
   services`): the next firing uses the values. The reporter's unit and the
   failed-unit hook already name `github-issues`; there is no `-channel` to
   edit. Until both are filled in, the channel is set aside and every
   firing fails, sends no heartbeat ping (so Healthchecks pages you at
   Emergency) and the failed-unit page repeats hourly (the table "What can go
   wrong", below).
5. To rotate it, make a new token the same way, replace the line, and delete
   the old token on GitHub. Put a reminder in your calendar for its expiry:
   an expired token is a channel that fails every firing.

Never paste the token into an issue, a chat or the repository. The reporter
never prints it: it is removed from every message and issue.

## Alerts analysed by Claude

A job on the Mac mini reads each alert issue, asks Claude Code what
happened, and writes the answer as one comment on the issue, usually within
minutes of the issue opening. The comment says what happened, which rule
applied (the ADR or manual section), and which of three kinds it is: a
**code defect**, a **data problem**, or an **owner action**. For a code
defect it may also open a fix as a draft pull request. For an owner action
it says what you must do, and never does it.

**Two repositories.** The alert issues live in a **private** repository of
their own (the alerts repository, `-alerts-repo`); the code, and every fix
PR, stay in the public one (the code repository, `-code-repo`). Given only
one of the two settings, the job uses that repository for both. The
reporter's `TI_GITHUB_ISSUES_REPO` on the server must name the same private
alerts repository, so the issues the reporter opens are the ones the job
reads. A fix PR, being public, links its alert issue by the issue's full
address (`https://github.com/<owner>/<alerts-repo>/issues/<n>`), which only
you can open, and quotes none of the issue's text: its words are Claude's,
through the redactor.

**How it works.** launchd runs `ti-triage` every five minutes, as the
macOS user `ti-triage` (setup: `docs/runbooks/mac-mini.md`, "2. The alert
triage job"). Each run is one poll:

1. It first removes any gate run folder older than a day (left by a job
   killed during a check). Then it lists the alerts repository's issues
   with the label `alert`, then those with `alert-resolved` (an alert that
   cleared between two polls has swapped one for the other; "Design note: a
   resolved issue leaves the alert label", above; listing `alert` first
   means an issue relabelled between the two listings is in the second, and
   no other issue or pull request can fill a listing and keep an alert out
   of reach): the open ones, newest first, opened since the job was first
   switched on, and the closed ones only if they closed in the last 30
   days. Issues opened before the job was switched on are left alone, so
   switching the job on never sweeps up old incidents. Every poll lists all
   of them, every page; the job's state file records each issue it has handled (analysed,
   skipped past the daily limit, or failed), and an issue in that record is
   not read further. A listing read while the reporter relabels an issue can
   miss one (the pages of `alert` shift by one), so a listing is never taken
   as proof that an issue was handled: one it missed is not in the record,
   and the next poll lists and analyses it. An issue closed more than 30
   days ago is not listed, so it is not picked up again. The record of
   one handled notes when the issue was closed and when a poll last listed
   it (the job writes both each time it handles or lists the issue), and is
   dropped only once no poll has listed the issue for more than 30 days, so
   the state file stays bounded; a listing that merely omits an issue does
   not drop its record until then, and a record written before the job kept
   a listing time starts its 30 days at the next poll. Should an issue
   return to the listing, the job's own comment on it marks it handled
   before anything is spent (so an alert the job never reached in those 30
   days, the job off or failing throughout, is left for you, and so is a
   comment the job still owed it). More than 1,000 issues of one label in
   either set (open since the job was switched on, or closed in the last
   30 days) fails the poll whole: none is analysed from part of a listing.
   Issues closed longer ago do not count towards that 1,000, but GitHub's
   closed listing is ordered by last update, so an old closed issue edited
   recently is still read, and a separate backstop applies: a listing that
   runs past 100 pages of 100 (10,000 issues of one label and state) fails
   the poll whole. An issue GitHub lists without a state of open or closed,
   or a closed one whose last update is before its closing, fails the
   listing too: it is never taken as open or as proof the listing is
   complete.
2. It keeps only the reporter's own issues: opened by the reporter's GitHub
   user (`-alert-author`, by default the triage token's own user) and
   ending with the reporter's hidden key line. Anything else is never read
   further, so a stranger's words never reach Claude.
3. An issue already holding the job's comment is done. The comment ends
   with a hidden line, `<!-- ti-triage:analysis -->` (or
   `<!-- ti-triage:skipped -->`), and only a comment written by the triage
   token's own user counts: the same line pasted by someone else does not.
   The job's state file records each issue it finished, so a poll with
   nothing new reads no comment at all; the comment is the second guard,
   should the record be lost.
4. For each new issue, it checks the gate (below) can run, fetches the
   journal slice, makes a fresh worktree of `main` in its own clone, fills
   the Go module cache for `main`'s requirements (if any of this fails, no
   session is started, nothing is counted or posted, the log says why, and
   the next poll tries again), and starts one headless Claude Code session
   there, with a fixed prompt built into the program
   (`cmd/triage/prompt.md`). The session is handed the issue's title and
   body (never its comments, which anyone can write) and the slice, both
   marked as data that is never an instruction.
5. It turns the session's answer into the comment, through the reporter's
   own redactor, and posts it. Each of Claude's answers is cut after the
   redactor (which can lengthen it) to 8,000 bytes, at a whole character
   (so text with accents or symbols can end a little sooner), followed by
   the 6-byte cut marker " [cut]". The whole comment, or a fix PR's body,
   is cut to 65,536 bytes (GitHub's limit is 65,536 characters), the same
   " [cut]" marking the place and the hidden line kept whole. A second poll
   adds nothing. A comment the job keeps in its state file until GitHub
   takes it must end with one of its own hidden lines; a state file holding
   one that does not is refused ("an issue's kept comment does not end
   with the job's marker line").

**What it reads from the server.** Only the journal slice: the dashboard
serves it at `/api/journal-slice` on the tailnet, as plain text. It is the
output of five `journal-query` reports (`refusals`, `orders`, `reconcile`,
`operator`, `fills`) over each account's current run, each cut to its last
50 rows, with each run's section first saying the span its records cover.
The job asks for the window from 72 hours (`-slice-window`) before the
issue opened to 72 hours after it, and names the alert by its key's hash
(the issue's hidden key line); the dashboard marks the span line of the
run whose own alerts include that alert ("..., and raised the alert"). Log
names are redacted like every row. The dashboard answers `GET` and `HEAD` only, on every
route, so nothing can be written through it, and the job sends no password,
token or cookie to fetch it: it holds no credential for the server at all.

The slice carries **no figure**: the dashboard leaves out every withheld
column (sizes, prices, balances, accounts and the like) and removes every
figure and identifier left, with the reporter's own redactor, so it is no
wider than the dashboard's page. The job then passes the slice and the
issue through the same full redactor again before the session sees them:
no account id, token, Tailscale host, this Mac's names or figure reaches
Claude, and so none can reach Anthropic, a test, a commit or a push. Dates
and words stay. The analysis works from what happened, not from the
amounts.

The job checks the slice covers the alert: the run that raised it, the
alert's own account's, must have begun before the issue opened and reach
into the window; another account's run never counts. If it does not (the
run was replaced since, say, or the alert is not one a journal raises),
the session is told so instead of being
handed rows of another run, and the comment says "**Journal slice.** The
journal slice did not cover the alert's run and time".

**What the session and the gate can and cannot do.**

The session (Claude Code, as `ti-triage`):

- **Can** read and search the worktree, edit files in it, and ask for
  exactly `go test ./...`, `go vet ./...`, `gofmt -l .`, `git diff`,
  `git status`, `git log -n 100` and `git show`, with no other flag, each
  through the job's own program (`ti-triage tool go-test` and so on: the
  only command lines allowed). That program runs nothing itself: it sends
  the command's name, and nothing else, to the job over a socket in a
  folder only `ti-triage` can open, and prints the answer. The job,
  outside the session's sandbox, runs that name's fixed command: `go test`,
  `go vet` and `gofmt` in the gate (below), as `ti-triage-gate`, over a
  copy of the worktree as the session left it, its new files included;
  the four `git` commands itself, in the worktree, with git's system and
  global settings cut off and no hook or file-system monitor, so git runs
  nothing the session wrote. So no code the session wrote ever runs as
  `ti-triage`, the user that holds the Claude token. Which cancellations count:
  a command the session gives up on (Claude Code's own time limit for a
  command, or the session's deadline) while the session's own command
  (`go test`, `go vet`, `gofmt`) runs in the gate is an interrupted
  command, never a gate outage; it is stopped in the gate too, and the job
  does not keep the alert for a later try because of it. A cancellation seen
  before the launcher process (`sudo`) has been started, whether the
  command had not begun or starting it failed with the session already
  stopped, is an interruption too. Once that process has started, a
  cancellation does not excuse a command that left no launch mark: that is
  a gate outage. Any failure or overrun of a
  gate-side step is a gate outage, always, whatever the session did
  meanwhile: reading the objects and making the copy (with its archive),
  reading the ports in use, the launcher (`sudo`, `sandbox-exec` and `env`
  starting a command), and ending the gate user's processes after it. Each
  of the first two steps runs under a time limit of its own, 60 seconds,
  safely under Claude Code's default two-minute limit for a command, and
  does not follow the session's cancellation; a step inside another (the
  ports read for the copy) ends no later than the step around it. The
  `sudo` that starts the session's own command is part of that command's
  own time, not a step with a limit of its own. Nothing else is allowed,
  and in a headless session nothing can be asked: the web, every MCP
  server, `git commit` and `git push`, `go build`, `gh`, `ssh`, `curl`,
  `tailscale`, `sudo` and the like are denied.
- **Cannot** reach the network from any command it runs: every command
  runs in Claude Code's own sandbox (`--settings`: `sandbox.enabled`, which
  must be available or the session does not start, no command let out of
  it, no network domain allowed and no Unix socket but the job's, writes
  only in the worktree, reads under `/Users` denied but for the worktrees
  and the job's program, and the token folders unreadable). The session runs in a
  process group of its own, ended whole at its time or the job's stop. Its file tools cannot leave the
  worktree (`--restricted`, and reads outside the working folder are
  blocked). Your own Claude Code settings are not loaded.
- **Cannot** use a GitHub token, an SSH key, a git credential helper or an
  Anthropic API key: none is in its environment (`ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN` and any key helper are cleared; git's system and
  global settings are cut off, so the macOS keychain is never asked; `gh`
  points at a configuration that does not exist).
- **Holds** one secret: the Claude token, in Claude Code's own
  environment, which is how it signs in and bills your subscription.
  Nothing the session wrote can read it: its tests run as
  `ti-triage-gate`, which cannot read another user's processes or files,
  and the only program the session's commands start as `ti-triage` is the
  job's own wrapper, which reads nothing and runs nothing (Claude Code's
  sandbox also unsets the token for it). What remains: Claude Code itself,
  and the model's own words, which the job passes through its redactor
  (the token's text is removed from everything it posts or pushes; an
  encoded copy of a token the model never sees is not a risk it can pose).
- **Never** performs an owner action. It has no way to: the job holds no
  SSH key and no login for the server, the broker or the secrets file, and
  the slice is read-only. An owner action is stated in the comment under
  "What you must do", in the runbook's words, and left to you.
- **Never** merges, marks a pull request ready, asks for a review, or
  mentions anyone: every `@` mention is taken out of what it writes, so it
  never calls `@coderabbitai` or `@greptileai`.

The gate (the code the session wrote, when the job tests a fix):

- **Runs** as a second macOS user, `ti-triage-gate`, with a group of its
  own (never the job's user, never root: the job checks), which cannot read
  `ti-triage`'s home or its token folder, through one fixed `sudo` rule,
  inside `sandbox-exec` with a profile that denies the network except
  loopback (so a test's own local servers work; DNS is denied too), and on
  loopback every port another program on the Mac listens on when the
  command starts and IB Gateway's and TWS's API ports (4001, 4002, 7496,
  7497); every Unix socket but those in its run folder and its run's own
  socket folder (`/tmp/tig-<run>`, named to the tests in
  `TI_TEST_SOCKET_DIR`), so no host service's socket under `/tmp` is in
  reach; every write outside its run folder and the shared temporary
  folders; and every read under `/Users` but its run folder, the module
  cache, the vulnerability database, the golden folder and the Go
  toolchain. The ports in use are read again just before each command.
  What remains: a program that starts listening on loopback while a
  command runs (up to `-check-timeout`, 45 minutes) is not denied to that
  command; IB Gateway's ports are always denied, so what is left is
  whatever else you run on the Mac mini's loopback during a check.
- **Runs** over a fresh copy of one commit (a check's), or of the
  worktree as the session left it (the session's own `go test`, `go vet`
  and `gofmt`, in one run for the whole session, so its build cache
  serves its next command), which the gate user itself
  unpacks and makes a git repository, so git never refuses it as another
  user's; the job starts `sudo` from `/` and the gate user enters the copy
  (`env -C`), since the copy is the gate user's alone; never in the job's clone or worktree, with an environment the job builds whole: an empty home, the
  module cache read-only and offline (`GOFLAGS=-mod=readonly`,
  `GOPROXY=off`), Go's vulnerability database from a file, and no token of
  any kind. Afterwards every process the gate user still runs is ended
  (`pkill -KILL -U` with the gate user's own UID, the only target the job
  ever gives it, after `sudo -l` confirms the rule allows exactly that),
  the job checks with `ps` that none is left (a process still listed is
  sent the same `pkill` once more, then checked again), and the copy is
  removed. If they cannot all be ended, or `ps` cannot list them (it fails
  or runs out of time), the command does not count as passed: the gate is
  unavailable for that run, and for a session's own command the issue is
  tried again later, whatever the session answered.
- **Checks** exactly the tree that is pushed: the job writes the copy's
  archive itself from git's objects, file by file, so a `.gitattributes`
  the session wrote (`export-ignore`, `export-subst`) cannot leave a file
  out of the copy or change one. It writes each archive into a fresh folder
  of its own that the gate user can read but not change, and the run
  folder is sticky, so nothing the gate's code left there (a link where the
  archive was) can turn the job's write onto a file of the job's.
- **Cannot** reach either token, the network, the job's state, or the
  clone the job pushes from. If the gate user, `sandbox-exec`, the `sudo`
  rule or the gate's folder is missing or wrong, no session is started,
  nothing is spent, and the log says "gate unavailable"; the issue is
  analysed once the gate runs. If the gate cannot run a fix's red or green
  check (its run folder, the commit's copy, `sudo`, `sandbox-exec` or `env`
  failing, the check's program not on the gate's `PATH`, or the gate
  user's processes not all ended after it), that is the gate's failure,
  never the change's (each command marks itself started, as the gate user,
  just before it runs; one with no mark never ran): the issue is
  analysed again at the next poll, once; a second time, the analysis is
  posted with "No fix PR was opened: gate unavailable".

**What a comment holds.** "Triage", then **What happened**, **Which rule
applied**, **Kind**, then **Journal slice** if the slice did not cover the
alert, and for an owner action **What you must do**, or for a code defect
**Fix**. Every word Claude wrote goes through the same redactor as the alert
issues ("What is kept out", above): no secret, account id, token, Tailscale
host or figure reaches the comment; only dates and ADR numbers keep their
digits. The one thing not redacted is the link to a fix PR, which the job
builds from GitHub's own answer. Each of Claude's answers is cut at 8,000
bytes, at a whole character, followed by the 6-byte cut marker " [cut]".

**A code defect's fix.** Only for a code defect, the session may change the
worktree, tests first. The job then takes the change through the
repository's normal flow, mechanically, and opens it only if every step
passes:

1. Every changed file must be eligible: a fix is only `*.go` files under
   `cmd/`, `internal/` and `transport/`, and files under `docs/`; its tests
   only `*_test.go` files and `testdata/` fixtures under those folders. A
   change touching anything else (the Makefile, scripts, CI configuration,
   `.claude/`, `deploy/`, `go.mod`, `go.sum`) or holding a binary file, a
   link or an oversize file is not opened, and neither is one with no test
   or only tests.
2. It commits the tests alone, then the rest, as `TrendInvesting triage`.
3. **Red.** In the gate, at the tests' commit, it runs `go test -run` over
   the test functions the tests' commit adds. One of them must **fail by
   name**: a test that passes, a test file that does not compile, a
   failure of some other test, a timeout or a commit adding no test
   function is not red, and a build failure anywhere in the output counts,
   even one long before the end the job keeps. **Green.** In the gate, at the fix's commit, it
   runs `-check-cmd` (default `make check`), which must **pass**.
4. It checks the commits' messages, every path they add, rename or remove,
   and every added line with the reporter's identifier rules, and pushes
   nothing that holds an account id, a token, a secret or a host name, or
   any binary file, which no line check can read.
5. It pushes the branch `triage/alert-<issue>` to the code repository
   (never forced, never to `main`) and opens a **draft** pull request into
   `main`, whose body links the alert issue and says the tests came first.
   The comment links it.

The draft then waits for you: mark it ready to start the review bots and
the merge gate as for any other PR, or close it. When any step fails, the
comment's **Fix** line says which, and nothing is opened.

**What it costs, and the cap.** A poll that finds no issue without its
comment makes no model call; it only lists issues on GitHub. Each issue gets
one session: the call is recorded in the state file, and synced to disk,
before it is made, so a run that dies during it does not make it again (the
issue then gets "triage failed: the analysis did not finish"). The one
exception is the job's own stop: a session ended by a SIGTERM or launchd's
`bootout` (an upgrade, say) leaves no comment, and the next poll analyses
the issue again, once; stopped a second time, it is spent (a gate that
could not run a fix's checks shares that one retry). A session is
bounded by `-max-turns` (default 60) and `-claude-timeout` (default 20
minutes). At most `-daily-cap` sessions (default 10) are made in a New York
day, counted on the day of each call, read once the session's preparation
is done, immediately before it starts; past it, each new issue gets the
comment "triage skipped: daily limit" once, and is never analysed later.
A session uses the Claude subscription of the login it is given
(`-claude-token-file`), never a pay-per-use key.

**Switching it off.** Create the file `OFF` in the job's folder, as the
`ti-triage` user: `sudo -u ti-triage touch /Users/ti-triage/triage/OFF`.
Every poll after that reads nothing, calls nothing and writes nothing, and
says "triage: off" in its log. Delete the file to switch it back on; issues
opened while it was off are then analysed (each within the cap). To stop
launchd starting it at all, `sudo launchctl bootout
system/com.trendinvesting.triage`; to start it again, `sudo launchctl
bootstrap system /Library/LaunchDaemons/com.trendinvesting.triage.plist`.

**Its log.** `/Users/ti-triage/triage/triage.log`. It holds errors and the
job's notices only: a poll that fails names the issue and why ("triage:
issue #41: GitHub answered 502 Bad Gateway"), and the next poll tries
again. A quiet log does not show the job is running (launchd's `print`
shows its last exit and runs), nor that nothing was analysed (the issues'
comments show that). Its rows are in "The triage job", below.

## Running the reporter

On the server it runs as `ti-reporter@boost.service`, started by its timer,
with:

```
/opt/trend-investing/bin/ti-reporter \
  -journal /var/lib/trend-investing/engines/boost/${TI_RUN}.log \
  -state /var/lib/trend-investing/reports/boost.reporter.json \
  -secrets-file %d/secrets -channel ntfy,pushover,github-issues -summary -account boost \
  -cash-ledger /var/lib/trend-investing/flex/boost/cash-movements.jsonl
```

`TI_RUN` comes from the engine's `run.env`, which systemd reads again at
every firing, so after a fresh run the next firing reads the new log. Before
the account's first run there is no `run.env`: the unit passes `-run-env`, and
the firing logs that it is waiting and skips only the reading of the log; the
channels, the trading units, the clock and its own failure page are handled as
usual, and the heartbeat is pinged when nothing is wrong. A missing or empty
`run.env` with a sign of a run (the reporter's `<account>.ran` marker or a run log in the folder) is an alert, a failed firing and no ping. The
state file is per account, not per run, so an alert already delivered is not
sent again from a later log. The reporter only reads the engine's directory
and writes only in `/var/lib/trend-investing/reports`. Check a run with
`systemctl status ti-reporter@boost` and `journalctl -u ti-reporter@boost`.
Only the Boost account's timer is enabled: the IRA engines have no
configuration yet.

The `github-issues` channel runs on the server, as part of the unit above;
its two values, `TI_GITHUB_ISSUES_TOKEN` and `TI_GITHUB_ISSUES_REPO`, live in
the encrypted `secrets.env` (see "Alerts as GitHub Issues", above). Without
both, `github-issues` is set aside and the run fails.

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

Besides the page and `/api/summary.json`, it serves `/api/journal-slice`:
the output of `journal-query`'s `refusals`, `orders`, `reconcile`,
`operator` and `fills` over each account's current run, each cut to its
last 50 rows, as plain text, headed by the log's file name (never its
path) and the span its records cover (`## <log>: covers <first> to
<last>`). It takes an optional window, `from` and `to` as RFC 3339 times
(`?from=2026-10-19T15:00:00Z&to=2026-10-22T15:00:00Z`), and keeps the rows
of the days it touches; a malformed window is answered 400. It carries no
figure: withheld columns are left out and every figure and identifier left
is removed, as in an alert issue, so it shows no more than the page. It is
what the triage job reads ("Alerts analysed by Claude", above); like every
route, it answers `GET` and `HEAD` only. The tailnet's policy admits only
your devices and the Mac mini to it (`docs/runbooks/lightsail-deployment.md`,
step 12).

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
- **One section per account.** Its first card is the equity comparison,
  followed by the costs card ("Costs: commissions, fees and interest",
  below) and the change-between-paired-points card. Then come the day's holdings, the targets the strategy decided, the
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
  each weekday morning (5:30 New York, again at 6:30 and 7:30), and IBKR may post a
  day's transfer only after its own end-of-day processing, so when the summary's day is later
  than that it adds "deposits or withdrawals from the last two trading
  sessions may not be recorded yet; a transfer appears in the summary two
  trading sessions after it posts (a Friday transfer on Tuesday's)". A
  transfer posted on day D is in the statement the reader reads the morning
  after it, or the morning after the next session at the latest, after that
  day's summary is sent, so the summary two sessions after D is the first
  that is sure to include it. A session is a
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

### Costs: commissions, fees and interest

**In short: nothing to do.** The costs card, under the equity comparison,
adds up what the account has paid IBKR, so you can see what trading and
borrowing cost. Its table has one column for the **month to date**, one for
the **year to date** and one **since the start** (the account's first
recorded day), and one row each for:

- **Commissions**, what IBKR charged for each trade;
- **Fees**, such as market-data or other fees IBKR posts to the account;
- **Interest paid**, the interest on money borrowed on margin;
- **Interest received**, the interest IBKR pays on cash, which is money *in*;
- **Net cost**, the first three less the fourth: what the account paid in all.

Under the table, one sentence says what the net cost since the start is as a
share of your **average net liquidation** (the mean of the account's recorded
net liquidations from its first day to the summary's day), and a second says
exactly what is counted and what may be missing. The reporter's daily summary
carries one line, "Costs this month: commissions X, fees Y, interest Z", where
Z is the interest paid less the interest received. The dashboard's
`/api/summary.json` carries the same block under `Costs` for each account
(`Month`, `Year` and `Since`, each with `Commissions`, `Fees`, `InterestPaid`
and `InterestReceived`, and `Share` of `AverageNetLiquidation`).

**Where the figures come from.** Commissions on trades come from the
account's own record: IBKR reports each execution's commission, and the
engine's log keeps it with the fill, so every fill from the account's first run
on counts. Fees and interest, and any commission IBKR posts to the account
without a fill (an adjustment), come from the **costs ledger**, which the Flex
reader writes beside the cash ledger each weekday morning
(`/var/lib/trend-investing/flex/<instance>/costs.jsonl`; the reconciliation
chapter, "Deposits and withdrawals, read from IBKR", says how). The reader
cannot see the engine's log, so it writes every commission IBKR posts, and
the reporter and the dashboard do the matching:

- **A commission is counted once.** A commission posting whose IBKR ID is a
  logged fill's execution ID, or a correction of that execution (IBKR keeps
  the first part of the ID and changes the last), is that fill's commission
  and is left out of the ledger's total; and a fill logged again as a
  correction of the same execution is counted once, at its latest
  revision (the higher last part of the ID, whatever order the log holds
  them in). A commission adjustment under a numeric ID cannot be matched to
  an execution, so it is counted as an adjustment (an assumption, reconciliation
  chapter). A cost IBKR restates counts once, at its latest value (a cost
  restated to zero counts as reversed), and the text counts every restatement
  ("N restatement(s) by IBKR (latest <day>)") and says how many the last
  reading recorded. The text under the table says how many postings were left
  out. IBKR's Cash Transactions normally holds only "Commission
  Adjustments", under IBKR's numeric transaction IDs, so the per-trade
  commissions come from the fills; the matching is **an assumption until the
  first live statement is checked** (reconciliation chapter, "The reader
  writes every commission, and the reporter counts each once").
- **Only this account's folder is read.** As for the cash ledger, a costs
  ledger in another account's folder is not read ("not accepted"), and
  nothing from it is counted.
- **A day after the summary's is never counted**, so an earlier day's summary
  does not change when the ledger grows. A cost, and a fill's commission,
  is dated by its New York day for the month and year, the day the first
  run's approval cut-off is read by, so a fee IBKR posts at 8:30 p.m. New
  York on the last day of a month is that month's, as on IBKR's own
  statement.
- **Costs before the account's start are not counted.** The reader writes
  none dated before the New York day you approved the first run: they belong
  to the account before the book you stated.
- **Only US dollars are added.** A posting in another currency is not added;
  the text says how many.

**What may be missing, and how it is said.** IBKR's report lags: a posting of
day D is in the statement read the morning after it, or the morning after
the next session at the latest, so the
last two sessions' costs may not be there yet (the text says so, and says
"costs recorded through <day>"). Until the Flex reader has written a costs
ledger, the card shows the commissions on fills only and says "fees and
interest not yet recorded by the Flex reader"; the daily summary's costs line
ends with the same words in brackets. The costs are read in their own step,
so they can be set aside while deposits and withdrawals are still recorded;
whenever the costs reading is not "read", or has fallen behind (more than
two sessions after "costs recorded through <day>" and before the summary's
day: the reading of the session before covers to the day before its
statement's last day, so a nightly reader is never behind), the line ends "(costs incomplete: the
Flex reader is set aside as of <day>)", "(… is behind as of <day>)" or
"(… the Flex reader's reading of <day> did not finish)", and the card says the
same: the figures are then partial. A ledger that cannot be read, or that
belongs to another account, is named the same way; the commissions on fills
are still counted. A fill that did not read is counted as an input not read, not
here, and the text says how many commissions are therefore left out.

**A worked example** (made-up numbers). In March 2027 the account is worth
about 10,000.00 all month. On the 3rd it buys, and IBKR charges 1.25 for the
trade: the fill in the log carries 1.25, and the reporter counts it from the
fill (the Flex reader's statement normally has no posting for it, an
assumption until the first live statement is checked). On the 10th IBKR posts a market-data fee of
-10.00, on the 12th interest paid of -3.21 (a short margin loan) and on the
12th interest received of 0.42. The month's column reads commissions 1.25,
fees 10.00, interest paid 3.21, interest received 0.42, net cost 14.04, and
the daily summary's line reads "Costs this month: commissions 1.25, fees
10.00, interest 2.79". If this is the account's first month, the net cost
since the start is 14.04, 0.14% of an average net liquidation of 10,000.00.
Had IBKR also listed that commission in Cash Transactions under the
execution's ID, the matching would leave it out, rather than show 2.50 and
15.29.

This card changes no order and no limit; it reports. It is also why the cash
checks have less to allow for: a fee or an interest posting is now recorded
(next to the deposits and withdrawals), though the daily bound on cash drift
is unchanged (reconciliation chapter, "Deposits and withdrawals, read from
IBKR").

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
- The line "Costs this month: commissions X, fees Y, interest Z" gives the
  month's commissions, fees and interest paid less interest received (the
  dashboard's costs card, above, has the year, the start and the share of
  net liquidation). When the Flex reader's costs are not available yet it says
  so in brackets, and the commissions on fills are still counted.
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
| `<account>: the run's settings file is missing` | urgent | this chapter, below |
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
| `<account>: reconciliation pending-cash-read (<id>)`. Its text begins "<account>: cash differs from the book by <amount> (<direction>) on <day>; the Flex reader has not covered that day yet; the account stays <state>; <buys>; it becomes Degraded if unexplained after the Flex reader covers <day> or more than 2 sessions close since, whichever comes first.", where `<state>` is the state already in force, and the sentence reads "the account is <state>" instead of "stays" when the same reconciliation input also moved the state (it settled an earlier pending difference as Degraded on cash), `<direction>` is "an increase, like a deposit" or "a decrease, like a withdrawal", and `<buys>` is "buys continue, sized without the increase until the reader explains it" or "buys are withheld until the reader explains it, and sells and exits continue". The account's state does not change; the public relay's copy leaves the amount out; a later reading of the same difference sends nothing, its settling sends nothing, and only its turning Degraded sends the reconciliation alert above | urgent, repeated until acknowledged | [reconciliation](reconciliation.md), "A deposit or withdrawal you make" |
| `<account>: venue report <id> not read` | urgent | [order lifecycle](order-lifecycle.md) |
| `<account>: operator clearance <id> not read`. Its text names the clearance's order IDs when its record can be read that far | urgent | [order lifecycle](order-lifecycle.md) |
| `<account>: operator clearance <id> refused`. Its text names the clearance's order IDs | urgent | [order lifecycle](order-lifecycle.md) |
| `<account>: order <id> not acknowledged` | urgent | [order lifecycle](order-lifecycle.md) |
| `<account>: order <id> acknowledged` | routine | [order lifecycle](order-lifecycle.md) |
| `<host>: the server's clock failed its check` (the server's name, not an account's) | urgent | this chapter, below, and [server](server.md), "The clock" |
| `<host>: <unit> failed` (the server's name and the failed unit's, such as `ti-adapter-boost.service`) | urgent | this chapter, below, and [server](server.md), "A failed unit"; the engine's and adapter's chapters have a row for their own unit |
| `<account>: daily summary <date>` | routine | this chapter, "What the summary shows, and its limits" |

**Why an order was refused.** The reason in `order refused (<reason>)`:

| Reason | Explained in |
| --- | --- |
| `kill-switch`, `engine-degraded`, `engine-halted`, `sale-attempted`, `broker-holding`, `sell-exceeds-holding`, `stale-broker-snapshot`, `undecided-order`, `order-count`, `order-size`, `price-band`, `missing-mark`, `unvalued-holding`, `pending-cash-decrease` | [states and kill switch](states-and-kill-switch.md) |
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
| `<account>: order refused (<reason>)` | The risk gate refused one order before anything was sent to IBKR. The reporter sends this urgent alert once per refused order (once per kill-switch engagement, once per stale-snapshot episode, and once per pending cash difference for `pending-cash-decrease`). It is not repeated. The body names the order, its side, size, limit price and the gate's detail | Look the reason up in "Which chapter explains each alert", above; that chapter's table says what to do |
| `<account>: decision withheld (<reason>)` | The engine decided no new targets for that session. Positions and orders already in place are unchanged. One alert per run of the same reason; a decided session ends the run, and the reason coming back later is a new alert. Urgent when an input is missing, no stock is affordable or sector labels are missing; routine otherwise | Look the reason up in the table above; that chapter says what to do |
| `<account>: no engine heartbeat` | The log has not changed while `-heartbeat-sessions` NYSE sessions (default 2) closed. One urgent alert per silence; a later silence is another. Orders already at IBKR are unaffected | `systemctl status ti-engine@<instance>` and `journalctl -u ti-engine@<instance>`. If it crashed, resume it; otherwise start a fresh run you approve ([restart](restart.md); runbook `docs/runbooks/durable-log.md`, "Restarting an engine") |
| `<account>: the history of earlier runs is unavailable` | The chain of runs behind the current log is broken: the body names the two logs and why. Only the equity comparison is affected: every other alert and the daily summary are sent as before, and the summary's equity section reads "history unavailable: the chain of runs is broken between <log A> and <log B>", then gives the comparison over the intact runs, as the dashboard does. Once per account, break and reason: the same link broken for another reason (a restored log that is not the one approved) is another alert, and once a firing finds the chain whole the break is forgotten, so a later break alerts again | Before any restart, restore the named earlier log from a backup of it as the engine wrote it, keeping its modification time and owned by `ti-trader` (`sudo rsync -a --chown=ti-trader:ti-trader`, or `sudo cp -p` then `sudo chown ti-trader:ti-trader`), and check with `ls -l --time-style=full-iso` that it is older than the current log and owned by `ti-trader` ("The history across restarts", above). If it cannot be recovered, the history starts at the first intact run from then on. Never put back a log moved into `quarantine/` |
| `<account>: a restored log looks newer than the current run` | A log the chain of runs names as an earlier run has a newer modification time than the current run, usually a log restored from a backup with the time of the restore. The body reads "<restored log> looks newer than the current run <log>; fix its modification time before the next restart". The running engine is unaffected, but the engine finds the latest run by modification time, so its next start would take the restored log for the latest run. Once per account and pair of logs | Before any restart, restore the log again from its backup with `sudo rsync -a --chown=ti-trader:ti-trader` (or `sudo cp -p` then `sudo chown ti-trader:ti-trader`), or set its time back with `sudo touch -d '<a time before the current log's>' <restored log>`, and check with `ls -l --time-style=full-iso` that it is older than the current log ("The history across restarts", above). If the engine already refused a start saying the latest earlier run is the restored log, do not approve that log: fix its time and start again with the approval naming the current log |
| `<account>: the reporter cannot read the journal` | The log does not verify, mixes runs, is missing, or holds a decision record that fails its checks. No other alert from that log is sent and no summary; the heartbeat is still checked unless the body says "The heartbeat is NOT checked" and why. Once per account and problem; the run exits with the error | Check the file with `sudo -u ti-trader journal-query` ([journal-query](journal-query.md)); never edit it. Runbook `docs/runbooks/durable-log.md`, "A log that cannot be read". Until it reads, nothing in it reaches you: watch the account in IBKR's app |
| `<account>: the reporter cannot check the heartbeat` | The NYSE calendar does not cover the span (it covers 2022 to 2030), so a silent engine would go unreported. Once per problem; the run exits with the error | Install a build whose calendar covers the year. Until then check the engine yourself each trading day |
| `<host>: the server's clock failed its check` | The reporter runs the clock check (`/usr/local/libexec/trend-investing/clock-check.sh`) at every firing, after it has sent the firing's other alerts, so a slow check never holds them back. A clock behind IBKR's makes every engine on the server count the account reading as stale and withhold every sell (ADR 0019). **When it pages:** after two checks in a row fail, one timer period apart, so within 10 minutes of the clock going wrong; a single failing check (a reference briefly unreachable, say) pages nothing, and one passing check starts the count again. A check recorded more than 150 seconds ahead of the time now means the clock was set back, and that counts as a failed check in its own right (the alert then says the clock stepped back by about so long), so a clock that keeps stepping back pages on the second firing that finds it. **Once per server:** the clock is the server's, so every account's reporter shares one record of it, `/var/lib/trend-investing/reports/host-clock.json` (`-clock-state`), under a lock: one check per 5-minute period, one page per fault, whichever account's reporter made the check. The alert gives the time of the check, the skew it measured (this clock minus the reference), or says none was measured (the clock not synchronised, no reference reachable, the check missing, not executable or timed out), or that its output could not be read, and what the check said. It is not repeated; when a later check passes the failure is forgotten, channel by channel (Pushover has nothing to close; a PagerDuty incident, if that channel is named, is resolved first), so drifting again pages again. A channel that works forgets it at once. If PagerDuty is set aside at that moment (a bad routing key), its record is kept and the incident is closed by the first firing in which PagerDuty works again; ntfy, which has nothing to close, is not held back by it. The same holds if PagerDuty's resolve fails (its service down, or its routing key invalid since the trigger): only PagerDuty's record is kept, to retry the resolve, and a new fault still pages ntfy. A routing key changed to another valid key is accepted by PagerDuty, so the resolve succeeds, closes nothing and the record is forgotten at once: the incident opened under the old key stays open, and it must be resolved by hand in PagerDuty. A channel taken out of `-channel` has its record dropped by the next passing check, but the program cannot reach its incident: if PagerDuty was removed, resolve the open `clock-drift:<host>` incident by hand in PagerDuty. Reporters on one host naming different `-channel` lists share this record, so keep the lists the same | Run the check by hand and fix the time syncing, or on the Mac mini turn on the Mac's automatic date and time ([server](server.md), "The clock"). If the alert says the check **is missing**, run `sudo deploy/provision.sh clock`, which installs it. If it says the check **is not executable by this user**, the reporter's user (`ti-trader`) cannot run it: `sudo chmod 0755 /usr/local/libexec/trend-investing/clock-check.sh` (or run `provision.sh clock` again). If it says the check **timed out after 40s**, the check hung (a reference or `timedatectl` did not answer): run it by hand to see which, and check the network and `systemctl status systemd-timesyncd`. If it says the output **could not be read**, read what the check said in the alert; the check's wording and the reporter disagree, so install a matching build. Until the clock is right, expect sales to be withheld as `stale-broker-snapshot` |

The daily summary's last line, "Clock check: ok at <time> (...)" or "Clock check:
FAILED at <time> (...)", is the check the reporter ran in the same firing that
sent the summary, or, when another account's reporter was checking at that
moment, the last check recorded for the server; it appears only when the unit
passes `-clock-check`.

### The reporter itself

| What you see | What the program does | What you do |
| --- | --- | --- |
| Nothing at all for a long time | Possible causes: the timer is not running, the binary is not installed, or every run stops before it sends (the rows below). If the whole server is down, the page comes from Healthchecks instead ([server](server.md), "The outside heartbeat"): your check `trend-investing-reporter` is down, and it names no account | `systemctl list-timers ti-reporter@boost.timer` shows the next firing; `systemctl status ti-reporter@boost` and `journalctl -u ti-reporter@boost` show the last run and its error |
| The journal says "reporter: waiting for the account's first run: .../run.env is missing or empty (start the engine once)", and the unit is not failed | No engine run has been started for the account, so there is no log to report on. The firing sends no alert of its own and, with nothing else wrong, succeeds and pings the outside heartbeat, so your Healthchecks check stays up: the reporter is alive, there is simply nothing to report yet ([server](server.md), "The reporter before the first run"). Nothing is paged | Nothing. Start the engine by `docs/runbooks/durable-log.md`, "Restarting an engine"; the next firing follows its log |
| `<account>: the run's settings file is missing`, urgent, and the unit failed | `run.env` is missing or empty, but the account has run: the reporter's marker `<account>.ran` is in `/var/lib/trend-investing/reports`, or a `.log` file is in the engine's folder, or that folder cannot be listed. It does not know which log to read, so nothing in the log is sent and no summary; it does not check the heartbeat (the alert says why it is unchecked), sends no ping, and the firing fails | Write `run.env` back as `docs/runbooks/durable-log.md`, "Restarting an engine", says; the next firing follows the log. Never delete the marker to silence this: it records that a run happened |
| The unit failed: the journal says the log named by `run.env` cannot be read or is missing | `run.env` exists, so a run was started and its log is expected: the reporter sends an alert for the unreadable journal on the channels it can and the firing fails, with no heartbeat ping. The unit's failure is paged once by the hook (`<host>: ti-reporter@boost.service failed`, below), not at every firing, with a reminder at most once an hour while every firing fails | Find the log `TI_RUN` names in `run.env` and restore it or correct `run.env` (`docs/runbooks/durable-log.md`); the next firing follows it |
| `<account>: state at reporter start: <state>`, urgent, then each open alert with its title beginning "state at reporter start:" | This channel began with the reporter after the log's earlier events: the state file is new or was lost (moved aside, say), or you just named the channel in `-channel`. It is sent no routine history, only what is in force: the page names the engine state and scope, an engaged kill switch, a pending cash read, the margin-cushion alarm and every open alert, and any failed trading unit (named with its manual row), and those alerts repeat until acknowledged. Once per channel | Read the page and act on each open alert as you would on its first page; acknowledge each with `operator acknowledge` and the key it gives ([operator](operator.md), "Acknowledging an alert"). Check the account on the dashboard or with `journal-query` |
| `<host>: <unit> failed`, urgent, once per failure: the unit is `ti-engine@boost.service`, `ti-adapter-boost.service`, `ti-reporter@boost.service` or `ti-data.service` (the data job, which has no reminders and never holds back the heartbeat; its page ends when a later run ends well, [server](server.md), "A failed unit"). The text names the unit, the failed run's own last journal line as the reason (or, for a run that wrote nothing, systemd's line about that run) and the manual row to read | A unit failed. For the engine, the adapter and the reporter, systemd does not restart it. For the data job, its timer runs it again at the next firing. The `ti-unit-failed@<unit>.service` unit started by the unit's `OnFailure=` sent this page to Pushover and your own ntfy server within a minute; it carries no balance, account number or secret. The page is keyed by the run that began the failure (systemd's invocation ID). Every run that fails before the unit is seen well again (a unit restarted that fails again, a reporter failing at each firing) is the same failure: no second page and no resolve-and-trigger of its incident, only (engine, adapter and reporter; never the data job) `<host>: <unit> still failing`, at most once an hour for as long as the unit stays failed, saying since when and, once two or more runs are known to have failed, how many. The reporter's every firing asks systemd for the engine's and the adapter's state and run at its start too (the data job's only to page it once if the hook could not, and to end its incident when it is inactive): it pages a failure the hook missed, and forgets the page of a unit no longer failed (resolving its PagerDuty incident first, if that channel is named); a failure after that pages again. A reporter's own failure is resolved by its next clean firing. While the engine or the adapter is failed the reporter sends no outside heartbeat ping ([server](server.md), "A failed unit") | Read the manual row the page names (the engine's or the adapter's chapter; for the data job, [server](server.md), "The data job fails"), `journalctl -u <unit>` for the whole reason, then start the engine or the adapter as that row says. Nothing restarts them for you; the data job's timer runs it again |
| `<host>: <unit> still failing`, urgent, at most once an hour: "still failing since <time>", plus ", N failed runs" only when two or more runs are known | The unit paged as `<host>: <unit> failed` is still failed and was never seen well between: a unit that failed once and stays failed (nothing restarts it) gets this reminder every hour with no count, and one that has failed again, run after run, gets it with the count of its failed runs. This is a reminder of the same failure, not a new page. It carries no journal line. It stops when the unit is seen well (a firing finds the engine or the adapter not failed, or the reporter finishes a clean firing), which resolves the incident | Act on the first page's manual row; for the reporter, the row of its error (`journalctl -u ti-reporter@boost`), such as starting the engine once or fixing a channel |
| A trading unit failed and no page about it came | The hook could not reach a channel (`systemctl status ti-unit-failed@<unit>` shows it failed), or the unit failed before the hook was installed. The next reporter firing pages it (within 5 minutes), and the state-at-start page names every failed trading unit | Wait for the next firing; if nothing comes, `systemctl --failed` on the server shows the unit, and the reporter's own error (`journalctl -u ti-reporter@boost`) says why it could not page |
| `<account>: the reporter cannot deliver to <channel>`, urgent, on the channels that still work | One channel's secret is empty or malformed, or its name in `-channel` is unknown, so that channel is set aside; every alert still goes to the others, the daily summary ends with "Channels not delivering:" naming it, and the run ends with an error naming it (the rows below). Once per channel and problem; nothing reaches the channel set aside until it is fixed | Fix that channel's line in `secrets.env` (the rows below say which), or the unit's `-channel`; the next firing delivers to it everything it missed |
| "reporter: the ntfy channel needs TI_NTFY_TOPIC ..." "reporter: the pushover channel needs TI_PUSHOVER_APP_TOKEN ..." or "... needs TI_PUSHOVER_USER_KEY ...", or "reporter: the pagerduty channel needs TI_PAGERDUTY_ROUTING_KEY ..." | That channel is set aside and the other still delivers (the row above). With no channel left that reaches you, the whole run is refused before anything is sent | Fill the line in with `sudo ti-secrets edit secrets.env`; the next firing uses it |
| "reporter: TI_NTFY_SERVER must be an absolute URL such as https://ntfy.example.org" "reporter: TI_PUSHOVER_URL must be an https URL such as https://api.pushover.net/1/messages.json (http is allowed only for a loopback host)" or "reporter: TI_PAGERDUTY_URL must be an absolute URL such as https://events.pagerduty.com/v2/enqueue" | That channel is set aside and the other still delivers; with none left, the run is refused before anything is sent | Correct the line, or leave `TI_PUSHOVER_URL` (or `TI_PAGERDUTY_URL`) empty for the service's own endpoint; an empty `TI_NTFY_SERVER` is the public ntfy.sh relay, which takes incident alerts but not the daily summary |
| "reporter: the secrets file cannot be read; check -secrets-file names the credential file", or "reporter: the secrets file's line N is not KEY=VALUE" | Reported in the run's error, which never shows a secret's text. Every line that reads is still used, so only a channel whose secret that leaves unset is set aside; with no channel left, nothing is sent | Correct that line of `secrets.env` (`KEY=VALUE`, no spaces round `=`) with `sudo ti-secrets edit secrets.env` |
| "reporter: the github-issues channel needs TI_GITHUB_ISSUES_TOKEN ..." or "... needs TI_GITHUB_ISSUES_REPO as owner/name ...", or "reporter: TI_GITHUB_API_URL must be https://api.github.com, or an https address whose host TI_GITHUB_API_ALLOW_HOST names ..." | That channel is set aside and the others still deliver (the rows above); no issue is opened | Fill in the name it gives with `sudo ti-secrets edit secrets.env` ("Alerts as GitHub Issues", the token steps) |
| "github-issues: GitHub answered 401 Unauthorized" (or 403 Forbidden, 404 Not Found, 5xx, or "rate limited"), or "github-issues: request failed" | That channel failed, so the alert is not delivered to it, and Pushover and ntfy are not held back. It is sent again at the next firing and its issue is made then; a rate limit is the same, and a firing that comes before GitHub lifts the limit fails the same way | 401: the token expired or was revoked, make a new one. 403 or 404: the token lacks Issues read and write on that one repository, or `TI_GITHUB_ISSUES_REPO` is misspelt. Otherwise wait for the next firing |
| "github-issues: GitHub's answer is larger than N bytes" | That one delivery failed and is sent again at the next firing; the firing's other alerts are still tried. The answer was larger than any page GitHub can send, so comments on the issue cannot be the cause on GitHub itself. Either something other than GitHub is answering (a wrong address, or a proxy in between), or GitHub has changed its limits. If the listing of alert issues is the answer that is too large, GitHub's deliveries are not tried for the rest of that firing | Check that `TI_GITHUB_API_URL` is unset or points at GitHub's API (the token is sent to that address), and that no proxy sits in front of it. Do not delete comments: that is not the cause |
| "github-issues: not tried: GitHub failed earlier in this firing (...)" or "github-issues: not tried: this firing's time for GitHub (the send timeout) has run out" | GitHub could not list the alert issues, ran out of `-send-timeout`, limited the rate, answered 5xx or could not be reached, earlier in the same firing, so the firing's later alerts were not sent to it. Pushover and ntfy had them first. All are sent again at the next firing | The reason in the brackets is the first failure; act on it as in the row above |
| "github-issues: more than 1000 issues carry the alert label, so the list of alert issues is incomplete and none is written ..." | The channel fails every firing and writes nothing, since an issue it could not see would be opened twice. Pushover and ntfy still get every alert, but each such firing fails the reporter's unit (its failure page) and sends no outside heartbeat ping. The reporter takes the `alert` label off every issue it resolves (and adds `alert-resolved`), so what fills the list is issues you closed by hand, and single-event alerts you have read | Remove the `alert` label only from closed alert issues whose incident is over, such as a single-event alert you have read (filter `label:alert is:closed`, select, Label), until fewer than 1,000 carry it. Leave the label on an issue you closed by hand while its alert still repeats (not yet acknowledged): without it the reporter cannot see that issue |
| "github-issues: issue has more than 1000 comments, so whether this delivery was made cannot be told and nothing is written" | That issue takes no more comments, so its repeats and reminders fail on GitHub every firing; Pushover and ntfy still get them, but each such firing fails the reporter's unit (its failure page) and sends no outside heartbeat ping | Acknowledge the alert (`operator acknowledge`) so it stops repeating; its issue then closes, with no closing comment and with its `alert` label taken off, and the alert's next incident opens a new issue that does not name it (find the closed one by the label `alert-resolved`) |
| "github-issues: resolve: ... (given up after 3 firings: the next incident opens a fresh issue)" | GitHub refused, three firings running, to close the issue of a broken history made whole or a restored log cleared. The reporter stops trying, so the fault's next incident is not held back: it opens a new issue that names the old one | Close the issue it names by hand |
| "github-issues: GitHub's listing named an issue without its number or its state" | GitHub's answer was not one the reporter can trust, so nothing is written to GitHub that firing | Usually clears at the next firing; if it persists, check `TI_GITHUB_API_URL` points at GitHub's API |
| "github-issues: GitHub did not name the token's user", or "github-issues: GitHub named a comment without its author, so whether this delivery was made cannot be told and nothing is written" | The reporter cannot tell its own comments from others', so a delivery that must check them (a repeat, a reminder, a close, or a first delivery whose earlier issue is closed) writes nothing; the token's user is asked once a firing, so every such delivery that firing fails at once. A first delivery with no earlier issue still opens its issue | Usually clears at the next firing; if it persists, check `TI_GITHUB_API_URL` points at GitHub's API |
| "ntfy: server answered 403 Forbidden" (or another status), "pushover: server answered 400 Bad Request", "pushover: the answer did not acknowledge the message", "pagerduty: server answered 400 Bad Request", "pagerduty: the answer did not acknowledge the event", or "post failed" | That channel failed, so the alert is not delivered, even if the other channel took it. Only the failed channel is retried, at every firing, until it takes the alert | A wrong topic, server, token or user key: correct `TI_NTFY_TOPIC`, `TI_NTFY_SERVER`, `TI_PUSHOVER_APP_TOKEN`, `TI_PUSHOVER_USER_KEY` or `TI_PAGERDUTY_ROUTING_KEY` in `secrets.env`. A network fault clears by itself |
| Pushover took the message, but your phone shows nothing | Both channels accepted the alert, so the reporter counts it delivered. What your phone does with it is up to the Pushover and ntfy apps | Check the Pushover app's notification settings (and iOS's, including Critical Alerts, [server](server.md), "The pager"), and that the ntfy app is subscribed to exactly the topic (and server) in `secrets.env` |
| "reporter: the state file is unreadable and is not reset, since a new file forgets which alerts are open: ..." | The run stops before anything is sent, every firing | Move the file aside: `sudo -u ti-trader mv /var/lib/trend-investing/reports/boost.reporter.json /var/lib/trend-investing/reports/boost.reporter.json.bad`. The next firing starts a new state file, which sends none of the log's routine history but pages, once per channel, the state at reporter start and every open alert (see "The delivery rules"); the open alerts then repeat until acknowledged |
| "reporter: write state: ..." | Alerts may have gone, but were not recorded, so the next firing sends them again | Free disk space, or fix the folder's owner (`ti-trader`) |
| "<account>: the daily summary has no channel: name your own ntfy server" on your channels, and "reporter: Incident alerts are unaffected, but the daily summary is not sent ..." in the unit's output | Every alert still goes. The summary does not: no ntfy server of your own takes it (ntfy is not in `-channel`, or `TI_NTFY_SERVER` is empty or the public ntfy.sh relay, or the ntfy channel was set aside). Said once. The run does not fail for this, but a channel set aside fails the run naming it, as in the rows above. Pushover never takes the summary | Set up your own ntfy server ([server](server.md), "Your own ntfy server"), put its URL in `TI_NTFY_SERVER` and keep `ntfy` in the unit's `-channel`. Until then read the summary on the dashboard |
| The daily summary stops arriving on your phone while incident pages still do | The ntfy channel failed for the summary (your server stopped, the phone left the tailnet, or Tailscale is down on the server) and the run ended with an error naming `ntfy:`. The summary is tried again at every firing | `systemctl status ti-ntfy` on the server; the phone must be signed in to Tailscale. Incident pages are not affected: they go to Pushover as well |
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
| The equity card or the summary says "deposits or withdrawals from the last two trading sessions may not be recorded yet; a transfer appears in the summary two trading sessions after it posts (a Friday transfer on Tuesday's)" (or "how far the Flex reader has recorded is not known ...") | The Flex reader has not yet read the summary's day: it runs each weekday morning (5:30 New York, again at 6:30 and 7:30), and IBKR may post a day's transfer only after its own end-of-day processing. A transfer from the last two trading sessions is not in the comparison yet; it is in the summary two trading sessions after it posts (a Friday transfer on Tuesday's) | Nothing, if a later line says the day is recorded. If "not known" stays, check the reader's status file ([reconciliation](reconciliation.md), "What can go wrong") |
| The equity card or the summary says "the recorded withdrawals reached or exceeded the SPY shadow's value on <date>; dollar figures are not meaningful after that day" (or "... dollar figures restart from <date>") | Your recorded withdrawals took the SPY shadow account to zero that day, so its dollar figures (the gap and the dollar line) mean nothing until your next deposit restarts it; the verdict, which is time-weighted, stays | Nothing to fix: it is a true statement of the record. If the withdrawal was not yours, check the ledger's transfers against IBKR's activity ([reconciliation](reconciliation.md)) |
| It will not start: "dashboard: -cash-ledger ... must be SOURCE=PATH ...", "... names ..., which is no -journal or -engine-dir", "... is given twice for ..." or "... names no ledger file" | It refuses to guess which account a ledger belongs to; a `-journal`'s ledger is always named | Name each ledger as `SOURCE=PATH` with SOURCE exactly the `-engine-dir` or `-journal` it belongs to ("Running the dashboard", above) |
| It will not start: "dashboard: ... is neither loopback nor a Tailscale address ..." or "... is not an address of this host's Tailscale interface" | It refuses to listen anywhere but loopback or this machine's own Tailscale address | Use `-addr 127.0.0.1:8088` (the unit's) and publish it with `sudo tailscale serve --bg 8088` |

### The triage job

`/Users/ti-triage/triage/triage.log` on the Mac mini shows every poll's
error and the job's notices; a quiet log proves neither that the job runs
nor that nothing was analysed ("Its log", above).

| What you see | What the program does | What you do |
| --- | --- | --- |
| No triage comment appears on a new alert issue | Possible causes: the job is off (the `OFF` file, or launchd's job is not loaded), the Mac mini is asleep or off the tailnet, the token expired, or the issue is not the reporter's (another author, or no hidden key line), or it was opened before the job was first switched on. The alert itself is unaffected: Pushover and ntfy have it | Read the log. `sudo launchctl print system/com.trendinvesting.triage` shows whether launchd runs it. Check `-alert-author` names the reporter's GitHub user if the two tokens belong to different users |
| "triage: off (/Users/ti-triage/triage/OFF exists); nothing read, nothing written" in the log | Every poll does nothing while the file is there | Delete the file to switch the job back on |
| "triage skipped: daily limit" on an issue | The day's `-daily-cap` sessions were made, so this issue was not analysed, and never will be | Read the alert and its journal yourself. Raise `-daily-cap` in the plist only if the cost is worth it |
| "triage failed: ..." on an issue (the analysis failed, ran past its time, named no known kind, or did not finish) | That issue's one session is spent; it is not tried again | Read the alert and its journal yourself, and the log for the reason |
| "triage: issue #N: no session was started, and the issue is tried again at the next poll: gate unavailable: ..." in the log | No session was started and nothing was spent: the gate's user `ti-triage-gate` (a user of its own, not `ti-triage`, not root), `/usr/bin/sandbox-exec`, the `sudo` rule (both its lines: `sudo -l` must allow the `pkill` line exactly), `netstat`, or the gate's folder (`-gate-dir`, the job's own, mode 750) is missing or wrong, and the gate never runs the session's code without them | Set the gate up again (runbook, "2. The alert triage job", steps 2 and 3); the issue is analysed at the first poll after |
| "triage: issue #N: the repository could not be prepared ..." in the log | The fetch of `main` or the module downloads failed: no session was started and nothing was spent | Check the Mac mini's network; the next poll tries again |
| "triage: issue #N: gate unavailable: ..." in the log after a session | The gate could not run a fix's red or green check, or serve the session's own commands (its run folder or the job's socket failing), or end the gate user's processes after one: the issue is analysed again at the next poll, once; a second time its comment says "No fix PR was opened: gate unavailable" | As for the gate's row above |
| "**Journal slice.** The journal slice did not cover the alert's run and time" in a comment | The run that raised the alert, the alert's own account's, does not span it in the slice (the run was replaced since, the dashboard serves another account, or the alert is one no journal raises, such as a failed unit), so the session was told and worked from the alert alone | Read that run's journal yourself with `journal-query` on the server |
| "**Fix.** No fix PR was opened: ..." in a comment | The session found a code defect, but its change failed a step of the gate: a file outside the Go code and `docs/`, a binary, no test, only tests, no new test function, the new test did not fail by name before the fix, the check failed, it held an identifier, or the push or the PR failed. Nothing was opened; if the push worked, the branch `triage/alert-<issue>` is on GitHub | Treat it as a defect report: fix it the normal way, or delete the branch |
| "triage: issue #N: the job was stopped during the session" in the log | The job was stopped (a SIGTERM, or `launchctl bootout`) while Claude worked: no comment, and the next poll analyses that issue again, once | Nothing; if it happens twice the issue gets "triage failed: the analysis did not finish" |
| "triage: list the alert issues: more than 1000 issues carry the label ... and are open and opened since the job began" (or "... and are closed in the last 30 days") in the log | More than 1,000 issues of one label in one set, open since the job was first switched on or closed in the last 30 days: no poll analyses anything, since the job does not act on part of a listing. Issues closed longer ago do not count towards the 1,000, but the listing as a whole also fails past 100 pages of 100 | Something may be raising alerts in a loop, so look at the reporter first. Then move `/Users/ti-triage/triage/state.json` aside: the next poll starts a new record from that moment, issues already analysed are recognised by their comments, and older ones are left for you |
| A draft PR from `triage/alert-<issue>` | The job's proposed fix, tests first, never merged by the job | Review it; mark it ready to start the review bots and the merge gate, or close it |
| "triage: the state file is not understood, so nothing is done (...)" or "the state file cannot be read" | No poll runs: the record does not parse, or lacks a field or breaks a rule (no start, a day that is not a date, no count of calls or one below zero, no issues' records), and without it the job cannot tell which issues it already made a session for | Move `/Users/ti-triage/triage/state.json` aside. The next poll starts a new record: issues already analysed are recognised by their comments; one opened before that poll is left alone |
| "triage: issue #N: GitHub answered 401 Unauthorized" (or 403, 404) | The poll writes nothing for that issue; the next poll tries again | The triage token expired, was revoked, or lacks a permission (Issues on the alerts repository; Contents and Pull requests on the code repository): make a new one (runbook, step 5) |
| "triage: another poll is running; this one does nothing" | A poll started by hand while launchd's ran, or the other way round | Nothing |
| "triage: -token-file must be a file only you can read (chmod 600)" | The job does not start | `chmod 600` the file, as `ti-triage` |
| A comment saying "The journal slice could not be fetched" | The dashboard could not be reached over the tailnet, so the analysis used the alert issue alone | Check the dashboard (`systemctl status ti-dashboard`) and that the Mac mini is on the tailnet |

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
  `TestTheDailySummaryNeverGoesThroughThePublicRelayOrToPagerDuty`,
  `TestTheSecretsFileSuppliesTheChannelSecrets`,
  `TestOneChannelsBadSecretNeverSilencesTheOther`,
  `TestPushoverPostsTheDocumentedFormFieldsAndPagesAtEmergencyPriority`,
  `TestPushoverAnswerThatIsNotAcceptedIsNotADeliveryAndLeaksNothing`,
  `TestAMalformedSecretsLineSetsAsideOnlyWhatItLeavesUnset`,
  `TestTheServiceFileRunsTheReporterOnTheEnginesLog`,
  `TestTheDailySummaryGivesTheVerdictWithTheFlexLedgersTransfersMirrored`,
  `TestAMultipartNtfyMessageHasOneDeadlineForAllItsParts`,
  `TestRestoringTheSummaryRouteResolvesTheNoticesIncident`,
  `TestTheNoChannelNoticeIsKeyedByTheAccountAlone`,
  `TestTheDailySummaryGivesThisMonthsCostsFromTheLedgerBesideTheCashLedger`,
  `TestAReporterThatFailsEveryFiringPagesOnceAndRemindsHourly`,
  `TestAFailureAfterTheUnitWasSeenWellPagesAgain`,
  `TestAFiringThatFindsATradingUnitFailedSendsNoPing` and
  `TestTheTimerMakesTheRepeatAndAcknowledgementBoundsTrue`; in
  `internal/report`, `TestTheStateAtReporterStartNamesBoostsStoppedBuys`,
  `TestTheSecondRejectionStopsBuysAtOnceInTheStanding` and
  `TestAnUnreadableDecisionDoesNotSilenceTheStandingAndIsNamedAsPossiblyStopping`.
- Tests in `cmd/dashboard`, among them `TestEveryRouteRefusesEveryWritingMethod`,
  `TestDashboardShowsTheDaysSkippedTrades`,
  `TestDashboardRefusesAJournalThatDoesNotVerify`,
  `TestListenAddressMustBeLoopbackOrThisHostsTailscaleAddress`,
  `TestTheEquityCardOpensWithTheVerdictInWordsAndColour`,
  `TestTheChartDrawsGrowthSoADepositDoesNotBendIt`,
  `TestTheDashboardReadsTheFlexLedgerForEachEngineDirectory`,
  `TestTheCashLedgerFlagNamesTheAccountItBelongsTo`,
  `TestTheCostsCardSitsUnderTheEquityComparisonWithEveryPeriodAndKind`,
  `TestTheDashboardTotalsTheCostsLedgerBesideEachAccountsCashLedger` and
  `TestTheServiceFileServesTheBoostLogOnTheOnePort`.
- Tests in `cmd/triage`, among them
  `TestAFakeAlertIssueGetsOneAnalysisCommentAndASecondPollAddsNothing`,
  `TestAPollWithNothingNewSpendsNoModelUsage`,
  `TestTheDailyCapSkipsWithOneCommentAndNoModelCall`,
  `TestAResolvedIssueNeverAnalysedIsAnalysed`, `TestTheOffFileStopsTheJob`,
  `TestAnInvocationIsRecordedBeforeTheModelIsCalled`,
  `TestTheStateIsSyncedBeforeTheModelIsCalled`,
  `TestAnInterruptedSessionLeavesTheIssueEligibleOnce`,
  `TestTheDailyCapCountsTheDayOfEachCall`,
  `TestAnIncompleteOrInvalidStateFileStopsTheJob`,
  `TestHandledIssuesAreNeverReadAgain`,
  `TestAClosedAlertsRecordIsDroppedThirtyDaysAfterItWasLastListed`,
  `TestARecordWhoseIssueDisappearsFromListingsIsBounded`,
  `TestAClosedIssueHandledJustBeforeDay30KeepsItsRecordUntilUnlistedFor30Days`,
  `TestAnAlertShiftedPastAPageByARelabelIsAnalysedAtTheNextPoll`,
  `TestFieldsThatGrowUnderRedactionStillFitOneComment`,
  `TestADefectGoesThroughTheGateToADraftPullRequestOnGitHub`,
  `TestAlertsAreListedFromOneRepositoryAndThePROpenedInAnother`,
  `TestTheGateCommitsTheTestFirstRedThenTheFixGreenAndPushesTheBranch`,
  `TestTheRedCheckMustShowTheNewTestFailingByName`,
  `TestTheGateCommandRunsAsItsOwnUserInASandbox`,
  `TestTheSandboxDeniesTheNetwork`, `TestAnUnavailableGateRefuses`,
  `TestALinkTheGatePlantsNeverRedirectsTheJobsWrites`,
  `TestTheGatesCopyIgnoresTheSessionsExportRules`,
  `TestAGateFailureDuringTheSessionsCommandsIsTheGates`,
  `TestAnAlertOpenedBetweenTheTwoListingsIsInTheNext`,
  `TestOnlyGoCodeAndDocsAreOpenedAsAFix`,
  `TestThePrePushCheckReadsFileNamesAndRefusesBinaries`,
  `TestAnOwnerActionIsStatedAndNeverDone`,
  `TestNoSecretAccountIDOrFigureReachesACommentOrPR`,
  `TestClaudeIsHandedTheSliceAndTheIssueWithEveryFigureRemoved`,
  `TestASliceThatDoesNotCoverTheAlertIsSaidSo`,
  `TestTheSliceIsAPlainGETWithNoCredential`,
  `TestTheClaudeSessionIsHeadlessWithItsToolsRestricted` and
  `TestTheSessionRunsInClaudeCodesSandboxWithExactCommands`; in
  `cmd/dashboard`, `TestTheJournalSliceServesTheJournalQueryReportsOfEachCurrentRun`,
  `TestTheJournalSliceCarriesNoFigure`,
  `TestTheJournalSliceTakesAWindowAndSaysWhatEachRunCovers`
  and `TestEveryRouteRefusesEveryWritingMethod`, which covers the slice.
- Tests in `internal/report` for the costs block:
  `TestTotalsPerKindForTheMonthTheYearAndSinceTheStart`,
  `TestACommissionAlreadyOnAFillIsNotCountedTwice`,
  `TestEarlierRunsFillsCountSinceTheStart` and
  `TestCostsAsAShareOfAverageNetLiquidation`.
- `deploy/rehearse.sh`, which installs the programs in a clean Ubuntu
  container and runs the reporter with its unit's own command line.
