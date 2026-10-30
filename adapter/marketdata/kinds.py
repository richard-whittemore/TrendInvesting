"""The inputs the data job writes: each event type with its schema version, and the source it names.

One table, read by the encoder (encode.py) that writes the stream and by
the IBKR adapter that delivers it (adapter/ibkr/delivery.py, which loads
this file by its path, installed beside it by provision.sh adapter), so
the adapter refuses any type, version or source the data job does not
write. The versions are Go's event constants (internal/event), ADR 0015.
No imports: the adapter loads it on its own.
"""
SOURCE = "marketdata"
CALENDAR = ("market.session.calendar", 1)     # SessionCalendarSchemaVersion
UNIVERSE = ("market.universe.snapshot", 1)    # UniverseSnapshotSchemaVersion
BAR = ("market.bar.completed", 2)             # CompletedBarAdjustedVersion
SESSION_CLOSED = ("market.session.closed", 1)  # SessionClosedSchemaVersion
HISTORY = ("market.close.history", 1)          # CloseHistorySchemaVersion
RETRACTED = ("market.session.retracted", 1)    # SessionRetractedSchemaVersion

# Every (type, schema version) the data job writes.
KINDS = (CALENDAR, UNIVERSE, BAR, SESSION_CLOSED, HISTORY, RETRACTED)

# The inputs the IBKR adapter itself delivers into the stream, from one
# account read per batch (adapter/ibkr/snapshot.py; ADR 0025; the owner's
# decision of 2026-10-18): the engine's account snapshot, the broker snapshot
# (version 1 in a log written before version 2) and each mark. The data job
# never writes them, so they are not in KINDS and the adapter's stream check
# still refuses one in a file.
ACCOUNT_SNAPSHOT = ("account.snapshot", 3)            # AccountSnapshotPositionsVersion
BROKER_SNAPSHOT = ("account.broker.snapshot", 2)      # BrokerSnapshotReadAtVersion
BROKER_SNAPSHOT_V1 = ("account.broker.snapshot", 1)   # BrokerSnapshotSchemaVersion
MARK = ("market.mark.recorded", 2)                    # MarkRecordedEvidenceVersion
# An IRA's adapter (config account_snapshot_version 4, resolve_contracts):
# the account snapshot with its cash, and each mark with its IBKR contract.
ACCOUNT_SNAPSHOT_CASH = ("account.snapshot", 4)       # AccountSnapshotCashVersion
MARK_CONTRACT = ("market.mark.recorded", 3)           # MarkRecordedContractVersion
MARK_UNAVAILABLE = ("market.mark.unavailable", 1)     # MarkUnavailableSchemaVersion
# Fractional capital-path inputs remain delivered history (ADR 0015; ADR 0026 D7-F).
ACCOUNT_SNAPSHOT_FRACTIONAL_CASH = ("account.snapshot", 6)
BROKER_SNAPSHOT_FRACTIONAL = ("account.broker.snapshot", 3)
ADAPTER_KINDS = (ACCOUNT_SNAPSHOT, BROKER_SNAPSHOT, BROKER_SNAPSHOT_V1, MARK, ACCOUNT_SNAPSHOT_CASH, MARK_CONTRACT,
                 MARK_UNAVAILABLE, ACCOUNT_SNAPSHOT_FRACTIONAL_CASH, BROKER_SNAPSHOT_FRACTIONAL)

# The account snapshots that state an account's positions, which the data
# job reads an IRA's holdings from (tidata.holdings): version 3, the one the
# adapter writes for Boost, and version 4, version 3 with the cash an IRA's
# buys are budgeted from, the one it writes for the momentum engine
# (AccountSnapshotCashVersion; ADR 0026 point 2). A snapshot of any other
# version is refused there, never read as holding nothing (ADR 0015).
POSITIONS_SNAPSHOTS = (ACCOUNT_SNAPSHOT, ACCOUNT_SNAPSHOT_CASH)
# A target-weight account's fill (FillTargetWeightVersion), whose instrument
# a holdings reading adds; version 4 is the Turtle reducer's.
TARGET_WEIGHT_FILL = ("execution.fill", 5)
# The owner's Run approval: versions 1 and 2 state no starting book, 3 to 6
# may (OperatorRunApprovedSchemaVersion; a later version is refused until
# this table names it).
RUN_APPROVED_VERSIONS = (1, 2, 3, 4, 5, 6)
RUN_APPROVED_BOOK_FROM = 3

# Every input a run's stream delivers: the data job's KINDS plus the adapter's
# own. A log holding any of these holds copied history, which a fresh run's
# log cannot (ADR 0027); both readers of the log (the adapter's and the data
# job's) count this one table, so they cannot diverge. Operator, approval,
# configuration and reconciliation inputs are not in it.
DELIVERED_KINDS = KINDS + ADAPTER_KINDS
