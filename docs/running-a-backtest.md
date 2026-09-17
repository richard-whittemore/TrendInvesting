# Running a backtest

One command runs a declared configuration over a bar fixture and writes the run's journal.

```sh
go run ./cmd/backtest \
  -config cmd/backtest/testdata/configuration.json \
  -bars   cmd/backtest/testdata/bars.json \
  -out    run.jsonl
```

- `-config` is a JSON `event.ConfigurationPayload`: the strategy identifier, Sizing Mode, channel lengths, maximum Units, slippage, the Notional Account and the commission schedule. A configuration with zero slippage is refused before any bar is read (ADR 0013).
- `-bars` is a JSON array of `event.CompletedBarPayload`, in the order the run delivers them.
- `-max-records` bounds the records the run holds in memory before its journal is written, defaulting to 2,000,000 — roughly twice a decade of daily bars over a hundred instruments, and about two gigabytes of records. Both figures are arithmetic rather than measurement, and the per-record size is the shakier of the two: ~1 kB is the mean serialised line length of the golden fixture standing in for an `Entry`'s resident size, which nothing has measured. The bound is checked between inputs, so a run exceeds it by the decisions of the input that reached it and by no more than `journal.MaxEmissionsPerInput` (65,536); an input causing more decisions than that is refused whole. A journal is composed in memory, because the header states the span the run covered and that is not known until the last input has arrived (ADR 0017); the bound is what turns "the process died having written nothing" into a run that stops, says so, and leaves the records it took. A run that reaches it exits non-zero and writes the journal of what it recorded, under the span that journal actually covers. It is registered as a failed run only if it was given a registry to record itself in: `-registry` and `-run-id` are all-or-nothing (either without the other is refused), and a run invoked without them registers nothing anywhere, so there the non-zero exit is the whole signal. Raise it only knowing the cost is memory in proportion.
- `-out` is where the journal is written. **An existing file is never overwritten**: a journal is recorded evidence (AGENTS.md rule 6, and ADR 0018 for how it is installed), so the command refuses and asks you to move it aside or choose another path. There is no overwrite flag. The write goes through a temporary file in the same directory, which is then linked into place — a hard link rather than a rename, because a rename would silently replace a journal that appeared while the run was in progress, whereas a link fails atomically. So an interrupted run leaves nothing partial behind, and two runs racing for the same path end with one journal and one clear error rather than one journal overwritten by the other.

## What it writes

A journal: the complete ordered stream of the run's input and decision events, one record per line, under a header. The header states the configuration hash and strategy version — both derived from the configuration actually run (ADR 0016), never supplied on the command line — the span of input event times covered, the chain algorithm, and the journal format version.

The span is derived from the run rather than supplied, and it is also **checked**: it must be exactly the first and last event time among the journal's own `input` records, or the journal is refused before a byte is written. The chain covers the span, so an edit to it is detectable — but only against a chain nobody repaired, and a writer composing its own header would otherwise be trusted rather than checked. The span speaks for the input stream alone: a decision is attributed to the input that caused it.

Each record is `{"sequence": N, "kind": "input"|"decision", "envelope": {...}, "record_hash": "sha256:..."}`, chained over the previous record's hash, the record's kind, and the canonical bytes of its own envelope; the chain is seeded with the hash of the header, so editing which run the journal claims to be breaks it at the first record (ADR 0017). Records appear in recording order: each input, then the decisions that input caused. `kind` is what a reader — replay equivalence above all — uses to tell the two apart, and the chain covers it.

The run's inputs are the configuration event, each completed bar (each one driven through the per-bar fill protocol of ADR 0005, so the fills the simulator decides are interleaved as inputs of their own), and finally an end-of-stream event that expires any proposal still outstanding — so every proposal in a completed run reaches exactly one terminal event.

## How to verify a journal

```sh
go run ./cmd/backtest -verify run.jsonl
```

This recomputes the chain and reports the run's identity, its record count, and its final record hash; it exits non-zero naming the first broken link if the file was edited after it was written.

Verification answers one question only — **was this file edited** — and deliberately not the other. Whether this build still produces the recorded decisions is replay equivalence's question, asked by replaying the journal's input stream through a fresh reducer. A journal can fail either check independently, and the two failures mean different things (ADR 0017).

