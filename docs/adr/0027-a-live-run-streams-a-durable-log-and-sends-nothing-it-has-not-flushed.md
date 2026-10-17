# ADR 0027: A live run streams a durable log, and sends nothing it has not flushed

- Status: Accepted (2026-10-17, by the owner)
- Accepted together with the owner's decisions of 2026-10-16 recorded as an amendment (PR #348): the durable log is mandatory outside backtests, and no run resumes across builds.
- Date: 2026-10-16
- Relates to: ADR 0014, ADR 0016, ADR 0017, ADR 0018, ADR 0019 (amended 2026-09-24), ADR 0022, ADR 0026 (§3, §5, Consequences)

## In plain English

*A summary. Where it differs from the text below, the text governs.*

A backtest writes its journal once, at the end. A live engine runs for weeks,
so a crash would lose everything it decided since it started, including
orders the adapter may already have sent. So a live engine now also writes a
**durable log** as it goes. For each input it writes the input and every
decision that input caused, then forces it to disk, and only then hands the
decisions to the adapter. An order that is not safely on disk can't be sent.

After a crash, the engine is restarted from that log. It feeds the logged
inputs through a fresh reducer and checks each step reproduces exactly what
the log says. Then it carries on. If the crash cut the last write short, that
unfinished input is left out. None of its decisions were sent, so the
adapter sends that input again. Any other damage stops the restart and needs
a person.

The journal itself doesn't change.

## Context

