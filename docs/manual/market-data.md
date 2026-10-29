# Market data: fetch, record, adjust, encode

## What it does and why it exists

The engines never talk to a data vendor. A separate step, `adapter/marketdata`
(Python, standard library only), fetches end-of-day prices from Massive once,
writes each answer to a file that is never changed afterwards, and later turns
those files into the input stream an engine reads (ADR 0026, section 8). A
replay reads the same files, so it sees exactly what the live run saw and
never touches the network.

There are four stages:

1. **Fetch**: ask Massive (and, for sectors, iShares and Nasdaq) for one
   session's figures.
2. **Record**: write the answer, untouched, to one file per kind and session.
3. **Adjust**: work out split- and dividend-adjusted prices from the recorded
   raw prices and the recorded splits and dividends.
4. **Encode**: write the engines' input envelopes (one JSON line each) from
   the recordings alone.

Ranking the universe is a Go rule; nothing here ranks.

## Running it

Run from `adapter/`. Every command takes `--out DIR`, the recordings folder.

| Command | What it records or writes | Needs the Massive key |
| --- | --- | --- |
| `python3 -m marketdata universe --out DIR` | the last session's bars for every US stock, plus the list of common-stock (type CS) tickers | yes |
| `python3 -m marketdata bars --out DIR --instruments SPY,IEF,SHY` | the named instruments' bars | yes |
| `python3 -m marketdata backfill --out DIR --sessions 260` | one whole-market bar file per past session (about 54 minutes at Massive's 5 requests a minute; resumable) | yes |
| `python3 -m marketdata actions --out DIR --lookback-days 400` | every split and every dividend dated in the window | yes |
| `python3 -m marketdata sectors --out DIR` | iShares' IWV holdings file and Nasdaq's stock screener, for sector labels, for the latest settled session only | no |
| `python3 -m marketdata encode --out DIR --first YYYY-MM-DD --asof YYYY-MM-DD --configuration-hash H --strategy-version V --run-id R --stream FILE ...` (all of these but `--asof` are required: the engine's configuration hash, strategy version and run identity come from its durable log; `--run-id` takes the run's name, the value the data job stamps on every envelope as its correlation id: the `TI_RUN` of the run that began the chain, which is its durable log's file name without `.log`, for a paper run and a dry-run first run alike. It is not the run's identity, the Run approval's ID or, for a dry-run first run, the configuration's hash and as-of time, which the job uses only to find the run's folder) | the engines' input stream (below) | no, and it never uses the network |
| `python3 -m marketdata retract --out DIR --session YYYY-MM-DD --reason TEXT` | that the exchange cancelled a session (below) | no |

If your recordings hold an older 253-session backfill, extend it to 260
before a fresh run's first decision: `backfill --sessions 260` resumes and
records only the missing days. A stock you hold with a missing day is scored
across it when it has at least 253 usable closes within the last 260
sessions; with fewer (for example, a 253-session backfill with one missing
day leaves 252), it reads `short-history` and is sold. 260 sessions leave
the seven spare days the rule allows.

`--asof` defaults to today's date in New York, not the computer's own time
zone. The session is the latest NYSE trading day on or before it, taken from
the committed NYSE calendar (`nyse_calendar.py`).

The key comes from `MASSIVE_API_KEY` in the environment, else
`--secrets-file` (on the server: mode 600, outside the repository), else the
Mac Keychain item `MASSIVE_API_KEY`. It is sent only as a request header and
only to `https://api.massive.com`. It is never printed, logged or written.

## Recording

Each answer becomes `DIR/<kind>_<session>.json` (splits and dividends also
carry the start of their window in the name). The file holds the raw rows as
Massive sent them, the source, the time they were fetched and a SHA-256
fingerprint of the content. A file is never overwritten: asking for a
session already on disk is refused, and the backfill skips it. A file is
written in full or not at all, and the folder is flushed to disk after it,
so a power cut cannot leave half a file under the final name (ADR 0018).

## Adjusting

Massive's whole-market bars are raw: a 2-for-1 split halves the price
overnight and a dividend drops it by the cash paid. A strategy reading raw
prices would see those as losses. Two adjusted views fix that, both computed
from the recorded raw bars and the recorded splits and dividends, never from
a vendor's own adjusted series:

- **Split-adjusted**: prices and volumes restated as if every split had
  already happened (or had not yet).
- **Split- and dividend-adjusted**: also corrected for cash paid out, so a
  dividend is not a false drop. Boost's RSI(2) and momentum's scores,
  volatility and trend read this view. Orders are still priced from the raw
  close. The daily summary and the dashboard also read SPY's in this view,
  for the SPY side of the equity comparison only: its day-to-day ratios are
  SPY held with its dividends reinvested (alerts and dashboard chapter, "How
  the comparison with SPY works"). Your own account's figures never come
  from it.

The rule (stated in `adjust.py`) is the one QuantConnect uses: a split
multiplies by its ratio, and the cash dividends sharing one ex-date are one
correction, (P minus their total cash) / P, where P is the raw close of the
session before the ex-date. An ordinary and a special dividend paid on one
date are therefore added together, not applied one after the other.

There are two forms of the same rule:

- `adjust.adjusted_closes` works backward from the last recorded session, as
  QuantConnect does. It is for research and the history check.
- `adjust.forward_factors` works forward from a fixed **base** session. On
  the base the adjusted price equals the raw price; each later split or
  dividend scales the later bars. A bar's adjusted figures depend only on
  events up to its own session, so a bar already handed to an engine is
  never restated by a later dividend. The encoder uses this form.

Both forms give the same day-to-day returns; their price levels differ by a
constant per stock, so momentum and volatility agree.

Splits and dividends are read through one reader, `history.load_actions`.
Recorded windows overlap, so an action that appears in several is counted
once (by Massive's id). The recorded windows must together cover every day
asked for.

## Encoding

`encode` reads only the recordings and writes a new file (never overwriting
one) of envelopes, one per line. The file is written in full or not at all:
it is built under a temporary name in the same folder, flushed to disk, then
given its final name, so a failed or interrupted run leaves nothing to block
the retry. For momentum (the default, `--engine momentum`) the order is:

1. one `market.session.calendar`: every session from the first to one past
   the last, from the NYSE calendar;
2. for each session, oldest first:
   - a `market.bar.completed` version 2 for each instrument asked for that
     traded that session: the raw view, the split-adjusted view and the
     split- and dividend-adjusted view, with the base it was adjusted from;
   - a `market.universe.snapshot` where a universe was recorded: one row per
     common stock with a bar that session, with its raw close and volume and
     its sector label and the label's source;
   - a `market.session.closed` naming exactly the bars written.

Boost reads only its one instrument's bars and the session closes, and
refuses any other input, so `--engine boost` writes just those: no calendar
and no universe snapshot, even where a universe was recorded. It needs
exactly one `--instruments` entry (for example `SPY`) and no `--optional` or
`--universe-members`. The account snapshots and the mark Boost also reads are
the IBKR adapter's, made from one account reading as it delivers the file;
the mark's price is this step's bar close ("What happens to the file",
below).

Every bar's time is the session's close: 4 p.m. New York, or 1 p.m. on an
early close (the day after Thanksgiving, and July 3 and December 24 when they
trade). Running `encode` twice over the same recordings writes the same bytes.

Keep `--base` the same for the life of an engine's journal. The engines refuse
an instrument's bar on a different base from its earlier bars, so changing the
base needs a new run.

`--instruments` names instruments that must have a bar every session;
`--optional` names ones written only where they traded. `--universe-members`
also writes a bar for every common stock in the universe, about 5,000 a
session. That stream is correct, but large: a year of warm-up is about 1.3
million envelopes. The compact stream below is the shape the owner chose
(2026-10-16) and the one to run the IRAs on.

The momentum configuration must declare which shape its stream is, in
`momentum.universe_input`: `"bars"` for a stream written with
`--universe-members`, `"compact"` for one written with `--ranker`. The
engine refuses a configuration without it and never infers the shape from
the inputs. `encode` with neither `--ranker` nor `--universe-members`
writes a bars stream with bars only for the instruments named with
`--instruments` and `--optional`, so most of the 500 ranked stocks have no
bar: a momentum engine configured `"bars"` withholds each month's decision
from it as `missing-bars` (every ranked stock needs its bar, even when the
stocks you hold rank), and one configured `"compact"` as
`histories-missing`, so it is not a stream to run an IRA on ([the momentum
universe](momentum-universe.md)).

### The compact stream (`--ranker`)

With `--ranker`, `encode` writes the compact universe input:

- bars only for the instruments you name (`--instruments`, `--optional`):
  SPY, IEF, SHY and anything you hold;
- the one universe snapshot per session, as above: every common stock's raw
  close and volume and its sector label;
- at the last session of each month (a decision session), right after that
  snapshot, one `market.close.history` for each of the 500 stocks the engine
  will rank, carrying that stock's split- and dividend-adjusted closes for
  the last 260 sessions (the engine keeps 260 so a stock you hold can be
  scored across a missing day; [the momentum universe](momentum-universe.md)).
  They are exactly the closes its daily bars would
  have carried, and a session a stock is left out of for bad data (see
  below) is missing from its history, just as its bar would be.

So a month costs about 21 small sets of bars, 21 universe snapshots and 500
histories, instead of about 105,000 bars. A new journal's warm-up is only the
named instruments' bars (SPY's 200-session trend among them; start `--first`
at the base for that), not every stock's: the first month's histories carry
the stocks' 260 sessions.

Which 500 is the ranking rule, and that lives in Go, so this step never
works it out itself. `--ranker` names the command that does:

```
python3 -m marketdata encode ... --ranker "bin/universe-rank -config momentum.json"
```

`universe-rank` (`go build -o bin/universe-rank ./cmd/universe-rank`) reads
the IRA's own configuration file, which must say `"universe_input":
"compact"` (it refuses `"bars"`), and answers, for each universe snapshot,
whether its session is a decision session, which stocks the engine will rank
from it, and how many sessions of closes each history must cover. It uses
the engine's own code for all three. `encode` starts it once per universe
session, with that one session's snapshot (about 500 starts for a two-year
warm-up, a few seconds in all; one start for a daily firing), and stops one
that has not answered within 60 seconds. The engine then ranks the snapshot again
itself and refuses the stream if the histories name any other set of stocks,
so a mistaken or stale ranker cannot slip a different universe in.

In the compact stream the calendar starts at `--base` rather than `--first`,
because the histories reach back to the base and the engine refuses a close
on a day its calendar does not list. A decision session with no universe
recording gets no histories, and the engine withholds that month's decision
(`missing-universe`) and alerts, rather than decide from part of the market.

**Memory the compact stream needs.** The IRA's rule ranks the whole market
every session, so unlike Boost this step cannot simply ignore the other
stocks. It never needs all of them at once, though: it lists the recordings
once, then reads them one session at a time, three times over. The first
read checks every recording and notes, per stock, only the few figures that
decide which sessions a stock is left out of for bad data (its bad days, its
first close, and its close on the session before each of its dividends). The
second builds each session's snapshot and asks the ranker about that one
session. The third writes the stream as it goes, never holding the whole
file, and keeps only the closes of the stocks ranked at a decision whose
260-session window covers that day, 8 bytes a close, dropping them once the
last decision that needs them is written. The stream is byte for byte the
one the step wrote before it read this way (a recorded fixture in the tests
pins it).

Every read uses the listing taken at the start, bar and sector recordings
alike, and each file is opened and checked against the listing before it is
read. A bar recording of a session inside the encode's span that arrives
after the listing (the daily universe step finishing late, say, when
`--asof` is that day) refuses the stream as missing ("no recording for
trading day ..."); encode again once it is recorded. A bar recording outside
the span, or a second one beside a recording the day already has, is
ignored. A sector recording that arrives after the listing is ignored for
this run: the newest listed one within 7 days is used, or, if there is none,
the stream is refused ("no sectors-... recording on or before ..."). A
listed recording replaced or removed before one of its reads refuses the
stream ("... changed during the encode" or "... disappeared during the
encode"); run the encode again. One replaced while it is open being read is
read in its original form, the one listed.

The budget is **64 MiB of Python memory** (the tracemalloc peak, the measure
the Boost figures above use), for the warm-up and the daily firing alike,
and it holds at the real market's size. The test's made-up recordings hold
2,000 listed stocks in each session, with quarterly dividends, some splits
and 500 stocks ranked; `make check` runs them over two years (502 sessions,
2025-01-02 to 2026-12-31) and a plain `make adapter-test` over fourteen
months (291 sessions) in about half the time. Over two years the warm-up
peaks at about 13 MB and the daily firing at about 13 MB, where reading
every session at once peaked at about 2.3 GB and 1.15 GB; the test also
holds the two-year fixture under 19 MiB, the 64 MiB budget scaled by the
fixture's share of the real-size peak (13.2 MB of 43.2 MB), so a regression
that grows the peak in proportion past 64 MiB at the real size (about 1.5x)
fails on the fixture first. Measured once on made-up sessions
of 10,000 instruments over the same two years, the warm-up and the daily
firing each peak at about 43 MB of Python memory, and about 90 MB and 96 MB
for the whole process: both the test's ~13 MB and the real size's ~43 MB
are under the one budget, and far inside
the 2 GB server beside the Gateway, three engines and their adapters; the
whole server's total is measured on the server before the IRAs go live.

What moves the peak, at the sizes measured:

- one session is held, so about 4 MB per extra 1,000 stocks listed in a
  session;
- the ranked stocks' windows, 8 bytes a close: about 1 MB for 500 stocks
  x 260 sessions. A stock ranked at any decision whose window has started keeps its closes,
  so a year in which the ranked set changes completely every month holds
  about thirteen months' sets at once, 8 bytes a close: about 13 MB for
  6,500 stocks, still inside the budget;
- the length of the history, through what is kept for every session: the
  named instruments' bars (SPY, IEF, SHY and the holdings, a few per
  session) and their adjustment figures, a date and a few figures per
  session for the ranking (which sessions decide, and when each universe
  was fetched), and the span's split and dividend rows. Fourteen months
  peak at about 11 MB, two years at about 13;
- bad data: each stock keeps the reason for its first 3 malformed bars and
  counts the rest as runs of sessions with one shared reason, so a feed
  broken for many stocks every session adds a few entries per stock, not
  one per stock per session (the exclusion lines then name the first 3 bad
  bars and the runs after them). A stock that alternates between good and
  bad bars day by day costs one entry per bad stretch.


### A session the exchange cancels

Now and then the exchange closes on a day its normal rules call a session (a
national day of mourning, a storm). Left alone, that day looks like a gap in
every stock's prices, and the momentum rule refuses to rank a stock with a
gap in its last 253 sessions: the IRAs would stop rebalancing for a year.
Record the closure once:

```
python3 -m marketdata retract --out DIR --session 2026-11-25 --reason "NYSE closed: national day of mourning"
```

From then on `encode` writes the calendar from the NYSE rules as before,
followed at once by a `market.session.retracted` for that day, and treats the
day as no session: it needs no price recording, histories skip it, and the
calendar reaches one extra session ahead to step past it. The engine removes
the day from its calendar, so every window simply reaches one session
further back. If the cancelled day was the last of its month, the session
before it becomes the decision session. If that session has already closed
when the retraction arrives, an engine configured `"bars"` decides at
once; one configured `"compact"` waits for that session's histories, which
a stream encoded from the next session on does not carry, and withholds the
decision as `histories-missing` if the next session closes first (see
[momentum-universe](momentum-universe.md)).

A stock written only because it is a universe member is left out of some
sessions, instead of refusing the stream, when its data is bad. No bar is
written for those sessions and the session close does not name it.

- A malformed bar (a missing or negative close or volume, a high below the
  close) leaves out that one session's bar and its universe snapshot row,
  so the snapshot records that the stock was left out: a momentum engine
  configured `"bars"` needs a bar for every stock it ranks from the
  snapshot, and the stock is not ranked from that session's. Nothing more
  is needed: the momentum rule ranks a stock on its last 253 closes
  and leaves it out of any month's ranking whose window is missing a
  session, and records why (ADR 0026, point 5). So the stock is ranked
  again when its window is complete under that rule, 253 sessions after the
  bad one.
- A split or dividend that cannot be adjusted exactly is not applied, so
  the stock's adjusted prices jump at the ex-date. Only the first session on
  or after the ex-date is left out, as for a malformed bar, and the prices
  on each side of the jump have their true returns. No complete 253-close
  window spans the jump, because the ex-date session is missing from every
  such window, so the momentum rule ranks the stock again once its window
  lies wholly after the jump, 253 sessions after the ex-date. If that
  session also holds a malformed bar, it is still one session left out and
  both reasons are named. Two Go tests pin the rule this relies on: a stock
  with more than 253 closes but one session missing inside its window is
  left out of the ranking, not ranked on a shifted window
  (`TestScoreLeavesOutANameWithAGapInItsWindow` and
  `TestDecideRecordsEveryRankedName`, in `internal/targetweight/momentum`).
- A split or dividend with no readable date cannot be placed, so that stock
  is left out of every session.

Each session a stock is left out of, its universe snapshot row is left out
too, whichever stream is written (`--universe-members`, `--ranker` or
neither), so every stream ranks the same stocks. Its bars before the bad
session are written as usual, so bad data later on never changes an
earlier session. `encode` names each stretch on stderr (`marketdata: excluded
TICKER from FIRST to LAST (N sessions): reason`). SPY, IEF, SHY and every
other instrument are still written, and the same bad data in SPY, IEF, SHY
or any instrument named with `--instruments` or `--optional` still refuses
the whole stream. That refusal is what lets the engine score a stock you hold
across a missing day (ADR 0026 point 5, amended 2026-10-18): a holding named
with `--optional` can be missing a day only because it did not trade, never
because a split was left unadjusted. That guarantee holds only for the
compact stream (`--ranker`, below), where only named stocks have bars.
Naming every stock you hold with `--optional` is mandatory: one you leave
unnamed is treated as not held when its prices have a gap. In a bars stream (`--universe-members`) every member
has bars, named or not, so the engine scores no holding across a gap there
at all: a held stock with a gap is sold, as before 2026-10-18.

Sector labels are GICS from iShares' IWV holdings where it lists the stock,
else Nasdaq's sector, both mapped onto Morningstar's eleven sector names so
the momentum cap counts one vocabulary (plan.md section 4). A stock neither
source labels gets no label, and the momentum rule ranks it with no sector
cap, as the backtest did ([momentum-universe](momentum-universe.md)).
`encode` refuses a session whose iShares file has no `Sector` column, whose
screener response has no `sector` field, or in which either
source's stocks map to no Morningstar sector at all: a renamed column or vocabulary is an
outage to fix, not a month of unlabelled stocks.

Both sources serve only today's file, so `sectors` records the latest
settled session only: from an hour after its close until the next session's
close. `--asof` on a weekend or holiday means the trading day before it. A
past session is refused before anything is fetched, because today's file
saved under an earlier date would let a backtest use labels it could not
have had. A rerun fetches only the source not yet recorded for the session,
so a source that is down does not block one already saved.

## The daily data job

On the server, one job does all of the above for the Boost engine every
weekday, with no one at the keyboard (each IRA has its own instance of it:
"A job for the IRAs", below): `ti-data` (`adapter/marketdata/tidata.py`,
installed as `/opt/trend-investing/bin/ti-data` and run by `ti-data.service`).

**When it runs.** At 06:05, 07:05 and 08:05 New York time, Monday to
Friday (`ti-data.timer`), each time for the previous session: Tuesday to
Friday read the day before, Monday reads Friday, and the morning after a
holiday reads the last session before it. The market closes at 4 p.m. (1 p.m.
on an early close), and a session is not recorded until an hour after its
close, so its prices are final; a test checks every firing of the timer
against the service's `--settle-minutes 60` for every weekday of 2026 and
2027. Once the day's file exists, the later firings find it and fetch
nothing. If the server was off, the run happens when it starts again, and
catches up every session it missed in one file. A run that hangs is stopped
after an hour (`TimeoutStartSec=1h`) and shows as failed, like any other
refusal.

**The free plan's timing.** On Massive's free plan a session's prices are
not served on the evening of the session: asked at 17:05, 18:05 or 20:05 on
the day, Massive answers "403 forbidden", and the same request succeeds the
next morning (confirmed on the real server: a run at about 7 a.m. on
2026-10-27 recorded the session). The adapter's account reading is then
taken the next morning, and it stands for the session's close only with the
Flex reader's executions proof (your decision of 2026-10-27): IBKR's Trade
Confirmation statement, read each weekday morning before the job, showing
no execution from the close to midnight
([reconciliation](reconciliation.md), "The executions proof"). Without the
proof by 09:00 New York the session is withheld (`stale-account-snapshot`)
and places no order.

What the job does with that answer:

- A 403 for the latest settled session is "not available yet on this plan"
  from the session's own day (so also on the evening of the session, after a
  boot catch-up or a run you start by hand) until 08:00 New York on the next
  trading day, including over a weekend or holiday. The job prints one line in the journal
  (`ti-data: session 2026-10-23 is not available yet on this plan ...; the
  next firing asks again`), writes nothing, exits cleanly and pages no one.
  The next firing asks again.
- A 403 that is still there at the 08:05 firing (or any run at or after 08:00
  New York on the next trading day) is a failure: the unit shows failed and you are paged, because the
  plan should have served the session by then (the plan lapsed, or the key is
  wrong). Check the plan and the key at the Massive dashboard, then
  `sudo systemctl start ti-data`. Started on the evening of the session after
  the one that failed, that run fetches the missed session but then meets the
  same quiet 403 for that day's session, so it writes no stream and exits
  cleanly (which closes the incident). The next trading day's morning
  firing (06:05; on a Friday evening, the following Monday's, since the timer
  runs Monday to Friday and not at the weekend) writes one file holding both
  sessions. The timer also fires on a market holiday; that firing is usually the
  first after the session before it, so it fetches and writes that session
  (or waits quietly on a 403, since the failure hour is on the next trading
  day), and the next trading day's firing then finds that file and fetches
  only what is still missing. To have the stream sooner, run `ti-data --session <date>` for the
  last session once Massive serves it, which is the morning.
- A 403 for any older session of a catch-up, for a session you ask for with
  `--session` that is not the latest settled one, and any other error (401, 429,
  500, a timeout), is a failure at every hour.

The evening firings the job once had (17:05, 18:05, 20:05) are gone: on the
free plan they could only fail, and a quiet failure is not worth having
three times a day. If you move to a paid plan, the timer can be changed back
to an evening time. On a paid plan the session is served in the evening, so
what pages is any error other than a 403 (a 401, 429, 500 or timeout), a 403
for an older session, or a 403 still there at or after 08:00 New York on the
next trading day; a 403 on the session's own evening is a quiet wait, and an
evening-only timer has no morning firing to ask again.

**The upgrade option.** Massive's Starter plan serves a session's prices the
same day after the close. It is your choice and costs money; nothing in the
system needs it. With it, the job could run in the evening (edit
`OnCalendar` in `ti-data.timer`, for example `Mon..Fri 17,18,20:05
America/New_York`, which is after the 4 p.m. close plus the hour the job
waits, then `sudo systemctl daemon-reload`; keep `America/New_York` or the
times are the server's own, which may be earlier), the file would exist a few minutes after
17:05, and the decision would not change: Boost still orders for the next
open either way.

**What it does, in order.**

1. Picks the latest settled session.
2. Reads the Boost engine's durable log (the `-log` file of the run named by
   `TI_RUN` in the engine's `run.env`) for the run's configuration hash and
   strategy version, and uses the run's name (the log's file name without `.log`; the
   run's *identity* is a separate thing, used only to find its folder) as the
   stream's correlation id, so the stream carries exactly the name the engine expects. The log's
   first input is the configuration the engine started with; the job refuses
   if `/etc/trend-investing/engines/boost.config` no longer agrees with it on
   the instrument, `warm_up_start` or an indicator length, since the stream
   would then carry the run's hash over a history built for another
   configuration.
3. Fetches from Massive, once, the whole-market bars of each session since
   the last stream it wrote (one request a session, about 13 seconds each at
   the free plan's 5 a minute) and the splits and dividends of the last two
   weeks, and records them.
4. Checks every file it has already written for the run: each must still come
   out byte for byte the same from the recordings. If one does not, it stops
   (see "A file already written differs" below) and writes nothing. A split
   or dividend dated after a file never changes that file: prices are
   adjusted forward from `warm_up_start`, so an action changes only the
   sessions on and after its ex-date, and SPY's routine quarterly dividend
   goes into the next file without stopping anything (a test pins this for a
   dividend and for a split). Only SPY's actions matter to the Boost stream,
   so a revised figure for another ticker's dividend in the market-wide
   recordings is ignored; a revision of SPY's own is not (below).

   **What the check costs.** The job encodes the recordings once, from
   `warm_up_start` through the newest file's last session, and compares each
   file with its own sessions of that one encoding, so the check grows with
   the number of sessions, not with the number of files. Measured on the Mac
   mini with market-sized recordings (about 11,000 tickers a session) for a
   warm-up from 2025-01-02 and ten daily files (447 sessions): about 8
   seconds for the check, about 16 seconds for a whole daily run (the check
   and the new file each read the history once), 8 seconds for a firing that
   finds the day already written. That grows by about 18 milliseconds a
   session per read, so a year more of sessions adds about 5 seconds to
   each; allow two or three times as long on the Lightsail server. Encoding
   every file separately instead would have cost about 8 seconds per file,
   over half an hour a run after a year, against the unit's one-hour
   timeout.
5. Writes the engine's input for the new sessions: SPY's bar, then the
   session close, for each session, to
   `/var/lib/trend-investing/data/streams/boost/<TI_RUN>/boost-<YYYY-MM-DD>.jsonl`,
   named by the last session it holds. Each file follows on from the one
   before it, to be read in date order. The adapter delivers each file to
   the engine ("What happens to the file", below).

**The file format.** One event per line, in the order they are to be
delivered. The `sequence` on each line is its position in that file (1, 2,
3, ...), not the engine's own input number. The engine numbers every input
of a run in one series, whatever it came from (the configuration, the Run
approval, fills, account snapshots, these files), so only the program that
delivers the files, which sends one input at a time and reads the engine's
answer, knows the next number: it is to replace each line's `sequence` with
it, stamp the line's `recorded_at` with the time it sends it, and send
everything else as written. The adapter
is that program ("What happens to the file", below). A file therefore
never claims a number another input could already have taken.

**The one-time warm-up.** The engine's 200-day average and its RSI need
history before the first decision. Before a run's first paper day, after the
engine has been started once (so its durable log exists), run the warm-up
once, with the configuration's `warm_up_start` date:

```sh
sudo systemd-run --unit=ti-warmup --collect -p User=ti-trader -p Group=ti-trader \
  -p LoadCredential=secrets:/run/trend-investing/secrets/secrets.env \
  /opt/trend-investing/bin/ti-data --data /var/lib/trend-investing/data \
  --config /etc/trend-investing/engines/boost.config \
  --engine-log /var/lib/trend-investing/engines/boost/<TI_RUN>.log \
  --backfill-from <warm_up_start>
```

The secrets rest on disk only encrypted (ADR 0028), so the credential is the
unlocked copy `sudo ti-unlock` decrypts into `/run/trend-investing/secrets/`:
until you have unlocked, systemd refuses the command. The job runs as its own
transient unit and keeps running if your terminal closes: follow it with
`sudo journalctl -fu ti-warmup`, and wait for `ti-warmup.service: Deactivated
successfully.`; the file it wrote is the last path in that journal. At the
free plan's pace it takes about an hour and a half for a warm-up from
2025-01-02. The ordered, rehearsed steps around it are in
[the first dry-run day runbook](../runbooks/first-dry-run-day.md).

It records every session from that date (resumable: a second try fetches
only what is missing), checks there is no missing day and at least as many
sessions as the longest indicator window (200 for the trend average), and
writes the run's first file, holding the whole history. Any other date is
refused, because the indicators are seeded from the configured one.

**Memory the data job needs.** Each recorded session holds every instrument
Massive returns (about 10,000), but Boost reads one. The job therefore reads
a day's recording, keeps only the configured instrument's bar from it and lets
the rest go before it reads the next day. Memory holds at most one day's
recordings (two when both the grouped and the universe recording exist, as
they are held together to check they agree) plus the kept bars, in the warm-up and in every daily
firing alike (the daily firing re-reads the whole recorded history to check
the files already written, so it needs the same care). Measured on synthetic
sessions of 10,000 instruments: the job's Python-memory peak is about 10 MB
and its whole-process peak about 53 MB (a day with both recordings peaks at
about 16 MB of Python objects and so roughly 59 MB whole-process), the same for 150 sessions as for 456
(the two years from 2025-01-02), where before it grew with the history, to
about 1.1 GB at only 150 sessions. The budget is 100 MB for the job, so it
fits beside the Gateway and the dashboard on the 2 GB server with a wide
margin. If the server is short of memory anyway (the kernel's log,
`journalctl -k | grep -i "out of memory"`, shows the job killed), pause the
two big neighbours for the warm-up and start them again afterwards:

```sh
sudo systemctl stop ti-gateway ti-dashboard
# run the warm-up above and wait for ti-warmup.service: Deactivated successfully.
sudo systemctl start ti-gateway ti-dashboard
```

The Gateway logs in again when it starts, so the phone prompt may need
approving ("Unlocking the secrets after a boot" in [the server](server.md)).
Nothing is lost by the pause: the job resumes from what it has recorded.

Massive's free plan reaches back two years only, so the Boost
configuration's `warm_up_start` must lie within the last two years. The
shipped dry-run file says 2025-01-02, a session inside that window with well
over the 200 sessions the trend average needs before the first dry-run day.
It stays inside the window until 2 January 2027; a warm-up run after that
needs a later date, which is a new configuration and so a new run. Your paper
copy (`docs/runbooks/first-paper-order.md`, A5) keeps the same value unless you
choose another session inside the window with at least 200 sessions before the
first paper day; a paper copy that keeps 2025-01-02 differs from the shipped
file in its mode and account only.

A new engine run (a fresh `TI_RUN`) starts with no files and needs the
warm-up again; the recordings are already on disk, so it fetches only the
days since.

**A crash resume.** After a crash the engine is resumed under a new
`TI_RUN` ([durable log](../runbooks/durable-log.md), "Resume after a
crash"): a new log that holds a copy of the crashed run's records. It is the
same run, not a new one, so it needs **no** warm-up and gets no folder of
its own. The data job and the adapter both find the run's folder by the
run's identity, not by the log's name: its Run approval's ID (a paper run
always has one, the first run's stating the starting book; a dry-run first
run, which needs none, is known by its configuration and as-of time), which
the copied log carries. The folder is the one named after the run that began
the chain, the `TI_RUN` its warm-up was run for, and stays so through any
number of resumes:

- the data job writes the next day's file into
  `/var/lib/trend-investing/data/streams/boost/<first TI_RUN>/`, and every
  line still carries that first run's name, so the files already written
  still check byte for byte;
- the adapter, started on the new log, carries on with the files it had
  written down before the crash: whatever the crashed run had already
  recorded (it is in the copied log) is struck off and never sent again,
  the input it was waiting on is asked for again and answered from the
  copied log, and the rest of the batch goes to the resumed run, behind the
  snapshot already taken, once.

Keep the first run's log (`<first TI_RUN>.log`) in the engine's folder
while its resumes run: the folder is matched to the run through it. If it
is missing, both stop, and say so from the resumed log itself: a log that
already holds committed delivered inputs (bars, session closes and the
data job's other inputs, and the adapter's account and broker snapshots and marks; an operator input such as a kill switch, the
approval and the configuration never count) holds copied history, which a
fresh run's log cannot, so the message says "the log <path> already holds
N committed delivered inputs beyond its configuration", N counting delivered
inputs only (market data and the adapter's snapshots and marks), that if it is a crash resume
of an earlier run you restore that run's log beside it and start again, and
that you do not run a warm-up under this name; if a stream folder with no
log beside it exists, the message names it as the likely first run's.
Restore the log from the backup rather than running a warm-up for the new
name. A fresh run's log holds nothing but its configuration and approval,
and only that gets the "No warm-up yet" instruction (below). While the
first log is there, a warm-up run for the resumed `TI_RUN` makes no second
folder: it writes the next day into the first run's folder like any day. If
a folder named after the resumed `TI_RUN` exists anyway, once the first log
is back the data job and the adapter both stop, naming both folders ("more
than one stream folder"), as they do for any two folders of one run, the
one named after the running `TI_RUN` included. Move the one you made out of
`streams/boost/` (keep it, it is evidence) and start them again. A fresh run
after an earlier one (with your approval) is a new run with its own
identity: it never takes the earlier run's folder, and needs its own
warm-up.

Only the logs of folders in `streams/boost/` are read, and each one first
by its first records alone (its configuration and, for a fresh run, its
approval: the run's identity). A log whose first records name another run
is read no further. A log whose first records cannot be read (one an older
build wrote, or one damaged after its run ended) cannot be matched to any
run. The data job prints a `ti-data warning:` line (the adapter logs a
warning) naming its folder, and both stop, naming the skipped folders
("cannot be matched to any run"), whether or not a folder of the running
run's own name exists: the skipped log may be this resumed run's own first
log with a damaged head, and a warm-up for the new name would deliver the
whole warm-up again to a run whose log already holds it. If this run is a
crash resume, restore its first run's log from the backup as the engine
wrote it; if the log is an old run's, once that run has ended move its
folder out of `streams/boost/` and its log out of the engine's log folder
(`/var/lib/trend-investing/engines/<instance>/`, beside the running log),
and keep both as evidence in a folder outside both: a durable log is never
edited or deleted. A log whose first records name this run is read
whole: if it cannot be read, both stop, naming it ("names this run in its
first records but cannot be read"); restore it from the backup as the
engine wrote it.

A run is taken for a resume of the run whose folder it finds only when its
log begins with that run's records, copied exactly: `-resume` writes the
crashed run's records into the new log one for one under the same header,
so each keeps its `record_hash`, and the earlier log's committed records
are the first records of the new one, hash for hash, in the same order (the
resumed run's own records follow). The hashes are not taken on trust: each
one compared is computed again from its record and the one before it, the
way the engine chains them, in both logs, and a log whose stated hashes are
not the ones its records give was changed after the engine wrote it. Both
stop, naming it ("does not follow its record hash chain"); restore it from
the backup as the engine wrote it, never edit it. That run must also have committed more
than its configuration: the hash chain cannot tell a resume of a run that
committed only its configuration from a second start of the same run,
whose configuration record is the same, byte for byte. Otherwise both stop,
naming both logs ("nothing shows it is that run's crash resume"), the
message names the three cases below with the remedy for each, and nothing
is written or sent:

- **a dry-run first run started again without an approval** (same
  `run.env`, same configuration and as-of time; a paper engine refuses such
  a start itself, and a paper first run has an approval to tell it by): it
  has the first run's identity but not its records, and is never handed the
  first run's folder, record of files sent and stream without its own
  warm-up. Stop it, and start a fresh run with its own warm-up (a dry run
  needs no approval; a paper run needs its approval) ([paper
  trading](paper-trading.md), "Never start a run again without an
  approval");
- **a first run that crashed having committed nothing but its
  configuration**: nothing in the two logs tells its resume from a second
  start, so it is not taken for either. Nothing of it needs keeping: start
  a fresh run with its approval and its own warm-up;
- **a log that is not a copy of that run's records**: one that repeats the
  run's input IDs with other content, say, or a copy the engine is still
  writing (the data job ran while `-resume` was copying). It is not that
  run's resume: resume the crashed run's own log with `engine -resume`, or,
  if the engine was still copying, run the job again once it is listening.
  The check is against the first run of a chain of resumes, the one whose
  folder it is: a copy still being written is refused until it holds all
  of that run's records, and taken from then on, even while a later
  resume's records are still being copied behind them. That does no harm:
  the data job only learns which folder to write into, and writes market
  data under the first run's name, whatever the rest of the copy holds; the
  adapter reads the log only once the engine listens, when the copy is
  whole.

**What you see.** Each run prints the build it is (`ti-data <commit>`), then
the path of the file it wrote, or of the file that already held the session;
`journalctl -u ti-data` shows it. `ti-data --version` prints the build too. A
refusal prints `ti-data refused:` and the reason, exits 1, and the unit
shows as failed, which the alerting (plan.md section 7) is to watch. The unit
is never skipped: before the job is installed each firing fails with a message
saying so. Before the Boost engine has been started once (no `run.env`, no
`.log` run log in its folder and no run folder under `streams/boost`) the firing is not a failure: it prints one line, `ti-data: waiting for the
account's first run: ... run.env is missing or empty`, writes nothing and
succeeds, so nothing pages you ([server](server.md), "The reporter before the
first run"). Once `run.env` exists the job is unchanged. A `run.env` that is
missing or empty while a run log or a run stream folder exists, or that is not a regular file, is over
64 KiB or cannot be read (a link to nothing included), is a refusal like any other.

**What can go wrong.** In every case no file is written for the session,
so the engine never reads half a day. Recordings already made are kept, and
the next run (or `sudo systemctl start ti-data` once the cause is fixed)
carries on from them.

- **No Massive key**: the line `MASSIVE_API_KEY=` in `secrets.env`
  (stored encrypted; edit it with `sudo ti-secrets edit secrets.env`) is
  empty or missing. The key is never
  printed, even when an error from Massive would contain it: it is replaced
  by `[MASSIVE_API_KEY]`.
- **The session has not settled**: only when a session is named with
  `--session` before its close plus an hour.
- **No warm-up yet** for the engine's run: the message gives the warm-up
  command's date. It is given only to a fresh run, whose log holds nothing
  but its configuration, approval and any operator inputs; a log that
  already holds committed delivered inputs is a crash resume whose first log is missing, and the
  message says to restore that log instead (next but one, below).
- **A log that already holds committed delivered inputs, with no stream folder of its
  run** (a crash resume whose first run's log is missing from the engine's
  folder: "A crash resume", above): the data job and the adapter refuse,
  naming the log and its count of committed delivered inputs, and the stream folder with no
  log beside it as the likely first run's. Restore the first run's log
  from the backup, unchanged, and start again; do not run a warm-up under
  the new name.
- **The engine's durable log cannot be read** (a `run.env` exists, so a run
  was started; before that the job waits quietly and succeeds), or its first input is not a Boost configuration.
- **The configuration file and the engine's log disagree**: the file was
  edited after the engine started. Put the file back as the engine started
  with it; an edited configuration is a new run.
- **Massive returns nothing for a trading day**, or a recording from an
  earlier day is missing: the run names the day. A missing earlier day means
  a recording was removed; restore it from the backup (today that means a
  Lightsail snapshot, if snapshots are on: [server](server.md), "Backups").
- **A history shorter than the indicators need** at the warm-up.
- **The configuration is not usable**: "it is not a Boost configuration"
  (the engine's first input is another strategy's, or the file is), "warm_up_start
  ... is not an NYSE session" (pick a trading day), "--backfill-from ... is not
  the configuration's warm_up_start" (the warm-up takes that date only), or
  "session ... precedes the warm-up start". Correct the date you passed; the
  configuration is never edited after the engine started (an edited
  configuration is a new run).
- **A file already written differs** from what its recordings now give.
  Every run checks every earlier file, so this stops the job the day it
  happens, naming the file. The causes: Massive published a split or
  dividend late, dated inside a file already written; the engine's log
  header changed (the engine was restarted under the same run name with
  another build or configuration); a file was edited or removed; or a new
  ti-data build encodes an earlier session differently (a change to the
  encoder, the calendar or the adjustment arithmetic that alters any byte of
  a session already delivered). The job
  never rewrites a file and writes nothing more for the run, because the
  engine has already read the old one: a vendor's revision after delivery is
  your decision, not the job's. What to do: read the message, and for a late
  action check it against the fund's own notice. If it is real, the run's
  history is wrong from that date on: stop the engine, and start a new run
  (a fresh `TI_RUN`, `docs/runbooks/durable-log.md`) with its own warm-up,
  which builds the whole history with the action in it. If a file was edited
  or removed, restore it from the backup, unchanged ([server](server.md),
  "Backups"). If it began after a new
  ti-data build was installed, put the previous build back
  (`/opt/trend-investing/bin/ti-data`) and the job carries on; install a new
  build only together with a fresh engine run and its warm-up, or after
  confirming that the new build reproduces every file of the current run
  (`ti-data --session <the last written session>` reports the file and exits
  0 when it does). Never edit a file or a recording to make them agree.
- **Two recordings give different figures for one dividend** (or split):
  the refusal reads `ConflictingActions: dividends <id> differs between
  recordings`. Massive changed its figure between two fetches whose windows
  overlap, and the job will not choose. Only SPY's actions are checked: a
  revised figure for another ticker does not stop the Boost job. Each daily
  run records the last two weeks of actions again, so an action Massive
  published some days before revising it is held, with the old figure, in
  every daily recording from its first appearance to the day before the
  change, and in the warm-up's recording too when its long window covers the
  ex-date. What to do:
  1. Find every recording that holds it:
     `grep -l '"<id>"' /var/lib/trend-investing/data/dividends_*.json`, and
     compare their `cash_amount` (and ex-date). The list is usually more
     than two files.
  2. Check the true figure against the fund's own published distribution
     notice.
  3. Move every recording that holds the wrong figure, unchanged, into
     `/var/lib/trend-investing/data/quarantine/` (create it, owned by
     `ti-trader`), the warm-up's included if it is among them. Moving only
     one leaves the others to stop the next run the same way. The job reads
     only the data folder itself, never `quarantine`, and never deletes or
     edits a recording. If the correct figure is in none of them, quarantine
     them all.
  4. Run `sudo systemctl start ti-data`. The windows the quarantined files
     covered are now missing, so the job fetches them again from Massive. If
     Massive still gives the wrong figure, stop: the job can record only what
     Massive publishes, and the paper run waits while you decide.
  5. If the corrected figure falls inside a file already written, the run
     then stops at "A file already written differs" (above): start a new run.

### A job for the IRAs

The Traditional IRA's momentum engine gets its own instance of the same
morning job: `ti-data@trad-ira.service`, fired by `ti-data@trad-ira.timer`
(the Roth IRA's will be `ti-data@roth-ira` once it has a configuration).
Boost's `ti-data.service` and its timer are unchanged, and so is every byte
of Boost's stream (a test pins its bytes). The two jobs share the
recordings folder: a session either one recorded is not fetched again.

Why an instance per engine, not one job for all: each engine has its own
`run.env` and run, so one unit could not load both; a failure of the IRA's
job pages as `ti-data@trad-ira.service`, never as Boost's; and Boost's unit,
its paging and its schedule did not have to change at all.

**When it runs.** At 06:15, 07:15 and 08:15 New York time, Monday to
Friday: Boost's three mornings, ten minutes later, so Boost's job has
usually recorded the session's whole-market bars already and the IRA's job
reuses them. The two may still run at once (a catch-up after the server was
off, a start by hand, a long Boost catch-up still running at :15), and that
is safe: a recording the other job wrote first is used as it stands, never
fetched twice into the same file, and a 429 from Massive at the free plan's
5 requests a minute is waited out. Each firing reads the previous session (a test
checks every firing of 2026 and 2027). The free plan serves a session only
the next morning (`docs/live-trading/lessons-2026-10.md` L2), and an IRA
decides on the first session of a month from the previous session's
closes, so the month-end universe and the ranked stocks' histories are
written that morning, before the adapter's 09:00 cutoff. A month whose last
day is a holiday is read from the calendar: for May 2027 (Memorial Day is
Monday the 31st) the job firing on Tuesday 1 June reads Friday the 28th,
and the Go ranker calls it the decision session because the next session
is in June (a test runs exactly this). A 403 at 06:15 or 07:15 is a quiet
wait; one still there at 08:15 is a failure that pages, as for Boost.

**What it does, in order.**

1. Picks the latest settled session, as Boost's job does.
2. Reads the IRA engine's durable log (the run named by `TI_RUN` in
   `/var/lib/trend-investing/engines/trad-ira/run.env`) for the run's
   configuration hash, strategy version and name, and refuses unless
   `/etc/trend-investing/engines/trad-ira.config` holds the same momentum and
   glide sections as the configuration the engine logged (the ranker reads
   the file, so an edited file would rank another market). The
   configuration must say `"universe_input": "compact"`.
3. Reads from the same log **which stocks you hold**: the positions of the
   engine's last account snapshot (version 3, as the adapter writes today,
   or 4, which adds the cash an IRA's buys are budgeted from), or, before
   the first one, of the Run approval's starting book, plus every stock
   filled since. A fresh run approved without a starting book takes the
   previous run's book, so its holdings are read from the previous run's
   log, the one its approval names (checked by its final record hash, and
   so on back through earlier runs that did the same). Those get their own daily bars (`--optional`); nothing else
   does. There is no flag to name them, so a holding cannot be forgotten. A
   first run before its first reading holds nothing. A snapshot, fill or
   approval of another version, or a log whose record hash chain is broken
   (changed after the engine wrote it), stops the job, naming the log.
   The log is read a line at a time, keeping a few fields of each input, so
   the memory it needs is its longest line (a day's universe snapshot, under
   1 MB) and a few hundred bytes an input, not the log: a test reads a year of
   daily snapshots inside the 64 MiB budget.
4. Checks the run's folder against its **manifest**, `manifest.json`
   beside the files, which the job writes whole each time it publishes one:
   each file's name, first and last session, length and SHA-256, the
   sessions in it written without a universe, and the split and dividend
   recordings the files were last checked against. A file truncated,
   changed or gone (the newest included) stops the job; it is never written
   again from today's recordings and holdings. The files must form one
   unbroken chain from the warm-up. A newest file that the manifest does not
   list yet (the job stopped between the two writes) is taken in only if it
   encodes again byte for byte. One run of the instance's job at a time
   holds its lock (`streams/trad-ira/.ti-data.lock`); a firing that finds it
   held writes nothing: quietly at 06:15 and 07:15, which the next firing
   follows, and as a failure that pages at 08:15, the last before the 09:00
   cutoff. The manifest is written before the run's first file too, so a stop
   between the two is taken in the same way. The requested file of a firing
   that finds its session already written is always encoded again.
5. Records first, before asking Massive anything, the latest session's
   sector labels (the sources serve only that session's files, so a morning
   when Massive fails still keeps them; a source that is down is named in a
   warning line). Then each session since the run's last file: the
   whole-market bars once (skipped if Boost's job already recorded them; a
   cancelled session recorded with `retract` is never asked for, and a file
   is never named after one: when the latest session is cancelled, the
   firing's session is the last real close before it (its file, if not yet
   written, is written now and named after that close), and the next file
   begins after the cancelled day and carries its retraction; see
   "Cancelled sessions, one rule" below), and the
   universe (the common-stock listing dated that session, written with the
   very bars just recorded, so the two recordings agree) of every such
   session that has labels from both sources within the week up to it. So
   when every firing of a month's first morning failed, the next morning's
   catch-up still writes the month-end universe and histories. A session
   with no labels within a week gets no universe. For the latest session
   the labels can still be fetched until its next close, so before 08:00 on
   the next trading day (the firings at 06:15 and 07:15) the job writes
   nothing and the next firing asks again (`the sector labels of ... are not
   recorded yet`). From 08:00 on (the last morning firing at 08:15, or a run
   you start by hand after 08:00) it writes the file without that universe, one warning line names
   every session left without one, a decision on any of them is withheld
   (`missing-universe`) and the next mornings carry on. A published file is
   never rewritten, so a month-end universe published without its labels is
   lost for good: that month is withheld and the next decides afresh; a
   later firing that finds the file says so. Then the splits and dividends
   of the last two weeks, as Boost's job does.
6. Checks the files already written that a split or dividend recording
   which appeared, changed or went since the manifest's check could reach
   (those ending on or after the earliest window among them: this firing's
   own two-week window, Boost's, or one another account's backfill recorded
   long ago), each encoded again alone, with the holdings its own bars name.
   One that differs stops the job ("A file already written differs",
   above). Each takes a whole-history encoding, so a firing checks at most
   15 files; more (a backfill reaching far back) stops the job with a line
   naming the recordings, and you run it once by hand with
   `--reencode-all` (the warm-up command below with `--reencode-all` in
   place of `--backfill-from ...`), which checks them all, however long it
   takes.
7. Writes the compact stream for the new sessions, exactly what `encode
   --ranker` writes for them (a test compares the bytes, on the recorded
   fixture of the compact stream above): the calendar from the run's base,
   SPY, IEF, SHY and the holdings' bars, the session's universe snapshot,
   and on a month's last session the 500 ranked stocks' histories, to
   `/var/lib/trend-investing/data/streams/trad-ira/<TI_RUN>/trad-ira-<YYYY-MM-DD>.jsonl`.
   The ranker is `/opt/trend-investing/bin/universe-rank`, run on a
   private copy of the configuration checked against the engine's log, so
   a file edited while the job runs cannot change the ranking. The stocks
   left out for bad data are named on stderr after the file is written.

**Cancelled sessions, one rule.** A file covers its sessions from first to
last and is written under exactly the retractions recorded for sessions on
or before its last, and every later check of it encodes it the same way. A
retraction of a later session travels with the next file: its calendar and
a `market.session.retracted` right after it, before any session closes. So
a closure announced days ahead, or one recorded before a delayed file is
written, leaves that file with the rules' calendar through the cancelled
day, and the next file brings the retraction. A retraction of a session on
or before a file's last, recorded after the file was published, changes a
stream the engine has already read: the job stops for a person ("A file
already written differs", above; when it is the file's own first session,
the message reads "... begins at ..., but a retraction of ... was recorded
after it was published"; a cancelled day recorded with bars stops it too). The firing's session is the last real close on or before the
latest settled session; when its file exists, the firing returns it
unchanged, after every check a new session's firing makes.

**Memory.** The stream is the one-session-at-a-time path above, inside the
same 64 MiB budget; a test runs this job's warm-up and daily firing, with
the real Go ranker, on the two-year fixture of 2,000 stocks. On the server
each new session has two recordings (Boost's whole-market bars and the
universe), which are read together to check they agree: about 6 MB more at
the real market's size, still inside the budget. The engine's durable log
is read a line at a time, keeping only a few fields of each input (step 3),
so its growth (about half a megabyte a day of universe snapshots) does not
grow the job's memory: the same test reads a log of a year of daily
snapshots inside the budget.

**The one-time warm-up.** Once the IRA engine has been started for its run
(so its durable log exists) and before its adapter delivers anything, run
the warm-up once. Unlike Boost's configuration, the momentum configuration
names no start date, so you choose the run's **base**, the first session of
its history; every later file is adjusted from it. Use `2025-01-02`: Boost's
warm-up has recorded every session since then, so only the days the IRA
needs beyond them are fetched, and it leaves well over the 260 sessions a
ranked stock's history needs before the first decision (and SPY's 200).
Massive's free plan reaches back two years only, so a base for a later run
must still lie within two years of the day its warm-up runs, unless its
sessions are already recorded.

```sh
sudo systemd-run --unit=ti-warmup-ira --collect -p User=ti-trader -p Group=ti-trader -p RuntimeMaxSec=4h \
  -p LoadCredential=secrets:/run/trend-investing/secrets/secrets.env \
  /opt/trend-investing/bin/ti-data --engine momentum --instance trad-ira \
  --ranker /opt/trend-investing/bin/universe-rank --data /var/lib/trend-investing/data \
  --config /etc/trend-investing/engines/trad-ira.config \
  --engine-log /var/lib/trend-investing/engines/trad-ira/<TI_RUN>.log \
  --backfill-from 2025-01-02
```

Follow it with `sudo journalctl -fu ti-warmup-ira` until `ti-warmup-ira.service:
Deactivated successfully.`; the file it wrote is the last path in that
journal. Until the warm-up has run, each morning firing of the run is
refused with a message naming this command (and pages). Before the IRA
engine's first run (no `run.env`) each firing waits quietly: `ti-data:
waiting for the account's first run: ... (start the trad-ira engine once)`.

**What can go wrong.** As for Boost's job (above), and also:

- "it is not a momentum configuration", or "its universe_input is
  'bars'": the IRA's file or its logged configuration is another
  strategy's, or reads the bars stream; the job writes the compact stream
  only.
- "the momentum configuration ... is not the one the engine's durable log
  ... holds": the file was edited after the engine started; put it back.
- "the sector labels of ... are not recorded yet": iShares or Nasdaq is
  down this morning. Before 08:00 nothing is written and the next firing
  (or `sudo systemctl start ti-data@trad-ira`) asks the sources again.
- "no universe is recorded for ... session(s)": the labels were still
  missing at the 08:15 firing (or a start after 08:00), so the file was
  written without that session's universe. A published file is never
  rewritten: that session's universe is lost for the run, a decision on it
  is withheld as `missing-universe` ([momentum universe](momentum-universe.md)),
  a start after that changes nothing, and the next mornings carry on. The
  remedy is to keep the sector sources healthy (a renamed column or page
  needs a new build), not to run the job again.
- "cannot read holdings: account.snapshot version ..." (or a fill's or an
  approval's), or "does not follow its record hash chain": the engine's log
  holds an input this build does not read, or was changed after the engine
  wrote it. Nothing is written; restore the log from the backup, or install
  the build that reads that version.
- "the run's files are not one unbroken chain", "... is missing: the job
  published it", "... changed since it was published", or "ends at ...,
  not at the ... its manifest lists": a file the job wrote for the run is
  gone or changed. Restore it from the backup, unchanged; never write it
  again.
- "holds stream files but no manifest" or "the run's manifest ... cannot
  be read": restore `manifest.json` from the backup, unchanged. Without
  one, rebuild it by hand: the warm-up command above with
  `--rebuild-manifest` in place of `--backfill-from ...` takes in every
  file in chain order, each only if it encodes again byte for byte, and
  refuses, writing no manifest, if any differs (then the run's history no
  longer comes from its recordings: start a fresh run with its own warm-up),
  or if a split or dividend recording appears while it runs (run it again).
  It refuses while the manifest can be read: a file the manifest lists that
  is gone is restored from the backup, never forgotten by a rebuild.
- "split and dividend recordings appeared or changed ... more than one
  firing checks": run the job once by hand with `--reencode-all` (it names
  the recordings); if a file then differs, see "A file already written
  differs".
- "another run of the trad-ira data job holds its lock": a warm-up or a
  start by hand is running. At 06:15 and 07:15 the firing wrote nothing and
  succeeded; at 08:15 it fails and pages ("... this is the last morning
  firing"): check the run holding the lock (`systemctl list-units
  'ti-warmup-ira*'`). A run by hand is stopped after four hours
  (`RuntimeMaxSec=4h` above), so a hung one cannot hold the lock for days.
- A month's last session cancelled after the session before it was written:
  that earlier session becomes the decision session once the retraction
  arrives with the next file, but its file was written, under the rule
  above, without the month's histories (the ranker then saw a session
  still to come in the month). The engine withholds the month
  (`histories-missing`, [momentum universe](momentum-universe.md)). Nothing
  repairs it automatically; the next month decides as usual. Recording
  the closure before that earlier session's file is written does not help
  either, by the same rule.
- "... is retracted, but its grouped bars are recorded": a session was
  cancelled after its bars were recorded. Check which is true; a wrong
  retraction is moved to `/var/lib/trend-investing/data/quarantine/` as for
  a wrong action recording.
- "cannot read holdings: ... previous log ...": the fresh run took the
  previous run's book, and that log is missing or not the one its approval
  names. Restore it beside the run's log, as the engine wrote it.
- "the ranker refused" or "did not answer within 60 s": `universe-rank` is
  missing, from another build, or given another configuration
  ([momentum universe](momentum-universe.md), "What can go wrong").
- `ConflictingActions` for any stock, not only SPY: the IRA's stream ranks
  the whole market, so a revised dividend of any stock stops its job (the
  quarantine steps above apply), while Boost's job carries on.
- "--backfill-from ... is not the run's base": a warm-up date given again
  later must be the run's own base.

**Checking it the day it is installed.** Run these on the server right
after installing the build, the same day, not the next morning
(`docs/live-trading/lessons-2026-10.md` L10). With no IRA run started yet:

```sh
sudo systemctl start ti-data@trad-ira.service
systemctl show -p Result --value ti-data@trad-ira.service          # success
sudo journalctl -u ti-data@trad-ira.service -n 2 -o cat
#   ti-data <commit>
#   ti-data: waiting for the account's first run: /var/lib/trend-investing/engines/trad-ira/run.env is missing or empty (start the trad-ira engine once)
systemctl list-timers ti-data@trad-ira.timer                        # NEXT: the next weekday 06:15, 07:15 or 08:15 New York
/opt/trend-investing/bin/universe-rank -config /etc/trend-investing/engines/trad-ira.config </dev/null; echo $?   # 0, nothing printed
```

Once the IRA engine's run has started and its warm-up has run, the next
firing writes the day's file:

```sh
sudo systemctl start ti-data@trad-ira.service
sudo journalctl -u ti-data@trad-ira.service -n 1 -o cat
#   /var/lib/trend-investing/data/streams/trad-ira/<TI_RUN>/trad-ira-<the previous session>.jsonl
f=$(sudo journalctl -u ti-data@trad-ira.service -n 1 -o cat)
sudo head -c 120 "$f"; echo                                         # {"id":"marketdata:calendar:<base>:...
sudo grep -c '"type":"market.universe.snapshot"' "$f"               # 1
sudo grep -o '"type":"market.bar.completed"' "$f" | wc -l           # 3 (IEF, SHY, SPY) plus one per holding
sudo grep -c '"type":"market.close.history"' "$f"                   # 500 on the morning after a month's last session, else 0
```

The engine does not read that file yet: delivering it, after the morning
account reading, is the IRA adapter's job (`ti-adapter-trad-ira`, not built
yet), and the daily summary and dashboard do not show the IRA's data job
yet either.

### What happens to the file

The file is the engine's input for those sessions, and the Boost adapter
(`ti-adapter-boost`) carries it there. Nothing else reads it.

**When.** The adapter looks in the run's folder,
`/var/lib/trend-investing/data/streams/boost/<TI_RUN>/`, on every turn of
its loop (about once a second) while its link to IB Gateway is up and
matched. A new file is picked up within a second or two of the data job
writing it, which on a normal day is a few minutes after the 06:05 to 08:05 firing that
records the previous session.
If the adapter is not running then, it picks the file up when you next
start it. If several files are waiting (the adapter was stopped for some
days), it takes them all, oldest first.

**What it sends, in order.**

1. **One account reading, as two snapshots.** Just before the file goes,
   the adapter reads the account at IBKR (positions, cash, net liquidation,
   Excess Liquidity and every open order) and asks IBKR for SPY's last
   traded price. From that one reading it sends the engine its **account
   snapshot** (positions and net liquidation, which orders are valued
   against) and then the **broker snapshot** (the same holdings, net
   liquidation, Excess Liquidity, every open order, and whether the market
   was open, by the NYSE calendar the data job uses: 9:30 to 4 p.m. New
   York, 1 p.m. on an early close).

   **As of the close, when it can stand for it** (your decision of
   2026-10-18; ADR 0019, ADR 0025 point 5, ADR 0026 point 1). The engine
   decides a session from the account at that session's close. Between the
   close and the next open the account changes only through an order still
   working, an execution, a corporate action IBKR books overnight or a cash
   posting (interest, a fee), so when the reading is taken after the close
   of the file's last session, either on that session's own New York date
   or on a later date before the next session opens with the Flex reader's
   executions proof (your decision of 2026-10-27), with no open order for the account at IBKR (working, pending or cancel-pending,
   from any client; in either read of the account and in the last list of
   open orders, read after both), no execution in the account at or since
   the close (IBKR's own list, asked for every client and read last; it
   reaches back only to midnight, which is why a reading on a later date
   needs the executions proof to cover the close to that midnight
   ([reconciliation](reconciliation.md), "The executions proof"); a
   closing-auction fill stamped at the close itself counts),
   every holding with an official close in the file, IBKR's own net
   liquidation at the read above zero, the holdings exactly the previous
   close's plus or minus the fills since, and the cash the previous close's
   adjusted by those fills within **1% of net liquidation** (room for a
   day's interest or a dividend; `cash_drift` records the difference, and
   `unexplained_cash` that difference added up since the cash was last
   explained (for Boost, whose book states no cash, since your starting
   book), both shown in the daily summary; this check needs no client
   ID, so it catches a trade the adapter is not shown, a split or a
   transfer), and the engine holds no snapshot already stamped at or after
   that close, both snapshots are stamped **as of the close**. The previous
   close is the last account snapshot of this run, or of the earlier runs
   its approvals name, however long ago, or the **starting book** you
   stated when you approved the account's first run (or a fresh run, after
   a transfer or a sale by hand you checked; `operator approve -first-run` for the first
   run, docs/runbooks/first-paper-order.md, and `operator approve -log
   <previous run's log> ... -positions ... -cash ... -currency USD` for a
   fresh run, [operator](operator.md), "An approval"), whichever is latest: a fresh run's
   first reading is compared with the last run's close, never taken as the
   book unchecked. The
   broker snapshot records the real read time beside it (`read_at`) as
   evidence. If any of that fails, both are stamped with the real read time
   and the broker snapshot says every condition that failed
   (`not_as_of_close`). The engine then has no
   account snapshot as of the close, so it withholds the session
   (`stale-account-snapshot`) and orders nothing, and the reporter pages
   you with the reason. A batch of several files (the adapter was stopped
   for some days) is read once, after the last session's close, so only
   that last session can be decided; the earlier ones are withheld the same
   way.

   **One price basis: a reading as of the close is valued at the close**
   (ADR 0019, amended 2026-10-18). Each holding's mark in the broker
   snapshot is the closing price of that instrument's bar in the file's
   last session, the number the backtest marks at, and the net liquidation
   of both snapshots is the account's cash plus each holding times that
   close; IBKR's own net liquidation at the read goes beside it
   (`read_net_liquidation`), and the margin alarm compares Excess Liquidity
   (IBKR's, at the read) with that figure, never with the close's. A
   holding with no bar in the file (anything but SPY) cannot be valued at
   the close, so the reading is stamped at its read time. A reading stamped
   at its read time is IBKR's own reading throughout: IBKR's net
   liquidation, each holding marked at IBKR's last price, or with no mark
   ("mark set false") when IBKR gave none, and then the engine cannot size
   the joint account's emergency sale back to 98% from that snapshot,
   holds that sale back and asks you to act by hand.

   The account read and IBKR's last prices are one reading. The adapter first
   applies every report IBKR has already sent, so a fill or a cancellation
   received before the read reaches the engine ahead of the snapshots; it
   does not read while a fill waits for its commission; and after the
   prices it reads the account a second time. If a fill or an order report
   arrives between the start of the first read and the end of the second
   (an execution of any client's order counts, not only the engine's), if
   the holdings, open orders or cash differ between the two reads (a trade
   you placed in IBKR's own app, which IBKR reports to that app only, still
   shows there), or if any part of the account could not be read (IBKR
   timed out on the balances, say), it reads the whole account again, up
   to three times; IBKR's open orders and executions, read after the
   second read, are inside the same check, so an execution arriving while
   they are read reads again too (each execution counted once, by its ID,
   so a replay of the day's list is no change). The snapshots are the
   second read, with its IBKR time.
   IBKR's answers for the positions and the open orders carry no request
   number, so if IBKR does not finish either list in time, or Gateway
   loses IBKR's servers part-way through (it restores them on the same
   link), a late end of it
   could be taken for the end of the next read's list on the same link:
   the adapter then drops the link instead of reading again on it, and
   takes the snapshot once the link is back and matched (`ibkr broker
   snapshot: IBKR had not ended its ...` in its log). The log line `ibkr
   broker snapshot read_at=... as_of=... cutoff: ...` names the read time,
   the stamp, the last report recorded before each snapshot and, when the
   stamp is the read time, why.
2. **The reconciliation, with every file.** From the same reading, IBKR's
   positions and cash, for the engine to compare against its book (ADR
   0025 point 5, ADR 0019). Once the engine holds a book it goes **first,
   before the two snapshots**: the engine compares IBKR's holdings with the
   book it holds and the fills recorded since, before the new snapshot
   replaces that book, so shares no fill explains are found (Degraded on
   the instrument), never quietly taken over. A run's first file, before
   which the engine holds no book, carries it behind the snapshots when the
   reading matched the previous close or your starting book (above). When
   it did not, or had nothing to be compared with, **the reconciliation goes
   alone and both snapshots are withheld**: the engine, holding no book,
   finds it unverifiable and halts, so the difference is never taken as the
   book, and the halted run's log holds no snapshot of that reading for the
   next run to be compared with. The adapter's log gives the reason. At every later
   start of the adapter, and every reconnect, the adapter sends one too,
   read once every trade IBKR replayed has its commission and its fill has
   reached the engine, and before any of the file's lines; if IBKR does not
   give the positions or the cash in three reads in a row, the adapter
   stops for you rather than hold the day's file behind it.
3. **The file, line by line, as written**: each session's SPY bar, then that
   session's close, and, between the last session's bar and its close,
   **SPY's mark** (`market.mark.recorded`): the bar's official close, which
   the engine checks a buy's price against (a buy with no mark is refused,
   `missing-mark`), with IBKR's last price beside it (`broker_last`). The
   price band compares the buy's reference price with IBKR's last price when
   there is one, and with the official close otherwise (then it always
   passes: the band has teeth only when IBKR provides a price; ADR 0026
   point 3, amended 2026-10-18), and the order records which
   (`mark_source`). Without IBKR market-data permissions on the paper login
   there is no last price, and nothing else changes: **no decision needs
   IBKR market data**. The engine therefore records the
   account snapshot before the bars, the mark before the close, and each
   session closes only after its bar, before anything for that day is
   decided (ADR 0019, ADR 0021). Each line goes with the engine's next
   input number in place of the file's own (the file format, above), and
   with the time it is sent as its "recorded" time, as every input the
   adapter sends; nothing else in it changes. ADR 0019 measures the broker
   snapshot's age at the close itself (the session's end, the time the
   engine decides at), from the snapshot's stamp, and counts a negative age
   as stale: a sale withheld or a sell refused with `stale-broker-snapshot`
   at every close means the snapshot was stamped after the close, at its
   read time, and its `not_as_of_close` says which condition failed. That
   comparison uses only the two stamps: the close's comes from the file and
   the snapshot's from IBKR's clock, so setting the server's clock never
   clears it. The server's clock matters elsewhere (each input's recorded
   time, the adapter's own timeouts); check it separately with
   `clock-check.sh` ([the server](server.md), "The clock"). A fill or a cancellation IBKR
   reports while the engine works through the file goes to the engine
   before the file's next line, so no session is decided on an account
   behind a report already received. A trade IBKR has reported but whose
   commission has not come yet holds the file's next line back until the
   fill is recorded and sent; if the commission never comes, the adapter stops
   after `commission_wait` seconds, as it always does
   ([paper trading](paper-trading.md), "What can go wrong").

Several waiting files go behind one reading, oldest first. The next day's
file gets its own.

**Exactly once.** Before anything is sent, the adapter writes down which
files it is about to hand over, beside its pending file
(`/var/lib/trend-investing/adapters/boost/pending.json.delivered`). It then
puts the snapshots (and a first file's reconciliation), the mark and every
line of the files in its outbox in one write,
and marks the files handed over. From the outbox each input goes once, as
every other input does ([re-delivery](redelivery.md)). So:

- a file it has handed over is never sent again to its run, after a
  restart, a reconnect, or a later file;
- if it stops partway through a file, the next start carries on from the
  next line; the lines already sent are not sent again;
- if it stops between writing down the files and the outbox write, the next
  start finds nothing of them in the outbox or in the engine's log, takes a
  fresh snapshot and hands them over then; if it stops just after the
  outbox write, the next start finds the snapshot there and does not hand
  them over a second time;
- if the engine stopped after recording an input but before answering, the
  adapter asks for that input's answer again under the same name, and the
  engine answers from its log without applying it twice.

**What you see.** In the adapter's log (`journalctl -u ti-adapter-boost`):
`ibkr stream queued run=<TI_RUN> files=boost-<date>.jsonl inputs=<n>` when a
file is handed over, then one `ibkr input=... answered with ...` line per
input as the engine answers. In the engine's journal (`journal-query`), the
account and broker snapshots, the bar, the mark and the session close, and
whatever the engine decided at that close. Never edit the `.delivered` file; the adapter reads
it strictly and stops on anything it did not write.

**What can go wrong, and what the adapter does.** In every case below it
sends nothing from the file (or from any file after it), and stops for you
with a message naming the file. To settle a refused file: with the adapter
stopped, move the file out of the run's folder (keep it, it is evidence;
never edit it), run the data job again (`sudo systemctl start ti-data`),
which writes the sessions again from its recordings, and start the adapter
(`sudo systemctl start ti-adapter-boost`).

- **The file is not as the data job writes it**: a line that is not JSON or
  not complete, a field missing or extra, a kind of input, version or source
  the data job does not write, a line whose contents do not match its own
  checksum (`payload_hash`), one input named twice, line numbers that are
  not 1, 2, 3, ... in order, or a last line that is not the close of the
  session the file's name gives. Something other than the data job wrote or
  changed it. Every waiting file is checked before anything is written down,
  so a refused file leaves nothing behind in the outbox.
- **Two waiting files overlap**: a second file for the same session (a copy
  under another name, say), a file whose first session is not after the
  last session of the file before it, or two files naming the same input.
  One input may repeat: a momentum stream's calendar reaches back to the
  run's start, so every file writes each recorded session retraction
  (`market.session.retracted`) again under the same name. A repeat with the
  same contents is sent once; one whose contents differ is refused.
  The message names the later file; move it out as below (the earlier one
  is then delivered).
- **The file is not this run's** ("it is not this run's"): it carries
  another run's name, configuration hash or build than the engine now
  running. A file copied from another run's folder, or a run restarted under
  another configuration or build without a fresh `TI_RUN`.
- **A file older than one already delivered** appears: the sessions would go
  to the engine out of order. A file put back by hand, or a clock problem.
- **The run's folder does not exist** when the adapter starts: the warm-up
  has not been written for this `TI_RUN`. The adapter refuses to start and
  names the warm-up command. Run the warm-up ("The one-time warm-up",
  above), then start the adapter.
- **The run's stream folder cannot be found or told apart** (the message
  begins "the run's stream folder is found by the run's identity, from its
  durable log", and says the log cannot be read or that no earlier folder is
  a whole copy of this run's history): the adapter stops and delivers
  nothing, since a wrong folder could feed the engine another run's days.
  Check the engine's log is whole and readable, then move the unmatched
  folder aside as the message says and start the adapter again.
- **The kept bases file does not read** (`pending.json.basis`, a warning in
  the adapter's log naming it): nothing stops. The adapter walks the earlier
  runs again to rebuild the previous close and replaces the file; it is only
  a saving of work. An earlier run's log that cannot be read, or whose record
  hash chain is broken, does stop it, naming the log: restore it from the
  backup as the engine wrote it.
- **The `.delivered` file cannot be read**: something other than the adapter
  changed it. The adapter stops before delivering anything. Restore it from
  the backup as it was ([server](server.md), "Backups"); never edit it.
- **The link to IB Gateway drops while the snapshot is read**: nothing is
  written down or sent. Once the link is back and matched, the adapter takes
  a fresh snapshot and hands the file over then.
- **The reading cannot stand for the close**: it was read before the
  close, or read on a later New York date than the close without the Flex reader's executions proof reaching the midnight that starts the reading's day (no proof, one set aside, or a statement ending too early), with an execution at or after the close in that proof, a fill journalled at or after the close, or read at or after the next session's open ([reconciliation](reconciliation.md), "The executions proof"); IBKR shows an open order for the account from any
  client (working, pending or cancel-pending); IBKR's executions could not
  be read in full, or one is at or since the close; a holding has no
  official close in the file; IBKR's own net liquidation is not above zero;
  the holdings are not the previous close's plus or minus the fills
  recorded since; the cash is more than 1% of the net liquidation at the
  close (IBKR's own at the read when the account cannot be valued at the
  close) from the previous close's adjusted by those fills; or the engine
  already holds a snapshot as of this close or later (the conditions
  above). This stops nothing, except for a run's first reading that is not
  the book it was compared with: that one sends no snapshot, only its
  reconciliation, and the account **halts** as unverifiable; the remedy is
  a fresh run whose approval states the book you read in IBKR
  ([reconciliation](reconciliation.md), "What can go wrong"). Otherwise
  the file goes, with both snapshots stamped at the read time and the
  reason in the broker snapshot (`not_as_of_close`); the engine withholds
  that session
  (`stale-account-snapshot`) and you are paged with the reason
  ([paper trading](paper-trading.md), "What can go wrong").
- **IBKR cannot give the whole account three times running** (the message
  begins "the account could not be read in full for the broker snapshot"
  and names what could not be read): nothing is written down or sent, and
  the adapter stops, since a snapshot missing a figure would hold back the
  sale back to 98% for nothing. Check IB Gateway's link (`journalctl -u
  ti-gateway`), and that the account holds only whole US-dollar shares and
  reports its balances in US dollars, then start the adapter again; the
  file is delivered then. This is not one of the refused-file cases: leave
  the file where it is.
- **The adapter was not running when the file was written** and starts
  later that morning: it delivers the file then, behind a reading that
  stands for the close only with the executions proof and only before the
  next open. Started after 09:30 it is withheld, and an order released for
  a day already past is never sent (the adapter stops on it:
  [paper trading](paper-trading.md), "What can go wrong"). Keep the adapter
  running on a paper day.
- **A fresh run with the old run's file still in the outbox** (you started a
  new `TI_RUN` before the old file was all sent): its snapshot and lines are
  dropped unsent together, in one write, with one log line, so a stop
  partway never leaves the lines without their snapshot; the new run's own
  warm-up carries
  the whole history again. The old run's files are then marked "dropped" in
  `.delivered`, not delivered, as they are when a stop left them written
  down but not yet in the outbox. If you later resume the old run (its own
  under a new `TI_RUN` with its log's copy, after a crash: [durable
  log](../runbooks/durable-log.md), and "A crash resume", above), the
  adapter hands its dropped files over again behind a fresh snapshot; any
  line its engine had already recorded is skipped, never sent twice.
- **More than one stream folder of one run** (a warm-up run for a resumed
  `TI_RUN` while the first run's log was missing, then the log restored,
  say: "A crash resume", above): the adapter refuses to start, naming both
  folders, and sends nothing. Move the folder that should not
  be there out of `streams/boost/`, keep it, and start the adapter again.
- **A run that only shares the identity of the run whose folder it finds**
  ("nothing shows it is that run's crash resume": a dry-run first run
  started again without an approval, a resume of a first run that committed
  only its configuration, or a log that is not a copy of that run's
  records: "A crash resume", above): the adapter refuses to start, naming
  both logs, and sends nothing. For the first two, start a fresh run with
  its own warm-up (a dry run needs no approval; a paper run needs its
  approval); for the third, resume the crashed run's
  own log with `engine -resume`.
- **A log beside a stream folder that cannot be read**: one whose first
  records cannot be read cannot be matched to any run. The adapter warns,
  naming its folder, and refuses to start ("cannot be matched to any run"),
  whether or not a folder of the running run's own name exists: after a
  crash resume, restore the first run's log from the backup; for an old
  run's log, once its run has ended, move the folder out of `streams/boost/`
  and its log out of the engine's log folder, beside the running log,
  keeping both as evidence. One whose first records name this run but whose later
  records cannot be read stops the adapter ("names this run in its first
  records but cannot be read"), and one whose stated record hashes are not
  the ones its records give stops it too ("does not follow its record hash
  chain"): restore that log from the backup as the engine wrote it.

## What can go wrong, and what the step does

**The daily job and the delivery, at a glance.** The detail and the steps
are in the sections named in the last column; the list after the table is
for the recording and encoding steps.

| What you see | What the program does | What you do |
| --- | --- | --- |
| `ti-data refused:` "no secrets file", "cannot read MASSIVE_API_KEY from the secrets file" or "MASSIVE_API_KEY is not set in the secrets file" | No file is written; the unit shows as failed. The key is never printed | Put the key on the `MASSIVE_API_KEY=` line of `secrets.env` (`sudo ti-secrets edit secrets.env`; the file is stored encrypted), then `sudo systemctl start ti-data` ("What can go wrong" under "The daily data job") |
| `ti-data refused:` "run ... has no stream yet: run the warm-up once first" | No file is written | Run the one-time warm-up with the date it names ("The one-time warm-up") |
| `ti-data refused:` the engine's durable log "cannot be read", "is not an engine durable log", "holds no committed configuration input: start the engine first", "holds no committed input, so it names no run", or the run's first input "is not its configuration" | No file is written | Start the engine once (so `run.env` and its log exist), check `TI_RUN`, and restore a log that cannot be read from the backup as the engine wrote it; never edit it |
| `ti-data refused:` "it is not a Boost configuration", "warm_up_start ... is not an NYSE session", "--backfill-from ... is not the configuration's warm_up_start", "session ... precedes the warm-up start", or "the Boost configuration ... is not the one the engine's durable log ... holds" | No file is written | Correct the date or put the configuration back as the engine started with it; an edited configuration is a new run |
| `ti-data refused:` "the history ... holds N sessions; the indicators need M", or Massive returns nothing for a trading day | No file is written; recordings already made are kept | For a history that is too short, choose an earlier `warm_up_start` (more sessions) inside Massive's two-year window; once the engine has started, a changed `warm_up_start` changes the recorded configuration, so it needs a new run. If Massive returned nothing for a day, let the next firing retry it instead |
| `ti-data refused:` "... differs from the stream its recordings and the engine log ... give now", or "cannot be encoded again from their recordings" | Nothing more is written for the run: the engine has already read the old file | "A file already written differs" under "The daily data job": usually a new run with its own warm-up |
| `ti-data refused:` "ConflictingActions: dividends <id> differs between recordings" | No file is written | Check the figure against the fund's notice and quarantine the recordings that hold the wrong one ("Two recordings give different figures for one dividend") |
| `ti-data refused:` "has more than one stream folder", "cannot be matched to any run", "names this run in its first records but cannot be read", "does not follow its record hash chain", or "a second start of a first run" | No file is written | Restore or move aside as "A crash resume" says; never run a warm-up for a resumed run's new name |
| The adapter stops: "the stream file ... is refused", "the run's stream folder ... does not exist", the same folder and log messages as the row above, or "the delivery record ... cannot be read" | Nothing from any file is sent | "What happens to the file", "What can go wrong, and what the adapter does" |
| The adapter stops: "could not be read in full for the broker snapshot", or "the reconciliation owed ... could not be read" | Nothing is written down or sent; the file stays | Check IB Gateway's link and the account's currency, then start the adapter again |
| You are paged: a decision withheld as stale-account-snapshot, "The adapter's reading after the close could not stand for the close" | Both snapshots are stamped at the read time with the reason; the engine orders nothing for that session. A run's first reading that is not your starting book instead halts the account as unverifiable | "The reading cannot stand for the close" under "What happens to the file" |

**The recording and encoding steps.** Every failure writes nothing and
exits with status 1 and a message on stderr, for the scheduler to alert on.

- **The session has not settled.** A session is refused until an hour after
  its close (`--settle-minutes`, default 60), so an intraday price is never
  recorded as a close.
- **Massive returns nothing on a trading day.** Treated as missing data, not a
  holiday; nothing is recorded and the next run tries again. Closed days come
  only from the NYSE calendar.
- **Massive refuses for rate (HTTP 429) repeatedly, repeats a page, or
  redirects somewhere other than Massive.** Refused. A redirect elsewhere
  would carry the key, so it is never followed.
- **A recording is missing for a trading day** (a gap), or holds no bars.
  The backfill, history reader and encoder all refuse rather than skip it.
- **A required instrument has no bar.** Refused, naming it and the sessions.
- **The split or dividend recordings do not cover the sessions**, or two
  recordings hold different copies of one action. Refused, naming the first
  uncovered day or the action.
- **The whole-market and universe recordings of one session disagree.**
  Refused.
- **A bar or row is malformed**: a missing or repeated ticker, a price that is
  not a positive number, a high below the close, a volume below zero. For a
  stock written only as a universe member, or listed only in a universe
  snapshot, a bad bar or row or an adjustment that cannot be done leaves that
  stock out of the sessions it reaches instead (see above).
- **An adjustment cannot be done exactly**: a dividend whose previous session
  has no close for that stock, total cash on an ex-date at or above that
  close, a split and a dividend on the same date, two splits on one date, or
  a ratio or cash figure that is not a finite number of the right sign.
- **Sector recordings are missing or more than 7 days old** for a universe
  session. Refused.
- **A sector source redirects off HTTPS or off its two hosts.** Refused at
  every hop, not only the first; each recording names the URL its body was
  actually read from.
- **Sector labels for a past session**: `sectors` refuses a session that is
  no longer the latest one, and the encoder refuses a sector recording
  fetched before its session's close or after the next session's close.
- **A boost stream asks for more than one instrument.** Refused.
- **The ranker fails, cannot be run, or answers something that does not fit
  the request** (another session, a stock not in the snapshot, a stock named
  twice, stocks for a session that is not a decision session, no window),
  or does not answer within 60 seconds. Refused, naming the session
  ("universe 2026-11-30: the ranker refused: ...").
- **A bar or sector recording disappears or is replaced while `encode`
  runs.** Refused when it happens before one of the file's reads ("...
  disappeared during the encode", "... changed during the encode"); nothing
  is written. Encode again once the recordings are settled. A file replaced
  while it is open being read is read as listed. A recording that only
  appears meanwhile is not read (see "Memory the compact stream needs").
- **A cancelled session has price recordings.** Refused: either the day
  traded or the retraction is wrong; find out which before encoding again.
- **A retraction for a day that is not a session by the rules, without a
  reason, or recorded twice.** Refused.
- **The NYSE calendar does not reach the year asked for** (it covers
  2022-2030). Refused; extend it and check it against NYSE's published
  holidays and early closes.
- **A recording fails to flush to disk after it is written.** The step says
  the file is recorded but may not survive a power cut; check it reads, and do
  not record it again.

## What backs it

- ADR 0026, section 8 (the market-data step), ADR 0018 (durable files),
  ADR 0015 (event and schema versions) and plan.md section 4 (sectors).
- `adapter/marketdata/README.md` lists the commands and flags.
- Tests in `adapter/marketdata/tests/`: `test_encode.py` (stream shape,
  adjustment, universe rows, every refusal above, byte-identical replays
  with the network disabled), `test_forward_adjust.py` (the forward form
  agrees with the backward one, including two dividends on one ex-date and a
  split dated on a non-trading day just before an ex-date),
  `test_history.py`, `test_marketdata.py`, `test_nyse_calendar.py`,
  `test_sectors.py` and `test_tidata.py` (the daily job: warm-up, daily and
  catch-up streams the Go validators accept, files numbered from 1, every
  refusal above including a late dividend inside a delivered file, the
  byte-identical second run, the key never printed, every timer firing
  against the settle time, the unit's timeout and checks, a durable log the
  Go engine wrote (`tests/testdata/boost-fixture.log`, written by
  `cmd/engine/data_job_log_fixture_test.go`), and the packed file: refused
  from a dirty tree, the same bytes for one commit, stamped with it). All use
  made-up data.
- `adapter/marketdata/tests/testdata/envelope_contract.go` decodes the encoded
  stream with the Go engine's own validators, refusing unknown fields, so a
  stream the engines would refuse fails the Python tests first.
- The compact stream and retractions: `test_compact.py` (the stream's shape,
  a count per session showing no input is written per listed stock, each
  history against the closes the full stream's bars carry, bad data, every
  refusal, the CLI, the ranker asked one session at a time, and the streamed
  file byte for byte the stream `encode` returns, a recorded fixture whose
  stream must have the bytes the encoder wrote before it read one session
  at a time, with a stock ranked, dropped and ranked again keeping its whole
  window, recordings changed while it runs, an overflowing adjusted close,
  and a ranker that refuses or hangs), `test_memory.py`
  (`MomentumMemoryBudgetTest`: the momentum warm-up and daily firing inside
  the 64 MiB budget, and the fixture inside 19 MiB, on 2,000-stock sessions,
  every history its whole window; `MomentumStressMemoryTest`: bad bars for
  half the market every session, and a ranked set that changes completely
  every month, each adding under 1 MiB), and with the real Go ranker the Go validators and
  `tests/testdata/decision_equivalence.go`, which feeds the full and the
  compact stream of the same recordings through two fresh momentum engines
  and requires identical decisions. The engine side is in the momentum
  universe chapter, [momentum-universe](momentum-universe.md).
- The IRAs' data job: `test_tidata_momentum.py` (the job's warm-up and its
  next morning's file byte for byte what `encode --ranker` writes, the
  daily file the compact stream's recorded bytes, the retracted session
  never fetched, a second firing fetching nothing; the holdings read from a
  synthetic engine log: none, the last snapshot's, a fill since, the
  starting book's, no flag, a snapshot, fill or approval of an unread
  version, a broken record hash chain; the latest session's universe
  recorded from the same bars as its whole-market recording, its sector
  labels, a session with no labels left without a universe and the next
  morning carrying on, the month-end universe written by the catch-up after
  a missed month-start; a recording another job writes first; a missing
  file in the run's chain; the ranker reading the checked configuration;
  exclusions named; a holiday month-end decided by the real Go ranker; the instance
  unit and its timer against Boost's), `test_memory.py`
  (`test_the_iras_data_job_stays_inside_the_budget`: the job itself, with the
  real ranker, on the two-year fixture, its daily firing reading a log of a
  year of daily universe snapshots), `BoostBytesTest` in
  `test_tidata.py` (Boost's files keep their bytes), and the rehearsal
  (`deploy/rehearse.sh`: the instance refused while the secrets are locked,
  paging as `ti-data@trad-ira.service`, waiting before the first run, and a
  warm-up and a second firing as its unit runs them, on synthetic
  recordings, with the installed ranker).
