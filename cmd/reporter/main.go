// Command reporter reads one account's journal and delivers what a person
// needs to know: alerts for every refusal, Degraded or Halted state,
// reconciliation that did not match, withheld decision, missing input, order
// not acknowledged in time, venue report the engine did not read, lost
// heartbeat and journal it cannot read, each once per incident, and
// optionally the daily summary (docs/live-trading/plan.md section 3; ADR
// 0019, "Alerting"). A Degraded, Halted or reconciliation alert is sent
// again every -repeat-interval until the journal records the owner's
// acknowledgement of it. Each delivery channel is validated on its own: one
// whose secret is empty or malformed is set aside, named in the run's error,
// in an urgent alert on the other channels and in the daily summary, and the
// others still deliver. It only reads the journal. It writes its own state
// file of delivered incident keys and when each channel last took each and,
// with -clock-check, the host's clock state and its lock, shared by every
// reporter on the host. Every rule lives in internal/report; this is
// composition.
//
// It runs once per invocation, from a timer. The heartbeat it checks is the
// journal file's modification time, with silence counted in NYSE sessions; a
// reporter on the engine's own host cannot be the external dead-man's switch
// ADR 0019 asks for, which is a separate service.
//
// While the host's maintenance window is open (-maintenance; internal/maintenance)
// the reporter still runs and still pings the outside heartbeat, and records,
// without delivering, a failed unit and a logged-out Gateway; the first
// firing after it closes judges them afresh and pages what is still wrong. Capital-safety alerts are never held
// (internal/report, maintenance.go).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// The defaults of -repeat-interval and -ack-within, which the reporter's
// timer is set against (deploy/systemd/ti-reporter@.timer).
const (
	defaultRepeatInterval = 30 * time.Minute
	defaultAckWithin      = 10 * time.Minute
)

