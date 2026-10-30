// Command gateway is the reference wiring of every pattern in this repository
// into one front-door process. It is deliberately small: read it top to bottom
// to see the order things happen in, then copy the shape.
//
// Run: go run ./cmd/gateway -upstream http://127.0.0.1:9000
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/auth"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/config"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/health"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/httpserver"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/idempotency"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lb"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lifecycle"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lro"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/middleware"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/proxy"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/ratelimit"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/shedding"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/telemetry"
)

// Routes is the hot-reloadable part of the configuration.
type Routes struct {
	Upstreams []string `json:"upstreams"`
}

func main() {
	var (
		addr      = flag.String("addr", ":8080", "listen address")
		upstreams = flag.String("upstream", "http://127.0.0.1:9000", "comma-separated upstream URLs")
		jwksURL   = flag.String("jwks", "", "JWKS URL; empty disables auth (dev only)")
		issuer    = flag.String("issuer", "", "expected token issuer")
		audience  = flag.String("audience", "", "expected token audience")
		trusted   = flag.String("trusted-proxies", "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16", "CIDRs allowed to set X-Forwarded-*")
	)
	flag.Parse()

	log := telemetry.NewLogger(os.Stdout, slog.LevelInfo, "gateway", "dev", os.Getenv("ENV"))
	slog.SetDefault(log)
	ctx := context.Background()

	// --- hot configuration -------------------------------------------------
	routes, err := config.New(Routes{Upstreams: strings.Split(*upstreams, ",")}, func(r Routes) error {
		if len(r.Upstreams) == 0 {
			return apierror.New(400, apierror.CodeInvalidInput, "at least one upstream is required")
		}
		return nil
	})
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(2)
	}
	balancer := &lb.PowerOfTwoChoices{MaxFails: 1}
	for _, u := range routes.Load().Upstreams {
		pu, err := url.Parse(u)
		if err != nil {
			log.Error("bad upstream", "url", u, "err", err)
			os.Exit(2)
		}
		balancer.Add(lb.NewUpstream(u, pu, 1))
	}

	// --- proxy to services --------------------------------------------------
	upstream := proxy.New(proxy.Options{
		Balancer: balancer, Attempts: 2, MaxRetryBodyBytes: 1 << 20, FailDuration: 10 * time.Second,
		Hop: "gateway", Log: log,
	})

	// --- long-running operations -------------------------------------------
	ops := lro.NewManager(lro.NewMemoryStore())

	// --- identity -----------------------------------------------------------
	var authn middleware.Middleware = func(h http.Handler) http.Handler { return h }
	if *jwksURL != "" {
		ks := auth.NewKeySet(*jwksURL)
		if err := ks.Refresh(ctx); err != nil {
			log.Warn("jwks not reachable at startup; will retry lazily", "err", err)
		}
		authn = auth.Middleware(&auth.Verifier{Issuer: *issuer, Audiences: []string{*audience}, Keys: ks}, "gateway")
	}

	// --- limits -------------------------------------------------------------
	perUser := ratelimit.NewKeyed(50, 100, 100_000) // 50 rps sustained, burst 100, 100k tracked keys
	userKey := func(r *http.Request) string {
		if isProbe(r) {
			return "" // load-balancer probes never consume a client's budget
		}
		if p := auth.PrincipalFrom(r.Context()); p != nil {
			return p.Subject
		}
		return middleware.ClientIPFrom(r.Context())
	}
	inflight := shedding.NewInFlight(400, 100)
	inflight.LongRunning = func(r *http.Request) bool { return r.Header.Get("Accept") == "text/event-stream" }

	var ready lifecycle.Readiness
	var cidrs []netip.Prefix
	for _, c := range strings.Split(*trusted, ",") {
		if p, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil {
			cidrs = append(cidrs, p)
		}
	}

	// --- routes -------------------------------------------------------------
	mux := http.NewServeMux()
	mux.Handle("GET /livez", health.Handler("livez", log, health.Ping))
	mux.Handle("GET /readyz", health.Handler("readyz", log, health.Ping, health.ReadinessCheck(&ready)))
	mux.Handle("GET /v1/operations/{id}", lro.GetHandler(ops.Store))
	mux.Handle("POST /v1/operations/{id}/cancel", lro.CancelHandler(ops))
	mux.Handle("/", upstream) // everything else goes to services

	// --- the chain: outermost first -----------------------------------------
	handler := middleware.Chain(mux,
		middleware.Recover(log),
		middleware.RequestID("X-Request-Id"),
		middleware.RealIP(cidrs, "X-Azure-ClientIP"),
		telemetry.Propagate(false), // public endpoint: start our own trace, link the caller's
		telemetry.AccessLog(log, telemetry.AccessLogOptions{Skip: isProbe}),
		telemetry.RequestMetrics(telemetry.NoopRecorder{}),
		shedding.ShutdownRetryAfter(func() bool { return !ready.Ready() }, 2, isProbe),
		middleware.MaxBytes(8<<20),
		middleware.Deadline("X-Request-Deadline", 30*time.Second, 5*time.Minute),
		authn,
		inflight.Middleware,
		ratelimit.Middleware(perUser, userKey, "per-user", true),
		idempotency.Middleware(idempotency.Options{Store: idempotency.NewMemoryStore(), Scope: userKey}),
	)

	// --- run ----------------------------------------------------------------
	cfg := httpserver.Defaults()
	cfg.Addr = *addr
	srv := httpserver.New(ctx, cfg, handler, log)

	var g lifecycle.Group
	g.Add(lifecycle.SignalActor(ctx))
	g.Add(lifecycle.ContextActor(ctx, func(ctx context.Context) error {
		return httpserver.Run(ctx, srv, cfg, &ready, log)
	}))
	g.Add(lifecycle.ContextActor(ctx, func(ctx context.Context) error {
		ops.SweepLoop(ctx, time.Minute)
		return nil
	}))
	if err := g.Run(); err != nil && !lifecycle.IsSignal(err) {
		log.Error("gateway stopped with error", "err", err)
		os.Exit(1)
	}
	ops.Wait()
	log.Info("gateway stopped")
}

func isProbe(r *http.Request) bool {
	return r.URL.Path == "/livez" || r.URL.Path == "/readyz"
}
