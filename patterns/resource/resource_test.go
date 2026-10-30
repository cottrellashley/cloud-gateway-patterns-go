package resource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type capSpec struct {
	Provider string   `json:"provider"`
	Modes    []string `json:"modes,omitempty"`
}

type capStatus struct {
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty"`
}

type capability = Object[capSpec, capStatus]

func newCap(name, provider string) *capability {
	return &capability{APIVersion: "gateway/v1", Kind: "Capability", Metadata: Meta{Name: name}, Spec: capSpec{Provider: provider}}
}

func TestCreateGetConflictSemantics(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore[capSpec, capStatus]()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return base })

	in := newCap("portfolios", "snowflake")
	in.Metadata.UID = "client-supplied"
	in.Status.Conditions = []Condition{{Type: "Ready", Status: ConditionTrue}}
	created, err := s.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if created.Metadata.UID == "client-supplied" || created.Metadata.UID == "" {
		t.Fatal("uid must be server-assigned")
	}
	if created.Metadata.Generation != 1 || created.Metadata.ResourceVersion == "" || !created.Metadata.CreatedAt.Equal(base) {
		t.Fatalf("server metadata not set: %+v", created.Metadata)
	}
	if len(created.Status.Conditions) != 0 {
		t.Fatal("status supplied on create must be ignored")
	}
	if _, err := s.Create(ctx, newCap("portfolios", "x")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
	if _, err := s.Create(ctx, newCap("Bad Name", "x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// Update with stale version conflicts; with current version succeeds.
	stale := created.Metadata.ResourceVersion
	upd := *created
	upd.Spec.Modes = []string{"execute"}
	v2, err := s.Update(ctx, &upd)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Metadata.Generation != 2 {
		t.Fatalf("spec change must bump generation, got %d", v2.Metadata.Generation)
	}
	upd.Metadata.ResourceVersion = stale
	if _, err := s.Update(ctx, &upd); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	// Metadata-only update does not bump generation; status is ignored.
	v2.Metadata.Labels = map[string]string{"tier": "gold"}
	v2.Status.Conditions = []Condition{{Type: "Ready", Status: ConditionTrue}}
	v3, err := s.Update(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}
	if v3.Metadata.Generation != 2 || v3.Metadata.Labels["tier"] != "gold" || len(v3.Status.Conditions) != 0 {
		t.Fatalf("metadata update semantics wrong: %+v", v3)
	}
	// Status subresource writes status only and leaves generation alone.
	v3.Status = capStatus{ObservedGeneration: 2}
	v3.Spec.Provider = "ignored"
	v4, err := s.UpdateStatus(ctx, v3)
	if err != nil {
		t.Fatal(err)
	}
	if v4.Status.ObservedGeneration != 2 || v4.Spec.Provider != "snowflake" || v4.Metadata.Generation != 2 {
		t.Fatalf("status update semantics wrong: %+v", v4)
	}
	// Unconditional update (empty version) is allowed.
	v4.Metadata.ResourceVersion = ""
	if _, err := s.Update(ctx, v4); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizersAndGracefulDelete(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore[capSpec, capStatus]()
	obj, _ := s.Create(ctx, newCap("index", "snowflake"))
	obj.Metadata.AddFinalizer("gateway/route-cleanup")
	obj, err := s.Update(ctx, obj)
	if err != nil {
		t.Fatal(err)
	}
	del, err := s.Delete(ctx, "index", "")
	if err != nil {
		t.Fatal(err)
	}
	if !del.Metadata.Deleting() {
		t.Fatal("object with finalizer must enter deleting state")
	}
	if _, err := s.Get(ctx, "index"); err != nil {
		t.Fatal("deleting object must stay visible")
	}
	// Spec is frozen while deleting.
	del.Spec.Provider = "other"
	if _, err := s.Update(ctx, del); !errors.Is(err, ErrInvalid) {
		t.Fatalf("spec change on deleting object must be rejected, got %v", err)
	}
	// Second delete is a no-op.
	again, err := s.Delete(ctx, "index", "")
	if err != nil || again.Metadata.ResourceVersion != del.Metadata.ResourceVersion {
		t.Fatalf("repeat delete must be idempotent: %v", err)
	}
	// Controller finishes its work and removes the finalizer: object disappears.
	del.Spec.Provider = "snowflake"
	del.Metadata.RemoveFinalizer("gateway/route-cleanup")
	if _, err := s.Update(ctx, del); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "index"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("object must be gone after last finalizer removed, got %v", err)
	}
	// Object without finalizers is removed immediately, with version check.
	obj2, _ := s.Create(ctx, newCap("plain", "x"))
	if _, err := s.Delete(ctx, "plain", "999"); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if _, err := s.Delete(ctx, "plain", obj2.Metadata.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx)
	if len(list) != 0 {
		t.Fatalf("list should be empty, got %d", len(list))
	}
}

func TestConditions(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	var conds []Condition
	if !SetCondition(&conds, Condition{Type: "Ready", Status: ConditionFalse, Reason: "Provisioning", ObservedGeneration: 1}, t0) {
		t.Fatal("new condition must report changed")
	}
	// Same status, new generation: transition time unchanged.
	if !SetCondition(&conds, Condition{Type: "Ready", Status: ConditionFalse, Reason: "Provisioning", ObservedGeneration: 2}, t1) {
		t.Fatal("observedGeneration change must report changed")
	}
	if c := FindCondition(conds, "Ready"); !c.LastTransitionTime.Equal(t0) || c.ObservedGeneration != 2 {
		t.Fatalf("transition time must not move without a status change: %+v", c)
	}
	// Identical call: no change.
	if SetCondition(&conds, Condition{Type: "Ready", Status: ConditionFalse, Reason: "Provisioning", ObservedGeneration: 2}, t1) {
		t.Fatal("identical condition must not report changed")
	}
	// Status flips: transition time moves.
	SetCondition(&conds, Condition{Type: "Ready", Status: ConditionTrue, Reason: "Serving", ObservedGeneration: 2}, t1)
	if c := FindCondition(conds, "Ready"); !c.LastTransitionTime.Equal(t1) || !IsConditionTrue(conds, "Ready") {
		t.Fatalf("status flip must move transition time: %+v", c)
	}
	if !RemoveCondition(&conds, "Ready") || len(conds) != 0 || RemoveCondition(&conds, "Ready") {
		t.Fatal("remove semantics wrong")
	}
}

func TestPreconditionsAndETag(t *testing.T) {
	if ETag("42") != `"42"` {
		t.Fatalf("ETag = %s", ETag("42"))
	}
	mk := func(method, ifMatch, ifNoneMatch string) *http.Request {
		r := httptest.NewRequest(method, "/x", nil)
		if ifMatch != "" {
			r.Header.Set("If-Match", ifMatch)
		}
		if ifNoneMatch != "" {
			r.Header.Set("If-None-Match", ifNoneMatch)
		}
		return r
	}
	if err := CheckPreconditions(mk("PUT", `"42"`, ""), "42"); err != nil {
		t.Fatal(err)
	}
	if err := CheckPreconditions(mk("PUT", `"41"`, ""), "42"); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
	if err := CheckPreconditions(mk("PUT", `*`, ""), "42"); err != nil {
		t.Fatal("If-Match: * must match any existing version")
	}
	if err := CheckPreconditions(mk("GET", "", `"42"`), "42"); !errors.Is(err, ErrNotModified) {
		t.Fatalf("want ErrNotModified, got %v", err)
	}
	if err := CheckPreconditions(mk("GET", "", `W/"1", "42"`), "42"); !errors.Is(err, ErrNotModified) {
		t.Fatalf("list of tags must be scanned, got %v", err)
	}
	if err := CheckPreconditions(mk("PUT", "", `"42"`), "42"); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("If-None-Match on a write that matches must fail, got %v", err)
	}
	if err := CheckPreconditions(mk("GET", "", ""), "42"); err != nil {
		t.Fatal("no headers, no precondition")
	}
	rec := httptest.NewRecorder()
	SetETag(rec, "7")
	if rec.Header().Get("ETag") != `"7"` {
		t.Fatal("SetETag did not write header")
	}
}

func TestAPIErrorMapping(t *testing.T) {
	cases := map[error]int{
		ErrNotFound:           http.StatusNotFound,
		ErrAlreadyExists:      http.StatusConflict,
		ErrConflict:           http.StatusConflict,
		ErrPreconditionFailed: http.StatusPreconditionFailed,
		ErrInvalid:            http.StatusBadRequest,
		errors.New("db down"): http.StatusInternalServerError,
	}
	for err, want := range cases {
		if got := APIError(err).Status; got != want {
			t.Errorf("%v: status %d, want %d", err, got, want)
		}
	}
	if APIError(nil) != nil {
		t.Fatal("nil must map to nil")
	}
}
