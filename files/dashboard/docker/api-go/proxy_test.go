package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The proxy handling decides two security properties at once: whether HSTS is
// sent, and which address the login rate limiter counts against. A mistake in
// either is silent, so each branch is pinned here.

func req(remote string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "http://lab.local/", nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestSchemeDirectTLS(t *testing.T) {
	r := req("192.168.23.50:4444", nil)
	r.TLS = &tls.ConnectionState{}
	if got := requestScheme(r); got != "https" {
		t.Fatalf("direct TLS: want https, got %q", got)
	}
}

func TestSchemePlainHTTP(t *testing.T) {
	if got := requestScheme(req("192.168.23.50:4444", nil)); got != "http" {
		t.Fatalf("plain: want http, got %q", got)
	}
}

func TestSchemeProxyTerminatedTLS(t *testing.T) {
	// 172.20.0.1 is the connector on the docker bridge: a trusted proxy.
	r := req("172.20.0.1:5000", map[string]string{"X-Forwarded-Proto": "https"})
	if got := requestScheme(r); got != "https" {
		t.Fatalf("proxied TLS: want https, got %q", got)
	}
}

func TestSchemeIgnoresHeaderFromUntrustedPeer(t *testing.T) {
	// 203.0.113.9 is a public address, not one of our proxies. A caller that
	// sets X-Forwarded-Proto itself must not be believed, or HSTS could be
	// forced on over plain HTTP and the site would break for that client.
	r := req("203.0.113.9:5000", map[string]string{"X-Forwarded-Proto": "https"})
	if got := requestScheme(r); got != "http" {
		t.Fatalf("untrusted peer: want http, got %q", got)
	}
}

func TestClientIPBehindProxy(t *testing.T) {
	r := req("172.20.0.1:5000", map[string]string{"X-Forwarded-For": "198.51.100.7"})
	if got := clientIP(r); got != "198.51.100.7" {
		t.Fatalf("want 198.51.100.7, got %q", got)
	}
}

func TestClientIPSkipsTrustedHops(t *testing.T) {
	// A chain of our own proxies: the rightmost untrusted entry is the client.
	r := req("172.20.0.1:5000", map[string]string{
		"X-Forwarded-For": "198.51.100.7, 172.20.0.9",
	})
	if got := clientIP(r); got != "198.51.100.7" {
		t.Fatalf("want 198.51.100.7, got %q", got)
	}
}

func TestClientIPIgnoresForgedHeader(t *testing.T) {
	// The peer is not a proxy, so the header is attacker-controlled and the
	// rate limiter must key on the real socket address. Trusting it would let
	// an attacker pick a fresh IP per attempt and never be blocked.
	r := req("203.0.113.9:5000", map[string]string{"X-Forwarded-For": "1.2.3.4"})
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("want 203.0.113.9, got %q", got)
	}
}

func TestClientIPFallsBackWhenOnlyTrustedHops(t *testing.T) {
	// Every X-Forwarded-For entry is itself one of our proxies, so there is no
	// real client in the chain and the direct peer is the honest answer.
	r := req("172.20.0.1:5000", map[string]string{"X-Forwarded-For": "172.20.0.9"})
	if got := clientIP(r); got != "172.20.0.1" {
		t.Fatalf("want the direct peer, got %q", got)
	}
}

// The default trusted set is the docker bridge only. A lab client on the LAN
// reaches :8080 directly and must never be believed when it sets the header,
// or it could rotate its own IP and never trip the rate limiter.
func TestLANClientIsNotTrusted(t *testing.T) {
	r := req("192.168.23.50:5000", map[string]string{"X-Forwarded-For": "1.2.3.4"})
	if got := clientIP(r); got != "192.168.23.50" {
		t.Fatalf("LAN client must not be trusted, got %q", got)
	}
	if got := requestScheme(req("192.168.23.50:1",
		map[string]string{"X-Forwarded-Proto": "https"})); got != "http" {
		t.Fatalf("LAN client must not set the scheme, got %q", got)
	}
}

func TestTrustedProxyDefaultIncludesBridge(t *testing.T) {
	// 172.20.0.1 is where the connector peers from.
	if !fromTrustedProxy(req("172.20.0.1:1", nil)) {
		t.Fatal("172.20.0.1 must be a trusted proxy")
	}
	if fromTrustedProxy(req("172.21.0.1:1", nil)) {
		t.Fatal("172.21.0.1 is outside lab_default and must not be trusted")
	}
}

// HSTS must appear exactly when the request reached us over TLS, including via
// the proxy - that is the whole point of this change.
func TestHSTSHeaderEmitted(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		proto  string
		tls    bool
		want   bool
	}{
		{"direct tls", "192.168.23.50:1", "", true, true},
		{"proxied tls", "172.20.0.1:1", "https", false, true},
		{"plain http", "192.168.23.50:1", "", false, false},
		{"forged proto", "203.0.113.9:1", "https", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := req(c.remote, nil)
			if c.proto != "" {
				r.Header.Set("X-Forwarded-Proto", c.proto)
			}
			if c.tls {
				r.TLS = &tls.ConnectionState{}
			}
			got := requestScheme(r) == "https"
			if got != c.want {
				t.Fatalf("HSTS sent=%v, want %v", got, c.want)
			}
		})
	}
}
