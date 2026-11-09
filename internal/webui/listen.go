package webui

import (
	"fmt"
	"net"
	"os"
)

// DefaultBind is where `ngxsetup web` listens unless told otherwise: every
// interface. The typical server has no browser of its own, so a
// loopback-only default printed a URL nobody could open without first
// setting up an SSH tunnel. Access is still gated by the per-run sign-in
// token (auth.go), and the port is opened in ufw only for the session.
const DefaultBind = "0.0.0.0"

// ListenURL is one address the web UI can be reached at, with the
// interface it belongs to ("" when the bind address was given directly).
type ListenURL struct {
	Iface string
	URL   string
}

// ResolveBind turns the --bind value into an address net.Listen accepts.
// Besides an IP address (or "localhost"), it accepts a network interface
// name such as "eth1", which resolves to that interface's IPv4 address, or
// its first usable IPv6 one when it has no IPv4 address.
func ResolveBind(bind string) (string, error) {
	if bind == "" {
		return DefaultBind, nil
	}
	if bind == "localhost" || net.ParseIP(bind) != nil {
		return bind, nil
	}
	ifi, err := net.InterfaceByName(bind)
	if err != nil {
		return "", fmt.Errorf("--bind %q is neither an IP address nor a network interface on this machine", bind)
	}
	ips := interfaceIPs(ifi)
	if len(ips) == 0 {
		return "", fmt.Errorf("network interface %s has no usable IP address", bind)
	}
	return ips[0].String(), nil
}

// IsWildcardBind reports whether bind listens on every interface.
func IsWildcardBind(bind string) bool {
	if bind == "" {
		return true
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsUnspecified()
}

// IsLoopbackBind reports whether bind is reachable only from this machine.
func IsLoopbackBind(bind string) bool {
	if bind == "localhost" {
		return true
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

// listenURLs lists the URLs a browser elsewhere can use to reach a listener
// on bind:port: one per address of every interface that is up for a
// wildcard bind, or just the bind address itself otherwise.
func listenURLs(bind string, port int, path string) []ListenURL {
	mk := func(host string) string {
		return fmt.Sprintf("https://%s%s", net.JoinHostPort(host, fmt.Sprint(port)), path)
	}
	if !IsWildcardBind(bind) {
		return []ListenURL{{URL: mk(bind)}}
	}

	var out []ListenURL
	if ifaces, err := net.Interfaces(); err == nil {
		for i := range ifaces {
			ifi := &ifaces[i]
			if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
				continue
			}
			for _, ip := range interfaceIPs(ifi) {
				// A "::" listener also accepts IPv4; a "0.0.0.0" one does not
				// accept IPv6, so do not advertise addresses it cannot serve.
				if ip.To4() == nil && net.ParseIP(bind).To4() != nil {
					continue
				}
				out = append(out, ListenURL{Iface: ifi.Name, URL: mk(ip.String())})
			}
		}
	}
	if len(out) == 0 {
		host := "<this-machine>"
		if h, err := os.Hostname(); err == nil && h != "" {
			host = h
		}
		out = append(out, ListenURL{URL: mk(host)})
	}
	return out
}

// interfaceIPs returns ifi's IPv4 addresses followed by its global-unicast
// IPv6 ones. Link-local IPv6 addresses are left out: a URL for one needs a
// zone (%eth0) that browsers do not accept.
func interfaceIPs(ifi *net.Interface) []net.IP {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var v4, v6 []net.IP
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		if ipn.IP.To4() != nil {
			v4 = append(v4, ipn.IP.To4())
		} else if ipn.IP.IsGlobalUnicast() {
			v6 = append(v6, ipn.IP)
		}
	}
	return append(v4, v6...)
}