A hash chain is tamper-**evidence**, not tamper-**proofing**: whoever can rewrite one record can rewrite the file. Anchoring each run's final record hash in the git-committed run registry is what closes that.

## How to check a journal replays

```sh
go run ./cmd/backtest -replay run.jsonl
```

This feeds the journal's own recorded inputs back through a freshly constructed reducer and compares the decisions it emits, in order, with the decisions the journal recorded. It exits non-zero naming the first divergence — its position, what the journal records there, and what this build now produces instead.

The reducer is built from the journal alone: the strategy version and configuration hash from its header, and the configuration payload from its own input stream. Nothing is supplied on the command line, because a journal is meant to be self-describing evidence.

One invocation performs exactly one operation. `-verify`, `-replay` and `-runs` are mutually exclusive with each other and with the flags that describe a run to perform (`-config`, `-bars`, `-out`, `-run-id`, `-variant`, `-max-records`); asking for two is refused rather than silently given one of them. `-registry` is the one flag two operations share — it names where a run is recorded, and where recorded runs are read from — so it is refused alongside `-verify` or `-replay`, which would ignore it.

Replay refuses rather than reports a divergence whenever it cannot ask the question at all — a journal it cannot read, an unknown record kind, a header strategy version it cannot parse, a missing or undecodable configuration. Among those refusals, these are the **identity-consistency checks**, each holding one part of the header against what the journal itself records:

- **The journal's rules version is not this build's.** Replay compares runs on the rules version alone; the build suffix is traceability only (ADR 0016). A mismatch means this engine's rules have moved on since the journal was written — which is not evidence that the journal is wrong. A journal written by a *different build of the same rules* replays normally, which is the point of splitting the two axes.
- **The header names a strategy its own configuration does not declare.** The configuration hash pins only the payload, so without this a journal could claim one strategy in its header while having run another.
- **The header claims a configuration the journal does not record.** The header's configuration hash is the run's identity (ADR 0012); replaying under a configuration the header does not claim would answer a question nobody asked.
- **A record names a run other than the header's.** Every record must state the header's strategy version and configuration hash. This matters most for inputs: they are fed to the reducer and their own identity fields are never compared to anything, whereas recorded decisions are already pinned by the byte comparison.

What this proves is the **reducer's** determinism. A journal's inputs include the fills the simulator decided (ADR 0005), so replaying them re-derives the reducer's own decisions — Setup evaluation, sizing, the Add, stop and exit rules — and says nothing about whether the fill simulator would produce the same fills again from the bars alone. That stronger, whole-pipeline property is a separate question.

## Recording the run in the registry

Every run — successful, failed, or abandoned — belongs in the run registry, under the configuration hash that identifies it (ADR 0012). Add `-registry` and `-run-id` to the run:

```sh
go run ./cmd/backtest \
  -config   cmd/backtest/testdata/configuration.json \
  -bars     cmd/backtest/testdata/bars.json \
  -out      runs/journals/baseline-2026-09-13.jsonl \
  -registry runs \
  -run-id   baseline-2026-09-13 \
  -variant  baseline
```

- `-registry` is the registry root, committed to git. `-run-id` is required with it: the identifier is chosen by you, never derived from a clock, so that two records of one run are recognisable as such. Lower-case letters, digits and interior hyphens only — it becomes a file name, and upper case would collide on a case-insensitive filesystem.
- `-variant` is the Variant this run declares, defaulting to `baseline`. There is no unattributed run. `-run-id` and `-variant` both describe how a run is *recorded*, so both are refused without `-registry`, and alongside `-verify`, `-replay` or `-runs`, rather than being accepted and quietly dropped.
- `-out` is recorded **relative to the registry root**, so put it inside the repository — ideally under the registry root itself — and the journal an entry points at is committed beside it. A `-out` and a `-registry` that cannot be expressed relative to one another (one absolute and one relative, or two volumes) are refused rather than recorded as an absolute path, which would name a location that exists on exactly one machine.

### The layout, and why it is this one

One file per run, in a directory named for its configuration hash:

```
runs/
  sha256-<digest>/
    baseline-2026-09-13.json
    baseline-2026-09-14.json
  sha256-<other digest>/
    recompute-n-2026-09-15.json
```

