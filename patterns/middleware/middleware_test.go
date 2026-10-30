package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { order = append(order, "h") }), mk("a"), mk("b"), mk("c"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if got := strings.Join(order, ""); got != "abch" {
		t.Fatalf("order %q", got)
	}
}

func TestRecoverWritesEnvelopeAndKeepsAbortHandler(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Recover(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("X-Error-Code") != "InternalError" {
		t.Fatalf("got %d %s", rec.Code, rec.Header().Get("X-Error-Code"))
	}
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("ErrAbortHandler must propagate")
		}
	}()
	Recover(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestRequestIDReusesValidAndRejectsJunk(t *testing.T) {
	var seen string
	h := RequestID("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = RequestIDFrom(r.Context()) }))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-Id", "fd-abc.123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "fd-abc.123" || rec.Header().Get("X-Request-Id") != "fd-abc.123" {
		t.Fatalf("valid id not reused: %q", seen)
	}
	req.Header.Set("X-Request-Id", "bad id\n")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen == "bad id\n" || len(seen) != 32 {
		t.Fatalf("junk id accepted: %q", seen)
	}
}

func TestRealIPOnlyTrustsConfiguredProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	var got string
	h := RealIP(trusted, "X-Azure-ClientIP")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = ClientIPFrom(r.Context()) }))

	// Untrusted peer: headers ignored.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "203.0.113.9" {
		t.Fatalf("spoofed: %s", got)
	}

	// Trusted peer: walk XFF from the right, skipping trusted hops.
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 198.51.100.7, 10.9.9.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "198.51.100.7" {
		t.Fatalf("want rightmost untrusted, got %s", got)
	}

	// Preferred header wins when peer trusted.
	req.Header.Set("X-Azure-ClientIP", "192.0.2.44")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "192.0.2.44" {
		t.Fatalf("preferred header ignored: %s", got)
	}
}

func TestDeadlineWrites504WhenHandlerDoesNothing(t *testing.T) {
	h := Deadline("X-Request-Deadline", 20*time.Millisecond, time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestDeadlineHonoursClientHeaderButCapsAtMax(t *testing.T) {
	var d time.Duration
	h := Deadline("X-Request-Deadline", time.Minute, 2*time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dl, _ := r.Context().Deadline()
		d = time.Until(dl)
		w.WriteHeader(204)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-Deadline", time.Now().Add(10*time.Second).Format(time.RFC3339Nano))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if d > 2*time.Second || d < time.Second {
		t.Fatalf("deadline not capped: %v", d)
	}
}

func TestMaxBytes(t *testing.T) {
	h := MaxBytes(4)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			t.Fatal("expected error")
		}
		var mbe *http.MaxBytesError
		if !errorsAs(err, &mbe) {
			t.Fatalf("got %T", err)
		}
		w.WriteHeader(413)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("too long body")))
}

func errorsAs(err error, target **http.MaxBytesError) bool {
	for err != nil {
		if e, ok := err.(*http.MaxBytesError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

var _ = context.Background