func main() {
	if err := run(os.Args[1:], os.Getenv, time.Now, os.Stdout, http.DefaultClient); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// delivered is the reporter's state file: the incident keys already sent,
// per channel, when each channel last took each, which times a repeat, how
// many repeats each channel has taken, which names each repeat, and when each
// channel was first seen with no record, before which the log's events are
// history to it (see channelBaseline).
type delivered struct {
	Keys      []string             `json:"keys"`
	Sent      map[string]time.Time `json:"sent,omitempty"`
	Repeats   map[string]int       `json:"repeats,omitempty"`
	Baselines map[string]time.Time `json:"baselines,omitempty"`
}

// channelBaseline is the time before which the log's events are history to
// a channel with no record, given the start of the run that first sees it. A
// new or lost state file, or a newly named channel, begins at the start of
// the run, so the log's earlier history is not replayed to it (ADR 0019,
// "Alerting": each incident once). What is still in force is not history:
// the channel is told the account's standing once (tellStateAtStart) and
// every open alert, repeating and not acknowledged, raised before it
// (sendersFor); events after it are sent as usual. A test replaces it.
var channelBaseline = func(start time.Time) time.Time { return start }

func run(args []string, getenv func(string) string, now func() time.Time, out io.Writer, client *http.Client) error {
	if len(args) > 0 && args[0] == "notify-unit-failed" {
		return notifyUnitFailed(args[1:], getenv, now, out, client)
	}
	flags := flag.NewFlagSet("reporter", flag.ContinueOnError)
	flags.SetOutput(out)
	journalPath := flags.String("journal", "", "path to the account's journal (read only)")
	statePath := flags.String("state", "", "path to the reporter's own state file of delivered alerts")
	channels := flags.String("channel", "stdout", "comma-separated channels: stdout (terminal echo, never a delivery), ntfy, pagerduty, pushover, github-issues (one issue per alert, closed when it resolves)")
	benchmark := flags.String("benchmark", "SPY", "benchmark instrument for the equity comparison")
	date := flags.String("date", "", "summary date, YYYY-MM-DD, sent once the log records that day's session closed (default: the latest closed session)")
	summary := flags.Bool("summary", false, "also deliver the daily summary")
	sessions := flags.Int("heartbeat-sessions", 2, "NYSE sessions that may close with nothing written to the journal before a heartbeat alert fires (at least 2); weekends and holidays close none")
	accountName := flags.String("account", "", "account name for alerts when the journal cannot name its own (default: the journal file's name)")
	sendTimeout := flags.Duration("send-timeout", 30*time.Second, "longest one send to one channel may take before it counts as failed")
	repeat := flags.Duration("repeat-interval", defaultRepeatInterval, "how often a Degraded, Halted or reconciliation alert is sent again until the journal records its acknowledgement (the owner's setting, ADR 0019)")
	secretsFile := flags.String("secrets-file", "", "KEY=VALUE file of channel secrets, read for any not set in the environment (under systemd: %d/secrets, the credential file)")
	ackWithin := flags.Duration("ack-within", defaultAckWithin, "longest an order to be sent may go without the venue acknowledging it, from its commit, before an alert fires")
	clockCheck := flags.String("clock-check", "", "command that checks the server's clock (deploy/libexec/clock-check.sh): run after the incident alerts at every firing, a failure two checks in a row pages once for the host, a passing check resolves it, and the daily summary shows the last check")
	clockState := flags.String("clock-state", "", "the host's clock state, shared by every reporter instance on the host (default: host-clock.json beside -state)")
	gatewayAdapters := flags.String("gateway-adapters", "", "comma-separated adapter units (ti-adapter-<instance>.service) whose journal shows a sustained loss of IBKR (codes 1100/2110) for the Gateway check")
	gatewayState := flags.String("gateway-state", "", "the host's Gateway state (default: host-gateway.json beside -state)")
	// Deprecated and ignored: an earlier build's unit file passes it, and a binary
	// installed alone (ti-release installs no unit files) must still start.
	_ = flags.String("gateway-marker", "", "deprecated, ignored")
	gatewayOwner := flags.String("gateway-owner", "", "the account whose reporter owns the Gateway check, which reads the ti-gateway and -gateway-adapters journals with -journalctl after the incident alerts: logged out for more than 10 minutes outside the Gateway's restart windows pages once for the host, hourly reminders follow, and a completed login resolves it; every other instance skips it, so three accounts do not page three times")
	cashLedger := flags.String("cash-ledger", "", "the Flex reader's ledger of the account's deposits and withdrawals (/var/lib/trend-investing/flex/<instance>/cash-movements.jsonl), which the daily summary's SPY shadow account mirrors, and beside which the reader's costs.jsonl gives the summary its costs line, accepted only from the flex/<instance> folder of the engine directory the journal is in; without it, or before the reader has written it, the summary says the comparison assumes none")
	units := flags.String("units", "", "comma-separated trading units to check with systemctl show at the start of every firing (ti-engine@<instance>.service,ti-adapter-<instance>.service): a failed unit is paged once per failure episode if the OnFailure hook has not, a failed unit is named in the state-at-start page, and the page of a unit no longer failed is resolved")
	resolveUnits := flags.String("resolve-units", "", "comma-separated units (the data jobs, ti-data.service and ti-data@<account>.service) that systemctl show samples only to end their failure episode once a run has ended well: the reporter pages one once only when its OnFailure hook could not, never reminds or names it, and it never holds back the heartbeat; only a data job is accepted, and not a unit also in -units")
	selfUnit := flags.String("self-unit", "", "this reporter's own unit (ti-reporter@<instance>.service): its failure page is resolved by the next firing that finishes without a fault")
	unitState := flags.String("unit-state", "", "the host's record of failed-unit pages, shared with the OnFailure hook (default: unit-failures.json beside -state)")
	systemctl := flags.String("systemctl", "systemctl", "the systemctl that answers -units")
	runEnv := flags.String("run-env", "", "the engine's run.env (/var/lib/trend-investing/engines/<instance>/run.env): while it is missing or empty and nothing shows a run (no *.log in its folder, no <account>.ran marker beside -state; -account is required with it) the firing logs that it is waiting for the account's first run and skips only the journal step; every other check still runs, and the firing pings the outside heartbeat only when nothing else is wrong. With any sign of a run it is an urgent alert and a failed firing")
	journalctl := flags.String("journalctl", "journalctl", "the journal reader that gives a failed unit's last line")
	maintenanceFile := flags.String("maintenance", "", "the host's maintenance window marker (default: /run/ti-maintenance/maintenance.json, written by ti-maintenance): while a window is open, failed units and a logged-out Gateway are recorded and not paged, the heartbeat is still pinged, and the first firing after the window closes pages what is still wrong; capital-safety alerts are never held")
	err := flags.Parse(args)
	if err != nil {
		return err
	}
	if *journalPath == "" || *statePath == "" {
		return errors.New("reporter: -journal and -state are required")
	}
	if *gatewayOwner != "" && *accountName == "" {
		return errors.New("reporter: -gateway-owner needs -account: one account's reporter owns the host's Gateway check")
	}
	if *runEnv != "" && *accountName == "" {
		return errors.New("reporter: -run-env needs -account: the first-run marker is the account's own, named by it, and a name taken from the state file could be shared by two accounts")
	}
	if *sessions < 2 {
		return errors.New("reporter: -heartbeat-sessions must be at least 2: a session's close is counted before the engine writes that session's records, so 1 would page on every trading day")
	}
	if *ackWithin <= 0 {
		return errors.New("reporter: -ack-within must be positive: an order is acknowledged some time after its commit, so a bound of zero would page on every order")
	}
	if *sendTimeout <= 0 {
		return errors.New("reporter: -send-timeout must be positive: a send with no bound can hold the run forever, and with it every later alert")
	}
	if *repeat <= 0 {
		return errors.New("reporter: -repeat-interval must be positive: an alert repeated on every run would page without pause")
	}
	tradingUnits := splitUnits(*units)
	if err := checkTradingUnits("-units", tradingUnits); err != nil {
		return err
	}
	resolveOnly := splitUnits(*resolveUnits)
	if err := checkResolveUnits(resolveOnly, tradingUnits); err != nil {
		return err
	}
	if *selfUnit != "" {
		if err := checkTradingUnits("-self-unit", []string{*selfUnit}); err != nil {
			return err
		}
	}
	// Every send is bounded, so a stalled channel fails as that channel's
	// failure, is retried next run, and never holds back the other alerts or
	// the state file.
	bounded := *client
	bounded.Timeout = *sendTimeout
	// A fault in the secrets file or in one channel's secrets sets aside only
	// what it touches: every other channel still delivers, and the run fails
	// naming each fault (ADR 0019, "Alerting").
	var failures []error
	if *secretsFile != "" {
		var secretsErr error
		if getenv, secretsErr = withSecretsFile(getenv, *secretsFile); secretsErr != nil {
			failures = append(failures, secretsErr)
		}
	}
	// run.env missing or empty means no settings, and so no log path: the
	// journal step alone is skipped for it. Whether that is a quiet wait or a
	// fault is decided once the state is loaded (runEvidence). Every other
	// step of the firing is unchanged (ADR 0019, "Alerting").
	runEnvEmpty := false
	if *runEnv != "" {
		empty, err := runEnvIsEmpty(*runEnv)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		runEnvEmpty = empty
	}
	senders, setAside, err := buildSenders(*channels, getenv, &bounded, out)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, f := range setAside {
		failures = append(failures, f.err)
	}
	withRedaction(senders, secretValues(getenv, *secretsFile))
	// systemd is asked once for the trading units, at the start of the firing,
	// before anything is delivered (the data job is asked again, under the
	// unit-failure record's lock, by unitStep): a unit that fails while the firing runs is paged by its
	// hook, and its page is newer than this sample, so it is never resolved
	// on the sample's word (ADR 0019, "Alerting").
	sampledAt := now()
	unitSamples, unitsErr := sampleUnits(*systemctl, tradingUnits)
	if unitsErr != nil {
		failures = append(failures, unitsErr)
	}
	failedNow := map[string]bool{}
	for _, unit := range tradingUnits {
		failedNow[unit] = unitSamples[unit].failed
	}
	seen, sent, repeats, baselines, err := loadState(*statePath)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	// A maintenance window is open for this firing only if it was open when
	// the firing started and is still open at the moment of each decision it
	// holds (windowNow, asked at that moment with the clock at that moment):
	// one that ends mid-firing holds nothing from then on.
	windowFile := maintenancePath(*maintenanceFile)
	windowUntil, windowProblem, windowPrint := windowRead(windowFile, sampledAt)
	windowOpen := !windowUntil.IsZero()
	windowNow := func() time.Time {
		if !windowOpen {
			return time.Time{}
		}
		return windowEnd(windowFile, now())
	}
	// A marker that is there and is not a window is said once, never paged.
	if windowProblem != "" && !seen[channelKey("marker-note", windowPrint)] {
		seen[channelKey("marker-note", windowPrint)] = true
		_, _ = fmt.Fprintln(out, "reporter: the maintenance marker "+windowProblem+"; it counts as no window, so nothing is held (said once for this content)")
	}
	// The firing waits only on proof that the account has never run: nothing
	// shows a run but the missing settings file. Any sign of one is a fault.
	var lostRunEnv error
	marker := ranMarkerPath(*statePath, *accountName)
	waiting := false
	if runEnvEmpty {
		if sign := runEvidence(filepath.Dir(*runEnv), marker); sign != "" {
			lostRunEnv = fmt.Errorf("reporter: the run's settings file is missing but a run exists: %s is missing or empty and %s", *runEnv, sign)
		} else {
			waiting = true
			_, _ = fmt.Fprintln(out, "reporter: waiting for the account's first run: "+*runEnv+" is missing or empty (start the engine once)")
		}
	}
	start := now()
	if pruneReceipts(seen, sent, repeats, start) {
		if err := saveState(*statePath, seen, sent, repeats, baselines); err != nil {
			failures = append(failures, err)
		}
	}
	// A new baseline is on disk before any delivery starts, so a run stopped
	// midway (systemd stops one at its TimeoutStartSec) cannot leave the next
	// run to take a later one and drop what the log recorded between the two.
	// One that cannot be saved is dropped: the channel is then sent the whole
	// log, which fails noisy, never silent (ADR 0019, "Alerting").
	if added := baseline(baselines, seen, senders, setAside, channelBaseline(start)); len(added) > 0 {
		if err := saveState(*statePath, seen, sent, repeats, baselines); err != nil {
			failures = append(failures, err)
			for _, name := range added {
				delete(baselines, name)
			}
		}
	}
	label := *accountName
	if label == "" {
		label = filepath.Base(*journalPath)
	}
	var (
		info      os.FileInfo
		statErr   error
		history   report.History
		envelopes []event.Envelope
		account   string
		alerts    []report.Alert
		refused   error
	)
	switch {
	case waiting:
	case lostRunEnv != nil:
		refused = lostRunEnv
	default:
		info, statErr = os.Stat(*journalPath)
		history, envelopes, account, alerts, refused = journalReader(*journalPath, now(), *ackWithin, seen)
		if account != "" {
			label = account
		}
		if refused == nil && *runEnv != "" {
			if err := writeRanMarker(marker); err != nil {
				failures = append(failures, err)
			}
		}
	}
	examined := false
	var forgotten []string
	withIssueEvidence(senders, *journalPath)
	withIssueRecords(senders, repeats)
	if !waiting && refused == nil {
		var restored, current string
		restored, current, examined = restoredLogNewer(*journalPath)
		if restored != "" {
			alerts = append(alerts, report.RestoredLogNewerAlert(account, restored, current, now()))
		}
	}
	if !waiting && refused == nil && history.Broken == nil {
		// A whole chain clears every break delivered for the account, so a
		// break after a repair is a new incident (ADR 0019, "Alerting":
		// once per incident).
		forget(report.HistoryUnavailableKeyPrefix(account), seen, sent, repeats)
		forgotten = append(forgotten, report.HistoryUnavailableKeyPrefix(account))
	}
	if !waiting && refused == nil && examined && !slices.ContainsFunc(alerts, func(a report.Alert) bool { return a.Kind == report.KindRestoredLogNewer }) {
		// A folder examined with no log newer than the current run clears
		// the restored-log alerts delivered for the account, so the same log restored wrongly again
		// is a new incident (ADR 0019, "Alerting": once per incident).
		forget(report.RestoredLogNewerKeyPrefix(account), seen, sent, repeats)
		forgotten = append(forgotten, report.RestoredLogNewerKeyPrefix(account))
	}
	// The heartbeat is checked first so a refused journal's alert can say
	// whether liveness is monitored.
	var heartbeat []report.Alert
	unchecked := ""
	switch {
	case waiting:
	case lostRunEnv != nil:
		unchecked = "the settings file that names the journal is missing"
	case statErr != nil:
		unchecked = "the journal file has no modification time to check"
	default:
		a, lost, err := report.HeartbeatAlert(label, info.ModTime(), now(), *sessions)
		if err != nil {
			// A check the calendar cannot answer is as silent as the lost
			// heartbeat it would have caught, so it reaches a person too.
			unchecked = "the calendar cannot answer: " + err.Error()
			failures = append(failures, err)
			heartbeat = append(heartbeat, report.HeartbeatUncheckedAlert(label, err, now()))
		} else if lost {
			heartbeat = append(heartbeat, a)
		}
	}
	if refused != nil {
		// A journal that cannot be read must not silence the one path that
		// reaches a person (ADR 0019, "Alerting"): the failure is itself an
		// alert, the heartbeat is checked where it can be, and the error is
		// returned.
		failures = append(failures, refused)
		if lostRunEnv != nil && errors.Is(refused, lostRunEnv) {
			alerts = []report.Alert{report.RunEnvMissingAlert(label, refused, unchecked, now())}
		} else {
			alerts = []report.Alert{report.JournalRefusedAlert(label, refused, unchecked, now())}
		}
	}
	alerts = append(alerts, heartbeat...)
	for _, f := range setAside {
		alerts = append(alerts, report.ChannelRefusedAlert(label, f.channel, f.err, now()))
	}
	// Each delivery is saved as soon as a channel takes it, so a run stopped
	// before it ends (systemd stops one at its TimeoutStartSec) makes the
	// next run send again at most the one alert on the one channel that had
	// taken it but was not yet saved: delivery is at-least-once, PagerDuty
	// folds the repeat by its key and ntfy posts it twice (ADR 0019,
	// "Alerting"). The state is saved once more at the end, so the
	// file exists after a run that delivered nothing.
	at := now()
	record := func() error { return saveState(*statePath, seen, sent, repeats, baselines) }
	unitStatePath := *unitState
	if unitStatePath == "" {
		unitStatePath = filepath.Join(filepath.Dir(*statePath), "unit-failures.json")
	}
	// Every alert reaches the channels that page before any reaches an issue
	// tracker, so a slow or failing tracker never holds back a page (ADR
	// 0019, "Alerting": one channel's fault must not silence another).
	paging, trackers := splitTrackers(senders)
	// A failed trading unit is part of the standing told to a new channel,
	// unless a window holds it: it is then paged at the window's close.
	stateAtStartUnits := sortedUnits(tradingUnits, failedNow)
	if !windowNow().IsZero() {
		stateAtStartUnits = nil
	}
	for _, group := range [][]sender{paging, trackers} {
		if len(group) == 0 {
			continue
		}
		if !waiting && refused == nil {
			if err := tellStateAtStart(group, envelopes, alerts, baselines, seen, sent, repeats, at, *repeat, stateAtStartUnits, record); err != nil {
				failures = append(failures, err)
			}
		}
		for _, a := range report.Fresh(alerts, nil) {
			if heldDuring(a, windowNow) {
				holdAlert(sent, windowNow(), at, a.Key)
				continue
			}
			current, starting := sendersFor(group, a, baselines, seen)
			if err := deliver(current, a, seen, sent, repeats, at, *repeat, record); err != nil {
				failures = append(failures, err)
			}
			if len(starting) > 0 {
				marked := a
				marked.Title = report.StateAtStartTitle + a.Title
				if err := deliver(starting, marked, seen, sent, repeats, at, *repeat, record); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	// A repeating alert the owner has acknowledged closes its issue.
	if err := closeAcknowledged(senders, alerts, seen); err != nil {
		failures = append(failures, err)
	}
	// A forgotten alert closes its issue, so its next incident is a new one.
	if err := closeForgotten(senders, forgotten, seen, sent, repeats); err != nil {
		failures = append(failures, err)
	}
	if err := saveState(*statePath, seen, sent, repeats, baselines); err != nil {
		failures = append(failures, err)
	}
	// A failed trading unit is paged by the OnFailure hook at the moment it
	// fails; this firing pages a failure episode the hook missed and resolves
	// the page of a unit not failed at the sample. A systemctl that could not
	// be run checked nothing, so no page is resolved on its word.
	if unitsErr == nil && len(tradingUnits)+len(resolveOnly) > 0 {
		soft, hard := unitStep(unitStatePath, tradingUnits, resolveOnly, unitSamples, sampledAt, *journalctl, secretValues(getenv, *secretsFile), senders, setAside, now(), *repeat, windowNow, func(unit string) (unitSample, error) {
			samples, err := sampleUnitsFor(*systemctl, []string{unit}, "the trading units were checked; only the data job's resolve and backup page are skipped at this firing")
			return samples[unit], err
		})
		if soft != nil {
			_, _ = fmt.Fprintln(out, "reporter: the data job's page or resolve could not be completed and is retried at the next firing: "+soft.Error())
		}
		if hard != nil {
			failures = append(failures, hard)
		}
	}
	// The clock check comes after every incident alert is sent, so a slow or
	// hung check never holds one back (ADR 0019, "Alerting"; its bounds are in
	// clock.go). The clock is the host's: its state is shared by every
	// reporter instance on the host.
	var clock *report.ClockCheck
	if *clockCheck != "" {
		path := *clockState
		if path == "" {
			path = filepath.Join(filepath.Dir(*statePath), "host-clock.json")
		}
		c, err := clockStep(path, *clockCheck, hostName(), senders, setAside, now, *repeat)
		clock = c
		if err != nil {
			failures = append(failures, err)
		}
	}
	// The Gateway is the host's, so one instance owns its check (ADR 0019,
	// "Alerting"); like the clock check it follows the incident alerts.
	if *gatewayOwner != "" && *accountName == *gatewayOwner {
		path := *gatewayState
		if path == "" {
			path = filepath.Join(filepath.Dir(*statePath), "host-gateway.json")
		}
		if sched, err := gatewayScheduleFrom(getenv); err != nil {
			failures = append(failures, err)
		} else if err := gatewayStep(path, gatewayJournalChecker(*journalctl, splitUnits(*gatewayAdapters)), gatewayUnitSampler(*systemctl), hostName(), sched, windowNow, senders, setAside, now); err != nil {
			failures = append(failures, err)
		}
	}
	// A summary that cannot be built is reported, but never holds back the
	// incident alerts: they are what ADR 0019 requires to reach a person.
	// The notice that the summary has no channel is the account's, keyed by
	// the account the journal names, so it is said only when the journal
	// reads, as the summary itself is sent only then.
	if *summary && !waiting && refused == nil {
		if err := saySummaryRoute(senders, setAside, account, out, seen, sent, repeats, at, *repeat, record); err != nil {
			failures = append(failures, err)
		}
	}
	if *summary && !waiting && refused == nil {
		// The ledger is taken only from the account's own flex/<instance>
		// folder, the instance of the engine directory the journal is in
		// (report.LoadCashLedgerFor).
		ledger := report.LoadCashLedgerFor(*cashLedger, filepath.Base(filepath.Dir(*journalPath)), os.ReadFile)
		costs := report.LoadCostLedgerFor(*cashLedger, filepath.Base(filepath.Dir(*journalPath)), os.ReadFile)
		a, due, err := summaryAlert(history, ledger, costs, envelopes, *date, *benchmark, clock)
		switch {
		case err != nil:
			failures = append(failures, fmt.Errorf("reporter: the daily summary was not sent: %w", err))
		case due:
			a.Body += notDelivering(setAside)
			if err := deliver(senders, a, seen, sent, repeats, at, *repeat, record); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := saveState(*statePath, seen, sent, repeats, baselines); err != nil {
		failures = append(failures, err)
	}
	// A firing that finished without a fault resolves this reporter's own
	// failure page (the OnFailure hook sent it when an earlier firing failed).
	if *selfUnit != "" && len(failures) == 0 {
		if err := clearUnit(unitStatePath, *selfUnit, getenv("INVOCATION_ID"), senders, setAside, at); err != nil {
			failures = append(failures, err)
		}
	}
	// The outside heartbeat is last, and only for a firing that finished
	// without a fault and found no trading unit failed: a ping means every
	// step ran and the engine and the reporter are healthy (ADR 0019,
	// "Alerting": the outside heartbeat). A ping that fails is logged by
	// pingHeartbeat and never fails the run.
	if err := errors.Join(failures...); err != nil {
		return err
	}
	// A window still pings: its failed units are recorded and page when it
	// closes, and the outside check must not page the install's own noise.
	for _, failed := range failedNow {
		if failed && windowNow().IsZero() {
			return nil
		}
	}
	pingHeartbeat(getenv, &bounded, out)
	return nil
}

// baseline gives every delivery channel with no record in the state file the
// baseline time: a channel that has no baseline yet and has taken nothing,
// whether the state file is new or the channel was just named. A channel set
// aside is baselined too, so what it missed while set aside reaches it once
// it works. A channel already in the state file keeps what it has. It
// returns the channels it baselined.
func baseline(baselines map[string]time.Time, seen map[string]bool, senders []sender, setAside []channelFault, at time.Time) (added []string) {
	names := make([]string, 0, len(senders)+len(setAside))
	for _, s := range senders {
		if _, echo := s.(echoOnly); !echo {
			names = append(names, s.name())
		}
	}
	for _, f := range setAside {
		names = append(names, f.channel)
	}
	for _, name := range names {
		if _, ok := baselines[name]; ok || hasKeys(seen, name) {
			continue
		}
		baselines[name] = at
		added = append(added, name)
	}
	return added
}

// hasKeys reports whether the channel has taken any alert.
func hasKeys(seen map[string]bool, channel string) bool {
	for k := range seen {
		if strings.HasPrefix(k, channel+"|") {
			return true
		}
	}
	return false
}

// sendersFor returns the senders alert a may go to now. To a channel whose
// baseline is after the event the alert reports, the alert is history and is
// not sent, unless it is still open (repeating and not acknowledged): then it
// is in force, not history, and is in starting, to be sent marked as the
// state at reporter start until the channel has taken it, and in current
// after that, so it repeats as usual (ADR 0019, "Alerting"). An alert with no
// event time is current.
func sendersFor(senders []sender, a report.Alert, baselines map[string]time.Time, seen map[string]bool) (current, starting []sender) {
	for _, s := range senders {
		if _, echo := s.(echoOnly); !echo {
			if b, ok := baselines[s.name()]; ok && !a.At.IsZero() && a.At.Before(b) {
				switch {
				case !a.Repeat || a.Acknowledged:
				case seen[channelKey(s.name(), a.Key)]:
					current = append(current, s)
				default:
					starting = append(starting, s)
				}
				continue
			}
		}
		current = append(current, s)
	}
	return current, starting
}

// tellStateAtStart sends the account's standing (report.StateAtStartAlert)
// once to every channel that has a baseline after the log's first event,
// that is, one the log's earlier events are held back from as history, and
// has not been told it: what is in force must reach the owner all the same
// (ADR 0019, "Alerting"). A channel such an account has nothing in force for
// is recorded as told, so an incident that begins later reaches it as itself
// alone.
func tellStateAtStart(senders []sender, envelopes []event.Envelope, alerts []report.Alert, baselines map[string]time.Time, seen map[string]bool, sent map[string]time.Time, repeats map[string]int, at time.Time, every time.Duration, failedUnits []string, record func() error) error {
	var first time.Time
	for _, e := range envelopes {
		if first.IsZero() || e.EventTime.Before(first) {
			first = e.EventTime
		}
	}
	a, inForce, err := report.StateAtStartAlert(envelopes, alerts)
	if err != nil {
		return err
	}
	// A trading unit that systemd shows failed is part of what is in force.
	a, inForce = report.WithFailedUnits(a, inForce, failedUnits)
	var due []sender
	for _, s := range senders {
		if _, echo := s.(echoOnly); echo {
			continue
		}
		b, ok := baselines[s.name()]
		if !ok || b.IsZero() || !first.Before(b) || seen[channelKey(s.name(), a.Key)] {
			continue
		}
		due = append(due, s)
	}
	if len(due) == 0 {
		return nil
	}
	if !inForce {
		for _, s := range due {
			seen[channelKey(s.name(), a.Key)] = true
		}
		return record()
	}
	return deliver(due, a, seen, sent, repeats, at, every, record)
}

// sortedUnits are the units of list that failed, in list order.
func sortedUnits(list []string, failed map[string]bool) []string {
	var out []string
	for _, u := range list {
		if failed[u] {
			out = append(out, u)
		}
	}
	return out
}

// summaryNoChannelPrefix begins the key of the notice that the daily summary
// has no channel.
const summaryNoChannelPrefix = "summary-no-channel:"

// saySummaryRoute tells the owner, once, on the incident channels, that the
// daily summary is not sent because no channel takes it: it carries
// balances, which go only to the owner's own ntfy server. The owner's only
// eyes are the channels, so the notice is a routine message on them, not a
// line in the unit's output alone (ADR 0019, "Alerting"); it carries no
// balance, and is shown in the unit's output (out, or the stdout echo when
// named) while no channel has it yet. A server of the owner's own that takes
// the summary clears the notice, so it returns if the server is later left
// out or set aside: it is resolved on each working PagerDuty channel that
// took it, since an incident acknowledged on the phone takes no further
// triggers under its key, and forgotten channel by channel, keeping the
// record of a PagerDuty channel set aside or whose resolve failed so the
// resolve is sent in a later run (ADR 0019, "Alerting"; as clockStep does).
func saySummaryRoute(senders []sender, setAside []channelFault, account string, out io.Writer, seen map[string]bool, sent map[string]time.Time, repeats map[string]int, at time.Time, every time.Duration, record func() error) error {
	for _, s := range senders {
		if t, ok := s.(summaryTaker); ok && t.takesSummary() {
			return clearSummaryNotice(senders, setAside, seen, sent, repeats, at)
		}
	}
	a := report.Alert{
		Key: summaryNoChannelPrefix + account, Kind: kindSummaryNoChannel, Severity: report.Routine, AccountID: account,
		Title: account + ": the daily summary has no channel: name your own ntfy server",
		Body: "Incident alerts are unaffected, but the daily summary is not sent. It carries your balances, which go only to your own ntfy server (TI_NTFY_SERVER), " +
			"never to PagerDuty, Pushover or the public ntfy.sh relay, and no such server is named, or the one named is set aside (the channel alert says why). " +
			"Set one up: docs/manual/server.md, \"Your own ntfy server\"; until then read the summary on the dashboard. This is said once.",
	}
	echoed := slices.ContainsFunc(senders, func(s sender) bool { _, echo := s.(echoOnly); return echo })
	if !echoed && !deliveredAnywhere(seen, a.Key) {
		_, _ = fmt.Fprintln(out, "reporter: "+a.Body)
	}
	return deliver(senders, a, seen, sent, repeats, at, every, record)
}

// clearSummaryNotice resolves and forgets every notice that the daily
// summary had no channel, channel by channel (see saySummaryRoute).
func clearSummaryNotice(senders []sender, setAside []channelFault, seen map[string]bool, sent map[string]time.Time, repeats map[string]int, at time.Time) error {
	keys := map[string]bool{}
	for k := range seen {
		if _, key, ok := strings.Cut(k, "|"); ok && strings.HasPrefix(key, summaryNoChannelPrefix) {
			keys[key] = true
		}
	}
	var failures []error
	for key := range keys {
		failed, err := resolveAll(senders, key, seen, at)
		if err != nil {
			failures = append(failures, err)
		}
		keep := map[string]bool{}
		for _, f := range setAside {
			keep[f.channel] = f.channel == "pagerduty"
		}
		for _, name := range failed {
			keep[name] = true
		}
		forgetAlert(key, keep, seen, sent, repeats)
	}
	return errors.Join(failures...)
}

// readJournal reads and verifies the journal and returns the history of the
// runs before it, its envelopes, its account and its alerts, including every
// venue report the engine did not read and every order to be sent not
// acknowledged within ackWithin of its commit by now, or the reason it was
// refused. Only the summary's equity comparison reads the earlier runs, so a
// broken chain of them refuses nothing: it is one more alert, delivered once
// (ADR 0019, "Alerting").
// journalReader reads the journal for a firing; a test wraps it.
var journalReader = readJournal

func readJournal(path string, now time.Time, ackWithin time.Duration, seen map[string]bool) (history report.History, envelopes []event.Envelope, account string, alerts []report.Alert, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return report.History{}, nil, "", nil, fmt.Errorf("reporter: read journal: %w", err)
	}
	_, entries, err := report.LoadEntries(raw)
	if err != nil {
		return report.History{}, nil, "", nil, err
	}
	envelopes = make([]event.Envelope, len(entries))
	for i, e := range entries {
		envelopes[i] = e.Envelope
	}
	account, strategy, err := report.AccountOf(envelopes)
	if err != nil {
		return report.History{}, nil, "", nil, err
	}
	alerts, err = report.Alerts(envelopes)
	if err != nil {
		return report.History{}, nil, account, nil, err
	}
	orders, unread, err := report.OrderLifecycles(entries, strategy)
	if err != nil {
		return report.History{}, nil, account, nil, err
	}
	alerts = append(alerts, report.UnreadReportAlerts(account, unread)...)
	alerts = append(alerts, report.UnacknowledgedAlerts(orders, now, ackWithin, func(key string) bool { return deliveredAnywhere(seen, key) })...)
	history = report.EarlierRunsOf(path, raw, recoverLogFile)
	if history.Broken != nil {
		alerts = append(alerts, report.HistoryUnavailableAlert(account, history, now))
	}
	return history, envelopes, account, alerts, nil
}

// restoredLogNewer returns a log of the journal's directory that an approval
// names as a previous log but that is newer by modification time than the
// chain's head, and that head (restart.ChainHead, the rule the dashboard
// finds the current run by), or "" when there is none. The engine finds the
// latest run by modification time, so it would take such a log, restored
// without its time, for the latest run at its next start. Only the logs'
// first records are read. A directory that cannot be listed or read here
// warns of nothing and is not examined: the journal itself was read, and its
// alerts stand. examined is true only when every log's start was read, so
// that no restored log was found is known and not merely unseen.
func restoredLogNewer(path string) (restored, current string, examined bool) {
	if filepath.Ext(path) != ".log" {
		return "", "", false
	}
	dir := filepath.Dir(path)
	files, err := journalquery.ListLogs(dir, ".log")
	if err != nil {
		return "", "", false
	}
	open := func(name string, groups int) (journal.Recovery, error) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return journal.Recovery{}, err
		}
		defer func() { _ = f.Close() }()
		if groups < 0 {
			return journal.RecoverLog(f)
		}
		return journal.RecoverLogStart(f, groups)
	}
	head, newer, err := restart.ChainHead(files,
		func(name string) (journal.Recovery, error) { return open(name, 2) },
		func(name string) (journal.Recovery, error) { return open(name, -1) })
	if err != nil {
		return "", "", false
	}
	return newer, head, true
}

// forget removes from the delivery state every channel's record of the
// alerts whose keys begin with prefix, except an issue tracker's: its record
// is dropped once closeForgotten has closed the alert's issue.
func forget(prefix string, seen map[string]bool, sent map[string]time.Time, repeats map[string]int) {
	for k := range seen {
		if ch, key, ok := strings.Cut(k, "|"); ok && ch != issuesChannel && strings.HasPrefix(key, prefix) {
			delete(seen, k)
			delete(sent, k)
			delete(repeats, k)
		}
	}
}

// recoverLogFile recovers the durable log at path.
func recoverLogFile(path string) (journal.Recovery, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		// The reason reaches the alert channels, which are not told the
		// host's paths.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return journal.Recovery{}, err
	}
	return journal.RecoverLog(bytes.NewReader(raw))
}

// deliveredAnywhere reports whether any channel took the alert keyed key, so
// an owner who was told of an incident is told it cleared.
func deliveredAnywhere(seen map[string]bool, key string) bool {
	for k := range seen {
		if strings.HasSuffix(k, channelKey("", key)) {
			return true
		}
	}
	return false
}

// summaryAlert wraps a day's summary as a routine message keyed by account
// and date, so a rerun the same day does not resend it. A day is summarised
// only once the log records its session closed, since the day's decision and
// orders are written with that close: a summary sent earlier would be
// incomplete and, being keyed by the day, never replaced. With no date it is
// the latest closed session's day, and it is not yet due when none has
// closed. A stated date whose session has not closed is an error. A clock
// check run in this firing adds its time and result as the summary's last line.
// ledger is the Flex reader's record of the owner's transfers, which the
// equity comparison's shadow account mirrors; costs its record of
// commissions, fees and interest, which the costs line totals.
func summaryAlert(history report.History, ledger report.CashLedger, costs report.CostLedger, envelopes []event.Envelope, date, benchmark string, clock *report.ClockCheck) (report.Alert, bool, error) {
	if date == "" {
		last, ok := report.ClosedSessionDate(envelopes)
		if !ok {
			return report.Alert{}, false, nil
		}
		date = last
	}
	s, err := report.SummarizeWithCosts(history, ledger, costs, envelopes, date, benchmark)
	if err != nil {
		return report.Alert{}, false, err
	}
	if !report.SessionClosedOn(envelopes, date) {
		return report.Alert{}, false, fmt.Errorf("the log records no session closed on %s, so that day is not complete; it is sent once it is", date)
	}
	body := report.RenderText(s)
	if clock != nil {
		body += report.ClockSummaryLine(*clock) + "\n"
	}
	return report.Alert{
		Key: "summary:" + s.AccountID + ":" + s.Date, Kind: kindSummary, Severity: report.Routine, AccountID: s.AccountID,
		Title: fmt.Sprintf("%s: daily summary %s", s.AccountID, s.Date), Body: body,
	}, true, nil
}

// notDelivering is the daily summary's line naming each channel set aside
// this run, and why; empty when every channel named delivers.
func notDelivering(setAside []channelFault) string {
	if len(setAside) == 0 {
		return ""
	}
	names := make([]string, len(setAside))
	for i, f := range setAside {
		names[i] = fmt.Sprintf("%s (%s)", f.channel, f.err)
	}
	return "Channels not delivering: " + strings.Join(names, "; ") + "\n"
}

// loadState reads the delivered channel keys, when each was last sent and
// how many repeats each has taken. A key with no send time (a state file
// written before repeats) is due for its repeat at once, if it repeats; a key
// with no count has taken no repeat.
func loadState(path string) (seen map[string]bool, sent map[string]time.Time, repeats map[string]int, baselines map[string]time.Time, err error) {
	seen, sent, repeats, baselines = map[string]bool{}, map[string]time.Time{}, map[string]int{}, map[string]time.Time{}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return seen, sent, repeats, baselines, nil
	}
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("reporter: read state: %w", err)
	}
	var d delivered
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("reporter: the state file is unreadable and is not reset, since a new file forgets which alerts are open: %w", err)
	}
	for _, k := range d.Keys {
		seen[k] = true
	}
	for k, at := range d.Sent {
		sent[k] = at
	}
	for k, n := range d.Repeats {
		repeats[k] = n
	}
	for k, at := range d.Baselines {
		baselines[k] = at
	}
	return seen, sent, repeats, baselines, nil
}

