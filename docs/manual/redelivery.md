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
  byte (or, if it refused that input, the same refusal). It does not decide again.
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
- **An order IBKR can never accept is retired, and a retired order is never
  sent.** An order refused before "about to send" for good no longer
  wedges the adapter (the owner's decision of 2026-10-22). One whose day is
  over is retired by the adapter itself; one IBKR can never accept as it
  stands (not a limit order, a fraction of a share, no contract number)
  stops the adapter until you retire it with `relay.py retire`. Either way
  the adapter records the retirement for the engine first, then in its
  decisions file, then marks the order never sent, so a stop part-way
  through retires it again on restart without a second record, and asking
  again for the reply that released it passes over the order. The order is
  never sent, the next input goes on, and IBKR is checked first as for any
  order: an order IBKR shows under its tag is never retired. See [paper
  trading](paper-trading.md), "What can go wrong", and
  [operator](operator.md), "Retiring an order the adapter can never send".
- **An order an ended run left unsent is settled for the fresh run, so a
  sale back to 98% is tried again.** When an engine run ends (you stop it
  to leave Halted, to upgrade, after a reboot) while the adapter is still
  waiting on an input whose reply released orders it never sent, that
  run's engine can no longer be told they were never sent, so its log
  still counts the incident's one sale back to 98% as attempted. Before you
  approve the fresh run, with the adapter and the engine stopped, settle
  them (the owner's decision of 2026-10-22):

  ```
  python3 adapter/ibkr/relay.py settle-ended-run PENDING_FILE --journal ENDED_LOG --by "your name" --reason "why the run ended"
  ```

  `ENDED_LOG` is the durable log of the run that ended (its `-log` file),
  the run the adapter sent the waiting input to, if one is waiting. It
  reads every input that log commits, not only the waiting one: an order
  you settled `--never-sent` or `--not-handed-over` at an earlier input,
  or that the adapter retired, is covered too. A decision counts only
  when it names the input and the run it was made in: one an earlier
  build recorded without its input or its run is never evidence, so its
  order is listed not settled with that reason and its sale stays
  attempted (check IBKR and sell by hand if the sale is still needed). A retirement of it still queued for the ended run's engine
  does not stop it being settled: it is struck off. For each order a committed
  reply released to be sent that the adapter's records show never sent, it
  writes one settlement line to `PENDING_FILE.never-sent`: the log, the
  run, the input that released it, the adapter's evidence (`unmarked`,
  `refused` or `never-sent`) and the retirement it amounts to, retired by
  you with your reason. It then marks the order never sent, so no adapter
  ever sends it, and strikes off any retirement of it still queued for the
  ended run's engine, which the fresh run's engine would refuse. It never
  settles an order that may have reached IBKR. It prints "order ID not
  settled" with the reason only for an order you must act on: one marked
  about to be sent or allowed to be sent again; one the adapter found IBKR
  showing (when the adapter stops with "settled as never sent, but IBKR
  shows" it records the order `at-broker` first, before it strikes any
  retirement of it, so a stop part way still leaves it `at-broker`); one
  the ended log released more than once, since one release may have been
  sent; one whose last recorded decision was made in another run; one IBKR
  reported on (the ended log or the outbox holds a venue's report) though
  the adapter's records show it never sent; a never-sent mark no recorded
  decision backs; a never-sent decision recorded for another input, or
  with no input or no run; and one whose last decision records a run
  that is not a run's identifier (a damaged file: check it). When any is listed it then prints "N order(s) not settled above:
  each one's sale stays attempted in the fresh run until you act on it as
  its line says". An order simply sent (marked sent, reported on by IBKR,
  or already retired in the ended log) needs nothing of you: it is not
  listed, only counted in one line, "N order(s) the ended run sent or its
  log already retires: nothing to settle". The last line gives the count
  settled and says whether to approve with `-never-sent`. Running it again
  settles nothing twice. It refuses while the adapter runs, for a waiting
  input sent to another run, while the engine of `ENDED_LOG`'s run still
  runs (stop it first), with no input waiting for a log that is not the
  run that ended (the run the adapter's decisions last record: give
  `--journal` the log of the run the message names), with no input
  waiting and no decision that records its run (the message says so:
  which run ended, and which of its orders were never sent, cannot be
  proved, so nothing is settled; each sale stays attempted, so check IBKR
  for every order that run released, sell by hand any sale still needed,
  and approve without `-never-sent`), as root or as any user but the one
  that owns the adapter's files (as `retire`), and without your name and
  a reason. A damaged line in the decisions file refuses it naming the
  line; do not edit the file.

  That the run has ended is proved by the engine's instance lock,
  `engine.lock` in the engine's state folder (the folder of its `-log`,
  `/var/lib/trend-investing/engines/<instance>/` on the server): the
  engine creates it and holds it locked for as long as it runs, whatever
  log it writes, so an engine that resumed the run into a newer log still
  holds it. The command refuses while it is held ("still runs"), and
  refuses a folder with no `engine.lock` at all, since then no engine of
  this build wrote it and the run's end cannot be proved. Never delete
  `engine.lock`.

  The command writes the file whole each time (a temporary file renamed
  over it), so a stop never leaves half a line. A file left with a torn
  last line (a write cut short, by a power loss under an earlier build) is
  repaired by running the command again: it drops that line, prints
  "dropped a torn last line", and writes the file whole from the adapter's
  records. Only a last line with no line end after it is torn: any other
  line it cannot read, a whole damaged last line included, refuses the
  command; do not edit the file. Each decision it records is written the
  same way, whole or not at all.

  `--sent` is only for an order IBKR actually shows, and it cannot be
  undone: it strikes off the order's queued retirement, so the engine is
  never told the order was not sent, and the engine keeps the sale as
  working. Check IBKR first (open orders, cancelled orders and trade
  history, for the order reference that is the client order ID) and run
  it only once you have found the order there.

  If you find an order it settled at IBKR (open, cancelled or traded), it
  was sent: with the adapter stopped, mark it sent with `python3
  adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent
  --reason "what IBKR shows"`, then run `settle-ended-run` again, which
  now refuses that order and withdraws its line (it prints "settlement
  withdrawn"); a line is never applied for an order marked sent. `--sent`
  marks the order `at-broker` first (the mark the adapter stops at on
  every start, removing any retirement of it still queued), then records
  your finding, strikes off any retirement of the order still queued, and
  marks it sent last; if it stops part way (it prints a refusal or an
  error), run the same `--sent` again before anything else: it finishes
  what the stop left and records your finding only once.
  Then
  approve with `operator approve ... -never-sent PENDING_FILE.never-sent`
  (or without `-never-sent` when the command says no order is settled): the
  tool checks each line against the log by the engine's own retirement
  rule and the input that released the order, refuses the approval
  naming any line it does not bear out, and states each settled sale back
  to 98% not attempted, with one more of the incident's sales retired and
  the line carried as its evidence (`sale_settlements`). The engine checks
  the same lines against the log again before it starts, and the fresh run
  tries the sale again under the next attempt's client order ID. At the
  fresh run's start, settle the waiting input with `resolve --run-ended`
  as before; nothing of it is sent, and a retirement still queued of an
  order of it the adapter found at IBKR (`at-broker`) is struck off with
  it, so it never reaches the fresh run's engine.
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
  - `test_a_never_sent_order_ibkr_shows_open_or_executed_stops_for_a_person`,
    `test_never_sent_is_refused_for_any_order_but_the_one_the_relay_is_refusing`
    and `test_never_sent_is_refused_when_the_relay_recorded_no_refusal_for_the_waiting_input`:
    a never-sent settlement names only the order the adapter is refusing,
    and is honoured only while IBKR shows nothing under its tag.
  - `RetireTests` in `adapter/ibkr/tests/test_relay.py`:
    `test_an_order_whose_session_is_over_is_retired_by_the_relay_which_goes_on`,
    `test_a_retired_order_is_never_sent_when_its_input_is_asked_for_again`
    (a stop before the mark, or after it before the input is cleared, sends
    nothing and records the retirement once),
    `test_an_order_whose_session_is_over_that_ibkr_shows_is_never_retired`,
    `test_a_retirement_recorded_with_no_input_is_never_queued_for_an_order_ibkr_shows`,
    `test_a_retirement_recorded_with_no_input_is_finished_only_while_the_relay_holds_the_lock`,
    `test_a_retirement_recorded_with_no_input_that_is_not_as_the_relay_writes_one_stops_the_relay`,
    `test_a_retirement_recorded_with_no_input_is_queued_once_across_a_stop`,
    `test_a_retirement_already_queued_is_struck_off_for_an_order_ibkr_shows`,
    `test_an_order_ibkr_can_never_accept_stops_the_relay_for_the_owner` and
    the retire command's tests; `adapter/ibkr/tests/test_paper.py`,
    `test_a_release_left_waiting_past_its_session_is_retired_and_never_sent`.
  - `adapter/ibkr/tests/test_loop.py`,
    `test_an_earlier_runs_uncommitted_input_stops_a_fresh_run_for_a_person_and_is_never_sent_to_it`
    and `test_an_earlier_runs_committed_reply_with_an_unsent_order_stops_naming_it_and_settles_without_sending`:
    a fresh run is never sent an earlier run's waiting input, and never sends
    an order the earlier run released; the adapter stops naming them, and
    after `resolve --run-ended` nothing of them is sent.
    `test_run_ended_is_refused_for_an_input_of_the_current_run` and
    `test_after_an_earlier_runs_input_is_settled_the_fresh_runs_own_first_input_goes_through`:
    the settlement is refused for the running run's own input, and once it
    is made the fresh run's own inputs go through.
    `test_run_ended_settles_each_report_on_an_earlier_runs_order_so_none_drains_into_the_fresh_run`
    and `test_an_outbox_entry_recorded_at_a_settlement_is_never_sent_even_if_the_strike_was_cut_short`:
    every input waiting behind the settled one is settled with it and never
    sent to the fresh run.
    `test_two_unapproved_starts_are_two_runs_and_the_first_ones_input_stops_the_second`
    and `test_an_approved_fresh_run_resumed_after_a_crash_takes_its_own_input_awaiting_its_reply`:
    a run with no Run approval still has an identity of its own, and a
    resumed run, approved or not, is asked for its own input again.
    `test_a_crash_resume_keeps_the_saved_batch_and_delivers_the_rest_of_it_once`
    and `test_a_crash_resume_after_every_input_was_committed_sends_nothing_again`
    (`adapter/ibkr/tests/test_delivery.py`): a crash resume under a new
    `TI_RUN` keeps the day's market-data batch and its stream folder (found
    by the run's identity, [market data](market-data.md), "A crash
    resume"), so what the crashed run committed is never sent again and the
    rest goes once; `test_a_fresh_run_drops_the_earlier_runs_batch_and_needs_its_own_warm_up_folder`:
    a fresh run never takes the earlier run's batch or folder.
  - `adapter/ibkr/tests/test_loop.py`,
    `test_settle_ended_run_records_each_order_the_ended_run_released_and_the_adapter_never_sent`:
    the ended run's unsent order is settled once, with its evidence, in the
    line `operator approve -never-sent` reads (`cmd/operator`,
    `TestTheAdaptersSettlementLineIsOneTheApprovalReads`), marked never
    sent, and never sent by the fresh run;
    `test_settle_ended_run_never_settles_an_order_the_adapter_may_have_sent`,
    `test_settle_ended_run_names_the_order_the_adapter_recorded_it_refused`,
    `test_settle_ended_run_never_settles_an_order_the_ended_log_shows_retired_or_reported`,
    `test_settle_ended_run_never_settles_an_order_the_relay_found_at_ibkr`,
    `test_a_stop_after_any_write_of_the_at_broker_record_leaves_the_order_refused_by_settle_ended_run`,
    `test_marking_sent_an_order_stopped_at_the_broker_strikes_any_retirement_of_it_still_queued`,
    `test_settle_ended_run_never_settles_a_client_order_id_released_twice_or_decided_in_another_run`,
    `test_settle_ended_run_prints_only_the_orders_the_owner_must_act_on_and_counts_the_sent_ones`,
    `test_settle_ended_run_with_no_input_waiting_is_refused_for_a_log_that_is_not_the_run_that_ended`,
    `test_settle_ended_run_is_refused_while_the_ended_runs_engine_still_holds_its_instance_lock`
    (`cmd/engine`,
    `TestTheEngineHoldsItsInstanceLockWhileItRunsWhateverLogItWrites`,
    `TestTheEngineRefusesToStartWhileItsInstanceLockIsHeld` and
    `TestAnInstanceLockTheFilesystemCannotTakeRefusesTheStartAndLeavesNoLog`),
    `test_settle_ended_run_never_settles_on_a_decision_recorded_with_no_input_or_no_run`,
    `test_settle_ended_run_refuses_an_order_whose_last_decision_records_a_malformed_run`,
    `test_settle_ended_run_with_no_input_waiting_refuses_damaged_decisions_naming_the_line`,
    `test_marking_sent_a_never_sent_order_records_the_decision_before_it_strikes_any_queued_retirement`,
    `test_marking_sent_a_never_sent_order_stopped_after_any_step_delivers_no_retirement_on_restart`,
    `test_run_ended_strikes_a_queued_retirement_of_an_order_the_relay_found_at_ibkr`,
    `test_run_ended_strikes_a_queued_retirement_of_any_order_an_earlier_run_of_the_chain_released`,
    `test_run_ended_reads_no_log_older_than_the_bounded_walk_and_refuses_a_missing_log_the_walk_reads`,
    `test_run_ended_keeps_a_queued_retirement_of_an_order_the_fresh_run_released_and_it_is_delivered`,
    `test_an_order_marked_at_the_broker_is_never_read_as_settled_never_sent_whatever_its_last_decision_says`,
    `test_a_newline_terminated_damaged_settlement_line_is_refused_never_dropped_as_torn`,
    `test_a_decision_cut_short_mid_write_leaves_the_decisions_readable_and_settle_ended_run_runs_again`,
    `test_settle_ended_run_is_refused_for_a_log_that_is_not_the_run_the_input_was_sent_to`,
    `test_settle_ended_run_is_refused_as_root_or_any_user_but_the_owner_of_the_adapters_files`
    and `test_settle_ended_run_is_refused_without_a_person_and_a_reason_and_while_the_adapter_runs`:
    an order that may have reached IBKR, or that the log already ends, is
    never settled, and the command changes nothing when refused;
    `test_settle_ended_run_settles_a_never_sent_sale_an_earlier_input_of_the_run_released`:
    every input of the run is covered;
    `test_settle_ended_run_strikes_every_retirement_queued_for_the_ended_run`:
    no queued retirement reaches the fresh run;
    `test_settle_ended_run_repairs_a_torn_last_line`: the file is written
    whole and a torn tail repaired;
    `test_a_settled_order_found_at_ibkr_is_marked_sent_and_its_settlement_withdrawn`:
    marking it sent withdraws its line. `adapter/ibkr/tests/test_relay.py`,
    `test_a_never_sent_order_ibkr_shows_open_or_executed_stops_for_a_person`
    and `test_a_retired_order_ibkr_shows_is_marked_at_broker_and_its_queued_retirement_struck_off`:
    the relay records the order `at-broker` before it stops;
    `test_a_damaged_decision_line_stops_the_relay_as_an_invalid_record_naming_it`:
    a damaged decisions line stops the relay naming it.
    `cmd/operator`, `TestTheApprovalReadsTheAdaptersSettlementOfANeverSentSale`,
    and `internal/restart`,
    `TestASaleTheAdapterNeverSentIsNotAttemptedOnceSettled`,
    `TestASaleTheAdapterSentStaysAttempted`,
    `TestASettlementThatDoesNotMatchTheLogIsRefused` and
    `TestTheEngineChecksTheApprovalsSettlements`: the approval states a
    settled sale not attempted, a sale that was sent stays attempted, and a
    settlement the log does not bear out is refused, by the operator tool
    and the engine alike; `cmd/engine`,
    `TestAFreshRunApprovedAfterASettledNeverSentSaleSellsUnderTheNextAttemptsID`:
    the fresh run sells under the next attempt's client order ID.
  - `test_a_changed_retry_under_a_pending_id_is_refused_and_the_original_settled`:
    a changed copy under the waiting input's ID is refused; the original's
    reply is asked for and its order sent once.

## What can go wrong, and what the program does then

The adapter's pending file, hand-over and outbox are in the [glossary](glossary.md).

| What you see | What the program does | What you do |
| --- | --- | --- |
| The adapter stops with "was being handed to the broker when the adapter stopped" | The adapter stopped while sending that order, and IBKR shows it neither open nor executed (the order may have been taken and cancelled, or never arrived). It will not guess: it does not send that order again, or anything else, until you settle it. | Look in IBKR for an order whose `orderRef` is the client order ID named. Do not place it by hand until you know. Leave the adapter stopped (the command refuses with "stop it first" while the adapter runs), and settle the order with the adapter's resolve command; never edit the `.handover` file by hand (the adapter refuses any value in it it does not know and stops again). From the repository: `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "what IBKR showed"`, or `--resend` in place of `--sent`. `PENDING_FILE` is the adapter's pending file (not the `.handover` file next to it). Choose: **`--sent`** if IBKR shows the order at all (working, filled or cancelled): it is never sent again. **`--resend`** only if the order was committed in today's session and IBKR has no trace of it today (open orders, cancelled orders and today's trades): the adapter sends it again at its **original limit**, whatever the price has done since. Start the adapter the same day: the permission is dated, and an adapter started on a later day does not send the order but stops with "was allowed to be sent again on", and the order needs settling again, by `--sent` after checking IBKR's history for every day since. If, after choosing `--resend`, you find the order at IBKR after all (for example the audit trail shows it accepted and then cancelled), withdraw the permission before starting the adapter: run the same command with `--sent`. That is the only change the command accepts for an order already allowed to be sent again; a second `--resend` is refused. The command refuses `--resend` for an order from an earlier session, because one that filled or was cancelled on an earlier day no longer shows in today's lists: check IBKR's order and trade history for every day since the order was committed (an Account Management activity statement or the audit trail), then mark it `--sent` and let the next rebalance place a fresh order. The command also refuses a client order ID the record does not hold or does not mark `"handing-over"` or `"resend:"` and a date (an order marked `"sent"` was handed to IBKR and needs no decision, and no decision can undo that), so a typo naming any other order changes nothing; it keeps each decision, its reason, the time (UTC) and the session date it was judged against in the file ending `.handover.decisions`. A third decision, **`--never-sent`**, is only for the order the adapter is refusing before it wrote "about to send" and always will; it is refused for any order the record marks and for any order but the one the adapter recorded it is refusing (the command names that one). An order whose day is over is retired by the adapter itself, and one IBKR can never accept as it stands is retired with `relay.py retire` ([operator](operator.md), "Retiring an order the adapter can never send"), which also tells the engine; `--never-sent` tells the engine nothing, so it is refused for an order IBKR can never accept, whose only settlement is `retire`; keep it for a refusal neither covers. See [paper trading](paper-trading.md), "What can go wrong", for when to use it and the engine-side clearance that follows. A fourth, **`--not-handed-over`**, is only for an order the adapter stopped on because IB Gateway's socket closed **before** placeOrder ("so nothing was handed to IB Gateway"), which it records in `PENDING_FILE.not-handed-over`: after IBKR's order history shows no order under its client order ID, it settles that order as never sent on any day, where `--resend` is accepted only in the order's own session. It is refused for any other order, including one stopped mid-send. See [paper trading](paper-trading.md), the row "IB Gateway closed the connection as an order went out". Leave the pending file alone, and do not edit the engine's log: the log is the record of what the engine committed, and the adapter asks for the waiting input's committed reply again on restart. Then start the adapter. |
| The adapter stops with "was settled as never sent, but IBKR shows" | An order you settled with `--never-sent` has an open order or trade at IBKR carrying its client order ID after all. The adapter will not pass over an order the broker has: it stops and sends nothing more. | Check the order at IBKR. With the adapter stopped, mark it `python3 adapter/ibkr/relay.py resolve PENDING_FILE CLIENT_ORDER_ID --sent --reason "what IBKR showed"` (the only decision accepted for a never-sent order), reconcile, then start the adapter. |
| The adapter stops naming the `.handover.decisions` file with "retirement, with no input, as" and "not a retirement as the relay records one" | A retirement in that file was recorded by an older adapter, with no input named, or edited by hand, and its reason, its detail or who retired it is not one the adapter writes (`session over` by `adapter`, or `owner retired` by the owner named, each with its reason). The adapter would have to tell the engine of that retirement again for the order now waiting, and it never sends the engine a retirement it would refuse, so it stops with nothing sent, queued or marked. | Do not guess. Restore the line from your backup; if no backup holds it, correct it to what the adapter wrote (the reason, the detail and who retired it, as the adapter's earlier stop message or the engine's log of that retirement shows), then start the adapter. If IBKR shows the order under its tag, the adapter instead marks it never sent and stops with "was settled as never sent, but IBKR shows" (the row above): settle it with `--sent`; nothing is told to the engine, and any retirement of that order still waiting in the outbox is removed first, so `--sent` never lets one through. |
| The engine refuses a retirement of an order it has retired already, just after the adapter started again | A known limit. An older adapter queued the retirement under the order's ID alone; it reached the engine, and the adapter stopped before it marked the order. On restart the adapter cannot tell whether that retirement was this order's, so it tells the engine of it again, and the engine, which already ended the order, refuses the second one rather than end an order twice. Nothing is sent to IBKR. | Read the refusal as the [engine](engine.md) chapter's row for an input refused with a message starting `targetweight:` says. Then check two things: the engine's log holds the order's first retirement, and IBKR shows nothing under the order's tag. If both hold, the order is retired once, as it should be. If IBKR shows the order, it is working at IBKR: settle it as the row "was settled as never sent, but IBKR shows" says, and reconcile the account. Do not edit the engine's log or the outbox. |
| The adapter stops naming the `.handover` file with "which is none of", "not an object of marks" or "is not JSON" | The `.handover` file holds something other than the adapter's own marks, most likely after a hand edit. The adapter stops and sends nothing while it is so. | Do not guess which orders it covered. Restore the file to a JSON object from client order ID to `"handing-over"`, `"sent"`, `"never-sent"` or `"resend:"` and a date written exactly `YYYY-MM-DD` as the adapter wrote it (any other date form, such as `20261016`, is refused), then settle each `"handing-over"` order with the resolve command above. |
| The adapter, or the resolve command, stops naming the `.handover.decisions` file with "line N is not JSON" or "is not a decision as relay.py resolve writes one" | The file of your resolve decisions holds a line the adapter did not write: a hand edit, a disk fault, or a last line cut short by an adapter build from before each decision was written whole. N is the line's number in the file, blank lines counted. Each decision is now written whole (a temporary file renamed over the file), so a stop never cuts one short. The adapter reads every decision strictly, at every start, reconnect, resolve and settle-ended-run command, because a later decision can undo an earlier one (`--sent` after `--never-sent`), so it sends nothing while any line is unreadable. | Do not delete or edit lines to make it start. Restore the file from your backup, then start the adapter. If no backup holds it, leave the adapter stopped and check every order the file names at IBKR before deciding anything: a damaged line may be a decision that took effect, so it is never simply removed. |
| The adapter stops at start with "has no occurred_at that can be read" or "has no filled_at that can be read" | A report on an order (a status or a fill) in one of the adapter's own records, the engine's log of a run, the outbox, the file of inputs settled as left by an earlier run, or the pending file, has no time in the form the adapter writes. The adapter reads IBKR's reports from these records to know each order's latest status, and will not take one it did not write as it writes one. The message names the report and where it is (the file, or the run whose log holds it). | Do not edit the file to make it start. The record was changed outside the adapter: restore the file named from your backup, check the order named against IBKR, and start the adapter again. Never edit the engine's log; if the message names it, follow `docs/runbooks/durable-log.md`. |
| The adapter stops at start naming the `.run-ended` file with "with no recorded run", or `resolve --run-ended` refuses with "has no record of the run it was sent to" | A settlement of an input left by an earlier run must name that run, so the adapter knows where its reports fall among the runs and which status of each order is the latest. The adapter records the run an input is sent to (the `.run` file) before it saves the input, so a settlement with no run, or a waiting input with no `.run` record of its own, is not something the adapter wrote: the file was edited, or written by another tool. The adapter will not guess where it belongs; it sends nothing. | Do not edit the file to make it start. Restore the `.run-ended` or `.run` file named (and the pending file beside it) from your backup, check the orders the input names against IBKR, and start the adapter again. If no backup holds the record, leave the adapter stopped and investigate before anything is sent. |
| `resolve --run-ended` refuses naming the outbox file and an entry with "not a retirement as the relay queues one" | Before it settles the waiting input, the command reads every retirement still queued in the adapter's outbox (the file ending `.outbox` beside the pending file) as the adapter writes one: version 1, one of the two reasons with who retired it fitting the reason (`session over` by `adapter`, or `owner retired` by the owner named), its detail, the order's client order ID, the time to the second, and an entry ID of `order-retired:` and that client order ID, or that and an input ID. The entry named is none of that, so it is not something the adapter wrote: a hand edit, another tool, or a disk fault. The command will not guess whose retirement it is, so it refuses the whole settlement: nothing is struck off the outbox, nothing is recorded as settled, and the input still waits. | Do not edit the outbox by hand, and do not delete the entry to make the command pass: the outbox is the adapter's record of what the engine is still to be told. With the adapter stopped, restore the outbox file from your backup, then run `resolve --run-ended` again. If no backup holds it, leave the adapter stopped, check the order the entry names against IBKR and the engine's logs, and investigate before anything is settled or sent. |
| The adapter, or the resolve command, refuses with "is held by another adapter or by relay.py resolve" or "stop it first" | Another process still has the pending file open: an adapter that is still running (perhaps hung) or a resolve command still at work. Only one may edit the hand-over record at a time. | Find that process and stop it, then try again. Never delete the file ending `.lock` beside the pending file: it stays on disk after every stop by design and is not a stale lock. Deleting it while a process still holds it lets a second one lock a new file, and two of them then edit the record at once. |
| The adapter stops on IBKR's correction of a trade it recorded (two execution IDs that differ only at the end) | It records no second fill and stops at every start until you acknowledge the correction. | Check IBKR's trades, then, with the adapter stopped: `python3 adapter/ibkr/relay.py acknowledge-correction PENDING_FILE --journal ENGINE_LOG EXEC_ID --corrects ORIGINAL_EXEC_ID --reason "what IBKR shows"`. `ENGINE_LOG` is the engine's `-log` file. The full row is in [paper trading](paper-trading.md), "What can go wrong". |
| The adapter stops on a trade IBKR reported after the order was recorded cancelled or refused ("after the order was recorded as") | The engine reads nothing on an order that has ended, so it cannot record the trade as a fill. The adapter stops at every start until you acknowledge it. | Check IBKR, then, with the adapter stopped: `python3 adapter/ibkr/relay.py acknowledge-late-fill PENDING_FILE --journal ENGINE_LOG EXEC_ID --after CLIENT_ORDER_ID --reason "what IBKR shows"`, and reconcile the account by hand. The full row is in [paper trading](paper-trading.md), "What can go wrong". |
| The adapter reports "retried with other content" | The adapter was given a copy of its waiting input with something changed. It finished the original first (asked for its reply and sent its orders once) and refused the changed copy; the original on disk is untouched. | A fault in the adapter. Find why the input changed before starting again. |
| The adapter reports "committed with different content" | The adapter sent a different input under an ID the engine had already used. The engine refused it and recorded nothing. That is a fault in the adapter, not the market. | Stop the adapter. Find why the input changed before starting again. |
| An urgent alert that an order was not acknowledged (see [order lifecycle](order-lifecycle.md)) | The reporter alerts once: the order is on disk and IBKR has not reported on it. Re-delivery is what resends an order the adapter never received. | Follow the alert's text. Look in IBKR for an order whose `orderRef` is the alert's client order ID **before** anything is placed by hand. |
| The engine was resumed and the adapter has no pending file | The adapter received its last reply before the crash and acted on it, so it has nothing to ask for. | Nothing: the adapter sends its next input as usual. |
| The adapter stops at start with "awaits its reply from an earlier engine run" | The input it was waiting on was sent to an earlier run, and the engine now running is a fresh run. Re-delivery only ever asks the same run again (a crash is resumed); a fresh run never takes the earlier run's input and never sends the orders the earlier run decided. | Follow the row with the same words in [paper trading](paper-trading.md), "What can go wrong": resume the crashed run if that was meant, or check IBKR and settle the input with `relay.py resolve --run-ended`. |
| The engine refuses to resume | The log could not be resumed (see [restart](restart.md)), so the engine does not start the run. No reply can be re-delivered from a run that did not resume. | Follow `docs/runbooks/durable-log.md`. A fresh run starts from a reconciled baseline, so check IBKR's open orders first. |

What this does **not** do yet: sending is built for paper only, and has been
tested against a stand-in for IB Gateway, not yet against IBKR's paper
account by hand (see [paper trading](paper-trading.md)). Live stays
switched off (AGENTS.md rule 5).
