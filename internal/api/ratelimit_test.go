package api

import (
	"net/http"
	"net/netip"
	"testing"
	"time"
)

func TestRateLimiterBurst(t *testing.T) {
	rl := NewRateLimiter(&RateLimiterConfig{RequestsPerMinute: 2, Burst: 2})
	defer rl.Close()

	if ok, _ := rl.Allow("k"); !ok {
		t.Fatal("req 1 should pass")
	}
	if ok, _ := rl.Allow("k"); !ok {
		t.Fatal("req 2 should pass (burst=2)")
	}
	ok, retryAfter := rl.Allow("k")
	if ok {
		t.Error("req 3 should be rate-limited")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter = %v, want a positive backoff hint", retryAfter)
	}
}

// TestRateLimiterRefillsProportionally: a client that waits out part of its
// cooldown should get part of its quota back, not nothing until a whole window
// has elapsed.
func TestRateLimiterRefillsProportionally(t *testing.T) {
	rl := NewRateLimiter(&RateLimiterConfig{RequestsPerMinute: 60, Burst: 1}) // 1 token/sec
	defer rl.Close()

	if ok, _ := rl.Allow("k"); !ok {
		t.Fatal("first request should pass")
	}
	if ok, _ := rl.Allow("k"); ok {
		t.Fatal("second immediate request should be limited")
	}

	// Backdate the bucket by a second: exactly one token's worth of time.
	rl.mu.Lock()
	rl.clients["k"].lastSeen = time.Now().Add(-time.Second)
	rl.mu.Unlock()

	if ok, _ := rl.Allow("k"); !ok {
		t.Error("a token should have refilled after one second, not one whole window")
	}
}

// TestRateLimiterCloseIsIdempotent: Close used to close(rl.done) unguarded, so
// a second call panicked. Server.Close reaches it, and an explicit close plus a
// deferred one is one refactor away.
func TestRateLimiterCloseIsIdempotent(t *testing.T) {
	// Recover here rather than letting the panic abort the package's test
	// binary, so a regression fails this one test with a useful message.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second Close panicked: %v", r)
		}
	}()

	rl := NewRateLimiter(&RateLimiterConfig{RequestsPerMinute: 60, Burst: 1})
	rl.Close()
	rl.Close()
}

func TestRateLimitKeyIgnoresUnauthenticatedAPIKey(t *testing.T) {
	// With auth disabled, an X-API-Key header is attacker-controlled: keying
	// on it lets a caller mint a fresh bucket per request.
	r1 := httpRequest(t, "1.2.3.4:1111", map[string]string{"X-API-Key": "random-a"})
	r2 := httpRequest(t, "1.2.3.4:2222", map[string]string{"X-API-Key": "random-b"})

	k1 := rateLimitKey(r1, nil)
	k2 := rateLimitKey(r2, nil)
	if k1 != k2 {
		t.Errorf("keys = %q and %q, want both to fall back to the client IP", k1, k2)
	}
	if k1 != "ip:1.2.3.4" {
		t.Errorf("key = %q, want ip:1.2.3.4", k1)
	}
}

func TestRateLimitKeyUsesAuthenticatedIdentity(t *testing.T) {
	r := httpRequest(t, "1.2.3.4:1111", nil)
	r = withPrincipal(r, &Principal{Identity: "abc123", Scope: ScopeRead})

	if got := rateLimitKey(r, nil); got != "key:abc123" {
		t.Errorf("key = %q, want key:abc123", got)
	}
}

// TestClientIPIgnoresUntrustedForwardedFor: honouring the header from any peer
// is the same bypass in another guise.
func TestClientIPIgnoresUntrustedForwardedFor(t *testing.T) {
	r := httpRequest(t, "203.0.113.9:5000", map[string]string{"X-Forwarded-For": "9.9.9.9"})

	if got := clientIP(r, nil); got != "203.0.113.9" {
		t.Errorf("clientIP with no trusted proxies = %q, want the peer address", got)
	}

	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	if got := clientIP(r, trusted); got != "203.0.113.9" {
		t.Errorf("clientIP from an untrusted peer = %q, want the peer address", got)
	}
}

func TestClientIPHonoursTrustedProxy(t *testing.T) {
	trusted := ParseTrustedProxies([]string{"10.0.0.1"})
	r := httpRequest(t, "10.0.0.1:5000", map[string]string{"X-Forwarded-For": "198.51.100.7"})

	if got := clientIP(r, trusted); got != "198.51.100.7" {
		t.Errorf("clientIP behind a trusted proxy = %q, want the forwarded client", got)
	}
}

// TestClientIPWalksPastTrustedHops: with a chain of proxies the closest
// address the chain cannot have forged is the first untrusted one.
func TestClientIPWalksPastTrustedHops(t *testing.T) {
	trusted := ParseTrustedProxies([]string{"10.0.0.0/8"})
	r := httpRequest(t, "10.0.0.1:5000", map[string]string{
		"X-Forwarded-For": "198.51.100.7, 10.0.0.9, 10.0.0.2",
	})

	if got := clientIP(r, trusted); got != "198.51.100.7" {
		t.Errorf("clientIP = %q, want 198.51.100.7", got)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got := ParseTrustedProxies([]string{"10.0.0.0/8", "192.168.1.1", "", "nonsense"})
	if len(got) != 2 {
		t.Fatalf("parsed %d prefixes, want 2", len(got))
	}
	if !got[0].Contains(netip.MustParseAddr("10.1.2.3")) {
		t.Error("CIDR entry does not match an address inside it")
	}
	if !got[1].Contains(netip.MustParseAddr("192.168.1.1")) {
		t.Error("single-address entry does not match itself")
	}
}

func httpRequest(t *testing.T, remoteAddr string, headers map[string]string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://example.test/api/v1/repos", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}
