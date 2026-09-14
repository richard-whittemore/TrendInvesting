# The run registry

Every backtest — successful, failed, or abandoned — is recorded here, under the configuration hash that identifies it (ADR 0012). Nothing in this directory is ever rewritten or deleted (AGENTS.md rule 6): the graveyard of failed Variants is the point, because a Variant that is still standing can only be judged against the ones that are not.

```
runs/
  sha256-<digest>/            # the configuration hash, ':' written as '-'
    <run-id>.json             # one file per run
```

One file per run, and **no index file**. The layout is the index: a manifest would be a single mutable blob that every run rewrote and that two branches would conflict over on every merge, whereas two runs never write the same path here.

An entry records the configuration itself as well as its hash — a hash identifies a run, it does not reproduce one — together with the strategy version, the span, the Variant, the status, why the run ended, and where its evidence lives, including the journal's final record hash. That last value is what anchors ADR 0017's chain from outside the journal.

Runs are recorded by `cmd/backtest -registry`, and read back by `cmd/backtest -registry ... -runs <configuration-hash>`. See `docs/running-a-backtest.md`.
