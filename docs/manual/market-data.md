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
| `python3 -m marketdata encode --out DIR --first ... --asof ... --stream FILE ...` | the engines' input stream (below) | no, and it never uses the network |
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
  close.

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
`--universe-members`. The account snapshots and marks Boost also reads come
from the broker side, not from this step.

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
the engine's own code for all three. The engine then ranks the snapshot again
itself and refuses the stream if the histories name any other set of stocks,
so a mistaken or stale ranker cannot slip a different universe in.

In the compact stream the calendar starts at `--base` rather than `--first`,
because the histories reach back to the base and the engine refuses a close
on a day its calendar does not list. A decision session with no universe
recording gets no histories, and the engine withholds that month's decision
(`missing-universe`) and alerts, rather than decide from part of the market.

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
weekday, with no one at the keyboard: `ti-data` (`adapter/marketdata/tidata.py`,
installed as `/opt/trend-investing/bin/ti-data` and run by `ti-data.service`).

**When it runs.** At 17:05, 18:05 and 20:05 New York time, Monday to
Friday (`ti-data.timer`). The market closes at 4 p.m. (1 p.m. on an early
close), and a session is not recorded until an hour after its close, so its
prices are final; 17:05 is after that hour on every trading day, and a test
checks every firing of the timer against the service's `--settle-minutes 60`
for every session of 2026 and 2027. The 18:05 and 20:05 firings are retries:
if 17:05 failed (Massive down, too many requests), the same evening tries
again; once the day's file exists they find it and fetch nothing. On a
holiday it finds the last session already written and does nothing. If the
server was off, the run happens when it starts again, and catches up every
session it missed in one file. A run that hangs is stopped after an hour
(`TimeoutStartSec=1h`) and shows as failed, like any other refusal.

**What it does, in order.**

1. Picks the latest settled session.
2. Reads the Boost engine's durable log (the `-log` file of the run named by
   `TI_RUN` in the engine's `run.env`) for the run's configuration hash and
   strategy version, and uses the run's name as the stream's correlation id,
   so the stream carries exactly the identity the engine expects. The log's
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
sudo systemd-run --wait --pipe --collect -p User=ti-trader -p Group=ti-trader \
  -p LoadCredential=secrets:/etc/trend-investing/secrets.env \
  /opt/trend-investing/bin/ti-data --data /var/lib/trend-investing/data \
  --config /etc/trend-investing/engines/boost.config \
  --engine-log /var/lib/trend-investing/engines/boost/<TI_RUN>.log \
  --backfill-from <warm_up_start>
