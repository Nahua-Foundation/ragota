package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostJSON_success(t *testing.T) {
	var reqCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		assert.Equal(t, "hello", payload["message"])

		resp := map[string]string{"result": "ok"}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	var out map[string]string
	c := &Client{BaseURL: server.URL}
	err := c.PostJSON(context.Background(), "/api/test", map[string]string{"message": "hello"}, &out)
	require.NoError(t, err)
	assert.Equal(t, 1, reqCount)
	assert.Equal(t, "ok", out["result"])
}

func TestDo_retry_500_then_success(t *testing.T) {
	var reqCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		if reqCount <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "done"})
	}))
	defer server.Close()

	var out map[string]string
	c := &Client{BaseURL: server.URL, Retries: 2}
	err := c.Do(context.Background(), http.MethodPost, "/api/retry", map[string]string{"x": "1"}, &out)
	require.NoError(t, err)
	assert.Equal(t, 3, reqCount)
	assert.Equal(t, "done", out["status"])
}

func TestDo_no_retry_on_400(t *testing.T) {
	var reqCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request body"))
	}))
	defer server.Close()

	c := &Client{BaseURL: server.URL, Retries: 2}
	err := c.Do(context.Background(), http.MethodPost, "/api/bad", map[string]string{}, nil)
	require.Error(t, err)

	var httpErr *Error
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, 400, httpErr.Status)
	assert.Contains(t, httpErr.Body, "bad request body")
	assert.Equal(t, 1, reqCount)
}

func TestDo_body_truncation_500(t *testing.T) {
	longBody := strings.Repeat("x", 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(longBody))
	}))
	defer server.Close()

	c := &Client{BaseURL: server.URL}
	err := c.Do(context.Background(), http.MethodPost, "/api/big", nil, nil)
	require.Error(t, err)

	var httpErr *Error
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, 500, httpErr.Status)
	assert.Len(t, httpErr.Body, 512)
}

func TestDo_context_cancellation_during_backoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var reqCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := &Client{
		BaseURL: server.URL,
		Retries: 5,
		Backoff: 2 * time.Second, // long backoff so cancellation triggers before it completes
	}

	// Cancel after the first request, ensuring the backoff in attempt 1 sees cancellation.
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	err := c.Do(ctx, http.MethodPost, "/api/cancel", nil, nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, elapsed < 1*time.Second, "expected fast failure but took %v", elapsed)
	assert.Equal(t, context.Canceled, err)

	// The first attempt returns 500 (retryable), then cancellation fires during backoff.
	assert.Equal(t, 1, reqCount)
}

func TestDo_negative_retries_sends_once(t *testing.T) {
	var reqCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	// 0 would select the default of 2; a caller whose request must not be
	// repeated has to be able to say so.
	c := &Client{BaseURL: server.URL, Retries: -1}
	require.Error(t, c.Do(context.Background(), http.MethodPost, "/api/once", nil, nil))
	assert.Equal(t, 1, reqCount)
}

func TestDo_honours_retry_after(t *testing.T) {
	var reqCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		if reqCount == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	// A backoff far longer than the hint: honouring the hint is the only way
	// this finishes quickly.
	c := &Client{BaseURL: server.URL, Retries: 1, Backoff: 30 * time.Second}
	start := time.Now()
	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/api/limited", nil, nil))
	assert.Equal(t, 2, reqCount)
	assert.Less(t, time.Since(start), 10*time.Second)
}

func TestDo_raw_bytes_out(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("flowchart LR\n"))
	}))
	defer server.Close()

	var raw []byte
	c := &Client{BaseURL: server.URL}
	require.NoError(t, c.GetJSON(context.Background(), "/diagram", &raw))
	assert.Equal(t, "flowchart LR\n", string(raw))
}

func TestError_RetryAfter(t *testing.T) {
	none := &Error{Status: 429}
	_, ok := none.RetryAfter()
	assert.False(t, ok, "no header means the server asked for nothing")

	secs := &Error{Status: 429, Header: http.Header{"Retry-After": []string{"7"}}}
	d, ok := secs.RetryAfter()
	require.True(t, ok)
	assert.Equal(t, 7*time.Second, d)

	// A date already past is "retry now", not "wait a negative time".
	past := &Error{Status: 503, Header: http.Header{
		"Retry-After": []string{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)},
	}}
	d, ok = past.RetryAfter()
	require.True(t, ok)
	assert.Equal(t, time.Duration(0), d)
}
