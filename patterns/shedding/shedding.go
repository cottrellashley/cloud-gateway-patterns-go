// Package shedding protects the process from overload. Three tools, each
// copied from a production system:
//
//   - Admission: chi's Throttle — N concurrent slots plus a bounded backlog
//     with a wait timeout. Requests beyond the backlog are rejected at once.
//   - InFlight: the Kubernetes apiserver max-in-flight filter — separate
//     limits for read-only and mutating requests, long-running requests
//     (streams, watches) exempt, privileged callers exempt, 429 + Retry-After.
//   - ShutdownRetryAfter: the apiserver WithRetryAfter filter — once the
//     process is draining, new requests get 429, Retry-After and
//     Connection: close so clients move to another replica.
//
// Status codes: 503 means "this server is overloaded, try another replica";
// 429 means "you (this client) are over your share". Kubernetes uses 429 for
// in-flight limits because it has no other replica to point at; a gateway
// behind a load balancer should prefer 503 for capacity and 429 for quotas.
package shedding

import (
	"net/http"
	"strconv"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
)

// Admission bounds concurrency with a backlog.
type Admission struct {
	tokens         chan struct{}
	backlog        chan struct{}
	backlogTimeout time.Duration
	status         int
	retryAfter     int
}

// NewAdmission allows limit concurrent requests and queues up to backlog more
// for at most backlogTimeout. status is the rejection code (503 recommended).
func NewAdmission(limit, backlog int, backlogTimeout time.Duration, status int) *Admission {
	if limit < 1 {
		panic("shedding: limit must be >= 1")
	}
	a := &Admission{
		tokens:         make(chan struct{}, limit),
		backlog:        make(chan struct{}, limit+backlog),
		backlogTimeout: backlogTimeout,
		status:         status,
		retryAfter:     1,
	}
	for i := 0; i < limit+backlog; i++ {
		if i < limit {
			a.tokens <- struct{}{}
		}
		a.backlog <- struct{}{}
	}
	return a
}

// Middleware applies the admission control.
func (a *Admission) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		select {
		case <-ctx.Done():
			a.reject(w, "request canceled before admission")
			return
		case btok := <-a.backlog:
			defer func() { a.backlog <- btok }()
			select {
			case tok := <-a.tokens:
				defer func() { a.tokens <- tok }()
				next.ServeHTTP(w, r)
				return
			default:
			}
			timer := time.NewTimer(a.backlogTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				a.reject(w, "server busy: backlog wait timed out")
			case <-ctx.Done():
				a.reject(w, "request canceled while queued")
			case tok := <-a.tokens:
				defer func() { a.tokens <- tok }()
				next.ServeHTTP(w, r)
			}
		default:
			a.reject(w, "server busy: capacity and backlog exhausted")
		}
	})
}

func (a *Admission) reject(w http.ResponseWriter, msg string) {
	apierror.Write(w, apierror.New(a.status, apierror.CodeOverloaded, msg).WithRetryAfter(a.retryAfter))
}

// Class separates reads from writes for InFlight.
type Class int

const (
	ReadOnly Class = iota
	Mutating
)

// ClassifyByMethod is the apiserver rule: GET, HEAD, OPTIONS are read-only.
func ClassifyByMethod(r *http.Request) Class {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ReadOnly
	}
	return Mutating
}

// InFlight is the max-in-flight filter.
type InFlight struct {
	readOnly chan struct{}
	mutating chan struct{}
	// Classify defaults to ClassifyByMethod.
	Classify func(*http.Request) Class
	// LongRunning requests (SSE, WebSocket, watch) are not counted; they are
	// bounded by connection limits instead.
	LongRunning func(*http.Request) bool
	// Exempt callers (operators, health probes) always get through.
	Exempt func(*http.Request) bool
	// Status is the rejection code; 429 (apiserver) or 503.
	Status     int
	RetryAfter int
}

// NewInFlight creates the filter; a zero limit means unlimited for that class.
func NewInFlight(readOnlyLimit, mutatingLimit int) *InFlight {
	f := &InFlight{Classify: ClassifyByMethod, Status: http.StatusTooManyRequests, RetryAfter: 1}
	if readOnlyLimit > 0 {
		f.readOnly = make(chan struct{}, readOnlyLimit)
	}
	if mutatingLimit > 0 {
		f.mutating = make(chan struct{}, mutatingLimit)
	}
	return f
}

// Middleware applies the filter.
func (f *InFlight) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.LongRunning != nil && f.LongRunning(r) {
			next.ServeHTTP(w, r)
			return
		}
		var c chan struct{}
		if f.Classify(r) == Mutating {
			c = f.mutating
		} else {
			c = f.readOnly
		}
		if c == nil {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case c <- struct{}{}:
			defer func() { <-c }()
			next.ServeHTTP(w, r)
		default:
			if f.Exempt != nil && f.Exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			apierror.Write(w, apierror.New(f.Status, apierror.CodeOverloaded, "too many requests in flight").WithRetryAfter(f.RetryAfter))
		}
	})
}

// ShutdownRetryAfter rejects new requests once draining has started so that
// clients and load balancers move on. Exempt requests (health probes) still
// pass so the probe can observe the state.
func ShutdownRetryAfter(draining func() bool, retryAfterSeconds int, exempt func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !draining() || (exempt != nil && exempt(r)) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Connection", "close")
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
			apierror.Write(w, apierror.New(http.StatusTooManyRequests, apierror.CodeShuttingDown,
				"the server is shutting down, please retry").WithRetryAfter(retryAfterSeconds))
		})
	}
}
