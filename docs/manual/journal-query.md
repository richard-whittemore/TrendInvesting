# journal-query: asking a run what happened

## What it does and why it exists

`journal-query` reads the journals a run left behind and answers questions
about them in tables: which orders were planned, released and refused; which
fills happened, at what quantity, price and commission; what you held after
each fill; how much commission you paid per instrument per year; and when the
engine refused something or went degraded or halted.

It exists so you do not have to read raw files to find out what your system
did. It only reads. It never changes a journal and never talks to the engine
or the broker.

## What a journal is

A journal is the engine's logbook for one run: one line per event, in order,
under a header naming the configuration the run used. Each line carries a
fingerprint (a hash) built from the line before it, so changing or removing
any line in the middle breaks every fingerprint after it. That makes an edited
journal detectable (ADR 0017). The tool checks the fingerprints before it
reports anything.

## Running it

Build or run from the repository root:

```
go run ./cmd/journal-query <report> [flags] <journal> [<journal> ...]
```

Reports:

| Report | Answers |
| --- | --- |
| `orders` | orders planned, released and refused, and order lifecycle changes |
| `fills` | each fill with quantity, price and commission |
| `refusals` | refused orders, withheld sales back to 98% and engine-state changes (degraded, halted) |
| `totals` | fills, shares bought and sold, and commission per instrument per year, plus a TOTAL row |
| `positions` | the holding after each fill, rebuilt from the fills; a recorded symbol change moves the holding to the new id |
| `reconcile` | whether the fills agree with the account snapshots recorded in the journal |
| `operator` | your own instructions the engine recorded: kill switches, approvals and rebalance clearances (not acknowledgements), with who recorded each and why ([operator](operator.md), "Seeing what was recorded") |

Flags, all optional: `-account A1`, `-instrument AAA`, `-from 2026-01-01`,
`-to 2026-12-31` (UTC dates, both ends inclusive of the whole day) and
`-format text|csv|json` (default `text`). You can name several journals. The
`orders`, `fills`, `refusals`, `positions`, `reconcile` and `operator`
reports show the source journal in the first column; `totals` combines all selected journals.

Name one file per run. A run resumed from a crash writes a new log that begins
with every record of the log it resumed, and a second copy of a journal holds
all of its records again, so naming either pair would count that history twice.
The tool refuses such files and names the two that overlap. It decides this
from the history, not from a record that merely looks alike: two files are the
same history when, over the sequence numbers both hold, every record is
identical (same sequence, id, type, payload, event time, recorded time,
strategy version and configuration) and at least one of them is not the
configuration record. The configuration record is ignored because independent
runs of one configuration all start with the same one. Independent runs of the
same configuration, even over overlapping dates, can therefore be named
together.

## Example questions

All examples use made-up numbers.

How much commission did I pay on each instrument each year?

```
$ journal-query totals trading.jsonl
instrument  year  fills  bought  sold  commission
AAA         2025  2      150     0     1.50
AAA         2026  1      0       150   1.50
BBB         2025  1      200     0     2.00
BBB         2026  1      0       200   2.00
TOTAL       all   5      350     350   7.00
```

What happened to BBB in one period?

```
$ journal-query fills -instrument BBB -from 2025-06-01 -to 2026-12-31 trading.jsonl
```

Why was an order not sent, and did the engine change state?

```
$ journal-query refusals -instrument BBB orders.jsonl
journal      seq  time        account  instrument  event         reason                          detail
orders.jsonl 5    2026-03-02  A1       BBB         refused       order-size                      over-limit
orders.jsonl 6    2026-03-02  A1       BBB         engine-state  degraded:risk-limit-breached    breach
```

Do my fills explain the positions the broker reported?

```
$ journal-query reconcile positions.jsonl
journal          seq  time        account  instrument  fills  snapshot  status
positions.jsonl  5    2026-01-10  A1       AAA         120    120       ok
positions.jsonl  5    2026-01-10  A1       BBB         10     10        ok
positions.jsonl  7    2026-01-20  A1       AAA         0      20        DISAGREES
positions.jsonl  7    2026-01-20  A1       BBB         10     10        ok
1 disagreement(s) between the fills and the account snapshots
```

For a spreadsheet or another program, add `-format csv` or `-format json`.
Both carry the same figures as the text table.

### How `positions` and `reconcile` work

