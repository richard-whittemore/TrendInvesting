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

On the server it is installed: `deploy/build-engine.sh` builds it beside
the engine from the same commit, and `deploy/provision.sh` installs it in
`/opt/trend-investing/bin` and links it into `/usr/local/bin`. The logs
belong to the engine's user, so run it as that user:

```
sudo -u ti-trader journal-query <report> [flags] /var/lib/trend-investing/engines/<instance>/<run>.log
```

Each run leaves two files in `/var/lib/trend-investing/engines/<instance>/`:
`<run>.log`, the durable log written as it goes, and `<run>.journal`, the
journal written when it stops. Name one of them, not both (below). The log
can be read while the engine is still running.

On the Mac, from the repository root:

```
go run ./cmd/journal-query <report> [flags] <journal> [<journal> ...]
```

Reports:

| Report | Answers |
| --- | --- |
| `orders` | orders planned, released and refused, trades skipped (sized but not ordered), and order lifecycle changes |
| `fills` | each fill with quantity, price and commission |
| `refusals` | refused orders, withheld sales back to 98% and engine-state changes (degraded, halted) |
| `totals` | fills, shares bought and sold, and commission per instrument per year, plus a TOTAL row |
| `positions` | the holding after each fill, rebuilt from the fills; a recorded symbol change moves the holding to the new id |
| `reconcile` | whether the fills agree with the account snapshots recorded in the journal; it fails, rather than report a clean result, when the log is empty or cut short, any journal compared nothing, or fills came after an account's last snapshot (`-allow-trailing-fills` accepts those). This is not the engine's morning reconciliation; the [glossary](glossary.md) ("Reconciled, reconciliation") tells the three apart |
| `operator` | your own instructions the engine recorded: kill switches and rebalance clearances, with who recorded them and the detail they gave, and fresh-run approvals, with the approver, the latch and the incident cleared or carried. Alert acknowledgements are not listed ([operator](operator.md), "Seeing what was recorded") |

