package config

import (
	"strings"
	"testing"
	"time"
)

// env builds a lookup over a literal map, so that a test never touches the
// process environment and the cases can run in parallel.
func env(pairs map[string]string) Env {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

func TestFromEnvDefaults(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.BaseURL != DefaultBaseURL || c.Timeout != DefaultTimeout || c.MaxBytes != DefaultMaxBytes {
		t.Fatalf("defaults are %+v", c)
	}
	if c.AuthStyle != AuthAPIKey {
		t.Errorf("auth style default is %q", c.AuthStyle)
	}
	if c.APIKey != "" || c.Repos != nil {
		t.Errorf("nothing should be assumed about the key or the scope: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the default configuration does not validate: %v", err)
	}
}

// ragota's deployment docs name RAGOTA_MCP_KEY the read-scoped key and
// RAGOTA_API_KEY the admin one, and a compose file commonly exports both.
// Reading the admin one in preference would hand a model-facing process the key
// that can delete a repository — the one outcome the scopes exist to prevent.
func TestTheNarrowerKeyWins(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"RAGOTA_MCP_KEY": "read-key",
		"RAGOTA_API_KEY": "admin-key",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.APIKey != "read-key" {
		t.Fatalf("picked %q; RAGOTA_MCP_KEY must win over RAGOTA_API_KEY", c.APIKey)
	}
}

func TestAPIKeyFallsBackToTheGeneralName(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"RAGOTA_API_KEY": "only-key"}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.APIKey != "only-key" {
		t.Fatalf("APIKey is %q", c.APIKey)
	}
}

func TestFromEnvReadsEveryKnob(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"RAGOTA_URL":        "https://ragota.internal:9443/",
		"RAGOTA_AUTH_STYLE": "Bearer",
		"RAGOTA_TIMEOUT":    "45s",
		"RAGOTA_MAX_BYTES":  "8192",
		"RAGOTA_REPOS":      " orders , ,billing, ",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.BaseURL != "https://ragota.internal:9443/" {
		t.Errorf("BaseURL is %q", c.BaseURL)
	}
	if c.AuthStyle != AuthBearer {
		t.Errorf("auth style is %q; the value should be case-insensitive", c.AuthStyle)
	}
	if c.Timeout != 45*time.Second || c.MaxBytes != 8192 {
		t.Errorf("timeout %s max_bytes %d", c.Timeout, c.MaxBytes)
	}
	// A trailing comma or a blank entry must not become a repository named "",
	// which would scope every query to nothing.
	if len(c.Repos) != 2 || c.Repos[0] != "orders" || c.Repos[1] != "billing" {
		t.Errorf("repos parsed as %q", c.Repos)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestFromEnvIgnoresBlankValues(t *testing.T) {
	// An MCP client's launch block often exports a variable it has no value for.
	// An empty string must mean "unset", not "the base URL is the empty string".
	c, err := FromEnv(env(map[string]string{
		"RAGOTA_URL":       "   ",
		"RAGOTA_TIMEOUT":   "",
		"RAGOTA_MAX_BYTES": "",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.BaseURL != DefaultBaseURL || c.Timeout != DefaultTimeout || c.MaxBytes != DefaultMaxBytes {
		t.Fatalf("blank values overrode the defaults: %+v", c)
	}
}

func TestFromEnvRejectsUnparseableNumbers(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"RAGOTA_TIMEOUT", "ninety", "RAGOTA_TIMEOUT"},
		{"RAGOTA_MAX_BYTES", "lots", "RAGOTA_MAX_BYTES"},
	} {
		_, err := FromEnv(env(map[string]string{tc.key: tc.value}))
		if err == nil {
			t.Fatalf("%s=%q was accepted", tc.key, tc.value)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error does not name the variable: %v", err)
		}
	}
}

func TestValidateRejectsWhatCannotWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no scheme", func(c *Config) { c.BaseURL = "localhost:8080" }, "scheme"},
		{"wrong scheme", func(c *Config) { c.BaseURL = "ftp://localhost:8080" }, "scheme"},
		{"no host", func(c *Config) { c.BaseURL = "http://" }, "host"},
		{"bad auth style", func(c *Config) { c.AuthStyle = "basic" }, "RAGOTA_AUTH_STYLE"},
		{"zero timeout", func(c *Config) { c.Timeout = 0 }, "RAGOTA_TIMEOUT"},
		{"negative budget", func(c *Config) { c.MaxBytes = -1 }, "RAGOTA_MAX_BYTES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := FromEnv(env(nil))
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			tc.mutate(c)
			err = c.Validate()
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not carry %q: %v", tc.want, err)
			}
		})
	}
}

func TestRedactedNeverCarriesTheKey(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"RAGOTA_MCP_KEY": "s3cret-value",
		"RAGOTA_REPOS":   "orders",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	got := c.Redacted()
	if strings.Contains(got, "s3cret") {
		t.Fatalf("the key leaked into a log line: %s", got)
	}
	// It should still say that a key is being sent, and how — a missing
	// credential and a rejected one look the same from the outside otherwise.
	if !strings.Contains(got, "auth="+AuthAPIKey) || !strings.Contains(got, "repos=orders") {
		t.Errorf("Redacted is missing something useful: %s", got)
	}

	c.APIKey = ""
	if got := c.Redacted(); !strings.Contains(got, "auth=none") {
		t.Errorf("an unauthenticated setup should say so: %s", got)
	}
}
