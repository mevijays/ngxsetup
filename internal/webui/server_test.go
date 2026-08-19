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
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// There is no login (see the package doc for why), so the only thing this
// package's own tests can exercise without touching the real system via
// provision.New is the one guard that survives that decision: the
// X-Requested-With header on mutating requests. Handlers that call into
// provision.New touch the real system by design, the same way every CLI
// command does, and are exercised live against the LXC test box instead of
// through `go test` on a developer's own machine.
func TestMutatingEndpointRequiresFetchHeader(t *testing.T) {
	s := &Server{}
	h := s.routes()

	rec := doJSON(t, h, "POST", "/api/config", `{"key":"timezone","value":"UTC"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status without X-Requested-With = %d, want 403", rec.Code)
	}
}

func TestMutatingEndpointAcceptsTheFetchHeader(t *testing.T) {
	s := &Server{}
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
	s := &Server{}
	h := s.routes()

	rec := doJSON(t, h, "GET", "/api/logs/sources", "", nil)
	if rec.Code == http.StatusForbidden {
		t.Error("a read-only GET endpoint required the mutation-only fetch header")
	}
}
