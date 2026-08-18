package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Nahua-Foundation/ragota/pkg/client"
)

// coreStub answers just enough for the startup check, which is all this file is
// about: everything past it is covered against a fuller stub in internal/server.
func coreStub(t *testing.T, apiVersion string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.HealthResponse{
			Status: "ok", Version: "v0.0.0-stub", APIVersion: apiVersion,
		})
	})
	mux.HandleFunc("/api/v1/repos", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Repo{{ID: "orders-aaaaaaaaaaaa", Name: "orders", Status: "idle"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestVersionFlagPrintsAndStops(t *testing.T) {
	var out bytes.Buffer
	if err := runMCP([]string{"-version"}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(out.String()) != version {
		t.Errorf("printed %q, wanted %q", out.String(), version)
	}
}

func TestCheckFlagVerifiesAndExitsWithoutServing(t *testing.T) {
	srv := coreStub(t, client.SchemaVersion)
	t.Setenv("RAGOTA_URL", srv.URL)
	t.Setenv("RAGOTA_MCP_KEY", "k")

	var out bytes.Buffer
	if err := runMCP([]string{"-check"}, &out); err != nil {
		t.Fatalf("run -check: %v", err)
	}
	// -check must not fall through into serving, which would block on stdin.
	for _, want := range []string{"ok:", "v0.0.0-stub", client.SchemaVersion, srv.URL, "10 tools"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("-check output is missing %q: %s", want, out.String())
		}
	}
}

func TestFlagsOverrideTheEnvironment(t *testing.T) {
	srv := coreStub(t, client.SchemaVersion)
	// A launch block that pointed somewhere else must lose to the flag, so that
	// an operator can test one address without editing the client config.
	t.Setenv("RAGOTA_URL", "http://127.0.0.1:1")

	var out bytes.Buffer
	if err := runMCP([]string{"-check", "-url", srv.URL}, &out); err != nil {
		t.Fatalf("run -check: %v", err)
	}
	if !strings.Contains(out.String(), srv.URL) {
		t.Errorf("the flag did not win: %s", out.String())
	}
}

func TestAnUnreachableServerFailsBeforeServing(t *testing.T) {
	t.Setenv("RAGOTA_URL", "http://127.0.0.1:1")
	t.Setenv("RAGOTA_TIMEOUT", "1s")

	var out bytes.Buffer
	err := runMCP([]string{"-check"}, &out)
	if err == nil {
		t.Fatal("run accepted an unreachable server")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") || !strings.Contains(err.Error(), "RAGOTA_URL") {
		t.Errorf("the message must name the address and the variable that sets it: %v", err)
	}
}

func TestAnIncompatibleContractFailsBeforeServing(t *testing.T) {
	srv := coreStub(t, "99.0.0")
	t.Setenv("RAGOTA_URL", srv.URL)

	var out bytes.Buffer
	err := runMCP([]string{"-check"}, &out)
	if err == nil {
		t.Fatal("run accepted a server on another major version")
	}
	// Discovering this through a decode error later would read as a bug in this
	// program rather than as a version mismatch.
	if !strings.Contains(err.Error(), "99.0.0") {
		t.Errorf("the message must name the server's version: %v", err)
	}
}

func TestABadEnvironmentIsRejectedWithoutDialling(t *testing.T) {
	t.Setenv("RAGOTA_TIMEOUT", "soon")

	var out bytes.Buffer
	err := runMCP([]string{"-check"}, &out)
	if err == nil || !strings.Contains(err.Error(), "RAGOTA_TIMEOUT") {
		t.Fatalf("wanted a message naming RAGOTA_TIMEOUT, got %v", err)
	}
}
