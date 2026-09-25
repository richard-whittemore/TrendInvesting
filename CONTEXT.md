# Trend Investing

A stock-first trend-following research and trading platform. It evaluates a source-verified Turtle control, a rules-based Sublime control, and predeclared hybrid experiments, and must be able to explain and reproduce every decision it makes.

This glossary is deliberately opinionated: several of these concepts have near-synonyms in the source material that mean subtly different things, and conflating them has already caused real bugs.

## Language

### Price and volatility

**True Range**:
The greatest of a bar's own high-to-low range, the distance from the previous close up to the current high, and the distance from the previous close down to the current low. It measures a bar's movement including any opening gap.

**N**:
The 20-day Wilder average of True Range, and the project's canonical volatility measure for the Turtle rules. It is an average of True Range, never of price.
_Avoid_: ATR (means something narrower here), volatility, sigma.

**ATR**:
The volatility measure used by the Sublime rules, whose lookback period the sources do not disclose. Deliberately a separate term from **N**: the two are conceptually similar but are not interchangeable, and a rule citing one must not be implemented with the other.

**Completed bar**:
A bar whose period has ended. Every signal is computed from completed bars only; the bar being decided on is never an input to its own decision. This applies to *every* input to that decision, not only the **Entry Channel**: **N**, the Setup's **Tier**, the **Unit** size and the **Protective Stop** are all computed from the completed bars preceding the decision bar. Warm-up is likewise counted in completed bars, never calendar days.

