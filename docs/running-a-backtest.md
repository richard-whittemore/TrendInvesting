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
- `-out` is where the journal is written. **An existing file is never overwritten**: a journal is recorded evidence (AGENTS.md rule 6), so the command refuses and asks you to move it aside or choose another path. There is no overwrite flag. The write goes through a temporary file in the same directory, which is then linked into place — a hard link rather than a rename, because a rename would silently replace a journal that appeared while the run was in progress, whereas a link fails atomically. So an interrupted run leaves nothing partial behind, and two runs racing for the same path end with one journal and one clear error rather than one journal overwritten by the other.

## What it writes

A journal: the complete ordered stream of the run's input and decision events, one record per line, under a header. The header states the configuration hash and strategy version — both derived from the configuration actually run (ADR 0016), never supplied on the command line — the span of input event times covered, the chain algorithm, and the journal format version.

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

One invocation performs exactly one operation. `-verify`, `-replay` and `-runs` are mutually exclusive with each other and with the flags that describe a run to perform (`-config`, `-bars`, `-out`, `-run-id`); asking for two is refused rather than silently given one of them. `-registry` is the one flag two operations share — it names where a run is recorded, and where recorded runs are read from — so it is refused alongside `-verify` or `-replay`, which would ignore it.

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

### What an entry records

The configuration hash, the configuration itself (a hash identifies a run; it does not reproduce one), the strategy version, the span of input event times, the Variant, the status, the reason it ended, and where its evidence lives — including the journal's **final record hash and record count**, which is what anchors ADR 0017's chain outside the journal and closes its end-truncation gap.

The hash is derived from the recorded configuration, never supplied, and re-derived when the entry is read back, so a run cannot be re-attributed to another Variant by editing one string. An entry filed under the wrong configuration, or in a file not named for its own run id, is refused rather than returned.

**Artefacts are recorded only when this run actually installed that journal.** A journal is installed by hard link, so a run can lose that link to a concurrent run and find a complete, valid, verifiable journal at its own destination — belonging to the other run. Recording it would produce an entry claiming evidence this run did not produce, anchored to another run's chain head; the header cannot tell them apart, since two runs of one configuration have identical headers and a header carries no run id. A run that loses that race is therefore recorded as `failed` with **no artefacts at all**, which is strictly better than a corrupted audit trail.

### The status vocabulary

Closed, and about the **run** rather than the verdict on it:

- `completed` — the run reached the end of its input stream and wrote a journal.
- `failed` — the run stopped before the end of its input stream. Whatever partial journal it left is recorded with it, because that is exactly what a reviewer reads.
- `abandoned` — the run was declared and deliberately not carried through, or its output discarded.

An unrecognised status fails closed on the way in *and* on the way out; it is never stored as read. Whether a Variant is adopted or rejected is a judgement made over many runs against ADR 0012's five criteria, and is deliberately not something a single run's own record can assert about itself.

### Zero slippage is refused twice

A zero-slippage run is invalid by construction (ADR 0013), and the refusal lives in two places on purpose. The **run** refuses it before a single bar is read, because a refusal that could be avoided by not registering the run would not be one — the invalid run must never produce an equity curve at all. The **registry** refuses it too, whatever the run's status, because it accepts entries it did not itself produce and ADR 0013 names it as the thing that must reject one. Retention is not a licence here: nothing was produced that is evidence of anything.

### Finding a run, and replaying it

```sh
go run ./cmd/backtest -registry runs -runs sha256:<digest>
```

This lists every run recorded under that configuration — id, status, Variant, span, and the journal each one wrote — whatever became of each. Journal paths are resolved against the registry root as they are printed, so a path it names can be fed straight to `-replay`; a run that left no journal says `(no journal)`.

A registry root that does not exist is reported rather than read as an empty one: "this configuration has never been run" and "this registry is not there" are different findings, and only one of them is evidence.

## The committed fixture

`cmd/backtest/testdata/` holds a one-instrument fixture and the golden journal it produces: a 32-bar run under a deliberately small test configuration (a 20-bar Entry Channel, a 10-bar Exit Channel, two Units) that opens a Campaign, adds its second Unit inside the breakout bar, and is finally stopped out on a bar that also breached the Exit Channel — leaving the exit proposal for the end-of-stream event to expire.

The golden journal is asserted byte for byte. Regenerate it only deliberately:

```sh
go test ./cmd/backtest -run TestTheCommandTurnsABarFixtureIntoTheGoldenJournal -update
```

A diff in that file is a change in what this system decides, to be read before it is accepted.

The golden run fixes the build identifier (`+test`) rather than taking `internal/buildinfo.Version`, which differs between machines and release builds: a journal asserted byte for byte must record what the platform decided, not which machine decided it. `main` passes the real build, and a separate test holds that wiring in place.
