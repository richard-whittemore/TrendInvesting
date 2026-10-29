#!/usr/bin/env python3
"""Append memory evidence and report a UTC week (docs/live-trading/lessons-2026-10.md L1; ADR 0026).

No account identifiers or credentials are read. Server use is MemTotal minus
MemAvailable, never a sum of unit peaks. MemoryPeak is the cgroup's lifetime
high-water mark; sampled_peak_bytes bounds what this week actually observed.
"""
import argparse
from collections import Counter
from datetime import datetime, timedelta, timezone
import fcntl
import json
import os
from pathlib import Path
import statistics
import subprocess
import sys

ACCOUNTS = ("boost", "trad-ira", "roth-ira")
SERVICES = {"ti-gateway.service", "ti-dashboard.service", "ti-data.service", "ti-adapter-boost.service"}
SERVICES |= {f"ti-{kind}@{account}.service" for account in ACCOUNTS
             for kind in ("engine", "flex", "reporter")}
SERVICES |= {f"ti-{kind}@{account}.service" for account in ACCOUNTS[1:] for kind in ("adapter", "data")}
TIMERS = {"ti-data.timer", "ti-memory.timer"}
TIMERS |= {f"ti-{kind}@{account}.timer" for account in ACCOUNTS for kind in ("flex", "reporter")}
TIMERS |= {f"ti-data@{account}.timer" for account in ACCOUNTS[1:]}


def week_bounds(week_start):
    start = datetime.strptime(week_start, "%Y-%m-%d").replace(tzinfo=timezone.utc)
    if start.strftime("%Y-%m-%d") != week_start:
        raise ValueError("week-start must be YYYY-MM-DD")
    return start, start + timedelta(days=7)


def run_systemctl(*args):
    return subprocess.run(["systemctl", *args], check=True, capture_output=True,
                          text=True, timeout=20).stdout


def counter(value):
    # systemd reports unavailable counters as [not set] or UINT64_MAX.
    return int(value) if value.isdecimal() and int(value) < 2**64 - 1 else None


def sample(record, week_start, *, now=None, systemctl=run_systemctl, meminfo=Path("/proc/meminfo")):
    """Append one locked observation; outside the week append nothing.

    lessons-2026-10.md L1 and ADR 0026 require measured server evidence.
    Injected now must be timezone-aware; active units require both counters.
    Each appended schema-version-1 row includes the units and timer text.
    """
    now = now or datetime.now(timezone.utc)
    start, end = week_bounds(week_start)
    if not start <= now < end:
        return False
    units = set(SERVICES)
    units.update(line.split()[0] for line in systemctl("list-units", "--all", "--plain", "--no-legend",
                                                     "--type=service", "ti-*").splitlines() if line.strip())
    readings = {}
    for unit in sorted(units):
        props = dict(line.split("=", 1) for line in
                     systemctl("show", unit, "--property=LoadState,ActiveState,MemoryCurrent,MemoryPeak").splitlines()
                     if "=" in line)
        state = props.get("ActiveState", "unknown")
        current, peak = counter(props.get("MemoryCurrent", "")), counter(props.get("MemoryPeak", ""))
        if state in ("active", "activating", "reloading") and (current is None or peak is None):
            raise ValueError(f"{unit}: memory counters unavailable; enable MemoryAccounting and check systemd")
        readings[unit] = dict(current_bytes=current, peak_bytes=peak, active_state=state,
                              load_state=props.get("LoadState", "unknown"))
    timers = set(TIMERS)
    timers.update(line.split()[0] for line in systemctl("list-unit-files", "--no-legend", "--type=timer",
                                                      "ti-*").splitlines()
                  if line.strip() and "@.timer" not in line.split()[0])
    # systemctl cat includes the installed instance drop-ins, including the
    # empty OnCalendar reset. The record preserves every schedule change.
    schedule = {unit: systemctl("cat", unit) for unit in sorted(timers)}
    info = {}
    for line in Path(meminfo).read_text().splitlines():
        parts = line.split()
        if parts and parts[0] in ("MemTotal:", "MemAvailable:"):
            if len(parts) != 3 or parts[2] != "kB":
                raise ValueError("/proc/meminfo: expected kB")
            info[parts[0][:-1]] = int(parts[1]) * 1024
    total, available = info["MemTotal"], info["MemAvailable"]
    if not 0 <= available <= total or total <= 0:
        raise ValueError("/proc/meminfo: invalid total or available memory")
    row = dict(schema_version=1, week_start=week_start, at=now.isoformat(), schedule=schedule,
               units=readings, server=dict(total_bytes=total, available_bytes=available, used_bytes=total-available))
    # Append only, never truncate an earlier experiment's evidence (AGENTS.md).
    with Path(record).open("a", encoding="utf-8") as stream:
        fcntl.flock(stream, fcntl.LOCK_EX)
        stream.write(json.dumps(row, sort_keys=True, allow_nan=False) + "\n")
        stream.flush()
        os.fsync(stream.fileno())
    return True


