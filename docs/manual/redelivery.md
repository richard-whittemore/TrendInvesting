# Re-delivery: an order the engine decided is never lost and never doubled

*For the owner. What happens to an order when the engine or the adapter
stops at the wrong moment, and why it is sent exactly once.*

## What it does

The engine writes every decision to its durable log, forces it to disk, and
only then replies to the adapter (journal-before-send, ADR 0027). So there is
a moment, after the write and before the reply arrives, when an order exists
on disk but the adapter has not received it. A crash in that moment would
leave the order decided but never sent.

Re-delivery closes that gap:

- **The adapter can ask for the reply to any input it sent.** It asks by
  sending that input again, unchanged, under the same ID. The engine looks up
  what it recorded for that input and sends back exactly that reply, byte for
  byte. It does not decide again.
- **This works after a restart too.** When the engine resumes from its log
  after a crash (see [restart](restart.md)), it rebuilds the record of every
  reply. The adapter asks again for the input it never got an answer to, and
  receives the order that was decided before the crash.
- **The same input never makes a second order.** Asking again returns the
  same reply; the input is not applied twice, so nothing new is recorded.
  An input sent under an ID the engine has already used, but with different
  content, is refused: one ID names one input.
- **The adapter remembers what it is waiting for.** Before it sends an input,
  the IBKR adapter writes it to a small file. It deletes the file only after it
  has acted on the reply. If the adapter itself restarts, it asks for that
  input's reply first, before sending anything new.
- **The adapter sends each order once.** It hands an order to IBKR only if the
  reply marks it to be sent (dry-run orders never are), it has not sent it
  already, and IBKR shows no order with that order's tag (its client order
  ID, which IBKR calls `orderRef`), neither open nor already executed.
- **The adapter writes down that it is about to send, before it sends.**
  Once the order's request to IBKR is fully built, and just before it is
  handed over, it records on disk "about to send this client order ID", and
  the moment the sender returns, "sent". That means the request left the
  adapter; IBKR's acknowledgement is the separate lifecycle report that
  moves the order to acknowledged. An order whose request cannot be built is
  never recorded, since it never left the adapter. It never sends a
  recorded order again. If the adapter stopped in between, it cannot know
  whether IBKR got the order, so after its restart it looks for the order in
  IBKR's open orders **and** its executions (an order that has already filled
  is no longer open). If either shows it, IBKR has it and nothing is sent. If
  neither does, the adapter stops with "was being handed to the broker when
  the adapter stopped" and sends nothing more until you have looked: the order
  may have reached IBKR and been cancelled, or never arrived, and only you can
  tell which.
- **A changed input never replaces the one waiting for its reply.** If the
  adapter is asked to send an input under the ID of the one it is still
  waiting on, but with different content, it first finishes the original
  (asks for its reply and sends its orders once), then refuses the changed
  copy. The original is never overwritten on disk.

## Why

A real order can be decided once and must be sent once. Sending it twice
doubles the position; not sending it at all leaves the account off its
target without anyone knowing. The engine cannot tell whether a reply
reached the adapter, so it never resends on its own (ADR 0027 §6). The
adapter is the only side that knows what it received, so the adapter asks.
Because the answer always comes from the log, it is the same answer before
and after a crash.

## The rules and their sources

- ADR 0027 §4: journal-before-send. Durable means *possibly* sent.
- ADR 0027 §6: a restart rebuilds the run from the log and sends nothing by
  itself. The amendment "Re-delivery on request" records that the adapter
  asks for a reply by sending its input again.
- ADR 0026 point 2: every order carries a client order ID derived from the
  decision, which is what lets IBKR and reconciliation recognise it.
- ADR 0019: an order IBKR holds that the log does not explain is a
  reconciliation difference, which is why the adapter checks IBKR before
  sending anything a second time.

## The tests

- `cmd/engine`, `TestAnInputSentAgainIsAnsweredWithItsCommittedReply`: the
  same input sent twice gets the same reply, byte for byte, and the log holds
  one order.
- `cmd/engine`, `TestAnotherInputUnderACommittedIDIsRefused`: a different
  input under a used ID is refused and nothing is recorded.
- `cmd/engine`, `TestAResumedEngineServesACommittedButUnsentOrderAgain`: the
  engine stops after recording an order the adapter never received; resumed
  from its log, it hands over the same reply, the order included, and the new
  log holds no second order.
