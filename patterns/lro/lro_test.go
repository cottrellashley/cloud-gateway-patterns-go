package lro

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func waitStatus(t *testing.T, s Store, id string, want Status) *Operation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		op, err := s.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if op.Status == want {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("operation %s never reached %s", id, want)
	return nil
}

func TestLifecycleSuccessAndIdempotency(t *testing.T) {
	m := NewManager(NewMemoryStore())
	ctx := context.Background()
	op, replayed, err := m.Start(ctx, "query", "key-1", func(ctx context.Context, p Progress) (json.RawMessage, error) {
		p(50, "half")
		return json.RawMessage(`{"rows":3}`), nil
	})
	if err != nil || replayed || op.Status != Accepted {
		t.Fatalf("start: %v %v %+v", err, replayed, op)
	}
	done := waitStatus(t, m.Store, op.ID, Succeeded)
	if string(done.Result) != `{"rows":3}` || done.PercentComplete != 100 || done.EndedAt == nil {
		t.Fatalf("%+v", done)
	}
	again, replayed, _ := m.Start(ctx, "query", "key-1", func(context.Context, Progress) (json.RawMessage, error) { t.Fatal("must not run"); return nil, nil })
	if !replayed || again.ID != op.ID {
		t.Fatalf("idempotency: %v %s", replayed, again.ID)
	}
}

func TestCancelIsCooperative(t *testing.T) {
	m := NewManager(NewMemoryStore())
	started := make(chan struct{})
	op, _, _ := m.Start(context.Background(), "job", "", func(ctx context.Context, p Progress) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	<-started
	waitStatus(t, m.Store, op.ID, Running)
	cur, err := m.Cancel(context.Background(), op.ID)
	if err != nil || cur.Status.Terminal() {
		t.Fatalf("cancel should be pending: %v %+v", err, cur)
	}
	final := waitStatus(t, m.Store, op.ID, Canceled)
	if final.Error == nil || final.Error.Code != "Canceled" {
		t.Fatalf("%+v", final)
	}
}

func TestFailureAndPanicBecomeFailed(t *testing.T) {
	m := NewManager(NewMemoryStore())
	op, _, _ := m.Start(context.Background(), "job", "", func(context.Context, Progress) (json.RawMessage, error) {
		return nil, errors.New("snowflake: compilation error at line 3")
	})
	f := waitStatus(t, m.Store, op.ID, Failed)
	if f.Error == nil || f.Error.Code != "InternalError" || f.Error.Message == "snowflake: compilation error at line 3" {
		t.Fatalf("error must be an envelope with cause hidden: %+v", f.Error)
	}
	op2, _, _ := m.Start(context.Background(), "job", "", func(context.Context, Progress) (json.RawMessage, error) { panic("boom") })
	waitStatus(t, m.Store, op2.ID, Failed)
}

func TestHTTPSurface(t *testing.T) {
	m := NewManager(NewMemoryStore())
	block := make(chan struct{})
	op, _, _ := m.Start(context.Background(), "job", "", func(ctx context.Context, p Progress) (json.RawMessage, error) {
		<-block
		return json.RawMessage(`1`), nil
	})
	rec := httptest.NewRecorder()
	WriteAccepted(rec, op, "/v1/operations", false)
	if rec.Code != 202 || rec.Header().Get("Operation-Location") != "/v1/operations/"+op.ID || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("accepted: %d %v", rec.Code, rec.Header())
	}

	mux := http.NewServeMux()
	mux.Handle("GET /v1/operations/{id}", GetHandler(m.Store))
	mux.Handle("POST /v1/operations/{id}/cancel", CancelHandler(m))

	waitStatus(t, m.Store, op.ID, Running)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/operations/"+op.ID, nil))
	if rec.Code != 200 || rec.Header().Get("Retry-After") == "" || rec.Header().Get("ETag") == "" {
		t.Fatalf("get running: %d %v", rec.Code, rec.Header())
	}
	etag := rec.Header().Get("ETag")
	req := httptest.NewRequest("GET", "/v1/operations/"+op.ID, nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 304 {
		t.Fatalf("expected 304, got %d", rec.Code)
	}
	close(block)
	waitStatus(t, m.Store, op.ID, Succeeded)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req) // same If-None-Match, version changed
	if rec.Code != 200 || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("terminal get: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/operations/"+op.ID+"/cancel", nil))
	if rec.Code != 200 {
		t.Fatalf("cancel terminal should be 200, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/operations/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("missing: %d", rec.Code)
	}
}

func TestSweep(t *testing.T) {
	s := NewMemoryStore()
	m := NewManager(s)
	op, _, _ := m.Start(context.Background(), "job", "", func(context.Context, Progress) (json.RawMessage, error) { return nil, nil })
	waitStatus(t, s, op.ID, Succeeded)
	n, _ := s.Sweep(context.Background(), time.Now().Add(time.Second))
	if n != 1 {
		t.Fatalf("swept %d", n)
	}
}
