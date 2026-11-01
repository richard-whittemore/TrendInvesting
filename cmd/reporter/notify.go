package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// sender delivers one alert over one channel.
type sender interface {
	name() string
	// send delivers a; repeat is 0 for the alert's first delivery on this
	// channel and counts its repeats after that.
	send(a report.Alert, repeat int) error
}

// echoOnly marks a sender that shows an alert to whoever is watching the
// terminal and reaches no one else, so it never counts as a delivery.
type echoOnly interface{ echo() }

// summaryTaker marks a sender that may be given the daily summary, which
// carries the account's balances. Only the owner's own ntfy server is one: a
// public relay can be read by anyone who learns the topic (owner decision
// 2026-10-17, docs/live-trading/plan.md section 7), and the summary is a
// routine message that never opens a PagerDuty incident (ADR 0019,
// "Alerting"; docs/manual/alerts-and-dashboard.md, "The delivery rules": a
// page always means an incident).
type summaryTaker interface{ takesSummary() bool }

// kindSummary is the daily summary's kind: a routine message, not an
// incident, and the one message that carries balances.
const kindSummary report.Kind = "daily-summary"

// kindSummaryNoChannel is the routine notice that the daily summary has no
// channel that takes it.
const kindSummaryNoChannel report.Kind = "daily-summary-no-channel"

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

const defaultPushoverURL = "https://api.pushover.net/1/messages.json"

// withSecretsFile returns a lookup that answers from getenv, and, for a name
// the environment leaves empty, from the KEY=VALUE lines of the secrets file
// at path: under systemd the secrets arrive as a credential file
// (LoadCredential), never in the environment (deploy/etc/secrets.env.example).
// Blank lines and # comments are skipped and values are trimmed. A file that
// cannot be read, or a line that is not KEY=VALUE, is reported in the error,
// which names the line's number and never its text, which may be a secret;
// the lookup still answers from the environment and every line that reads,
// so a fault leaves unset only what it touches and each channel is judged on
// its own secrets (ADR 0019, "Alerting": one channel's fault must not silence
// another).
func withSecretsFile(getenv func(string) string, path string) (func(string) string, error) {
	values := map[string]string{}
	lookup := func(key string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return values[key]
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return lookup, errors.New("reporter: the secrets file cannot be read; check -secrets-file names the credential file")
	}
	var faults []error
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			faults = append(faults, fmt.Errorf("reporter: the secrets file's line %d is not KEY=VALUE", n+1))
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return lookup, errors.Join(faults...)
}

// channelFault is a delivery channel set aside because its configuration is
// empty or malformed: the channel's name and why, never a secret's value.
type channelFault struct {
	channel string
	err     error
}

// buildSenders builds every channel named, each validated on its own: a
// channel whose secret or setting is empty or malformed, or whose name is
// unknown, is set aside as a fault and the others are still built, so one
// bad secret never silences every channel (ADR 0019, "Alerting"). The error
// is set only when no channel that reaches a person was built: it joins every
// fault, since nothing can then deliver.
func buildSenders(channels string, getenv func(string) string, client *http.Client, out io.Writer) ([]sender, []channelFault, error) {
	var senders []sender
	var faults []channelFault
	reaches := false
	for _, c := range strings.Split(channels, ",") {
		name := strings.TrimSpace(c)
		var s sender
		var err error
		switch name {
		case "stdout":
			senders = append(senders, stdoutSender{out: out})
			continue
		case "ntfy":
			s, err = ntfyFrom(getenv, client)
		case "pagerduty":
			s, err = pagerDutyFrom(getenv, client)
		case "pushover":
			s, err = pushoverFrom(getenv, client)
			if p, ok := s.(*pushoverSender); ok {
				p.warn = out
			}
		case issuesChannel:
			s, err = githubIssuesFrom(getenv, client)
		default:
			err = fmt.Errorf("reporter: unknown channel %q (stdout, ntfy, pagerduty, pushover, github-issues)", c)
		}
		if err != nil {
			faults = append(faults, channelFault{channel: name, err: err})
			continue
		}
		senders = append(senders, s)
		reaches = true
	}
	if !reaches {
		errs := make([]error, 0, len(faults)+1)
		for _, f := range faults {
			errs = append(errs, f.err)
		}
		errs = append(errs, errors.New("reporter: no delivery channel named; stdout only shows alerts on the terminal and reaches no one, so add ntfy, pagerduty or pushover"))
		return nil, faults, errors.Join(errs...)
	}
	return senders, faults, nil
}

