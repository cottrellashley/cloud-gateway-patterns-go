// Package lro implements long-running operations the way Azure Resource
// Manager and the Azure REST guidelines specify them, and the way the MCP
// Tasks extension converged on independently:
//
//   - start returns 202 Accepted with Operation-Location and Retry-After;
//   - the operation record has a small fixed state machine
//     (Accepted → Running → Succeeded | Failed | Canceled);
//   - GET on the record returns 200 with a weak ETag; 304 on If-None-Match;
//     Retry-After while non-terminal;
//   - cancel is cooperative: it flips the state to Canceled only once the
//     worker observes the cancelled context;
//   - the same Idempotency-Key returns the same operation, never a second job;
//   - records are retained for a documented period after completion.
package lro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
)

// Status values.
type Status string

const (
	Accepted  Status = "Accepted"
	Running   Status = "Running"
	Succeeded Status = "Succeeded"
	Failed    Status = "Failed"
	Canceled  Status = "Canceled"
)

// Terminal reports whether no further transitions are possible.
func (s Status) Terminal() bool { return s == Succeeded || s == Failed || s == Canceled }

// Operation is the status monitor resource.
type Operation struct {
	ID              string          `json:"id"`
	Kind            string          `json:"kind"`
	Status          Status          `json:"status"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
	EndedAt         *time.Time      `json:"endedAt,omitempty"`
	PercentComplete float64         `json:"percentComplete,omitempty"`
	Message         string          `json:"message,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           *apierror.Error `json:"error,omitempty"`
	PollIntervalMs  int             `json:"pollIntervalMs"`
	Version         int64           `json:"-"` // bumps on every update; the ETag
}

// Store persists operations. Production: Cosmos DB (id as partition key,
// TTL for retention) or Postgres. Updates are compare-and-swap on Version.
type Store interface {
	Create(ctx context.Context, op *Operation) error
	Get(ctx context.Context, id string) (*Operation, error)
	Update(ctx context.Context, id string, fn func(op *Operation) error) (*Operation, error)
	// Sweep deletes terminal operations that ended before cutoff.
	Sweep(ctx context.Context, cutoff time.Time) (int, error)
}

// ErrNotFound is returned for unknown or swept operations.
var ErrNotFound = errors.New("lro: operation not found")

// MemoryStore is the test/single-replica store.
type MemoryStore struct {
	mu  sync.Mutex
	ops map[string]*Operation
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{ops: map[string]*Operation{}} }

func (s *MemoryStore) Create(_ context.Context, op *Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ops[op.ID]; ok {
		return fmt.Errorf("lro: duplicate id %s", op.ID)
	}
	cp := *op
	s.ops[op.ID] = &cp
	return nil
}

func (s *MemoryStore) Get(_ context.Context, id string) (*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *op
	return &cp, nil
}

func (s *MemoryStore) Update(_ context.Context, id string, fn func(op *Operation) error) (*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *op
	if err := fn(&cp); err != nil {
		return nil, err
	}
	cp.Version++
	cp.UpdatedAt = time.Now()
	s.ops[id] = &cp
	out := cp
	return &out, nil
}

func (s *MemoryStore) Sweep(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, op := range s.ops {
		if op.Status.Terminal() && op.EndedAt != nil && op.EndedAt.Before(cutoff) {
			delete(s.ops, id)
			n++
		}
	}
	return n, nil
}

// Progress is what a worker calls to report.
type Progress func(percent float64, message string)

// Worker does the work. It must return promptly when ctx is cancelled; the
// manager then records Canceled (or Failed, if the worker returns another
// error after cancellation).
type Worker func(ctx context.Context, progress Progress) (json.RawMessage, error)

// Manager starts, tracks and cancels operations.
type Manager struct {
	Store        Store
	Retention    time.Duration
	PollInterval time.Duration
	NewID        func() string
	// Deadline bounds every operation; 0 means none.
	Deadline time.Duration

	mu      sync.Mutex
	cancels map[string]context.CancelCauseFunc
	keys    map[string]string // idempotency key -> op id
	wg      sync.WaitGroup
}

// NewManager builds a manager with 24h retention and a 1s poll interval.
func NewManager(store Store) *Manager {
	return &Manager{
		Store: store, Retention: 24 * time.Hour, PollInterval: time.Second,
		NewID:   func() string { return "op_" + strconv.FormatInt(time.Now().UnixNano(), 36) + randomSuffix() },
		cancels: map[string]context.CancelCauseFunc{}, keys: map[string]string{},
	}
}

var errCanceledByClient = errors.New("lro: canceled by client")

