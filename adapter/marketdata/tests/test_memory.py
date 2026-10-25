"""Memory budget of the data job: a recorded session holds every instrument
Massive returns (thousands), but Boost's stream needs only its configured
instrument, so a warm-up or a daily firing keeps one recording in memory at a
time and the configured instrument's bars from the rest. Synthetic data only;
no network."""
import tempfile
import tracemalloc
import unittest

from marketdata import nyse_calendar, record
from marketdata.tests.test_tidata import BASE, RUN, Job, after_settle, spy

SESSIONS = 60        # sessions recorded
INSTRUMENTS = 3000   # instruments in each recorded session (the real day holds ~10,000)
# A parsed recording of 3,000 instruments is about 2 MB, so the whole history
# held at once is ~120 MB. One recording at a time plus the configured
# instrument's history fits well inside this budget (bytes, tracemalloc peak).
BUDGET = 16 * 1024 * 1024
PAIRED_BUDGET = 16 * 1024 * 1024


def record_history(data, days, instruments=INSTRUMENTS, kinds=("grouped",)):
    for day in days:
        bars = [spy(day)] + [{"T": "X{:05d}".format(n), "o": 10.0 + n, "h": 11.0 + n, "l": 9.0 + n,
                              "c": 10.5 + n, "v": 1000 + n, "vw": 10.4 + n, "n": 50, "t": 0}
                             for n in range(instruments)]
        for kind in kinds:
            record.write(str(data), {"kind": kind, "source": "synthetic", "session": day.isoformat(),
                                     "fetched_at": "2026-01-01T00:00:00Z", "adjusted": False, "bars": bars})


def peak_of(job, now, *extra):
    tracemalloc.start()
    try:
        tracemalloc.reset_peak()
        status, _, err = job.main(now, *extra)
        peak = tracemalloc.get_traced_memory()[1]
    finally:
        tracemalloc.stop()
    return status, err, peak


class MemoryBudgetTest(unittest.TestCase):
    def setUp(self):
        self.days = nyse_calendar.sessions(BASE, nyse_calendar.sessions_after(BASE, SESSIONS - 1)[-1])

    def test_the_warm_up_stays_inside_the_budget(self):
        with tempfile.TemporaryDirectory() as d:
            job = Job(d)
            record_history(job.data, self.days)
            status, err, peak = peak_of(job, after_settle(self.days[-2]), "--backfill-from", BASE.isoformat())
            self.assertEqual(status, 0, err)
            self.assertLess(peak, BUDGET, "warm-up peak {:.1f} MB".format(peak / 1e6))

    def test_the_daily_firing_stays_inside_the_budget(self):
        with tempfile.TemporaryDirectory() as d:
            job = Job(d)
            record_history(job.data, self.days)
            self.assertEqual(job.main(after_settle(self.days[-2]), "--backfill-from", BASE.isoformat())[0], 0)
            status, err, peak = peak_of(job, after_settle(self.days[-1]))
            self.assertEqual(status, 0, err)
            self.assertLess(peak, BUDGET, "daily peak {:.1f} MB".format(peak / 1e6))
            self.assertTrue((job.stream_dir() / "boost-{}.jsonl".format(self.days[-1])).exists())

    def test_a_day_with_both_recordings_stays_inside_the_budget(self):
        # Two recordings of one day are held together for the agreement check,
        # so the peak is two days' recordings plus the kept bars, not more.
        with tempfile.TemporaryDirectory() as d:
            job = Job(d)
            record_history(job.data, self.days, kinds=("grouped", "universe"))
            status, err, peak = peak_of(job, after_settle(self.days[-2]), "--backfill-from", BASE.isoformat())
            self.assertEqual(status, 0, err)
            print("paired-recording peak {:.2f} MB".format(peak / 1e6))
            self.assertLess(peak, PAIRED_BUDGET, "paired warm-up peak {:.1f} MB".format(peak / 1e6))

    def test_the_other_instruments_change_no_byte_of_the_stream(self):
        days = self.days[:12]
        streams = []
        for instruments in (INSTRUMENTS, 0):
            with tempfile.TemporaryDirectory() as d:
                job = Job(d)
                record_history(job.data, days, instruments)
                self.assertEqual(job.main(after_settle(days[-1]), "--backfill-from", BASE.isoformat())[0], 0)
                streams.append((job.stream_dir() / "boost-{}.jsonl".format(days[-1])).read_bytes())
        self.assertEqual(streams[0], streams[1])


if __name__ == "__main__":
    unittest.main()
