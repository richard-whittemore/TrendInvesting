package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// httpSlicer fetches the journal slice from the dashboard's read-only route
// over the tailnet (docs/manual/alerts-and-dashboard.md, "Alerts analysed by
// Claude"). It holds no credential: it sends one GET with no Authorization
// or cookie, refuses a redirect, and reads the answer to a bound. The
// dashboard answers every writing method with 405, so nothing the job holds
// can change the server through this path.
type httpSlicer struct {
	url    string
	client *http.Client
	limit  int64
}

// maxSlice is the most the slice is read to: each of the dashboard's five
// reports cut to its last rows is far smaller.
const maxSlice = 4 << 20

// newHTTPSlicer accepts only an https address of the dashboard's slice
// route on a Tailscale host (…ts.net), or on the one host allow names.
func newHTTPSlicer(raw, allow string, client *http.Client) (*httpSlicer, error) {
	refuse := errors.New("triage: -slice-url must be https://<server>.<tailnet>.ts.net/api/journal-slice, the dashboard's journal slice on the tailnet")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Path != "/api/journal-slice" || u.RawQuery != "" {
		return nil, refuse
	}
	host := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, ".ts.net") && (allow == "" || !strings.EqualFold(allow, u.Host)) {
		return nil, refuse
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.Jar = nil
	return &httpSlicer{url: raw, client: &c, limit: maxSlice}, nil
}

// Fetch is the slice's text.
func (s *httpSlicer) Fetch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, http.NoBody)
	if err != nil {
		return "", errors.New("build request")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", errors.New("the dashboard could not be reached over the tailnet")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the dashboard answered %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, s.limit+1))
	if err != nil {
		return "", errors.New("the slice did not arrive whole")
	}
	if int64(len(raw)) > s.limit {
		return "", fmt.Errorf("the slice is larger than %d bytes", s.limit)
	}
	return string(raw), nil
}
