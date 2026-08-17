package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Nahua-Foundation/ragota/internal/httpx"
)

// apiPrefix is where the versioned routes live. /health, /ready and /metrics
// sit outside it, and outside authentication with them.
const apiPrefix = "/api/v1"

// defaultTimeout matches the server's default write timeout: /context does
// retrieval, graph expansion and possibly reranking inside one request, and a
// client that gives up first turns a slow answer into no answer.
const defaultTimeout = 120 * time.Second

// errorBodyLimit is how much of an error body the client reads. The JSON error
// shape is small, but it is parsed rather than logged, and a body truncated
// mid-object decodes into nothing at all.
const errorBodyLimit = 8 << 10

// Client talks to a ragota server.
//
// It is safe for concurrent use. Build one per server and share it: it holds
// the connection pool.
type Client struct {
	baseURL string
	header  http.Header
	http    *http.Client
	retries int
	backoff time.Duration

	// Two transports rather than one, because "retry" is not a property of the
	// client but of the call. Everything retrieval asks for is safe to repeat;
	// AddRepo and Index are not repeated behind the caller's back (see once).
	retrying *httpx.Client
	once     *httpx.Client
}

// Option configures a Client at construction.
type Option func(*Client)

// New returns a Client for the server at baseURL, which must carry a scheme
// and host ("http://localhost:8080"). A trailing slash is ignored.
//
// With no options the client sends no credential, which is what a server
// configured with auth.type "none" expects. Add WithAPIKey otherwise.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		header:  http.Header{},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultTimeout}
	}
	base := httpx.Client{
		BaseURL:        c.baseURL,
		Header:         c.header,
		HTTP:           c.http,
		Retries:        c.retries,
		Backoff:        c.backoff,
		ErrorBodyLimit: errorBodyLimit,
	}
	retrying, once := base, base
	// Negative disables retries entirely; 0 would select httpx's default.
	once.Retries = -1
	c.retrying, c.once = &retrying, &once
	return c
}

// WithAPIKey sends key as the X-API-Key header.
//
// Send the key itself. The "read:" / "admin:" prefix that appears in the
// server's api_keys configuration is how the operator records what a key may
// do; it is not part of the credential, and a client that sends it will not
// authenticate.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.header.Set("X-API-Key", key) }
}

// WithBearerToken sends key as "Authorization: Bearer <key>" instead of
// X-API-Key. The server accepts either; this exists for the gateways that
// forward Authorization and drop headers they do not know.
func WithBearerToken(key string) Option {
	return func(c *Client) { c.header.Set("Authorization", "Bearer "+key) }
}

// WithHTTPClient supplies the *http.Client to send with — for a custom
// transport, a proxy, or a timeout other than the 120s default.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithRetries sets how many extra attempts a repeatable call gets after a
// connection error, a 429 or a 5xx. 0 selects the default of 2; a negative
// value disables retries.
//
// It never applies to AddRepo or Index: those change server state and are sent
// exactly once whatever this says.
func WithRetries(n int) Option {
	return func(c *Client) { c.retries = n }
}

// WithBackoff sets the first delay between attempts, doubled on each further
// one. 0 selects the default of 500ms. A 429 that names a Retry-After
// overrides it for that wait.
func WithBackoff(d time.Duration) Option {
	return func(c *Client) { c.backoff = d }
}

// WithUserAgent sets the User-Agent header, so that a server operator reading
// access logs can tell which client is asking.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.header.Set("User-Agent", ua) }
}

// BaseURL returns the server address this client was built for.
func (c *Client) BaseURL() string { return c.baseURL }

// --- transport ---

// get performs a repeatable GET.
func (c *Client) get(ctx context.Context, path string, out any) error {
	return toAPIError(c.retrying.GetJSON(ctx, path, out))
}

// post performs a POST that is safe to repeat. Most of this API's POSTs are:
// search, navigation and graph queries are reads that carry a body too
// structured for a query string.
func (c *Client) post(ctx context.Context, path string, in, out any) error {
	return toAPIError(c.retrying.PostJSON(ctx, path, in, out))
}

// postOnce performs a POST that changes server state, without retrying it.
//
// Both such calls are documented idempotent on the server — AddRepo re-registers
// and Index folds into the job already queued — but a silent retry still lies to
// the caller: the second attempt's ack describes the state the first attempt
// created, so an Index that started a pass comes back saying "queued". A caller
// that saw a transport error and wants to try again can say so itself.
func (c *Client) postOnce(ctx context.Context, path string, in, out any) error {
	return toAPIError(c.once.PostJSON(ctx, path, in, out))
}

// getText performs a repeatable GET against an endpoint that does not answer
// JSON.
func (c *Client) getText(ctx context.Context, path string) (string, error) {
	var raw []byte
	if err := toAPIError(c.retrying.GetJSON(ctx, path, &raw)); err != nil {
		return "", err
	}
	return string(raw), nil
}

// toAPIError turns a transport failure into *Error when the server answered at
// all. A connection error, a cancelled context or a body that is not JSON when
// it should be are passed through untouched: they are not something the API
// said, and dressing them up as an API error code would invent one.
func toAPIError(err error) error {
	var httpErr *httpx.Error
	if !errors.As(err, &httpErr) {
		return err
	}
	out := &Error{StatusCode: httpErr.Status, Body: httpErr.Body}

	var body ErrorResponse
	if json.Unmarshal([]byte(httpErr.Body), &body) == nil && body.Code != "" {
		out.Code = body.Code
		out.Message = body.Error
		out.LastCommit = body.LastCommit
		out.LimitBytes = body.LimitBytes
		out.RetryAfter = time.Duration(body.RetryAfterSeconds) * time.Second
	} else {
		// No readable JSON: a proxy answered, or the body was cut off. The
		// status is then the only thing that survived, and it is still enough
		// to tell "not found" from "rate limited".
		out.Code = codeForStatus(httpErr.Status)
	}
	if out.RetryAfter == 0 {
		if d, ok := httpErr.RetryAfter(); ok {
			out.RetryAfter = d
		}
	}
	return out
}

// --- path building ---

// apiPath joins path segments under /api/v1, escaping each one. Repository and
// job ids come from the server, but they reach this package through the
// caller, and an id with a slash in it must not be able to reach another route.
func apiPath(segments ...string) string {
	var b strings.Builder
	b.WriteString(apiPrefix)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// withQuery appends q to path, and nothing at all when q is empty — a bare "?"
// is a different request path to some proxies.
func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// positiveInt adds n to q under name when it is positive. Zero means "the
// server's default", and sending an explicit 0 would ask for something else.
func positiveInt(q url.Values, name string, n int) {
	if n > 0 {
		q.Set(name, strconv.Itoa(n))
	}
}
