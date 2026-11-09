package webui

import (
	"net"
	"strings"
	"testing"
)

func TestResolveBind(t *testing.T) {
	for _, in := range []string{"0.0.0.0", "127.0.0.1", "::", "10.1.2.3", "localhost"} {
		got, err := ResolveBind(in)
		if err != nil || got != in {
			t.Errorf("ResolveBind(%q) = %q, %v; want it unchanged", in, got, err)
		}
	}
	if got, err := ResolveBind(""); err != nil || got != DefaultBind {
		t.Errorf("ResolveBind(\"\") = %q, %v; want %q", got, err, DefaultBind)
	}
	if _, err := ResolveBind("no-such-iface0"); err == nil {
		t.Error("ResolveBind accepted an interface that does not exist")
	}

	// A real interface name resolves to one of its own addresses.
	ifaces, _ := net.Interfaces()
	for i := range ifaces {
		ips := interfaceIPs(&ifaces[i])
		if len(ips) == 0 {
			continue
		}
		got, err := ResolveBind(ifaces[i].Name)
		if err != nil || got != ips[0].String() {
			t.Errorf("ResolveBind(%q) = %q, %v; want %s", ifaces[i].Name, got, err, ips[0])
		}
		break
	}
}

func TestBindKinds(t *testing.T) {
	if DefaultBind != "0.0.0.0" || !IsWildcardBind(DefaultBind) {
		t.Errorf("default bind %q should listen on every interface", DefaultBind)
	}
	for _, b := range []string{"0.0.0.0", "::", ""} {
		if !IsWildcardBind(b) || IsLoopbackBind(b) {
			t.Errorf("%q: want wildcard, not loopback", b)
		}
	}
	for _, b := range []string{"127.0.0.1", "::1", "localhost"} {
		if !IsLoopbackBind(b) || IsWildcardBind(b) {
			t.Errorf("%q: want loopback, not wildcard", b)
		}
	}
}

func TestListenURLs(t *testing.T) {
	const path = "/login?token=abc"

	got := listenURLs("10.1.2.3", 8443, path)
	if len(got) != 1 || got[0].URL != "https://10.1.2.3:8443"+path {
		t.Errorf("specific bind: got %+v", got)
	}

	got = listenURLs("0.0.0.0", 8443, path)
	if len(got) == 0 {
		t.Fatal("wildcard bind produced no URLs")
	}
	for _, u := range got {
		if !strings.HasPrefix(u.URL, "https://") || !strings.HasSuffix(u.URL, ":8443"+path) {
			t.Errorf("malformed URL %q", u.URL)
		}
		if strings.Contains(u.URL, "127.0.0.1") || strings.Contains(u.URL, "[") {
			t.Errorf("0.0.0.0 bind advertised a loopback or IPv6 URL: %q", u.URL)
		}
	}
}
