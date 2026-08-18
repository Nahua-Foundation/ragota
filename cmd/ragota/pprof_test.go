package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestStartPprofDisabledByDefault(t *testing.T) {
	if shutdown := startPprof(""); shutdown != nil {
		t.Fatal("startPprof(\"\") returned a server; profiling must be off unless asked for")
	}
}

func TestStartPprofServesIndex(t *testing.T) {
	// A fixed port would flake, so let the OS pick a free one and hand the
	// address to startPprof, which binds it itself.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bound := ln.Addr().String()
	_ = ln.Close()

	shutdown := startPprof(bound)
	if shutdown == nil {
		t.Fatalf("startPprof(%q) returned nil", bound)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

	url := "http://" + bound + "/debug/pprof/goroutine?debug=1"
	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("empty goroutine profile")
	}
}

func TestLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:6060", true},
		{"localhost:6060", true},
		{"[::1]:6060", true},
		{":6060", false},
		{"0.0.0.0:6060", false},
		{"10.0.0.5:6060", false},
		{"garbage", false},
	}
	for _, tt := range tests {
		if got := loopbackAddr(tt.addr); got != tt.want {
			t.Errorf("loopbackAddr(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}
