// Gateway adapter evidence cases, in journal order. Rules: docs/manual/server.md,
// "The Gateway is logged out"; ADR 0019, "Alerting". Loss must remain unrestored
// in the current invocation for at least three hours, including plain connects. A start
// ends the previous invocation's evidence; it does not itself prove recovery.
// Synthetic journals only; command and delivery stand-ins only record calls
// and return fixtures. Every row runs with IBC login and with a later logout.
//
// TestGatewayAdapterCurrentInvocation/<row>/{login,logout}
// Row                     Evidence          Login: open/keep/resolve  Logout: open/keep/resolve
// loss                    loss              yes/yes/no               yes/yes/no
// loss_rest               loss,rest         no/no/yes                yes/yes/no
// loss_conn               loss,conn         yes/yes/no               yes/yes/no
// loss_start              loss,start        no/no/yes*               yes/yes/no
// loss_start_conn         loss,start,conn   no/no/yes                yes/yes/no
// loss_start_conn_loss    loss,start,conn,loss yes/yes/no             yes/yes/no
// loss_conn_loss          loss,conn,loss    yes/yes/no               yes/yes/no
//
// start = "ibkr adapter starting", conn = "ibkr link connected" (also the
// adapter's state=connected handshake), rest = existing restoration/reconciliation.
// failed = the new invocation exits before connecting, with no loss of its own.
// loss_conn_loss keeps the FIRST loss time; loss_start_conn_loss uses the first
// loss of the NEW invocation. Plain connects never restart the three-hour clock.
// * Resolution uses a completed IBC login newer than the open incident; the
// existing positive-recovery requirement remains unchanged for a start alone.
package main

import (
	"strings"
	"testing"
	"time"
)

const (
	adapterStart = "host adapter[3]: ibkr adapter starting mode=paper client_id=11 sending=off ibapi=synthetic"
	adapterConn  = "host adapter[3]: ibkr link connected client_id=11"
)

func TestGatewayAdapterCurrentInvocation(t *testing.T) {
	cases := []struct {
		name   string
		events []string
		lost   bool
	}{
		{"loss", []string{linkLost}, true},
		{"loss_rest", []string{linkLost, linkUp}, false},
		{"loss_conn", []string{linkLost, adapterConn}, true},
		{"loss_start", []string{linkLost, adapterStart}, false},
		{"loss_start_conn", []string{linkLost, adapterStart, adapterConn}, false},
		{"loss_start_conn_loss", []string{linkLost, adapterStart, adapterConn, linkLost}, true},
		{"loss_conn_loss", []string{linkLost, adapterConn, linkLost}, true},
	}
	for _, tc := range cases {
		for _, ibcState := range []string{"login", "logout"} {
			t.Run(tc.name+"/"+ibcState, func(t *testing.T) {
				g := newStandInGateway(t)
				g.adapter(stamp(tuesday(6, 0), linkLost))
				r := &relay{sent: map[string][]string{}}
				existing := t.TempDir()
				for _, at := range []time.Time{tuesday(10, 0), tuesday(10, 10)} {
					if err := gatewayRun(t, r, existing, g.path(), "boost", at); err != nil {
						t.Fatal(err)
					}
				}
				if gatewayPages(r) != 2 {
					t.Fatal("initial loss did not open one Gateway incident on both channels")
				}
				g.login(tuesday(10, 20))
				if ibcState == "logout" {
					g.ibc(stamp(tuesday(10, 20), ibcCompleted), stamp(tuesday(10, 21), "host ibc[1]: LoginState is LOGGED_OUT"))
				}
				lines := []string{stamp(tuesday(5, 0), adapterStart), stamp(tuesday(6, 0), tc.events[0])}
				for i, event := range tc.events[1:] {
					lines = append(lines, stamp(tuesday(10, 21+i), event))
				}
				g.adapter(lines...)
				filtered, err := journalQuery(g.path(), "ti-adapter-boost.service", linkEvent)
				if err != nil {
					t.Fatal(err)
				}
				since, _, _, err := lostSince(filtered)
				if err != nil {
					t.Fatal(err)
				}
				var wantSince time.Time
				if tc.lost {
					wantSince = tuesday(6, 0)
					if tc.name == "loss_start_conn_loss" {
						wantSince = tuesday(10, 23)
					}
				}
				if !since.Equal(wantSince) {
					t.Errorf("since = %v, want first loss %v", since, wantSince)
				}
				ev, err := gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"})(tuesday(13, 30))
				if err != nil {
					t.Fatal(err)
				}
				wantOut := tc.lost || ibcState == "logout"
				if ev.out != wantOut {
					t.Errorf("current-invocation outage = %v, want %v: %s", ev.out, wantOut, ev.said)
				}
				if strings.Contains(ev.said, "ti-adapter-boost.service has reported") != tc.lost {
					t.Errorf("adapter loss evidence must stand = %v: %s", tc.lost, ev.said)
				}
				if ev.ibkrUnreachable != (tc.lost && ibcState == "login") {
					t.Errorf("IBKR-unreachable wording = %v, want %v", ev.ibkrUnreachable, tc.lost && ibcState == "login")
				}
				if err := gatewayRun(t, r, existing, g.path(), "boost", tuesday(13, 30)); err != nil {
					t.Fatal(err)
				}
				wantResolves := 1
				if wantOut {
					wantResolves = 0
				}
				if got := resolvesOf(r); got != wantResolves {
					t.Errorf("existing incident: resolves = %d, want %d", got, wantResolves)
				}
				fresh := &relay{sent: map[string][]string{}}
				dir := t.TempDir()
				for _, at := range []time.Time{tuesday(13, 30), tuesday(13, 40)} {
					if err := gatewayRun(t, fresh, dir, g.path(), "boost", at); err != nil {
						t.Fatal(err)
					}
				}
				wantPages := 0
				if wantOut {
					wantPages = 2
				}
				if got := gatewayPages(fresh); got != wantPages {
					t.Errorf("new incident: pages = %d, want %d", got, wantPages)
				}
			})
		}
	}
}

