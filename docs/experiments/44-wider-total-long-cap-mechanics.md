# Wider total-long cap: mechanics only

## Scope and findings

ADR 0008's two declared Variants have stable identities `total-long-cap-24`
and `total-long-cap-36`. `registry.WiderTotalLongCap` changes only the
configuration's total-long limit and identifying strategy label. It preserves
all other values in the supplied control. The Baseline remains 12 Units.
Command fixtures use the existing synthetic fixture control, including its
shortened channels and generous group caps; they are not historical research.

The existing `.report` includes peak sector Units, the equally weighted mean
largest-sector share over nonempty Sessions, peak concurrent total open Units,
and distinct Campaign openings, including still-open Campaigns.
ReportSchemaVersion is 2. Reading schema 1 explicitly upcasts its missing
exposure to unknown, and never rewrites evidence. The wrapper, opening,
registry, event schemas and RulesVersion 1.16.0 are unchanged.

## Work log and testing evidence

- Read the full issue brief, development and domain guidance, ADRs 0008,
  0012 and 0015, and existing Variant and registry implementations.
- Added failing declaration, report integration, hand-computed concentration,
  Campaign lifecycle and schema compatibility tests before implementation.
- Added seeded cap properties for 12, 24 and 36: real competing Session
  proposals and fills, plus classified entry/Add checks with 4/6/10 caps.
  Each reaches the exact total-long limit and observes declines beyond it.
- Added two synthetic command golden sets: journal, registry, report and
  opening. Existing goldens, fingerprint rows and released decision corpora
  are unchanged. New journals pin existing rules under new configurations.
- Verified byte reproduction, hash-chain verification, replay and full rerun.
- `make check` passed with 95.9% coverage and all 270 LEAN adapter Python tests.
  Unix-socket tests required execution outside the filesystem sandbox.
  The intended offline vulnerability setting was ineffective: govulncheck
  ignored `GOVULNDB` and used its default online database. No market data was
  fetched. Subsequent checks omit a second vulnerability-database invocation.

## Concerns and remaining research

The current classification seam puts every instrument into the shared
Unclassified Group. Its unchanged 10-Unit Baseline cap binds before 12, 24
or 36; point-in-time classification remains the prerequisite for a diversified
comparison. Every nonempty Session currently has largest-sector share 1,
and peak sector Units equal peak total Units. That is expected under ADR 0008.

The first implementation used an event-by-event peak share, which reached 1
on a single-Unit opening even if the subsequent book diversified. The owner's
follow-up replaces it with the three Session measures specified in ADR 0012's
reporting section; schema 2 is still unreleased and changes in place.

Judging either Variant against ADR 0012, running diversified Regime Windows,
and drawing a twelve-versus-wider conclusion remain blocked by #42. This is
not a completed research experiment, adoption decision or issue closure.
Issue updates and follow-up filing are left to the orchestrating session;
no GitHub network operations were performed.

## Session exposure follow-up work log

- Added failing tests before implementation for the three replacement fields,
  multi-Session two-group arithmetic, Session boundaries and report integration.
  The red run failed on the absent fields and accumulator.
- Two-group hand calculation: `(0,0), (1,0), (2,2), (2,2), (1,3), (0,0)` gives
  peak sector 3, mean share 11/16, peak total 4. Repeated unchanged Sessions
  retain equal weight; empty Sessions are excluded from the mean.
- Samples include same-Session fills after the close marker. Tests cover
  transient intraday peaks, partial stops, exits, re-entry, incomplete Sessions,
  invalid evidence, schema-2 round trips and version-1 unknown-exposure reads.
- Updated only the two unreleased report goldens' exposure fields. Journals,
  decision content, registry entries, openings and released corpora are unchanged.
- Validation: `make check` passed: formatting, vet, staticcheck, architecture
  lint, race-enabled Go tests, coverage audit/floor (95.9%), govulncheck,
  build, and all 270 LEAN adapter tests. Vulnerability database access was
  permitted; no market data was fetched. Registry/backtest suites, report
  goldens, journal byte comparisons, replay and rerun checks passed.
