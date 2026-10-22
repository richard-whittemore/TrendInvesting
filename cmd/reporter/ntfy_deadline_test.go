package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// TestAMultipartNtfyMessageHasOneDeadlineForAllItsParts: the send timeout
// bounds one send to one channel, so a message posted in parts has one
// deadline for them all, not one per part. A message that cannot be sent
// within it fails the ntfy channel alone, and the next channel is reached
// without waiting for the rest of the parts (ADR 0019, "Alerting": one
// channel's fault must not silence another).
func TestAMultipartNtfyMessageHasOneDeadlineForAllItsParts(t *testing.T) {
	const perPart, timeout = 300 * time.Millisecond, time.Second
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(perPart):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	var paged atomic.Int32
	pd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paged.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer pd.Close()
	client := &http.Client{Timeout: timeout}
	senders := []sender{
		ntfySender{server: slow.URL, topic: "t", client: client},
		pagerDutySender{url: pd.URL, routingKey: "rk", client: client},
	}
	// Eight parts at 300ms each take 2.4s, each part within the timeout; the
	// bound below sits 700ms over the deadline and 700ms under the serial
	// time, so a slow machine does not fail it and one deadline per part would.
	a := report.Alert{Key: "k", Title: "joint: engine degraded", Body: strings.Repeat("x", 8*4096), Severity: report.Urgent}
	seen, sent, repeats := map[string]bool{}, map[string]time.Time{}, map[string]int{}
	begun := time.Now()
	err := deliver(senders, a, seen, sent, repeats, begun, time.Hour, func() error { return nil })
	took := time.Since(begun)
	if err == nil || !strings.Contains(err.Error(), "ntfy") {
		t.Errorf("a message not sent within the send timeout must fail the ntfy channel: %v", err)
	}
	if seen[channelKey("ntfy", "k")] {
		t.Error("a message cut off by the deadline was recorded as taken by ntfy")
	}
	if paged.Load() != 1 || !seen[channelKey("pagerduty", "k")] {
		t.Errorf("PagerDuty took the alert %d time(s); the ntfy deadline must not hold it back", paged.Load())
	}
	if took > timeout+700*time.Millisecond {
		t.Errorf("the delivery took %s: the multipart ntfy send overran its %s deadline", took, timeout)
	}
}
