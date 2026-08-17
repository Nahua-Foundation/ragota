package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// startPprof starts the runtime profiling listener on addr, or does nothing
// when addr is empty (the default).
//
// The profiler runs on its own listener rather than on the API router: the
// endpoints dump heap contents, command lines and goroutine stacks, so they
// must not inherit the API's CORS or auth configuration, and must be reachable
// only by whoever can reach the address the operator picked. A non-loopback
// bind is legal (a container publishes it deliberately) but logged as a
// warning, because it is almost never what a developer means.
//
// It returns a shutdown function; nil when profiling is disabled.
func startPprof(addr string) func(context.Context) error {
	if addr == "" {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// No WriteTimeout: /debug/pprof/profile and /trace hold the response
		// open for the whole sampling window, which a timeout would truncate.
		ReadHeaderTimeout: 10 * time.Second,
	}

	if !loopbackAddr(addr) {
		slog.Warn("pprof listening on a non-loopback address; profiles expose heap contents and stacks", "addr", addr)
	}

	go func() {
		slog.Info("pprof listening", "addr", addr, "index", "http://"+addr+"/debug/pprof/")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("pprof server error", "addr", addr, "err", err)
		}
	}()

	return srv.Shutdown
}

// loopbackAddr reports whether a host:port binds to the loopback interface
// only. An unparseable or unresolved host counts as non-loopback: the warning
// is the safe answer when the bind target is unclear.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	switch host {
	case "localhost":
		return true
	case "":
		// ":6060" binds every interface.
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
