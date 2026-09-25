# ADR 0012: Experiment protocol — span, regime windows, out-of-sample discipline, and what "winning" means

- Status: Accepted
- Date: 2026-09-08

## Context

Tunable parameters are how hindsight leaks into a backtest, and the best-looking result is usually the most over-fitted one. This record fixes, in advance, how results are produced and judged, so that no choice is made after seeing the equity curve.

The protection the Baseline needs is often misstated as "every Baseline number comes from Faith." That is not true, and stating it that way invites an implementer to "correct" a deliberate adaptation back to the source value. The Baseline contains numbers Faith never wrote: the Unit Volatility Fraction is 0.5 %, half his (ADR 0003), and the Universe thresholds are ours entirely (ADR 0009).

The accurate statement is narrower and stronger: **no Baseline parameter was chosen by looking at results.** Every Baseline number is either transcribed from the source or justified by an argument made before any backtest was run. That is why the Baseline needs no in-sample/out-of-sample protection, and why Variants do.

## Decision

### Parameter provenance

Every parameter falls into exactly one of three categories, and its category determines who may change it and how.

**Source-fixed** — transcribed from `The Turtle Rules` and not changed in the Baseline: the 55-bar Entry Channel and 20-bar Exit Channel, the Stop Multiple of 2, the half-N Add spacing, the four-Unit maximum, the 4/6/10/12 Unit caps, and the 20 %-per-10 % Drawdown Step. Changing one of these produces a Variant, never a corrected Baseline.

**Baseline-declared adaptation** — chosen by this project because Faith's futures rules do not map onto long-only equities unchanged, each justified by a recorded argument rather than by a result: the 0.5 % Unit Volatility Fraction (concentration argument, ADR 0003), the Universe eligibility thresholds (deliberately permissive so Sublime's stricter filters stay measurable, ADR 0009), the fill model (ADR 0005), 0.05 N slippage (ADR 0013), and the yearly re-basing date (ADR 0007). These are fixed for the Baseline; changing one is a declared Variant.

**Variant-tunable** — thresholds a Variant introduces, and the only parameters ever chosen by fitting: the 4PS thresholds, Grade cut-offs, alternative cap sizes, and the recompute-N rule. These, and only these, are what the in-sample/out-of-sample discipline below exists to protect.

An implementer who finds a Baseline number that differs from the source should expect to find its justification in the ADR that set it. If there is no such justification, that is a defect in the record, not a licence to change the number.

### Protocol

**Span and regimes.** Backtests run over the full available history (QC equity data begins 1998-01) and are additionally evaluated over seven named Regime Windows: 1998–2000 late bull, 2000–02 bear, 2003–07 bull, 2008–09 crash, 2009–19 bull, 2020 COVID, 2022 correction. The windows are fixed here and not revised after results are seen.

**In-sample / out-of-sample.** A hard split at **2016-01-01**. Variant parameters are chosen only on 1998–2015, from a grid declared before the run. The out-of-sample window is opened **exactly once per Variant**; a second look is a new hypothesis and is recorded as one. Walk-forward evaluation (rolling five-year fit, two-year test) is a robustness check in addition, never a substitute.

**A Variant is adopted only if it clears all of:**

1. Better out-of-sample annualised return divided by maximum drawdown than the Baseline on the same span.
2. The improvement points the same way in at least five of the seven Regime Windows.
3. It survives ±30 % perturbation of every threshold it introduced.
4. It does not reduce the number of independent Campaigns below 60 % of the Baseline's.
5. It does not increase peak sector concentration.

**"No Variant wins" is a legitimate outcome.** Every result — adopted, rejected, and failed — is retained under its configuration hash, as `docs/development.md` already requires.

**Ablation order.** The first two Variants run are "recompute N per Add" (ADR 0006) and the wider total-long cap (ADR 0008), because each addresses a known, mechanistically-predicted weakness of the Baseline.

## Consequences

- Return per unit of drawdown, not CAGR, is the primary metric: it measures return per unit of pain, and a strategy that would have been abandoned mid-drawdown is not a good strategy.
- The regime and perturbation rules filter out regime luck and coincidental thresholds; the trade-count floor and concentration rule filter out hindsight selection and accidental sector bets.
- Every run's configuration, span, split, and outcome are journaled, so an adopted Variant can be shown to have cleared every rule.

## Amendment — executable windows and opening records (Accepted, 2026-09-25)

**Status: Accepted**, by the owner's ratification of 2026-09-25 ("Accept
all"), of every point raised when this amendment was first recorded as
Proposed the same day. These are conservative implementation conventions;
they do not change any Accepted strategy parameter above.

- **Configuration:** `internal/registry/protocol.json` is the versioned research
  configuration, embedded in the build. No command-line or trading-configuration
  override exists. Each report retains the entire protocol and its canonical
  SHA-256 fingerprint; each opening retains that fingerprint. Changing the file
  is a reviewed protocol change, never a new per-run setting.
