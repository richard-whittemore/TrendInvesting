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
  already, and IBKR does not already hold an order with that order's tag
  (its client order ID, which IBKR calls `orderRef`).

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
  twice; it skips an order IBKR already holds; after its own restart it asks
  for the pending input before the next one; it never sends a dry-run order.

## What can go wrong

| What you see | What it means | What to do |
| --- | --- | --- |
| The adapter reports "committed with different content" | It sent a different input under an ID it had already used. That is a fault in the adapter, not the market. | Stop the adapter. Nothing was recorded for the refused input. Find why the input changed before starting again. |
| An "order not acknowledged" alert (see [order lifecycle](order-lifecycle.md)) | The order is on disk and IBKR has not reported on it. | Follow the alert's text. Look in IBKR for an order whose `orderRef` is the alert's client order ID **before** anything is placed by hand. |
| The engine was resumed and the adapter has no pending file | The adapter received its last reply before the crash and acted on it. | Nothing: the adapter sends its next input as usual. |
| The engine refuses to resume | The log could not be resumed (see [restart](restart.md)). No reply can be re-delivered from a run that did not resume. | Follow `docs/runbooks/durable-log.md`. A fresh run starts from a reconciled baseline, so check IBKR's open orders first. |

What this does **not** do yet: the IBKR adapter does not speak the engine's
socket yet, and sending to IBKR is still switched off for paper and live
(AGENTS.md rule 5). The relay is ready for both and is tested against fakes.
