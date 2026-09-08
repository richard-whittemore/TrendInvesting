# ADR 0008: Faith's Unit caps apply to equities at instrument / industry / sector / total-long, and unclassified names are treated as correlated

- Status: Accepted
- Date: 2026-09-08

## Context

Faith caps exposure at 4 Units per market, 6 per closely-correlated group, 10 per loosely-correlated group, and 12 per direction [T p.16]. Equities need an equivalent grouping. The obvious source of sector and industry labels is fundamentals data, but delisted stocks stop receiving fundamental updates, so relying on those labels reintroduces survivorship bias into the universe — the very thing the point-in-time universe is meant to remove.

## Decision

Faith's numbers are kept: **4 Units per instrument, 6 per industry, 10 per sector, 12 total long**. With a Unit Volatility Fraction of 0.5 % these correspond to roughly 2 %, 3 %, 5 % and 6 % of equity per 1N move. Groups use the data provider's point-in-time industry and sector labels where present. **Every instrument without a label belongs to a single shared "unclassified" group capped at the loosely-correlated level (10 Units).**

## Consequences

- An unknown name is assumed correlated with every other unknown name. That fails safe and keeps delisted stocks in the universe without pretending their sector is known.
- Price-only correlation clusters (bias-free by construction) are a declared Variant.
- Twelve total Units means at most three fully-Loaded names on a thousand-stock universe. Faith's twelve were spread across ~21 futures; the number is kept for fidelity because the Baseline is the control, and **a wider total-long cap (24 and 36) is the second early ablation**, immediately after "recompute N per Add" (ADR 0006). If concentration hurts, that ablation shows it directly.
- A cap check is performed on every proposed entry and Add, against post-trade exposure, and a rejection is journaled with the cap that bound.
