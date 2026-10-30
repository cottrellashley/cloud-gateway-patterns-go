package session

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newMgr(t *testing.T) *Manager {
	key := make([]byte, 32)
	rand.Read(key)
	c, err := NewCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultCookieOptions("gw_session")
	opts.Secure = false
	return NewManager(c, NewMemoryStore(), opts)
}

func TestCodecRoundTripAndTamper(t *testing.T) {
	m := newMgr(t)
	s := m.codec.Seal([]byte("hello"), []byte("ad"))
	pt, err := m.codec.Open(s, []byte("ad"))
	if err != nil || string(pt) != "hello" {
		t.Fatal(err, string(pt))
	}
	if _, err := m.codec.Open(s, []byte("other")); err == nil {
		t.Fatal("different AD must fail")
	}
	b := []byte(s)
	b[len(b)-1] ^= 1
	if _, err := m.codec.Open(string(b), []byte("ad")); err == nil {
		t.Fatal("tampered must fail")
	}
}

func TestSaveLoadRotateClear(t *testing.T) {
	m := newMgr(t)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	s := &Session{Subject: "ashley", Data: map[string]string{"theme": "dark"}}
	if err := m.Save(ctx, rec, s); err != nil {
		t.Fatal(err)
	}
	cookie := rec.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge <= 0 {
		t.Fatalf("cookie attrs: %+v", cookie)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	got, err := m.Load(req)
	if err != nil || got.Subject != "ashley" || got.Data["theme"] != "dark" {
		t.Fatalf("load: %v %+v", err, got)
	}

	// rotate: old id invalid, new cookie valid
	rec = httptest.NewRecorder()
	oldID := got.ID
	if err := m.Rotate(ctx, rec, got); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.Get(ctx, oldID); !errors.Is(err, ErrNoSession) {
		t.Fatal("old session must be gone")
	}
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.AddCookie(rec.Result().Cookies()[0])
	if _, err := m.Load(req2); err != nil {
		t.Fatalf("rotated load: %v", err)
	}

	// clear
	rec = httptest.NewRecorder()
	m.Clear(ctx, rec, req2)
	if c := rec.Result().Cookies()[0]; c.MaxAge != -1 {
		t.Fatalf("clear cookie: %+v", c)
	}
	if _, err := m.Load(req2); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after clear: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	m := newMgr(t)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	s := &Session{Subject: "x"}
	_ = m.Save(ctx, rec, s)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])
	m.now = func() time.Time { return time.Now().Add(9 * time.Hour) }
	if _, err := m.Load(req); !errors.Is(err, ErrExpired) {
		t.Fatalf("want expired, got %v", err)
	}
}
