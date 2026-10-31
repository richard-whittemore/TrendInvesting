"""Owner-edited interruption windows; docs/manual/ibkr-adapter.md, scheduled interruptions."""
import json
import re
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from config import ConfigError

DEFAULT_PATH = "/etc/trend-investing/ibkr-maintenance.json"
DAYS = ("Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun")


class MaintenanceSchedule:
    def __init__(self, windows=()):
        self.windows = tuple(windows)
        self.file_exists = True

    @classmethod
    def load(cls, path=DEFAULT_PATH):
        try:
            with open(path, encoding="utf-8") as stream:
                return cls.from_mapping(json.load(stream, object_pairs_hook=_unique_fields))
        except FileNotFoundError:
            schedule = cls()
            schedule.file_exists = False
            return schedule
        except (OSError, ValueError) as err:
            raise ConfigError("maintenance schedule {}: {}".format(path, err)) from None

    @classmethod
    def from_mapping(cls, value):
        if not isinstance(value, dict) or set(value) != {"windows"} or not isinstance(value["windows"], list):
            raise ConfigError("maintenance schedule must be an object with a windows array")
        windows = []
        for index, row in enumerate(value["windows"]):
            prefix = "maintenance window {}: ".format(index + 1)
            if not isinstance(row, dict) or set(row) != {"days", "start", "end", "zone"}:
                raise ConfigError(prefix + "requires only days, start, end and zone")
            days = row["days"]
            if (not isinstance(days, list) or not days or any(not isinstance(d, str) for d in days)
                    or len(set(days)) != len(days)
                    or not (days == ["daily"] or all(d in DAYS for d in days))):
                raise ConfigError(prefix + "days must be ['daily'] or distinct Mon through Sun names")
            times = []
            for key in ("start", "end"):
                text = row[key]
                # ADR 0025: 24:00 is the next midnight, only as an end.
                if key == "end" and text == "24:00":
                    text = "00:00"
                if not isinstance(text, str) or not re.fullmatch(r"(?:[01][0-9]|2[0-3]):[0-5][0-9]", text):
                    raise ConfigError(prefix + key + " must be HH:MM (00:00 through 23:59; end also allows 24:00)")
                times.append(datetime.strptime(text, "%H:%M").time())
            if times[0] == times[1] and row["end"] != "24:00":
                raise ConfigError(prefix + "start and end must differ")
            try:
                zone = ZoneInfo(row["zone"])
            except (TypeError, ValueError, ZoneInfoNotFoundError):
                raise ConfigError(prefix + "zone must name an installed IANA time zone") from None
            windows.append((set(range(7)) if days == ["daily"] else {DAYS.index(d) for d in days},
                            times[0], times[1], zone))
        schedule = cls(windows)
        # Refuse weekly coverage without a gap: every loss must have a bound
        # (docs/manual/ibkr-adapter.md, scheduled interruptions).
        year = datetime.now(timezone.utc).year
        day = datetime(year, 1, 1, tzinfo=timezone.utc)
        while day.year == year:
            schedule.window_end(day)
            day += timedelta(days=1)
        return schedule

    def window_end(self, now):
        """End of the union containing now, UTC, or None; starts inclusive, ends exclusive.

        Rule: ADR 0025, "Scheduled interruptions and bounded shutdown".
        The caller supplies now as an aware UTC instant. The result is an
        aware UTC instant too, never local clock arithmetic.
        A repeated local boundary uses its earliest start and latest end. A
        nonexistent boundary rolls forward across the DST gap. Compare UTC
        instants, never subtract local datetimes across a clock change.
        """
        intervals = []
        for days, start, end, zone in self.windows:
            today = now.astimezone(zone).date()
            for offset in range(-8, 9):
                day = today + timedelta(days=offset)
                if day.weekday() not in days:
                    continue
                finish = day + timedelta(days=int(end <= start))
                intervals.append((_boundary(day, start, zone, False), _boundary(finish, end, zone, True)))
        merged = []
        for start, end in sorted(intervals):
            if merged and start <= merged[-1][1]:
                merged[-1] = (merged[-1][0], max(end, merged[-1][1]))
            else:
                merged.append((start, end))
        for start, end in merged:
            if start <= now < end:
                if end - start > timedelta(days=8):
                    raise ConfigError("maintenance windows must leave a weekly gap and a finite end")
                return end
        return None


def _boundary(day, clock, zone, ending):
    local = datetime.combine(day, clock)
    choices = [local.replace(tzinfo=zone, fold=fold).astimezone(timezone.utc) for fold in (0, 1)]
    valid = [at for at in choices if at.astimezone(zone).replace(tzinfo=None) == local]
    return (max if ending else min)(valid) if valid else max(choices)


def _unique_fields(pairs):
    fields = {}
    for key, value in pairs:
        if key in fields:
            raise ValueError("duplicate JSON field: " + key)
        fields[key] = value
    return fields