An account's first snapshot is its starting point (its baseline). Each fill
then moves the position: entries, adds and buys buy; stops, exits and sells
sell. A Turtle fill's `kind` is entry, add, stop or exit; a target-weight
(Boost or momentum IRA) fill's `kind` is its side, buy or sell. At every
later snapshot the tool compares the position the fills imply with the one
the snapshot reports, instrument by instrument, and lists every comparison,
not only the failures. After a comparison the snapshot becomes the new
starting point, so one mismatch is reported once and not repeated.

Turtle fills do not name an account. They are counted toward the journal's
account when the journal's records name exactly one account, counting every
record that names one (a target-weight fill included). Target-weight fills
name their own account.

For a target-weight account's limit orders, an `orders` lifecycle row shows
the order's side, its limit price and, in `note`, its client order id; a
Turtle stop order's row shows its stop price and its tag. A dry-run engine's
own synthetic report on an order it never sent (version 3, see `engine.md`)
shows as `synthetic-lifecycle-canceled`; it is never an execution, so it
adds nothing to `fills`, `totals` or `positions`.

## What can go wrong, and what the tool does

- **The journal does not verify** (edited, damaged, or a line removed in the
  middle). The tool refuses the file, prints nothing from it and exits with an
  error that names the file and the record where the chain breaks, for example
  `refused, the hash chain does not verify ... broken at record 3`. If you
  name several journals, one bad file refuses the whole query so you never read
  a partial answer. Dropping lines from the very end is not detectable by the
  chain alone; the run registry's recorded final hash covers that (ADR 0017).
- **The file is missing or unreadable.** The tool stops with the operating
  system's error and prints nothing.
- **The fills disagree with an account snapshot.** The `reconcile` table is
  printed in full, the disagreeing rows say `DISAGREES`, and the command exits
  with an error counting them. Treat this as a reason to investigate before
  trusting either the fills or the snapshot; the tool does not decide which
  is right.
- **Several accounts in one journal and a fill to place.** The tool refuses
  `positions` and `reconcile` rather than guess which account a fill belongs
  to.
- **A durable log (the live engine's crash-safe log, ADR 0027).** Read the
  same way as a journal, through the journal package's own log reader, which
  checks the chain and every commit. Only committed groups are reported: the
  uncommitted tail a crash can leave holds no trusted figure and is left out.
  A damaged log (not merely torn) is refused whole, naming the file.
- **A record whose values are invalid** (for example a fill with a negative
  quantity or an unknown kind, which a journal can keep as a rejected input).
  The chain only proves the file was not altered, so the tool also checks each
  record's values and refuses the file, naming the record, before it prints
  any figure.
- **The same fill delivered more than once.** A fill id that appears again
  with the same execution (an absent and an empty unit list are the same, and
  one instant is the same in any UTC offset) is one execution and is counted once. The same id
  with different content is refused, naming both records.
- **A record from a newer or older build** (a schema version this build does
  not read, including a newer account snapshot). Refused, naming the record; the
  tool never guesses at a figure. It reads fills of versions 4 (Turtle) and 5
  (target-weight), order lifecycle changes of versions 1 (Turtle stop orders),
  2 (target-weight limit orders) and 3 (a dry-run's synthetic report; a
  synthetic report at version 2, or a venue's at version 3, is refused), planned orders of versions 1 and 2 (2 is a
  sale back to 98%), risk-gate releases and refusals of versions 1, 2 and 3
  (3 adds the broker figures and the kill-switch, broker-holding,
  sale-attempted and stale-broker-snapshot reasons, and the refusal's
  alert), withheld sales back to 98% of version 1 (`risk.sale.withheld`,
  listed by `refusals` with their reason, `lapsed` included), and account
  snapshots of versions 3 (positions) and 4
  (positions and cash; the cash is not reported). Only the equity-and-cash
  snapshot (version 2), which holds no positions, is passed over by design.
- **A bad flag** (a malformed date, a backwards range, an unknown format or
  report). Refused before any file is read.

## What backs it

- ADR 0017 (journal tamper-evidence) defines the journal and its hash chain,
  which the tool verifies with the same code the engine uses (`journal.Verify`).
- ADR 0027 (durable log) defines the live log form, read as described above.
- Tests: `cmd/journal-query/main_test.go` drives the command line against
  small hand-built journals whose totals are added up by hand, including
  tamper refusal, byte-identical files after every report and format,
  date and account filters, CSV and JSON, and a deliberate position
  disagreement. `internal/journalquery/journalquery_test.go` covers the query
  logic itself.
- The tool opens journals read-only and its query code receives bytes, never
  a file handle, so it cannot write.
