package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// Scope is what an API key is allowed to do. Two are enough for the split that
// matters: a client acting for a language model retrieves, and nothing that
// model can be talked into must be able to delete a repository or compact an
// index.
type Scope string

const (
	// ScopeRead may call the retrieval and inspection routes.
	ScopeRead Scope = "read"
	// ScopeAdmin adds every mutating and administrative route, and implies read.
	ScopeAdmin Scope = "admin"
)

// Principal is the caller a request authenticated as. Identity is derived from
// the credential, never the credential itself, so it can be logged and used as
// a rate-limit key.
type Principal struct {
	Identity string
	Scope    Scope
}

// Allows reports whether the principal may call a route that requires want.
//
// A nil principal is allowed everything: nil means no authentication is
// configured, and a scope check that failed closed there would take away routes
// an unauthenticated deployment has always served. Scopes divide up an
// authenticated caller's rights; they are not a second gate in front of them.
func (p *Principal) Allows(want Scope) bool {
	switch {
	case p == nil, p.Scope == ScopeAdmin:
		return true
	default:
		return p.Scope == want
	}
}

// Authenticator verifies a request and reports the principal it authenticated.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, bool)
}

// APIKeyAuth authenticates requests using API keys with constant-time comparison.
type APIKeyAuth struct{ keys []scopedKey }

// scopedKey is one configured key: the digest it is compared against, and the
// scope it carries.
type scopedKey struct {
	digest [32]byte
	scope  Scope
}

// NewAPIKeyAuth builds an authenticator over the configured keys. A key may
// carry a scope prefix — see splitScope.
func NewAPIKeyAuth(keys []string) *APIKeyAuth {
	a := &APIKeyAuth{}
	for _, raw := range keys {
		key, scope := splitScope(raw)
		if strings.TrimSpace(key) == "" {
			// A prefix with nothing behind it is a typo. Registering it would
			// mint a key that authenticates whoever presents that same blank.
			continue
		}
		a.keys = append(a.keys, scopedKey{digest: sha256.Sum256([]byte(key)), scope: scope})
	}
	return a
}

// splitScope separates a configured key from the scope prefix it may carry:
// "read:s3cret" is a retrieval-only key, "admin:s3cret" a full one.
//
// An unprefixed key is admin, because that is what every key granted before
// scopes existed: an operator who upgrades without editing the config keeps the
// deployment they had, and opts into the restriction by prefixing a key.
func splitScope(raw string) (string, Scope) {
	switch {
	case strings.HasPrefix(raw, "read:"):
		return strings.TrimPrefix(raw, "read:"), ScopeRead
	case strings.HasPrefix(raw, "admin:"):
		return strings.TrimPrefix(raw, "admin:"), ScopeAdmin
	default:
		return raw, ScopeAdmin
	}
}

// requestAPIKey extracts the presented API key from a request.
func requestAPIKey(r *http.Request) string {
	if key := r.Header.Get("X-API-Key"); key != "" {
		return key
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// Authenticate reports the principal a request presents a valid key for.
func (a *APIKeyAuth) Authenticate(r *http.Request) (*Principal, bool) {
	key := requestAPIKey(r)
	if key == "" {
		return nil, false
	}
	h := sha256.Sum256([]byte(key))
	// Every configured key is compared and none of them exits the loop early,
	// so the time this takes does not say which key matched.
	match := -1
	for i, k := range a.keys {
		if subtle.ConstantTimeCompare(k.digest[:], h[:]) == 1 {
			match = i
		}
	}
	if match < 0 {
		return nil, false
	}
	// Truncated digest: enough to separate callers, never the key itself.
	return &Principal{Identity: hex.EncodeToString(h[:8]), Scope: a.keys[match].scope}, true
}

type ctxKey int

const ctxKeyPrincipal ctxKey = iota

// withPrincipal attaches an authenticated principal to the request context.
func withPrincipal(r *http.Request, p *Principal) *http.Request {
	if p == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal, p))
}

// principalOf returns the principal a request authenticated as, or nil when
// authentication is not configured.
func principalOf(r *http.Request) *Principal {
	p, _ := r.Context().Value(ctxKeyPrincipal).(*Principal)
	return p
}

// identityOf returns the authenticated identity of a request, if any.
func identityOf(r *http.Request) string {
	if p := principalOf(r); p != nil {
		return p.Identity
	}
	return ""
}

// AuthMiddleware wraps an HTTP handler with authentication.
func AuthMiddleware(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := a.Authenticate(r)
			if !ok {
				writeErrorCode(w, http.StatusUnauthorized, CodeUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, withPrincipal(r, p))
		})
	}
}

// requireScope rejects a request whose key does not carry the scope its route
// needs. 403 rather than 404: the caller is authenticated and the route exists,
// and hiding that only makes the failure harder to diagnose than the fact it
// leaks is worth.
func requireScope(want Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !principalOf(r).Allows(want) {
				writeErrorCode(w, http.StatusForbidden, CodeForbidden,
					"this API key does not carry the "+string(want)+" scope this route requires")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
