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

> **Implementation note (2026-09-24, RulesVersion 1.9.0).** All four caps are
> now enforced, for both entries and Adds, against post-trade exposure —
> the Units that would be held, across every OPEN Campaign sharing the
> cap's grouping, once the proposed Unit joined them. Previously only the
> per-instrument cap existed (`event.ConfigurationPayload.MaxUnits`), and a
> Campaign already at it silently proposed no further Add rather than
> declining one whose rung was actually reached; that silence is now a
> journalled `strategy.proposal.declined` naming the cap
> (`event.DeclineReasonUnitCapExceeded`), its configured limit, and the
> exposure that would have resulted (`Cap`, `CapLimit`,
> `PostTradeExposure`; `strategy.proposal.declined` schema 5).
>
> **Configuration.** The four numbers are configured, never hardcoded:
> `ConfigurationPayload.MaxUnits` (per instrument, carried since schema 1),
> `MaxUnitsPerIndustry`, `MaxUnitsPerSector` and `MaxUnitsTotalLong` (schema
> 5, #55). The Unclassified Group is capped at `MaxUnitsPerSector`'s own
> value — this Decision's own "loosely-correlated level" — rather than by a
> fifth, separately configurable number, since the ADR text ties it to that
> level directly.
>
> **The classification seam.** No classification input event exists yet:
> the point-in-time industry/sector universe is #37/#41's own work.
> `internal/strategy/unit_caps.go`'s `classificationOf` is the one place a
> point-in-time label will be resolved; until then it always reports
> Unclassified, and every Unclassified instrument shares CONTEXT.md's
> single Unclassified Group. A Campaign's classification is resolved once,
> when it opens (`openCampaign`), and stored on `campaignState`, never
> re-read afterward — the same freeze-at-entry discipline ADR 0006 applies
> to N and the Unit size — so a later classification input can never
> retroactively change an already-open Campaign's cap grouping.
>
> **Same-pass headroom is deliberately NOT shared across proposals.** ADR
> 0010 states Unit-cap headroom is "known at the previous close." ADR 0020
> amended that sentence for cash only, and says explicitly: "For Unit-cap
> headroom that stands unchanged." Neither ADR describes a running ledger
> for caps the way ADR 0020 built one for cash, and ADR 0020's own
> "Alternatives rejected" section rejects "aggregate check with a new
> priority order" for cash precisely because ranking competing proposals
> against a shared budget "would invent a rule the Turtle sources do not
> state." ADR 0021's own "Open, deferred to #34" section confirms the
> current state plainly: "within one session-close pass nothing is spent,"
> and a shared cap/cash budget across a pass's proposals is future work
> (#33/#34/#105). This implementation therefore checks each proposal
> against every OTHER open Campaign's current, already-committed Units —
> never against a sibling proposal still being decided in the same pass.
> Two entries that would each fit the total-long cap alone are therefore
> BOTH proposed if decided within the same session-close pass; a shared
> per-pass budget is the unresolved decision #33/#34/#105 exist to make,
> not one this ticket invented. See the #36 implementation report for the
> full text considered and the options this leaves open.
