package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doJSON(t *testing.T, h http.Handler, method, path, body string, extraHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: testSession})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const testSession = "test-session-value"

// newTestServer returns a Server whose session cookie doJSON already sends.
func newTestServer() *Server { return &Server{loginToken: "test-login-token", session: testSession} }

// The access guards — the session cookie and the X-Requested-With header on
// mutating requests — are what this package's own tests can exercise
// without touching the real system via provision.New. Handlers that call into
// provision.New touch the real system by design, the same way every CLI
// command does, and are exercised live against the LXC test box instead of
// through `go test` on a developer's own machine.
func TestMutatingEndpointRequiresFetchHeader(t *testing.T) {
	s := newTestServer()
	h := s.routes()

	rec := doJSON(t, h, "POST", "/api/config", `{"key":"timezone","value":"UTC"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status without X-Requested-With = %d, want 403", rec.Code)
	}
}

func TestMutatingEndpointAcceptsTheFetchHeader(t *testing.T) {
	s := newTestServer()
	h := s.routes()

	rec := doJSON(t, h, "POST", "/api/cache/purge", `{}`, map[string]string{"X-Requested-With": "ngxsetup-web"})
	// This reaches provision.New, which is expected to behave like any CLI
	// invocation on whatever machine runs `go test` — it may succeed or
	// fail depending on the local environment, but it must not be rejected
	// by the header guard (403) once the header is present.
	if rec.Code == http.StatusForbidden {
		t.Error("request with the correct X-Requested-With header was still rejected as missing it")
	}
}

// Read-only endpoints must not require the fetch header — only mutations do.
func TestReadOnlyEndpointsDoNotRequireTheFetchHeader(t *testing.T) {
	s := newTestServer()
	h := s.routes()

	rec := doJSON(t, h, "GET", "/api/logs/sources", "", nil)
	if rec.Code == http.StatusForbidden {
		t.Error("a read-only GET endpoint required the mutation-only fetch header")
	}
}

func TestEverythingRequiresTheSessionCookie(t *testing.T) {
	h := newTestServer().routes()
	for _, path := range []string{"/", "/app.js", "/api/status", "/api/backups/download?path=x.sql"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, rec.Code)
		}
	}

	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "a-guess"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong session value was accepted (%d)", rec.Code)
	}
}

func TestEmptySessionNeverAuthenticates(t *testing.T) {
	h := (&Server{}).routes()
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: ""})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a server with no session configured let a request through (%d)", rec.Code)
	}
}

func TestLoginTradesTheTokenForASecureCookie(t *testing.T) {
	s := newTestServer()
	h := s.routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login?token=wrong", nil))
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("a wrong token signed in: status %d, cookies %v", rec.Code, rec.Result().Cookies())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", s.loginPath(), nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("login = %d (Location %q), want a 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != sessionCookie || c.Value != testSession || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie is not locked down: %+v", c)
	}
}
