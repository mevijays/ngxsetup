package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/provision"
	"ngxsetup/internal/state"
)

// ---- WordPress (wp-cli) ---------------------------------------------------------
//
// Every call here goes through provision.RunWP/QueryWP, which run wp-cli as
// the site's own account inside its PHP sandbox — the same path `ngxsetup wp`
// takes — so the browser gets no more reach into the machine than the CLI.

// wpOverview is one site's WordPress state for the WordPress page.
type wpOverview struct {
	Installed bool              `json:"installed"`
	Version   string            `json:"version,omitempty"`
	Updates   []json.RawMessage `json:"core_updates"`
	Plugins   []json.RawMessage `json:"plugins"`
	Themes    []json.RawMessage `json:"themes"`
	Users     []json.RawMessage `json:"users"`
	Errors    []string          `json:"errors,omitempty"`
}

func (s *Server) handleWPOverview(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rec, err := c.WPSite(r.PathValue("domain"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, loadWPOverview(r.Context(), c, *rec))
}

func loadWPOverview(ctx context.Context, c *provision.Ctx, rec state.Site) wpOverview {
	ov := wpOverview{Updates: []json.RawMessage{}, Plugins: []json.RawMessage{}, Themes: []json.RawMessage{}, Users: []json.RawMessage{}}
	if _, err := c.QueryWP(ctx, rec, "core", "is-installed"); err != nil {
		return ov // WordPress's installer has not been run yet
	}
	ov.Installed = true

	// Independent queries, each of which boots WordPress: run them together.
	var mu sync.Mutex
	var wg sync.WaitGroup
	list := func(dst *[]json.RawMessage, label string, args ...string) {
		defer wg.Done()
		out, err := c.QueryWP(ctx, rec, args...)
		var items []json.RawMessage
		if err == nil {
			err = json.Unmarshal([]byte(jsonPart(out)), &items)
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			ov.Errors = append(ov.Errors, fmt.Sprintf("%s: %v", label, err))
			return
		}
		if items != nil {
			*dst = items
		}
	}
	wg.Add(5)
	go func() {
		defer wg.Done()
		out, err := c.QueryWP(ctx, rec, "core", "version")
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			ov.Errors = append(ov.Errors, fmt.Sprintf("core version: %v", err))
			return
		}
		ov.Version = strings.TrimSpace(out)
	}()
	go list(&ov.Updates, "core updates", "core", "check-update", "--format=json")
	go list(&ov.Plugins, "plugins", "plugin", "list", "--format=json",
		"--fields=name,title,status,version,update,update_version")
	go list(&ov.Themes, "themes", "theme", "list", "--format=json",
		"--fields=name,title,status,version,update,update_version")
	go list(&ov.Users, "users", "user", "list", "--format=json",
		"--fields=ID,user_login,user_email,roles")
	wg.Wait()
	return ov
}

// jsonPart drops anything wp-cli printed before its JSON (a PHP notice from a
// plugin, say), which would otherwise make the whole list unparseable.
// `core check-update` prints nothing at all when there is no update.
func jsonPart(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return "[]"
	}
	if i := strings.IndexAny(out, "[{"); i > 0 {
		out = out[i:]
	}
	return out
}

type wpRunRequest struct {
	Args []string `json:"args"`
}

const (
	maxWPArgs   = 64
	maxWPArgLen = 4096
)

// handleWPRun runs one wp-cli command, given as an argument list (never a
// shell string — nothing here passes through a shell).
func (s *Server) handleWPRun(w http.ResponseWriter, r *http.Request) {
	var req wpRunRequest
	if err := readJSON(r, &req); err != nil || len(req.Args) == 0 {
		writeJSONError(w, http.StatusBadRequest, "args: a non-empty list of wp-cli arguments is required")
		return
	}
	if len(req.Args) > maxWPArgs {
		writeJSONError(w, http.StatusBadRequest, "too many arguments")
		return
	}
	for _, a := range req.Args {
		if len(a) > maxWPArgLen || strings.ContainsRune(a, 0) {
			writeJSONError(w, http.StatusBadRequest, "an argument is too long or contains a NUL byte")
			return
		}
	}
	domain := r.PathValue("domain")
	var wpOut string
	output, err := runCaptured(func() error {
		c, err := newCtx(actionContext(r), false)
		if err != nil {
			return err
		}
		rec, err := c.WPSite(domain)
		if err != nil {
			return err
		}
		logx.Step("wp %s", strings.Join(req.Args, " "))
		wpOut, err = c.RunWP(c.Context, *rec, "", req.Args...)
		if err != nil {
			return fmt.Errorf("wp-cli failed: %w", err)
		}
		return nil
	})
	writeActionResult(w, output+wpOut, err, nil)
}

type wpResetPasswordRequest struct {
	User string `json:"user"`
}

// handleWPResetPassword sets a new random password on a WordPress account and
// returns it once. The password reaches wp-cli on stdin (--prompt), never as
// an argument a process listing could show.
func (s *Server) handleWPResetPassword(w http.ResponseWriter, r *http.Request) {
	var req wpResetPasswordRequest
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.User) == "" {
		writeJSONError(w, http.StatusBadRequest, "user is required")
		return
	}
	if strings.HasPrefix(req.User, "-") {
		writeJSONError(w, http.StatusBadRequest, "invalid user")
		return
	}
	domain := r.PathValue("domain")
	var data any
	output, err := runCaptured(func() error {
		c, err := newCtx(actionContext(r), false)
		if err != nil {
			return err
		}
		rec, err := c.WPSite(domain)
		if err != nil {
			return err
		}
		password, err := provision.GenerateWPPassword()
		if err != nil {
			return err
		}
		out, err := c.RunWP(c.Context, *rec, password+"\n", "user", "update", req.User, "--prompt=user_pass")
		if err != nil {
			return fmt.Errorf("wp-cli failed: %w\n%s", err, out)
		}
		logx.Change("set a new password for %s on %s", req.User, rec.Domain)
		data = map[string]string{"user": req.User, "password": password}
		return nil
	})
	writeActionResult(w, output, err, data)
}
