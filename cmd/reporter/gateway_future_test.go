// Journal lines dated ahead of the firing's clock, as a clock stepped back
// leaves them (docs/manual/server.md, "The Gateway is logged out"; ADR 0019,
// "Alerting"). The invariant: no future-dated line may resolve the Gateway
// incident or delay its detection; a future-dated outage line may keep one
// open. Synthetic journals only; the command stand-ins only read fixtures.
//
// A line's time is one of: past (at or before the firing's clock), within the
// allowance (at most gatewayLineAllowance, two minutes, ahead), or future
// (further ahead). Two minutes is the allowance gateway-start already gives a
// refusal ahead of the clock (server.md, "The retry is bounded"): the firing
// reads its clock once, before its journal reads, so a line written during
// those reads can be dated a little after it. A smaller allowance only defers
// a genuine recovery by one firing; it never fails open. A line within the
// allowance is a past line in every respect except the session notice cursor.
//
// The case table. "Opens" is a firing with no incident; "incident" is one with
// an incident opened two hours earlier, with fresh recovery evidence beside the
// line wherever that shows the line alone keeps it; kept = still open after.
//
// IBC login completed.
// Past: opens nothing; resolves an incident it is newer than.
// Within: as past.
// Future: opens nothing (by journal order it still supersedes earlier IBC
// lines); keeps an incident open whatever other recovery shows; never counts
// as the newest login, so it cannot hide a reset wait.
//
// IBC logout, refusal, retries stopped, IBC start.
// Past: opens (the grace counts from the firing); keeps.
// Within: as past.
// Future: as past. Its time is never read: its journal position makes it IBC's
// newest event, and not opening would delay detection. Never resolves.
//
// Reset wait.
// Past: opens when newer than the newest login; keeps; quiets its own night;
// the logged-out grace counts from 01:00 ET unless a login or a start inside
// the night superseded it.
// Within: as past, but quiets only from its own time (the next firing).
// Future: opens nothing (its time places it, so its place is unknown); keeps
// an incident open; never quiets; cannot promote an earlier login to recovery.
//
// Adapter link loss.
// Past: opens three hours after the invocation's earliest unrestored loss;
// keeps an incident once three hours old (a younger loss does not).
// Within: as past.
// Future: opens nothing; neither starts nor moves the clock, which runs from
// the earliest loss not in the future; keeps an incident open while no
// restoration or start follows it.
//
// Adapter restoration or reconciliation.
// Past: ends the loss, so opens nothing; resolves an incident it is newer than.
// Within: as past.
// Future: ignored: an earlier loss in the invocation stands and opens on its
// own clock; an incident is kept; never recovery evidence.
//
// Existing-session dialog (the one-shot notice, not the incident).
// Past: noticed once; the cursor moves to its time.
// Within: held; noticed at the first firing at or after its time.
// Future: held until its time; the cursor never passes the firing's clock, so
// a later real dialog dated before it is still noticed.
//
// Adapter starts and plain connects are not in the table: only their journal
// order is read, never their time, so their date changes nothing.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const futureResetLine = "host gateway-start[1]: gateway-start: reset wait: login refused at 12:00 ET; waiting"

