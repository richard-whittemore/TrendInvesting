package report

import (
	"strings"
	"testing"
	"time"
)

func TestGatewayLoggedOutAlertIsUrgentRepeatsAndSaysWhatToDo(t *testing.T) {
	since := time.Date(2026, 10, 27, 23, 45, 0, 0, time.UTC)
	a := GatewayLoggedOutAlert("srv", since, since.Add(time.Hour), "gateway: logged out: dialog\n")
	if a.Key != GatewayKey("srv") || a.Key != "gateway-logged-out:srv" || a.Kind != KindGatewayLoggedOut || a.Severity != Urgent || !a.Repeat {
		t.Errorf("alert %+v is not an urgent repeating gateway incident", a)
	}
	if a.Title != "srv: IB Gateway is logged out" {
		t.Errorf("title %q", a.Title)
	}
	for _, want := range []string{"2026-10-27T23:45:00Z", "gateway: logged out: dialog", "paper login", "systemctl restart ti-gateway", "3 failures"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body lacks %q: %s", want, a.Body)
		}
	}
}