ADR 0017 composes a journal in memory and writes it in one pass, because its
header states the span of input times and the chain is seeded from the
header. For a backtest the exposure is one run long. For `cmd/engine` it is
the process's lifetime, and decisions cross to an adapter that may act on
them while their only record is in memory (#151).

Three accepted or proposed rules need more than that:

- ADR 0019 (amended 2026-09-24): Degraded and Halted states survive a restart,
  read back from a durable record, and durable retention of inputs and
  terminal decisions is required before further live action.
- ADR 0026 §3: only a released order, once durably journalled, may be sent:
  **journal-before-send**.
- ADR 0026 §5: a restart reads its state from the durable journal and never
  recomputes it from a fresh window of broker data.

Issue #151 weighed three shapes: an incremental journal, a second durability
record beside the journal, and a journal rolled on a fixed boundary.

## Decision

### 1. A durable log beside the journal; ADR 0017's format is unchanged

A live run streams a **durable log**: an append-only file distinct from the
journal. The journal is still composed and written exactly as ADR 0017 states.

An incremental *journal* would need its header before its span is known. ADR
0017 already rejects each way of doing that (a footer, a placeholder header
rewritten at close, a two-pass span). A log whose header states no span
avoids the problem without reopening any of them. Rolling the journal daily
(#151's third option) bounds the loss to a day, but does nothing for
journal-before-send: an order sent at 10:00 would still have no durable record
until the roll.

### 2. The log format

The first line is a header naming the run, with no span:

```
{"log_version":1,"chain_algorithm":"seed=sha256(canonical_log_header_bytes); record=sha256(previous_record_hash||kind||canonical_envelope_bytes)","configuration_hash":"...","strategy_version":"..."}
```

Then, for each input, one **group**:

```
{"record":{"sequence":N,"kind":"input","envelope":{...},"record_hash":"sha256:..."}}
{"record":{"sequence":N+1,"kind":"decision","envelope":{...},"record_hash":"sha256:..."}}
...
{"commit":{"sequence":M,"record_hash":"sha256:..."}}
```

A record is ADR 0017's record, chained by ADR 0017's record formula. The seed
is `SHA-256(canonical_log_header_bytes)`, through `event.CanonicalBytes`, so
re-attributing a log to another configuration or build breaks record 1, as
for a journal. A group is exactly one input followed by the decisions the
reducer returned for it, in the order `journal.Recorder` records them. The
commit names the group's last record and its hash. A group with no commit was
never made durable as a whole.

The log is created exclusively and never appended to by a second process (ADR
0018). A version other than 1 is refused in both directions (ADR 0015's rule).

### 3. The fsync policy

- **One write and one fsync per input.** The whole group and its commit are
  written in one `write` call, then flushed. `Append` returns only after the
  flush succeeds.
- **The header is flushed before the log is used**, and the composition root
  flushes the log's directory after creating it, so the name survives too.
- **A failed write or flush ends the log.** It accepts nothing further and is
  never retried. After a failed `fsync` the kernel may already have dropped
  the unflushed pages, so a retry that reports success proves nothing. The
  recorder refuses every later input, and the run continues only by
  restarting from the log.

At daily bars this is a few hundred flushes a trading day. A flush per record
would add flushes without adding a guarantee, because nothing is released
part way through a group.

### 4. Journal-before-send

A durable `journal.Recorder` appends and flushes an input's group before
`Apply` returns any of that input's decisions. `Apply`'s return value is the
only path a decision has to the adapter (`cmd/engine`'s decider sends nothing
on an error), so the ordering is structural, not a convention a caller must
remember. On any error of its own (durability, the emission ceiling, an
unstampable decision) a durable recorder returns no decisions at all.

The guarantee runs one way: **not durable means never sent**. Durable means
*possibly* sent. A crash after the flush and before the send, or a group
written before a flush that then failed and that survives the crash anyway,
leaves a decision on disk that the adapter never acted on. That is the normal
in-between state of journal-before-send, and it is why restart never resends
(§6). Orders that were logged but may not have been sent are resolved by
deterministic client order IDs, reconnect matching (ADR 0026 §1) and
reconciliation (ADR 0019).

### 5. The truncated-tail rule

`journal.RecoverLog` accepts a log only when it is:

1. a complete, valid header line; then
2. zero or more committed groups, every line complete and valid, the chain
   intact and the sequence contiguous from 1; then optionally
3. a **torn tail**: some complete, valid record lines of *one* uncommitted
   group, followed by at most one final fragment with no newline (a part line,
   or the NUL bytes of a file extended before its data reached the disk).

The torn tail is what a crash during one `Append` leaves. It was never
committed, so none of its decisions was released. It is excluded from what is
recovered and reported with its byte offset and length. **The file is never
modified**: the torn bytes stay as evidence (AGENTS.md rule 6).

Anything else is **damage** and is refused with `journal.LogDamageError`.
That includes a complete line that does not parse, a broken chain, a missing
commit before a later group, a commit naming the wrong record, or NUL bytes
followed by more lines. Trimming damage could drop a committed group whose
orders were sent. A log with no complete header is refused too: nothing was
ever recorded in it.

This rule is deliberately strict. A disk that persisted pages out of order
during the crash can leave a valid commit after a hole. That is refused
rather than guessed at, and goes to a person.

### 6. Restart

`cmd/engine -log <new> -resume <old>`:

1. recovers `<old>` under §5 and refuses a log recorded under another
   configuration hash or strategy version, or one whose first input is not the
   configuration input this invocation composed (another `-config` or
   `-as-of`). These checks run before `<new>` is created;
2. feeds every recovered input, configuration first, through the same
   `replay.Engine` the socket uses (so the contiguity cursor ends where the
   log ends) into a fresh reducer wrapped by a durable recorder on `<new>`;
3. requires each input to regenerate **exactly** the group the log records,
   compared by kind and canonical envelope bytes (ADR 0016's definition of
   the same envelope). A difference stops the restart with
   `journal.ResumeDivergenceError`. An error the reducer also returned in the
   recorded run (a refused input, a halt) is part of that run and does not.

Under the same build and configuration, `<new>` therefore begins with the
committed part of `<old>`, byte for byte, and continues from there. The
reducer's state, including Degraded or Halted when ADR 0019's states exist,
is the recorded run's, read back from the log (ADR 0026 §5). Decisions
regenerated during the restart are **not sent**. The adapter's next input
continues the recovered sequence. A torn input is sent again.

### 7. What is bounded, and what is not

- **A crash loses at most the one input whose `Append` it interrupted**, and
  none of that input's decisions were released. This is the bound #151 asks
  to be stated.
- The journal is still written at shutdown. A resumed process's journal
  covers the whole run, the recovered part included. It verifies under
  `journal.Verify` and replays byte-identically. A journal can also be
  composed from a log directly (`Recovery.JournalHeader` and `journal.Write`).
- Memory is not bounded by this decision: the recorder still holds the run
  until shutdown, under ADR 0017's record bound.

## Alternatives rejected

- **Stream the journal itself.** ADR 0017 rejects every way of writing its
  span-seeded header before the span is known. Reopening that is a change to
  an accepted ADR and to every journal reader, for no guarantee the log does
  not already give.
- **Roll the journal daily.** Bounds the loss, but not journal-before-send.
- **One line per group.** Commits are atomic per line, but a group can hold
  `MaxEmissionsPerInput` decisions, far past the 8 MiB line bound a reader
  needs to resist a corrupt file.
- **Truncate a torn tail in place and keep appending.** Edits recorded
  evidence. A new log per process keeps every file write-once.
- **Accept any unparseable tail as torn.** It could silently drop a committed
  group whose orders were sent.

## Consequences

- `internal/journal` gains `CreateLog`, `LogWriter`, `RecoverLog`,
  `NewDurableRecorder` and `Resume`. The package still opens no path, because
  the composition root hands it a `SyncWriter` (an `*os.File`).
- `cmd/engine` gains `-log` and `-resume`. **`-log` is optional in this
  slice**, so existing invocations, including the LEAN adapter's end-to-end
  test, are unchanged. Making it mandatory before paper trading is an owner
  decision.
- **A restart must use the same build.** The strategy version, which includes
  the build (ADR 0016), is stamped on every decision. Another build therefore
  regenerates different bytes, and the restart is refused. Restarting across
  an upgrade needs its own decision.
- A refused log (§5) or a refused restart (§6) needs a runbook entry: what an
  operator checks, and how a new run is started from a reconciled baseline
  (ADR 0019's recovery procedure).
- Every existing journal, journal test and replay test is unchanged.
