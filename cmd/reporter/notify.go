package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// sender delivers one alert over one channel.
type sender interface {
	name() string
	send(report.Alert) error
}

// echoOnly marks a sender that shows an alert to whoever is watching the
// terminal and reaches no one else, so it never counts as a delivery.
type echoOnly interface{ echo() }

// relayed marks a sender whose messages pass through a public relay, which
// anyone who learns the topic can read. The daily summary, which carries the
// account's balances, is never sent through one (owner decision 2026-10-17,
// docs/live-trading/plan.md section 7).
type relayed interface{ publicRelay() bool }

// kindSummary is the daily summary's kind: a routine message, not an
// incident, and the one message that carries balances.
const kindSummary report.Kind = "daily-summary"

// publicNtfy reports whether an ntfy server is the public ntfy.sh relay.
func publicNtfy(server string) (bool, error) {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return false, errors.New("reporter: TI_NTFY_SERVER must be an absolute URL such as https://ntfy.example.org")
	}
	host := strings.ToLower(u.Hostname())
	return host == "ntfy.sh" || strings.HasSuffix(host, ".ntfy.sh"), nil
}

const defaultPagerDutyURL = "https://events.pagerduty.com/v2/enqueue"

func buildSenders(channels string, getenv func(string) string, client *http.Client, out io.Writer) ([]sender, error) {
	var senders []sender
	reaches := false
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
			public, err := publicNtfy(server)
			if err != nil {
				return nil, err
			}
			senders = append(senders, ntfySender{server: strings.TrimRight(server, "/"), topic: topic, client: client, public: public})
			reaches = true
		case "pagerduty":
			key := getenv("TI_PAGERDUTY_ROUTING_KEY")
			if key == "" {
				return nil, errors.New("reporter: the pagerduty channel needs TI_PAGERDUTY_ROUTING_KEY (the Events API v2 integration key; it is a secret)")
			}
			endpoint := getenv("TI_PAGERDUTY_URL")
			if endpoint == "" {
				endpoint = defaultPagerDutyURL
			}
			senders = append(senders, pagerDutySender{url: endpoint, routingKey: key, client: client})
			reaches = true
		default:
			return nil, fmt.Errorf("reporter: unknown channel %q (stdout, ntfy, pagerduty)", c)
		}
	}
	if !reaches {
		return nil, errors.New("reporter: no delivery channel named; stdout only shows alerts on the terminal and reaches no one, so add ntfy or pagerduty")
	}
	return senders, nil
}

// channelKey is the state-file key recording that one channel took one alert.
func channelKey(channel, alertKey string) string { return channel + "|" + alertKey }

// deliver sends an alert to every delivery channel that has not yet taken it
// and records each success in seen. An alert counts as delivered only when
// every delivery channel has taken it: a failure on one channel is never
// covered by another's success, and only the channel that failed is tried
// again. The terminal echo shows an alert while any channel still lacks it
// and never counts. The daily summary skips a channel on a public relay, and
// is an error when no other delivery channel was named. The error joins every
// channel failure.
func deliver(senders []sender, a report.Alert, seen map[string]bool) error {
	var pending []sender
	private := false
	for _, s := range senders {
		_, echo := s.(echoOnly)
		if r, ok := s.(relayed); ok && r.publicRelay() && a.Kind == kindSummary {
			continue
		}
		private = private || !echo
		if echo || !seen[channelKey(s.name(), a.Key)] {
			pending = append(pending, s)
		}
	}
	if !private {
		return errors.New("the daily summary carries balances and never goes through a public relay such as ntfy.sh; name pagerduty or your own ntfy server (TI_NTFY_SERVER) to receive it")
	}
	reaching := 0
	for _, s := range pending {
		if _, echo := s.(echoOnly); !echo {
			reaching++
		}
	}
	if reaching == 0 {
		return nil
	}
	var failures []error
	for _, s := range pending {
		err := s.send(a)
		if _, echo := s.(echoOnly); echo {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", s.name(), err))
			continue
		}
		seen[channelKey(s.name(), a.Key)] = true
	}
	return errors.Join(failures...)
}

type stdoutSender struct{ out io.Writer }

func (stdoutSender) name() string { return "stdout" }

func (stdoutSender) echo() {}

func (s stdoutSender) send(a report.Alert) error {
	_, err := fmt.Fprintf(s.out, "[%s] %s\n%s\n\n", a.Severity, a.Title, a.Body)
	return err
}

// ntfySender posts to an ntfy server (https://docs.ntfy.sh/publish/). The
// topic is the credential: it never appears in an error or in output.
type ntfySender struct {
	server, topic string
	client        *http.Client
	public        bool
}

func (ntfySender) name() string { return "ntfy" }

func (n ntfySender) publicRelay() bool { return n.public }

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

// pagerDutySender triggers an incident through the PagerDuty Events API v2
// (https://developer.pagerduty.com/api-reference/368ae3d938c9e-send-an-event-to-pager-duty).
// The alert key is the dedup key (see dedupKey), so a retried trigger joins
// the incident it would have opened. The answer is read back: only HTTP 202
// with status "success" is an acknowledged delivery, and anything else is a
// failure. The routing key is the credential and never appears in an error or
// in output.
type pagerDutySender struct {
	url, routingKey string
	client          *http.Client
}

func (pagerDutySender) name() string { return "pagerduty" }

// maxDedupKey is the Events API v2 limit on dedup_key; a longer key is
// answered 400 and the alert would never be delivered.
const maxDedupKey = 255

// dedupKey is the alert key when it fits PagerDuty's limit. A longer one
// (an engine state naming many instruments or orders) keeps a readable
// prefix and ends in the SHA-256 of the whole key, so it is stable across
// runs and distinct for distinct alerts. The full key travels in
// custom_details.
func dedupKey(key string) string {
	if len(key) <= maxDedupKey {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	suffix := "~sha256:" + hex.EncodeToString(sum[:])
	prefix := key[:maxDedupKey-len(suffix)]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + suffix
}

type pagerDutyEvent struct {
	RoutingKey  string           `json:"routing_key"`
	EventAction string           `json:"event_action"`
	DedupKey    string           `json:"dedup_key"`
	Payload     pagerDutyPayload `json:"payload"`
}

type pagerDutyPayload struct {
	Summary       string            `json:"summary"`
	Source        string            `json:"source"`
	Severity      string            `json:"severity"`
	CustomDetails map[string]string `json:"custom_details"`
}

func (p pagerDutySender) send(a report.Alert) error {
	severity := "info"
	if a.Severity == report.Urgent {
		severity = "critical"
	}
	body, err := json.Marshal(pagerDutyEvent{
		RoutingKey: p.routingKey, EventAction: "trigger", DedupKey: dedupKey(a.Key),
		Payload: pagerDutyPayload{Summary: a.Title, Source: a.AccountID, Severity: severity, CustomDetails: map[string]string{"detail": a.Body, "alert_key": a.Key}},
	})
	if err != nil {
		return errors.New("build request")
	}
	req, err := http.NewRequest(http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("build request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("post failed: %w", urlErr.Err)
		}
		return errors.New("post failed")
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("server answered %s", resp.Status)
	}
	var ack struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(answer, &ack); err != nil || ack.Status != "success" {
		return errors.New("the answer did not acknowledge the event")
	}
	return nil
}
