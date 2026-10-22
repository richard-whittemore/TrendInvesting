package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func slicerFor(t *testing.T, srv *httptest.Server) *httpSlicer {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	s, err := newHTTPSlicer(srv.URL+"/api/journal-slice", u.Host, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTheSliceIsAPlainGETWithNoCredential: the access path carries no
// credential of any kind; there is nothing in the job that could write to
// the server through it.
func TestTheSliceIsAPlainGETWithNoCredential(t *testing.T) {
	var method, auth, cookie string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, auth, cookie = r.Method, r.Header.Get("Authorization"), r.Header.Get("Cookie")
		_, _ = io.WriteString(w, "## boost.log: journal-query refusals\n")
	}))
	defer srv.Close()
	got, err := slicerFor(t, srv).Fetch(context.Background(), time.Time{}, time.Time{}, "")
	if err != nil || !strings.Contains(got, "journal-query refusals") {
		t.Fatalf("slice %q, err %v", got, err)
	}
	if method != http.MethodGet || auth != "" || cookie != "" {
		t.Errorf("sent %s with Authorization %q, Cookie %q", method, auth, cookie)
	}
}

// TestTheSliceClientHasNoWritingMethod fails if the slice client's file names
// any writing method: the job's one way into the server only reads (the
// dashboard answers every writing method on every route with 405, which
// cmd/dashboard's TestEveryRouteRefusesEveryWritingMethod checks).
func TestTheSliceClientHasNoWritingMethod(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "slice.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" && strings.HasPrefix(sel.Sel.Name, "Method") && sel.Sel.Name != "MethodGet" {
				t.Errorf("slice.go names http.%s", sel.Sel.Name)
			}
			if sel.Sel.Name == "Post" || sel.Sel.Name == "PostForm" {
				t.Errorf("slice.go calls %s", sel.Sel.Name)
			}
		}
		return true
	})
}

func TestTheSliceAddressMustBeHTTPSOnTheTailnet(t *testing.T) {
	for _, c := range []struct {
		u, allow string
		ok       bool
	}{
		{"https://lightsail.tail1a2b3.ts.net/api/journal-slice", "", true},
		{"http://lightsail.tail1a2b3.ts.net/api/journal-slice", "", false},
		{"https://example.com/api/journal-slice", "", false},
		{"https://lightsail.tail1a2b3.ts.net/", "", false},
		{"https://user@lightsail.tail1a2b3.ts.net/api/journal-slice", "", false},
		{"https://10.0.0.5/api/journal-slice", "10.0.0.5", true},
	} {
		if _, err := newHTTPSlicer(c.u, c.allow, http.DefaultClient); (err == nil) != c.ok {
			t.Errorf("newHTTPSlicer(%q, %q) = %v, want ok %v", c.u, c.allow, err, c.ok)
		}
	}
}

func TestTheSliceRefusesARedirectAStatusAndAnOversizeAnswer(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/", http.StatusFound)
		},
		"status":   func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusInternalServerError) },
		"oversize": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", 200)) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewTLSServer(h)
			defer srv.Close()
			s := slicerFor(t, srv)
			s.limit = 100
			if _, err := s.Fetch(context.Background(), time.Time{}, time.Time{}, ""); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

// TestTheSliceIsAskedForTheAlertsWindow: the job asks the route for the
// rows of one window, as RFC 3339 instants in the query, and for the alert
// by its key's hash, and nothing else.
func TestTheSliceIsAskedForTheAlertsWindow(t *testing.T) {
	var query url.Values
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	from := time.Date(2026, time.October, 19, 15, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.October, 22, 15, 0, 0, 0, time.UTC)
	if _, err := slicerFor(t, srv).Fetch(context.Background(), from, to, alertKey); err != nil {
		t.Fatal(err)
	}
	if query.Get("from") != "2026-10-19T15:00:00Z" || query.Get("to") != "2026-10-22T15:00:00Z" || query.Get("alert") != alertKey || len(query) != 3 {
		t.Errorf("query %v", query)
	}
}