```

It records every session from that date (resumable: a second try fetches
only what is missing), checks there is no missing day and at least as many
sessions as the longest indicator window (200 for the trend average), and
writes the run's first file, holding the whole history. Any other date is
refused, because the indicators are seeded from the configured one.

Massive's free plan reaches back two years only, so the paper Boost
configuration's `warm_up_start` must lie within the last two years. The
shipped dry-run file says 2024-01-02, which is already outside that window.
In your paper copy (`docs/runbooks/first-paper-order.md`, A5) set
`"warm_up_start": "2025-01-02"`, the recommended value: it is a session, it
is within the window, and it leaves well over the 200 sessions the trend
average needs before the first paper day. The choice is yours; any session
inside the window with at least 200 sessions before the first paper day
works. 2025-01-02 stays inside the window until 2 January 2027; a warm-up
run after that needs a later date. It changes the paper configuration's hash only (the paper run is a
new run anyway); the shipped dry-run file and its runs are unchanged.

A new engine run (a fresh `TI_RUN`) starts with no files and needs the
warm-up again; the recordings are already on disk, so it fetches only the
days since.

**A crash resume.** After a crash the engine is resumed under a new
`TI_RUN` ([durable log](../runbooks/durable-log.md), "Resume after a
crash"): a new log that holds a copy of the crashed run's records. It is the
same run, not a new one, so it needs **no** warm-up and gets no folder of
its own. The data job and the adapter both find the run's folder by the
run's identity, not by the log's name: its Run approval (or, for the
engine's very first run, its configuration and as-of time), which the
copied log carries. The folder is the one named after the run that began
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
data job's other inputs, and the adapter's broker snapshots; an operator input such as a kill switch, the
approval and the configuration never count) holds copied history, which a
fresh run's log cannot, so the message says "the log <path> already holds
N committed delivered inputs beyond its configuration", N counting delivered
inputs only (market data and the adapter's broker snapshots), that if it is a crash resume
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
(`/var/lib/trend-investing/engines/<account>/`, beside the running log),
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

- **the engine's first run started again without an approval** (same
  `run.env`, same configuration and as-of time): it has the first run's
  identity but not its records, and is never handed the first run's
  folder, record of files sent and stream without its own warm-up. Stop
  it, and start it as a fresh run, with its approval and its warm-up
  ([paper trading](paper-trading.md), "Never start the engine's first run
  again without an approval");
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
is never skipped: before the job is installed, or before the Boost engine has
been started once (no `run.env`), each firing fails with a message saying
which.

**What can go wrong.** In every case no file is written for the session,
so the engine never reads half a day. Recordings already made are kept, and
the next run (or `sudo systemctl start ti-data` once the cause is fixed)
carries on from them.

- **No Massive key**: the line `MASSIVE_API_KEY=` in
  `/etc/trend-investing/secrets.env` is empty or missing. The key is never
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
- **The engine has not been started yet** (no `run.env`: the unit fails
  before the job runs, naming the file), or **its durable log cannot be
  read**, or its first input is not a Boost configuration.
- **The configuration file and the engine's log disagree**: the file was
  edited after the engine started. Put the file back as the engine started
  with it; an edited configuration is a new run.
- **Massive returns nothing for a trading day**, or a recording from an
  earlier day is missing: the run names the day. A missing earlier day means
  a recording was removed; restore it from the backup (today that means a
  Lightsail snapshot, if snapshots are on: [server](server.md), "Backups").
- **A history shorter than the indicators need** at the warm-up.
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

### What happens to the file

The file is the engine's input for those sessions, and the Boost adapter
(`ti-adapter-boost`) carries it there. Nothing else reads it.

**When.** The adapter looks in the run's folder,
`/var/lib/trend-investing/data/streams/boost/<TI_RUN>/`, on every turn of
its loop (about once a second) while its link to IB Gateway is up and
matched. A new file is picked up within a second or two of the data job
writing it, which on a normal day is a few minutes after 17:05 New York.
If the adapter is not running then, it picks the file up when you next
start it. If several files are waiting (the adapter was stopped for some
days), it takes them all, oldest first.

**What it sends, in order.**

1. **The broker snapshot.** Just before the file goes, the adapter reads
   the account at IBKR (positions, cash, net liquidation, Excess Liquidity
   and every open order) and asks IBKR for SPY's last traded price, which
   is the "mark" the snapshot gives each holding. It also says whether the
   market was open at that moment (by the NYSE calendar the data job uses:
   9:30 to 4 p.m. New York, 1 p.m. on an early close). After 17:05 that is
   always "closed". A price counts only if IBKR's answer for it finished in
   time (IBKR finishes it at the end of an 11-second window, so the adapter
   waits up to `mark_timeout`, which the configuration sets and the
   example sets to 15 seconds) and every last price in it was a real price above zero; otherwise
   the holding is listed with no mark ("mark set false"), as it is when IBKR
   gives no last price at all (the paper login has no market data for SPY,
   say). The engine then cannot size the joint account's emergency sale
   back to 98% from that snapshot, holds that sale back and asks you to act
   by hand. This mark is read only by that sale. A buy is checked against a
   different mark, one recorded at or after the session's close
   (`market.mark.recorded`), and is refused without one (`missing-mark`,
   which leaves the account Degraded). Nothing records that mark today, so
   every buy is refused that way in paper; where it will come from (IBKR's
   last price, as here, or the data job) is still your decision
   ([paper trading](paper-trading.md), "Not built yet").

   The account read and the marks are one reading. The adapter first
   applies every report IBKR has already sent, so a fill or a cancellation
   received before the read reaches the engine ahead of the snapshot; it
   does not read while a fill waits for its commission; and after the marks
   it reads the account a second time. If a fill or an order report
   arrives between the start of the first read and the end of the second
   (an execution of any client's order counts, not only the engine's), if
   the holdings, open orders or cash differ between the two reads (a trade
   you placed in IBKR's own app, which IBKR reports to that app only, still
   shows there), or if any part of the account could not be read (IBKR
   timed out on the balances, say), it reads the whole account again, up
   to three times. The snapshot is the second read, with its IBKR time.
   IBKR's answers for the positions and the open orders carry no request
   number, so if IBKR does not finish either list in time, or Gateway
   loses IBKR's servers part-way through (it restores them on the same
   link), a late end of it
   could be taken for the end of the next read's list on the same link:
   the adapter then drops the link instead of reading again on it, and
   takes the snapshot once the link is back and matched (`ibkr broker
   snapshot: IBKR had not ended its ...` in its log). The log line `ibkr
   broker snapshot as_of=... cutoff: ...` names the last report recorded
   before each snapshot.
2. **The file, line by line, as written**: each session's SPY bar, then that
   session's close. The engine therefore records the snapshot before the
   bars, and each session closes only after its bar, before anything for
   that day is decided (ADR 0019, ADR 0021). Each line goes with the
   engine's next input number in place of the file's own (the file format,
   above), and with the time it is sent as its "recorded" time, as every
   input the adapter sends, so the snapshot is never recorded later than
   the close it serves while the server's clock keeps time with IBKR's
   (ADR 0019 measures the snapshot's age at the close's recorded time, from
   the snapshot's IBKR time); nothing else in it changes. If the server's
   clock is behind IBKR's by more than the moment it takes to send the
   file, the snapshot's IBKR time is later than the close's recorded time:
   the engine counts that negative age as stale (ADR 0019) and blocks
   every sell at that close (reason `stale-broker-snapshot`) rather than
   trust it: the sale back to 98% is withheld (`risk.sale.withheld`), and
   any other sell, such as an ordinary strategy sale, is refused
   (`risk.order.refused`). Either at every close is the sign: check the
   server's clock (`timedatectl`) against IBKR's. A fill or a cancellation IBKR reports
   while the engine works through the file goes to the engine before the
   file's next line, so no session is decided on an account behind a
   report already received. A trade IBKR has reported but whose commission
   has not come yet holds the file's next line back until the fill is
   recorded and sent; if the commission never comes, the adapter stops
   after `commission_wait` seconds, as it always does
   ([paper trading](paper-trading.md), "What can go wrong").

Several waiting files go behind one snapshot, oldest first. The next day's
file gets its own snapshot.

**Exactly once.** Before anything is sent, the adapter writes down which
files it is about to hand over, beside its pending file
(`/var/lib/trend-investing/adapters/boost/pending.json.delivered`). It then
puts the snapshot and every line of the files in its outbox in one write,
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
broker snapshot, the bar and the session close, and whatever the engine
decided at that close. Never edit the `.delivered` file; the adapter reads
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
- **The `.delivered` file cannot be read**: something other than the adapter
  changed it. The adapter stops before delivering anything. Restore it from
  the backup as it was ([server](server.md), "Backups"); never edit it.
- **The link to IB Gateway drops while the snapshot is read**: nothing is
  written down or sent. Once the link is back and matched, the adapter takes
  a fresh snapshot and hands the file over then.
- **IBKR cannot give the whole account three times running** (the message
  begins "the account could not be read in full for the broker snapshot"
  and names what could not be read): nothing is written down or sent, and
  the adapter stops, since a snapshot missing a figure would hold back the
  sale back to 98% for nothing. Check IB Gateway's link (`journalctl -u
  ti-gateway`), and that the account holds only whole US-dollar shares and
  reports its balances in US dollars, then start the adapter again; the
  file is delivered then. This is not one of the refused-file cases: leave
  the file where it is.
- **The adapter was not running at 17:05** and starts the next morning: it
  delivers the file then, and an order the engine releases at that close is
  for a session already past, so it is never sent (the adapter stops on it:
  [paper trading](paper-trading.md), "What can go wrong"). Keep the adapter
  running through the evening of a paper day.
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
  ("nothing shows it is that run's crash resume": the engine's first run
  started again without an approval, a resume of a first run that committed
  only its configuration, or a log that is not a copy of that run's
  records: "A crash resume", above): the adapter refuses to start, naming
  both logs, and sends nothing. For the first two, start a fresh run with
  its approval and its own warm-up; for the third, resume the crashed run's
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

Every failure writes nothing and exits with status 1 and a message on stderr,
for the scheduler to alert on.

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
  twice, stocks for a session that is not a decision session, no window).
  Refused, naming the session.
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
  refusal, the CLI), and with the real Go ranker the Go validators and
  `tests/testdata/decision_equivalence.go`, which feeds the full and the
  compact stream of the same recordings through two fresh momentum engines
  and requires identical decisions. The engine side is in the momentum
  universe chapter, [momentum-universe](momentum-universe.md).