// ntfyFrom builds the ntfy channel from its secrets: the topic, which is the
// only secret, and the server, the public ntfy.sh relay when unset.
func ntfyFrom(getenv func(string) string, client *http.Client) (sender, error) {
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
	return ntfySender{server: strings.TrimRight(server, "/"), topic: topic, client: client, public: public}, nil
}

// pagerDutyFrom builds the PagerDuty channel from its secrets: the routing
// key, and the Events API endpoint, PagerDuty's own when unset. An endpoint
// that is not an absolute URL is refused here, where it names the channel,
// rather than failing every send.
func pagerDutyFrom(getenv func(string) string, client *http.Client) (sender, error) {
	key := getenv("TI_PAGERDUTY_ROUTING_KEY")
	if key == "" {
		return nil, errors.New("reporter: the pagerduty channel needs TI_PAGERDUTY_ROUTING_KEY (the Events API v2 integration key; it is a secret)")
	}
	endpoint := getenv("TI_PAGERDUTY_URL")
	if endpoint == "" {
		endpoint = defaultPagerDutyURL
	}
	if u, err := url.Parse(endpoint); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("reporter: TI_PAGERDUTY_URL must be an absolute URL such as https://events.pagerduty.com/v2/enqueue")
	}
	return pagerDutySender{url: endpoint, routingKey: key, client: client}, nil
}

// pushoverFrom builds the Pushover channel from its secrets: the application's
// API token and the owner's user key, and the messages endpoint, Pushover's own
// when unset. An endpoint that is not an absolute URL is refused here, where it
// names the channel, rather than failing every send.
func pushoverFrom(getenv func(string) string, client *http.Client) (sender, error) {
	token := getenv("TI_PUSHOVER_APP_TOKEN")
	if token == "" {
		return nil, errors.New("reporter: the pushover channel needs TI_PUSHOVER_APP_TOKEN (the application's API token; it is a secret)")
	}
	user := getenv("TI_PUSHOVER_USER_KEY")
	if user == "" {
		return nil, errors.New("reporter: the pushover channel needs TI_PUSHOVER_USER_KEY (the owner's user key; it is a secret)")
	}
	endpoint := getenv("TI_PUSHOVER_URL")
	if endpoint == "" {
		endpoint = defaultPushoverURL
	}
	if !secureEndpoint(endpoint) {
		return nil, errors.New("reporter: TI_PUSHOVER_URL must be an https URL such as https://api.pushover.net/1/messages.json (http is allowed only for a loopback host)")
	}
	return &pushoverSender{url: endpoint, token: token, user: user, client: client}, nil
}

// secureEndpoint reports whether the address a secret is posted to is https,
// or http on a loopback host (a stand-in on the same machine), with no user
// information. The address's text is never in an error.
func secureEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		ip := net.ParseIP(u.Hostname())
		return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return false
}

// channelKey is the state-file key recording that one channel took one alert.
func channelKey(channel, alertKey string) string { return channel + "|" + alertKey }