// futureFiring runs one Gateway firing at at against the stand-in journals and
// returns the incident record it leaves.
func futureFiring(t *testing.T, g standInGateway, path string, at time.Time, senders ...sender) hostGateway {
	t.Helper()
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	err = gatewayStep(path, gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"}), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil },
		"synthetic-host", sched, func() time.Time { return time.Time{} }, "", nil, senders, nil, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	h, err := loadHostGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestGatewayFutureDatedEvidence(t *testing.T) {
	now := tuesday(12, 0)
	type journals struct{ ibc, adapter []string }
	type cell struct {
		name     string
		incident bool // an incident is open before the firing
		lines    func(at time.Time) journals
		offsets  map[string]time.Duration // past, within, future: the line's time from now
		want     map[string]bool          // incident open after the firing
	}
	times := func(past time.Duration) map[string]time.Duration {
		return map[string]time.Duration{"past": past, "within": time.Minute, "future": time.Hour}
	}
	start := stamp(now.Add(-5*time.Hour), adapterStart)
	restored := stamp(now.Add(-30*time.Minute), linkUp)
	restoration := func(label, line string) []cell {
		return []cell{
			{label + "/open", false, func(at time.Time) journals {
				return journals{[]string{stamp(now.Add(-5*time.Hour), ibcCompleted)}, []string{start, stamp(now.Add(-4*time.Hour), linkLost), stamp(at, line)}}
			}, times(-time.Hour), map[string]bool{"past": false, "within": false, "future": true}},
			{label + "/incident", true, func(at time.Time) journals {
				return journals{[]string{stamp(now.Add(-5*time.Hour), ibcCompleted)}, []string{start, stamp(at, line)}}
			}, times(-time.Hour), map[string]bool{"past": false, "within": false, "future": true}},
		}
	}
	cells := []cell{
		{"login/open", false, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-3*time.Hour), ibcRefused), stamp(at, ibcCompleted)}, nil}
		}, times(-time.Hour), map[string]bool{"past": false, "within": false, "future": false}},
		{"login/incident", true, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-3*time.Hour), ibcRefused), stamp(at, ibcCompleted)}, []string{start, restored}}
		}, times(-time.Hour), map[string]bool{"past": false, "within": false, "future": true}},
		{"logout/open", false, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-4*time.Hour), ibcCompleted), stamp(at, ibcRefused)}, nil}
		}, times(-time.Hour), map[string]bool{"past": true, "within": true, "future": true}},
		{"logout/incident", true, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-4*time.Hour), ibcCompleted), stamp(at, ibcRefused)}, []string{start, restored}}
		}, times(-time.Hour), map[string]bool{"past": true, "within": true, "future": true}},
		{"retries-stopped/open", false, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-4*time.Hour), ibcCompleted), stamp(at, ibcStopped)}, nil}
		}, times(-time.Hour), map[string]bool{"past": true, "within": true, "future": true}},
		{"ibc-start/open", false, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-4*time.Hour), ibcCompleted), stamp(at, ibcStarting)}, nil}
		}, times(-time.Hour), map[string]bool{"past": true, "within": true, "future": true}},
		{"reset-wait/open", false, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-4*time.Hour), ibcCompleted), stamp(at, futureResetLine)}, nil}
		}, times(-time.Hour), map[string]bool{"past": true, "within": true, "future": false}},
		{"reset-wait/incident", true, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-4*time.Hour), ibcCompleted), stamp(at, futureResetLine)}, []string{start, restored}}
		}, times(-time.Hour), map[string]bool{"past": true, "within": true, "future": true}},
		{"loss/open", false, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-5*time.Hour), ibcCompleted)}, []string{start, stamp(at, linkLost)}}
		}, times(-3 * time.Hour), map[string]bool{"past": true, "within": false, "future": false}},
		{"loss-then-past-loss/open", false, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-5*time.Hour), ibcCompleted)}, []string{start, stamp(at, linkLost), stamp(now.Add(-3*time.Hour), linkLost)}}
		}, times(-4 * time.Hour), map[string]bool{"past": true, "within": true, "future": true}},
		{"loss/incident", true, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-30*time.Minute), ibcCompleted)}, []string{start, stamp(at, linkLost)}}
		}, times(-3 * time.Hour), map[string]bool{"past": true, "within": false, "future": true}},
		{"reset-wait-after-future-login/open", false, func(at time.Time) journals {
			return journals{[]string{stamp(now.Add(-4*time.Hour), ibcCompleted), stamp(at, ibcCompleted), stamp(now.Add(-time.Hour), futureResetLine)}, nil}
		}, times(-2 * time.Hour), map[string]bool{"past": true, "within": false, "future": true}},
	}
	cells = append(cells, restoration("restoration", linkUp)...)
	cells = append(cells, restoration("reconciliation", linkReconciled)...)
	for _, c := range cells {
		for _, when := range []string{"past", "within", "future"} {
			t.Run(c.name+"/"+when, func(t *testing.T) {
				g := newStandInGateway(t)
				j := c.lines(now.Add(c.offsets[when]))
				g.ibc(j.ibc...)
				g.adapter(j.adapter...)
				path := filepath.Join(t.TempDir(), "host-gateway.json")
				if c.incident {
					raw := fmt.Sprintf(`{"logged_out_since":%q}`, now.Add(-2*time.Hour).Format(time.RFC3339))
					if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				h := futureFiring(t, g, path, now)
				if got := h.Since != nil; got != c.want[when] {
					t.Fatalf("incident open after the firing = %v, want %v", got, c.want[when])
				}
			})
		}
	}
}

