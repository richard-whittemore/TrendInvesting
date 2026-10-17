package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

var t0 = time.Date(2024, time.January, 2, 21, 0, 0, 0, time.UTC)

func envelope(t *testing.T, seq uint64, eventType string, version uint32, id string, payload any) event.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return event.Envelope{ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
		EventTime: t0, RecordedAt: t0, Sequence: seq, Source: "test", StrategyVersion: "v", ConfigurationHash: "h",
		PayloadHash: event.HashPayload(raw), Payload: raw}
}

func writeJournal(t *testing.T, dir string) string {
	t.Helper()
	return writeJournalWith(t, dir)
}

// writeJournalWith writes the run with extra entries after its own.
func writeJournalWith(t *testing.T, dir string, extra ...journal.Entry) string {
	t.Helper()
	cfg := event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint <b>", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR}
	snap := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: t0, Currency: "USD", NetLiquidation: 4000,
		Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 39}}}
	refused := event.OrderRefusedPayload{AccountID: "joint", PlanID: "p", Tag: "targets:0", ClientOrderID: "tw-a", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 39, LimitPrice: 101, Reason: event.RefusedOrderSize, Detail: "figures",
		Figures: event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 0.5, ExposureAfter: 0.98, OrderFraction: 0.48, OrdersInDecision: 1}}
	entries := []journal.Entry{
		{Kind: journal.KindInput, Envelope: envelope(t, 1, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "cfg", cfg)},
		{Kind: journal.KindInput, Envelope: envelope(t, 2, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "snap", snap)},
		{Kind: journal.KindDecision, Envelope: envelope(t, 3, event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, "refuse", refused)},
	}
	entries = append(entries, extra...)
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "v", t0, t0), entries); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, http.NoBody))
	return rec
}

func TestDashboardShowsEachAccountEscapedAndNeverCached(t *testing.T) {
	h := newHandler([]string{writeJournal(t, t.TempDir())}, "SPY")
	rec := get(h, http.MethodGet, "/")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, body)
	}
	for _, want := range []string{"SPY", "39", "order-size", "refused", "2024-01-02"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>") || !strings.Contains(body, "&lt;b&gt;") {
		t.Error("an account name must be escaped")
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("headers = %v", rec.Header())
	}
	if strings.Contains(strings.ToLower(body), "<form") || strings.Contains(strings.ToLower(body), "<script") {
		t.Error("a read-only page has no form and no script")
	}
}

func TestDashboardSaysWhenAnInputWasNotRead(t *testing.T) {
	rejected := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: t0, Currency: "USD", NetLiquidation: -1}
	path := writeJournalWith(t, t.TempDir(),
		journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 4, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "snap-bad", rejected)})
	body := get(newHandler([]string{path}, "SPY"), http.MethodGet, "/").Body.String()
	if !strings.Contains(body, "Inputs not read: 1") || !strings.Contains(body, "4000.00") {
		t.Fatalf("the page must show the figures without the rejected input, and say it left one out:\n%s", body)
	}
}

func TestDashboardJSONCarriesTheSummariesAndAlerts(t *testing.T) {
	h := newHandler([]string{writeJournal(t, t.TempDir())}, "SPY")
	rec := get(h, http.MethodGet, "/api/summary.json")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var got []struct {
		Summary struct{ Refusals int }
		Alerts  []struct{ Kind string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Summary.Refusals != 1 || len(got[0].Alerts) != 1 || got[0].Alerts[0].Kind != "refusal" {
		t.Fatalf("got %+v", got)
	}
}

func TestEveryRouteRefusesEveryWritingMethod(t *testing.T) {
	h := newHandler([]string{writeJournal(t, t.TempDir())}, "SPY")
	for _, path := range []string{"/", "/api/summary.json", "/healthz", "/nowhere"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, "PURGE"} {
			rec := get(h, method, path)
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s = %d allow %q", method, path, rec.Code, rec.Header().Get("Allow"))
			}
		}
	}
	if rec := get(h, http.MethodGet, "/nowhere"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path = %d", rec.Code)
	}
	if rec := get(h, http.MethodHead, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("head healthz = %d", rec.Code)
	}
}

