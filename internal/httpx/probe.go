package httpx

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// DialTarget opens a TCP connection to a "host:port" address or to the host of
// a URL. It proves the endpoint is routable and listening without sending a
// request, so probing costs nothing on the far side.
func DialTarget(target string, timeout time.Duration) error {
	addr, err := HostPort(target)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// HostPort turns a URL or a bare host:port into a dialable address.
func HostPort(target string) (string, error) {
	if !strings.Contains(target, "://") {
		if _, _, err := net.SplitHostPort(target); err != nil {
			return "", fmt.Errorf("not a host:port address: %q", target)
		}
		return target, nil
	}

	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", target, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("url %q has no host", target)
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	switch u.Scheme {
	case "https":
		return net.JoinHostPort(u.Hostname(), "443"), nil
	case "http":
		return net.JoinHostPort(u.Hostname(), "80"), nil
	default:
		return "", fmt.Errorf("url %q has no port and an unknown scheme %q", target, u.Scheme)
	}
}
