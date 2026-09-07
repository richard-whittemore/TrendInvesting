# ADR 0003: Sizing is volatility-normalised, and the sizing mode is explicit

- Status: Accepted
- Date: 2026-09-06

## Context

Turtle and Sublime size positions on different principles. The Turtle rules size a Unit so that a 1N move equals a fixed fraction of equity [T p.14]; the Protective Stop at 2N is a *consequence*, so a wider stop leaves the share count unchanged and raises Risk at Stop. Sublime sizes so that the entry-to-stop distance equals a fixed fraction of equity [M p.56]; a wider stop *reduces* the share count and leaves Risk at Stop unchanged.

Both the Notion notes and the earlier QuantConnect prototype collapsed these into the Sublime form (`amountRisked / (stopMultiple × N)`), which is algebraically identical to the Turtle form only when the Stop Multiple is 2. The moment the 3×ATR stop experiment runs, the two silently diverge, and a configured "2 % risk" starts to mean something different from what the source says.

## Decision

Position sizing takes two independent, named inputs — **Unit Volatility Fraction** (equity fraction a 1N move represents; Faith uses 0.01) and **Stop Multiple** (N multiples to the Protective Stop; Faith uses 2) — and **Risk at Stop is always derived, never configured**.

The **Sizing Mode** is an explicit configuration value: `volatility-normalised` (the Baseline) or `fixed-risk-at-stop` (the Sublime Variant). Switching between them is a declared experiment, not a side effect of changing the stop.

The Baseline runs a Unit Volatility Fraction of **0.5 %**, half Faith's 1 %. With four Units at P, P+½N, P+1N, P+1½N and the common stop trailed to 2N below the last fill, the Units carry ½N, 1N, 1½N and 2N of risk respectively — 5 % of equity in one instrument at full load under Faith's fraction, 2.5 % under ours. A long-only equity book in a single regime lacks the cross-asset diversification the futures Turtles had, so the halving is the cheapest available insurance. Faith's 1 % is retained as a declared Variant.

## Consequences

- A change to the Stop Multiple changes Risk at Stop visibly, in the derived value, rather than silently re-scaling the whole book.
- The reducer must carry the Sizing Mode in its configuration and the decision journal, so replay can show which principle sized a given Unit.
- Golden tests transcribe Faith's Heating Oil sizing example [T p.14–15] under the volatility-normalised mode and the reconstructed Sublime example under fixed-risk-at-stop, so both modes are verified against their sources.
