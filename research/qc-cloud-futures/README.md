# Faith's original Turtle rules, on futures -- QuantConnect Cloud research check

## What this is, and what it is not

This folder is a **free, research-only** check of a different question from
`research/qc-cloud`'s own. That folder checks the *equity-adapted Baseline*
(half Faith's Unit Volatility Fraction, long-only) against US stocks, and
finds it loses money there after realistic costs -- see its own README.md.
This folder checks Faith's **own, unadapted** system -- System 2, 55/20, his
own 1% Unit Volatility Fraction, traded long AND short -- on a diversified
**futures** portfolio, the market the Turtles actually traded [T p.10-11].
QuantConnect's Free plan includes futures data in the cloud, so this check
is free too.

This is **not** the production Go engine (`internal/strategy`,
`internal/sizing`, `internal/indicator`, `internal/fills`). It is a separate,
much simpler Python re-implementation of Faith's rules, built to run entirely
inside QuantConnect's browser-based IDE, on the Free plan, with no
locally-installed tools and no API key. It makes no claim of bit-for-bit
parity with the Go engine, and it is not `research/qc-cloud`'s own code
copied and modified -- it is a fresh implementation, generalised for both
directions and for futures' own contract mechanics from the start, and
`rules.py` here is not imported by, or the same file as, `research/qc-cloud/rules.py`.

**No market data is committed to this repository.** This folder is text
files only; QuantConnect supplies the price history when you run a backtest
on their servers. This work was done, and this README written, without
fetching any market data or any external documentation -- every QuantConnect
API name below is written from this repository's own prior verified use of
LEAN's Python API (`research/qc-cloud/main.py`, `adapter/lean/`) and this
project's best understanding of LEAN's documented continuous-futures
support, not from a live lookup. "Uncertain about the API", below, names
every place that could be wrong, and how to tell.

## The rules, and their citations

Every rule is Faith's own, from *The Original Turtle Trading Rules* (Curtis
Faith, 2003), cited by page as `[T p.N]`, transcribed via
`docs/methodology/Methodology_Analysis.md` Section 2. `rules.py`'s own
docstrings repeat every citation next to the code it governs.

- **System 2, 55/20** [T p.18-19, p.26]: entries on a strict breakout of the
  55-bar Entry Channel (either side); exits on a strict reversal through the
  20-bar Exit Channel (the opposite side). `rules.Channel`, `rules.is_breakout`,
  `rules.exit_channel_breach`.
- **N** [T p.13]: Wilder's 20-day average of True Range, seeded with a
  simple average. `rules.true_range`, `rules.WilderN`. Unchanged from the
  equity Baseline; N is a volatility measure, not an equity- or
  futures-specific one.
- **Unit sizing** [T p.14-15]: `quantity = floor(account x fraction / (N x
  dollars_per_point))`. `rules.unit_quantity`. `dollars_per_point` is read
  from each contract's own `SymbolProperties.ContractMultiplier`
  (`main.py`'s `_assign_mapped_symbol`), never assumed to be 1 as it is for
  shares.
- **Faith's 1% Unit Volatility Fraction, not the equity Baseline's 0.5%.**
  ADR 0003 halves Faith's figure "the cheapest available insurance" for "a
  long-only equity book in a single regime [that] lacks the cross-asset
  diversification the futures Turtles had." This script trades the
  diversified, symmetric long-and-short futures book that argument is
  explicitly *about* -- the population Faith's 1% describes -- so
  `rules.UNIT_VOLATILITY_FRACTION` defaults to Faith's own **0.01**.
  `TurtleFuturesResearch.UNIT_VOLATILITY_FRACTION` (`main.py`) is a class
  attribute; set it to `0.005` to run the equity Baseline's own fraction
  here instead, for a direct comparison.
- **2N stops, ½N Adds, 4 Units per market** [T p.19-23]: `rules.protective_stop_level`,
  `rules.next_add_level`, `rules.raised_stop`, `rules.Campaign`, all
  direction-aware (a `direction` parameter, +1 long / -1 short) since
  Faith's own text is written for a long and this script generalises it
  symmetrically for a short (documented in each function's own docstring
  as this script's own reasoned mirror image, never claimed as a separate
  disclosed rule). Frozen Campaign N and Unit size at entry, unchanged from
  ADR 0006's own reasoning (not an equity-specific argument).
- **Faith's limits: 6 closely correlated, 10 loosely correlated, 12 per
  direction** [T p.16]: `rules.UnitCaps`, `rules.MAX_UNITS_PER_MARKET` (4),
  `MAX_UNITS_PER_CLOSELY_CORRELATED_GROUP` (6),
  `MAX_UNITS_PER_LOOSELY_CORRELATED_GROUP` (10), `MAX_UNITS_PER_DIRECTION`
  (12). Every cap is tracked **per direction**, reading Faith's own
  "single direction: all long or all short" [T p.16] as applying at every
  level, not only the total: a fully-Loaded long book leaves the short
  side's headroom untouched (`test_rules.py`'s
  `test_long_and_short_caps_are_independent`).
