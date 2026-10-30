package idempotency

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReplayReuseAndInProgress(t *testing.T) {
	var executions atomic.Int32
	block := make(chan struct{})
	h := Middleware(Options{Store: NewMemoryStore(), Scope: func(r *http.Request) string { return r.Header.Get("X-User") }})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			executions.Add(1)
			if r.URL.Path == "/slow" {
				<-block
			}
			w.Header().Set("Location", "/orders/1")
			w.WriteHeader(201)
			w.Write([]byte(`{"id":1}`))
		}))
	mk := func(path, body, key, user string) *http.Request {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("X-User", user)
		return req
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, mk("/orders", `{"sku":"a"}`, "k1", "u1"))
	if rec.Code != 201 || rec.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("first: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, mk("/orders", `{"sku":"a"}`, "k1", "u1"))
	if rec.Code != 201 || rec.Header().Get("Idempotent-Replayed") != "true" || rec.Header().Get("Location") != "/orders/1" || rec.Body.String() != `{"id":1}` {
		t.Fatalf("replay: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if executions.Load() != 1 {
		t.Fatalf("handler ran %d times", executions.Load())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, mk("/orders", `{"sku":"DIFFERENT"}`, "k1", "u1"))
	if rec.Code != 422 || rec.Header().Get("X-Error-Code") != "IdempotencyKeyReused" {
		t.Fatalf("reuse: %d %s", rec.Code, rec.Header().Get("X-Error-Code"))
	}
	// same key, different user: independent
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, mk("/orders", `{"sku":"a"}`, "k1", "u2"))
	if rec.Code != 201 || executions.Load() != 2 {
		t.Fatalf("scoped: %d %d", rec.Code, executions.Load())
	}
	// in progress
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), mk("/slow", ``, "k2", "u1")); close(done) }()
	for executions.Load() < 3 {
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, mk("/slow", ``, "k2", "u1"))
	if rec.Code != 409 {
		t.Fatalf("in progress: %d", rec.Code)
	}
	close(block)
	<-done
}

func TestServerErrorsAreNotMemoised(t *testing.T) {
	var n atomic.Int32
	h := Middleware(Options{Store: NewMemoryStore()})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/x", strings.NewReader("b"))
		req.Header.Set("Idempotency-Key", "k")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if i == 1 && (rec.Code != 200 || rec.Header().Get("Idempotent-Replayed") != "") {
			t.Fatalf("retry after 503 must re-execute: %d", rec.Code)
		}
	}
}

func TestSafeMethodsBypass(t *testing.T) {
	h := Middleware(Options{Store: NewMemoryStore(), Required: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 204 {
		t.Fatal("GET must bypass")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))
	if rec.Code != 400 {
		t.Fatalf("required key missing: %d", rec.Code)
	}
}