// The allowance's edge: a login two minutes ahead of the clock is recovery; one
// a second beyond it is not.
func TestGatewayLineAllowanceEdge(t *testing.T) {
	now := tuesday(12, 0)
	for ahead, kept := range map[time.Duration]bool{2 * time.Minute: false, 2*time.Minute + time.Second: true} {
		t.Run(ahead.String(), func(t *testing.T) {
			g := newStandInGateway(t)
			g.ibc(stamp(now.Add(-3*time.Hour), ibcRefused), stamp(now.Add(ahead), ibcCompleted))
			path := filepath.Join(t.TempDir(), "host-gateway.json")
			raw := fmt.Sprintf(`{"logged_out_since":%q}`, now.Add(-2*time.Hour).Format(time.RFC3339))
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if h := futureFiring(t, g, path, now); (h.Since != nil) != kept {
				t.Fatalf("kept = %v, want %v", h.Since != nil, kept)
			}
		})
	}
}

// A future loss cannot postpone the three-hour clock: with no other loss, the
// clock starts at the first loss that is not in the future once one is written.
func TestGatewayFutureLossDoesNotPostponeDetection(t *testing.T) {
	now := tuesday(12, 0)
	g := newStandInGateway(t)
	g.ibc(stamp(now.Add(-5*time.Hour), ibcCompleted))
	g.adapter(stamp(now.Add(-5*time.Hour), adapterStart), stamp(now.Add(6*time.Hour), linkLost), stamp(now, linkLost))
	check := gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"})
	for at, out := range map[time.Time]bool{now.Add(3*time.Hour - time.Second): false, now.Add(3 * time.Hour): true} {
		ev, err := check(at)
		if err != nil {
			t.Fatal(err)
		}
		if ev.out != out {
			t.Errorf("at %s: out = %v, want %v: %s", at.Format(time.Kitchen), ev.out, out, ev.said)
		}
	}
}

// A reset wait quiets only from its own time, and one dated in the future never
// quiets (docs/manual/server.md, "The Gateway is logged out").
func TestGatewayResetWaitAheadOfTheClockNeverQuiets(t *testing.T) {
	night := time.Date(2024, time.January, 3, 0, 10, 0, 0, newYork)
	for name, tc := range map[string]struct {
		ahead time.Duration
		quiet bool
	}{"past": {-time.Minute, true}, "within": {time.Minute, false}, "future": {30 * time.Minute, false}} {
		t.Run(name, func(t *testing.T) {
			g := newStandInGateway(t)
			g.ibc(stamp(night.Add(-time.Hour), ibcCompleted), stamp(night.Add(tc.ahead), futureResetLine))
			ev, err := gatewayJournalChecker(g.path(), nil)(night)
			if err != nil {
				t.Fatal(err)
			}
			if ev.resetWait != tc.quiet {
				t.Fatalf("reset wait = %v, want %v: %s", ev.resetWait, tc.quiet, ev.said)
			}
		})
	}
}

// The session notice cursor never passes the firing's clock: a dialog dated
// ahead of it is held until its time, and a real dialog written after it but
// dated before it is still noticed (docs/manual/server.md, "When another login
// takes the Gateway's session").
func TestGatewaySessionDialogAheadOfTheClock(t *testing.T) {
	now := tuesday(12, 0)
	notices := func(r *policyRecorder) int {
		n := 0
		for _, a := range r.alerts {
			if a.Kind == "gateway-session" {
				n++
			}
		}
		return n
	}
	for name, tc := range map[string]struct {
		ahead  time.Duration
		firing []time.Duration // firings, from now
		want   []int           // notices after each
	}{
		"past":   {-time.Minute, []time.Duration{0, 5 * time.Minute}, []int{1, 1}},
		"within": {time.Minute, []time.Duration{0, 5 * time.Minute, 10 * time.Minute}, []int{0, 1, 1}},
		"future": {time.Hour, []time.Duration{0, 10 * time.Minute, 15 * time.Minute, time.Hour, time.Hour + 5*time.Minute}, []int{0, 1, 1, 2, 2}},
	} {
		t.Run(name, func(t *testing.T) {
			g := newStandInGateway(t)
			lines := []string{stamp(now.Add(-5*time.Hour), ibcCompleted), stamp(now.Add(tc.ahead), sessionDialog)}
			g.ibc(lines...)
			path := filepath.Join(t.TempDir(), "host-gateway.json")
			r := &policyRecorder{channel: "ntfy"}
			for i, offset := range tc.firing {
				if offset == 10*time.Minute && name == "future" {
					// A real dialog, written after the future one, dated before it.
					g.ibc(append(lines, stamp(now.Add(6*time.Minute), sessionDialog))...)
				}
				at := now.Add(offset)
				h := futureFiring(t, g, path, at, r)
				if got := notices(r); got != tc.want[i] {
					t.Fatalf("firing %d (%s): notices = %d, want %d", i, offset, got, tc.want[i])
				}
				if h.SessionNotifiedThrough != nil && h.SessionNotifiedThrough.After(at) {
					t.Fatalf("firing %d: cursor %s passed the firing's clock %s", i, h.SessionNotifiedThrough, at)
				}
			}
		})
	}
}