// deliver sends an alert to every delivery channel it is due on
// (report.Due: not yet taken, or a repeating, unacknowledged alert last taken
// at least every ago) and records each success in seen and its time in sent.
// An alert counts as delivered only when every delivery channel has taken it:
// a failure on one channel is never covered by another's success, and only
// the channel that failed is tried again. The terminal echo shows an alert
// while any channel still lacks it and never counts. The daily summary goes
// only to a channel that takes it (summaryTaker), and is not sent when there
// is none: the caller says so (summaryRoute). Each repeat a channel takes is counted in repeats, so the
// channel can tell a repeat from the first delivery. After each success,
// record saves the state, so a run stopped midway has saved every delivery
// it made. The error joins every channel failure and every failed save.
func deliver(senders []sender, a report.Alert, seen map[string]bool, sent map[string]time.Time, repeats map[string]int, now time.Time, every time.Duration, record func() error, policies ...*pagingPolicy) error {
	var policyErr error
	if p := policyOf(policies); p != nil {
		key := channelKey("escalation", a.Key)
		before := sent[key]
		observed := sent[channelKey("observed", a.Key)]
		senders, a, every, policyErr = p.route(senders, a, sent, now, every)
		if !before.Equal(sent[key]) || !observed.Equal(sent[channelKey("observed", a.Key)]) {
			policyErr = errors.Join(policyErr, record())
		}
	}

	var pending []sender
	for _, s := range senders {
		_, echo := s.(echoOnly)
		if t, ok := s.(summaryTaker); !echo && a.Kind == kindSummary && (!ok || !t.takesSummary()) {
			continue
		}
		key := channelKey(s.name(), a.Key)
		if echo || report.Due(a, seen[key], sent[key], now, every) {
			pending = append(pending, s)
		}
	}
	reaching := 0
	for _, s := range pending {
		if _, echo := s.(echoOnly); !echo {
			reaching++
		}
	}
	if reaching == 0 {
		return policyErr
	}
	// An issue tracker is sent the alert after every other channel, so a
	// slow one never holds back a page (ADR 0019, "Alerting").
	pending = trackersLast(pending)
	var failures []error
	failures = append(failures, policyErr)
	for _, s := range pending {
		key := channelKey(s.name(), a.Key)
		repeat := 0
		if seen[key] {
			repeat = repeats[key] + 1
		}
		err := s.send(a, repeat)
		if _, echo := s.(echoOnly); echo {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", s.name(), err))
			continue
		}
		keepReceipt(s, a.Key, seen, now)
		seen[key] = true
		if a.Repeat || policyOf(policies) != nil {
			sent[key] = now
		}
		if repeat > 0 {
			repeats[key] = repeat
		}
		if err := record(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// issueTracking marks a channel that writes to an issue tracker
// (issuesSender): it is sent every alert after the channels that page.
type issueTracking interface{ tracksIssues() }

// trackersLast is senders in their order, with the issue trackers moved to
// the end.
func trackersLast(senders []sender) []sender {
	first, last := splitTrackers(senders)
	return append(first, last...)
}

// splitTrackers parts senders into the channels that page and the issue
// trackers, each in its order.
func splitTrackers(senders []sender) (paging, trackers []sender) {
	for _, s := range senders {
		if _, ok := s.(issueTracking); ok {
			trackers = append(trackers, s)
			continue
		}
		paging = append(paging, s)
	}
	return paging, trackers
}

type stdoutSender struct{ out io.Writer }

func (stdoutSender) name() string { return "stdout" }

func (stdoutSender) echo() {}

func (s stdoutSender) send(a report.Alert, _ int) error {
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

// takesSummary is true only for the owner's own server: the summary's
// balances never go to a public relay.
func (n ntfySender) takesSummary() bool { return !n.public }

// ntfyMessageLimit is the most bytes one ntfy message may carry, the
// server's default message-size-limit (https://docs.ntfy.sh/config/): a
// longer one is refused or turned into an attachment the phone does not
// show.
const ntfyMessageLimit = 4096

// send posts the alert's text; through a public relay it posts the alert's
// public text when it has one, which carries no balance (owner decision
// 2026-10-17). A text longer than ntfy's message limit is posted as numbered
// parts, each whole UTF-8 and within the limit, the title ending "(i/n)",
// rather than failing the channel and never reaching the owner (ADR 0019,
// "Alerting"). A part that fails fails the send, and the whole alert is
// posted again at the next firing. The client's timeout (-send-timeout, the
// longest one send to one channel may take) is one deadline for every part
// together, so a message that cannot be posted within it fails this channel
// alone and the other channels are reached without waiting (ADR 0019,
// "Alerting": one channel's fault must not silence another).
func (n ntfySender) send(a report.Alert, _ int) error {
	body := a.Body
	if n.public && a.PublicBody != "" {
		body = a.PublicBody
	}
	ctx := context.Background()
	if n.client.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, n.client.Timeout)
		defer cancel()
	}
	parts := splitMessage(body, ntfyMessageLimit)
	for i, part := range parts {
		title := a.Title
		if len(parts) > 1 {
			title = fmt.Sprintf("%s (%d/%d)", a.Title, i+1, len(parts))
		}
		if err := n.post(ctx, title, part, a.Severity); err != nil {
			return err
		}
	}
	return nil
}

// splitMessage cuts text into parts of at most limit bytes, each ending at a
// line's end where one falls in its second half, and never inside a UTF-8
// character; joined, the parts are text.
func splitMessage(text string, limit int) []string {
	var parts []string
	for len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if nl := strings.LastIndexByte(text[:cut], '\n'); nl >= limit/2 {
			cut = nl + 1
		}
		parts = append(parts, text[:cut])
		text = text[cut:]
	}
	return append(parts, text)
}

// post sends one message to the topic, within the send's deadline ctx.
func (n ntfySender) post(ctx context.Context, title, body string, severity report.Severity) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.server+"/"+url.PathEscape(n.topic), bytes.NewBufferString(body))
	if err != nil {
		return errors.New("build request")
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", "3")
	if severity == report.Urgent {
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
// the incident it would have opened. A repeat carries its ordinal in the dedup
// key and opens an incident of its own: PagerDuty folds a trigger into an
// open incident under the same key, and an incident acknowledged on the phone
// takes no further triggers, while only an acknowledgement recorded in the
// journal stops the repeats (ADR 0019, ADR 0026). The answer is read back: only HTTP 202
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
	RoutingKey  string            `json:"routing_key"`
	EventAction string            `json:"event_action"`
	DedupKey    string            `json:"dedup_key"`
	Payload     *pagerDutyPayload `json:"payload,omitempty"`
}

type pagerDutyPayload struct {
	Summary       string            `json:"summary"`
	Source        string            `json:"source"`
	Severity      string            `json:"severity"`
	CustomDetails map[string]string `json:"custom_details"`
}

func (p pagerDutySender) send(a report.Alert, repeat int) error {
	severity := "info"
	if a.Severity == report.Urgent {
		severity = "critical"
	}
	incident := a.Key
	if repeat > 0 {
		incident = fmt.Sprintf("%s#repeat-%d", a.Key, repeat)
	}
	body, err := json.Marshal(pagerDutyEvent{
		RoutingKey: p.routingKey, EventAction: "trigger", DedupKey: dedupKey(incident),
		Payload: &pagerDutyPayload{Summary: a.Title, Source: a.AccountID, Severity: severity, CustomDetails: map[string]string{"detail": a.Body, "alert_key": a.Key}},
	})
	if err != nil {
		return errors.New("build request")
	}
	return p.post(body)
}

// resolve closes the incident triggered under the alert key key (the first
// delivery's dedup key), so the next trigger under it opens a new incident
// and pages again. A resolve carries no payload.
func (p pagerDutySender) resolve(key string) error {
	body, err := json.Marshal(pagerDutyEvent{RoutingKey: p.routingKey, EventAction: "resolve", DedupKey: dedupKey(key)})
	if err != nil {
		return errors.New("build request")
	}
	return p.post(body)
}

// post sends one event and reads the answer back: only HTTP 202 with status
// "success" is an acknowledged delivery.
func (p pagerDutySender) post(body []byte) error {
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

// Pushover's documented limits (https://pushover.net/api): a message of at
// most 1024 characters, a title of at most 250, and for an emergency a retry
// of at least 30 seconds and an expire of at most 10800.
const (
	pushoverMessageLimit = 1024
	pushoverTitleLimit   = 250
	pushoverRetry        = 60 * time.Second
	pushoverExpire       = 30 * time.Minute
	// pushoverAnswerLimit is the most of an answer that is read: a longer one
	// is not Pushover's.
	pushoverAnswerLimit = 1 << 16
	// pushoverKeyLimit and pushoverAccountLimit are the most characters of
	// the alert key and of the account the message carries, so that the body
	// always keeps room; one longer is cut with an ellipsis.
	pushoverKeyLimit     = 512
	pushoverAccountLimit = 128
)

// pushoverSender sends an alert through the Pushover messages API
// (https://pushover.net/api). A page (an urgent alert) is an emergency,
// priority 2: the phone repeats it every pushoverRetry until it is
// acknowledged on the phone or pushoverExpire passes. Every other alert is
// priority 1. It carries what the PagerDuty payload carries (title, detail,
// alert key and account) and nothing more: no account number and no secret.
// An alert is one message, cut to Pushover's limits, never several, so a long
// page never opens a second emergency; the detail is shortened first, and the
// alert key and account that end the message are kept whole up to their own
// limits (pushoverKeyLimit, pushoverAccountLimit), beyond which each is cut. A retried delivery follows
// the reporter's rules (ADR 0019, ADR 0026): a message Pushover did not
// accept is sent again on the next firing, and a repeat of an unacknowledged
// alert is a new emergency only when the repeat interval says so, until the
// journal records the acknowledgement. The answer is read back, within a size
// limit and in full: only a 2xx whose whole body is one JSON object with
// "status":1 is a delivery. An emergency's receipt is handed to the reporter
// (receipter) so the page can be cancelled when its incident resolves. The
// token and user key are credentials and never appear in an error or in
// output: an error is a fixed message with the HTTP status code or the kind
// of transport fault.
type pushoverSender struct {
	url, token, user string
	client           *http.Client
	// warn receives the reporter's fixed warnings; nil discards them.
	warn io.Writer

	mu      sync.Mutex
	receipt string
}

func (*pushoverSender) name() string { return "pushover" }

// cutRunes shortens text to at most limit characters, ending in an ellipsis
// when it was cut, and never inside a UTF-8 character. The limit must be
// positive: a limit below 1 gives the empty string.
func cutRunes(text string, limit int) string {
	if limit < 1 {
		return ""
	}
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit-1]) + "\u2026"
}

