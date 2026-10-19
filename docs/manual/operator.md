# The operator tool: your kill switch, your approval, your clearance, your acknowledgement

*For the owner. How you tell a running engine to stop ordering, approve a
fresh run, clear a rebalance that is stuck, and acknowledge an alert so it
stops repeating, what each of these records, why it works this way, and
what can go wrong.*

## What it is

`operator` (source: `cmd/operator`) is a small command you run on the
server, **as the engine's user, `ti-trader`**: `sudo -u ti-trader operator
...`. It never talks to IBKR and never changes a running engine's state
by itself. It writes your instruction down, checked, in a place the engine
reads, and the engine records it in its durable log as one of its inputs
before acting on it. That is the rule for everything a person tells the
system (ADR 0026, point 3): an instruction is a recorded input, never a
setting the program glances at, so the log always shows who said what,
when, and what the engine did next, and a replay of the log does the same
thing again.

**Run it as `ti-trader`.** The tool writes each file readable by the user
that ran it only (mode `0600`), and the engine runs as `ti-trader`, so a file
written as you, or as root, is one the engine cannot read: the engine would
report it as waiting at every input and never act on it. So the tool checks
who owns the inbox (that is the engine's user) and refuses to write when it
is run as anyone else, saying `run it as the engine's user, sudo -u <engine
user> operator ...`. Approval files are read by the engine too: write them
as `ti-trader` as well, into a folder `ti-trader` can read. Every example
below leaves out the `sudo -u ti-trader` in front to keep it short.

It has four commands:

| Command | What it records | Where it goes | When the engine acts |
| --- | --- | --- | --- |
| `operator kill-switch` | the kill switch engaged or released | the engine's operator inbox | before the engine's next input from the adapter |
| `operator rebalance-cleared` | your clearance of a rebalance that is stuck | the engine's operator inbox | before the engine's next input from the adapter |
| `operator acknowledge` | your acknowledgement of one alert, by its alert key | the engine's operator inbox | before the engine's next input from the adapter; the reporter stops repeating the alert at its next run |
| `operator approve` | your approval of a fresh run | an approval file you pass to the engine with `-approval` | when that fresh run starts, as its second input |

**`-account` is the account's name in the engine's configuration**, its
`account_id`: `joint` for Boost (`/etc/trend-investing/engines/boost.config`).
It is not the IBKR account number, which only the adapter holds. The alerts
use the same name (`joint: engine halted (...)`), and every alert key ends
with `@joint`. An instruction for any other name is refused by the engine
with no effect (see "What can go wrong" below).

## The operator inbox

Each engine is started with `-operator <folder>`, its inbox, a folder that
must exist; a backtest takes none (the engine refuses to start with both:
`-operator applies to a run that can reach a broker`).

**On the server the inbox is not set up for you.** The provisioner
(`deploy/provision.sh`) installs the `operator` tool itself, in
`/opt/trend-investing/bin` and linked into `/usr/local/bin`, when
`deploy/build-engine.sh` has built it. But it does not create the inbox
folder, and the engine's service file (`ti-engine@.service`) does not pass
`-operator`. Until you do both, an engine on the server reads no kill
switch, clearance or acknowledgement. Do this once per account, before
the engine starts:

```
sudo install -d -o ti-trader -g ti-trader -m 0750 /var/lib/trend-investing/engines/boost/operator
```

and, in that account's `run.env`, put `-operator` with that folder into
`TI_RESTART_ARGS` on every start, beside any `-approval` or `-resume`:

```
TI_RESTART_ARGS=-operator /var/lib/trend-investing/engines/boost/operator -approval /var/lib/trend-investing/engines/boost/approval-<new run>.json
```

The folder must belong to `ti-trader`: the tool refuses to write into a
folder owned by anyone else, and an engine whose `-operator` folder is
missing refuses to start (`engine: the operator inbox: ...`). The tool
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
writes a marker beside the file, ending `.delivered`, saying where it was
recorded. An instruction with that marker, or already recorded in any log
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
every case the engine writes a note beside the file, under the file's own
name, ending `.refused`, saying why, prints the same on its output
(`journalctl -u ti-engine@<account>`), and never tries that file again.

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

- a marker beside the file, the same name ending `.delivered`, holding
  `delivered: it is journalled at sequence <N>`:
  `sudo -u ti-trader cat /var/lib/trend-investing/engines/boost/operator/<file>.delivered`.
  If a `.refused` note sits beside it too, the engine recorded the
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

The momentum engine is not composed into the running program yet (only
Boost is), so today a clearance is read by the momentum strategy and its
tests only. A clearance sent to the Boost engine is recorded and refused
(`not one this handler reads`), with no effect. Clear only after IBKR shows the order finished. A later IBKR
report on a cleared order is refused, which stops the engine until you
reconcile. The reporter reads the clearance the same way, so that report
also raises the urgent "venue report not read" alert.

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
ntfy messages and no more PagerDuty incidents. Incidents already open in
PagerDuty stay open until you resolve them there. The alert is not marked
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
note beside the file in the inbox and on the engine's output. One for
another account is recorded and refused, with a `.refused` note
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
yet: the reporter reads them from the log, and the `.delivered` marker beside
the file in the inbox says at which sequence the engine recorded one.

`-account`, `-from`, `-to` and `-format csv|json` work as for the other
reports ([journal-query](journal-query.md)).

## What can go wrong

Every message below is what the tool prints, or what the engine writes in a
`.refused` note and on its output (`journalctl -u ti-engine@<account>`).
When the tool refuses, it writes nothing; run it again once fixed.

### When you run the tool

| What you see | What the program does | What you do |
| --- | --- | --- |
| `usage: operator <kill-switch\|rebalance-cleared\|acknowledge\|approve> [flags]` or `operator: unknown command "..."` | Nothing written | Use one of the four commands |
| `flag provided but not defined: -...`, or `operator <command>: unexpected arguments [...]` | Nothing written: a misspelt flag, or a value with spaces not in quotes | Check the spelling; quote `-detail "..."` |
| `operator: -inbox is required: the engine's operator inbox, its -operator folder` | Nothing written | Add `-inbox /var/lib/trend-investing/engines/<account>/operator` |
| `operator: the inbox: stat ...: no such file or directory` | Nothing written: the inbox folder does not exist | Create it as "The operator inbox" says, and start the engine with `-operator` |
| `operator: the inbox ... belongs to user <n>, the engine's user, but this tool runs as user <m> ...` | Nothing written: you ran it as yourself or root, and the engine could not read the file | Run it as `sudo -u ti-trader operator ...`. If the folder itself belongs to root, fix its owner (`sudo chown ti-trader:ti-trader <inbox>`) |
| `operator: write into <inbox>: ... permission denied` (or `write`, `flush`, `install`) | Nothing written | Check the folder's owner and mode; check the disk is not full |
| `operator: <file> already holds a record, which is never overwritten (ADR 0018); record it again a second later` | Nothing written: two instructions of one kind in the same second (or the same `-at`) share a file name | Run it again a second later, or with a later `-at` |
| `operator: flush the inbox <inbox>, so the record's name survives a power loss: ...` | The file was written, but the folder could not be flushed to disk | Check the disk. The record is in the inbox; look for its `.delivered` marker before recording it again |
| `operator: -state must be engaged or released, not "...": the switch is stated, never assumed` | Nothing written | Run it again with `-state engaged` or `-state released` |
| `operator: -at must be an RFC 3339 time: ...` | Nothing written | Give a time like `2026-10-17T14:30:05Z`, or leave `-at` out |
| `operator: invalid kill switch payload: account id is required` (or `operator is required`, `detail is required`) | Nothing written: `-account`, `-by` or `-detail` missing | Add the missing flag |
| `operator: invalid rebalance cleared payload: decision id is required ...`, `client order ids are required ...`, `client order id <n> is empty`, or `client order id must be strictly ascending; "x" follows "x"` | Nothing written: `-decision` or `-orders` missing, an empty name between commas, or an order named twice | Name the decision and each open order once (`journal-query orders`) |
| `operator: invalid alert acknowledged payload: account id is required` (or `alert key is required`, `acknowledged by is required`) | Nothing written | Add `-account`, `-alert` or `-by` |
| `operator: -alert must hold no newline: ...` | Nothing written (spaces around the key are trimmed, a line break inside it is refused) | Copy the key from the alert again, in single quotes, on one line |
| `operator: -log is required: ...` or `operator: -out is required: ...` (`approve`) | Nothing written | Name the previous run's log and where to write the approval |
| `operator: open the previous log: ...` or `operator: <log> cannot be read as a durable log (docs/runbooks/durable-log.md): ...` | Nothing written | Check the path; a damaged log follows `docs/runbooks/durable-log.md`, "A log that cannot be read" |
| `operator: <log> holds nothing past its configuration: its start was cut short, so resume it ...` | Nothing written | Resume that run with `-resume` ([restart](restart.md)) |
| `restart: <log> records no incident, so there is none to clear: it ended normal`, or `restart: <log> ended <state> in incident <id>, so that is the incident to clear, not <id>` | Nothing written: `-clear-incident` names an incident the log did not end in | Leave `-clear-incident` out, or copy the incident ID from the tool's own output without it |
| `restart: the approval is refused` with `account id is required` or `approved by is required` | Nothing written | Add `-account` or `-by` |
| `engine: decode <id> in <log>: ...` (`approve`) | Nothing written: a record the approval depends on cannot be read | Treat the log as damaged: `docs/runbooks/durable-log.md` |
| `operator: <file> already exists; an approval is evidence and is never overwritten (ADR 0018)` | Nothing written | Choose a new `-out` name |
| The approval prints `the kill switch was engaged when it ended; a fresh run starts with it released ...` | The approval is written; the fresh run will start with the switch off | Record `operator kill-switch -state engaged` before starting the engine |

