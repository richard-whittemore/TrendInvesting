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
