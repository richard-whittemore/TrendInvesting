package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// pingHeartbeat tells the outside heartbeat service, a Healthchecks-style
// endpoint off this server, that a firing finished well (ADR 0019,
// "Alerting": a heartbeat, checked from outside the system). The service
// pages the owner when the pings stop, which is how the server's own death is
// noticed: nothing on the server can page about that. The address is
// TI_HEARTBEAT_URL, a secret (the service's address is its key), so it never
// reaches an error or a log line. The request is a POST with no body, no query
// and no header of ours, so nothing about the account can ride on it. A ping
// that cannot be sent never fails the run: the failure is logged once, to out,
// and the next firing pings again. Without the setting nothing is sent.
func pingHeartbeat(getenv func(string) string, client *http.Client, out io.Writer) {
	address := getenv("TI_HEARTBEAT_URL")
	if address == "" {
		return
	}
	if err := postPing(address, client); err != nil {
		_, _ = fmt.Fprintf(out, "reporter: the heartbeat ping was not sent: %v (the next firing tries again)\n", err)
	}
}

// postPing sends one ping and reports why it failed in words that never carry
// the address.
func postPing(address string, client *http.Client) error {
	if u, err := url.Parse(address); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("TI_HEARTBEAT_URL is not an absolute http or https URL")
	}
	req, err := http.NewRequest(http.MethodPost, address, http.NoBody)
	if err != nil {
		return errors.New("the request could not be built")
	}
	resp, err := client.Do(req)
	if err != nil {
		// The transport's error names the address, so only its kind is kept.
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return errors.New("the service did not answer in time")
		}
		return errors.New("the service could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the service answered %s", resp.Status)
	}
	return nil
}
