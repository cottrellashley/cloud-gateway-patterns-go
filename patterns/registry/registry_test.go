package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The namespace interface the host expects for "http.handlers".
type handler interface {
	Serve(string) string
}

type upper struct {
	Prefix   string `json:"prefix"`
	provOK   bool
	cleaned  *int32
	failProv bool
}

func (*upper) ModuleInfo() Info {
	return Info{ID: "http.handlers.upper", New: func() Module { return &upper{} }}
}
func (u *upper) Provision(*Context) error {
	if u.failProv {
		return errors.New("boom")
	}
	u.provOK = true
	return nil
}
func (u *upper) Validate() error {
	if strings.ContainsAny(u.Prefix, " ") {
		return errors.New("prefix must not contain spaces")
	}
	return nil
}
func (u *upper) Cleanup() error {
	if u.cleaned != nil {
		atomic.AddInt32(u.cleaned, 1)
	}
	return nil
}
func (u *upper) Serve(s string) string { return u.Prefix + strings.ToUpper(s) }

type notAHandler struct{}

func (notAHandler) ModuleInfo() Info {
	return Info{ID: "http.handlers.broken", New: func() Module { return notAHandler{} }}
}

func newReg(t *testing.T) *Registry {
	t.Helper()
	r := New("gateway")
	r.Register(&upper{})
	return r
}

