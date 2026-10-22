package main

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// posts is a channel that records each ntfy post's title and body and takes
// it.
type posts struct{ titles, bodies []string }

func (p *posts) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	p.titles = append(p.titles, req.Header.Get("Title"))
	p.bodies = append(p.bodies, string(body))
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
}

// TestAnNtfyMessageOverTheServersLimitIsSentInNumberedParts: ntfy takes a
// message of at most 4096 bytes (its message-size-limit), so a longer one,
// such as an engine state naming many instruments, is sent as numbered parts
// that together are the whole message, each a whole UTF-8 text, rather than
// failing the channel and never reaching the owner (ADR 0019, "Alerting").
func TestAnNtfyMessageOverTheServersLimitIsSentInNumberedParts(t *testing.T) {
	p := &posts{}
	n := ntfySender{server: "https://ntfy.private.test", topic: "t", client: &http.Client{Transport: p}}
	body := strings.Repeat("Degraded on SPY, TLT and GLD — check each at the broker.\n", 200)
	if err := n.send(report.Alert{Key: "k", Title: "joint: engine degraded", Body: body, Severity: report.Urgent}, 0); err != nil {
		t.Fatal(err)
	}
	if len(p.bodies) < 2 {
		t.Fatalf("a %d-byte message went in %d post(s)", len(body), len(p.bodies))
	}
	for i, b := range p.bodies {
		if len(b) > 4096 || !utf8.ValidString(b) {
			t.Errorf("part %d is %d bytes (limit %d) or not whole UTF-8", i+1, len(b), 4096)
		}
		if want := "joint: engine degraded (" + strconv.Itoa(i+1) + "/" + strconv.Itoa(len(p.bodies)) + ")"; p.titles[i] != want {
			t.Errorf("part %d is titled %q, want %q", i+1, p.titles[i], want)
		}
	}
	if got := strings.Join(p.bodies, ""); got != body {
		t.Error("the parts together are not the whole message")
	}

	short := &posts{}
	n.client = &http.Client{Transport: short}
	if err := n.send(report.Alert{Key: "k", Title: "joint: engine halted", Body: "short"}, 0); err != nil {
		t.Fatal(err)
	}
	if len(short.bodies) != 1 || short.titles[0] != "joint: engine halted" {
		t.Errorf("a message within the limit is one post with its own title: %q", short.titles)
	}
}
