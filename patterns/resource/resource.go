// Package resource is the uniform resource model: one envelope for every
// object the control plane manages, with Kubernetes semantics for metadata,
// desired state (spec) versus observed state (status), conditions,
// generations and optimistic concurrency.
//
// It implements docs/00-principles/01-uniform-resource-model.md and
// 02-declarative-intent-and-reconciliation.md. The shape follows
// k8s.io/apimachinery/pkg/apis/meta/v1 (TypeMeta, ObjectMeta, Condition) and
// the API conventions in kubernetes/community; the HTTP precondition helpers
// follow RFC 9110 and the Azure REST guidelines on conditional requests.
package resource

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
)

// Meta is the metadata every object carries.
type Meta struct {
	// Name is unique within the collection and appears in the URL.
	Name string `json:"name"`
	// UID distinguishes an object from a deleted-and-recreated one with the
	// same name. Set by the store.
	UID string `json:"uid,omitempty"`
	// ResourceVersion is an opaque token that changes on every write. Clients
	// send it back unchanged (If-Match / resourceVersion) for optimistic
	// concurrency. Set by the store.
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// Generation increases only when Spec changes, never on status writes.
	// Status reports the generation it observed so clients can tell whether
	// the controller has caught up with the latest spec.
	Generation int64 `json:"generation,omitempty"`
	// CreatedAt is set by the store on Create.
	CreatedAt time.Time `json:"createdAt,omitzero"`
	// DeletionAt is set when a graceful delete is requested on an object
	// that still has finalizers. The object stays visible until the last
	// finalizer is removed, then disappears.
	DeletionAt *time.Time `json:"deletionAt,omitempty"`
	// Finalizers are names of controllers that must run before removal.
	Finalizers []string `json:"finalizers,omitempty"`
	// Labels are for selection; Annotations are for tooling. Neither is
	// interpreted by the store.
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Object is the envelope: type identity, metadata, desired state, observed
// state. Spec is written by clients; Status is written by controllers
// through a separate operation and ignored on ordinary updates.
type Object[S, T any] struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   Meta   `json:"metadata"`
	Spec       S      `json:"spec"`
	Status     T      `json:"status,omitzero"`
}

// Deleting reports whether a graceful delete is in progress.
func (m Meta) Deleting() bool { return m.DeletionAt != nil }

// HasFinalizer reports whether name is present.
func (m Meta) HasFinalizer(name string) bool {
	for _, f := range m.Finalizers {
		if f == name {
			return true
		}
	}
	return false
}

// AddFinalizer appends name if absent and reports whether it changed.
func (m *Meta) AddFinalizer(name string) bool {
	if m.HasFinalizer(name) {
		return false
	}
	m.Finalizers = append(m.Finalizers, name)
	return true
}

// RemoveFinalizer removes name and reports whether it changed.
func (m *Meta) RemoveFinalizer(name string) bool {
	out := m.Finalizers[:0:0]
	for _, f := range m.Finalizers {
		if f != name {
			out = append(out, f)
		}
	}
	changed := len(out) != len(m.Finalizers)
	m.Finalizers = out
	if len(m.Finalizers) == 0 {
		m.Finalizers = nil
	}
	return changed
}

// ConditionStatus is True, False or Unknown. Unknown is a real state:
// a controller that cannot tell must say so, not guess.
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition is the standard status element (meta/v1.Condition). Type is a
// CamelCase name such as Ready or Accepted; Reason is a CamelCase
// programmatic identifier; Message is for humans.
type Condition struct {
	Type               string          `json:"type"`
	Status             ConditionStatus `json:"status"`
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	LastTransitionTime time.Time       `json:"lastTransitionTime"`
	Reason             string          `json:"reason"`
	Message            string          `json:"message,omitempty"`
}

// FindCondition returns the condition of the given type or nil.
func FindCondition(conds []Condition, typ string) *Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}

// IsConditionTrue reports whether the condition exists and is True.
func IsConditionTrue(conds []Condition, typ string) bool {
	c := FindCondition(conds, typ)
	return c != nil && c.Status == ConditionTrue
}