func TestIDParts(t *testing.T) {
	cases := []struct{ id, ns, name string }{
		{"http", "", "http"},
		{"http.handlers.upper", "http.handlers", "upper"},
		{"a.b", "a", "b"},
	}
	for _, c := range cases {
		if ID(c.id).Namespace() != c.ns || ID(c.id).Name() != c.name {
			t.Errorf("%q: got ns=%q name=%q", c.id, ID(c.id).Namespace(), ID(c.id).Name())
		}
	}
	for _, bad := range []string{"", "Http", "a..b", "a.", "a b", "a-b"} {
		if ID(bad).Validate() == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestRegisterPanics(t *testing.T) {
	r := newReg(t)
	expectPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		f()
	}
	expectPanic("duplicate", func() { r.Register(&upper{}) })
	expectPanic("reserved", func() {
		r.Register(fakeModule{Info{ID: "gateway", New: func() Module { return fakeModule{} }}})
	})
	expectPanic("nil ctor", func() { r.Register(fakeModule{Info{ID: "x.y"}}) })
	expectPanic("ctor returns nil", func() {
		r.Register(fakeModule{Info{ID: "x.z", New: func() Module { return nil }}})
	})
	expectPanic("invalid id", func() {
		r.Register(fakeModule{Info{ID: "X.Y", New: func() Module { return fakeModule{} }}})
	})
}

type fakeModule struct{ info Info }

func (f fakeModule) ModuleInfo() Info { return f.info }

func TestNamespaceListingAndConforms(t *testing.T) {
	r := newReg(t)
	r.Register(notAHandler{})
	ids := r.Namespace("http.handlers")
	if len(ids) != 2 || ids[0].ID != "http.handlers.broken" || ids[1].ID != "http.handlers.upper" {
		t.Fatalf("namespace listing: %+v", ids)
	}
	if len(r.Namespace("http")) != 0 {
		t.Fatal("Namespace must list direct children only")
	}
	err := Conforms[handler](r, "http.handlers")
	if err == nil || !strings.Contains(err.Error(), "http.handlers.broken") {
		t.Fatalf("Conforms should name the offender, got %v", err)
	}
}

func TestLoadLifecycle(t *testing.T) {
	r := newReg(t)
	c := NewContext(context.Background(), r)
	h, err := LoadAs[handler](c, "http.handlers.upper", json.RawMessage(`{"prefix":">"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Serve("hi"); got != ">HI" {
		t.Fatalf("Serve = %q", got)
	}
	if !h.(*upper).provOK {
		t.Fatal("Provision did not run")
	}
	if got := c.Loaded(); len(got) != 1 || got[0] != "http.handlers.upper" {
		t.Fatalf("Loaded = %v", got)
	}

	// unknown field is rejected
	if _, err := c.Load("http.handlers.upper", json.RawMessage(`{"prefx":">"}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	// unknown module
	if _, err := c.Load("http.handlers.nope", nil); !errors.Is(err, ErrUnknownModule) {
		t.Fatalf("want ErrUnknownModule, got %v", err)
	}
	// validation failure cleans up and reports phase
	if _, err := c.Load("http.handlers.upper", json.RawMessage(`{"prefix":"a b"}`)); err == nil || !strings.Contains(err.Error(), "validate http.handlers.upper") {
		t.Fatalf("validate error: %v", err)
	}
	// Close runs cleanup once; second Close is a no-op; loads after Close fail
	var cleaned int32
	h.(*upper).cleaned = &cleaned
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&cleaned) != 1 {
		t.Fatalf("cleanup ran %d times", cleaned)
	}
	if _, err := c.Load("http.handlers.upper", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	if c.Context().Err() == nil {
		t.Fatal("context not cancelled on Close")
	}
}

func TestLoadAsWrongTypeCleansUp(t *testing.T) {
	r := newReg(t)
	c := NewContext(context.Background(), r)
	_, err := LoadAs[http.Handler](c, "http.handlers.upper", nil)
	if !errors.Is(err, ErrWrongType) {
		t.Fatalf("want ErrWrongType, got %v", err)
	}
	if len(c.Loaded()) != 0 {
		t.Fatal("wrong-type module left registered in context")
	}
}

func TestCleanupOrderIsReverse(t *testing.T) {
	r := New()
	var order []string
	mk := func(name string) Module {
		return &orderMod{name: name, order: &order}
	}
	r.Register(&orderMod{name: "a", ctor: func() Module { return mk("a") }})
	r.Register(&orderMod{name: "b", ctor: func() Module { return mk("b") }})
	c := NewContext(context.Background(), r)
	for _, id := range []ID{"ns.a", "ns.b"} {
		if _, err := c.Load(id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(order) != "[b a]" {
		t.Fatalf("cleanup order = %v", order)
	}
}

type orderMod struct {
	name  string
	ctor  func() Module
	order *[]string
}

func (m *orderMod) ModuleInfo() Info { return Info{ID: ID("ns." + m.name), New: m.ctor} }
func (m *orderMod) Cleanup() error {
	*m.order = append(*m.order, m.name)
	return nil
}

func TestLoadInline(t *testing.T) {
	r := newReg(t)
	c := NewContext(context.Background(), r)
	h, err := LoadInlineAs[handler](c, "http.handlers", "handler", json.RawMessage(`{"handler":"upper","prefix":"!"}`))
	if err != nil {
		t.Fatal(err)
	}
	if h.Serve("x") != "!X" {
		t.Fatal("inline load produced wrong module")
	}
	if _, err := LoadInline(c, "http.handlers", "handler", json.RawMessage(`{"prefix":"!"}`)); err == nil {
		t.Fatal("missing inline key accepted")
	}
}

// Hot reload: the new generation is loaded completely before the old one is
// closed, so there is never a moment without a working handler.
func TestMakeBeforeBreak(t *testing.T) {
	r := newReg(t)
	var cleaned int32
	gen1 := NewContext(context.Background(), r)
	h1, err := LoadAs[handler](gen1, "http.handlers.upper", json.RawMessage(`{"prefix":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	h1.(*upper).cleaned = &cleaned
	var live atomic.Pointer[handler]
	live.Store(&h1)

	gen2 := NewContext(context.Background(), r)
	h2, err := LoadAs[handler](gen2, "http.handlers.upper", json.RawMessage(`{"prefix":"2"}`))
	if err != nil {
		t.Fatal(err)
	}
	live.Store(&h2) // switch
	if err := gen1.Close(); err != nil {
		t.Fatal(err)
	}
	if (*live.Load()).Serve("x") != "2X" || atomic.LoadInt32(&cleaned) != 1 {
		t.Fatal("old generation not cleaned after switch, or new one not live")
	}
}
