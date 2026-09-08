# ADR 0011: The Watchlist is ranked in every configuration, filtered only by caps in the Baseline, and a Signal never outlives its bar

- Status: Accepted
- Date: 2026-09-08

## Context

On a broad equity universe, Signals are not rare: Sublime's own scanner reports 150–200 candidates a day from 20,000 instruments [V 00:03:22], and 50–300 normally, up to 1,000 in a trending market [M p.54]. The Baseline may hold 12 Units in total. Capacity, not scarcity, is the binding constraint, so the ranking of Setups is load-bearing nearly every day — and it is tempting to let a quality judgement decide which Signals to take. Faith is explicit that skipping valid signals was the behaviour that separated the Turtles who failed from those who did not [T p.20].

Separately, a Signal that could not be acted on (a cap or the cash rule bound) raises the question of whether it is still live the next day.

## Decision

1. **Every Eligible instrument outside a Campaign is a Setup, evaluated exactly once per completed bar.** There is no separate "review the Watchlist, then rescan" pass: a Setup that was Tier B yesterday is re-evaluated today alongside every other Setup, against today's bar. One pass, one answer per instrument per day.
2. **The Watchlist — all Tier B and Tier A Setups, ranked by Strength — exists in every configuration** and is a first-class observable: it is the pre-image of tomorrow's Signals and the basis for reconciling why a Signal did or did not become a Campaign.
3. **In the Baseline, the only thing that stops a Tier A Setup from becoming a Campaign is a cap or the cash rule.** Grade plays no part. In the Sublime Variant, Grade is an additional gate — "Grade B only when no Grade A Setup is in Tier A" — and that difference is a declared ablation.
4. **A Signal belongs to one bar and expires with it.** A Tier A Setup that was not entered is not carried forward; it must re-qualify on a later bar. The operator's rule, verbatim: *never enter a Tier A stock without first checking that it still belongs in Tier A.* A genuinely trending stock will make a new 55-bar high and re-signal on its own. Persistent Signals are a declared Variant.

## Consequences

- The reducer holds no "pending signal" memory in the Baseline; Setup state for the Baseline is a pure function of the bar and the frozen configuration. The Sublime Variant's 4PS phases are the one place Setup state carries memory, and those transitions are journaled.
- A future reader who wants to "fix" the Baseline by adding a quality filter, or by carrying a good Signal forward a day, should find this record first.
- Scanning cost is not a design input: the per-bar evaluation across the whole universe is a few arithmetic operations per instrument on rolling windows and is dominated by data loading, which the adapter owns.
