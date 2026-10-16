package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// sender delivers one alert over one channel.
type sender interface {
	name() string
	send(report.Alert) error
}

func buildSenders(channels string, getenv func(string) string, client *http.Client, out io.Writer) ([]sender, error) {
	var senders []sender
	for _, c := range strings.Split(channels, ",") {
		switch strings.TrimSpace(c) {
		case "stdout":
			senders = append(senders, stdoutSender{out: out})
		case "ntfy":
			topic := getenv("TI_NTFY_TOPIC")
			if topic == "" {
				return nil, errors.New("reporter: the ntfy channel needs TI_NTFY_TOPIC (an unguessable topic name; it is the only secret)")
			}
			server := getenv("TI_NTFY_SERVER")
			if server == "" {
				server = "https://ntfy.sh"
			}
			senders = append(senders, ntfySender{server: strings.TrimRight(server, "/"), topic: topic, client: client})
		default:
			return nil, fmt.Errorf("reporter: unknown channel %q (stdout, ntfy)", c)
		}
	}
	return senders, nil
}

// deliver sends an alert on every channel. It counts as delivered when at least
// one channel took it (ADR 0019: a failed delivery falls back to the other
// channel); the error is returned only when none did.
func deliver(senders []sender, a report.Alert) error {
	var failures []error
	for _, s := range senders {
		if err := s.send(a); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", s.name(), err))
			continue
		}
		return nil
	}
	return errors.Join(failures...)
}

type stdoutSender struct{ out io.Writer }

func (stdoutSender) name() string { return "stdout" }

func (s stdoutSender) send(a report.Alert) error {
	_, err := fmt.Fprintf(s.out, "[%s] %s\n%s\n\n", a.Severity, a.Title, a.Body)
	return err
}

// ntfySender posts to an ntfy server (https://docs.ntfy.sh/publish/). The
// topic is the credential: it never appears in an error or in output.
type ntfySender struct {
	server, topic string
	client        *http.Client
}

func (ntfySender) name() string { return "ntfy" }

func (n ntfySender) send(a report.Alert) error {
	req, err := http.NewRequest(http.MethodPost, n.server+"/"+url.PathEscape(n.topic), bytes.NewBufferString(a.Body))
	if err != nil {
		return errors.New("build request")
	}
	req.Header.Set("Title", a.Title)
	req.Header.Set("Priority", "3")
	if a.Severity == report.Urgent {
		req.Header.Set("Priority", "5")
	}
	resp, err := n.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("post failed: %w", urlErr.Err)
		}
		return errors.New("post failed")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("server answered %s", resp.Status)
	}
	return nil
}