// The three-hour clock survives plain connects and repeated losses; only a new
// invocation, restoration or reconciliation ends loss evidence (docs/manual/server.md,
// "The Gateway is logged out"; ADR 0019, "Alerting").
func TestGatewayAdapterLossClock(t *testing.T) {
	for name, boundary := range map[string]string{
		"connected": adapterConn, "handshake": linkHandshake, "start": adapterStart,
		"restored": linkUp, "reconciled": linkReconciled,
	} {
		for _, code := range []string{"1100", "2110"} {
			t.Run(name+"/"+code, func(t *testing.T) {
				g := newStandInGateway(t)
				loss := strings.ReplaceAll(linkLost, "2110", code)
				g.adapter(stamp(tuesday(6, 0), loss), stamp(tuesday(7, 0), boundary),
					stamp(tuesday(8, 0), loss), stamp(tuesday(8, 30), loss))
				wantSince := tuesday(8, 0)
				if name == "connected" || name == "handshake" {
					wantSince = tuesday(6, 0)
				}
				check := gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"})
				for _, offset := range []time.Duration{-time.Second, 0} {
					ev, err := check(wantSince.Add(3*time.Hour + offset))
					if err != nil {
						t.Fatal(err)
					}
					if ev.out != (offset == 0) {
						t.Errorf("at three hours %+v: outage = %v, want %v", offset, ev.out, offset == 0)
					}
					if ev.out && !strings.Contains(ev.said, stamp(wantSince, loss)) {
						t.Errorf("loss must begin at first unrestored loss: %s", ev.said)
					}
				}
			})
		}
	}
}

// Neither a new invocation nor an API handshake is proof
// of IBKR recovery (docs/manual/server.md, "The Gateway is logged out";
// ADR 0019, "Alerting"). Journal order, not wall-clock order, sets invocation.
func TestGatewayAdapterStartAndHandshakeRecoveryEvidence(t *testing.T) {
	for name, boundary := range map[string]string{"start": adapterStart, "connected": adapterConn, "handshake": linkHandshake} {
		t.Run(name, func(t *testing.T) {
			g := newStandInGateway(t)
			g.login(tuesday(5, 0))
			g.adapter(stamp(tuesday(6, 0), linkLost), stamp(tuesday(5, 30), boundary))
			ev, err := gatewayJournalChecker(g.path(), []string{"ti-adapter-boost.service"})(tuesday(14, 0))
			if err != nil {
				t.Fatal(err)
			}
			if ev.out != (name != "start") || !ev.recoveredAt.Equal(tuesday(5, 0)) {
				t.Fatalf("only a start ends loss; neither event invents recovery: %+v", ev)
			}
		})
	}
}
