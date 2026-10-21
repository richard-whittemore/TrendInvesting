// Command dashboard serves a read-only view of each account's journal: the
// daily summary and the alerts, in HTML and JSON (docs/live-trading/plan.md
// section 3). It has no write path: it answers GET and HEAD only, opens its
// journals for reading, and refuses to listen anywhere but loopback or this
// host's Tailscale address, so on the server it is reached only over
// Tailscale. Every rule lives in internal/report; this is composition.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
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
	var engineDirs stringList
	flags.Var(&engineDirs, "engine-dir", "an engine's log directory: each request shows the current run in it, the newest link of its chain of runs, followed back to the first; repeat for each account")
	addr := flags.String("addr", defaultAddr, "listen address: loopback or a Tailscale address")
	benchmark := flags.String("benchmark", "SPY", "benchmark instrument for the equity comparison")
	var cashLedgers stringList
	flags.Var(&cashLedgers, "cash-ledger", "the Flex reader's ledger of an account's deposits and withdrawals (/var/lib/trend-investing/flex/<instance>/cash-movements.jsonl), which the equity comparison's shadow account mirrors: the path alone when one -journal or -engine-dir is given, else SOURCE=PATH naming that -journal or -engine-dir; repeat for each account")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(journals)+len(engineDirs) == 0 {
		return errors.New("dashboard: at least one -journal or -engine-dir is required")
	}
	ledgers, err := ledgersOf(cashLedgers, journals, engineDirs)
	if err != nil {
		return err
	}
	if err := checkListenAddr(*addr, hostInterfaces); err != nil {
		return err
	}
	server := &http.Server{
		Addr: *addr, Handler: newHandlerFor(journals, engineDirs, ledgers, *benchmark),
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

// account is one journal's view. Warning, for an engine directory, names a
// log restored without its modification time (report.RestoredLogNewer).
type account struct {
	Summary report.Summary
	Alerts  []alertView
	Warning string `json:",omitempty"`
	// Name and Message, set for an engine directory that cannot be shown, are
	// the whole card: the directory's name, and "<name>: <why>", in place of
	// the account's figures. Every other account still renders in full.
	Name    string `json:",omitempty"`
	Message string `json:",omitempty"`
	// Anchor is a message card's page anchor, "dir-<n>", where n is the card's
	// position in the whole account list, counting the fixed journals first
	// (not its position among the engine directories): no account's id can be
	// taken for it, nor another directory of the same name.
	Anchor string `json:"-"`
}

// MarshalJSON gives a message card its name and message alone, so a consumer
// cannot take it for an account with a zero balance.
func (a account) MarshalJSON() ([]byte, error) {
	if a.Message != "" {
		return json.Marshal(struct{ Name, Message string }{a.Name, a.Message})
	}
	type plain account
	return json.Marshal(plain(a))
}

type alertView struct {
	Kind     report.Kind
	Severity report.Severity
	At       time.Time
	Title    string
	Body     string
}

// newHandlerFor serves the journals and, for each engine directory, the
// current run in it, the head of its chain of runs, resolved afresh on every
// request, so a restart that starts a new run is shown without restarting
// the dashboard. Each is shown with the runs before it, followed back to the
// first (report.EarlierRunsOfRecovery). Every request re-reads and
// re-verifies the current log, so the page is never staler than the file.
// ledgers names each account's Flex ledger (source).
func newHandlerFor(journals, engineDirs []string, ledgers map[string]string, benchmark string) http.Handler {
	return newHandlerWith(source{journals: journals, dirs: engineDirs, ledgers: ledgers}, benchmark, os.ReadFile)
}

// newHandlerWith is newHandlerFor reading each file whole with readFile.
func newHandlerWith(sources source, benchmark string, readFile func(string) ([]byte, error)) http.Handler {
	return newHandlerCached(sources, benchmark, newLogCache(readFile, func(path string) (io.ReadCloser, error) { return os.Open(path) }))
}

// newHandlerCached is newHandlerFor keeping the earlier logs in logs.
func newHandlerCached(sources source, benchmark string, logs *logCache) http.Handler {
	logs.benchmark = benchmark
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/api/summary.json", func(w http.ResponseWriter, _ *http.Request) {
		accounts, err := load(sources, benchmark, logs)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(accounts)
	})
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		accounts, err := load(sources, benchmark, logs)
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

// errNoRun is the engine directory holding no run yet: a first start, which
// the page says so about rather than calling the journal unavailable.
var errNoRun = errors.New("the engine directory holds no run yet")

