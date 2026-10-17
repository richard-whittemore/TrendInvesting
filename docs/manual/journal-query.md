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
| `refusals` | refused orders and engine-state changes (degraded, halted) |
| `totals` | fills, shares bought and sold, and commission per instrument per year, plus a TOTAL row |
| `positions` | the holding after each fill, rebuilt from the fills |
| `reconcile` | whether the fills agree with the account snapshots recorded in the journal |

Flags, all optional: `-account A1`, `-instrument AAA`, `-from 2026-01-01`,
`-to 2026-12-31` (UTC dates, both ends inclusive of the whole day) and
`-format text|csv|json` (default `text`). You can name several journals; the
first column shows which file each row came from.

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
then moves the position: entries and adds buy, stops and exits sell. At every
later snapshot the tool compares the position the fills imply with the one
the snapshot reports, instrument by instrument, and lists every comparison,
not only the failures. After a comparison the snapshot becomes the new
starting point, so one mismatch is reported once and not repeated.

Fills do not name an account. They are counted toward the journal's account
when the journal names exactly one.

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
- **A durable log (the live engine's crash-safe log, ADR 0027).** The tool
  recognises the form and refuses it by name for now. Reading it is a separate
  piece of work, waiting on that log reader landing.
- **A record whose values are invalid** (for example a fill with a negative
  quantity or an unknown kind, which a journal can keep as a rejected input).
  The chain only proves the file was not altered, so the tool also checks each
  record's values and refuses the file, naming the record, before it prints
  any figure.
- **The same fill delivered more than once.** A fill id that appears again
  with identical content is one execution and is counted once. The same id
  with different content is refused, naming both records.
- **A record from a newer or older build** (a schema version this build does
  not read, including a newer account snapshot). Refused, naming the record; the
  tool never guesses at a figure. Only the equity-and-cash snapshot (version 2),
  which holds no positions, is passed over by design.
- **A bad flag** (a malformed date, a backwards range, an unknown format or
  report). Refused before any file is read.

## What backs it

- ADR 0017 (journal tamper-evidence) defines the journal and its hash chain,
  which the tool verifies with the same code the engine uses (`journal.Verify`).
- ADR 0027 (durable log) defines the live log form; see above for its status.
- Tests: `cmd/journal-query/main_test.go` drives the command line against
  small hand-built journals whose totals are added up by hand, including
  tamper refusal, byte-identical files after every report and format,
  date and account filters, CSV and JSON, and a deliberate position
  disagreement. `internal/journalquery/journalquery_test.go` covers the query
  logic itself.
- The tool opens journals read-only and its query code receives bytes, never
  a file handle, so it cannot write.