### After the tool has written it

| What you see | What the program does | What you do |
| --- | --- | --- |
| No `.delivered` marker beside the file | The engine has not read the inbox since: it is stopped, was started without `-operator`, or the adapter has sent it nothing | Check the engine runs with `-operator` (`systemctl status ti-engine@<account>` shows its command line); nothing else is needed: it records the file before the adapter's next input |
| `.delivered` marker and no `.refused` note | Recorded in the durable log at the sequence the marker states, and in force | Nothing; `journal-query operator <log>` shows it |
| `.delivered` and a `.refused` note: `kill switch for account "..."; this gate is "joint"` | Recorded with no effect: `-account` was not the configuration's name (the IBKR number, say) | Record it again with `-account joint` and the current time |
| `.refused`: `kill switch <id> states <time>, earlier than <id>, already applied ...` | Recorded with no effect: backdated with `-at` before a kill switch already applied | Record it again with the current time |
| `.refused`: `... states <time>, later than now (...) by more than the 5m0s the clocks may differ ...` | Never recorded: any instruction more than five minutes ahead of the engine's clock | Record it again with the current time |
| `.refused`: `operator: decode the operator record: ...`, `... has more after the record ...`, `... is not the record its payload makes ... edited or damaged ...`, or an unknown type or schema version | Never recorded: the file was edited or damaged, or written by another build | Record the instruction again with the tool; never edit inbox files |
| `.refused`: `acknowledgement ... is for account "..."; this run decides "joint"` | Recorded with no effect; the alert keeps repeating | Record it again with `-account joint` |
| The alert keeps repeating after a delivered acknowledgement with no `.refused` note | The key named no alert (a typing slip): recorded, does nothing | Copy the key from the alert again, in single quotes |
| `.refused`: `... names orders [...], but the orders not terminal are [...]`, `names <id>, but the rebalance in progress is <id>`, or `no rebalance is in progress ...` | Recorded with no effect | Clear again naming exactly the decision and orders the note lists |
| `.refused`: `input type "operator.rebalance.cleared" is not one this handler reads` | A clearance sent to the Boost engine: recorded with no effect | Nothing; Boost has no rebalance to clear |
| `engine: operator record <file> was not delivered, so it stays in the inbox and is delivered at the next start: ...` on the engine's output | No note; the run stops: the disk failed, or the log is full (the most records a run holds) | Resume the run, or after a full log start a fresh one ([restart](restart.md)); the instruction is recorded first |
| `engine: list the operator inbox ...` or `engine: read the operator record ...: ...; it waits for the next input` | Nothing recorded meanwhile; it tries again at the next input | Fix the folder's or file's owner or mode (`ti-trader`) |
| `engine: mark <file> delivered at ...: ...` | The instruction is in the log, but its marker could not be written | Check the disk; the log entry stands, and the engine does not record it twice |
| The engine will not start: `engine: the operator inbox: ...` or `... is not a directory` | Nothing runs | Create the folder ("The operator inbox") or correct `-operator` in `run.env` |
| The engine will not start: `-operator applies to a run that can reach a broker; a backtest takes no operator input` | Nothing runs | Leave `-operator` out of a backtest |
| You record the switch while the engine is stopped | The engine records it before its first input from the adapter after it starts | Nothing more |
| The engine is restarted fresh while the switch was on | The fresh run starts with the switch off; `operator approve` warns you | Record the switch again before starting it |
| A Boost order refused under the switch, after you release it | Planned again at the next close, for the difference to Boost's target | Nothing; check the next close's release with `journal-query` |
| An order is already working at IBKR when you engage the switch | The switch stops only new orders from this program; it does not cancel anything at IBKR | Cancel it in IBKR's own app. Stopping the Gateway is not a cancel: an order the engine released goes out once the Gateway is back the same day |
| The server has no inbox yet, and you need to stop trading now | No recorded kill switch is possible | Stop the engine and the adapter (`sudo systemctl stop ti-engine@boost ti-adapter-boost`), and cancel at IBKR ([server](server.md), "Reading the logs and the kill switch on the server") |

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
`TestTheApprovalWarnsOfAKillSwitchLeftEngaged`,
`TestTheApprovalWarnsOfTheSwitchThisAccountLeftEngaged`,
`TestTheInboxAndApprovalDirectoriesAreFlushed`,
`TestEachClearedOrderIDIsTrimmed`,
`TestARecordTheEngineCouldNotReadIsRefused` (`cmd/operator`, what the
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
`TestAnApprovalCannotUnderstateTheIncidentsSaleAttempt` (`cmd/engine`, how
the engine responds); `TestARefusalThatLeavesTheEngineNormalIsReplannedAtTheNextDecision`,
`TestTheOwnersClearanceEndsARebalanceInProgress`,
`TestAClearanceMustNameTheRebalanceInProgress`, `TestBoostReadsNoClearance`
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
`TestRebalanceClearedValidate` (`internal/event`).

## Not built yet

- The inbox in the server's setup: `deploy/provision.sh` does not create
  `/var/lib/trend-investing/engines/<account>/operator`, and
  `ti-engine@.service` does not pass `-operator`. You add both by hand, as
  "The operator inbox" says. The tool itself is built by
  `deploy/build-engine.sh` and installed by the provisioner.
- The momentum engines in the running program: only Boost is composed, so a
  rebalance clearance has nothing to act on yet.
- Acknowledgements in `journal-query operator`: use the `.delivered` marker.
- A day-trade counter for the margin account (counted by hand for now: see
  the [margin-warning runbook](../runbooks/margin-warning.md)).
