package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ngxsetup/internal/system"
)

// FPMStatus is PHP-FPM's own view of one pool's health, queried directly
// over its unix socket rather than through nginx — pool.conf.tmpl's
// pm.status_path is deliberately "reachable only over the unix socket,
// never exposed through a site's server block," and this respects that
// rather than adding a web-reachable location to get at it.
//
// Field names match FPM's own JSON status output exactly (confirmed live
// against a real pool: `{"pool":"...","process manager":"dynamic",...}`),
// so the json tags below are FPM's vocabulary, not this package's.
type FPMStatus struct {
	Pool               string `json:"pool"`
	ProcessManager     string `json:"process manager"`
	StartSince         int64  `json:"start since"`
	AcceptedConn       int64  `json:"accepted conn"`
	ListenQueue        int    `json:"listen queue"`
	MaxListenQueue     int    `json:"max listen queue"`
	IdleProcesses      int    `json:"idle processes"`
	ActiveProcesses    int    `json:"active processes"`
	TotalProcesses     int    `json:"total processes"`
	MaxActiveProcesses int    `json:"max active processes"`
	MaxChildrenReached int    `json:"max children reached"`
	SlowRequests       int64  `json:"slow requests"`
}

// QueryFPMStatus queries one pool's status page over its unix socket via
// cgi-fcgi, the standalone tool built for exactly this — talking FastCGI to
// a socket with no web server in front of it. Confirmed live against a real
// pool's socket before this was written, including the exact JSON field
// names above.
func QueryFPMStatus(ctx context.Context, socketPath, statusPath string) (FPMStatus, error) {
	r := system.Runner{
		ExtraEnv: []string{
			"SCRIPT_NAME=" + statusPath,
			"SCRIPT_FILENAME=" + statusPath,
			"REQUEST_METHOD=GET",
			"QUERY_STRING=json",
		},
	}
	out, err := r.CombinedOutput(ctx, "cgi-fcgi", "-bind", "-connect", socketPath)
	if err != nil {
		return FPMStatus{}, fmt.Errorf("querying %s: %w", socketPath, err)
	}
	return parseFPMStatus(out)
}

// parseFPMStatus splits cgi-fcgi's CGI-style output (headers, a blank line,
// then the body) and decodes the JSON body — kept separate from the network
// call so the parsing itself can be tested against fixed sample output.
func parseFPMStatus(raw string) (FPMStatus, error) {
	body := raw
	if i := strings.Index(raw, "\r\n\r\n"); i >= 0 {
		body = raw[i+4:]
	} else if i := strings.Index(raw, "\n\n"); i >= 0 {
		body = raw[i+2:]
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return FPMStatus{}, fmt.Errorf("empty response (got: %q)", raw)
	}
	var status FPMStatus
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		return FPMStatus{}, fmt.Errorf("parsing status JSON: %w (body: %q)", err, body)
	}
	return status, nil
}