Flags, all optional: `-account A1`, `-instrument AAA`, `-from 2026-01-01`,
`-to 2026-12-31` (UTC dates, both ends inclusive of the whole day) and
`-format text|csv|json` (default `text`); `reconcile` also takes `-allow-trailing-fills` and `-final-hash <hex>` (the run registry's final record hash, for exactly one journal; a mismatch or an empty value exits 1 and nothing is printed). You can name several journals. The
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

Is one of the program's sells still working?

`orders` cannot say on its own. It lists what the program planned
(`planned`), released (`released`, with `send=true` when it was to be
sent) and refused (`refused`), and each report IBKR sent back on an order
(`lifecycle-` and one of the four statuses of
[order lifecycle](order-lifecycle.md): `lifecycle-submitted`,
`lifecycle-cancel-pending`, `lifecycle-canceled` or `lifecycle-invalid`), and
a dry-run engine's own report on an order it never sent
(`synthetic-lifecycle-canceled`, below), and each trade the program sized
but did not order (`skipped`, below). It prints no `working` or `pending` value, and it never shows an order you
placed by hand. A released sell has ended only once it has a
`lifecycle-canceled` or `lifecycle-invalid` row, or `fills` shows it filled
in full; `lifecycle-cancel-pending` may still fill. And with the adapter
stopped, nothing IBKR does reaches the journal. So the check before any sale
by hand is IBKR's own list of open orders, in its app; use `orders` to see
which sells the program released and what IBKR last said of them
([states-and-kill-switch](states-and-kill-switch.md), "Selling by hand
safely").

```
$ journal-query orders -instrument SPY boost.log
journal   seq  time        account  instrument  event                     side  quantity  price  note
boost.log 41   2026-03-02  joint    SPY         planned                   sell  54        85.50  incident-1
boost.log 42   2026-03-02  joint    SPY         released                  sell  54        85.50  mode=live,send=true
boost.log 43   2026-03-02  joint    SPY         lifecycle-submitted       sell  54        85.50  tw-1f2e3d4c5b6a79880a1b2c3d
boost.log 47   2026-03-02  joint    SPY         lifecycle-canceled        sell  54        85.50  tw-1f2e3d4c5b6a79880a1b2c3d
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

Which trades did the program size but not order?

`orders` lists each as `skipped`: the side, a quantity of 0 (nothing was
ordered), the price the trade was sized at, and in the note the reason
(`below-one-share`, `scaled-below-one-share` or `held-by-working-sells`,
explained in [momentum minimum lot](momentum-minimum-lot.md)) and the
shares the rule asked for, to at most six decimals; a smaller size is shown in scientific notation.

```
$ journal-query orders -instrument BBB ira.log
journal  seq  time        account  instrument  event    side  quantity  price  note
ira.log  12   2026-03-02  A1       BBB         skipped  buy   0         80.00  below-one-share: 0.4 shares wanted
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
not only the failures. A later snapshot of an account holding nothing, after a baseline holding nothing, is still a comparison (one row, instrument `-`, 0 against 0). Fills recorded after an account's last snapshot are not yet checked against anything: `reconcile` prints the table and then exits with status 1 and `N fills not yet covered by a snapshot`, unless you give `-allow-trailing-fills`. After a comparison the snapshot becomes the new
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

The tool prints its error on one line and exits with status 1. Apart from
`reconcile`'s disagreements, it prints no table when it refuses.

| What you see | What the program does | What you do |
| --- | --- | --- |
| `usage: journal-query <orders\|fills\|refusals\|totals\|positions\|reconcile\|operator> [flags] journal [journal ...]` | Nothing named: no report given. | Name a report first. |
| `unknown report "..."` | Refuses before reading any file. | Use one of the seven reports above. |
| `flag provided but not defined: -...`, followed by the list of flags | Refuses before reading any file. | Use `-account`, `-instrument`, `-from`, `-to`, `-format` or (for `reconcile`) `-allow-trailing-fills` or `-final-hash`. |
| `name at least one journal to query` | Refuses. | Name a log or journal after the flags. |
| `format "..." is not text, csv or json` | Refuses before reading any file. | Use one of those. |
| `from "..." is not a YYYY-MM-DD date` (or `to`), or `the range runs backwards: from ... is after to ...` | Refuses before reading any file. | Write dates as `2026-01-31`, the earlier one first. |
| `open ...: no such file or directory` or `permission denied` | Stops with the operating system's error; prints nothing. | Check the path. On the server, run it as `sudo -u ti-trader`. |
| `...: refused, the hash chain does not verify: ... broken at record N` | The journal was edited, damaged or had a line removed in the middle. The file is refused whole, and with several files the whole query is refused, so you never read a partial answer. Dropping lines from the very end is not detectable by the chain alone: when you give the anchor from the run registry with `-final-hash`, the check covers end truncation; without it, only the record chain is verified (ADR 0017). | Keep the file as evidence; never edit it. Use the run's other file (log or journal) and compare. |
| `...: refused, the journal does not verify: ...` | The journal's header or a line cannot be read (not a chain break). | As above. |
| `...: refused, the durable log does not verify: ...` | A durable log (the engine's crash-safe log, ADR 0027) is damaged, not merely torn, and is refused whole. Only committed groups are ever reported: the uncommitted tail a crash can leave is left out silently. | Keep it as evidence; see `docs/runbooks/durable-log.md`, "An unrecoverable log". |
| `... and ... overlap: over the sequences both hold they record the same history ...` | Refuses: a resumed run's log begins with every record of the log it resumed, and a second copy of a journal holds all its records again, so naming both would count that history twice. | Name one file per run ("Running it"). |
| `...: record N: ... at schema version V, which this build does not read (it reads [...])` | Refuses a record from a newer or older build rather than guess at a figure. | Run the `journal-query` built from the same commit as the engine that wrote the file (on the server they are built together). |
| `...: record N: ...` naming a value (a fill with a negative quantity or an unknown kind, say) | The chain only proves the file was not altered, so each record's values are checked too; the file is refused before any figure is printed. | Keep the file; open an issue with the record named. |
| `...: record N: fill "..." conflicts with record M, which used the same fill id differently` | Refuses. The same fill id with the same execution (an absent and an empty unit list are the same, and one instant is the same in any UTC offset) is one execution, counted once; with different content it is a conflict. | Open an issue with both records. |
| `...: record N: the fill cannot be attributed to an account: the journal names K accounts and a fill names none` | `positions` and `reconcile` refuse rather than guess which account a Turtle fill belongs to. | Nothing for Boost or the IRAs: their fills name their account. |
| `...: record N: the fill takes ... past the largest position that can be reported (int64 overflow)` (or a rename that overflows) | Refuses rather than print a wrong figure. | Open an issue: no real position is that large. |
| `reconcile refused: ... holds no committed record`, `... ends after record N (byte B), followed by K bytes not committed (yet): if the engine is still writing, read it again once it has finished; otherwise the log was cut short`, or `... records input (...) after the run was stopped ...` | `reconcile` prints no table. A log with nothing committed, a log whose last group was never committed (a crash, or read while the engine was mid-write) or a journal or log with input after the adapter recorded that the run stopped (a delisting, say) cannot stand as the whole run, so comparing it would pass for a check it was not. The other reports still read what is committed. | A live engine mid-append shows the same state: read it again once the engine has finished writing, or use the run's other file. If it stays cut short, keep it as evidence and treat the run as unreconciled; compare with IBKR (`docs/runbooks/reconciliation.md`). |
| `reconcile compared nothing in <journal>: no account snapshot after the baseline met the selection ...` | Exits with status 1 and prints no table: no fill in that journal was checked against any broker figure, which is not a pass. With several journals, one that compares nothing fails the whole run. | Widen `-account`, `-instrument`, `-from` and `-to`, or name only journals that hold a snapshot after the account's first. Before the first snapshot after the baseline (a first paper order, say) this is the expected result: it is no evidence yet. |
| `...: the final record hash is ... but ... was recorded, so the file is not the run's complete history` | With `-final-hash`: the journal does not end on the hash the run registry recorded, so records are missing from its end or it is another file. Exits 1, prints no table. | Find the run's other file; keep this one as evidence. |
| `N fills not yet covered by a snapshot` | Prints the table of what was compared, then exits with status 1: those fills came after the account's last snapshot, so nothing has checked them. | Run it again after the next snapshot, or give `-allow-trailing-fills` when you accept that those fills are unchecked. |
| The `reconcile` table with rows marked `DISAGREES`, then `N disagreement(s) between the fills and the account snapshots` | Prints the whole table, then exits with the error. It does not decide which side is right. | Investigate before trusting either the fills or the snapshot: compare with IBKR (`docs/runbooks/reconciliation.md`). |

## What it reads, and what it does not

It reads fills of versions 4 (Turtle) and 5 (target-weight), order
lifecycle changes of versions 1 (Turtle stop orders), 2 (target-weight
limit orders) and 3 (a dry-run's synthetic report; a synthetic report at
version 2, or a venue's at version 3, is refused), planned orders of
versions 1 and 2 (2 is a sale back to 98%), risk-gate releases and refusals
of versions 1, 2 and 3 (3 adds the broker figures, more reasons and the
refusal's alert), withheld sales back to 98% (`risk.sale.withheld`, listed
by `refusals` with their reason, `lapsed` included), skipped trades
(`strategy.order.skipped`, listed by `orders`), engine-state changes,
kill switches, rebalance clearances, approvals, symbol changes, and account
snapshots of versions 3 (positions) and 4 (positions and cash; the cash is
not reported). The equity-and-cash snapshot (version 2), which holds no
positions, is passed over by design.

It does not report the broker snapshots, the marks, the bars, withheld
decisions, the morning reconciliation's results (`risk.reconciliation.result`;
the engine-state change one causes does show in `refusals`) or alert
acknowledgements. For those, read the alert, the dashboard, or the file
itself.

## What backs it

- ADR 0017 (journal tamper-evidence) defines the journal and its hash chain,
  which the tool verifies with the same code the engine uses (`journal.Verify`).
- ADR 0027 (durable log) defines the live log form, read as described above.
- Tests: `cmd/journal-query/main_test.go` drives the command line against
  small hand-built journals whose totals are added up by hand, including
  tamper refusal, byte-identical files after every report and format,
  date and account filters, CSV and JSON, and a deliberate position
  disagreement. `internal/journalquery/journalquery_test.go` covers the query
  logic itself, among them `TestSkippedTradesAreListedWithTheOrders`.
- The tool opens journals read-only and its query code receives bytes, never
  a file handle, so it cannot write.