// pushoverMessage is the detail of an alert within Pushover's limit: the
// body, shortened as needed, then the alert key (cut at pushoverKeyLimit
// characters) and account (cut at pushoverAccountLimit), which are kept whole
// up to those limits, so the body always keeps room.
func pushoverMessage(a report.Alert) string {
	suffix := "\n\nAlert: " + cutRunes(a.Key, pushoverKeyLimit)
	if a.AccountID != "" {
		suffix += " (" + cutRunes(a.AccountID, pushoverAccountLimit) + ")"
	}
	return cutRunes(a.Body, pushoverMessageLimit-utf8.RuneCountInString(suffix)) + suffix
}

func (p *pushoverSender) send(a report.Alert, repeat int) error {
	form := url.Values{
		"token":    {p.token},
		"user":     {p.user},
		"title":    {cutRunes(a.Title, pushoverTitleLimit)},
		"message":  {pushoverMessage(a)},
		"priority": {"1"},
	}
	if a.Severity == report.Urgent {
		form.Set("priority", "2")
		form.Set("retry", fmt.Sprint(int(pushoverRetry.Seconds())))
		form.Set("expire", fmt.Sprint(int(pushoverExpire.Seconds())))
	}
	receipt, err := p.call(p.url, form)
	if err != nil {
		return err
	}
	switch {
	case a.Severity != report.Urgent:
		receipt = ""
	case receipt == "":
		// The phone rings, so this is a delivery, but there is no receipt to
		// cancel it with: the state records that, and the owner is told.
		receipt = noReceipt
		if p.warn != nil {
			_, _ = fmt.Fprintln(p.warn, "reporter: pushover accepted an emergency without a usable receipt; it cannot be cancelled and runs until it is acknowledged or expires")
		}
	}
	p.mu.Lock()
	p.receipt = receipt
	p.mu.Unlock()
	return nil
}

