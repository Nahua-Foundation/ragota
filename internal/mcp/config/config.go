// Package config resolves where ragota is and how this server talks to it.
//
// MCP servers are launched by their client with a static JSON block, so the
// environment is the only configuration channel that always exists. Flags
// complement it for the operator testing a setup by hand; the API key is
// deliberately not among them, because a flag lands in the process table.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is where a ragota started from its own README listens.
	DefaultBaseURL = "http://localhost:8080"

	// DefaultTimeout bounds one whole tool call, retries included.
	//
	// It matches the server's write timeout rather than an agent's patience:
	// /context runs retrieval, graph expansion and possibly reranking inside one
	// request, and a client that gives up first turns a slow answer into no
	// answer. Lower it with RAGOTA_TIMEOUT where a fast failure is worth more
	// than a slow answer.
	DefaultTimeout = 120 * time.Second

	// DefaultMaxBytes is the response budget the retrieval tools send when the
	// caller names none. See the tool descriptions for what it buys.
	DefaultMaxBytes = 16 << 10
)

// Auth styles. The server accepts either; the choice is about what survives the
// hop, not about what it understands.
const (
	// AuthAPIKey sends X-API-Key.
	AuthAPIKey = "api-key"
	// AuthBearer sends Authorization: Bearer, for a gateway in front of
	// ragota that forwards Authorization and drops headers it does not know.
	AuthBearer = "bearer"
)

// Config is everything this server needs to reach ragota.
type Config struct {
	// BaseURL carries a scheme and a host ("http://localhost:8080").
	BaseURL string
	// APIKey is the credential itself. The "read:"/"admin:" prefix in the
	// server's api_keys is the operator's record of what the key may do and is
	// not part of it.
	APIKey string
	// AuthStyle is AuthAPIKey or AuthBearer.
	AuthStyle string
	// Timeout bounds one tool call end to end.
	Timeout time.Duration
	// Repos is the default repository scope for the retrieval tools, applied
	// only when a call names none. It exists because a client's launch block is
	// the one place that knows which repositories this workspace is about, and a
	// model cannot guess repository ids.
	Repos []string
	// MaxBytes is the default response budget for the retrieval tools.
	MaxBytes int
}

// Env is the environment a Config is read from. Tests pass their own; main
// passes os.LookupEnv.
type Env func(string) (string, bool)

// OSEnv reads the process environment.
func OSEnv(key string) (string, bool) { return os.LookupEnv(key) }

// FromEnv builds a Config from the environment, filling in defaults. It does not
// contact the server; Validate checks the shape, and the caller checks reachability.
func FromEnv(env Env) (*Config, error) {
	c := &Config{
		BaseURL:   lookup(env, "RAGOTA_URL", DefaultBaseURL),
		APIKey:    apiKey(env),
		AuthStyle: strings.ToLower(lookup(env, "RAGOTA_AUTH_STYLE", AuthAPIKey)),
		Timeout:   DefaultTimeout,
		MaxBytes:  DefaultMaxBytes,
		Repos:     splitList(lookup(env, "RAGOTA_REPOS", "")),
	}
	if v, ok := env("RAGOTA_TIMEOUT"); ok && strings.TrimSpace(v) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("RAGOTA_TIMEOUT: %q is not a duration such as \"90s\": %w", v, err)
		}
		c.Timeout = d
	}
	if v, ok := env("RAGOTA_MAX_BYTES"); ok && strings.TrimSpace(v) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("RAGOTA_MAX_BYTES: %q is not a number: %w", v, err)
		}
		c.MaxBytes = n
	}
	return c, nil
}

// apiKey reads the credential, preferring the read-scoped name.
//
// ragota's own deployment docs name the two keys RAGOTA_MCP_KEY (granted
// "read:") and RAGOTA_API_KEY (granted "admin:"), and a compose file commonly
// exports both. Reading RAGOTA_API_KEY first would then hand a model-facing
// process the key that can delete a repository — the one outcome the scopes
// exist to prevent — so the narrower name wins when both are set.
func apiKey(env Env) string {
	if v := lookup(env, "RAGOTA_MCP_KEY", ""); v != "" {
		return v
	}
	return lookup(env, "RAGOTA_API_KEY", "")
}

// Validate reports a configuration that cannot work, before anything is dialled.
func (c *Config) Validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("RAGOTA_URL: %q is not a URL: %w", c.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("RAGOTA_URL: %q needs an http or https scheme (for example %s)", c.BaseURL, DefaultBaseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("RAGOTA_URL: %q names no host (for example %s)", c.BaseURL, DefaultBaseURL)
	}
	switch c.AuthStyle {
	case AuthAPIKey, AuthBearer:
	default:
		return fmt.Errorf("RAGOTA_AUTH_STYLE: %q is neither %q nor %q", c.AuthStyle, AuthAPIKey, AuthBearer)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("RAGOTA_TIMEOUT: %s is not a positive duration", c.Timeout)
	}
	if c.MaxBytes < 0 {
		return fmt.Errorf("RAGOTA_MAX_BYTES: %d is negative", c.MaxBytes)
	}
	return nil
}

// Redacted renders the configuration for a log line, without the credential.
func (c *Config) Redacted() string {
	key := "none"
	if c.APIKey != "" {
		key = c.AuthStyle
	}
	repos := "all"
	if len(c.Repos) > 0 {
		repos = strings.Join(c.Repos, ",")
	}
	return fmt.Sprintf("url=%s auth=%s timeout=%s max_bytes=%d repos=%s",
		c.BaseURL, key, c.Timeout, c.MaxBytes, repos)
}

func lookup(env Env, key, fallback string) string {
	if v, ok := env(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

// splitList reads a comma-separated list, dropping blanks so that a trailing
// comma or an empty variable does not become a repository named "".
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