- **Long AND short, symmetric, with a long-only comparison switch**: the
  task's own requirement. `TurtleFuturesResearch.LONG_ONLY` (`main.py`,
  default `False`) skips every short breakout when set `True`.
- **Strength ranking, symmetric** [T p.27-29]: "buy the strongest / sell the
  weakest ... within a correlated group." `rules.strength`,
  `rules.rank_signals(signals, direction)`: for a long, the most positive
  Strength first; for a short, the most negative first. Faith does not
  disclose how a simultaneous long Signal in one market and a short Signal
  in another are ordered against each other; this script decides every long
  candidate first, then every short candidate -- its own considered
  simplification, not a disclosed rule.
- **The drawdown rule** [T p.17]: `rules.NotionalAccount`, unchanged from
  ADR 0007 (Faith's own numbers, not equity-adapted).
- **The price cap and slippage** (ADR 0005's amendment, ADR 0013):
  `rules.price_cap`, `rules.slippage`, `rules.stop_limit_fill_price`, all
  generalised symmetrically for a short entry/Add (a stop-limit *sell*)
  exactly as the long case ADR 0005 states, mirror-imaged.

## The market list

A diversified set close to the Turtles' own [T p.10-11], via
`main.py`'s `CORRELATION_GROUPS` (ticker -> display name, closely-correlated
group, loosely-correlated group). Root ticker symbols (CME/CBOT/ICE market
convention, not any one vendor's naming) are passed directly to
`AddFuture`, sidestepping the need to know QuantConnect's own `Futures.*`
nested constant class names exactly (see "Uncertain about the API"):

| Ticker | Market | Closely-correlated group | Loosely-correlated group |
| --- | --- | --- | --- |
| ZN | 10-Year U.S. Treasury Note | rates | rates |
| ZB | 30-Year U.S. Treasury Bond | rates | rates |
| 6E | Euro FX | currencies-europe | currencies |
| 6S | Swiss Franc | currencies-europe | currencies |
| 6J | Japanese Yen | currencies-jpy | currencies |
| 6B | British Pound | currencies-gbp | currencies |
| 6C | Canadian Dollar | currencies-cad | currencies |
| 6A | Australian Dollar | currencies-aud | currencies |
| GC | Gold | metals-precious | metals |
| SI | Silver | metals-precious | metals |
| HG | Copper | metals-base | metals |
| CL | Crude Oil WTI | energy-petroleum | energy |
| HO | Heating Oil (NY Harbor ULSD) | energy-petroleum | energy |
| NG | Henry Hub Natural Gas | energy-natgas | energy |
| ES | S&P 500 E-mini | equity_indices | equity_indices |
| ZC | Corn | grains | grains |
| ZS | Soybeans | grains | grains |
| ZW | Chicago SRW Wheat | grains | grains |
| SB | Sugar No. 11 | softs | softs |
| KC | Coffee C | softs | softs |
| CC | Cocoa | softs | softs |
| CT | Cotton No. 2 | softs | softs |

That is 22 markets across the seven named correlation categories (rates,
currencies, metals, energy, grains, softs, equity indices) -- close to
Faith's own roughly 21 [T p.10-11]. Grains and softs are listed "if
available": `Initialize` wraps every
`AddFuture` call in a `try`/`except` and simply skips a market QuantConnect's
data does not carry, logging it and reporting the skipped list under the
`Markets unavailable` runtime statistic -- the run does not fail because one
market is missing.

### Correlation groups: what is disclosed and what is this script's own choice

Faith discloses three finer examples directly [T p.16]: heating oil/crude,
gold/silver, and CHF/DEM (the Swiss franc and the pre-EUR German mark, both
European currencies) as closely correlated; T-bill/Eurodollar as an example
of short-rate correlation. This script's `energy-petroleum` (crude +
heating oil), `metals-precious` (gold + silver), and `rates` (the note and
bond kept in one closely-correlated group, generalising the T-bill/Eurodollar
example along the curve) transcribe those directly. `currencies-europe`
(EUR + CHF) substitutes EUR for the pre-EUR DEM as the modern equivalent.
Every other closely-correlated group in the table above -- JPY, GBP, CAD,
and AUD each alone; `metals-base` (copper alone); `energy-natgas` (natural
gas alone, decoupled from oil since the shale era); `grains` and `softs`
each as one undivided group -- is this script's own **considered, unverified**
choice, made because Faith's text does not name a finer split for them.
This is the same kind of caveat `research/qc-cloud/README.md`'s own
Morningstar-derived classification carries for equities: a reasoned mapping,
not one this repository can verify without a live backtest.

## Continuous futures and roll handling

Each market is subscribed with `AddFuture(ticker, Resolution.Daily,
dataMappingMode=DataMappingMode.LastTradingDay,
dataNormalizationMode=DataNormalizationMode.BackwardsPanamaCanal)` and
`future.SetFilter(0, 182)` (`main.py`'s `Initialize`):

- **`DataMappingMode.LastTradingDay`**: LEAN rolls the mapped (tradable)
  contract forward on the front contract's own last trading day. A simple,
  deterministic choice; QuantConnect's other modes (`FirstDayMonth`,
  `OpenInterest`, `OpenInterestAnnual`) roll on a different schedule and are
  a declared, straightforward Variant to try if the first run's roll
  frequency or cost looks wrong.
- **`DataNormalizationMode.BackwardsPanamaCanal`**: additive back-adjustment.
  True Range and the Donchian channels are absolute price *differences*; an
  additive adjustment preserves those differences across a market's own many
  rolls over a multi-decade run without compounding a ratio at each one, the
  way `BackwardsRatio` would. This is the reasoning behind the choice, not a
  verified comparison run.
- **`future.SetFilter(0, 182)`**: keeps LEAN's continuous-contract chain
  mapped into contracts within 182 days of expiry, the value QuantConnect's
  own continuous-futures examples use.

**Trades hold the mapped contract, never the canonical continuous symbol.**
Every `_SymbolState` is keyed by the
*canonical* continuous `Symbol` (so N, the channels, and Strength stay
continuous across every roll, unaffected by which dated contract is
currently tradable), but every order (`StopLimitOrder`, `StopMarketOrder`,
`MarketOrder`) is placed on `state.mapped_symbol`, the actual dated contract
`future.Mapped` currently names.

**Detecting a roll: polling `future.Mapped`, not `SymbolChangedEvents`.**
QuantConnect's documented mechanism for a continuous-future roll is a
`SymbolChangedEvent` in `slice.SymbolChangedEvents`. This script instead
reads `future.Mapped` directly, once per Session, at the top of every
`OnData` (`_refresh_mapped_symbols`), and treats any change from the
previous Session's reading as a roll. This is a deliberately simpler,
self-correcting design that does not depend on this repository's ability to
verify `SymbolChangedEvents`' exact shape without a live run, and it also
naturally covers the *first* assignment of a mapped contract (which
`SymbolChangedEvents` would not fire for, since nothing "changed" from
nothing). If the first cloud run shows `future.Mapped` does not update
promptly on a roll day, `slice.SymbolChangedEvents` is the documented
fallback to switch to.

**Handling the roll** (`main.py`'s `_roll_market`): every resting entry/Add
order on the old contract is cancelled; if a Campaign is open, its WHOLE
position (every open Unit, at the same total Unit count) is closed on the
old contract and reopened on the new one with one `MarketOrder` each --
never per-Unit, a deliberate simplification to keep the roll's own
commission and order count small and to keep this script's own code simple,
since Faith's Units are a position-*sizing* construct, not an execution
requirement that the roll transaction itself must preserve. Each Unit's own
resting Exit Order is then re-placed on the new contract at its existing
(unchanged) stop level -- the stop levels themselves need no translation
between contracts, because the back-adjusted continuous series and the
currently-mapped contract's own raw price are, by the back-adjustment
convention, identical for the *current*, most-recent segment (the
adjustment is pushed into the historical segments behind older rolls, never
into the present one). **This is an assumption this repository cannot
verify without a live run** (see "Uncertain about the API").

**Roll cost is counted**: the commission on
both the closing and the reopening market order is added to `roll_commission`
and reported as the `Rolls` runtime statistic (`"<count> commission=<total>"`),
separately from `Commission`, the ordinary trading total. A roll order is
never counted as an entry, Add, or exit for `rules.Campaign`'s own
bookkeeping -- `main.py`'s `order_kind` tags it `"roll"` and
`_handle_order_event` returns once its fee is booked, touching no strategy
state.

## Long, short, and Unit caps by direction

`rules.UnitCaps` keys every one of Faith's four caps by `(symbol,
direction)` or `(group, direction)`, so a long Campaign in Crude Oil and a
short Campaign in Crude Oil are tracked as entirely separate exposures, each
against its own 4-per-market cap, and the 12-per-direction cap binds a
fully-Loaded long book independently of the short book (test_rules.py's
`test_long_and_short_caps_are_independent`). `rules.SessionCapLedger`
reserves headroom for every proposal decided within one Session's pass, so
two proposals that would each fit a cap alone cannot together exceed it
(mirrors ADR 0008/0020's own equity reasoning, applied here to Unit caps
only -- see "Deviations" below for why cash/margin affordability is not
similarly modelled in `rules.py`).

## Start date

`TurtleFuturesResearch.START_DATE` is set to `(1998, 1, 1)` -- the same
provisional figure `research/qc-cloud` uses for its own equity history --
**as a placeholder, not a verified figure**. This repository has not run a
QuantConnect backtest and cannot query QuantConnect's own futures data
coverage without network access, which this work was expressly asked not to
do. **On the first cloud run, check each market's own actual data start**
(QuantConnect's own project data explorer, or simply how far back each
`AddFuture` subscription actually delivers bars) and set `START_DATE` to the
true earliest date the run's own markets support, then report that date
back rather than trusting this placeholder. A market whose own history
starts later than the run's `START_DATE` is not a failure: `WARMUP_BARS`
and the ordinary evaluate-then-add discipline mean it simply is not ready to
trade (no breakout, no N) until it has accumulated enough of its own bars,
exactly as a stock added to the equity Baseline's universe mid-run is
handled there.

## Files

- `rules.py` -- the rule core. Pure Python, standard library only, no
  QuantConnect imports. Every function cites the analysis or the ADR it
  transcribes or symmetrically generalises.
- `test_rules.py` -- unit tests, written before `rules.py` (TDD), including
  golden tests transcribed directly from `Methodology_Analysis.md` /
  *The Turtle Rules*: the Heating Oil Unit-sizing worked example [T p.14-15]
  and the Gold and Crude Add ladders [T p.20], plus this script's own
  short-side mirror-image tests for each. Run with
  `python3 -m unittest discover -s research/qc-cloud-futures -p "test_*.py"`,
  or via `make research-test` from the repository root.
- `main.py` -- the thin `QCAlgorithm` that drives `rules.py` over the market
  list above.
- `build_upload.py` -- writes stripped copies of `main.py`/`rules.py` (every
  docstring and comment removed, via `ast`) to `dist/`, which fit
  QuantConnect's Free-plan 32,000-character-per-file limit (as built:
  `main.py` about 29,700 characters, `rules.py` about 14,900 -- run
  `python3 research/qc-cloud-futures/build_upload.py` to rebuild and see the
  current count). Standard library only; identical in mechanism to
  `research/qc-cloud/build_upload.py`.
- `dist/` -- `build_upload.py`'s output. Git-ignored: a build artifact, never
  the source of truth.
- `test_build_upload.py` -- checks both stripped files stay under the limit,
  both compile, and `test_rules.py`'s own suite passes against the stripped
  `rules.py`. Also run by `make research-test`.
- `README.md` -- this file.

## How to run it on QuantConnect

Follow `research/qc-cloud/README.md`'s own "How to run it on QuantConnect"
step by step, substituting this folder's files: build with
`python3 research/qc-cloud-futures/build_upload.py`, then paste
`research/qc-cloud-futures/dist/main.py` and
`research/qc-cloud-futures/dist/rules.py` into a **new** QuantConnect
project (Python), named e.g. `turtle-futures-research` -- never mix this
folder's files with `research/qc-cloud`'s own project, since the two are
independent checks with independent `rules.py` files. **This backtest needs
a Futures data add-on / subscription on your QuantConnect account** (check
your account's Data tab before running, and be sure it stays inside your
plan's free entitlement); if any market in the table above is not covered
by your account's own data access, `Initialize`'s own `try`/`except` skips it
and reports it under `Markets unavailable` rather than failing the whole
run.

## How to find the results

Identical mechanism to `research/qc-cloud`'s own "How to find the results":
read the **statistics panel**, not the Logs tab (the Free plan's 10 KB log
budget). The keys this script adds:

- `OVERALL` -- CAGR, max drawdown, CAGR ÷ max drawdown, over the whole run.
- `REGIME ...`, one per ADR 0012 Regime Window.
- `Campaigns` -- count, win rate, average win and loss, each in R (multiples
  of the Campaign's own initial 1-Unit risk).
- `Commission` -- total ordinary trading commission, in dollars, LEAN's own
  default Interactive Brokers futures fee schedule (see "Deviations": unlike
  `research/qc-cloud`, no custom fee model is written here).
- `Rolls` -- how many rolls occurred and their total commission ("roll
  cost"), reported separately from `Commission`.
- `Markets unavailable` -- which configured tickers this account's
  QuantConnect data did not carry, if any.
- `Declines` and one `Decline <reason>` key per reason.
- `SPY BUY-AND-HOLD` -- CAGR/max drawdown for buying and holding SPY
  (dividend-adjusted total return), over the identical span, as the
  cost- and effort-free comparison point.

## Deviations from Faith's text and from the equity Baseline

1. **Faith's own numbers, not the equity Baseline's adaptations.** Unit
   Volatility Fraction is 1% by default (not 0.5%); both directions trade
   by default (not long-only); Faith's own Unit caps (4/6/10/12) are kept
   exactly, per direction. See "The rules, and their citations" above.
2. **No cash/margin affordability model in `rules.py`.** Futures are
   margined (`SetBrokerageModel(...,
   AccountType.Margin)`). `research/qc-cloud`'s own `SessionLedger` reserves
   cash for a Cash equity account's own affordability check; this script
   reserves Unit-cap headroom only (`rules.SessionCapLedger`) and leaves
   margin affordability to QuantConnect's own margin model, which fails an
   order outright (`OrderStatus.Invalid`) if it cannot be margined --
   handled exactly like `research/qc-cloud`'s own anomaly path: the Unit-cap
   reservation is released, nothing else happens, and no cash amount is
   estimated in pure Python for a check this repository cannot price
   without a live margin model.
3. **No custom fee model.** `research/qc-cloud`'s equity script needed a
   custom `RawShareFeeModel` because its orders are stated in split-adjusted
   shares while LEAN's own fee model charges per RAW share. Futures carry no
   split/raw distinction (a contract is a contract), so this script relies
   on `SetBrokerageModel`'s own default Interactive Brokers futures fee
   schedule directly, with no override.
4. **The custom fill model targets `FutureFillModel`, not verified.**
   `research/qc-cloud`'s equivalent subclasses `EquityFillModel`, a class
   this repository has used before and confirmed works
   (`adapter/lean/orders.py`). `FutureFillModel` is this script's own,
   reasoned but UNVERIFIED guess at LEAN's equivalent per-security-type fill
   model class name for futures, following the naming pattern
   `EquityFillModel`/`ForexFillModel`/`OptionFillModel` this repository has
   observed. See "Uncertain about the API".
5. **Roll transactions are whole-position, not per-Unit**, and their
   commission is reported separately (`Rolls`) rather than folded into
   ordinary `Commission`, so a market that rolls often does not read as
   though it were trading often. See "Continuous futures and roll handling".
6. **Simultaneous cross-direction ranking is undisclosed and this script's
   own choice.** Faith states "buy the strongest / sell the weakest ...
   within a correlated group" [T p.27-29] but does not say how a
   simultaneous long Signal in one market and a short Signal in another are
   ordered against each other. This script decides every long candidate
   first, then every short candidate, each ranked within its own direction.
7. **No 20-day-median-dollar-volume tie-break.** The equity Baseline's
   ranking tie-break (ADR 0010's amendment) exists because a broad,
   liquidity-screened equity universe needs one; this script's fixed,
   small, always-liquid futures market list has no analogous eligibility
   screen, so ties in `rules.rank_signals` break by symbol alone.
8. **No `PartiallyFilled` accumulation machinery**, no split/dividend
   handling, no point-in-time universe eligibility screen, no classification
   input beyond the fixed `CORRELATION_GROUPS` table above -- all
   `research/qc-cloud` deviations that exist there specifically to handle
   equity-market mechanics (splits, dividends, a changing universe,
   fundamentals-derived sectors) this futures market list has no equivalent
   of. A `PartiallyFilled` order is still logged once, in full, and
   otherwise ignored exactly as `research/qc-cloud/main.py` documents,
   since the same ADR 0005/native-fill-model reasoning (an order fills its
   whole quantity or nothing) applies unchanged.
9. **Local LEAN has no futures data.** This repository's usual fidelity
   check (`research/qc-cloud/fidelity/`, comparing a local LEAN run against
   the Go engine's own journal) has no equivalent here: there is no Go
   engine futures implementation to compare against, and no local futures
   market data to run LEAN against even if there were. Only `test_rules.py`
   (pure Python, no LEAN) and `python3 -m py_compile` (syntax only, no
   QuantConnect imports resolved) can run locally; `make research-test`
   runs both. **The owner's first QuantConnect Cloud backtest is the real
   test** of every API call this file makes.

## Uncertain about the API

Everything below is written from this repository's own prior confirmed
LEAN usage and its best understanding of LEAN's documented Python API,
without a live QuantConnect session or network access to verify it. If the
first cloud run's compile step fails, look here first, in roughly the order
a compile error is likely to name them:

1. **`FutureFillModel`'s exact class name and namespace.** This script
   subclasses it (`main.py`'s `_stop_limit_fill_model`) by direct analogy
   with `research/qc-cloud`'s own confirmed `EquityFillModel` subclass. If
   QuantConnect's Python bindings expose it under a different name, the
   compile step will name it; substitute the correct class and keep the
   method bodies (`StopMarketFill`/`MarketFill`/`StopLimitFill`) unchanged,
   since their logic does not depend on the base class's name.
2. **`AddFuture`'s exact keyword names** (`dataMappingMode`,
   `dataNormalizationMode`, `extendedMarketHours`) and whether
   `DataNormalizationMode.BackwardsPanamaCanal` and
   `DataMappingMode.LastTradingDay` are the exact enum member spellings.
3. **`future.SetFilter(0, 182)`'s exact overload** -- whether it accepts
   plain integers (interpreted as days) or requires `timedelta`/`TimeSpan`
   values.
4. **`future.Mapped`'s exact property name and its timing** -- whether it
   is populated during `SetWarmUp`'s own historical delivery, and whether it
   reflects the NEW contract already on the very Session a roll occurs
   (`_refresh_mapped_symbols` assumes both).
5. **Whether `SymbolProperties.ContractMultiplier` is populated for every
   security type this script AddFuture's**, and whether it is available
   immediately once a contract becomes `Mapped` or only after its first bar
   arrives.
6. **Whether `StopLimitOrder` accepts a negative quantity** (a short
   entry/Add) with `limitPrice` below `stopPrice`, the configuration this
   script relies on for a short's stop-limit sell, symmetric to a long's
   stop-limit buy. `research/qc-cloud` has only ever exercised the long
   (buy) case.
7. **The back-adjustment continuity assumption** in "Continuous futures and
   roll handling": that the back-adjusted continuous series and the
   currently-mapped contract's raw price coincide for the present segment,
   so a Unit's stop level needs no translation across a roll. If the first
   run's equity curve shows an unexplained jump exactly on a roll date, this
   assumption is the first thing to check.

None of the above is guessed at random: each is this script's own
considered choice, documented so the owner's first cloud run can confirm or
correct it cheaply, rather than the run failing on an unexplained compile
error with no record of what was assumed.
