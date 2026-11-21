package main

import (
	"path/filepath"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

const kindAccountChanged report.Kind = "account-changed"

// reporterHistory composes the verified chain with the reporter's notice
// (manual, "The history across restarts"). FollowChain checks the approved
// hash before account identity: only an intact log of a known different
// account is a boundary. Missing, altered or unknown logs keep their alert.
func reporterHistory(path string, raw []byte, account string, at time.Time) (report.History, *report.Alert) {
	previousAccount := ""
	history := report.EarlierRunsOf(path, raw, func(previous string) (journal.Recovery, error) {
		previousAccount = ""
		recovered, err := recoverLogFile(previous)
		if err == nil {
			envelopes := make([]event.Envelope, len(recovered.Entries))
			for i, entry := range recovered.Entries {
				envelopes[i] = entry.Envelope
			}
			previousAccount, _, _ = report.AccountOf(envelopes)
		}
		return recovered, err
	})
	broken := history.Broken
	if broken == nil {
		return history, nil
	}
	if broken.Cause != restart.CauseIdentity || broken.Previous == "" || previousAccount == "" || previousAccount == account {
		alert := report.HistoryUnavailableAlert(account, history, at)
		return history, &alert
	}
	// Keep the same-account runs already recovered, including any mode
	// boundary; the earlier account's records never join the comparison.
	history.Broken = nil
	return history, &report.Alert{
		Key:  "account-changed:" + account + ":" + broken.Run + ":" + broken.Previous,
		Kind: kindAccountChanged, Severity: report.Routine, AccountID: account, At: at,
		Title: account + ": account changed",
		Body:  "Run " + filepath.Base(broken.Run) + ": the equity comparison starts with this run, the first on this account.",
	}
}
