package client

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrIncompatibleAPI reports that a server speaks a wire contract this package
// was not written against. Match it with errors.Is; the wrapped message names
// both versions.
var ErrIncompatibleAPI = errors.New("ragota: incompatible API version")

// Health reports whether the process is up, and what it is.
//
// It is a liveness probe: it touches no dependency, so it answers while the
// database is down. A call that must not be routed to a half-ready instance
// wants /ready, which this package does not wrap — an orchestrator, not a
// client, is what acts on it.
//
// Health needs no credential, which makes it the call to check connectivity
// with before deciding whether a failure is the network or the key.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var out HealthResponse
	if err := c.get(ctx, "/health", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckCompatibility asks the server which contract it speaks and reports
// whether this package can speak it.
//
// Call it once at startup rather than deriving the rule again at each call
// site. The health response is returned either way, so a caller that decides
// to carry on anyway still has the versions to log.
func (c *Client) CheckCompatibility(ctx context.Context) (*HealthResponse, error) {
	health, err := c.Health(ctx)
	if err != nil {
		return nil, err
	}
	return health, CompatibleWith(health.APIVersion)
}

// CompatibleWith reports whether this package can talk to a server serving
// apiVersion, as reported by HealthResponse.APIVersion.
//
// The rule is one-directional on purpose. A newer server is fine: this package
// reads the fields it knows and ignores the rest. An older one is not, because
// the fields this package documents may simply not be served — and the caller
// would read them as absent (no results, no truncation, no service) rather
// than as unsupported, which is the failure that does not look like one. A
// different major version is refused in both directions.
//
// An unparseable version is refused rather than assumed compatible: it is not
// this contract's numbering, so nothing is known about what it serves.
func CompatibleWith(apiVersion string) error {
	serverMajor, serverMinor, err := parseVersion(apiVersion)
	if err != nil {
		return fmt.Errorf("%w: server reports %q, this client speaks %s: %w",
			ErrIncompatibleAPI, apiVersion, SchemaVersion, err)
	}
	ourMajor, ourMinor, err := parseVersion(SchemaVersion)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIncompatibleAPI, err)
	}
	switch {
	case serverMajor != ourMajor:
		return fmt.Errorf("%w: server speaks %s, this client speaks %s (different major version)",
			ErrIncompatibleAPI, apiVersion, SchemaVersion)
	case serverMinor < ourMinor:
		return fmt.Errorf("%w: server speaks %s, this client speaks %s (server predates fields this client reads)",
			ErrIncompatibleAPI, apiVersion, SchemaVersion)
	default:
		return nil
	}
}

// parseVersion reads the major and minor of a "major.minor[.patch]" version.
func parseVersion(v string) (major, minor int, err error) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("malformed version %q", v)
	}
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, fmt.Errorf("malformed version %q", v)
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, fmt.Errorf("malformed version %q", v)
	}
	return major, minor, nil
}
