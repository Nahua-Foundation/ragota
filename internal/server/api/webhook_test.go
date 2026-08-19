package api_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/domain"
)

// webhookBody is a minimal GitHub push payload carrying one usable hint.
func webhookBody() []byte {
	return []byte(`{"repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"}}`)
}

func githubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// postWebhook sends body to /webhooks/git with the given headers and raw query
// string, and returns the status code.
func postWebhook(t *testing.T, baseURL string, body []byte, headers map[string]string, rawQuery string) int {
	t.Helper()
	url := baseURL + "/webhooks/git"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST webhook: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// oneRepo gives FindRepoByHints something to return so an authorized request
// reaches 202 rather than 404.
func oneRepo() *fakeService {
	return &fakeService{repos: map[string]*domain.Repo{"r1": {ID: "r1", Name: "widgets"}}}
}

func TestWebhook_Disabled_WhenSecretUnset(t *testing.T) {
	t.Setenv("RAGOTA_WEBHOOK_SECRET", "")
	srv := newTestServer(t, oneRepo(), nil)

	body := webhookBody()
	// A correct-looking signature must still be refused: the endpoint is closed,
	// not merely unauthenticated, when no secret is configured.
	got := postWebhook(t, srv.URL, body, map[string]string{
		"X-Hub-Signature-256": githubSignature("anything", body),
	}, "")
	if got != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when secret unset, got %d", got)
	}
}

func TestWebhook_GitHubHMAC(t *testing.T) {
	const secret = "s3cr3t-token"
	t.Setenv("RAGOTA_WEBHOOK_SECRET", secret)
	srv := newTestServer(t, oneRepo(), nil)
	body := webhookBody()

	t.Run("valid signature accepted", func(t *testing.T) {
		got := postWebhook(t, srv.URL, body, map[string]string{
			"X-Hub-Signature-256": githubSignature(secret, body),
		}, "")
		if got != http.StatusAccepted {
			t.Fatalf("expected 202 for valid HMAC, got %d", got)
		}
	})

	t.Run("wrong secret rejected", func(t *testing.T) {
		got := postWebhook(t, srv.URL, body, map[string]string{
			"X-Hub-Signature-256": githubSignature("wrong", body),
		}, "")
		if got != http.StatusUnauthorized {
			t.Fatalf("expected 401 for bad HMAC, got %d", got)
		}
	})

	t.Run("tampered body rejected", func(t *testing.T) {
		// Signature computed over the original body, but a different body sent.
		got := postWebhook(t, srv.URL, []byte(`{"repository":{"full_name":"evil/repo"}}`),
			map[string]string{"X-Hub-Signature-256": githubSignature(secret, body)}, "")
		if got != http.StatusUnauthorized {
			t.Fatalf("expected 401 for tampered body, got %d", got)
		}
	})
}

func TestWebhook_GitLabToken(t *testing.T) {
	const secret = "gl-secret"
	t.Setenv("RAGOTA_WEBHOOK_SECRET", secret)
	srv := newTestServer(t, oneRepo(), nil)
	body := webhookBody()

	if got := postWebhook(t, srv.URL, body, map[string]string{"X-Gitlab-Token": secret}, ""); got != http.StatusAccepted {
		t.Fatalf("expected 202 for valid GitLab token, got %d", got)
	}
	if got := postWebhook(t, srv.URL, body, map[string]string{"X-Gitlab-Token": "nope"}, ""); got != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong GitLab token, got %d", got)
	}
}

func TestWebhook_ManualToken(t *testing.T) {
	const secret = "manual-secret"
	t.Setenv("RAGOTA_WEBHOOK_SECRET", secret)
	srv := newTestServer(t, oneRepo(), nil)

	if got := postWebhook(t, srv.URL, webhookBody(), map[string]string{"X-Webhook-Token": secret}, ""); got != http.StatusAccepted {
		t.Fatalf("expected 202 for valid manual token, got %d", got)
	}
}

// TestWebhook_QueryTokenRejected pins the fix: the secret used to be accepted
// via ?token=, which request loggers record. It must no longer authenticate.
func TestWebhook_QueryTokenRejected(t *testing.T) {
	const secret = "query-secret"
	t.Setenv("RAGOTA_WEBHOOK_SECRET", secret)
	srv := newTestServer(t, oneRepo(), nil)

	got := postWebhook(t, srv.URL, webhookBody(), nil, "token="+secret)
	if got != http.StatusUnauthorized {
		t.Fatalf("expected 401 for query-string token, got %d", got)
	}
}

func TestWebhook_NoCredentials(t *testing.T) {
	t.Setenv("RAGOTA_WEBHOOK_SECRET", "some-secret")
	srv := newTestServer(t, oneRepo(), nil)

	if got := postWebhook(t, srv.URL, webhookBody(), nil, ""); got != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no auth headers, got %d", got)
	}
}
