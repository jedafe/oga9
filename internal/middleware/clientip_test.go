package middleware

import (
	"net"
	"net/http"
	"testing"
)

func req(remote, xff string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIPOf(t *testing.T) {
	_, proxyNet, _ := net.ParseCIDR("10.0.0.0/8")
	trusted := []*net.IPNet{proxyNet}

	cases := []struct{ name, remote, xff, want string }{
		{"direct untrusted peer", "203.0.113.5:4000", "", "203.0.113.5"},
		{"untrusted peer — xff ignored", "203.0.113.5:4000", "1.2.3.4", "203.0.113.5"},
		{"trusted peer, single client hop", "10.0.0.9:4000", "198.51.100.7", "198.51.100.7"},
		// attacker-supplied leftmost must not win: the last untrusted hop
		// before our proxies is the attributable client.
		{"leftmost spoof loses", "10.0.0.9:4000", "1.1.1.1, 198.51.100.7", "198.51.100.7"},
		{"two proxies", "10.0.0.9:4000", "1.1.1.1, 198.51.100.7, 10.0.0.2", "198.51.100.7"},
		{"all hops trusted", "10.0.0.9:4000", "10.0.0.1, 10.0.0.2", "10.0.0.1"},
		{"unparseable hop is untrusted", "10.0.0.9:4000", "garbage, 10.0.0.2", "garbage"},
	}
	for _, c := range cases {
		if got := ClientIPOf(req(c.remote, c.xff), trusted); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
