package shedding

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdmissionRejectsBeyondBacklog(t *testing.T) {
	a := NewAdmission(1, 1, 50*time.Millisecond, http.StatusServiceUnavailable)
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(200)
	}))
	var wg sync.WaitGroup
	var codes [3]int32
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
			atomic.StoreInt32(&codes[i], int32(rec.Code))
		}(i)
		time.Sleep(10 * time.Millisecond) // deterministic arrival order
	}
	<-started
	time.Sleep(100 * time.Millisecond) // backlog waiter times out
	close(release)
	wg.Wait()
	var ok, rejected int
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 503:
			rejected++
		}
	}
	if ok != 1 || rejected != 2 {
		t.Fatalf("codes %v", codes)
	}
}

func TestInFlightSeparatesClasses(t *testing.T) {
	f := NewInFlight(1, 1)
	f.Exempt = func(r *http.Request) bool { return r.Header.Get("X-Operator") == "1" }
	block := make(chan struct{})
	h := f.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block; w.WriteHeader(200) }))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	time.Sleep(20 * time.Millisecond)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil)) // read slot taken
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("second read: %d", rec.Code)
	}
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil)) // write slot free
	time.Sleep(20 * time.Millisecond)
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Operator", "1")
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	close(block)
	<-done
	if rec.Code != 200 {
		t.Fatalf("exempt caller rejected: %d", rec.Code)
	}
}

func TestShutdownRetryAfter(t *testing.T) {
	var draining atomic.Bool
	h := ShutdownRetryAfter(draining.Load, 5, func(r *http.Request) bool { return r.URL.Path == "/readyz" })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 204 {
		t.Fatal("must pass before draining")
	}
	draining.Store(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 429 || rec.Header().Get("Connection") != "close" || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("draining: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 204 {
		t.Fatal("probe must be exempt")
	}
}
