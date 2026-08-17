package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nahua-Foundation/ragota/pkg/client"
)

// The retry behaviour is about what the transport does between answers, which
// a real server cannot be made to demonstrate on demand — these few tests stub
// the answers. Everything about the contract itself is checked against the real
// router in client_test.go.

func TestRetryAfterOverridesTheBackoff(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded","code":"rate_limited","retry_after_seconds":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","version":"test","api_version":"0.2.0"}`))
	}))
	defer ts.Close()

	// A backoff far longer than the server's hint: if the client waited its own
	// backoff instead of the server's, this would take half a minute.
	c := client.New(ts.URL, client.WithRetries(1), client.WithBackoff(30*time.Second))

	start := time.Now()
	health, err := c.Health(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.APIVersion != "0.2.0" {
		t.Errorf("api_version = %q", health.APIVersion)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server saw %d calls, want 2", got)
	}
	if elapsed > 10*time.Second {
		t.Errorf("waited %s; the Retry-After of 1s was ignored", elapsed)
	}
}

func TestRepeatableCallsRetryAndMutatingOnesDoNot(t *testing.T) {
	var search, add atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			search.Add(1)
		case strings.HasSuffix(r.URL.Path, "/repos"):
			add.Add(1)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error","code":"internal_error"}`))
	}))
	defer ts.Close()

	ctx := context.Background()
	c := client.New(ts.URL, client.WithRetries(2), client.WithBackoff(time.Millisecond))

	if _, err := c.Search(ctx, &client.SearchRequest{Query: "x"}); !errors.Is(err, client.ErrInternal) {
		t.Fatalf("Search gave %v, want ErrInternal", err)
	}
	if got := search.Load(); got != 3 {
		t.Errorf("a repeatable call was attempted %d times, want 3", got)
	}

	if _, err := c.AddRepo(ctx, &client.AddRepoRequest{Name: "x", Source: "local", Path: "/tmp"}); err == nil {
		t.Fatal("AddRepo against a failing server returned no error")
	}
	if got := add.Load(); got != 1 {
		t.Errorf("a state-changing call was attempted %d times, want 1", got)
	}
}

func TestAnswerThatIsNotTheErrorShapeStillCarriesTheStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// What a reverse proxy in front of the server answers with.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body><h1>502 Bad Gateway</h1></body></html>"))
	}))
	defer ts.Close()

	c := client.New(ts.URL, client.WithRetries(-1))
	_, err := c.Stats(context.Background())

	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not a *client.Error: %T (%v)", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", apiErr.StatusCode)
	}
	if !errors.Is(err, client.ErrInternal) {
		t.Errorf("a 502 with no code did not fall back to internal_error: %v", err)
	}
	if !strings.Contains(apiErr.Body, "Bad Gateway") {
		t.Errorf("the body a proxy answered with was dropped: %q", apiErr.Body)
	}
	if !strings.Contains(err.Error(), "Bad Gateway") {
		t.Errorf("error message hides what came back: %q", err.Error())
	}
}

func TestCompatibleWith(t *testing.T) {
	tests := []struct {
		apiVersion string
		ok         bool
		why        string
	}{
		{client.SchemaVersion, true, "the version this package was written against"},
		{"0.2.7", true, "a patch release serves the same fields"},
		{"0.3.0", true, "a newer minor adds fields this client ignores"},
		{"0.1.9", false, "an older minor may not serve fields this client reads"},
		{"1.0.0", false, "a different major is a different contract"},
		{"", false, "no version at all"},
		{"garbage", false, "not this contract's numbering"},
	}
	for _, tt := range tests {
		err := client.CompatibleWith(tt.apiVersion)
		if tt.ok && err != nil {
			t.Errorf("CompatibleWith(%q) = %v, want nil (%s)", tt.apiVersion, err, tt.why)
		}
		if !tt.ok {
			if err == nil {
				t.Errorf("CompatibleWith(%q) = nil, want an error (%s)", tt.apiVersion, tt.why)
			} else if !errors.Is(err, client.ErrIncompatibleAPI) {
				t.Errorf("CompatibleWith(%q) = %v, which does not match ErrIncompatibleAPI", tt.apiVersion, err)
			}
		}
	}
}

func TestSentinelsCarryNoOccurrenceDetail(t *testing.T) {
	// A sentinel names a condition. Handing one back as though it were an
	// answer — with a status, a message, a backoff — is the mistake this
	// guards: callers compare against it, they do not read it.
	if client.ErrRepoBusy.StatusCode != 0 || client.ErrRepoBusy.Message != "" || client.ErrRepoBusy.RetryAfter != 0 {
		t.Errorf("ErrRepoBusy carries occurrence detail: %+v", client.ErrRepoBusy)
	}
	if client.ErrRepoBusy.Error() != client.CodeRepoBusy {
		t.Errorf("ErrRepoBusy.Error() = %q, want %q", client.ErrRepoBusy.Error(), client.CodeRepoBusy)
	}
}
