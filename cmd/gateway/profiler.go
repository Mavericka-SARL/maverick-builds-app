package main

import (
	"net/http"
	"net/http/pprof"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// startProfiler serves the Go runtime's profiles (CPU, heap, goroutines) on
// PPROF_ADDR, by default 127.0.0.1:6060: inside the gateway's own network
// namespace only, reached with `kubectl port-forward pod/<gateway> 6060`,
// never through the ingress or the public listener (which has its own
// handler, not http.DefaultServeMux). "off" turns it off.
//
// A production gateway sat at its CPU limit for over ten minutes with no
// database statement running and nothing in its log, and could not be
// looked into (2026-10-15).
func startProfiler(log zerolog.Logger) {
	addr := os.Getenv("PPROF_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6060"
	}
	if addr == "off" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Warn().Err(err).Str("addr", addr).Msg("profiler not served")
		}
	}()
}
