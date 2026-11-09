package webui

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"sync"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/provision"
)

// actionMu serializes every mutating action the web UI performs.
//
// provision.Ctx and logx are both built around the assumption that one
// command runs at a time in one process — true for the CLI by construction,
// but not for an HTTP server that can receive two requests at once. Rather
// than thread a Ctx and a log sink through every layer as a request-scoped
// value, one global lock makes that same assumption true here too: at most
// one provisioning action is ever in flight, exactly as if two operators were
// taking turns at the same terminal. Read-only requests (status, site list,
// stats) do not take this lock — only anything that calls into provision.New
// with intent to change the machine does.
var actionMu sync.Mutex

// runCaptured runs fn with logx redirected to a buffer, and returns
// everything it printed alongside fn's error. This is what lets the web UI
// show the exact same transcript the CLI would have printed for the same
// command, instead of re-deriving a separate human-readable summary for
// every action.
func runCaptured(fn func() error) (output string, err error) {
	actionMu.Lock()
	defer actionMu.Unlock()

	var buf bytes.Buffer
	logx.SetOutput(&buf, &buf)
	defer logx.SetOutput(os.Stdout, os.Stderr)
	logx.ResetWarnings()

	err = fn()
	return buf.String(), err
}

// actionContext is the context a mutating action runs under: the request's,
// minus its cancellation. A request's context is cancelled the moment the
// browser disconnects — a closed tab, a laptop going to sleep, a proxy
// timeout — and every command the action is running (apt, mysql, certbot)
// is killed with it, leaving whatever it was doing half-done. An action,
// once started, runs to completion instead, the same as a CLI command whose
// terminal stays open; the per-command timeouts in system.Runner still
// bound it.
func actionContext(r *http.Request) context.Context {
	return context.WithoutCancel(r.Context())
}

// newCtx builds a provision.Ctx the same way the CLI does for one command.
func newCtx(ctx context.Context, dryRun bool) (*provision.Ctx, error) {
	return provision.New(ctx, provision.Options{DryRun: dryRun})
}