// Start creates the record, then runs the worker in the background. The
// worker's context is detached from the request context (the HTTP request
// returns immediately) but inherits its values (trace, principal).
// idempotencyKey may be empty; when set, a repeat returns the existing
// operation.
func (m *Manager) Start(ctx context.Context, kind, idempotencyKey string, w Worker) (*Operation, bool, error) {
	m.mu.Lock()
	if idempotencyKey != "" {
		if id, ok := m.keys[idempotencyKey]; ok {
			m.mu.Unlock()
			op, err := m.Store.Get(ctx, id)
			return op, true, err
		}
	}
	now := time.Now()
	op := &Operation{ID: m.NewID(), Kind: kind, Status: Accepted, CreatedAt: now, UpdatedAt: now,
		PollIntervalMs: int(m.PollInterval / time.Millisecond)}
	if err := m.Store.Create(ctx, op); err != nil {
		m.mu.Unlock()
		return nil, false, err
	}
	if idempotencyKey != "" {
		m.keys[idempotencyKey] = op.ID
	}
	wctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	if m.Deadline > 0 {
		var c context.CancelFunc
		wctx, c = context.WithTimeout(wctx, m.Deadline)
		_ = c // cancel() below also stops the timer's parent
	}
	m.cancels[op.ID] = cancel
	m.mu.Unlock()

	m.wg.Add(1)
	go m.run(wctx, cancel, op.ID, w)
	return op, false, nil
}

func (m *Manager) run(ctx context.Context, cancel context.CancelCauseFunc, id string, w Worker) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		delete(m.cancels, id)
		m.mu.Unlock()
		cancel(nil)
	}()
	_, _ = m.Store.Update(ctx, id, func(op *Operation) error { op.Status = Running; return nil })
	progress := func(pct float64, msg string) {
		_, _ = m.Store.Update(ctx, id, func(op *Operation) error {
			if op.Status == Running {
				op.PercentComplete, op.Message = pct, msg
			}
			return nil
		})
	}
	result, err := func() (res json.RawMessage, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("lro: worker panicked: %v", r)
			}
		}()
		return w(ctx, progress)
	}()
	now := time.Now()
	_, _ = m.Store.Update(context.WithoutCancel(ctx), id, func(op *Operation) error {
		op.EndedAt = &now
		switch {
		case err == nil:
			op.Status, op.Result, op.PercentComplete = Succeeded, result, 100
		case errors.Is(context.Cause(ctx), errCanceledByClient):
			op.Status = Canceled
			op.Error = apierror.New(apierror.StatusClientClosedRequest, apierror.CodeCanceled, "the operation was canceled")
		default:
			op.Status = Failed
			op.Error = apierror.From(err)
		}
		return nil
	})
}

// Cancel requests cancellation. It returns the current record; the state
// becomes Canceled only when the worker stops.
func (m *Manager) Cancel(ctx context.Context, id string) (*Operation, error) {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	m.mu.Unlock()
	if ok {
		cancel(errCanceledByClient)
	}
	return m.Store.Get(ctx, id)
}

// Wait blocks until all workers have finished (for graceful shutdown).
func (m *Manager) Wait() { m.wg.Wait() }

// SweepLoop deletes expired records on the given interval until ctx ends.
func (m *Manager) SweepLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = m.Store.Sweep(ctx, time.Now().Add(-m.Retention))
		}
	}
}

func randomSuffix() string {
	// time-based IDs plus a counter are unique enough per process; stores
	// enforce uniqueness anyway.
	return strconv.FormatInt(int64(seq.Add(1)), 36)
}

// --- HTTP surface ---

// WriteAccepted writes the 202 response for a newly started operation.
// operationsPath is the base (e.g. "/v1/operations").
func WriteAccepted(w http.ResponseWriter, op *Operation, operationsPath string, replayed bool) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Operation-Location", operationsPath+"/"+op.ID)
	h.Set("Retry-After", strconv.Itoa(max(1, op.PollIntervalMs/1000)))
	h.Set("Operation-Id", op.ID)
	if replayed {
		h.Set("Idempotent-Replayed", "true")
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		Operation *Operation `json:"operation"`
	}{op})
}

// GetHandler serves GET {operationsPath}/{id} with ETag/304 and Retry-After.
func GetHandler(store Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		op, err := store.Get(r.Context(), id)
		if errors.Is(err, ErrNotFound) {
			apierror.Write(w, apierror.New(http.StatusNotFound, apierror.CodeNotFound, "operation not found or expired"))
			return
		}
		if err != nil {
			apierror.Write(w, err)
			return
		}
		etag := `W/"` + op.ID + ":" + strconv.FormatInt(op.Version, 10) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if !op.Status.Terminal() {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, op.PollIntervalMs/1000)))
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(struct {
			Operation *Operation `json:"operation"`
		}{op})
	})
}

// CancelHandler serves POST {operationsPath}/{id}/cancel. (Go's ServeMux
// patterns cannot express the ARM-style "{id}:cancel" suffix because a
// wildcard must be a whole path segment; a sub-resource path is equivalent.)
func CancelHandler(m *Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op, err := m.Cancel(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrNotFound) {
			apierror.Write(w, apierror.New(http.StatusNotFound, apierror.CodeNotFound, "operation not found or expired"))
			return
		}
		if err != nil {
			apierror.Write(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if op.Status.Terminal() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Operation *Operation `json:"operation"`
		}{op})
	})
}
