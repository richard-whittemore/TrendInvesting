package report

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestPagingSessionNear(t *testing.T) {
	for _, tc := range []struct {
		at   string
		want bool
	}{
		{"2026-10-30T05:29:59-04:00", false}, {"2026-10-30T05:30:00-04:00", true},
		{"2026-10-30T09:30:00-04:00", true}, {"2026-10-30T15:59:59-04:00", true}, {"2026-10-30T16:00:00-04:00", false},
		{"2026-10-31T08:00:00-04:00", false}, {"2026-11-27T15:00:00-05:00", true}, {"2026-12-25T08:00:00-05:00", true}, {"2026-12-25T10:00:00-05:00", false},
	} {
		t.Run(tc.at, func(t *testing.T) {
			at, _ := time.Parse(time.RFC3339, tc.at)
			got, err := SessionNear(at)
			if err != nil || got != tc.want {
				t.Fatalf("got %v %v want %v", got, err, tc.want)
			}
		})
	}
}

func TestPagingClasses(t *testing.T) {
	for _, kind := range []Kind{KindDegraded, KindHalted, KindReconciliation, KindUnacknowledged, KindRefusal, KindSaleByHand, KindRebalanceWithheld, KindPickWithheld, KindHeartbeat, KindMissingData} {
		if got := (Alert{Kind: kind}).Class(false); got != CapitalSafety {
			t.Errorf("%s class=%s", kind, got)
		}
	}
	for _, unit := range []string{"ti-engine@sample.service", "ti-adapter@sample.service", "ti-gateway.service", "ti-data.service", "ti-flex@sample.service", "ti-memory.service"} {
		a := UnitFailedAlert("host", unit, "synthetic", "failed", time.Time{})
		if a.Class(false) != Operational {
			t.Errorf("%s outside near", unit)
		}
		want := Operational
		if unit == "ti-engine@sample.service" || unit == "ti-adapter@sample.service" || unit == "ti-gateway.service" {
			want = CapitalSafety
		}
		if a.Class(true) != want {
			t.Errorf("%s near class=%s", unit, a.Class(true))
		}
	}
	if (Alert{Kind: "future-unknown"}).Class(false) != CapitalSafety {
		t.Fatal("unknown class must fail safe")
	}
}