// errAbandoned is the engine directory whose logs hold nothing past their
// header (no log holds a record, so the chain has no head): a run that started
// and crashed, not a first start.
var errAbandoned = errors.New("the engine directory holds only abandoned starts")

// reason says why a journal is not shown, without naming the server's paths.
// A broken chain of runs never reaches it: the page shows it on the equity
// card (load).
func reason(err error) string {
	switch {
	case errors.Is(err, errNoRun):
		return "no run yet: the engine has not started a run"
	case errors.Is(err, errAbandoned):
		return "the engine started a run but recorded nothing past its header"
	case strings.Contains(err.Error(), "does not verify"):
		return "journal does not verify"
	}
	return "journal unavailable"
}

// fail answers a fixed journal that cannot be shown.
func fail(w http.ResponseWriter, err error) {
	http.Error(w, reason(err), http.StatusInternalServerError)
}

// source is where the accounts' logs are: fixed journals, and engine
// directories whose current run is found on each request. ledgers names,
// by journal path or engine directory, the Flex reader's ledger of that
// account's deposits and withdrawals; an account without one is compared
// assuming none, and its card says so.
type source struct {
	journals, dirs []string
	ledgers        map[string]string
}

// ledgersOf places each -cash-ledger value with its account: a bare path
// belongs to the one -journal or -engine-dir given, and SOURCE=PATH to the
// -journal or -engine-dir SOURCE names. A ledger that cannot be placed, or
// a second for one account, is refused: a deposit mirrored on the wrong
// account would misstate both.
func ledgersOf(values, journals, dirs []string) (map[string]string, error) {
	sources := append(append([]string{}, journals...), dirs...)
	out := map[string]string{}
	for _, v := range values {
		src, path, named := strings.Cut(v, "=")
		switch {
		case !named && len(sources) != 1:
			return nil, fmt.Errorf("dashboard: -cash-ledger %q must be SOURCE=PATH, naming one of the %d -journal or -engine-dir values", v, len(sources))
		case !named:
			src, path = sources[0], v
		case !slices.Contains(sources, src):
			return nil, fmt.Errorf("dashboard: -cash-ledger names %q, which is no -journal or -engine-dir", src)
		}
		if path == "" {
			return nil, fmt.Errorf("dashboard: -cash-ledger %q names no ledger file", v)
		}
		if _, twice := out[src]; twice {
			return nil, fmt.Errorf("dashboard: -cash-ledger is given twice for %q", src)
		}
		out[src] = path
	}
	return out, nil
}