- `adapter/ibkr/tests/test_relay.py`: the adapter asks again after a crash
  and sends the order once; it never sends an order twice when the reply comes
  twice; it skips an order IBKR shows open or executed; after its own restart
  it asks for the pending input before the next one; it never sends a dry-run
  order.
  - `test_an_order_handed_over_before_a_crash_is_never_sent_again`: the
    adapter stops mid-send; by its restart the order has filled, so only its
    execution shows it, and it is not sent again.
  - `test_a_handover_the_broker_does_not_show_stops_the_relay_rather_than_resend`:
    the adapter stops mid-send and IBKR shows nothing; the restarted adapter
    refuses to go on, every time, and the order is sent once only.
  - `test_a_handover_record_with_an_unknown_mark_stops_the_relay_and_sends_nothing`:
    a hand-edited mark such as `"Sent"` stops the adapter; the order is not
    sent again.
  - `test_resolving_a_handover_as_sent_lets_the_relay_go_on_without_sending_it`
    and `test_resolving_a_same_session_handover_for_resend_sends_it_once_more`:
    the resolve command's two decisions; a resend goes out at the original
    limit.
  - `test_a_resend_is_refused_unless_the_order_was_committed_in_todays_session`
    and `test_resolving_an_order_the_record_does_not_hold_is_refused`: the
    command changes nothing for an earlier session's order, an unknown client
    order ID, an unknown decision or an empty reason.
  - `test_an_order_marked_sent_cannot_be_resolved`: neither decision is
    accepted for an order the adapter recorded as handed over, and nothing is
    sent again.
  - `test_a_resend_permission_expires_with_its_day_and_stops_for_another_check`:
    an adapter started the day after a resend was allowed sends nothing and
    stops, and the order can then only be marked sent.
  - `test_a_resend_permission_can_be_withdrawn_by_marking_the_order_sent`:
    an owner who allowed a resend and then found the order at IBKR marks it
    sent; a second resend is refused, both decisions are kept, and nothing is
    sent again.
  - `test_a_resend_mark_is_read_only_in_the_shape_resolve_writes`: a resend
    date in any form but `YYYY-MM-DD` (such as `20261016` or a week date)
    stops the adapter, whatever the Python version reads as a date.
  - `test_resolve_is_refused_while_an_adapter_holds_the_pending_file`,
    `test_an_adapter_cannot_start_while_resolve_or_another_adapter_holds_the_pending_file`
    and `test_each_relay_holds_its_own_lock_and_a_closed_relay_never_edits_or_sends`:
    the adapter and the command never edit the record at the same time, and
    a stopped relay edits and sends nothing.
  - `test_close_from_another_thread_waits_for_the_record_being_cleared`,
    `test_close_gives_up_after_its_timeout_while_the_sender_runs_and_works_once_it_returns`
    and their neighbours: stopping the adapter from another thread waits for
    the order or record edit in flight (at most `close_timeout`, 60 seconds
    by default), then raises `RelayCloseTimeout` and leaves the adapter
    running and holding the file, so a supervisor sees a stuck sender
    instead of a hang.
  - `test_an_order_the_mapper_cannot_shape_leaves_no_mark_and_is_sent_after_a_restart`:
    an order whose broker request cannot be built never reached IBKR, so it
    leaves no "about to send" record and is sent once after a restart.
  - `test_a_changed_retry_under_a_pending_id_is_refused_and_the_original_settled`:
    a changed copy under the waiting input's ID is refused; the original's
    reply is asked for and its order sent once.

## What can go wrong

