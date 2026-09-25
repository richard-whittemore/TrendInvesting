# ADR 0009: The Baseline universe is deliberately permissive, and losing eligibility never closes a Campaign

- Status: Accepted
- Date: 2026-09-08

## Context

Faith's only universe rule is liquidity [T p.10]. Sublime adds a $20 price floor, a 1 M-share volume floor, and a 5–10-year history requirement — rules that belong to the Sublime Variant, not the Turtle Baseline. If those were baked into the Baseline, their effect could never be measured.

## Decision

An instrument is **eligible** for the Baseline universe when it is common stock on a US primary exchange (no ETFs, ADRs, or SPACs), its price is at least $5, its 20-day median dollar volume is at least $5 M, and it has at least 250 completed bars of history. Eligibility is re-evaluated **point-in-time on the first trading day of each month**.

**Losing eligibility never closes an open Campaign.** A Campaign ends only through the Exit Channel, the Protective Stop, or a delisting; a delisting is a forced exit at the last available price, journaled as a distinct exit reason.

## Consequences

- Sublime's stricter filters become measurable ablations.
- Universe membership changes only through declared criteria on declared dates, so it can be reproduced exactly on replay.
- The thresholds are parameters and are sensitivity-tested.

## Amendment: the universe gate is on-or-off as a whole, and an unevaluated instrument is not a candidate while it is on (the owner's decision, 2026-09-25)

### Context

The Decision above settles the four criteria and the monthly cadence, but leaves two questions open that the port's first implementation exposed: what happens to an instrument the universe has not yet spoken about at all, and how a fixture or a run that sends no classification at all — the single-instrument acceptance runs, and every existing golden journal and decision-corpus scenario, none of which sends a classification input — is meant to behave. Gating everything by default would decline every one of them from their very first entry; never gating anything by default would let an unclassified instrument trade regardless of the configured thresholds. Neither is stated in the Decision.

### Decision

The three thresholds (price, dollar volume, history) are switched together, never individually:

- **The universe gate is on** when all three are configured positive. Every instrument must then be classified and evaluated eligible before an entry Signal can become a proposal. An instrument that has never been classified, or that was classified but whose own evaluation has not yet run, is declined `ineligible`, with a detail naming why — it is not a candidate for a new Campaign merely by default.
- **The universe gate is off** when all three are configured zero. No instrument is evaluated and no entry is ever declined for eligibility, whatever it is or is not classified as. This is the Baseline configuration until a provider-backed universe port exists, and it is what every existing fixture, golden journal and decision-corpus scenario runs under.
- **A partial configuration — some thresholds zero, others positive — is invalid** and is refused outright by `ConfigurationPayload.Validate`. The gate never disables one criterion silently by leaving it at zero while the others bind.

While the gate is on, evaluation is no longer purely monthly: the run's first Session close evaluates every instrument classified by then, and an instrument classified after its own last evaluation is evaluated again at the very next Session close, not held to the next month's boundary. An instrument neither newly classified nor newly reclassified is still re-evaluated only on the first trading day of each calendar month, as the Decision above states.

**Losing eligibility still never closes an open Campaign, and never gates an Add.** Nothing above changes that: the gate applies only to a NEW Campaign's entry, exactly as the Decision already states.

### Consequences

- Every existing fixture, golden journal and decision-corpus scenario keeps its configuration at all-zero thresholds and is unaffected by this amendment: the gate is off for all of them by construction, not by circumstance.
- A configuration that turns the gate on commits to classifying its whole universe; there is no partial or accidental gate.
- The first-Session and reclassification-triggered evaluations mean a run that turns the gate on mid-history, or that classifies an instrument mid-run, never waits out the rest of a calendar month before its declared criteria take effect.