// After a reset wait the logged-out grace counts from the reset night's end
// (01:00 ET), so the announced retry has the grace before a page
// (docs/manual/server.md, "The retry is bounded").
func TestGatewayGraceCountsFromTheResetNightsEnd(t *testing.T) {
	night := func(h, m int) time.Time { return time.Date(2024, time.January, 3, h, m, 0, 0, newYork) }
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry_logged=%v", retry), func(t *testing.T) {
			g := newStandInGateway(t)
			lines := []string{stamp(time.Date(2024, time.January, 2, 23, 0, 0, 0, newYork), ibcCompleted), stamp(night(-1, 50), ibcRefused),
				stamp(night(-1, 50), "host gateway-start[1]: gateway-start: reset wait: login refused at 23:50 ET inside IBKR's nightly reset hour; waiting 15m until 00:05 ET")}
			g.ibc(lines...)
			path := filepath.Join(t.TempDir(), "host-gateway.json")
			// The reset wait reaches ntfy only; the logged-out page is Pushover's.
			p, n := &policyRecorder{channel: "pushover"}, &policyRecorder{channel: "ntfy"}
			for m := -10; m <= 70; m += 5 {
				at := night(0, 0).Add(time.Duration(m) * time.Minute)
				if m == 60 && retry {
					g.ibc(append(lines, stamp(at, ibcStarting))...)
				}
				err := gatewayStep(path, gatewayJournalChecker(g.path(), nil), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil },
					"synthetic-host", sched, func() time.Time { return time.Time{} }, "", nil, []sender{p, n}, nil, func() time.Time { return at }, &pagingPolicy{})
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				if m >= 70 {
					want = 1
				}
				if got := len(p.alerts); got != want {
					t.Fatalf("at %s: pages = %d, want %d: %v", at.Format("15:04"), got, want, p.alerts)
				}
			}
		})
	}
}

// A start dated ahead of the clock after a reset wait still supersedes the
// reset night: ordinary logout handling resumes and the grace is not held to
// the night's 01:00 end (docs/manual/server.md, "The Gateway is logged out").
func TestGatewayFutureStartAfterResetWaitResumesOrdinaryHandling(t *testing.T) {
	night := func(h, m int) time.Time { return time.Date(2024, time.January, 3, h, m, 0, 0, newYork) }
	g := newStandInGateway(t)
	g.ibc(stamp(time.Date(2024, time.January, 2, 23, 0, 0, 0, newYork), ibcCompleted), stamp(night(-1, 50), ibcRefused),
		stamp(night(-1, 50), "host gateway-start[1]: gateway-start: reset wait: login refused at 23:50 ET inside IBKR's nightly reset hour; waiting 15m until 00:05 ET"),
		stamp(night(3, 0), ibcStarting))
	path := filepath.Join(t.TempDir(), "host-gateway.json")
	raw := fmt.Sprintf(`{"logged_out_since":%q}`, night(-1, 50).Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	sched, err := gatewayScheduleFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	p := &policyRecorder{channel: "pushover"}
	var first time.Time
	for m := 40; m <= 70; m += 5 {
		at := night(0, m)
		if err := gatewayStep(path, gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"}), func() (gatewayUnitSample, error) { return gatewayUnitSample{active: true}, nil },
			"synthetic-host", sched, func() time.Time { return time.Time{} }, "", nil, []sender{p}, nil, func() time.Time { return at }, &pagingPolicy{}); err != nil {
			t.Fatal(err)
		}
		if first.IsZero() && len(p.alerts) > 0 {
			first = at
		}
	}
	if !first.Equal(night(0, 40)) {
		t.Errorf("first page at %s, want 00:40: the start supersedes the reset night", first.Format("15:04"))
	}
}