**Session**:
The set of **Completed bars** that share one period end, across the whole universe: one trading day. A Session ends when its producer states which instruments it covered (`market.session.closed`). Only then are that day's Adds and entries decided, together and in ADR 0010's order, so the order in which the bars happened to arrive cannot change the outcome (ADR 0021). Each instrument has at most one bar per Session.
_Avoid_: day (ambiguous between a calendar date and a trading session), slice (LEAN's word for one delivery of data).

**Entry Channel**:
The extreme of the preceding 55 completed daily bars, beyond which a new **Campaign** may begin.
_Avoid_: Donchian 55, breakout band.

**Exit Channel**:
The extreme of the preceding 20 completed daily bars, beyond which an open **Campaign** is closed in full.
_Avoid_: Donchian 20, trailing channel.

**Breakout**:
Price exceeding the **Entry Channel**. A Breakout is an event in the market, not a decision to trade.

### Positions

**Unit**:
One indivisible increment of a position: the share count at which a 1N price move equals a declared fraction of account equity. The Turtle rules measure exposure and risk limits in Units rather than in currency.

**Campaign**:
The complete life of a position in one instrument, from the first Unit's entry to the exit of the last. A Campaign, not a Unit, is what gets entered, added to, stopped out, and exited. A Campaign begins **only when a recorded fill says it does** — never when an order is submitted and never from a quoted price — and its **N** and **Unit** size are frozen at that first entry (ADR 0006), measured from the price that actually filled. A proposal that is never filled leaves no Campaign behind. While an instrument is in a Campaign it is not a **Setup**, so no new entry is signalled for it.
_Avoid_: trade, position (both ambiguous between a Unit and the whole Campaign).

**Add**:
A further Unit taken into an already-profitable Campaign.
_Avoid_: pyramid, scale-in, compound (Sublime's word for its own, differently-conditioned version of this).

**Add Ladder**:
The set of prices at which successive Units of a Campaign will be added, determined when the Campaign opens.

**Protective Stop**:
The price at which a Campaign's Units are exited to cap loss. Every open Campaign has one at all times.
_Avoid_: stop-loss, trailing stop (the latter is a distinct Sublime exit variant).

**Stop Ladder**:
The progression of Protective Stops as Units are added to a Campaign.

**Exit Order**:
The one sell order a held Unit rests, for that Unit's own shares: at its **Protective Stop**, or, while an Exit-Channel exit is proposed for the Campaign, at the Exit Channel level if that is higher (a tie names the stop). A Campaign's Exit Orders together cover exactly its holding, so a stop and an exit can never both sell the same shares. The engine decides the level and records each change; a consumer mirroring orders never combines the two levels itself.
_Avoid_: exit proposal (the Campaign-level decision to exit, one of the two inputs), stop order.

**Order lifecycle report**:
A venue's account of a change in an order's lifecycle that is not an execution: acknowledged, amended, pending cancellation, cancelled, or refused, including an order the venue refused before it ever rested (ADR 0022). It moves no position — the only event that may is a **fill** — and the engine records it without deciding anything from it; it exists so reconciliation (ADR 0019) can tell an ordinary cancel-and-replace from an unexplained difference in the broker's own order book.
_Avoid_: fill, execution (both change a position; a lifecycle report never does).

**risk-free**:
A held Unit whose current Protective Stop, after being raised by the Stop Ladder, sits at or above its own entry price — reachable only by raising, never by an initial stop, which must sit strictly below entry. Its contribution to a Campaign's aggregate open risk is exactly zero: `max(0, EntryPrice − ProtectiveStop)`, a per-share price distance, never a negative figure and never a validation failure — sizing.AggregateOpenRisk scales that distance by the Unit's Quantity and by DollarsPerPoint to reach the account-currency figure. A break-even or profit-protecting stop is a legitimate outcome of the Stop Ladder, not a corrupted one (The Turtle Rules p.23–24).
_Avoid_: risk-free rate (the unrelated finance term).

**Loaded**:
Holding the maximum permitted number of Units for a given risk level — in one instrument, in a correlated group, or in one direction.

### Setups and the Watchlist

**Setup**:
An Eligible instrument that is not in a Campaign, together with its current Tier. Every Eligible instrument outside a Campaign is a Setup; most are far from any entry condition.

**Tier**:
The readiness stage of a Setup. **Tier B** means the Setup is approaching its entry condition; **Tier A** means the entry condition was met on the current completed bar. A Setup can enter Tier A directly without passing through Tier B.
_Avoid_: grade, quality, rank (Tier says nothing about how good a Setup is, only how close).

**Grade**:
The quality band of a Setup, derived from a mechanical score over its trend history, all-time-high proximity, timeframe alignment, and sector strength. Used only by the Sublime Variant. This is what the Sublime webinar calls "Tier A / Tier B"; that usage is deliberately not adopted here.
_Avoid_: tier.

**Signal**:
The event of a Setup reaching Tier A on a completed bar — the strategy recognising that its entry condition is met. In the Baseline a Signal is a Breakout; in the Sublime Variant it is the second breakout. A Signal belongs to one bar and expires with it, and so does the trade proposal a Signal produced: the next completed bar for that instrument supersedes an unfilled proposal (ADR 0011).
_Avoid_: breakout (a market event, not a strategy decision), trigger, alert.

**Watchlist**:
The ranked set of every Setup currently in Tier B or Tier A. It is the pre-image of tomorrow's Signals and the first thing reviewed each day, in every strategy configuration.

### Risk

**Unit Volatility Fraction**:
The fraction of account equity that a 1N move in one Unit represents. The primary sizing input under the Turtle rules.

**Stop Multiple**:
The number of N between a Unit's entry and its Protective Stop.

**Risk at Stop**:
The equity fraction lost if a position is stopped out. Under volatility-normalised sizing this is a *derived* quantity, not a configured one.

**Sizing Mode**:
Which quantity position size is keyed to. *Volatility-normalised* sizes from **N**, so a wider stop means the same share count and more Risk at Stop. *Fixed-risk-at-stop* sizes from the entry-to-stop distance, so a wider stop means fewer shares and unchanged Risk at Stop. The two are only equivalent at one particular Stop Multiple.

**Notional Account**:
The equity figure used for position sizing, which is reduced during a drawdown and is therefore not the same as actual account equity.
_Avoid_: equity, balance, capital.

**Price cap**:
The highest price, before slippage, at which an entry or Add may execute. It is the order's level plus a configured multiple of N (1N in the Baseline). The order rests as a stop-limit: a bar that gaps above the cap and never trades back down to it does not fill, and the Unit is skipped. The declared Variant `uncapped` rests a stop-market order with no cap (ADR 0005, as amended).
_Avoid_: limit (ambiguous with a Unit cap), gap buffer (the configured multiple, not the price).

**Hold**:
What a proposed entry or Add reserves from the moment it is proposed until its proposal fills, expires or is cancelled. It reserves its worst-case cost against spendable cash (the price cap plus slippage, times the quantity, plus commission), and one Unit of headroom under every Unit cap the Unit counts towards. Every later proposal is checked against cash less fill debits and standing holds, and against committed plus reserved Units. A fill replaces its hold with the fill's actual cost. A snapshot never releases a hold (ADR 0020, as amended).
_Avoid_: reservation (the act, not the record), margin, buying power (the broker's concept, which this does not model).

**Drawdown Step**:
A 20 % reduction of the Notional Account, triggered each time actual equity falls 10 % below the figure the Notional Account was last measured against.

**Strength**:
The mechanical ranking measure applied when several instruments signal at once: an instrument's price change over the preceding 63 completed bars, divided by its N.

### Universe

**Universe**:
The set of instruments the strategy may open a Campaign in on a given day. Membership is decided point-in-time from declared criteria and never from knowledge of what happened afterwards.

**Eligible**:
An instrument that satisfies the Universe criteria on the day in question. Losing eligibility affects only new Campaigns, never open ones.

**Unclassified Group**:
The single correlation group that holds every instrument whose industry and sector are unknown. Its members are treated as correlated with one another.

**Delisting Exit**:
The forced closing of a Campaign because its instrument ceased to trade, distinct from an Exit-Channel exit or a stop-out.

### Research

**Baseline**:
The source-verified Turtle stock control against which every experiment is measured.

**Variant**:
A predeclared change to the Baseline along a single dimension, evaluated as an ablation.
_Avoid_: version, tweak, config.

**Golden Scenario**:
A hand-checked worked example transcribed from a primary source and used as a test fixture. Golden Scenarios are transcribed, never invented.

**Regime Window**:
One of seven fixed, named spans of market history over which every result is evaluated separately, so that an improvement confined to one kind of market is recognised as such.

**Provenance tag**:
The classification of a strategy rule as *disclosed*, *reconstructed*, *proxy*, or *excluded*, according to how directly the source material supports it.
