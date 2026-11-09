package webui

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// Access control.
//
// Every `ngxsetup web` run generates two random values: a login token, which
// goes into the URL printed in the operator's terminal, and a session value,
// which the browser receives as a cookie in exchange for that token. Nothing
// is served without the cookie — not the API, not a backup download, not
// even the page itself — so reaching the port is not enough; you also need
// to have seen the terminal that started the server. Both values live only
// in this process's memory and die with it: restarting the command signs
// every browser out.

// sessionCookie uses the __Host- prefix, which browsers only accept with
// Secure, Path=/ and no Domain attribute — so the cookie cannot be set or
// overridden by any other host, and is never sent over plain HTTP.
const sessionCookie = "__Host-ngxsetup-session"

// randomSecret returns n random bytes, URL-safe base64-encoded.
func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a random secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// loginPath is the path to open, including this run's login token.
func (s *Server) loginPath() string { return "/login?token=" + s.loginToken }

// handleLogin trades the login token from the terminal for a session cookie,
// then redirects to the panel so the token does not stay in the address bar.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	if s.loginToken == "" || tok == "" || !constantTimeEqual(tok, s.loginToken) {
		writeUnauthorizedPage(w, "That sign-in link is not valid for this run of ngxsetup web. "+
			"Open the URL printed in the terminal that started it — every restart prints a new one.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.session,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// authenticated reports whether a request carries this run's session cookie.
func (s *Server) authenticated(r *http.Request) bool {
	if s.session == "" {
		return false
	}
	c, err := r.Cookie(sessionCookie)
	return err == nil && constantTimeEqual(c.Value, s.session)
}

// requireAuth guards everything except the login endpoint itself.
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			s.handleLogin(w, r)
			return
		}
		if !s.authenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSONError(w, http.StatusUnauthorized,
					"not signed in — open the URL printed in the terminal that started ngxsetup web")
				return
			}
			writeUnauthorizedPage(w, "Open the URL printed in the terminal that started ngxsetup web. "+
				"It contains the sign-in token for this run.")
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeUnauthorizedPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>ngxsetup</title>`+
		`<body style="font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;color:#334155">`+
		`<h1 style="font-size:1.25rem">Sign-in required</h1><p>%s</p></body>`, msg)
}