// syncFile flushes a file to disk; a test replaces it to see a failure.
var syncFile = func(f *os.File) error { return f.Sync() }

// saveState replaces the state file atomically and durably: the new content
// is on disk before the rename, and the rename before it returns. Without
// that, a power cut can leave an empty state file, which loadState refuses,
// and every later run then fails.
func saveState(path string, seen map[string]bool, sent map[string]time.Time, repeats map[string]int, baselines map[string]time.Time) error {
	raw, err := json.Marshal(deliveredOf(seen, sent, repeats, baselines))
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

// deliveredOf is the state file's form of the delivered keys, their send
// times, repeat counts and channel baselines.
func deliveredOf(seen map[string]bool, sent map[string]time.Time, repeats map[string]int, baselines map[string]time.Time) delivered {
	d := delivered{Keys: make([]string, 0, len(seen)), Sent: sent, Repeats: repeats, Baselines: baselines}
	for k := range seen {
		d.Keys = append(d.Keys, k)
	}
	sort.Strings(d.Keys)
	return d
}

// writeAtomic replaces the file at path with raw, atomically and durably.
func writeAtomic(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".reporter-state-*")
	if err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("reporter: write state: %w", err)
	}
	if err := syncFile(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("reporter: write state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := syncFile(dir); err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	return nil
}

// maxRunEnv bounds what is read of run.env, a short file of settings.
const maxRunEnv = 64 << 10

// runEnvIsEmpty reports whether the engine's run.env is missing or holds
// nothing, by the rule that the wait is only for an account that has never
// run (docs/manual/server.md, "The reporter before the first run"). A file
// that exists but is not a regular file, is larger than maxRunEnv or cannot be
// read is a fault, not a wait. It is opened without blocking and its
// descriptor checked, so a FIFO swapped in after a path check cannot hold the
// firing.
func runEnvIsEmpty(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		// Absent means Lstat finds nothing at the path: a link to nothing is
		// present, and cannot be read.
		_, lerr := os.Lstat(path)
		switch {
		case errors.Is(lerr, fs.ErrNotExist):
			return true, nil
		case lerr != nil:
			return false, fmt.Errorf("reporter: %s cannot be read: %w", path, lerr)
		}
		return false, fmt.Errorf("reporter: %s is present but cannot be opened: %w", path, err)
	}
	if err != nil {
		return false, fmt.Errorf("reporter: %s cannot be read: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	switch {
	case err != nil:
		return false, fmt.Errorf("reporter: %s cannot be read: %w", path, err)
	case !info.Mode().IsRegular():
		return false, fmt.Errorf("reporter: %s is not a regular file", path)
	case info.Size() > maxRunEnv:
		return false, fmt.Errorf("reporter: %s is larger than %d bytes and is not read", path, maxRunEnv)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxRunEnv+1))
	if err != nil {
		return false, fmt.Errorf("reporter: %s cannot be read: %w", path, err)
	}
	if len(raw) > maxRunEnv {
		return false, fmt.Errorf("reporter: %s is larger than %d bytes and is not read", path, maxRunEnv)
	}
	return len(bytes.TrimSpace(raw)) == 0, nil
}

