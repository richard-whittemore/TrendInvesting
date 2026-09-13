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
- `-out` is where the journal is written. **An existing file is never overwritten**: a journal is recorded evidence (AGENTS.md rule 6), so the command refuses and asks you to move it aside or choose another path. There is no overwrite flag. The write goes through a temporary file in the same directory and is renamed into place, so an interrupted run leaves nothing partial behind.

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

## The committed fixture

`cmd/backtest/testdata/` holds a one-instrument fixture and the golden journal it produces: a 32-bar run under a deliberately small test configuration (a 20-bar Entry Channel, a 10-bar Exit Channel, two Units) that opens a Campaign, adds its second Unit inside the breakout bar, and is finally stopped out on a bar that also breached the Exit Channel — leaving the exit proposal for the end-of-stream event to expire.

The golden journal is asserted byte for byte. Regenerate it only deliberately:

```sh
go test ./cmd/backtest -run TestTheCommandTurnsABarFixtureIntoTheGoldenJournal -update
```

A diff in that file is a change in what this system decides, to be read before it is accepted.

The golden run fixes the build identifier (`+test`) rather than taking `internal/buildinfo.Version`, which differs between machines and release builds: a journal asserted byte for byte must record what the platform decided, not which machine decided it. `main` passes the real build, and a separate test holds that wiring in place.
