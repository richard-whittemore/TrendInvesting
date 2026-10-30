# IRA drill checklist

Gate status: PENDING

Owner decisions: plan.md §9 Phase 3 (2026-10-28), paper sequencing and
No Borrow Margin/fractional shares (2026-10-29); ADR 0026 points 1, 2 and 5;
ADR 0027 restart model. Use one copy per IRA installation, named
`ira-drill-checklist-trad-ira.md` or `ira-drill-checklist-roth-ira.md` in this
same directory. `go test ./internal/manualaudit/` audits the template and every
`ira-drill-checklist*.md` copy there. Nothing here authorizes sending, including to paper.

Every step is mandatory. Fill its blank row for the first attempt with an ISO
date (`YYYY-MM-DD`), PASS or FAIL, and an outcome/evidence reference. For each
retry, keep the earlier row and add a new dated row with the same Step id below
it, in chronological order; attempts on the same date retain their row order.
Every recorded attempt needs a valid date, outcome and evidence, including
failed attempts. The latest attempt for each step determines its status: a
blank or latest FAIL keeps the gate pending, while a later PASS resolves an
earlier FAIL without erasing it. Only Richard signs the last row and changes
Gate status to PASS after every step's latest attempt passes. The latest
owner-signoff date must be on or after every other step's latest attempt
(CONTEXT.md: "IRA dry-run gate"). Use exactly five cells enclosed by leading
and trailing pipes for every attempt; do not put pipes in evidence text, even
escaped ones. Malformed attempt rows fail the audit.
`go test ./internal/manualaudit/` rejects a passed gate with missing, undated
or latest-failed steps, or a sign-off that predates a later attempt. Evidence paths must be private; record
logical instance names only, never account numbers, credentials or screenshots
with identifiers. Mask `DU` plus **letters and digits** (lesson L12).

| Step | Owner action and expected result | Date | Outcome | Evidence / observation |
| --- | --- | --- | --- | --- |
| paper-free | Confirm the paper account is free for the IRA (Boost live or paused); linked accounts have no paper twin. | | | |
| no-borrow | Confirm the real IRA is No Borrow Margin in Client Portal. | | | |
| fractional | Confirm fractional trading enabled on the real IRA; confirm M6b adapter/reporting readiness before paper. | | | |
| market-data | Confirm the username's IBKR market-data subscriptions and permissions cover every IRA stock, SPY, IEF and SHY; fresh recorded marks arrive. | | | |
| same-day | Run the installed IRA's same-day probe before leaving the server; record its PASS (lesson L10). | | | |
| gateway-restart | Restart Gateway and confirm login and the adapter recover; if a login dialog stalls, follow gateway-login.md (lesson L4). | | | |
| logged-out-page | During Gateway logout confirm the urgent logged-out alert arrives on both configured channels; acknowledge only after recording it (L4). | | | |
| withheld-page | Rehearse a withheld IRA decision and confirm its urgent rebalance page and reason on both channels; quiet success is a failure (L11). | | | |
| restart-hold | Run missed-rebalance.md's fake-venue restart drill: sells working, buys absent, deliberate stop, fresh approval, restart; observe rebalance-in-progress and no released orders. | | | |
| clearance | Richard confirms fake orders cancelled, records operator rebalance-cleared and observes it delivered without a refusal; January's abandoned buys stay absent. | | | |
| missed-recovery | Next synthetic month decides afresh; targets, orders and terminal state equal the on-time control; unchanged holdings/cash after fake cancellations. | | | |
| real-month | Gate half 1: one real month-end decided cleanly in dry-run, correct inputs, no withheld picks/refusals, no send=true; Richard reads the decision and summary. | | | |
| rehearsed-month | Gate half 2: one recorded month replayed through the installed server engine with -as-of; decision clean, same intended decisions as the recorded dry-run over identical inputs; private evidence retained. | | | |
| owner-signoff | Richard dates/signs this checklist only after both gate halves and every drill pass. Paper remains a separate gate; no order was sent. | | | |

Commands and expected lines: [Rehearsing a month and restart drill](missed-rebalance.md#rehearsing-a-month).
Gateway/login and alert checks: [Gateway login](gateway-login.md),
[alerts](../manual/alerts-and-dashboard.md). Same-day probe:
[first dry-run day, Traditional IRA](first-dry-run-day.md#the-traditional-ira-its-engine-warm-up-adapter-and-same-day-probe).
If a check fails, leave the gate pending, retain the failing evidence and record
remediation and a new dated row with the same Step id; never erase the failed
result (AGENTS.md).
