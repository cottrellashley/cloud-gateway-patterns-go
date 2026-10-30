package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lb"
)

func newBalancer(targets ...string) (*lb.WeightedRoundRobin, []*lb.Upstream) {
	b := lb.WeightedRoundRobin{MaxFails: 1} // passive health: one failure removes the upstream for FailDuration
	var ups []*lb.Upstream
	for _, t := range targets {
		u, _ := url.Parse(t)
		up := lb.NewUpstream(t, u, 1)
		b.Add(up)
		ups = append(ups, up)
	}
	return &b, ups
}

func TestForwardsAndSetsXForwarded(t *testing.T) {
	var gotXFF, gotHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotHost = r.Host
		w.WriteHeader(204)
	}))
	defer backend.Close()
	b, _ := newBalancer(backend.URL)
	h := New(Options{Balancer: b, Attempts: 1})
	req := httptest.NewRequest("GET", "http://gw.example/api/x", nil)
	req.RemoteAddr = "203.0.113.5:4444"
	req.Header.Set("X-Forwarded-For", "9.9.9.9") // must be dropped when untrusted
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if gotXFF != "203.0.113.5" {
		t.Fatalf("X-Forwarded-For %q", gotXFF)
	}
	if gotHost != "gw.example" {
		t.Fatalf("Host %q", gotHost)
	}
}

func TestRetriesIdempotentOnDeadUpstreamOnly(t *testing.T) {
	var hits atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		w.Write([]byte("ok:" + string(b)))
	}))
	defer live.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close() // connection refused from now on
	b, _ := newBalancer(dead.URL, live.URL)
	h := New(Options{Balancer: b, Attempts: 3, MaxRetryBodyBytes: 1024, FailDuration: time.Second, Log: slog.New(slog.DiscardHandler)})

	// PUT with small body: retried with the body replayed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PUT", "http://gw/x", strings.NewReader("payload")))
	if rec.Code != 200 || rec.Body.String() != "ok:payload" {
		t.Fatalf("PUT got %d %q", rec.Code, rec.Body.String())
	}

	// POST is not idempotent: no retry, so it may fail with 502 when it lands on dead.
	fails := 0
	for i := 0; i < 6; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "http://gw/x", strings.NewReader("p")))
		if rec.Code == 502 {
			fails++
			if rec.Header().Get("X-Error-Code") != "UpstreamError" {
				t.Fatalf("code header %q", rec.Header().Get("X-Error-Code"))
			}
		}
	}
	// After the first failure the dead upstream is out of rotation for FailDuration,
	// so at most the first POST can fail.
	if fails > 1 {
		t.Fatalf("passive health did not remove dead upstream: %d failures", fails)
	}
}

func TestNoUpstreamGives503(t *testing.T) {
	var b lb.WeightedRoundRobin
	h := New(Options{Balancer: &b})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://gw/x", nil))
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d headers %v", rec.Code, rec.Header())
	}
}