func TestIBKRSchedule(t *testing.T) {
	raw := []byte(`{"windows":[{"days":["daily"],"start":"23:45","end":"01:00","zone":"America/New_York"},{"days":["Sat"],"start":"00:00","end":"12:00","zone":"America/New_York"}]}`)
	s, err := ParseIBKRSchedule(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ at, end string }{
		{"2026-10-30T23:44:59-04:00", ""}, {"2026-10-30T23:45:00-04:00", "2026-10-31T12:00:00-04:00"},
		{"2026-10-31T12:00:00-04:00", ""}, {"2026-10-31T23:45:00-04:00", "2026-11-01T01:00:00-05:00"},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		got := s.WindowEnd(at)
		want, _ := time.Parse(time.RFC3339, tc.end)
		if !got.Equal(want) {
			t.Errorf("%s end=%s want=%s", tc.at, got, want)
		}
	}
}
func TestIBKRScheduleRejectsInvalid(t *testing.T) {
	for _, raw := range []string{`{}`, `{"windows":null}`, `{"windows":[],"extra":1}`, `{"windows":[],"windows":[]}`, `{"windows":[{"days":["daily"],"start":"00:00","end":"24:00","zone":"UTC"}]}`, `{"windows":[{"days":["Sat","Sat"],"start":"00:00","end":"12:00","zone":"UTC"}]}`} {
		if _, err := ParseIBKRSchedule([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if _, err := ParseIBKRSchedule([]byte(`{"windows":[{"days":["Sat"],"start":"00:00","end":"24:00","zone":"UTC"}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestIBKRScheduleSchemaAndDSTGap(t *testing.T) {
	for _, raw := range []string{
		``, `{"windows":[`, `{"windows":[], 3:true}`, `{"windows":[]} true`, `{"windows":[]} garbage`,
		`{"windows":[{"days":[],"start":"00:00","end":"12:00","zone":"UTC"}]}`,
		`{"windows":[{"days":["daily","Mon"],"start":"00:00","end":"12:00","zone":"UTC"}]}`,
		`{"windows":[{"days":["Sat"],"start":"24:00","end":"12:00","zone":"UTC"}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","end":"25:00","zone":"UTC"}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","end":"00:00","zone":"UTC"}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","zone":""}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","zone":"Local"}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","zone":"Synthetic/Unknown"}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","zone":"UTC","zone":"UTC"}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","zone":"UTC","extra":true}]}`,
	} {
		if _, err := ParseIBKRSchedule([]byte(raw)); err == nil {
			t.Errorf("accepted malformed schedule: %s", raw)
		}
	}
	s, err := ParseIBKRSchedule([]byte(`{"windows":[{"days":["Sun"],"start":"02:30","end":"04:00","zone":"America/New_York"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := time.Parse(time.RFC3339, "2026-03-08T03:29:00-04:00")
	at, _ := time.Parse(time.RFC3339, "2026-03-08T03:30:00-04:00")
	end, _ := time.Parse(time.RFC3339, "2026-03-08T04:00:00-04:00")
	if !s.WindowEnd(before).IsZero() || !s.WindowEnd(at).Equal(end) {
		t.Fatal("nonexistent start must roll forward over DST gap")
	}
	if got := s.ClosedSince(before, end); !got.Equal(end) {
		t.Fatalf("closed end=%s", got)
	}
	if got := s.ClosedSince(end, end); !got.IsZero() {
		t.Fatalf("end already counted: %s", got)
	}
	if _, err := SessionNear(time.Date(2099, 1, 5, 15, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("unknown calendar year treated as safe")
	}
}

func TestPagingNoticeClassesAndRemedies(t *testing.T) {
	for _, reset := range []bool{false, true} {
		a := IBKRNotice("synthetic-host", time.Date(2026, 10, 31, 0, 40, 0, 0, time.UTC), reset, "synthetic evidence")
		want := KindIBKRConnectivity
		if reset {
			want = KindGatewayResetWait
		}
		if a.Kind != want || a.Class(false) != Operational || !a.Repeat || a.Key != GatewayKey("synthetic-host") {
			t.Fatalf("notice=%+v", a)
		}
		if reset && !strings.Contains(a.Title, "reset wait") {
			t.Fatal(a.Title)
		}
		if !reset && (strings.Contains(a.Body, "sudo systemctl restart ti-gateway") || !strings.Contains(a.Title, "connectivity lost")) {
			t.Fatal(a.Body)
		}
	}
	for _, kind := range []Kind{KindGatewayLoggedOut, KindHistoryUnavailable, KindRestoredLogNewer} {
		if (Alert{Kind: kind}).Class(false) != Operational {
			t.Fatal(kind)
		}
	}
}

func TestPagingNoticeTextUsesConfiguredReminderPolicy(t *testing.T) {
	for _, a := range []Alert{GatewayLoggedOutAlert("synthetic", time.Time{}, time.Time{}, "synthetic"), UnitFailedAlert("synthetic", "ti-engine@sample.service", "run", "synthetic", time.Time{}), UnitStillFailedAlert("synthetic", "ti-engine@sample.service", "run", time.Time{}, 1, time.Time{})} {
		if strings.Contains(a.Body, "once an hour") || strings.Contains(a.Body, "reminded hourly") {
			t.Errorf("notice promises a fixed interval: %s", a.Title)
		}
	}
}

func TestIBKRScheduleExactFields(t *testing.T) {
	for _, raw := range []string{
		`{"Windows":[]}`, `{"windows":[],"Windows":[]}`,
		`{"windows":[{"Days":["Sat"],"start":"00:00","end":"12:00","zone":"UTC"}]}`,
		`{"windows":[{"days":["Sat"],"Start":"00:00","end":"12:00","zone":"UTC"}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","End":"12:00","zone":"UTC"}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","Zone":"UTC"}]}`,
		`{"windows":[{"days":["Sat"],"start":"00:00","end":"12:00","zone":"UTC","days":["Sun"]}]}`,
	} {
		if _, err := ParseIBKRSchedule([]byte(raw)); err == nil {
			t.Errorf("accepted alias or duplicate: %s", raw)
		}
	}
}

func TestIBKRScheduleShippedExample(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/etc/ibkr-maintenance.json.example")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseIBKRSchedule(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.windows) != 3 {
		t.Fatalf("windows=%d", len(s.windows))
	}
	for _, tc := range []struct{ at, end string }{
		{"2026-10-29T23:39:59-04:00", ""},
		{"2026-10-29T23:40:00-04:00", "2026-10-30T01:00:00-04:00"},
		{"2026-10-30T01:00:00-04:00", ""},
		{"2026-10-30T23:40:00-04:00", "2026-11-01T14:00:00-05:00"},
		{"2026-10-31T12:00:00-04:00", "2026-11-01T14:00:00-05:00"},
		{"2026-11-01T13:59:59-05:00", "2026-11-01T14:00:00-05:00"},
		{"2026-11-01T14:00:00-05:00", ""},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		end, _ := time.Parse(time.RFC3339, tc.end)
		if got := s.WindowEnd(at); !got.Equal(end) {
			t.Errorf("%s end=%s want=%s", tc.at, got, end)
		}
	}
}

func TestIBKRResetNoticeExplainsRefusal(t *testing.T) {
	a := IBKRNotice("synthetic", time.Time{}, true, "synthetic evidence")
	if strings.Contains(a.Body, "not a password failure") || !strings.Contains(a.Body, "refusal after 01:00 ET pages") {
		t.Fatal(a.Body)
	}
}
