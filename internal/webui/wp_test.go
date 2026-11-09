package webui

import (
	"net/http"
	"strings"
	"testing"
)

func TestJSONPartSkipsNoticesBeforeTheJSON(t *testing.T) {
	cases := map[string]string{
		"":                                   "[]",
		"[]":                                 "[]",
		`[{"name":"akismet"}]`:               `[{"name":"akismet"}]`,
		"PHP Notice: something\n[{\"a\":1}]": `[{"a":1}]`,
	}
	for in, want := range cases {
		if got := jsonPart(in); got != want {
			t.Errorf("jsonPart(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWPRunValidatesArguments(t *testing.T) {
	h := newTestServer().routes()
	hdr := map[string]string{"X-Requested-With": "ngxsetup-web"}
	for _, body := range []string{
		`{"args":[]}`,
		`{"args":["plugin","` + strings.Repeat("x", maxWPArgLen+1) + `"]}`,
		`{"args":["a\u0000b"]}`,
		`{"cmd":"plugin list"}`,
	} {
		rec := doJSON(t, h, "POST", "/api/sites/blog.example/wp/run", body, hdr)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %.40s… = %d, want 400", body, rec.Code)
		}
	}
	rec := doJSON(t, h, "POST", "/api/sites/blog.example/wp/reset-password", `{"user":"--role=administrator"}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a user name that looks like a flag was accepted (%d)", rec.Code)
	}
}

func TestWPEndpointsRequireTheFetchHeader(t *testing.T) {
	h := newTestServer().routes()
	for _, path := range []string{"/api/sites/blog.example/wp/run", "/api/sites/blog.example/wp/reset-password"} {
		if rec := doJSON(t, h, "POST", path, `{"args":["plugin","list"],"user":"a"}`, nil); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without X-Requested-With = %d, want 403", path, rec.Code)
		}
	}
}
