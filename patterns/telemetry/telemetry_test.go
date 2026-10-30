package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/auth"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/middleware"
)

func TestTraceParentRoundTrip(t *testing.T) {
	tp, err := ParseTraceParent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if err != nil || !tp.Sampled || tp.TraceIDString() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("%v %+v", err, tp)
	}
	if tp.String() != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatal(tp.String())
	}
	for _, bad := range []string{"", "00-abc-def-01", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"} {
		if _, err := ParseTraceParent(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	c := tp.Child()
	if c.TraceID != tp.TraceID || c.SpanID == tp.SpanID {
		t.Fatal("child must keep trace id and change span id")
	}
}

func TestPropagateAndAccessLogFields(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo, "gw", "1.0", "test")
	h := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			FromContext(r.Context()).Info("inside handler")
			w.WriteHeader(201)
			w.Write([]byte("hello"))
		}),
		middleware.RequestID(""),
		func(next http.Handler) http.Handler { // inject logger + principal
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := WithLogger(r.Context(), log)
				ctx = auth.WithPrincipal(ctx, &auth.Principal{Subject: "ashley", TenantID: "t1"})
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		},
		Propagate(true),
		AccessLog(log, AccessLogOptions{}),
	)
	req := httptest.NewRequest("GET", "/orders/42?secret=1", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Trace-Id") != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace header %q", rec.Header().Get("X-Trace-Id"))
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %d: %s", len(lines), buf.String())
	}
	var inside, access map[string]any
	json.Unmarshal([]byte(lines[0]), &inside)
	json.Unmarshal([]byte(lines[1]), &access)
	if inside["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || inside["request_id"] == nil {
		t.Fatalf("handler log not correlated: %v", inside)
	}
	if access["http.status"].(float64) != 201 || access["http.bytes"].(float64) != 5 || access["principal"] != "ashley" || access["url.path"] != "/orders/42" {
		t.Fatalf("access log: %v", access)
	}
	if _, ok := access["url.query"]; ok {
		t.Fatal("query must not be logged by default")
	}
	if _, ok := access["ts"]; !ok {
		t.Fatal("ts field missing")
	}
}

type recSpy struct {
	route  string
	status int
	n      int
}

func (s *recSpy) ObserveRequest(_, route string, status int, _ time.Duration, _, _ int64) {
	s.route, s.status, s.n = route, status, s.n+1
}
func (s *recSpy) InFlight(int) {}

func TestRequestMetricsUsesRoutePattern(t *testing.T) {
	spy := &recSpy{}
	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", RequestMetrics(spy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })))
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/users/42", nil))
	if spy.route != "GET /users/{id}" || spy.status != 404 || spy.n != 1 {
		t.Fatalf("%+v", spy)
	}
	_ = context.Background
}