def valid_bytes(value):
    return type(value) is int and value >= 0


def stats(values):
    return dict(peak_bytes=max(values) if values else None,
                median_bytes=statistics.median(values) if values else None, samples=len(values))


def report(record, week_start):
    """Report a UTC week, refusing a missing day, corrupt row or duplicate time.

    lessons-2026-10.md L1 and ADR 0026 require measured server evidence.
    The caller supplies the sampler's schema-version-1 JSONL for this week
    and disables the timer before reporting, retaining the original evidence.
    """
    start, end = week_bounds(week_start)
    rows, days, schedules, seen = [], Counter(), [], set()
    with Path(record).open(encoding="utf-8") as stream:
        fcntl.flock(stream, fcntl.LOCK_SH)
        for number, line in enumerate(stream, 1):
            try:
                row = json.loads(line)
                if row["schema_version"] != 1 or row["week_start"] != week_start:
                    raise ValueError("schema or week differs")
                at = datetime.fromisoformat(row["at"])
                if at.tzinfo is None or not start <= at < end:
                    raise ValueError("timestamp outside the UTC week")
                at = at.astimezone(timezone.utc)
                if at in seen:
                    raise ValueError("duplicate observation timestamp")
                seen.add(at)
                server = row["server"]
                if (not all(valid_bytes(server[k]) for k in ("total_bytes", "available_bytes", "used_bytes"))
                        or server["total_bytes"] <= 0
                        or server["used_bytes"] != server["total_bytes"] - server["available_bytes"]):
                    raise ValueError("invalid server counters")
                if not row["units"] or not isinstance(row["schedule"], dict) or not row["schedule"]:
                    raise ValueError("units or schedule missing")
                for unit in row["units"].values():
                    for key in ("current_bytes", "peak_bytes"):
                        if unit[key] is not None and not valid_bytes(unit[key]):
                            raise ValueError("invalid unit counter")
                rows.append((at, row))
                days[at.date().isoformat()] += 1
            except (ValueError, KeyError, TypeError, AttributeError) as err:
                raise ValueError(f"invalid record line {number}: {err}") from None
    missing = [(start + timedelta(days=i)).date().isoformat() for i in range(7)
               if not days[(start + timedelta(days=i)).date().isoformat()]]
    if missing:
        raise ValueError("; ".join(f"missing samples for {day}" for day in missing))
    rows.sort(key=lambda item: item[0])
    for at, row in rows:
        if not schedules or row["schedule"] != schedules[-1]["schedule"]:
            schedules.append(dict(since=at.isoformat(), schedule=row["schedule"], samples=0))
        schedules[-1]["samples"] += 1
    units = {}
    for name in sorted({name for _, row in rows for name in row["units"]}):
        observations = [row["units"][name] for _, row in rows if name in row["units"]]
        current = [u["current_bytes"] for u in observations if u["current_bytes"] is not None]
        peaks = [u["peak_bytes"] for u in observations if u["peak_bytes"] is not None]
        units[name] = stats(current)
        units[name]["sampled_peak_bytes"] = units[name]["peak_bytes"]
        units[name]["peak_bytes"] = max(current + peaks) if current or peaks else None
        units[name]["states"] = dict(Counter(u["active_state"] for u in observations))
    gaps = [(b[0] - a[0]).total_seconds() for a, b in zip(rows, rows[1:])]
    return dict(schema_version=1, week_start=week_start, week_end_exclusive=end.date().isoformat(),
                timezone="UTC", samples_per_day=dict(sorted(days.items())), expected_samples_per_day=288,
                first_sample=rows[0][0].isoformat(), last_sample=rows[-1][0].isoformat(),
                max_gap_seconds=max(gaps, default=0), schedules=schedules, units=units,
                server=stats([row["server"]["used_bytes"] for _, row in rows]),
                capacity_bytes=sorted({row["server"]["total_bytes"] for _, row in rows}),
                peak_note="Unit peak_bytes includes cgroup lifetime MemoryPeak, possibly before this week; "
                          "sampled_peak_bytes and server peak_bytes are five-minute observations. "
                          "Inactive/unavailable counters are null, never invented zeros.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("sample", "report"))
    parser.add_argument("--record", type=Path, required=True)
    parser.add_argument("--week-start", required=True, help="first of seven UTC dates, YYYY-MM-DD")
    args = parser.parse_args()
    try:
        if args.mode == "sample":
            sample(args.record, args.week_start)
        else:
            print(json.dumps(report(args.record, args.week_start), indent=2, sort_keys=True, allow_nan=False))
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as err:
        print(f"memory: FAIL: {err}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
