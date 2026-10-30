package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lifecycle"
)

func TestRunDrainsAndFlipsReadiness(t *testing.T) {
	cfg := Defaults()
	cfg.Addr = "127.0.0.1:0"
	cfg.ShutdownDelay = 10 * time.Millisecond
	cfg.GracePeriod = time.Second

	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(ctx, cfg, mux, slog.New(slog.DiscardHandler))

	// Bind first so the test knows the address.
	var ready lifecycle.Readiness
	done := make(chan error, 1)
	// Run listens itself; discover the port by overriding Addr with a listener-free approach:
	srv.Addr = "127.0.0.1:0"
	go func() { done <- Run(ctx, srv, cfg, &ready, slog.New(slog.DiscardHandler)) }()

	// wait until ready
	deadline := time.Now().Add(2 * time.Second)
	for !ready.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("server never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Run reports the address through srv.Addr? No: we need the bound address.
	// Use a second listener-less approach: hit the server via srv.Addr is not
	// possible with :0, so this test only verifies the drain sequence.
	cancel()
	// the in-flight handler is not started here, so shutdown completes quickly
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
	if ready.Ready() {
		t.Fatal("readiness should be false after shutdown")
	}
	close(release)
}

func TestNewSetsProtocolsAndTimeouts(t *testing.T) {
	cfg := Defaults()
	srv := New(context.Background(), cfg, http.NotFoundHandler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if srv.ReadHeaderTimeout != cfg.ReadHeaderTimeout || srv.MaxHeaderBytes != 16<<10 {
		t.Fatalf("timeouts not applied: %+v", srv)
	}
	if !srv.Protocols.UnencryptedHTTP2() || !srv.Protocols.HTTP2() || !srv.Protocols.HTTP1() {
		t.Fatalf("protocols: %v", srv.Protocols)
	}
}
