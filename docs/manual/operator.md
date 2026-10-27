# The operator tool: your kill switch, your approval, your clearance, your acknowledgement

*For the owner. How you tell a running engine to stop ordering, approve a
fresh run, clear a rebalance that is stuck, and acknowledge an alert so it
stops repeating, what each of these records, why it works this way, and
what can go wrong.*

## What it is

`operator` (source: `cmd/operator`) is a small command you run on the
server, **as the operator's user, `ti-operator`**: `sudo -u ti-operator
operator ...` (except `approve`, below). It never talks to IBKR and never changes a running engine's state
by itself. It writes your instruction down, checked, in a place the engine
reads, and the engine records it in its durable log as one of its inputs
before acting on it. That is the rule for everything a person tells the
system (ADR 0026, point 3): an instruction is a recorded input, never a
setting the program glances at, so the log always shows who said what,
when, and what the engine did next, and a replay of the log does the same
thing again.

**Run it as `ti-operator`.** The inbox belongs to `ti-operator`, and the
tool refuses to write when it is run as anyone but the inbox's owner, saying
`run it as the inbox's owner, sudo -u ti-operator operator ...`. It writes
each file mode `0640` in the inbox's group, the engine's, so the engine can
read it and only `ti-operator` can change it ("Who can write an
instruction", below). `operator approve` is the exception: it reads the
previous run's log and writes an approval file, not an instruction in the
inbox, so run it as the engine's user, `sudo -u ti-trader operator approve
...`, into a folder `ti-trader` can read. Every example below leaves out the
`sudo -u ...` in front to keep it short.

It has four commands:

| Command | What it records | Where it goes | When the engine acts |
| --- | --- | --- | --- |
| `operator kill-switch` | the kill switch engaged or released | the engine's operator inbox | before the engine's next input from the adapter |
| `operator rebalance-cleared` | your clearance of a rebalance that is stuck, or of a Boost order the adapter never sent | the engine's operator inbox | before the engine's next input from the adapter |
| `operator acknowledge` | your acknowledgement of one alert, by its alert key | the engine's operator inbox | before the engine's next input from the adapter; the reporter stops repeating the alert at its next run |
| `operator approve` | your approval of a fresh run, or of an account's first run with its starting book (`-first-run`) | an approval file you pass to the engine with `-approval` | when that run starts, as its second input |

**`-account` is the account's name in the engine's configuration**, its
`account_id`: `joint` for Boost (`/etc/trend-investing/engines/boost.config`).
It is not the IBKR account number, which only the adapter holds. The alerts
use the same name (`joint: engine halted (...)`), and every alert key ends
with `@joint`. An instruction for any other name is refused by the engine
with no effect (see "What can go wrong" below).

## Who can write an instruction

An instruction counts only because it is yours, so the user that writes one
is an identity of its own, which the engine and every unit that reads data
from outside do not run as (ADR 0026, kill-switch amendment):

| Who | Runs as | What it can do with the inbox |
| --- | --- | --- |
| You, with `operator` | `ti-operator`, a system user with no login shell that the provisioner creates, in the engine's group `ti-trader` | owns the inbox, and writes the instructions |
| The engine | `ti-trader` | reads it through its group; cannot write it (and its unit mounts it read-only) |
| The adapter, the data job, the reporter and the dashboard | `ti-trader`, each sandboxed | cannot reach it (`InaccessiblePaths=`), and can write nothing under the engines' folders |
| Any other user | | nothing: the folder is mode `0750` |

So a defect in a unit that reads IB Gateway's data, Massive's answers or a
web request cannot write a `released` or an acknowledgement that the durable
log would show as yours. The engine checks this each time it starts. It
refuses an inbox its group or others can write to, one owned by root or by
the user the engine itself runs as, any inbox when the engine runs as root,
one others can read (the records are readable only through the group), and
one it cannot list and read (owned by a group it is not in, whatever the
mode, or a listing or a record it is denied), so a kill switch is never left unread while it
decides. On a host other than the server (the Mac mini, a container), the
inbox's owner is whichever user that host's engine does not run as: give the
folder to that user, in a group the engine's user is in, mode `0750`.

The engine writes nothing into the inbox. What it notes about each file
(`.delivered` and `.refused`, below) goes in its own folder beside the inbox,
the inbox's name followed by `-notes`
(`/var/lib/trend-investing/engines/<instance>/operator-notes` on the server),
mode `0700`. It creates that folder at its first start, and refuses to start
if the one there is another user's, others can write it, or it cannot list
it: a note settles a file for good, so a forged one could silence your kill
switch. For the same reason a `.delivered` or `.refused` file in the inbox
itself means nothing to the engine: whoever can write the inbox could
otherwise withhold an instruction as well as write one. Only the notes folder
settles a file. While it runs, an inbox the engine cannot list, or a file in
it the engine cannot open (one you put there by hand with another mode, for
instance), stops the run with the error: it may be a kill switch, and the
engine does not decide with it unread.

**Upgrading from a build that wrote its notes into the inbox.** An earlier
build wrote `.delivered` and `.refused` beside each file in the inbox; this
build does not read them, and it refuses to start while the inbox holds one,
naming the file, because a record that was refused (for a time ahead, say)
would otherwise be read again and delivered once that time passed. So before
the first start of this build, move every file that has a `.delivered` or
`.refused` beside it out of the inbox, with its markers
(`sudo mkdir /var/lib/trend-investing/engines/<instance>/operator-old` and
`sudo mv` them there); nothing the engine needs is lost, since the log holds
what was recorded. A file already recorded in a log still in the engine's
log folder is not recorded again (the log is the evidence), and the engine
notes it in its own folder.

## The operator inbox

Each engine is started with `-operator <folder>`, its inbox, a folder that
must exist; a backtest takes none (the engine refuses to start with both:
`-operator applies to a run that can reach a broker`). On the server you do nothing to set it up:
the provisioner creates `/var/lib/trend-investing/engines/<instance>/operator`
for each account, owned by `ti-operator` in the engine's group `ti-trader`,
mode `0750`, and the engine's service file passes it as `-operator`. Only by
hand (a host the provisioner did not set up, or a folder removed by
mistake), create it with `sudo install -d -o ti-operator -g ti-trader -m 0750
/var/lib/trend-investing/engines/<instance>/operator` and pass
`-operator` with it. The tool
writes one small file per instruction there, named by the time and the
kind of instruction, for example `20261017T143005Z-operator.kill-switch.json`.
Each time the adapter sends the engine something (a price, an account
figure, a session close), the engine first reads the inbox. Every file it
has not yet recorded, in the order of the times the files state, becomes an input at the next place in
the run's numbered stream of inputs. The engine writes it to the durable log,
flushed to disk, and then handles the adapter's input. The engine only ever
checks an order while handling an input, so an instruction recorded before
the next input applies to every order checked from then on.

The time a file states is the one you gave with `-at`, or the time you ran
the tool. A file written later with an earlier `-at` sorts, and is
recorded, as of that earlier time.

The file stays in the inbox afterwards; the durable log, not the inbox, is
the evidence. Once the engine has flushed an instruction to the log, it
writes a marker in its notes folder, under the file's name, ending
`.delivered`, saying where it was recorded. An instruction with that marker, or already recorded in any log
in the engine's log folder (including the one a crash resume continues), is
never recorded again, so moving a damaged log into `quarantine`, or
starting a run with its log in another folder, does not replay old
instructions. Leave the markers in place.

Your instructions count in the order of the times they state (ADR 0019,
amendment of 2026-10-17, "operator instructions count in the order of the
times they state", a rule the owner accepted on 2026-10-18). A kill-switch
file stating a time earlier than the newest kill switch the engine has
applied for its account is recorded in the log and refused, with no effect
(and a `.refused` note naming the later one), so an old or backdated
`released` never undoes a later `engaged`, and the log still shows it was
tried. To change the switch, record it again with the current time.

Any instruction (kill switch, clearance or acknowledgement) stating a time more than five minutes after the engine's own clock (a slip in `-at`) is
never recorded: it would put every instruction stating the true time before
it. The engine refuses it with a `.refused` note; record it again with the
current time. Up to five minutes ahead is allowed, since the tool and the
engine may run on machines whose clocks differ by that much.

A file the engine cannot read (hand-edited, damaged, or holding more than
one record) is never used.
One the engine refuses (for another account, backdated before a later kill
switch, or a clearance that does not match) is recorded with no effect. In
every case the engine writes a note in its notes folder, under the file's
own name, ending `.refused`, saying why, prints the same on its output
(`journalctl -u ti-engine@<instance>`), and never tries that file again.

If the engine cannot write an instruction to the durable log (a disk fault
while it is being flushed, or a log that has reached the most records a run
holds), it was not recorded: the engine writes no note, stops the run with
the error on its output, and leaves the file as it was. Resume the run, or
after a full log start a fresh one (see [restart](restart.md)), and the
engine records it before the adapter's first input, so a kill switch is
never lost to a disk fault or a full log.

The adapter follows the engine's numbering: it reads the next number from
the engine's durable log before it sends ([IBKR adapter](ibkr-adapter.md)).
So the inbox is read only when the adapter sends something: a price, a
broker snapshot, an order report, the day's bars and close. With the adapter
stopped, or nothing arriving, your instruction waits in the inbox. That is
safe for the kill switch, because every order is checked while handling
such an input, and the inbox is read first. The decisions
the engine sends back carry the number of the last input it recorded, and
the adapter's next input carries the number after it. When the engine
records your instructions first and then refuses the adapter's input, the
refusal carries that number too, in its `sequence` field, and the adapter
continues from it. When one of your instructions could not be recorded,
or the engine could not record the adapter's input after yours (a log that
has reached the most records a run holds, or one input deciding more than a
run takes from one input), the refusal's `sequence` is the number of the
last instruction that was, says the adapter's input was not recorded, and
the run stops: a log missing a number could never be resumed, so the run
stops where its log ends, and resumed, the adapter's next input carries the
number after it. The same holds with no instruction recorded first: when
the engine cannot record the adapter's input for either of those reasons,
its number is used up, so the refusal says so and the run stops; resumed, the
adapter's next input carries the input's own number again. (Before this
release a run carried on after that refusal and left a gap in its log that
could not be resumed.) Either way the engine writes the refusal to the
adapter first and only then stops, so a connected adapter always receives
it; if the adapter has gone away, or the refusal cannot be delivered, the
run still stops, within a couple of seconds, with the same error. An input
carrying a number already used or out of date is different. An input sent
again under its own ID, at the sequence it was sent with, gets its committed
reply again. A different input under a used number, or the same input at
another sequence, is simply refused; the refusal uses up no number, and the
run carries on.

When the engine records your instructions before the adapter's input, the
input is recorded under a later number than the one it was sent with. If
the adapter sends that input again to ask for its reply, it is answered
under either number, and refused under any other. After a restart the log
no longer says which number it was sent with, so it is answered under any
number from the first of the instructions recorded just before it. After a crash resume the engine prints `the adapter's
next input carries sequence N`; a resume that also records an approval
prints the number once, on the approval's line.

## The kill switch

```
operator kill-switch -inbox /var/lib/trend-investing/engines/boost/operator \
  -account joint -state engaged -by owner -detail "why"
```

`-state` must be `engaged` or `released`; anything else is refused, so the
switch is never turned off by a typing slip. `-at` sets the time (RFC 3339);
without it the tool uses the current time, to the second. It prints:

```
recorded operator.kill-switch:<account>:2026-10-17T14:30:05Z (engaged) in <inbox>/20261017T143005Z-operator.kill-switch.json; the engine delivers it before the adapter's next input and journals it in its durable log
```

What the switch does is in [states-and-kill-switch](states-and-kill-switch.md):
every order is refused except the margin account's one sale back to 98%,
and the engine state does not change.

**"recorded" here means written to the inbox, not yet taken by the engine.**
To confirm the engine took it, look for any of these:

- a marker in the engine's notes folder, under the file's name, ending
  `.delivered`, holding `delivered: it is journalled at sequence <N>`:
  `sudo -u ti-trader cat /var/lib/trend-investing/engines/boost/operator-notes/<file>.delivered`.
  If a `.refused` note sits beside it there too, the engine recorded the
  instruction but refused it: it had no effect, and the note says why;
- the engine's own line, `engine: recorded operator input <id> at sequence
  <N>`, in `journalctl -u ti-engine@boost`;
- the switch in the log itself: `sudo -u ti-trader journal-query operator
  /var/lib/trend-investing/engines/boost/<run>.log` lists it with its
  sequence number. This is the evidence; the marker only points to it.

No marker yet means the engine has not read the inbox since: it is stopped,
or the adapter has sent it nothing (see "The operator inbox" above). The
first refusal the switch makes then reaches you as an urgent alert,
`joint: order refused (kill-switch)`.

**After you release the switch, Boost tries again at its next decision.**
Normally Boost orders only when its target changes (ADR 0026, point 2,
"Boost"). But an order the switch refused is planned again at the next
decision, after the market's next close: Boost works out the difference
between what the account holds and the target in force, and orders that.
If you engaged the switch while Boost wanted to buy, the buy goes at the
first close after you release it that decides, while the account is
Normal; if Boost wanted to sell, the sale back
to 98% has gone anyway, and the next decision sells whatever is still above
the target. While the switch stays on, each close plans the order again and
the switch refuses it again, so you see one `kill-switch` refusal per close
in the journal, but the reporter alerts you only for the first one of each
engagement (a log written before the engine marked that first refusal
alerts for every one). A close that withholds its decision (no bar, or no account
snapshot as of it) plans nothing, so the order waits for the next close
that decides.
This applies to any refusal that leaves the engine Normal (the kill switch,
or an IBKR snapshot that is too old); a refusal while the account is
Degraded or Halted follows that state's own rules
([states-and-kill-switch](states-and-kill-switch.md)). The owner decided
this on 2026-10-18. The momentum IRAs plan afresh at every decision time, so
their next rebalance orders as usual.

## A rebalance clearance

An IRA's monthly rebalance sends its sells, then its buys once the sells
are done. While any order of a rebalance is still working, the next
month's decision is held back (`rebalance-in-progress`) and nothing more is
sent (ADR 0026, point 5). Normally IBKR reports every order finished. If an
order is stuck, you deal with it at IBKR (cancel it, or confirm it is gone)
and then clear the rebalance:

```
operator rebalance-cleared -inbox <inbox> -account <IRA account> \
  -decision targets-decided:<account>:2026-09-30T20:00:00Z \
  -orders <client order id>,<client order id> -by owner -detail "cancelled both at IBKR"
```

The clearance must name the rebalance's decision and **every** order of it
that is still open, no more and no fewer. The engine refuses anything else
(a different decision, a missing or extra order, nothing in progress), so a
clearance never means more than you said. Once it is recorded, those orders
count as finished, the rest of that rebalance (its buys, if still waiting)
is never sent, and the next decision time decides afresh. `journal-query
refusals` and `journal-query orders` show which orders are open; the
[missed-rebalance runbook](../runbooks/missed-rebalance.md) has the steps.

For an IRA, clear only after IBKR shows the order finished. A later IBKR
report on a cleared IRA order is refused, which stops the engine until you
reconcile. The reporter reads the clearance the same way, so that report
also raises the urgent "venue report not read" alert. The momentum engine
is not composed into the running program yet (only Boost is), so today an
IRA's clearance is read by the momentum strategy and its tests only.

For Boost (the joint account), a clearance means something else: it
settles one order the adapter **never sent**, after `relay.py resolve
--never-sent`, so IBKR should have nothing to show for it. Name the decision that released it and that order. Boost then
stops listing it as an order IBKR has not ruled on (`undecided_orders`),
records it under `cleared_orders`, and nothing else changes; reconcile and
start a fresh run ([boost](boost.md)). Boost refuses a clearance of an
order IBKR has already named, even only as "submitted" (that order was
sent and its verdict is still to come), one that IBKR lists among its
open orders, a clearance that names any other order, and every clearance
in dry-run, where no order is listed. If IBKR reports on a cleared Boost
order after all, the clearance was wrong: Boost refuses the report, which
stops the engine until you reconcile with IBKR and start a fresh run, as
for an IRA.

You no longer need a clearance for an order whose day ended before the
adapter could send it, or for one IBKR can never accept: those are
**retired** (next section), and a retirement ends the order in the engine
and lets it plan again, which a Boost clearance does not.

## Retiring an order the adapter can never send

Sometimes the adapter refuses an order before writing "about to send", so
the order never leaves the adapter (the owner's decision of 2026-10-22;
[order lifecycle](order-lifecycle.md), "An order IBKR can never accept"):

- **Its session is over** (it was for a day that has ended). The adapter
  retires it **by itself**. You do nothing; you get a routine message.
- **IBKR can never accept it as it stands**: it is not a limit order, it is
  for a fraction of a share, or the stock has no IBKR contract number in
  the adapter's configuration. The adapter **stops at that order** and you
  are paged ("order ... not acknowledged"). Once you have found the cause,
  you retire it yourself with the adapter's own command:

```
sudo -u ti-trader python3 adapter/ibkr/relay.py retire \
  /var/lib/trend-investing/adapters/boost/pending.json <client order id> \
  --by <your name> --reason "no contract number for SPY; added to con_ids"
```

**Who may run it.** Only you, the owner, with the adapter **stopped**. It is
a command of the adapter (`relay.py`), not of `operator`, because it settles
the adapter's own record of the order as well as telling the engine. Run it
as the user that owns the adapter's files, `ti-trader` on the server (`sudo
-u ti-trader ...`, which only an administrator can do); it refuses to run as
root or as any other user, and refuses while an adapter holds the pending
file. `--by` must name you; the adapter's own name (`adapter`) is refused.

**What it accepts.** Only the order the adapter stopped at for a shape IBKR
can never accept, for the input it stopped on, and not yet handed over. It
refuses a typo (it names the order it is refusing), an order the record
marks as sent or about to be sent, an order the adapter stopped at for any
other reason (a login that does not manage the account, sending switched
off, no order number from IBKR yet: fix those and start the adapter again),
and nothing stopped at all.

**What it records.** Three things, in this order, beside the adapter's
pending file: an `execution.order.retired` input in the adapter's outbox
for the engine (reason `owner retired`, your name as who retired it, your
reason as its detail, and the time); a line `"decision": "retired"` in
`PENDING_FILE.handover.decisions` with your name, reason and the session
date; and the order marked never sent. It is a refusal of an order that
never left the adapter, never a cancel of an order at IBKR. When you start
the adapter it checks IBKR once more (if IBKR shows the order after all, it
stops: [re-delivery](redelivery.md)), goes on past the input it stopped on,
and sends the retirement to the engine, which journals it and re-plans at
its next decision. You then get one routine message, "order ... retired
(owner retired)".

## Acknowledging an alert

A Degraded, Halted or reconciliation alert is sent again every
`-repeat-interval` (30 minutes on the server), around the clock, overnight
and at weekends included, until you acknowledge it
([alerts](alerts-and-dashboard.md), "Repeated until you acknowledge it" and
"When the reporter runs"). Its text ends with `acknowledge with
alert key: <key>`. Copy that key, exactly, into:

```
operator acknowledge -inbox <inbox> -account joint \
  -alert 'engine-state:halted:risk-limit-breached:||:2026-10-17T14:00:00Z@<account>' \
  -by owner -detail "seen; reconciling at IBKR"
```

Put the key in single quotes: it can hold `|` and other characters your
shell would otherwise read. `-detail` is optional here; `-at` works as for
the other commands. It prints:

```
recorded operator.alert.acknowledged:<account>:2026-10-17T14:30:05Z (alert engine-state:halted:…@<account>) in <inbox>/20261017T143005Z-operator.alert.acknowledged.json; the engine delivers it before the adapter's next input and journals it in its durable log
```

What you see next: the engine writes the acknowledgement to its durable log
when it takes its next input from the adapter (it decides nothing from it).
With the engine stopped, or with no input arriving, nothing is journalled and
the alert keeps repeating until the engine runs again. From the reporter's
first run after it is journalled, that alert is sent no more, on any channel: no more
ntfy messages and no more Pushover pages. Pages already sent expire on
your phone by themselves. The alert is not marked
anywhere else, and a new alert (the account going Halted again later, say)
has a new key and repeats until you acknowledge that one.

Acknowledging changes nothing else. The account stays Degraded or Halted, and
the kill switch where it was, until you recover it
([reconciliation runbook](../runbooks/reconciliation.md)). An
acknowledgement is not refused for stating an earlier time than another
instruction: it changes no state, so the order it counts in does not matter.
Only one stating a time more than five minutes ahead of the engine's clock is
refused, as for every instruction. A key that names no alert is recorded and
does nothing; a misspelt key leaves the alert repeating, so check the next
run stops it. The tool does not check the key against the alerts: it cannot
see them, and the log is the only evidence of what you acknowledged.

Two acknowledgements in the same second share a file name, so the second is
refused; run it again a second later.

The engine refuses an acknowledgement stating a time more than five minutes
ahead or a file it cannot read: it is never recorded, and you see a `.refused`
note in the engine's notes folder and on the engine's output. One for
another account is recorded and refused, with a `.refused` note there
(`acknowledgement ... is for account ...; this run decides "joint"`): the
reporter does not count it and the alert keeps repeating. Record the acknowledgement again with the right account and
the current time.

## An approval

A fresh run (anything but a crash resume: see [restart](restart.md)) starts
only with your approval. `operator approve` writes it from the previous
log itself, so you do not copy hashes and states by hand:

```
operator approve -log /var/lib/trend-investing/engines/boost/<latest run>.log \
  -account joint -by owner -out /var/lib/trend-investing/engines/boost/approval-<new run>.json
```

It reads the log and states what it ends in: its file name and the
fingerprint of its last record, Boost's latch, and the incident it ended
in (Degraded or Halted; which instruments, and `total-cash` when a
[reconciliation](reconciliation.md) found cash it could not explain; and
whether that incident's one sale back to 98% was already attempted). That incident is carried into the
new run unless you clear it with `-clear-incident <incident id>`, which is
the step ADR 0019's recovery procedure ends with, after you have reconciled
the account. It prints what the approval says and the next step:

```
run-2026-10-20.log ends with Boost latched false; the approval carries incident engine-state:joint:… in force, halted, its one sale back to 98% attempted true
wrote <file>: start the fresh run with -approval <file> (docs/runbooks/durable-log.md, "Restarting an engine")
```

If the kill switch was engaged when the previous run ended, it also says so:
a fresh run starts with the switch released, so record it again with
`operator kill-switch -state engaged` before you start the engine. The
engine reads that record before the fresh run's first input from the
adapter, before any order is checked.

`operator approve` takes the incident's sale attempt from the log. After a
hand sale the log does not know about, approve with the book as it is after
your sale (`-positions ... -cash ... -currency USD`, typed right after
reading the book in IBKR, below), or the fresh run halts on its first reading
as unverifiable, and, when the sale was the incident's own, add
`-sale-attempted`: the approval then states the incident's one sale back to
98% as attempted, so the fresh run never makes it, and the tool prints "stated
by -sale-attempted". The flag requires the book: without `-positions`,
`-cash` and `-currency` it is refused. Do not edit the file the tool wrote.
The flag is refused, and nothing is written, unless the log ended in an
incident whose sale it does not record as attempted: not when the log ended
normal, not with `-clear-incident` (that leaves no incident in force), not
with `-first-run`, and not when the log already records the attempt (the
approval then carries it attempted, so leave the flag out).

**A sale the ended run released and the adapter never sent.** If the run
ended while the adapter still held such an order, settle it first with
`relay.py settle-ended-run` ([re-delivery](redelivery.md), "An order an
ended run left unsent"), then add `-never-sent PENDING_FILE.never-sent`.
Settle with the engine stopped: the command refuses while the engine
holds its instance lock (`engine.lock` in its state folder, held for as
long as it runs, a resume included). A sale whose only evidence is a
decision an earlier build recorded with no input or no run is never
settled: it stays attempted, so sell it by hand if it is still needed.
The tool checks each settlement in the file against the log by the
engine's own retirement rule, and a sale back to 98% it settles is stated
not attempted, with one more of the incident's sales retired and the
settlement carried as its evidence; it prints "settled by the adapter as
never sent" for each. Beside `-sale-attempted` (you sold by hand) the
order is still retired, but the sale stays attempted as you state, and
the tool prints that the fresh run makes no sale. Settlements of another log in the file are left
out. A settlement the log does not bear out (the order was reported on or
executed, so it was sent; another input released it; it does not describe
the order as released) refuses the approval, naming the order, and
nothing is written.

**An account's first run** is approved too, with the book it starts from,
since there is no previous log to read. Check the paper account's holdings
and total cash (`TotalCashValue`) in IBKR, then:

```
operator approve -first-run -account <IBKR account> -by owner \
  -positions SPY=<shares> -cash <total cash> -currency USD \
  -out /var/lib/trend-investing/engines/boost/first-run-approval.json
```

(`-positions ""` when the account holds nothing). It prints the book and the
next step. The cash is IBKR's `TotalCashValue` in USD, the currency the
adapter reads IBKR's cash in: any other `-currency` is refused. A first
run has no incident to clear, so `-clear-incident` with `-first-run` is
refused too. Approve right after you read the book: the approval's time is
the book's cutoff (a trade before it is taken as in the book, one after it
as a change since), so an `-at` later than now is refused whenever a
starting book is stated. A fresh run's approval takes the same three options when you
moved shares or cash in or out since the previous run, or to clear a halt
on a run's first reading (see [restart](restart.md)): the fresh run's first
reading is then compared with the book you state, not with the previous
run's last close (ADR 0019 and ADR 0027, amended 2026-10-18), and the tool
prints that book back before the next step. A first run in paper or live
mode does not start without its approval.

The engine checks the approval against the same log when the fresh run
starts (see [restart](restart.md), "What the engine checks"). One more check
is new: an approval that carries the incident in force and says its sale
was **not** attempted is refused when the log records an attempt, because the
new run would otherwise try the sale a second time (ADR 0019, amended
2026-10-15). Saying it **was** attempted is always accepted, since you may
have sold by hand.

The tool refuses a log it cannot read, one holding nothing past its
configuration (resume that one instead), an incident to clear that is not
the one the log ended in, and an approval file that already exists. It never
overwrites anything.

## Seeing what was recorded

`journal-query operator <log or journal> [...]` lists the kill switches, approvals and clearances
(not acknowledgements, below): the time to the second, the account, which input, what it did, who recorded
it and why:

```
journal  seq  time                  account  input              action                                     by     detail
run.log  57   2026-10-17T14:30:05Z  joint    kill-switch        engaged                                    owner  news before the open
run.log  2    2026-10-21T13:00:00Z  joint    run-approved       after run-1.log, latched false; no incident  owner  -
```

An approval carrying an incident in force shows its whole scope, the
instruments then the cash components, as in `incident engine-state:joint:3 in
force, degraded on SPY;total-cash, sale attempted false`, so an account
Degraded on its cash alone reads `degraded on total-cash`. An approval an
earlier build recorded (schema version 1, which could not state a cash scope)
is read as it was recorded. Acknowledgements are not listed in this report
yet: the reporter reads them from the log, and the `.delivered` marker in the
engine's notes folder says at which sequence the engine recorded one.

`-account`, `-from`, `-to` and `-format csv|json` work as for the other
reports ([journal-query](journal-query.md)).

## What can go wrong

Every message below is what the tool prints, or what the engine writes in a
`.refused` note in its notes folder and on its output (`journalctl -u ti-engine@<instance>`).
When the tool refuses, it writes nothing; run it again once fixed.

### When you run the tool

| What you see | What the program does | What you do |
| --- | --- | --- |
| `usage: operator <kill-switch\|rebalance-cleared\|acknowledge\|approve> [flags]` or `operator: unknown command "..."` | Nothing written | Use one of the four commands |
| `flag provided but not defined: -...`, or `operator <command>: unexpected arguments [...]` | Nothing written: a misspelt flag, or a value with spaces not in quotes | Check the spelling; quote `-detail "..."` |
| `operator: -inbox is required: the engine's operator inbox, its -operator folder` | Nothing written | Add `-inbox /var/lib/trend-investing/engines/<instance>/operator` |
| `operator: the inbox: stat ...: no such file or directory` | Nothing written: the inbox folder does not exist (the provisioner creates it on the server, so a path typed wrongly, or a folder removed by mistake) | Check the path against `/var/lib/trend-investing/engines/<instance>/operator`; if it is gone, create it as "The operator inbox" says |
| `operator: the inbox <inbox> belongs to user <n>, the user that writes the owner's instructions, but this tool runs as user <m>: run it as the inbox's owner, ``sudo -u ti-operator operator ...`` on the server` | Nothing written: you ran it as yourself, as root or as `ti-trader`, not as the inbox's owner | Run it again with `sudo -u ti-operator operator ...`. If the folder belongs to the wrong user, fix it as "Who can write an instruction" says (`sudo chown ti-operator:ti-trader <inbox>`). `operator approve` writes no instruction and is run as `ti-trader` |
| `operator: write into <inbox>: ... permission denied` (or `make <file> readable by the engine's group`, `write`, `flush`, `install`) | Nothing written, or a file the engine cannot read (it then stops the run naming it) | Check you ran it as `ti-operator`, the folder's owner and mode (`ti-operator:ti-trader`, `0750`), and that the disk is not full |
| `operator: <file> already holds a record, which is never overwritten (ADR 0018); record it again a second later` | Nothing written: two instructions of one kind in the same second (or the same `-at`) share a file name | Run it again a second later, or with a later `-at` |
| `operator: flush the inbox <inbox>, so the record's name survives a power loss: ...` | The file was written, but the folder could not be flushed to disk | Check the disk. The record is in the inbox; look for its `.delivered` marker in the engine's notes folder before recording it again |
| `operator: -state must be engaged or released, not "...": the switch is stated, never assumed` | Nothing written | Run it again with `-state engaged` or `-state released` |
| `operator: -at must be an RFC 3339 time: ...` | Nothing written | Give a time like `2026-10-17T14:30:05Z`, or leave `-at` out |
| `operator: invalid kill switch payload: account id is required` (or `operator is required`, `detail is required`) | Nothing written: `-account`, `-by` or `-detail` missing | Add the missing flag |
| `operator: invalid rebalance cleared payload: decision id is required ...`, `client order ids are required ...`, `client order id <n> is empty`, or `client order id must be strictly ascending; "x" follows "x"` | Nothing written: `-decision` or `-orders` missing, an empty name between commas, or an order named twice | Name the decision and each open order once (`journal-query orders`) |
| `operator: invalid alert acknowledged payload: account id is required` (or `alert key is required`, `acknowledged by is required`) | Nothing written | Add `-account`, `-alert` or `-by` |
| `operator: -alert must hold no newline: ...` | Nothing written (spaces around the key are trimmed, a line break inside it is refused) | Copy the key from the alert again, in single quotes, on one line |
| `operator: -log is required: the previous run's durable log (or -first-run for an account's first run)` or `operator: -out is required: where to write the approval` (`approve`) | Nothing written | Name the previous run's log with `-log`, or, for the account's first run (no previous log), add `-first-run` with `-positions`, `-cash` and `-currency`; and name where to write the approval with `-out` |
| `operator: -first-run approves an account's first run, which has no previous log: leave out -log`, `... which has no incident to clear: leave out -clear-incident`, or `... which has no incident whose sale to state: leave out -sale-attempted` (`approve`) | Nothing written | Run it again for the first run with only `-account`, `-by`, `-positions`, `-cash`, `-currency` and `-out` |
| `operator: -<positions, cash or currency> is required: a starting book states the holdings (-positions, empty when none), the total cash (-cash) and its currency (-currency) checked in IBKR` | Nothing written: a starting book is all three or none | Read the holdings and `TotalCashValue` in IBKR and give all three |
| `operator: -positions "...": "..." is not SYM=shares, a whole number of shares`, `... names <SYM> twice`, or `operator: -cash "..." is not an amount of cash` | Nothing written | Write holdings as `SPY=100` (several as `SPY=100,QQQ=5`), whole shares, each symbol once, and cash as a plain number |
| `operator: starting book position <SYM> states a zero quantity; an instrument not held is left out` (`approve`) | Nothing written: a holding of zero shares is stated by leaving the instrument out | Leave that instrument out of `-positions` (`-positions ""` when nothing is held) |
| `operator: -currency "...": the adapter reads IBKR's cash in USD, so state IBKR's USD TotalCashValue with -currency USD` | Nothing written: a book in another currency could never match the adapter's reading | State the USD figure with `-currency USD` |
| `operator: -at <time> is later than now: a starting book's approval time is its cutoff, so approve right after reading the book in IBKR, leaving out -at` | Nothing written | Leave `-at` out and approve right after you read the book |
| `operator: open the previous log: ...` or `operator: <log> cannot be read as a durable log (docs/runbooks/durable-log.md): ...` | Nothing written | Check the path; a damaged log follows `docs/runbooks/durable-log.md`, "A log that cannot be read" |
| `operator: <log> holds nothing past its configuration: its start was cut short, so resume it ...` | Nothing written | Resume that run with `-resume` ([restart](restart.md)) |
| `restart: <log> records no incident, so there is none to clear: it ended normal`, or `restart: <log> ended <state> in incident <id>, so that is the incident to clear, not <id>` | Nothing written: `-clear-incident` names an incident the log did not end in | Leave `-clear-incident` out, or copy the incident ID from the tool's own output without it |
| `operator: -sale-attempted states a sale made by hand, so state the book after it: -positions, -cash and -currency, read in IBKR once the sale has filled` (`approve`) | Nothing written: the flag states a hand sale, and the book after it is what the fresh run checks | Sell by hand first, read the book in IBKR once the sale has filled, and give all three with the flag; if you did not sell, leave the flag out |
| `restart: -sale-attempted states the sale of an incident left in force, and -clear-incident leaves none in force: use one or the other`, `restart: <log> records no incident, so there is no sale to state as attempted: it ended normal`, or `restart: <log> already records an attempt of incident <id>'s one sale back to 98%, so the approval carries it attempted without -sale-attempted` (`approve`) | Nothing written: no incident is left in force whose sale the log lacks | Leave `-sale-attempted` out (the tool then carries a recorded attempt itself), or drop `-clear-incident` |
| `restart: <log> ends with a cash difference pending a reading of IBKR's statement and not yet decided: reconciliation <id> (input <input>) found <amount> USD; ...` (`approve`) | Nothing written: a fresh run starts with no pending cash difference, so the one the log holds would be forgotten (ADR 0019, amended 2026-10-21). The engine refuses such an approval at its start too | Wait until the reconciliation settles it (usually the next morning after the Flex reader has covered the day), or check the account in IBKR after the transfer and approve again with `-positions`, `-cash` and `-currency`: the book you state replaces the pending difference ([reconciliation](reconciliation.md)) |
| `restart: the approval is refused` followed, on the next line, by `invalid operator run approved payload: account id is required` (or `approved by is required`, one line each when both are missing); for a first run, one line: `operator: invalid operator run approved payload: account id is required` (or `approved by is required`) (`approve`) | Nothing written: `-account` or `-by` missing | Add `-account` or `-by` |
| `restart: the adapter's settlement of order <id> as never sent is refused: <log> does not bear it out: ...`, `restart: the adapter's settlement of order <id> as never sent names input <input>, but in <log> input <other> released it ...`, or `restart: <file> line <n> is not a settlement as the adapter writes one: ...` (`approve -never-sent`) | Nothing written: a settlement retires a sale only when the log bears it out, since a sale that was sent must never be tried again | Do not edit the file. Check IBKR for the order named: if IBKR shows it, it was sent, so, with the adapter stopped, mark it sent (`relay.py resolve PENDING_FILE ID --sent --reason "what IBKR shows"`) and run `settle-ended-run` again, which withdraws its line; otherwise check that `-log` is the run `settle-ended-run` was given, and run that command again. A file with a torn last line is repaired by running `settle-ended-run` again ([re-delivery](redelivery.md)) |
| `engine: decode <id> in <log>: ...` (`approve`) | Nothing written: a record the approval depends on cannot be read | Treat the log as damaged: `docs/runbooks/durable-log.md` |
| `operator: <file> already exists; an approval is evidence and is never overwritten (ADR 0018)` | Nothing written | Choose a new `-out` name |
| The approval prints `the kill switch was engaged when it ended; a fresh run starts with it released ...` | The approval is written; the fresh run will start with the switch off | Record `operator kill-switch -state engaged` before starting the engine |

### When you retire an order

These are what `relay.py retire` prints after `refused:`; it records
nothing when it refuses.

| What you see | What the program does | What you do |
| --- | --- | --- |
| `refused: only the owner retires an order, as the user that owns the adapter's files ...` | Nothing recorded: you ran it as root or as a user that does not own the adapter's pending file | Run it again as `sudo -u ti-trader python3 adapter/ibkr/relay.py retire ...` |
| `refused: --by must name the owner retiring the order ...` or `refused: a reason is required: it is journalled with the retirement` | Nothing recorded | Give `--by <your name>` and a `--reason` that says what was wrong and what you changed |
| `refused: the adapter holds the pending file ...; stop it first` | Nothing recorded: an adapter is running (or hung) | Stop the adapter, then run it again |
| `refused: no input awaits its reply in ..., so no order is stopped` or `refused: the adapter has recorded no refused order for the input awaiting its reply ...` | Nothing recorded: the adapter is not stopped at an order | Nothing to retire. Check the adapter's last stop message |
| `refused: the adapter is refusing order <id> (...), not <id>` | Nothing recorded: a typo | Run it again with the client order ID it names |
| `refused: order <id> is marked ...: it was handed over or settled already` | Nothing recorded: the order may be at IBKR | Settle it as [re-delivery](redelivery.md) says, never by retiring it |
| `refused: the adapter refused order <id> for a reason that is not a shape IBKR can never accept (...)` | Nothing recorded: the stop has another cause, which will clear once fixed | Fix that cause (the reason is quoted) and start the adapter again; if the order must never go out, settle it with `resolve --never-sent` ([re-delivery](redelivery.md)) |

### After the tool has written it

| What you see | What the program does | What you do |
| --- | --- | --- |
| No `.delivered` marker for the file in the engine's notes folder | Usually the engine has not read the inbox since: it is stopped, was started without `-operator`, or the adapter has sent it nothing. A missing marker alone does not prove that: the engine can record the instruction and then fail to write the marker (the `mark ... delivered` row below) | Run `journal-query operator <log>` first: if the instruction is listed, it is recorded and in force. If not, check the engine runs with `-operator` (`systemctl status ti-engine@<instance>` shows its command line); nothing else is needed: it records the file before the adapter's next input |
| `.delivered` marker in the notes folder and no `.refused` note | Recorded in the durable log at the sequence the marker states, and in force | Nothing; `journal-query operator <log>` shows it |
| `.delivered` and a `.refused` note: `kill switch for account "..."; this gate is "joint"` | Recorded with no effect: `-account` was not the configuration's name (the IBKR number, say) | Record it again with `-account joint` and the current time |
| `.refused`: `kill switch <id> states <time>, earlier than <id>, already applied ...` | Recorded with no effect: backdated with `-at` before a kill switch already applied | Record it again with the current time |
| `.refused`: `... states <time>, later than now (...) by more than the 5m0s the clocks may differ ...` | Never recorded: any instruction more than five minutes ahead of the engine's clock | Record it again with the current time |
| `.refused`: `operator: decode the operator record: ...`, `... has more after the record ...`, `... is not the record its payload makes ... edited or damaged ...`, or an unknown type or schema version | Never recorded: the file was edited or damaged, or written by another build | Record the instruction again with the tool; never edit inbox files |
| `.refused`: `acknowledgement ... is for account "..."; this run decides "joint"` | Recorded with no effect; the alert keeps repeating | Record it again with `-account joint` |
| The alert keeps repeating after a delivered acknowledgement with no `.refused` note | The key named no alert (a typing slip): recorded, does nothing | Copy the key from the alert again, in single quotes |
| `.refused`: `... names orders [...], but the orders not terminal are [...]`, `names <id>, but the rebalance in progress is <id>`, or `no rebalance is in progress ...` | Recorded with no effect | Clear again naming exactly the decision and orders the note lists |
| `.refused`: `targetweight: clearance <id> names order <id>, which is not an order this run released that the venue has not ruled on; Boost clears only an order the adapter never sent`, or `... which the venue has already named as order <n> ...`, or `... which decision <id> did not release`, or `... is for account "..."` | A clearance sent to the Boost engine that it will not read: the order is not one awaiting IBKR's word (already ruled on, in dry-run none is ever listed), IBKR has named it so it was sent, another decision released it, or the account is wrong. Recorded with no effect | Check with `journal-query orders` and IBKR's app which orders are open. Clear again naming exactly the decision that released the order and an order the adapter never sent; if IBKR has named it, do not clear it, wait for its verdict |
| `engine: operator record <file> was not delivered, so it stays in the inbox and is delivered at the next start: ...` on the engine's output | No note; the run stops: the disk failed, or the log is full (the most records a run holds) | Resume the run, or after a full log start a fresh one ([restart](restart.md)); the instruction is recorded first |
| `engine: list the operator inbox <inbox>, so a kill switch in it could wait unread: ...`, `engine: read the operator record <file>, which could be a kill switch, so the run cannot decide with it unread: ...` or `engine: list the engine's operator notes <folder>, which say which records are settled, so the inbox cannot be read safely: ...` | The run stops with that error: it may be a kill switch, so nothing is decided with it unread | Fix the folder's or the file's owner or mode (`ti-operator:ti-trader`, `0750` for the folder, `0640` for a record; the notes folder is the engine's, `0700`), then resume the run ([restart](restart.md)) |
| `engine: note the refusal of <file> at <note>: ...` | The record was refused (no effect), but its note was not written, so the next start reads it again | Check the disk and the notes folder (the engine's own, `0700`). As root, move the record out of the inbox before the next start (`sudo mkdir -p /var/lib/trend-investing/engines/<instance>/operator-old && sudo mv /var/lib/trend-investing/engines/<instance>/operator/<file> /var/lib/trend-investing/engines/<instance>/operator-old/`, where `<instance>` is the engine instance, the name of its configuration file (`boost`, `trad-ira` or `roth-ira`), and `<file>` is the file the error names; `operator-old` sits beside the inbox, as in the upgrade step above): one refused for a time more than five minutes ahead would be delivered once that time has passed. Record the instruction again with the current time if you still mean it |
| `engine: mark <file> delivered at ...: ...` | The instruction is in the log, but its marker could not be written | Check the disk; the log entry stands, and the engine does not record it twice |
| The engine will not start: `engine: the operator inbox: ...` or `... is not a directory` | Nothing runs | The provisioner creates the folder and the engine's service file passes it as `-operator`: recreate it as "The operator inbox" says, or run `sudo deploy/provision.sh services` again. On a host the provisioner did not set up, correct the `-operator` path you pass |
| The engine will not start: `engine: the operator inbox <inbox> is mode <m>, writable by its group or by others; the provisioner creates it 0750` | Nothing runs: a kill switch could be written by anyone who can write the folder | `sudo chmod 0750 <inbox>` |
| The engine will not start: `engine: the operator inbox <inbox> is owned by root`, or `... is owned by the engine's own user (<n>), which could then write its own instructions`, or `engine: the engine runs as root, which can write any operator inbox ...` | Nothing runs: the owner of the inbox must be a user the engine does not run as, and the engine must not run as root | `sudo chown ti-operator:ti-trader <inbox>`; run the engine as `ti-trader` |
| The engine will not start: `engine: cannot tell who owns the operator inbox <inbox> ...`, or `engine: the engine's groups, to check it can read the operator inbox ...` | Nothing runs: the owner or the engine's groups could not be read | Check the folder and the engine's user (`id ti-trader`); then start again |
| The engine will not start: `engine: the operator inbox <inbox> is mode <m>, readable by others ...`, or `engine: the engine's user cannot list and read the operator inbox <inbox> ...`, or `engine: the engine cannot list the operator inbox ...`, or `engine: the engine cannot read the operator record <file> in <inbox> ...` | Nothing runs: the records are read only through the inbox's group, and a kill switch must not wait unread | `sudo chgrp ti-trader <inbox>` and `sudo chmod 0750 <inbox>`; for a record, `sudo chown ti-operator:ti-trader <file>` and `sudo chmod 0640 <file>`; then start again |
| The engine will not start: `engine: the operator inbox <inbox> holds <file>, a marker an earlier layout wrote there (or a forged one) ...` | Nothing runs: an earlier build wrote `.delivered` and `.refused` markers in the inbox, and this build does not read them, so the record each names could be read again | Move every file that has a marker out of the inbox, with its markers, into `operator-old` beside it, as "Upgrading from a build that wrote its notes into the inbox" says; then start again |
| The engine will not start: `engine: the engine's operator notes <folder> is mode <m>, writable by its group or by others`, or `... is not a directory`, or `... is not the engine's own (owner <n>, the engine runs as <m>)`, or `engine: the engine cannot list and read its operator notes <folder> ...`, or `engine: create the engine's operator notes ...`, or `engine: the engine's operator notes: ...` | Nothing runs: a forged note could silence your kill switch | For the engine's own folder in the wrong mode, `sudo chmod 0700 <folder>`. Otherwise move the folder aside; the engine creates it again. Move aside only a folder whose records are all in a log still in the log folder (the log, not the notes, stops an instruction being recorded twice); a record whose log you quarantined earlier would be recorded again, so move those records out of the inbox first |
| The engine will not start: `-operator applies to a run that can reach a broker; a backtest takes no operator input` | Nothing runs | Leave `-operator` out of a backtest |
| You record the switch while the engine is stopped | The engine records it before its first input from the adapter after it starts | Nothing more |
| The engine is restarted fresh while the switch was on | The fresh run starts with the switch off; `operator approve` warns you | Record the switch again before starting it |
| A Boost order refused under the switch, after you release it | Planned again at the next close, for the difference to Boost's target | Nothing; check the next close's release with `journal-query` |
| An order is already working at IBKR when you engage the switch | The switch stops only new orders from this program; it does not cancel anything at IBKR | Cancel it in IBKR's own app. Stopping the Gateway is not a cancel: an order the engine released goes out once the Gateway is back the same day |

## What backs this

ADR 0026, points 3 and 5 (the kill switch and the rebalance in progress as
recorded inputs); ADR 0019, amended 2026-09-24, "Alerting" (an alert
"repeats at a fixed interval until the owner acknowledges it.
Acknowledgement is recorded"); ADR 0027 and its amendment of 2026-10-17 (the durable log,
the fresh-run approval); ADR 0019, amended 2026-10-15 (one sale per
incident); ADR 0015 (each new input type is versioned: `operator.rebalance.cleared`
is version 1).

Tests: `TestTheKillSwitchIsRecordedInTheInbox`,
`TestTheClearanceIsRecordedInTheInbox`,
`TestTheAcknowledgementIsRecordedInTheInbox`,
`TestAnInputTheToolCannotRecordIsRefused`,
`TestTheApprovalStatesWhatThePreviousLogEndsIn`,
`TestTheApprovalReadsTheAdaptersSettlementOfANeverSentSale`,
`TestTheAdaptersSettlementLineIsOneTheApprovalReads`,
`TestTheApprovalWarnsOfAKillSwitchLeftEngaged`,
`TestTheApprovalWarnsOfTheSwitchThisAccountLeftEngaged`,
`TestTheInboxAndApprovalDirectoriesAreFlushed`,
`TestEachClearedOrderIDIsTrimmed`,
`TestARecordTheEngineCouldNotReadIsRefused`,
`TestARecordIsReadableThroughTheInboxsGroup` (`cmd/operator`, what the
tool records); `TestTheOperatorsKillSwitchStopsAndReleasesOrders`,
`TestAnOperatorInputIsDeliveredOnce`,
`TestAnOperatorRecordTheEngineRefusesIsNoted`,
`TestALogFailureWhileDeliveringAnOperatorRecordLeavesItForTheNextStart`,
`TestADeliveredRecordIsMarkedSoAMovedLogCannotRedeliverIt`,
`TestAKillSwitchOlderThanTheOneInForceIsRefused`,
`TestADeliveredRecordIsNotReadAgain`,
`TestARefusedInputAfterOperatorInputsStatesItsSequence`,
`TestARecorderRefusalAfterOperatorInputsStopsTheRun`,
`TestAResumedLogEndingInOperatorInputsAnswersTheNextInput`,
`TestAnInputSentAgainIsCheckedAgainstTheSequenceItWasSentWith`,
`TestAResumeWithAnApprovalStatesOneNextSequence`,
`TestTheOperatorInboxIsForARunThatCanReachABroker`,
`TestTheOperatorInboxMustNotBeWritableByOthers`,
`TestTheOperatorInboxBelongsToAUserTheEngineDoesNotRunAs`,
`TestTheEngineWritesNothingIntoTheOperatorInbox`,
`TestTheEnginesNotesFolderIsItsOwn`, `TestAMarkerInTheInboxDoesNotSettleARecord`,
`TestARecordAnEarlierLayoutDeliveredIsNotRecordedTwice`,
`TestARecordUnreadableAtRunTimeStopsTheDelivery`,
`TestAnUnlistableInboxAtRunTimeStopsTheDelivery`,
`TestTheProvisionerCreatesTheOperatorInbox`,
`TestNoNetworkFacingUnitCanWriteAnInstruction`,
`TestAnApprovalCannotUnderstateTheIncidentsSaleAttempt` (`cmd/engine`, how
the engine responds); `TestARefusalThatLeavesTheEngineNormalIsReplannedAtTheNextDecision`,
`TestTheOwnersClearanceEndsARebalanceInProgress`,
`TestAClearanceMustNameTheRebalanceInProgress`,
`TestBoostRefusesAClearanceOfAnOrderItNeverReleased`,
`TestAClearedNeverSentOrderIsNoLongerUndecided`,
`TestAClearanceOfNoUndecidedBoostOrderIsRefused`
(`internal/targetweight`); `TestReadEndStateFindsWhatAFreshRunMustBeToldOf`,
`TestAnApprovalNeverUnderstatesTheSaleAttempt`,
`TestProposeApprovalStatesTheEndStateOrClearsTheIncident`
(`internal/restart`); `TestADecodedRecordIsExactlyOneTheToolWrites`,
`TestARecordIsDeliveredAsAnOperatorInput` (`internal/operator`);
`TestTheOperatorReportListsEveryOperatorInput` (`internal/journalquery`);
`TestAnAcknowledgementTheOperatorToolRecordsStopsTheRepeats`
(`cmd/reporter`, the alert's key through the tool, the record decoded and
enveloped by the functions the engine delivers with, and no repeat after it
on either channel); `TestAnAlertAcknowledgementIsDeliveredAndJournalled`
(`cmd/engine`, the engine delivering and journalling it, with no refusal);
`TestARefusalRecordedAtAnotherSequenceStatesIt` (`transport`);
`TestRebalanceClearedValidate` (`internal/event`); for the retire command,
the owner's decision of 2026-10-22 (ADR 0026, point 2, "Boost", amended
2026-10-22) and `RetireTests` in `adapter/ibkr/tests/test_relay.py`
(`test_the_owners_retire_clears_the_stopped_order_and_the_relay_goes_on`,
`test_the_retire_command_refuses_an_order_that_is_not_stopped_as_unsendable`,
`test_the_retire_command_refuses_a_refusal_that_is_not_a_shape_ibkr_can_never_accept`,
`test_the_retire_command_refuses_anyone_but_the_owner`,
`test_the_retire_command_line`) and
`test_an_order_ibkr_can_never_accept_stops_until_the_owner_retires_it`
(`adapter/ibkr/tests/test_paper.py`);
`deploy/rehearsal/test-operator-user.sh` (the provisioner gives an existing
`ti-operator` the group `ti-trader`, or stops naming the mismatch).

## Not built yet

- The momentum engines in the running program: only Boost is composed, so a
  rebalance clearance has nothing to act on yet.
- Acknowledgements in `journal-query operator`: use the `.delivered` marker
  in the engine's notes folder.
- A day-trade counter for the margin account (counted by hand for now: see
  the [margin-warning runbook](../runbooks/margin-warning.md)).
