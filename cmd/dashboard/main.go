// Command dashboard serves a read-only view of each account's journal: the
// daily summary and the alerts, in HTML and JSON (docs/live-trading/plan.md
// section 3). It has no write path: it answers GET and HEAD only, opens its
// journals for reading, and refuses to listen anywhere but loopback or this
// host's Tailscale address, so on the server it is reached only over
// Tailscale. Every rule lives in internal/report; this is composition.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// defaultAddr is the dashboard's listen address: loopback, on the port the
// server's unit passes and the Lightsail runbook publishes to the tailnet
// with `tailscale serve` (deploy/systemd/ti-dashboard.service).
const defaultAddr = "127.0.0.1:8088"

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func run(args []string, log io.Writer) error {
	flags := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	flags.SetOutput(log)
	var journals stringList
	flags.Var(&journals, "journal", "path to an account's journal; repeat for each account")
	addr := flags.String("addr", defaultAddr, "listen address: loopback or a Tailscale address")
	benchmark := flags.String("benchmark", "SPY", "benchmark instrument for the equity comparison")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(journals) == 0 {
		return errors.New("dashboard: at least one -journal is required")
	}
	if err := checkListenAddr(*addr, hostInterfaces); err != nil {
		return err
	}
	server := &http.Server{
		Addr: *addr, Handler: newHandler(journals, *benchmark),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute,
	}
	_, _ = fmt.Fprintf(log, "dashboard: read-only, listening on %s\n", *addr)
	return server.ListenAndServe()
}

var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// netInterface is one network interface of the host and its addresses.
type netInterface struct {
	Name  string
	Addrs []netip.Addr
}

// hostInterfaces reads this host's interfaces and their addresses.
func hostInterfaces() ([]netInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]netInterface, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		n := netInterface{Name: iface.Name}
		for _, a := range addrs {
			if prefix, err := netip.ParsePrefix(a.String()); err == nil {
				n.Addrs = append(n.Addrs, prefix.Addr().Unmap())
			}
		}
		out = append(out, n)
	}
	return out, nil
}

// tailscaleInterface reports whether an interface name is one Tailscale
// creates: tailscale0 on Linux, a utun device on macOS.
func tailscaleInterface(name string) bool {
	return strings.HasPrefix(name, "tailscale") || strings.HasPrefix(name, "utun")
}

// checkListenAddr accepts only a literal loopback address, or a Tailscale
// address that one of this host's Tailscale interfaces owns, with a port. A
// hostname, a wildcard or any other interface would expose the dashboard
// beyond Tailscale; 100.64.0.0/10 is also the shared carrier-grade NAT range,
// so being in it is not enough.
func checkListenAddr(addr string, interfaces func() ([]netInterface, error)) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("dashboard: -addr %q must be host:port: %w", addr, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("dashboard: -addr host %q must be a literal loopback or Tailscale IP", host)
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return nil
	}
	if !tailscaleV4.Contains(ip) && !tailscaleV6.Contains(ip) {
		return fmt.Errorf("dashboard: %s is neither loopback nor a Tailscale address (100.64.0.0/10, fd7a:115c:a1e0::/48)", ip)
	}
	ifaces, err := interfaces()
	if err != nil {
		return fmt.Errorf("dashboard: cannot read this host's interfaces to check %s is its Tailscale address: %w", ip, err)
	}
	for _, iface := range ifaces {
		for _, a := range iface.Addrs {
			if a == ip && tailscaleInterface(iface.Name) {
				return nil
			}
		}
	}
	return fmt.Errorf("dashboard: %s is not an address of this host's Tailscale interface", ip)
}

// account is one journal's view.
type account struct {
	Summary report.Summary
	Alerts  []alertView
}

type alertView struct {
	Kind     report.Kind
	Severity report.Severity
	At       time.Time
	Title    string
	Body     string
}

// newHandler serves the journals read-only. Every request re-reads and
// re-verifies them, so the page is never staler than the file.
func newHandler(journals []string, benchmark string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/api/summary.json", func(w http.ResponseWriter, _ *http.Request) {
		accounts, err := load(journals, benchmark)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(accounts)
	})
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		accounts, err := load(journals, benchmark)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, pageData{Accounts: accounts, Theme: themeOf(r)})
	})
	return readOnly(mux)
}

// pageData is what the page renders: the accounts and the theme the reader
// chose, "dark" or "light", or empty to follow the device's own setting.
type pageData struct {
	Accounts []account
	Theme    string
}

// themeOf reads the ?theme= choice. Only "dark" and "light" are honoured; any
// other value leaves the device's setting in charge. The choice is a link the
// server answers, so the page needs no script and nothing is stored.
func themeOf(r *http.Request) string {
	switch t := r.URL.Query().Get("theme"); t {
	case "dark", "light":
		return t
	}
	return ""
}

// equityAssumption states in words what the equity comparison assumes. It is
// one sentence, shown on every equity card, so a change in how the benchmark
// side is built changes it in one place.
const equityAssumption = "The comparison assumes price return only, no deposits or withdrawals: the benchmark's dividends are left out, and money you add or take out counts as the account's own gain or loss."

// readOnly admits GET and HEAD and nothing else, whatever the route.
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		h.Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "read-only dashboard", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// fail answers without naming the server's paths.
func fail(w http.ResponseWriter, err error) {
	message := "journal unavailable"
	if strings.Contains(err.Error(), "does not verify") {
		message = "journal does not verify"
	}
	http.Error(w, message, http.StatusInternalServerError)
}

func load(journals []string, benchmark string) ([]account, error) {
	var out []account
	for _, path := range journals {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("read journal")
		}
		_, envelopes, err := report.Load(raw)
		if err != nil {
			return nil, err
		}
		a, err := view(envelopes, benchmark)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func view(envelopes []event.Envelope, benchmark string) (account, error) {
	date, err := report.LastDate(envelopes)
	if err != nil {
		return account{}, err
	}
	summary, err := report.Summarize(envelopes, date, benchmark)
	if err != nil {
		return account{}, err
	}
	alerts, err := report.Alerts(envelopes)
	if err != nil {
		return account{}, err
	}
	a := account{Summary: summary}
	for _, al := range alerts {
		a.Alerts = append(a.Alerts, alertView{Kind: al.Kind, Severity: al.Severity, At: al.At, Title: al.Title, Body: al.Body})
	}
	return a, nil
}