func TestDashboardRefusesAJournalThatDoesNotVerify(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.journal")
	if err := os.WriteFile(bad, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/api/summary.json"} {
		rec := get(newHandler([]string{bad}, "SPY"), http.MethodGet, path)
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "does not verify") {
			t.Errorf("%s = %d %q", path, rec.Code, rec.Body.String())
		}
	}
	rec := get(newHandler([]string{filepath.Join(dir, "none")}, "SPY"), http.MethodGet, "/")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("missing journal = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), dir) {
		t.Error("an error page must not leak the server's paths")
	}
}

// host is a machine with Tailscale up on tailscale0, and a second interface
// that owns an address in the shared CGNAT range Tailscale also uses.
func host() ([]netInterface, error) {
	return []netInterface{
		{Name: "lo", Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}},
		{Name: "tailscale0", Addrs: []netip.Addr{netip.MustParseAddr("100.101.102.103"), netip.MustParseAddr("fd7a:115c:a1e0::1")}},
		{Name: "utun4", Addrs: []netip.Addr{netip.MustParseAddr("100.64.0.1")}},
		{Name: "eth1", Addrs: []netip.Addr{netip.MustParseAddr("100.100.1.1"), netip.MustParseAddr("192.168.1.5")}},
	}, nil
}

func TestListenAddressMustBeLoopbackOrThisHostsTailscaleAddress(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8080": true, "[::1]:8080": true, "100.101.102.103:8080": true, "100.64.0.1:1": true,
		"[fd7a:115c:a1e0::1]:8080": true,
		// In the Tailscale range but owned by a carrier-grade NAT interface,
		// or by no interface of this host.
		"100.100.1.1:8080": false, "100.127.255.254:1": false, "[fd7a:115c:a1e0::2]:8080": false,
		"0.0.0.0:8080": false, ":8080": false, "192.168.1.5:8080": false, "100.128.0.1:8080": false, "100.63.0.1:8080": false,
		"localhost:8080": false, "example.com:80": false, "[::]:8080": false, "127.0.0.1": false, "8.8.8.8:80": false,
	} {
		if err := checkListenAddr(addr, host); (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestListenAddressIsRefusedWhenTheInterfacesCannotBeRead(t *testing.T) {
	broken := func() ([]netInterface, error) { return nil, errors.New("no interfaces") }
	if err := checkListenAddr("100.101.102.103:8080", broken); err == nil {
		t.Fatal("an address that cannot be checked must be refused")
	}
	if err := checkListenAddr("127.0.0.1:8080", broken); err != nil {
		t.Fatalf("loopback needs no interface check: %v", err)
	}
}

func TestTheHostsInterfacesAreRead(t *testing.T) {
	got, err := hostInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	loopback := false
	for _, i := range got {
		for _, a := range i.Addrs {
			loopback = loopback || a.IsLoopback()
		}
	}
	if !loopback {
		t.Fatalf("no loopback address among %v", got)
	}
}

func TestRunRefusesBadArguments(t *testing.T) {
	good := writeJournal(t, t.TempDir())
	for name, args := range map[string][]string{
		"no journal": {}, "public address": {"-journal", good, "-addr", "0.0.0.0:80"}, "bad flag": {"-zzz"},
	} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestThePackageHasNoWritePath fails if any production file in the package
// calls a filesystem-writing function or registers a handler for a writing
// method: the dashboard's read-only guarantee is checked mechanically.
func TestThePackageHasNoWritePath(t *testing.T) {
	forbidden := map[string]bool{
		"WriteFile": true, "Create": true, "CreateTemp": true, "OpenFile": true, "Remove": true, "RemoveAll": true, "Rename": true,
		"Mkdir": true, "MkdirAll": true, "Truncate": true, "Chmod": true, "Chtimes": true, "Symlink": true, "Link": true,
		"MethodPost": true, "MethodPut": true, "MethodPatch": true, "MethodDelete": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && (pkg.Name == "os" || pkg.Name == "http") && forbidden[sel.Sel.Name] {
					t.Errorf("%s: %s.%s is a write path", fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
				}
			}
			return true
		})
	}
}
