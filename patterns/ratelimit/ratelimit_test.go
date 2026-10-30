package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBucketRefillsAndBounds(t *testing.T) {
	b := NewBucket(10, 5) // 10/s, burst 5
	now := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		if !b.Allow(now) {
			t.Fatalf("burst request %d rejected", i)
		}
	}
	if b.Allow(now) {
		t.Fatal("6th request should be rejected")
	}
	if b.Allow(now.Add(100*time.Millisecond)) == false {
		t.Fatal("one token should have refilled after 100ms")
	}
	if _, ok := b.Reserve(now, 6, time.Hour); ok {
		t.Fatal("n > burst must fail")
	}
	d, ok := b.Reserve(now.Add(time.Second), 5, time.Second)
	if !ok || d != 0 {
		t.Fatalf("full bucket reserve: %v %v", d, ok)
	}
	d, ok = b.Reserve(now.Add(time.Second), 1, time.Second)
	if !ok || d != 100*time.Millisecond {
		t.Fatalf("expected 100ms wait, got %v %v", d, ok)
	}
}

func TestKeyedEvictsAndIsolates(t *testing.T) {
	k := NewKeyed(1, 2, 2)
	now := time.Unix(0, 0)
	k.now = func() time.Time { return now }
	ctx := context.Background()
	for _, key := range []string{"a", "a", "b"} {
		d, _ := k.Allow(ctx, key)
		if !d.Allowed {
			t.Fatalf("key %s should be allowed", key)
		}
	}
	d, _ := k.Allow(ctx, "a")
	if d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("a exhausted: %+v", d)
	}
	// capacity 2: adding c evicts the soonest-expiring key
	k.Allow(ctx, "c")
	if k.Len() != 2 {
		t.Fatalf("len %d", k.Len())
	}
	now = now.Add(time.Minute)
	d, _ = k.Allow(ctx, "a")
	if !d.Allowed || d.Remaining != 1 {
		t.Fatalf("after refill: %+v", d)
	}
}

func TestFixedWindow(t *testing.T) {
	now := time.Unix(100, 0)
	c := NewMemoryCounter()
	c.now = func() time.Time { return now }
	fw := &FixedWindow{Counter: c, Limit: 2, Unit: 10 * time.Second, now: func() time.Time { return now }}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if d, _ := fw.Allow(ctx, "u1"); !d.Allowed {
			t.Fatalf("hit %d rejected", i)
		}
	}
	d, _ := fw.Allow(ctx, "u1")
	if d.Allowed || d.RetryAfter != 10*time.Second || d.Remaining != 0 {
		t.Fatalf("over limit: %+v", d)
	}
	now = now.Add(10 * time.Second) // next window
	if d, _ := fw.Allow(ctx, "u1"); !d.Allowed || d.Remaining != 1 {
		t.Fatalf("new window: %+v", d)
	}
}

func TestMiddlewareHeadersAnd429(t *testing.T) {
	k := NewKeyed(1, 1, 10)
	h := Middleware(k, func(r *http.Request) string { return r.Header.Get("X-User") }, "per-user", false)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-User", "u")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 || rec.Header().Get("RateLimit-Policy") != `"per-user";q=1;w=1` {
		t.Fatalf("first: %d %q", rec.Code, rec.Header().Get("RateLimit-Policy"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "1" || rec.Header().Get("X-Error-Code") != "RateLimited" {
		t.Fatalf("second: %d %v", rec.Code, rec.Header())
	}
	if rec.Header().Get("RateLimit") == "" {
		t.Fatal("RateLimit header missing on 429")
	}
	// no key: not limited
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 204 {
		t.Fatal("unkeyed request must pass")
	}
}
