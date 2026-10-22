package report

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// KindUnitFailed is a trading unit (the engine, the adapter or the reporter)
// that stopped and is failed: systemd never restarts the engine or the adapter
// (ADR 0027, the restart model), so a stop waits for the owner, and the page
// is how the owner learns of it within minutes (ADR 0019, "Alerting").
const KindUnitFailed Kind = "unit-failed"

// UnitFailedKey is the alert key of a failed unit: one page per unit until
// the unit is no longer failed, when the caller resolves and forgets it so the
// next failure pages anew.
func UnitFailedKey(unit string) string { return "unit-failed:" + unit }

// UnitManualRows names, by unit-name prefix, the manual row the owner reads.
var UnitManualRows = []struct{ prefix, row string }{
	{"ti-adapter-", `docs/manual/ibkr-adapter.md, "What can go wrong", the row "The adapter's unit failed"`},
	{"ti-engine@", `docs/manual/engine.md, "While it runs", the row "The engine's unit failed"`},
}

// UnitManualRow is the manual row for a failed unit; every other unit is
// explained in the alerts chapter.
func UnitManualRow(unit string) string {
	for _, r := range UnitManualRows {
		if strings.HasPrefix(unit, r.prefix) {
			return r.row
		}
	}
	return `docs/manual/alerts-and-dashboard.md, "The reporter itself", the row "A trading unit failed"`
}

// UnitFailedAlert is the urgent page for a failed unit: its name, the last
// line it wrote (reason, already cleaned by UnitFailedReason) and the manual
// row to read. It carries no account number, balance or secret.
func UnitFailedAlert(host, unit, reason string, at time.Time) Alert {
	return Alert{
		Key: UnitFailedKey(unit), Kind: KindUnitFailed, Severity: Urgent, AccountID: host, At: at,
		Title: fmt.Sprintf("%s: %s failed", host, unit),
		Body: fmt.Sprintf("The unit %s is failed as of %s. Its last journal line: %s\n"+
			"systemd does not restart the engine or the adapter: every stop is for you to read and every start is yours. Read %s, then `journalctl -u %s` on the server. "+
			"You are told once for this failure; the incident is resolved when the unit is no longer failed, and the next failure is told again.",
			unit, at.UTC().Format(time.RFC3339), reason, UnitManualRow(unit), unit),
	}
}

// maxUnitReason is the most of a journal line kept for a page.
const maxUnitReason = 300

var (
	reasonURL     = regexp.MustCompile(`https?://\S+`)
	reasonAccount = regexp.MustCompile(`\b[A-Z]{1,3}\d{6,}\b`)
)

// UnitFailedReason cleans a unit's last journal line for a page: one line, at
// most maxUnitReason characters, with every address, every IBKR-style account
// number and every value in secrets (those of four characters or more) taken
// out, since the channels are given no secret or account (the secrets file's
// own values are the ones a unit might echo). An empty line says so.
func UnitFailedReason(line string, secrets []string) string {
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return "(the unit wrote no journal line)"
	}
	for _, s := range secrets {
		if len(s) >= 4 {
			line = strings.ReplaceAll(line, s, "[secret]")
		}
	}
	line = reasonURL.ReplaceAllString(line, "[address]")
	line = reasonAccount.ReplaceAllString(line, "[account]")
	if r := []rune(line); len(r) > maxUnitReason {
		line = string(r[:maxUnitReason]) + "..."
	}
	return line
}

// WithFailedUnits adds the failed trading units to the account's standing
// (a, inForce from StateAtStartAlert): named in its text when something else
// is in force, and as the whole of it when nothing is, so a reporter that
// begins delivering to a channel tells it of a unit that failed before.
func WithFailedUnits(a Alert, inForce bool, failed []string) (Alert, bool) {
	if len(failed) == 0 {
		return a, inForce
	}
	rows := make([]string, len(failed))
	for i, unit := range failed {
		rows[i] = fmt.Sprintf("%s (read %s)", unit, UnitManualRow(unit))
	}
	line := "Failed trading unit(s), which systemd does not restart: " + strings.Join(rows, "; ") + "."
	if !inForce {
		a.Severity = Urgent
		a.Title = fmt.Sprintf("%s: state at reporter start: %s", a.AccountID, "Normal, a trading unit is failed")
		a.Body, a.PublicBody = line, line
		return a, true
	}
	a.Body += " " + line
	a.PublicBody += " " + line
	return a, true
}
