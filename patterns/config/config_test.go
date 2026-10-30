package config

import (
	"context"
	"errors"
	"testing"
	"time"
)

type routes struct {
	Upstreams []string
	Timeout   time.Duration
}

func validate(r routes) error {
	if len(r.Upstreams) == 0 {
		return errors.New("no upstreams")
	}
	return nil
}

func TestSwapValidatesDedupesAndNotifies(t *testing.T) {
	h, err := New(routes{Upstreams: []string{"a"}, Timeout: time.Second}, validate)
	if err != nil {
		t.Fatal(err)
	}
	var notified int
	h.OnChange(func(routes) { notified++ })

	if changed, err := h.Swap(routes{Upstreams: []string{"a"}, Timeout: time.Second}); changed || err != nil {
		t.Fatal("identical config must not count as a change")
	}
	v1 := h.CurrentVersion()
	if _, err := h.Swap(routes{}); err == nil || h.CurrentVersion() != v1 {
		t.Fatal("invalid config must be rejected and the old one kept")
	}
	changed, err := h.Swap(routes{Upstreams: []string{"a", "b"}, Timeout: time.Second})
	if !changed || err != nil || notified != 1 || len(h.Load().Upstreams) != 2 || h.CurrentVersion() == v1 {
		t.Fatalf("swap: %v %v notified=%d", changed, err, notified)
	}
}

func TestCheckIfMatch(t *testing.T) {
	if err := CheckIfMatch(`"abc"`, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := CheckIfMatch(`W/"abc"`, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := CheckIfMatch(`"old"`, "abc"); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatal(err)
	}
	if err := CheckIfMatch("", "abc"); err != nil {
		t.Fatal("empty If-Match is unconditional")
	}
}

func TestWatchKeepsLastGood(t *testing.T) {
	h, _ := New(routes{Upstreams: []string{"a"}}, validate)
	calls := 0
	load := func(context.Context) (routes, error) {
		calls++
		switch calls {
		case 1:
			return routes{}, errors.New("provider down")
		case 2:
			return routes{Upstreams: []string{"b"}}, nil
		}
		return routes{Upstreams: []string{"b"}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	Watch(ctx, h, 10*time.Millisecond, load, nil)
	if got := h.Load().Upstreams; len(got) != 1 || got[0] != "b" {
		t.Fatalf("got %v", got)
	}
}