- **Dates:** named years include their whole UTC calendar years, represented as
  `[start, next-year-start)`. Thus 2000 belongs to both the late bull and bear,
  and 2009 to both the crash and subsequent bull. Nothing silently assigns these
  overlapping years to a preferred regime. Missing years remain outside all
  windows. The split is inclusive on the out-of-sample side. A spanning run is
  explicitly `mixed`, never usable as a fitting run; the full report also
  separates the in-sample and out-of-sample curves. Empty failed runs are
  `unknown`. The designation uses all input dates, including corporate actions
  -- the whole span a run was GIVEN, not merely what it goes on to apply
  before it might stop early. Both the report and the opening it sits beside
  retain that declared span alongside the executed one the equity curve
  itself reflects, so a run that stops early never leaves its own designation
  unexplained.
- **Metric:** the primary numerator is the annualised return specified by
  adoption criterion 1, not unannualised profit. Report total return too.
  With positive opening equity `E0`, final equity `E1`, and elapsed years
  `Y = elapsed UTC hours / (24 × 365.25)`, compute `(E1/E0)^(1/Y) - 1`.
  Maximum drawdown is `max((running_peak - equity)/running_peak)`, over the
  net account-equity marks, including costs and unrealised positions, never
  the Notional Account. The ratio is annualised return / maximum drawdown.
  Zero drawdown, insufficient history, zero elapsed time and non-finite
  arithmetic have a null ratio and an explicit state, never an infinite
  winning score. No external cash flows are supported by this calculation;
  they require a separately agreed return-adjustment methodology and fail
  closed.
- **Window marks:** use snapshot `AsOf`, not delivery time. Carry the last mark
  preceding a window to its starting boundary so its first loss is counted,
  even when the boundary mark and the window's only other mark share that
  same instant: the drawdown and total return between them are still
  reported, and only the annualised return, and the ratio it feeds, are left
  undefined over that zero elapsed time. Do not extrapolate the last observed
  mark to an unobserved window end.
  Every window is reported, including `no-data`; actual metric start/end and
  sample count show partial coverage. A partial or failed run is not evidence
  that a Variant clears the adoption criteria.
- **Opening:** every attempt that can execute any out-of-sample input consumes
  one opening, including a subsequently failed, interrupted or abandoned run.
  An immutable `.opening` sidecar is installed and flushed **before** strategy
  execution. A repeat remains executable but is prominently flagged as a new
  hypothesis, identified by configuration hash plus run ID, with links to all
  prior hypotheses. Changing a parameter hash does not reset a Variant's
  history. Old registry entries are included across all configuration hashes;
  a legacy Variant run whose span is unknown conservatively counts as exposure.
  A run and its opening are deduplicated. The Baseline is exempt, as stated above.
- **Authority and concurrency:** exactly-once protection applies to one complete,
  authoritative registry directory: the repository's registry on the owner's
  machine (owner ratification, 2026-09-25). An exclusive directory lock
  serialises reading history and installing openings on that filesystem. An
  abandoned lock requires an operator audit, not automatic expiry. A clone
  other than the owner's machine cannot prove a global first look while
  offline; a look at held-out data taken from any such clone must be reported
  by a person and reconciled against the authoritative registry before a
  Variant is adopted on it — no distributed exactly-once guarantee is
  claimed. Variant labels must remain stable across parameter choices and
  machines. Renaming a label does not establish a fresh hypothesis
  scientifically, even though a file registry cannot infer semantic equivalence.
- **Fitting:** `backtest -fit` rejects any span not wholly before the configured
  split, before strategy execution. An ordinary run is evaluation, not evidence
  of parameter selection. Declaring the parameter grid before fitting remains
  required by the Accepted protocol; a grid/selection provenance workflow is
  outside this reporting command and must be implemented before automated tuning.
- **Retention:** preserve existing version-1 registry entries and journals
  unchanged. New reports use append-only version-1 `.report` JSON sidecars beside
  each entry, containing configuration identity, status, protocol, journal anchor
  and any opening. Readers of old registry entries remain compatible. Commit
  sidecars with the registry and journals. Unregistered fixture runs print a
  report but do not establish auditable Variant results.

**Owner ratification (2026-09-25, "Accept all").** The questions this
amendment raised as Proposed are answered, and recorded here as decisions:

1. Boundary years belong to both neighbouring windows, which are half-open
   whole-UTC-year intervals (`[start, next-year-start)`).
2. The split date, 2016-01-01, is out-of-sample.
3. "Exactly once" is enforced within one authoritative registry: the
   repository's registry on the owner's machine. A look at held-out data
   taken from any other clone must be reported and reconciled by a person
   before a Variant is adopted.
4. Annualisation uses 365.25 days.
5. Grid and selection provenance is deferred to a later ticket.