// expiry is how long an emergency lasts: after it Pushover has stopped it.
func (*pushoverSender) expiry() time.Duration { return pushoverExpire }

// takeReceipt returns the receipt of the emergency the last send opened, and
// forgets it; empty when that send was not an emergency.
func (p *pushoverSender) takeReceipt() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.receipt
	p.receipt = ""
	return r
}

// cancel cancels the emergency with the receipt, so the phone stops
// repeating it (https://pushover.net/api#receipt). A receipt Pushover no
// longer knows (HTTP 400 whose answer says the receipt was not found: expired
// or already over) has nothing left to cancel and is not a failure. Any other
// 400, such as an invalid application token, is a failure: the receipt is
// kept and the cancel retried until the emergency expires.
func (p *pushoverSender) cancel(receipt string) error {
	endpoint, err := url.Parse(p.url)
	if err != nil {
		return errors.New("build request")
	}
	endpoint.Path = "/1/receipts/" + receipt + "/cancel.json"
	endpoint.RawQuery = ""
	code, body, err := p.post(endpoint.String(), url.Values{"token": {p.token}})
	if err != nil {
		return err
	}
	if code == http.StatusBadRequest && receiptUnknown(body) {
		return nil
	}
	if code/100 != 2 {
		return httpStatus(code)
	}
	return checkAck(body)
}