// SetCondition upserts c into conds. LastTransitionTime is set to now only
// when the Status changes (or the condition is new); reason, message and
// observedGeneration always take the new values. It reports whether
// anything changed, so callers can skip a status write when nothing did.
func SetCondition(conds *[]Condition, c Condition, now time.Time) bool {
	if c.LastTransitionTime.IsZero() {
		c.LastTransitionTime = now
	}
	existing := FindCondition(*conds, c.Type)
	if existing == nil {
		*conds = append(*conds, c)
		return true
	}
	changed := false
	if existing.Status != c.Status {
		existing.Status = c.Status
		existing.LastTransitionTime = c.LastTransitionTime
		changed = true
	}
	if existing.Reason != c.Reason {
		existing.Reason = c.Reason
		changed = true
	}
	if existing.Message != c.Message {
		existing.Message = c.Message
		changed = true
	}
	if existing.ObservedGeneration != c.ObservedGeneration {
		existing.ObservedGeneration = c.ObservedGeneration
		changed = true
	}
	return changed
}

// RemoveCondition deletes the condition of the given type.
func RemoveCondition(conds *[]Condition, typ string) bool {
	out := (*conds)[:0:0]
	for _, c := range *conds {
		if c.Type != typ {
			out = append(out, c)
		}
	}
	changed := len(out) != len(*conds)
	*conds = out
	return changed
}

// Store errors. Map them to the wire with APIError.
var (
	ErrNotFound           = errors.New("resource: not found")
	ErrAlreadyExists      = errors.New("resource: already exists")
	ErrConflict           = errors.New("resource: resource version conflict")
	ErrInvalid            = errors.New("resource: invalid")
	ErrPreconditionFailed = errors.New("resource: precondition failed")
	ErrNotModified        = errors.New("resource: not modified")
)

// APIError maps a store error to the shared error envelope. Unknown errors
// become 500 with the cause hidden, as apierror.From does.
func APIError(err error) *apierror.Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return apierror.Wrap(err, http.StatusNotFound, apierror.CodeNotFound, "the resource does not exist")
	case errors.Is(err, ErrAlreadyExists):
		return apierror.Wrap(err, http.StatusConflict, "AlreadyExists", "a resource with this name already exists")
	case errors.Is(err, ErrConflict):
		return apierror.Wrap(err, http.StatusConflict, apierror.CodeConflict, "the resource was modified; re-read it and retry with the current resourceVersion")
	case errors.Is(err, ErrPreconditionFailed):
		return apierror.Wrap(err, http.StatusPreconditionFailed, "PreconditionFailed", "the If-Match condition was not met")
	case errors.Is(err, ErrInvalid):
		return apierror.Wrap(err, http.StatusBadRequest, apierror.CodeInvalidInput, strings.TrimPrefix(err.Error(), "resource: "))
	default:
		return apierror.From(err)
	}
}

// ETag renders a resource version as a strong entity tag.
func ETag(resourceVersion string) string {
	return fmt.Sprintf("%q", resourceVersion)
}

// SetETag writes the ETag header for the given resource version.
func SetETag(w http.ResponseWriter, resourceVersion string) {
	if resourceVersion != "" {
		w.Header().Set("ETag", ETag(resourceVersion))
	}
}

// CheckPreconditions evaluates If-Match and If-None-Match against the
// current resource version (RFC 9110 §13). For writes, a non-matching
// If-Match yields ErrPreconditionFailed. For GET/HEAD, a matching
// If-None-Match yields ErrNotModified so the handler can answer 304.
// An absent header is not a precondition: the request proceeds.
func CheckPreconditions(r *http.Request, current string) error {
	if im := r.Header.Get("If-Match"); im != "" {
		if !tagMatches(im, current) {
			return ErrPreconditionFailed
		}
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if tagMatches(inm, current) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				return ErrNotModified
			}
			return ErrPreconditionFailed
		}
	}
	return nil
}

func tagMatches(header, current string) bool {
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" {
			return true
		}
		tag = strings.TrimPrefix(tag, "W/")
		if tag == ETag(current) {
			return true
		}
	}
	return false
}
