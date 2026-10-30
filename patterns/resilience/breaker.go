package resilience

import (
	"errors"
	"sync"
	"time"
)

// State of a Breaker.
type State int

const (
	Closed State = iota
	HalfOpen
	Open
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half-open"
	default:
		return "open"
	}
}

var (
	// ErrOpen is returned without calling the function while the breaker is open.
	ErrOpen = errors.New("resilience: circuit breaker is open")
	// ErrTooManyRequests is returned in half-open state when the trial slots are used.
	ErrTooManyRequests = errors.New("resilience: too many requests in half-open state")
)

// Counts are the statistics of the current generation.
type Counts struct {
	Requests             uint32
	TotalSuccesses       uint32
	TotalFailures        uint32
	ConsecutiveSuccesses uint32
	ConsecutiveFailures  uint32
}

// BreakerSettings mirror gobreaker.Settings.
type BreakerSettings struct {
	Name string
	// MaxRequests allowed through in half-open state before closing.
	MaxRequests uint32
	// Interval resets counts while closed (0 = never, so counts accumulate).
	Interval time.Duration
	// Timeout is how long the breaker stays open before trying half-open.
	Timeout time.Duration
	// ReadyToTrip decides, after a failure in closed state, whether to open.
	ReadyToTrip func(Counts) bool
	// IsSuccessful classifies errors; nil means err == nil.
	IsSuccessful  func(error) bool
	OnStateChange func(name string, from, to State)
}

// Breaker is gobreaker's state machine: closed → open on ReadyToTrip,
// open → half-open after Timeout, half-open → closed after MaxRequests
// consecutive successes, half-open → open on any failure. A generation
// counter makes results from a previous generation harmless.
type Breaker struct {
	s BreakerSettings

	mu         sync.Mutex
	state      State
	generation uint64
	counts     Counts
	expiry     time.Time
	now        func() time.Time
}

// NewBreaker applies gobreaker's defaults: MaxRequests 1, Timeout 60s,
// ReadyToTrip after 5 consecutive failures.
func NewBreaker(s BreakerSettings) *Breaker {
	if s.MaxRequests == 0 {
		s.MaxRequests = 1
	}
	if s.Timeout <= 0 {
		s.Timeout = 60 * time.Second
	}
	if s.ReadyToTrip == nil {
		s.ReadyToTrip = func(c Counts) bool { return c.ConsecutiveFailures > 5 }
	}
	if s.IsSuccessful == nil {
		s.IsSuccessful = func(err error) bool { return err == nil }
	}
	b := &Breaker{s: s, now: time.Now}
	b.toNewGeneration(b.now())
	return b
}

// Execute runs fn if the breaker allows it and records the outcome.
func (b *Breaker) Execute(fn func() error) error {
	gen, err := b.beforeRequest()
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			b.afterRequest(gen, false)
			panic(r)
		}
	}()
	err = fn()
	b.afterRequest(gen, b.s.IsSuccessful(err))
	return err
}

// Do is Execute for functions that return a value.
func Do[T any](b *Breaker, fn func() (T, error)) (T, error) {
	var v T
	err := b.Execute(func() error {
		var err error
		v, err = fn()
		return err
	})
	return v, err
}

// State returns the current state (advancing open → half-open if due).
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, _ := b.currentState(b.now())
	return s
}

func (b *Breaker) beforeRequest() (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	state, gen := b.currentState(now)
	if state == Open {
		return gen, ErrOpen
	}
	if state == HalfOpen && b.counts.Requests >= b.s.MaxRequests {
		return gen, ErrTooManyRequests
	}
	b.counts.Requests++
	return gen, nil
}

func (b *Breaker) afterRequest(before uint64, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	state, gen := b.currentState(now)
	if gen != before {
		return // result from a previous generation: ignore
	}
	if success {
		b.counts.TotalSuccesses++
		b.counts.ConsecutiveSuccesses++
		b.counts.ConsecutiveFailures = 0
		if state == HalfOpen && b.counts.ConsecutiveSuccesses >= b.s.MaxRequests {
			b.setState(Closed, now)
		}
		return
	}
	b.counts.TotalFailures++
	b.counts.ConsecutiveFailures++
	b.counts.ConsecutiveSuccesses = 0
	switch state {
	case Closed:
		if b.s.ReadyToTrip(b.counts) {
			b.setState(Open, now)
		}
	case HalfOpen:
		b.setState(Open, now)
	}
}

func (b *Breaker) currentState(now time.Time) (State, uint64) {
	switch b.state {
	case Closed:
		if !b.expiry.IsZero() && b.expiry.Before(now) {
			b.toNewGeneration(now)
		}
	case Open:
		if b.expiry.Before(now) {
			b.setState(HalfOpen, now)
		}
	}
	return b.state, b.generation
}

func (b *Breaker) setState(s State, now time.Time) {
	if b.state == s {
		return
	}
	prev := b.state
	b.state = s
	b.toNewGeneration(now)
	if b.s.OnStateChange != nil {
		b.s.OnStateChange(b.s.Name, prev, s)
	}
}

func (b *Breaker) toNewGeneration(now time.Time) {
	b.generation++
	b.counts = Counts{}
	switch b.state {
	case Closed:
		if b.s.Interval == 0 {
			b.expiry = time.Time{}
		} else {
			b.expiry = now.Add(b.s.Interval)
		}
	case Open:
		b.expiry = now.Add(b.s.Timeout)
	default:
		b.expiry = time.Time{}
	}
}