// receiptUnknown reports whether a 400 answer says Pushover does not know the
// receipt (its "errors" name "receipt not found"), and does not say the
// application token is invalid. The answer's text is never repeated.
func receiptUnknown(body []byte) bool {
	var answer struct {
		Token  string   `json:"token"`
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Token == "invalid" {
		return false
	}
	for _, e := range answer.Errors {
		if strings.Contains(strings.ToLower(e), "receipt not found") {
			return true
		}
	}
	return false
}

// httpStatus is a server's answer other than 2xx, by code alone: the status
// text is the server's and is never repeated.
type httpStatus int

func (s httpStatus) Error() string { return fmt.Sprintf("server answered HTTP %d", int(s)) }

// call posts form to endpoint and reads the answer back, returning the
// receipt it carries (empty unless the message was an emergency).
func (p *pushoverSender) call(endpoint string, form url.Values) (string, error) {
	code, body, err := p.post(endpoint, form)
	if err != nil {
		return "", err
	}
	if code/100 != 2 {
		return "", httpStatus(code)
	}
	if err := checkAck(body); err != nil {
		return "", err
	}
	var ack struct {
		Receipt string `json:"receipt"`
	}
	_ = json.Unmarshal(body, &ack)
	if !receiptShape.MatchString(ack.Receipt) {
		return "", nil
	}
	return ack.Receipt, nil
}

// jsonSpace is the four bytes JSON allows between its tokens.
const jsonSpace = " \t\n\r"

// checkAck requires the answer to be one JSON object, and nothing after it
// but white space, with "status":1.
func checkAck(answer []byte) error {
	var ack struct {
		Status int `json:"status"`
	}
	dec := json.NewDecoder(bytes.NewReader(answer))
	if err := dec.Decode(&ack); err != nil || len(bytes.Trim(answer[dec.InputOffset():], jsonSpace)) != 0 || ack.Status != 1 {
		return errors.New("the answer did not acknowledge the message")
	}
	return nil
}

// post posts form to endpoint and returns the status code and the whole
// answer, within the size limit. A redirect is never followed.
func (p *pushoverSender) post(endpoint string, form url.Values) (code int, answer []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, errors.New("build request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// A redirect is never followed: it could carry the form, and the token in
	// it, to another address. The 3xx answer is then a failure.
	client := *p.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, transportFault(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusBadRequest {
		return resp.StatusCode, nil, nil
	}
	answer, err = io.ReadAll(io.LimitReader(resp.Body, pushoverAnswerLimit+1))
	if err != nil {
		return 0, nil, errors.New("the answer could not be read in full")
	}
	if len(answer) > pushoverAnswerLimit {
		return 0, nil, errors.New("the answer was longer than any Pushover answer")
	}
	return resp.StatusCode, answer, nil
}

// receiptShape is what a Pushover receipt looks like: letters and digits.
var receiptShape = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// transportFault is a fixed message for a request that got no answer: the
// kind of fault, never the error's own text, which names the request's URL
// and may carry what it was posted.
func transportFault(err error) error {
	kind := "other"
	var netErr net.Error
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		kind = "timeout"
	case errors.As(err, &dnsErr):
		kind = "dns"
	case errors.Is(err, syscall.ECONNREFUSED):
		kind = "connection refused"
	case errors.As(err, &certErr):
		kind = "tls"
	}
	return fmt.Errorf("transport error (%s)", kind)
}