// ranMarkerPath is the account's durable record that a journal named by
// run.env has been read: <account>.ran beside the state file, with the same
// owner and mode. The account is -account, which -run-env requires, so two
// accounts never share a marker. The record is the account's own, it persists
// until an owner removes it, and it is read only to refuse a wait that would
// hide a run (ADR 0019, "Alerting"; docs/manual/server.md, "The reporter
// before the first run").
func ranMarkerPath(statePath, account string) string {
	return filepath.Join(filepath.Dir(statePath), account+".ran")
}

// writeRanMarker records the first successful read of a journal named by
// run.env. It is never removed by the program, and a stray file cannot stand
// in for it: it is written only after a real read (docs/manual/server.md,
// "The reporter before the first run").
func writeRanMarker(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := writeAtomic(path, []byte("a journal named by run.env has been read\n")); err != nil {
		return fmt.Errorf("reporter: write the first-run marker: %w", err)
	}
	return nil
}

// runEvidence names the first sign that a run has happened, or "" when there
// is none: the marker written by an earlier read of a journal, or a run log
// (*.log) in the engine's folder. A marker or a folder that cannot be examined
// is a sign, since it cannot be shown to hold none; an engine folder that does
// not exist at all is a fresh server, and shows none. The rule is that the
// wait is only for an account that has never run (docs/manual/server.md, "The
// reporter before the first run").
func runEvidence(engineDir, marker string) string {
	if _, err := os.Stat(marker); err == nil {
		return "the reporter has already read a journal for this account (" + filepath.Base(marker) + ")"
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "its first-run marker cannot be examined: " + err.Error()
	}
	entries, err := os.ReadDir(engineDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "its folder cannot be listed: " + err.Error()
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			return "the engine folder holds the run log " + e.Name()
		}
	}
	return ""
}
