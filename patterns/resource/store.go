package resource

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Store is the storage contract for one collection. Implementations must
// provide the same semantics as MemoryStore:
//
//   - Create fails with ErrAlreadyExists on a name clash (name idempotency).
//   - Update writes spec and client-owned metadata (labels, annotations,
//     finalizers), ignores status, bumps Generation only when spec changed,
//     and fails with ErrConflict when the supplied ResourceVersion is not
//     current. An empty ResourceVersion means "unconditional".
//   - UpdateStatus writes status only, with the same conflict rule.
//   - Delete removes the object immediately when it has no finalizers;
//     otherwise it sets DeletionAt and leaves it visible. When Update later
//     removes the last finalizer from a deleting object, it is removed.
//   - Every successful write returns the stored object with a fresh
//     ResourceVersion.
type Store[S, T any] interface {
	Create(ctx context.Context, obj *Object[S, T]) (*Object[S, T], error)
	Get(ctx context.Context, name string) (*Object[S, T], error)
	List(ctx context.Context) ([]*Object[S, T], error)
	Update(ctx context.Context, obj *Object[S, T]) (*Object[S, T], error)
	UpdateStatus(ctx context.Context, obj *Object[S, T]) (*Object[S, T], error)
	Delete(ctx context.Context, name, resourceVersion string) (*Object[S, T], error)
}

// MemoryStore is an in-memory Store with the full semantics. Use it for
// tests, for process-local resources, and as the reference when writing a
// Cosmos DB / Postgres implementation (ETag maps to ResourceVersion).
type MemoryStore[S, T any] struct {
	mu      sync.RWMutex
	objects map[string]*Object[S, T]
	seq     uint64
	now     func() time.Time
	// Validate, if set, runs on Create and Update before anything is stored.
	Validate func(obj *Object[S, T]) error
}

// NewMemoryStore returns an empty store.
func NewMemoryStore[S, T any]() *MemoryStore[S, T] {
	return &MemoryStore[S, T]{objects: map[string]*Object[S, T]{}, now: time.Now}
}

// SetClock overrides the time source (tests).
func (s *MemoryStore[S, T]) SetClock(now func() time.Time) { s.now = now }

func (s *MemoryStore[S, T]) nextVersion() string {
	s.seq++
	return strconv.FormatUint(s.seq, 10)
}

func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func clone[S, T any](obj *Object[S, T]) *Object[S, T] {
	// JSON round trip: deep copy without reflection on arbitrary S/T.
	raw, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("resource: object not serialisable: %v", err))
	}
	var out Object[S, T]
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("resource: object not round-trippable: %v", err))
	}
	return &out
}

func specEqual[S any](a, b S) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return bytes.Equal(ra, rb)
}

func validName(name string) error {
	if name == "" || len(name) > 253 {
		return fmt.Errorf("%w: name must be 1-253 characters", ErrInvalid)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return fmt.Errorf("%w: name %q must be lowercase letters, digits, '-', '.' or '_'", ErrInvalid, name)
		}
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: name %q must start with a letter or digit", ErrInvalid, name)
	}
	return nil
}

