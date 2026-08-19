package api

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimiter limits request rate per authenticated identity or client IP.
type RateLimiter struct {
	mu      sync.Mutex
	clients map[string]*client
	// ratePerSec is the sustained refill rate; burst is the bucket size.
	ratePerSec float64
	burst      float64
	trusted    []netip.Prefix
	ticker     *time.Ticker
	done       chan struct{}
	closeOnce  sync.Once
}

type client struct {
	tokens   float64
	lastSeen time.Time
}

// RateLimiterConfig configures rate limiting.
type RateLimiterConfig struct {
	RequestsPerMinute int
	Burst             int
	// TrustedProxies lists the IPs/CIDRs whose X-Forwarded-For header may be
	// believed. Anything else is keyed on the peer address, because a
	// forwarded header from an untrusted peer is attacker-controlled.
	TrustedProxies []netip.Prefix
}

// NewRateLimiter creates a new rate limiter.
func NewRateLimiter(cfg *RateLimiterConfig) *RateLimiter {
	if cfg.RequestsPerMinute <= 0 {
		cfg.RequestsPerMinute = 60
	}
	if cfg.Burst <= 0 {
		cfg.Burst = cfg.RequestsPerMinute
	}

	rl := &RateLimiter{
		clients:    make(map[string]*client),
		ratePerSec: float64(cfg.RequestsPerMinute) / 60,
		burst:      float64(cfg.Burst),
		trusted:    cfg.TrustedProxies,
		ticker:     time.NewTicker(5 * time.Minute),
		done:       make(chan struct{}),
	}

	go func() {
		for {
			select {
			case <-rl.ticker.C:
				rl.cleanupOldClients()
			case <-rl.done:
				return
			}
		}
	}()

	return rl
}

// Close stops the cleanup goroutine. Repeat calls are no-ops: a limiter can be
// reached from both an explicit shutdown path and a deferred one, and closing
// done twice would panic.
func (rl *RateLimiter) Close() {
	rl.closeOnce.Do(func() {
		close(rl.done)
		rl.ticker.Stop()
	})
}

// Allow reports whether a request from key may proceed. When it may not, the
// second result is how long the caller should wait for one token, which is
// served to the client as Retry-After.
//
// Tokens refill continuously with elapsed time rather than a whole window at
// once, so a client that waits out half its cooldown gets half its quota back
// instead of nothing.
func (rl *RateLimiter) Allow(key string) (bool, time.Duration) {
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	c, exists := rl.clients[key]
	if !exists {
		rl.clients[key] = &client{tokens: rl.burst - 1, lastSeen: now}
		return true, 0
	}

	elapsed := now.Sub(c.lastSeen).Seconds()
	if elapsed > 0 {
		c.tokens = math.Min(rl.burst, c.tokens+elapsed*rl.ratePerSec)
	}
	c.lastSeen = now

	if c.tokens >= 1 {
		c.tokens--
		return true, 0
	}

	wait := time.Duration((1-c.tokens)/rl.ratePerSec*float64(time.Second)) + time.Millisecond
	return false, wait
}

// clientIP returns the address to key an unauthenticated request on. A
// forwarded header is only believed when the immediate peer is a configured
// trusted proxy; otherwise any client could mint a fresh identity per request.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := peerIP(r.RemoteAddr)
	if len(trusted) == 0 || !addrInAny(peer, trusted) {
		return peer.String()
	}

	fwd := r.Header.Get("X-Forwarded-For")
	if fwd == "" {
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			if a, err := netip.ParseAddr(real); err == nil {
				return a.Unmap().String()
			}
		}
		return peer.String()
	}

	// Walk right to left past the trusted hops; the first untrusted address is
	// the closest one the chain cannot have forged.
	parts := strings.Split(fwd, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			continue
		}
		a = a.Unmap()
		if !addrInAny(a, trusted) {
			return a.String()
		}
	}
	return peer.String()
}

func peerIP(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func addrInAny(a netip.Addr, prefixes []netip.Prefix) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ParseTrustedProxies parses IPs and CIDRs into prefixes, ignoring entries it
// cannot understand.
func ParseTrustedProxies(entries []string) []netip.Prefix {
	var out []netip.Prefix
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			out = append(out, p)
			continue
		}
		if a, err := netip.ParseAddr(e); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		}
	}
	return out
}

// rateLimitKey derives the bucket key for a request.
//
// A client-supplied API key is only an identity once it has been
// authenticated. Keying on the raw header regardless of auth meant a caller
// could send a fresh random key per request and never hit the limit at all.
func rateLimitKey(r *http.Request, trusted []netip.Prefix) string {
	if id := identityOf(r); id != "" {
		return "key:" + id
	}
	return "ip:" + clientIP(r, trusted)
}

// RateLimitMiddleware creates rate limiting middleware.
func RateLimitMiddleware(rl *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" || r.URL.Path == "/ready" {
				next.ServeHTTP(w, r)
				return
			}

			ok, retryAfter := rl.Allow(rateLimitKey(r, rl.trusted))
			if !ok {
				seconds := int(math.Ceil(retryAfter.Seconds()))
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeJSON(w, http.StatusTooManyRequests, ErrorResponse{
					Error:             "rate limit exceeded",
					Code:              CodeRateLimited,
					RetryAfterSeconds: seconds,
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// cleanupOldClients removes clients that haven't been seen in a while.
func (rl *RateLimiter) cleanupOldClients() {
	rl.mu.Lock()
	for key, c := range rl.clients {
		if time.Since(c.lastSeen) > 5*time.Minute {
			delete(rl.clients, key)
		}
	}
	rl.mu.Unlock()
}