// load reads and recovers each account's current log once, afresh, and its
// earlier runs and the engine directories' other logs through logs. Only the
// equity comparison reads the earlier runs, so a broken chain of them is
// shown on the equity card, naming both logs, and never refuses the page:
// the current run's holdings, state, alerts and day's figures do not depend
// on it.
func load(src source, benchmark string, logs *logCache) ([]account, error) {
	type current struct{ path, warning, name, message, ledger string }
	var paths []current
	for _, path := range src.journals {
		paths = append(paths, current{path: path, ledger: src.ledgers[path]})
	}
	for _, dir := range src.dirs {
		path, warning, err := currentLogIn(dir, logs)
		if err != nil {
			// One directory's trouble is that account's card only.
			paths = append(paths, current{name: filepath.Base(dir), message: filepath.Base(dir) + ": " + reason(err)})
			continue
		}
		paths = append(paths, current{path: path, warning: warning, name: filepath.Base(dir), ledger: src.ledgers[dir]})
	}
	var out []account
	for i, c := range paths {
		a, err := loadOne(c.path, c.warning, c.ledger, benchmark, logs)
		if c.message == "" && err != nil && c.name != "" {
			// An engine directory's head log that cannot be shown is that
			// account's card; only a fixed journal fails the page.
			c.message = c.name + ": " + reason(err)
		}
		if c.message != "" {
			out = append(out, account{Name: c.name, Message: c.message, Anchor: "dir-" + strconv.Itoa(i)})
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// loadOne reads, verifies and views the log at path, with the cash ledger at
// ledger ("" for none), read afresh on every request as the log is.
func loadOne(path, warning, ledger, benchmark string, logs *logCache) (account, error) {
	if path == "" {
		return account{}, nil
	}
	raw, err := logs.readFile(path)
	if err != nil {
		return account{}, errors.New("read journal")
	}
	envelopes, recovery, err := report.LoadLog(raw)
	if err != nil {
		return account{}, err
	}
	var history report.History
	if recovery != nil {
		history = report.EarlierRunsOfRecovery(path, *recovery, logs.earlier)
	}
	a, err := view(history, report.LoadCashLedger(ledger, logs.readFile), envelopes, benchmark)
	if err != nil {
		return account{}, err
	}
	a.Warning = warning
	return a, nil
}

// currentLogIn returns the path of the current run in an engine's log
// directory: the head of its chain of runs, the log no other log's approval
// names as its previous log, ties broken by the engine's own latest-run rule
// (restart.ChainHead over the engine's listing, journalquery.ListLogs). Each
// log's first records are all the head is found by. warning names a log the
// chain names that is newer by modification time than the head, a log
// restored without its time, which the engine would take for the latest run
// at its next start; it is "" when there is none.
func currentLogIn(dir string, logs *logCache) (path, warning string, err error) {
	files, err := journalquery.ListLogs(dir, ".log")
	if err != nil {
		return "", "", errors.New("read the engine directory")
	}
	logs.sweep(dir, files)
	read := map[string]bool{}
	head, newer, err := restart.ChainHead(files,
		func(name string) (journal.Recovery, error) { return logs.start(filepath.Join(dir, name)) },
		func(name string) (journal.Recovery, error) {
			read[filepath.Join(dir, name)] = true
			return logs.tiedWhole(filepath.Join(dir, name))
		})
	if head != "" {
		delete(read, filepath.Join(dir, head))
	}
	// Only the logs this request's tie read stay whole; a log the tie no
	// longer reaches, and the head, are dropped.
	logs.keepTied(dir, read)
	if err != nil {
		return "", "", err
	}
	if head == "" {
		if len(files) > 0 {
			return "", "", errAbandoned
		}
		return "", "", errNoRun
	}
	if newer != "" {
		warning = report.RestoredLogNewer(newer, head)
	}
	return filepath.Join(dir, head), warning, nil
}

// logCache keeps, by path, size and modification time, what the page reads
// of logs other than the current one: of an earlier run's log only what the
// chain and the equity history read (report.ForHistory), and of any other
// log only its first records, which the chain's head is found by. A durable
// log is only ever appended to, so a log whose size and time are unchanged
// holds what it held: an earlier run's log is read again only when its size
// or modification time changes, and the current log, never kept whole, is
// read and verified afresh on every request. A log that ties with the
// current run for the chain's head (the log a resumed run copied) is read
// whole by the engine's latest-run rule; it is kept whole by its size and
// modification time, so it too is read again only when it changes, and the
// head is never kept whole. An entry whose file is gone is dropped.
type logCache struct {
	stat      func(string) (fs.FileInfo, error)
	readFile  func(string) ([]byte, error)
	open      func(string) (io.ReadCloser, error)
	benchmark string
	mu        sync.Mutex
	starts    map[string]cachedLog
	histories map[string]cachedLog
	tied      map[string]cachedLog
}

// newLogCache is an empty cache reading whole logs with readFile and the
// first records of a log through open.
func newLogCache(readFile func(string) ([]byte, error), open func(string) (io.ReadCloser, error)) *logCache {
	return &logCache{stat: os.Stat, readFile: readFile, open: open, starts: map[string]cachedLog{}, histories: map[string]cachedLog{}, tied: map[string]cachedLog{}}
}

// cachedLog is what is kept of one log, or the reason it does not recover,
// as of its size and modification time.
type cachedLog struct {
	size     int64
	modified time.Time
	recovery journal.Recovery
	err      error
}

func (c cachedLog) current(info fs.FileInfo) bool {
	return c.size == info.Size() && c.modified.Equal(info.ModTime())
}

// startGroups is how many committed groups a log's start holds: its
// configuration's and its approval's (ADR 0027).
const startGroups = 2

// start returns the first records of the log at path. A file that cannot
// be read is not kept, so a fault fixed at the file is seen on the next
// request.
func (c *logCache) start(path string) (journal.Recovery, error) {
	info, err := c.stat(path)
	if err != nil {
		c.drop(path)
		return journal.Recovery{}, errors.New("cannot be read")
	}
	c.mu.Lock()
	hit, ok := c.histories[path]
	if !ok || !hit.current(info) {
		hit, ok = c.starts[path]
	}
	c.mu.Unlock()
	if ok && hit.current(info) {
		return hit.recovery, hit.err
	}
	f, err := c.open(path)
	if err != nil {
		return journal.Recovery{}, errors.New("cannot be read")
	}
	defer func() { _ = f.Close() }()
	recovery, err := journal.RecoverLogStart(f, startGroups)
	c.mu.Lock()
	c.starts[path] = cachedLog{size: info.Size(), modified: info.ModTime(), recovery: recovery, err: err}
	c.mu.Unlock()
	return recovery, err
}

// earlier returns what the chain and the history read of the earlier run's
// log at path (report.ForHistory). A file that cannot be read is not kept.
func (c *logCache) earlier(path string) (journal.Recovery, error) {
	info, err := c.stat(path)
	if err != nil {
		c.drop(path)
		return journal.Recovery{}, errors.New("cannot be read")
	}
	c.mu.Lock()
	hit, ok := c.histories[path]
	c.mu.Unlock()
	if ok && hit.current(info) {
		return hit.recovery, hit.err
	}
	recovery, err := c.whole(path)
	if errors.Is(err, errUnreadable) {
		return journal.Recovery{}, err
	}
	kept := cachedLog{size: info.Size(), modified: info.ModTime(), recovery: report.ForHistory(recovery, c.benchmark), err: err}
	c.mu.Lock()
	c.histories[path] = kept
	delete(c.starts, path)
	c.mu.Unlock()
	return kept.recovery, kept.err
}

// errUnreadable is a log file that cannot be read; the reason is not shown,
// since a reader of the page is not told the server's paths.
var errUnreadable = errors.New("cannot be read")

// whole reads and recovers the log at path, keeping nothing; earlier and
// tiedWhole keep what they need of it.
func (c *logCache) whole(path string) (journal.Recovery, error) {
	raw, err := c.readFile(path)
	if err != nil {
		return journal.Recovery{}, errUnreadable
	}
	return journal.RecoverLog(bytes.NewReader(raw))
}

// tiedWhole is whole for a log that ties for the chain's head, kept by its
// size and modification time. A file that cannot be read is not kept.
func (c *logCache) tiedWhole(path string) (journal.Recovery, error) {
	info, err := c.stat(path)
	if err != nil {
		c.drop(path)
		return journal.Recovery{}, errUnreadable
	}
	c.mu.Lock()
	hit, ok := c.tied[path]
	c.mu.Unlock()
	if ok && hit.current(info) {
		return hit.recovery, hit.err
	}
	recovery, err := c.whole(path)
	if errors.Is(err, errUnreadable) {
		return journal.Recovery{}, err
	}
	c.mu.Lock()
	c.tied[path] = cachedLog{size: info.Size(), modified: info.ModTime(), recovery: recovery, err: err}
	c.mu.Unlock()
	return recovery, err
}

// keepTied drops every whole log kept from dir except those in keep, the logs
// the latest request's tie read: a log the tie no longer reaches, or that
// became the chain's head, is not kept whole.
func (c *logCache) keepTied(dir string, keep map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for path := range c.tied {
		if filepath.Dir(path) == filepath.Clean(dir) && !keep[path] {
			delete(c.tied, path)
		}
	}
}

// drop forgets the log at path.
func (c *logCache) drop(path string) {
	c.mu.Lock()
	delete(c.starts, path)
	delete(c.histories, path)
	delete(c.tied, path)
	c.mu.Unlock()
}

// sweep forgets every log kept from dir that its listing no longer holds.
func (c *logCache) sweep(dir string, files []restart.LoggedFile) {
	listed := map[string]bool{}
	for _, f := range files {
		listed[filepath.Join(dir, f.Name)] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range []map[string]cachedLog{c.starts, c.histories, c.tied} {
		for path := range m {
			if filepath.Dir(path) == filepath.Clean(dir) && !listed[path] {
				delete(m, path)
			}
		}
	}
}

// retained is how many records the cache keeps of the log at path.
func (c *logCache) retained(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.starts[path].recovery.Entries) + len(c.histories[path].recovery.Entries) + len(c.tied[path].recovery.Entries)
}

// held reports whether the cache keeps anything of the log at path.
func (c *logCache) held(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, start := c.starts[path]
	_, earlier := c.histories[path]
	_, tied := c.tied[path]
	return start || earlier || tied
}

func view(history report.History, ledger report.CashLedger, envelopes []event.Envelope, benchmark string) (account, error) {
	date, err := report.LastDate(envelopes)
	if err != nil {
		return account{}, err
	}
	summary, err := report.SummarizeWithCash(history, ledger, envelopes, date, benchmark)
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
