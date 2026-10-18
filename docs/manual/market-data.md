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
because a split was left unadjusted. So name every stock you hold; one you
leave unnamed is treated as not held when its prices have a gap.

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
  `test_history.py`, `test_marketdata.py`, `test_nyse_calendar.py` and
  `test_sectors.py`. All use made-up data.
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