**The layout is the index.** There is no manifest, catalogue or aggregate file, because any of those would be one mutable blob that every run rewrites — and two runs recorded on two branches would then conflict on every merge. Here two runs never write the same path, so two branches that each recorded a run merge as two additions.

**Nothing is ever overwritten.** The entry is written to a temporary file in its own directory, flushed, and hard-linked into place: `link(2)` fails with `EEXIST` atomically, so a run id the registry already holds is refused rather than replaced, and two runs recorded at the same instant can neither interleave nor clobber one another. This is the same install the journal uses, and for the same reason — `rename(2)` would replace the destination silently. The entry's **content and its directory entry are both flushed**, including the configuration-hash directory created to hold it, so a power loss just after a command reports success cannot come back with the run reported as recorded and nothing on disk.

**The link commits; the flush after it is a second fact.** The hard link is the moment the run is recorded, and the directory flush happens after it, so a flush that fails leaves the entry installed. The command reports both: it fails, *and* it says the run is recorded, names the file it is recorded in, and says the entry may not survive a power loss. Do not record that run again under another id — the record is there, `-runs` will list it, and committing the registry to git is what makes it durable (ADR 0017). Recording the **identical** entry again is not an overwrite and is not refused: the bytes the install would write are compared against the bytes already on disk, and identical content means nothing is written at all. Anything else under a run id the registry already holds is a genuine collision, and is refused.

### What an entry records

The configuration hash, the configuration itself (a hash identifies a run; it does not reproduce one), the strategy version, the span of input event times, the Variant, the status, the reason it ended, and where its evidence lives — including the journal's **final record hash and record count**, which is what anchors ADR 0017's chain outside the journal and closes its end-truncation gap.

The hash is derived from the recorded configuration, never supplied, and re-derived when the entry is read back, so a run cannot be re-attributed to another Variant by editing one string. An entry filed under the wrong configuration, or in a file not named for its own run id, is refused rather than returned.

**Artefacts are recorded only when this run actually installed that journal.** A journal is installed by hard link, so a run can lose that link to a concurrent run and find a complete, valid, verifiable journal at its own destination — belonging to the other run. Recording it would produce an entry claiming evidence this run did not produce, anchored to another run's chain head; the header cannot tell them apart, since two runs of one configuration have identical headers and a header carries no run id. A run that loses that race is therefore recorded as `failed` with **no artefacts at all**, which is strictly better than a corrupted audit trail.

### The status vocabulary

Closed, and about the **run** rather than the verdict on it:

- `completed` — the run reached the end of its input stream and wrote a journal. A step *after* that which failed — flushing a directory to disk, say — is recorded in the reason and fails the command, but does not make the run something other than completed.
- `failed` — the run stopped before the end of its input stream, or its journal never landed. Whatever partial journal it left is recorded with it, because that is exactly what a reviewer reads.
- `abandoned` — the run was deliberately not carried through. Interrupting a run (Ctrl-C, or `SIGTERM`) records it as this: the signal cancels the run rather than ending the process, so the partial journal it had written is still installed and still anchored, and the entry is still written. The handler stays installed until the command finishes, so a second interrupt will not kill it part-way through recording the run.

An unrecognised status fails closed on the way in *and* on the way out; it is never stored as read. Whether a Variant is adopted or rejected is a judgement made over many runs against ADR 0012's five criteria, and is deliberately not something a single run's own record can assert about itself.

### What is recorded, and what is refused before anything is

Registration begins once the configuration has been **read and accepted**. From there on every outcome is recorded, including one that stopped before the first bar: an unreadable bar fixture under a valid configuration is a failed run with no journal, no span and the reason it never started. A registry that only held runs that got far enough to go wrong interestingly would be a curated one.

Before that line, nothing is recorded, because nothing was performed: an unreadable or invalid configuration, the zero-slippage refusal below, and a `-out` that already exists (refused before the configuration is even read).

### Zero slippage is refused twice

A zero-slippage run is invalid by construction (ADR 0013), and the refusal lives in two places on purpose. The **run** refuses it before a single bar is read, because a refusal that could be avoided by not registering the run would not be one — the invalid run must never produce an equity curve at all. The **registry** refuses it too, whatever the run's status, because it accepts entries it did not itself produce and ADR 0013 names it as the thing that must reject one. Retention is not a licence here: nothing was produced that is evidence of anything.

### Finding a run, and replaying it

