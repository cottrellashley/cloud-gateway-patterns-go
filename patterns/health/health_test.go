package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lifecycle"
)

func TestReadyzReflectsReadinessAndWithholdsReasons(t *testing.T) {
	var ready lifecycle.Readiness
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := NamedCheck("db", func(context.Context) error { return errors.New("pg: connection refused host=db.internal") })
	h := Handler("readyz", log, Ping, ReadinessCheck(&ready), db)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "db.internal") {
		t.Fatalf("reason leaked: %s", body)
	}
	if !strings.Contains(body, "[-]ready failed") || !strings.Contains(body, "[-]db failed") {
		t.Fatalf("report: %s", body)
	}

	ready.Set(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz?exclude=db", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz?exclude=db&verbose", nil))
	if !strings.Contains(rec.Body.String(), "[+]db excluded: ok") {
		t.Fatalf("verbose: %s", rec.Body.String())
	}
}

func TestWithTimeoutDoesNotHang(t *testing.T) {
	slow := WithTimeout(NamedCheck("slow", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }), 20*time.Millisecond)
	start := time.Now()
	if err := slow.Check(context.Background()); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("check hung")
	}
}
