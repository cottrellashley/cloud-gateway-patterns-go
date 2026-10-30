package lb

import (
	"net/url"
	"testing"
	"time"
)

func mk(name string, w float64) *Upstream {
	u, _ := url.Parse("http://" + name)
	return NewUpstream(name, u, w)
}

func TestWRRHonoursWeights(t *testing.T) {
	var b WeightedRoundRobin
	a, c := mk("a", 3), mk("c", 1)
	b.Add(a)
	b.Add(c)
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		u, err := b.Pick()
		if err != nil {
			t.Fatal(err)
		}
		counts[u.Name]++
	}
	if counts["a"] != 300 || counts["c"] != 100 {
		t.Fatalf("distribution %v", counts)
	}
}

func TestWRRSkipsUnavailable(t *testing.T) {
	var b WeightedRoundRobin
	a, c := mk("a", 1), mk("c", 1)
	b.Add(a)
	b.Add(c)
	a.SetHealthy(false)
	for i := 0; i < 10; i++ {
		u, err := b.Pick()
		if err != nil || u != c {
			t.Fatalf("picked %v %v", u, err)
		}
	}
	c.SetHealthy(false)
	if _, err := b.Pick(); err != ErrNoAvailableUpstream {
		t.Fatalf("want ErrNoAvailableUpstream, got %v", err)
	}
}

func TestP2CPrefersLessLoaded(t *testing.T) {
	var b PowerOfTwoChoices
	a, c := mk("a", 1), mk("c", 1)
	b.Add(a)
	b.Add(c)
	for i := 0; i < 50; i++ {
		a.Acquire()
	}
	for i := 0; i < 100; i++ {
		u, err := b.Pick()
		if err != nil {
			t.Fatal(err)
		}
		if u != c {
			t.Fatalf("picked the loaded upstream")
		}
	}
}

func TestPassiveFailureIsForgotten(t *testing.T) {
	u := mk("a", 1)
	u.RecordFailure(20 * time.Millisecond)
	if u.Available(1) {
		t.Fatal("should be out of rotation after one failure with MaxFails=1")
	}
	time.Sleep(40 * time.Millisecond)
	if !u.Available(1) {
		t.Fatal("failure should have been forgotten")
	}
}

func TestFull(t *testing.T) {
	u := mk("a", 1)
	u.MaxRequests = 1
	u.Acquire()
	if u.Available(0) {
		t.Fatal("full upstream must not be available")
	}
	u.Release()
	if !u.Available(0) {
		t.Fatal("released upstream must be available")
	}
}
