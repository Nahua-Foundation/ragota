// Package httpx is a minimal JSON-over-HTTP client with retries,
// shared by qdrant storage and embedder providers.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error is returned for non-2xx responses. Body is truncated to
// Client.ErrorBodyLimit bytes (512 by default).
type Error struct {
	Status int
	Body   string
	// Header is the response header. It is kept because the interesting part
	// of a 429 or a 503 is not in the body: Retry-After is what says how long
	// to wait, and a caller that only sees the status has to guess.
	Header http.Header
}

func (e *Error) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, e.Body)
}

// RetryAfter returns the delay the server asked for, and whether it asked at
// all. Both forms RFC 9110 allows are accepted: a number of seconds and an
// HTTP-date. A date in the past yields 0, true — the server named a moment
// that has already passed, which is a request to retry now, not to wait.
func (e *Error) RetryAfter() (time.Duration, bool) {
	raw := strings.TrimSpace(e.Header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(raw); err == nil {
		if d := time.Until(at); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// Client posts and gets JSON against a base URL.
// Zero value is not usable; set BaseURL. Other fields have defaults.
type Client struct {
	BaseURL string
	Header  http.Header  // extra headers, e.g. Authorization
	HTTP    *http.Client // default: 60s timeout
	// Retries is the number of extra attempts on conn errors, 429 and 5xx.
	// 0 selects the default of 2; a negative value disables retries, which is
	// the only way to say "send this exactly once" — a caller whose request is
	// not safe to repeat needs to be able to say it.
	Retries int
	Backoff time.Duration // initial backoff, doubled per attempt; default 500ms
	// ErrorBodyLimit caps how much of a non-2xx body is read into Error.Body;
	// 0 selects the default of 512. Raise it when the body is a structured
	// error the caller parses rather than a string it logs — a truncated one
	// does not decode.
	ErrorBodyLimit int64
}

const (
	defaultRetries        = 2
	defaultBackoff        = 500 * time.Millisecond
	defaultErrorBodyLimit = 512
)

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// PostJSON marshals in (if non-nil), POSTs it and decodes into out (if non-nil).
func (c *Client) PostJSON(ctx context.Context, path string, in, out any) error {
	return c.Do(ctx, http.MethodPost, path, in, out)
}

// PutJSON marshals in (if non-nil), PUTs it and decodes into out (if non-nil).
func (c *Client) PutJSON(ctx context.Context, path string, in, out any) error {
	return c.Do(ctx, http.MethodPut, path, in, out)
}

func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
	}

	retries := c.Retries
	switch {
	case retries == 0:
		retries = defaultRetries
	case retries < 0:
		retries = 0
	}
	backoff := c.Backoff
	if backoff == 0 {
		backoff = defaultBackoff
	}

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			wait := backoff
			// A server that says how long to wait knows better than a blind
			// doubling: retrying a rate limiter before its window refills
			// spends an attempt on a certain 429. Our own backoff still grows
			// underneath, so a server that stops answering is not hammered at
			// whatever interval it last named.
			var httpErr *Error
			if errors.As(lastErr, &httpErr) {
				if d, ok := httpErr.RetryAfter(); ok {
					wait = d
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
				backoff *= 2
			}
		}
		var retryable bool
		retryable, lastErr = c.once(ctx, method, path, body, out)
		if lastErr == nil || !retryable {
			return lastErr
		}
	}
	return lastErr
}

// once performs a single attempt; the bool reports whether a retry may help.
func (c *Client) once(ctx context.Context, method, path string, body []byte, out any) (bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	// Paths always start with "/", so a base URL configured with a trailing
	// slash would otherwise produce a double slash.
	url := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return false, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return true, err // connection errors are retryable
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		limit := c.ErrorBodyLimit
		if limit <= 0 {
			limit = defaultErrorBodyLimit
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return retryable, &Error{Status: resp.StatusCode, Body: string(b), Header: resp.Header}
	}
	if out != nil {
		// A *[]byte asks for the body itself, for the endpoints that do not
		// answer JSON (a rendered diagram, the spec document).
		if raw, ok := out.(*[]byte); ok {
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				return false, fmt.Errorf("read response: %w", err)
			}
			*raw = b
			return false, nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return false, fmt.Errorf("decode response: %w", err)
		}
	}
	return false, nil
}
