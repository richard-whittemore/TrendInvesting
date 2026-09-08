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
A bar whose period has ended. Every signal is computed from completed bars only; the bar being decided on is never an input to its own decision.

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
The complete life of a position in one instrument, from the first Unit's entry to the exit of the last. A Campaign, not a Unit, is what gets entered, added to, stopped out, and exited.
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

**Loaded**:
Holding the maximum permitted number of Units for a given risk level — in one instrument, in a correlated group, or in one direction.

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

**Provenance tag**:
The classification of a strategy rule as *disclosed*, *reconstructed*, *proxy*, or *excluded*, according to how directly the source material supports it.
