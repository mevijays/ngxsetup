package provision

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// httpGet fetches a small text resource with a bounded body size.
//
// The size cap matters: these responses are written into configuration files,
// and an unbounded read of a compromised or misdirected endpoint would be an
// unbounded write to /etc.
func httpGet(url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// download saves a URL to a file, verifying the size is plausible.
func download(url, dest string, mode os.FileMode, maxBytes int64) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	tmp := dest + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if n > maxBytes {
		os.Remove(tmp)
		return fmt.Errorf("%s is larger than the expected maximum of %d bytes", url, maxBytes)
	}
	if n == 0 {
		os.Remove(tmp)
		return fmt.Errorf("%s returned an empty response", url)
	}
	// Rename only after a complete download, so an interrupted transfer never
	// leaves a truncated archive in place of a good one.
	return os.Rename(tmp, dest)
}

// validateCIDR checks a network in the form accepted by nginx's
// set_real_ip_from, guarding text that is about to be written into a config
// file from a remote source.
func validateCIDR(s string) error {
	if _, _, err := net.ParseCIDR(s); err != nil {
		if ip := net.ParseIP(s); ip == nil {
			return fmt.Errorf("%q is not an IP address or CIDR range", s)
		}
	}
	return nil
}
