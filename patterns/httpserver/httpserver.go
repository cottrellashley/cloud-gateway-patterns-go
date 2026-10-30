// Package httpserver builds and runs an http.Server the way Caddy, Traefik and
// the Kubernetes API server do: explicit timeouts, HTTP/2 on, a readiness flip
// before drain, a shutdown delay so load balancers notice, a bounded graceful
// period, then a hard close.
//
// Evidence (see docs/02-server/http-server-lifecycle.md):
//   - net/http Server.Shutdown closes listeners, then polls idle connections
//     with exponential backoff capped at 500ms until ctx expires.
//   - Caddy app.go: MaxHeaderBytes defaults to 16 KiB, IdleTimeout to 5
//     minutes, ShutdownDelay then GracePeriod.
//   - Traefik server_entrypoint_tcp.go Shutdown: RequestAcceptGraceTimeout
//     sleep, Shutdown with GraceTimeOut, and Close() when the deadline hits.
//   - Kubernetes genericapiserver RunWithContext: /readyz goes red first,
//     sleeps ShutdownDelayDuration, then stops accepting and drains.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lifecycle"
)

// Config is everything the server needs that is not the handler.
type Config struct {
	Addr string

	// ReadHeaderTimeout bounds slow-loris clients. Always set it.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds the whole request read. Keep it 0 for a gateway that
	// accepts uploads and set per-route body limits and deadlines instead.
	ReadTimeout time.Duration
	// WriteTimeout bounds the whole response write. Keep it 0 when the server
	// serves SSE, WebSockets or large downloads; use per-request deadlines.
	WriteTimeout time.Duration
	// IdleTimeout closes keep-alive connections that sit idle.
	IdleTimeout time.Duration
	// MaxHeaderBytes caps request headers. Caddy uses 16 KiB; stdlib 1 MiB.
	MaxHeaderBytes int

	// EnableH2C serves unencrypted HTTP/2 (behind a TLS-terminating front
	// door such as Azure Front Door or App Gateway). Go 1.24+ stdlib.
	EnableH2C bool

	// ShutdownDelay is how long to keep serving after readiness goes false so
	// that load balancers stop routing to this instance before it drains.
	ShutdownDelay time.Duration
	// GracePeriod bounds Shutdown; after it the server is closed hard.
	GracePeriod time.Duration
}

// Defaults are production values for a gateway behind a cloud load balancer.
func Defaults() Config {
	return Config{
		Addr:              ":8080",
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       5 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		EnableH2C:         true,
		ShutdownDelay:     5 * time.Second,
		GracePeriod:       30 * time.Second,
	}
}

// New builds the http.Server. BaseContext carries the process context so that
// handlers observe process shutdown through r.Context() if they want to.
func New(ctx context.Context, cfg Config, h http.Handler, log *slog.Logger) *http.Server {
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		Protocols:         new(http.Protocols),
	}
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetHTTP2(true)
	if cfg.EnableH2C {
		srv.Protocols.SetUnencryptedHTTP2(true)
	}
	if log != nil {
		srv.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	}
	return srv
}

// Run serves until ctx is done, then performs the drain sequence:
//
//  1. readiness.Set(false)            // /readyz turns red
//  2. sleep cfg.ShutdownDelay         // LB health checks observe it
//  3. srv.Shutdown(grace)             // stop listening, wait for in-flight
//  4. srv.Close() if grace expired    // drop stragglers
//
// It returns nil on a clean stop and the serve error otherwise. Use it as a
// lifecycle.Group actor: execute = Run, interrupt = cancel ctx.
func Run(ctx context.Context, srv *http.Server, cfg Config, ready *lifecycle.Readiness, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	if ready != nil {
		ready.Set(true)
	}
	log.Info("http server listening", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	if ready != nil {
		ready.Set(false)
	}
	if cfg.ShutdownDelay > 0 {
		log.Info("shutdown: readiness off, waiting for load balancer", "delay", cfg.ShutdownDelay)
		time.Sleep(cfg.ShutdownDelay)
	}
	// Shutdown must not inherit the already-cancelled ctx.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.GracePeriod)
	defer cancel()
	log.Info("shutdown: draining", "grace", cfg.GracePeriod)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown: grace period exceeded, closing connections", "err", err)
		_ = srv.Close()
	}
	<-serveErr
	log.Info("shutdown: complete")
	return nil
}
