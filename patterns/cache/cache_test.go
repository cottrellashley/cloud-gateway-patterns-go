package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlightCoalesces(t *testing.T) {
	var f Flight[string, int]
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	var shared atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err, sh := f.Do("k", func() (int, error) { calls.Add(1); <-release; return 7, nil })
			if err != nil || v != 7 {
				t.Errorf("%v %v", v, err)
			}
			if sh {
				shared.Add(1)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 || shared.Load() < 19 {
		t.Fatalf("calls=%d shared=%d", calls.Load(), shared.Load())
	}
}

func TestTTLEvictsAndExpires(t *testing.T) {
	c := NewTTL[string, int](2, time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("a") // a is now MRU
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted (LRU)")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatal("a should survive")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have expired")
	}
}

func TestGetOrLoadStampede(t *testing.T) {
	c := NewTTL[string, string](10, time.Minute)
	var loads atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "jwks", func(context.Context) (string, error) {
				loads.Add(1)
				time.Sleep(10 * time.Millisecond)
				return "doc", nil
			})
			if err != nil || v != "doc" {
				t.Error(v, err)
			}
		}()
	}
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("loads %d", loads.Load())
	}
}