// Create stores a new object. Server-owned metadata supplied by the client
// (uid, resourceVersion, generation, timestamps) is overwritten; status is
// cleared.
func (s *MemoryStore[S, T]) Create(ctx context.Context, obj *Object[S, T]) (*Object[S, T], error) {
	if err := validName(obj.Metadata.Name); err != nil {
		return nil, err
	}
	if s.Validate != nil {
		if err := s.Validate(obj); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.objects[obj.Metadata.Name]; exists {
		return nil, fmt.Errorf("%w: %s", ErrAlreadyExists, obj.Metadata.Name)
	}
	stored := clone(obj)
	var zero T
	stored.Status = zero
	stored.Metadata.UID = newUID()
	stored.Metadata.Generation = 1
	stored.Metadata.CreatedAt = s.now().UTC()
	stored.Metadata.DeletionAt = nil
	stored.Metadata.ResourceVersion = s.nextVersion()
	s.objects[stored.Metadata.Name] = stored
	return clone(stored), nil
}

// Get returns a copy of the named object.
func (s *MemoryStore[S, T]) Get(ctx context.Context, name string) (*Object[S, T], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return clone(obj), nil
}

// List returns copies of every object, sorted by name. Objects that are
// deleting are included (clients see DeletionAt).
func (s *MemoryStore[S, T]) List(ctx context.Context) ([]*Object[S, T], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Object[S, T], 0, len(s.objects))
	for _, obj := range s.objects {
		out = append(out, clone(obj))
	}
	slices.SortFunc(out, func(a, b *Object[S, T]) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
	return out, nil
}

func (s *MemoryStore[S, T]) checkVersion(current *Object[S, T], supplied string) error {
	if supplied != "" && supplied != current.Metadata.ResourceVersion {
		return fmt.Errorf("%w: %s has version %s, request carried %s", ErrConflict, current.Metadata.Name, current.Metadata.ResourceVersion, supplied)
	}
	return nil
}

// Update writes spec, labels, annotations and finalizers. Status is
// ignored; use UpdateStatus. Generation is incremented only if spec changed.
// Removing the last finalizer from a deleting object removes the object.
func (s *MemoryStore[S, T]) Update(ctx context.Context, obj *Object[S, T]) (*Object[S, T], error) {
	if s.Validate != nil {
		if err := s.Validate(obj); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.objects[obj.Metadata.Name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, obj.Metadata.Name)
	}
	if err := s.checkVersion(current, obj.Metadata.ResourceVersion); err != nil {
		return nil, err
	}
	if obj.Metadata.UID != "" && obj.Metadata.UID != current.Metadata.UID {
		return nil, fmt.Errorf("%w: %s uid mismatch (object was recreated)", ErrConflict, obj.Metadata.Name)
	}
	next := clone(current)
	if current.Metadata.Deleting() && !specEqual(current.Spec, obj.Spec) {
		return nil, fmt.Errorf("%w: %s is being deleted; spec is immutable", ErrInvalid, obj.Metadata.Name)
	}
	if !specEqual(current.Spec, obj.Spec) {
		next.Spec = clone(obj).Spec
		next.Metadata.Generation++
	}
	next.Metadata.Labels = cloneMap(obj.Metadata.Labels)
	next.Metadata.Annotations = cloneMap(obj.Metadata.Annotations)
	next.Metadata.Finalizers = slices.Clone(obj.Metadata.Finalizers)
	if len(next.Metadata.Finalizers) == 0 {
		next.Metadata.Finalizers = nil
	}
	next.Metadata.ResourceVersion = s.nextVersion()
	if next.Metadata.Deleting() && len(next.Metadata.Finalizers) == 0 {
		delete(s.objects, next.Metadata.Name)
		return next, nil
	}
	s.objects[next.Metadata.Name] = next
	return clone(next), nil
}

// UpdateStatus writes status only. Spec and client metadata are ignored.
func (s *MemoryStore[S, T]) UpdateStatus(ctx context.Context, obj *Object[S, T]) (*Object[S, T], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.objects[obj.Metadata.Name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, obj.Metadata.Name)
	}
	if err := s.checkVersion(current, obj.Metadata.ResourceVersion); err != nil {
		return nil, err
	}
	next := clone(current)
	next.Status = clone(obj).Status
	next.Metadata.ResourceVersion = s.nextVersion()
	s.objects[next.Metadata.Name] = next
	return clone(next), nil
}

// Delete removes the object, or marks it deleting if it has finalizers.
// resourceVersion "" is unconditional. Deleting an already-deleting object
// is a no-op that returns the current state.
func (s *MemoryStore[S, T]) Delete(ctx context.Context, name, resourceVersion string) (*Object[S, T], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.objects[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err := s.checkVersion(current, resourceVersion); err != nil {
		return nil, err
	}
	if len(current.Metadata.Finalizers) == 0 {
		delete(s.objects, name)
		return clone(current), nil
	}
	if current.Metadata.Deleting() {
		return clone(current), nil
	}
	next := clone(current)
	t := s.now().UTC()
	next.Metadata.DeletionAt = &t
	next.Metadata.ResourceVersion = s.nextVersion()
	s.objects[name] = next
	return clone(next), nil
}

func cloneMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
