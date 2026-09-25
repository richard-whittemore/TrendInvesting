# Wider total-long cap: mechanics only

## Scope and findings

ADR 0008's two declared Variants have stable identities `total-long-cap-24`
and `total-long-cap-36`. `registry.WiderTotalLongCap` changes only the
configuration's total-long limit and identifying strategy label. It preserves
all other values in the supplied control. The Baseline remains 12 Units.
Command fixtures use the existing synthetic fixture control, including its
shortened channels and generous group caps; they are not historical research.

The existing `.report` now includes the maximum sector share of open Units
and the count of distinct Campaign openings, including still-open Campaigns.
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
comparison. Every nonempty run currently has concentration 1.

The requested literal event-by-event maximum also includes the first Unit
opening, so even a subsequently diversified book reaches 1. Changing sampling
frequency or conditioning on book size requires an explicit protocol decision;
this implementation does neither. The metric conventions and schema are
specified in `docs/running-a-backtest.md`.

Judging either Variant against ADR 0012, running diversified Regime Windows,
and drawing a twelve-versus-wider conclusion remain blocked by #42. This is
not a completed research experiment, adoption decision or issue closure.
Issue updates and follow-up filing are left to the orchestrating session;
no GitHub network operations were performed.