```sh
go run ./cmd/backtest -registry runs -runs sha256:<digest>
```

This lists every run recorded under that configuration — id, status, Variant, span, and the journal each one wrote — whatever became of each. Journal paths are resolved against the registry root as they are printed, so a path it names can be fed straight to `-replay`; a run that left no journal says `(no journal)`.

A registry root that does not exist is reported rather than read as an empty one: "this configuration has never been run" and "this registry is not there" are different findings, and only one of them is evidence. For the same reason a hash that is not one is **refused** rather than answered: `sha256:` and exactly 64 lower-case hexadecimal characters (ADR 0016), and no other algorithm. A mistyped digest would otherwise name a directory that happens not to exist, and come back as "no run is recorded" — a typo reading as evidence that a Variant was never run.

## The committed fixture

`cmd/backtest/testdata/` holds a one-instrument fixture and the golden journal it produces: a 32-bar run under a deliberately small test configuration (a 20-bar Entry Channel, a 10-bar Exit Channel, two Units) that opens a Campaign, adds its second Unit inside the breakout bar, and is finally stopped out on a bar that also breached the Exit Channel — leaving the exit proposal for the end-of-stream event to expire.

The golden journal is asserted byte for byte. Regenerate it only deliberately:

```sh
go test ./cmd/backtest -run TestTheCommandTurnsABarFixtureIntoTheGoldenJournal -update
```

A diff in that file is a change in what this system decides, to be read before it is accepted.

The golden run fixes the build identifier (`+test`) rather than taking `internal/buildinfo.Version`, which differs between machines and release builds: a journal asserted byte for byte must record what the platform decided, not which machine decided it. `main` passes the real build, and a separate test holds that wiring in place.

## Reading the recorded decisions

```sh
go run ./cmd/backtest -decisions run.jsonl
go run ./cmd/backtest -decisions run.jsonl -date 2026-01-22 -instrument AAPL
go run ./cmd/backtest -decisions run.jsonl -reference reference.jsonl
```

`-decisions` describes what the journal recorded in sentences, in recording order,
including declined and expired proposals. Each line identifies the decision's
UTC event time and envelope sequence, the instrument (or account), the action,
the recorded reason or supporting figures, and the payload's rule and ADR.
A proposal is described as a proposal; only a recorded fill opens a Campaign.
No strategy conditions are recomputed by this mode.

`-date` selects a UTC **event** date (`YYYY-MM-DD`), not the recording date.
`-instrument` matches the instrument ID exactly; account-wide decisions have no
instrument and are excluded by that filter. Combined filters require both to
match. An empty selection prints no decision lines. These options and
`-reference` require `-decisions`, which joins `-verify`, `-replay`, `-runs`, and
`-diff-want`/`-diff-got` in the one-operation check and rejects backtest run flags.
The separate name keeps a readable record distinct from `-replay`'s test of
whether this build reproduces it.

Both journals pass the existing `journal.Verify`, `Read`, `CheckIdentity`, and
`Split` path before output. Verification is unconditional: an audit log must not
present edited evidence as a decision explanation. This is chain verification
and run-identity checking, not replay equivalence or external registry anchoring.
`journal.CheckSpan` is not present in this checkout; the log makes no additional
span-validation claim. The candidate's decision envelopes and typed payloads
are validated before filtering, and unknown decision types or schemas are
refused before any decision lines are written.

With `-reference`, `replay.Diff` compares the **complete** decision streams,
independent of display filters. Its existing human-readable reporter supplies
the first divergence, printed after the selected decisions and also returned as
an error (nonzero exit). Equal streams end with `no divergence`. There is no
second comparison implementation. Prices and levels use `event.CanonicalBytes`
without rounding or a second float formatter, preserving adjacent float64 values.

Some existing decision schemas do not carry `rule` or `adr`: Setup evaluations,
Campaign evaluations, declined proposals, and engine-state changes. Their lines
explicitly say `rule not recorded; ADR not recorded`. The log does not infer
citations from current code or silently omit those decisions. This leaves the
literal every-line citation requirement unresolved for those historical
payloads; [the provenance follow-up](https://github.com/richard-whittemore/TrendInvesting/issues/126)
tracks the schema decision. A log can explain only recorded evidence; it does
not fabricate a rejection when the producer emitted none. No journal or event
schema is changed by this mode.
