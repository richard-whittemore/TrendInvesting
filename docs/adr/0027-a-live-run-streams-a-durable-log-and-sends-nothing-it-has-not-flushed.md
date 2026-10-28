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
- `cmd/engine` gains `-log` and `-resume`. **`-log` was optional in this
  slice**, so existing invocations, including the LEAN adapter's end-to-end
  test, were unchanged. Superseded by the amendment below, which makes it
  mandatory outside `-mode backtest`.
- **A restart must use the same build.** The strategy version, which includes
  the build (ADR 0016), is stamped on every decision. Another build therefore
  regenerates different bytes, and the restart is refused. Superseded by the
  amendment below: a run does not resume across builds, and an upgrade starts a
  fresh run.
- A refused log (§5) or a refused restart (§6) needs a runbook entry: what an
  operator checks, and how a new run is started from a reconciled baseline
  (ADR 0019's recovery procedure).
- Every existing journal, journal test and replay test is unchanged.

## Amendment (2026-10-16, owner decisions #336, #337; accepted with the ADR 2026-10-17)

*This section records two decisions the owner made on 2026-10-16. The owner
accepted them together with this ADR on 2026-10-17.*

- **The durable log is mandatory before paper trading (#336).** This replaces
  the Consequences' statement that `-log` is optional. `cmd/engine` takes
  `-mode backtest|dry-run|paper|live`, which is required and has no default,
  so a live run that forgets it is refused rather than run without the log. In
  any mode but `backtest` it refuses to start without `-log`. A backtest,
  including the LEAN adapter's end-to-end test (which passes `-mode backtest`),
  keeps `-log` optional. The mode is a
  declaration by the person starting the engine; the live engine's
  configuration naming its adapter (ADR 0025 point 3, still to be built) is
  what will later bind a live adapter to a live mode.
- **A build upgrade starts a fresh run; it never resumes (#337).** This
  settles the question Consequences leaves open. An upgrade stops the run at
  the end of a session, with the old log kept as evidence, and starts a fresh
  run on the new build from a reconciled baseline (ADR 0019 recovery). There
  is no resume across builds, and the build part of the strategy version is
  never ignored when comparing decisions. `-resume` therefore refuses a log
  recorded by another build, and its message names this procedure. The
  runbook is `docs/runbooks/durable-log.md`.

## Amendment (2026-10-17, owner decision #365, accepted)

*The owner chose the restart model on 2026-10-16 and accepted this section,
which records how `cmd/engine` implements it, on 2026-10-17.*

- **A crash resumes; every other restart is a fresh run.** A crash is
  resumed with `-resume` as §4 and §6 describe. The crashed run is always
  the latest, so `-resume` must name the most recent run in the directory
  of `-log`, where an abandoned resume attempt is not a run (§6; amended
  below, "Abandoned resume attempts"); an older log is refused. Leaving Halted, a build
  upgrade, a cleared rebalance, a reboot or any deliberate stop is a fresh
  run with a new log. The decision-prelude model (feeding an earlier run's
  decisions into a new one, PR #364) is dropped.
- **A fresh run after an earlier one needs the owner's recorded approval.**
  In any mode but `backtest`, when the directory of `-log` already holds a
  durable log (`*.log`), the engine refuses to start without `-approval`, a
  file holding an `operator.run.approved` v1 payload: the account, the
  latest earlier log's file name and its final record hash, the incident it
  clears (if any), who approved and when, and the state in force: Boost's
  latch, and the incident in force (null when none) with the engine state
  and scope it holds the account in (Degraded with its instruments, or
  Halted; ADR 0019, amended 2026-09-24, "The state survives a restart") and
  whether its one sale back to 98% was attempted and the attempt's client
  order ID if sent (ADR 0019, amended 2026-10-15: one attempt per
  incident). Every key is
  required, null where there is none, and an unknown key is refused, so no
  state is a default. The engine checks that the log named is the most
  recent run in the directory (as above), that its last committed record
  hash is the one approved, and that the approved latch is the one that log
  ends with (its last decision's `latched_after`, or its own approval's
  latch when it decided nothing), and that the approval does not loosen
  the engine state that log ends in (the incident its own approval
  carried in force, then its `strategy.engine.state` decisions, since a
  restored incident records no state decision): it either clears the incident by its ID or carries it in
  force as Halted, or as Degraded over at least the same instruments. It
  then records the approval as the run's second input (sequence 2,
  straight after the configuration), so the adapter's first input carries
  sequence 3. The target-weight handler starts from the approved latch, in
  the incident's state and scope before any market input, and a sale
  recorded as attempted is never attempted again by the fresh run.
  `-approval` with a backtest or with no earlier run is refused, and so is
  `-approval` with `-resume`, except for one case: a fresh run records its
  configuration and its approval one input at a time, so a crash between
  them leaves a log holding nothing past its configuration. Such a log, in
  a directory holding an earlier run, resumes only with `-approval` naming
  that earlier run, checked as above and recorded as its second input;
  resumed alone, it would start from no latch and no incident. Its resume
  records the configuration before the approval, so a second crash in that
  window leaves another such log: a log recorded under the same
  configuration and build, opening with the same configuration input and
  holding nothing past it, is an attempt at the same start, and the earlier
  run the approval names is the latest log that is not one. A first run cut
  short likewise resumes without an approval however many attempts there
  are. So that no fresh run can be taken for such an attempt, a fresh run
  whose configuration input is the one the latest log opens with, when that
  log holds nothing past it, is refused: resume that log, or declare a new
  start with `-as-of`.
- **Build identity.** `-mode dry-run`, `paper` and `live` refuse an
  unstamped build ("dev"); `make engine` and `deploy/build-engine.sh` stamp
  it with `scripts/build-stamp.sh`: `git describe --always --dirty`, and for
  a dirty tree, one holding an untracked file that is not ignored included
  (`go build` compiles it), the hash of its whole working tree after
  "-dirty-", so two
  different uncommitted changes are two builds. A bare "-dirty" stamp, which
  names every dirty tree at its commit alike, is refused by `-resume`.
  `deploy/build-engine.sh` still refuses a dirty tree for the server.
- **No engine starts at boot.** The provisioner enables no engine and
  disables every engine instance an earlier provisioner enabled.
- **Mode binding.** `-mode` must equal the mode in the hashed configuration;
  a Turtle configuration, which declares none, runs only as a backtest. A
  paper configuration must name an IBKR paper account (ID beginning "DU"),
  and a live one must not.
- **Not decided here:** the engine cannot tell a crashed log from one a clean
  stop ended, so choosing `-resume` is the owner's judgement, guided by the
  runbook (`docs/runbooks/durable-log.md`, "Restarting an engine").

## Amendment (2026-10-17, later the same day: abandoned resume attempts)

*This section corrects how `cmd/engine` implements the restart model the
owner decided (#365, above); it changes no owner decision. The sentence
"`-resume` must name the most recently modified log" above is amended to
"the most recent run, where an abandoned resume attempt is not a run".*

- **An abandoned resume attempt is not a run.** A resume creates its new log
  and then copies the old log's committed groups into it one by one, the
  configuration first (§6). A resume refused for divergence, or one that
  crashes before or during the copy, therefore leaves a newer log whose
  committed records are a prefix of the older log's under the same header:
  empty, the configuration only, or more. Such a log is an abandoned
  attempt. It is never the latest run, never the run an approval names and
  never the run before a start; `-resume` of it is refused, naming the log
  it copied; it stays in the directory as evidence (ADR 0018). This
  replaces the attempt-at-the-same-start rule above: a second crash of a
  resume of a start cut short leaves a prefix of the first attempt, so the
  first attempt is the one resumed, with the same approval, and a first run
  cut short resumes without one. A log holding nothing past its
  configuration that is no such prefix is a start cut short, and stays a
  run.
- **A fresh run never repeats an earlier log's start.** So that a fresh run
  cut short can never be taken for an abandoned resume, a fresh run whose
  configuration input is the one any earlier log in the directory opens
  with, under the same header, is refused: declare a new start with
  `-as-of`. (A fresh run always starts at the first session it decides,
  after the previous run's.)
- **An approval of a log that ended Normal carries no incident in force.**
  It clears none and carries none: `incident_in_force` is the incident the
  previous run ended in, so for a log recording no incident any value but
  `null` is refused.
- **An unrecoverable log is set aside, never edited or deleted.** Every
  `*.log` in the root of the directory of `-log` is read to find the latest
  run, so a log that cannot be recovered (§5) refuses every start, and the
  refusal names the step: move the log, unchanged, into the `quarantine`
  folder in that directory. The engine reads only the directory's root, so
  it never reads the quarantine folder.

## Amendment (2026-10-17, accepted by the owner 2026-10-18: re-delivery on request, and an order's lifecycle state)

*Accepted by the owner on 2026-10-18. It adds to §4 and §6 and changes no
owner decision: the engine still never resends anything on its own.*

- **The adapter asks for a committed reply by sending the input again.** An
  input whose ID names a committed group (§2) is answered with the reply
  built from that group and is not applied again, so the same input never
  yields a second order. The reply is byte-identical however often it is
  asked for, before a restart or after one: it is built from the group, and
  its own `recorded_at` is its input's, never the wall clock. A refused
  input's reply is its refusal, repeated. An input sent under a committed
  input's ID with different content is refused and records nothing: an input
  ID names one input. An input that was never committed (refused before it
  was recorded, or the torn tail of §5) has no committed reply and is applied
  as new.
- **§6's "decisions regenerated during the restart are not sent" stands.**
  Resume rebuilds the index of committed replies from the log; nothing is
  pushed to the adapter. After a crash the adapter, which alone knows what it
  received, asks again for the input it holds no reply to. That closes §4's
  in-between state for an order the adapter never received: it is served
  again, unchanged, under the same client order ID.
- **The adapter's obligations.** It keeps the input awaiting its reply on
  durable storage until it has acted on the reply, asks for that input again
  after its own or the engine's restart before sending anything new, sends
  its next input only once it holds the reply to its last, and hands each
  released order marked `send` to the broker once, never one the broker
  already holds under that client order ID (reconnect matching, ADR 0026
  point 1).
- **An order's lifecycle state is read from the log.** *Committed*: its
  `risk.order.released` is in a committed group. *Released*: a later input is
  committed, which by the adapter's obligation above shows it holds the
  reply. *Acknowledged*: the venue's report naming its client order ID is
  committed (`execution.order.lifecycle` version 2, or `execution.fill`
  version 5). No new record type is needed.
- **An order not acknowledged in time is an incident** (ADR 0019,
  "Alerting"): the reporter raises one urgent alert per order marked `send`
  that is not acknowledged within its configured bound of its commit, and,
  once it is acknowledged, a routine notice that it cleared.

## Amendment (2026-10-18, owner decision: the starting book in the Run approval)

A fresh run's first reading after a close must never be adopted as the book
unchecked (ADR 0019, amended 2026-10-18): the IBKR adapter compares it with
the previous close's book, found back through the chain of runs the Run
approvals name. An account's first run has no earlier run, so its book is
stated by the owner instead:

- **`operator.run.approved` schema version 3** adds `starting_book`: the
  holdings in raw shares (strictly ascending, none zero; `[]` when none),
  IBKR's total cash and its currency, as the owner checked them in IBKR. The
  key is required, `null` when the approval states none. Versions 1 and 2
  stay readable and state none.
- **`operator.run.approved` schema version 4** (amendment of 2026-10-22)
  adds `incident_in_force.sale_retired_attempts`: how many of the
  incident's sales back to 98% the approved log, and the approvals before
  it, record as retired as never sent by a retirement the engine applied.
  Each had a client order ID of its own, so the fresh run's next attempt
  takes the next one (ADR 0026, point 2, "Order identity"). It is left out
  when none; the engine refuses an approval stating fewer than the log
  records. Versions 1 to 3 stay readable and state none.
- **`operator.run.approved` schema version 5** (ADR 0019's refinement of
  2026-10-22, "A sale an ended run never sent") adds
  `incident_in_force.sale_settlements`: the IBKR adapter's settlement of
  each of the incident's sales back to 98% the approved log released and
  the adapter never sent, written once that run ended (the log by file
  name, the run, the releasing input, the adapter's evidence, and the
  owner's retirement it amounts to). Each is counted in
  `sale_retired_attempts`; the engine applies each to the approved log by
  its own retirement rule before the fresh run starts and refuses the
  approval for one the log does not bear out. It is left out when none.
  Versions 1 to 4 stay readable and carry none.
- **A first run's approval.** It names no previous log and no final hash,
  states the starting book, and states the boost not latched and no
  incident, since no earlier run had either. Its input ID is
  `operator-run-approved:first-run:<the run's -as-of>:<the payload's
  hash>`, so the run's identity is its approval's, as a fresh run's is. In
  `-mode paper` or `live` an account's first run decides nothing without it
  (`operator approve -first-run -positions ... -cash ... -currency ...`);
  another first run may be given one. A first run cut short before its
  approval was recorded resumes with the same approval, as a fresh run's
  does. A first run's approval given to a directory that holds an earlier
  run, or a fresh run's given to one that holds none, is refused.
- **A fresh run's approval** may state a starting book too: the book the
  owner confirms after a change no fill explains (a transfer, a deposit),
  which then is the latest book the first reading is compared with.
- **The chain.** It ends at a first run's approval or at a run with none;
  the adapter's comparison walks it to its end, however old the runs, while
  its reconciliation of IBKR's lists still reads only the runs those lists
  can show (relay.run_logs).

## Note (2026-10-28): an IRA's approval states its rebalance in progress

The approval's "state in force" was Boost's latch. A momentum run (an IRA)
has no latch; what a fresh run of it cannot recompute is the rebalance in
progress, which ADR 0026, point 5 says a restart sends nothing for until the
owner clears it. So:

- **`operator.run.approved` schema version 6** adds `momentum`, stated by an
  IRA's approval only: `{"rebalance_in_progress": null}` when the approved
  log ends with every order terminal, or the rebalance's decision ID,
  decision time, phase (`sells-working`: its sells working and its buys never
  planned; `buys-working`: every sell terminal and its buys working) and
  every order not terminal. An IRA's approval states `"boost": null`; a Boost
  approval states no `momentum` key and is otherwise unchanged. An approval
  states one strategy's state, never both. Versions 1 to 5 stay readable and
  state no momentum state.
- **The engine checks it** against the approved log before it starts, by
  the run's own rule: every input the log records is applied, in order, to
  a fresh target-weight handler built from the log's configuration, which
  then states the rebalance in progress. The log commits a refused input
  with no mark of the refusal, and a refusal changes nothing, so an approval
  the run refused never counts and the one it accepted, wherever it stands,
  carries its rebalance until a clearance the run read ends it. An approval
  stating none where one is, or one the log does not hold, is refused, and
  so is a Boost-shaped approval for a momentum configuration, or an IRA one
  for Boost. When the approved log is a start cut short and no earlier run
  holds more, its own configuration is the state: no rebalance in progress.
- **The fresh run holds**: it withholds every decision as
  `rebalance-in-progress`, planning nothing, until the owner's
  `operator.rebalance.cleared` names that rebalance and exactly its orders;
  the next decision time then decides afresh (target-weight rules version
  1.26.0). `operator approve` reads the strategy from the log and writes
  either shape; `-first-run -strategy momentum` writes an IRA's first run's.