| What you see | What it means | What to do |
| --- | --- | --- |
| The adapter stops with "was being handed to the broker when the adapter stopped" | It stopped while sending that order, and IBKR shows it neither open nor executed. It may have been taken and cancelled, or never arrived. The adapter will not guess: it sends nothing more. | Look in IBKR for an order whose `orderRef` is the client order ID named. Do not place it by hand until you know. Leave the adapter stopped (the command refuses with "stop it first" while the adapter runs), and settle the order with the adapter's resolve command; never edit the `.handover` file by hand (the adapter refuses any value in it it does not know and stops again). From the repository: `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "what IBKR showed"`, or `--resend` in place of `--sent`. `PENDING_FILE` is the adapter's pending file (not the `.handover` file next to it). Choose: **`--sent`** if IBKR shows the order at all (working, filled or cancelled): it is never sent again. **`--resend`** only if the order was committed in today's session and IBKR has no trace of it today (open orders, cancelled orders and today's trades): the adapter sends it again at its **original limit**, whatever the price has done since. Start the adapter the same day: the permission is dated, and an adapter started on a later day does not send the order but stops with "was allowed to be sent again on", and the order needs settling again, by `--sent` after checking IBKR's history for every day since. If, after choosing `--resend`, you find the order at IBKR after all (for example the audit trail shows it accepted and then cancelled), withdraw the permission before starting the adapter: run the same command with `--sent`. That is the only change the command accepts for an order already allowed to be sent again; a second `--resend` is refused. The command refuses `--resend` for an order from an earlier session, because one that filled or was cancelled on an earlier day no longer shows in today's lists: check IBKR's order and trade history for every day since the order was committed (an Account Management activity statement or the audit trail), then mark it `--sent` and let the next rebalance place a fresh order. The command also refuses a client order ID the record does not hold or does not mark `"handing-over"` or `"resend:"` and a date (an order marked `"sent"` was handed to IBKR and needs no decision, and no decision can undo that), so a typo naming any other order changes nothing; it keeps each decision, its reason, the time (UTC) and the session date it was judged against in the file ending `.handover.decisions`. A third decision, **`--never-sent`**, is only for an order the adapter refused before it wrote "about to send" and always will (one released for an earlier session, or one it can never shape); it is refused for any order the record marks. See [paper trading](paper-trading.md), "What can go wrong", for when to use it and the engine-side clearance that follows. Leave the pending file alone, and do not edit the engine's log: the log is the record of what the engine committed, and the adapter asks for the waiting input's committed reply again on restart. Then start the adapter. |
| The adapter stops naming the `.handover` file with "which is neither", "not an object of marks" or "is not JSON" | The `.handover` file holds something other than the adapter's own marks, most likely after a hand edit. The adapter sends nothing while it is so. | Do not guess which orders it covered. Restore the file to a JSON object from client order ID to `"handing-over"`, `"sent"`, `"never-sent"` or `"resend:"` and a date written exactly `YYYY-MM-DD` as the adapter wrote it (any other date form, such as `20261016`, is refused), then settle each `"handing-over"` order with the resolve command above. |
| The adapter, or the resolve command, refuses with "is held by another adapter or by relay.py resolve" or "stop it first" | Another process still has the pending file open: an adapter that is still running (perhaps hung) or a resolve command still at work. Only one may edit the hand-over record at a time. | Find that process and stop it, then try again. Never delete the file ending `.lock` beside the pending file: it stays on disk after every stop by design and is not a stale lock. Deleting it while a process still holds it lets a second one lock a new file, and two of them then edit the record at once. |
| The adapter reports "retried with other content" | The adapter was asked to resend its waiting input with something changed. The original was finished and its orders sent once; the changed copy was refused. | A fault in the adapter. Find why the input changed before starting again. |
| The adapter reports "committed with different content" | It sent a different input under an ID it had already used. That is a fault in the adapter, not the market. | Stop the adapter. Nothing was recorded for the refused input. Find why the input changed before starting again. |
| An "order not acknowledged" alert (see [order lifecycle](order-lifecycle.md)) | The order is on disk and IBKR has not reported on it. | Follow the alert's text. Look in IBKR for an order whose `orderRef` is the alert's client order ID **before** anything is placed by hand. |
| The engine was resumed and the adapter has no pending file | The adapter received its last reply before the crash and acted on it. | Nothing: the adapter sends its next input as usual. |
| The engine refuses to resume | The log could not be resumed (see [restart](restart.md)). No reply can be re-delivered from a run that did not resume. | Follow `docs/runbooks/durable-log.md`. A fresh run starts from a reconciled baseline, so check IBKR's open orders first. |

What this does **not** do yet: the IBKR adapter does not speak the engine's
socket yet. Sending is built for paper only and is tested against a fake
gateway (see [paper trading](paper-trading.md)); live stays switched off
(AGENTS.md rule 5).
