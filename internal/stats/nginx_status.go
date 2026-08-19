package stats

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// NginxStatus is nginx's own connection/request counters, from its
// stub_status module. Field names follow stub_status's own output:
//
//	Active connections: 1
//	server accepts handled requests
//	 16 16 16
//	Reading: 0 Writing: 1 Waiting: 0
type NginxStatus struct {
	Active   int
	Accepts  int64
	Handled  int64
	Requests int64
	Reading  int
	Writing  int
	Waiting  int
}

// nginxStatusTimeout bounds the local HTTP call — this is a loopback
// request to nginx's own status page, which should answer in microseconds;
// anything slower means nginx itself is in trouble, not worth waiting long
// to find out.
const nginxStatusTimeout = 2 * time.Second

// QueryNginxStatus fetches and parses nginx's stub_status output from the
// loopback-only location 00-core.conf.tmpl defines. Returns a clear error
// (not a panic or a zero-value success) when nginx isn't reachable or the
// location isn't there — an older config from before this feature existed,
// say — so callers can show "unavailable" rather than silently zero.
func QueryNginxStatus(ctx context.Context) (NginxStatus, error) {
	client := &http.Client{Timeout: nginxStatusTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/ngxsetup-nginx-status", nil)
	if err != nil {
		return NginxStatus{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return NginxStatus{}, fmt.Errorf("querying nginx status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return NginxStatus{}, fmt.Errorf("nginx status returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return NginxStatus{}, err
	}
	return parseNginxStatus(string(body))
}

// parseNginxStatus is the text-parsing half, kept separate from the network
// call so it can be tested against fixed sample output without a running
// nginx.
func parseNginxStatus(raw string) (NginxStatus, error) {
	var status NginxStatus
	lines := strings.Split(raw, "\n")
	found := false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Active connections:"):
			status.Active, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Active connections:")))
			found = true
		case strings.HasPrefix(line, "Reading:"):
			// "Reading: 0 Writing: 1 Waiting: 0"
			fields := strings.Fields(line)
			for j := 0; j+1 < len(fields); j += 2 {
				v, _ := strconv.Atoi(fields[j+1])
				switch strings.TrimSuffix(fields[j], ":") {
				case "Reading":
					status.Reading = v
				case "Writing":
					status.Writing = v
				case "Waiting":
					status.Waiting = v
				}
			}
		default:
			// The accepts/handled/requests triplet is the line right after
			// the "server accepts handled requests" header, three
			// whitespace-separated integers with no other identifying text.
			if i > 0 && strings.Contains(lines[i-1], "accepts") && strings.Contains(lines[i-1], "handled") {
				fields := strings.Fields(line)
				if len(fields) == 3 {
					status.Accepts, _ = strconv.ParseInt(fields[0], 10, 64)
					status.Handled, _ = strconv.ParseInt(fields[1], 10, 64)
					status.Requests, _ = strconv.ParseInt(fields[2], 10, 64)
				}
			}
		}
	}
	if !found {
		return NginxStatus{}, fmt.Errorf("unrecognised stub_status output: %q", raw)
	}
	return status, nil
}
