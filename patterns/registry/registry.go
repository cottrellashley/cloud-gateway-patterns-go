// Package registry is a namespaced module registry with a provision /
// validate / cleanup lifecycle, in the shape of Caddy's module system
// (caddy.Module, caddy.ModuleInfo, caddy.RegisterModule, caddy.Provisioner,
// caddy.Validator, caddy.CleanerUpper) and Kubernetes admission plugins
// (admission.Plugins.Register / NewFromPlugins).
//
// It is the mechanism behind the principle "extension points, not forks"
// (docs/00-principles/04-extension-points-not-forks.md): a small kernel
// defines namespaces, each namespace is an interface, and every
// implementation registers itself under "<namespace>.<name>". Configuration
// names modules; the kernel instantiates them through the registry, runs
// their lifecycle, and type-asserts them to the namespace interface.
//
// A Context owns one generation of loaded modules. Hot reload is: build a
// new Context, load everything, switch traffic to it, then Close the old one
// (make before break, exactly as Caddy runs two configs for a brief moment).
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// ID is a module identifier of the form "<namespace>.<name>". The last
// dot-separated label is the name; the labels before it form the namespace.
// An ID with no dot has the empty namespace (a top-level module).
type ID string

// Namespace returns the part of the ID before the last dot ("" if none).
func (id ID) Namespace() string {
	s := string(id)
	i := strings.LastIndexByte(s, '.')
	if i < 0 {
		return ""
	}
	return s[:i]
}

// Name returns the last label of the ID.
func (id ID) Name() string {
	s := string(id)
	i := strings.LastIndexByte(s, '.')
	return s[i+1:]
}

// Validate reports whether the ID is non-empty, lowercase, and made only of
// letters, digits and underscores separated by single dots.
func (id ID) Validate() error {
	if id == "" {
		return errors.New("registry: module ID is empty")
	}
	for _, label := range strings.Split(string(id), ".") {
		if label == "" {
			return fmt.Errorf("registry: module ID %q has an empty label", id)
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			default:
				return fmt.Errorf("registry: module ID %q: label %q must be lowercase letters, digits or underscores", id, label)
			}
		}
	}
	return nil
}

// Module is implemented by every registrable type. ModuleInfo must be pure:
// no side effects, no initialisation (that belongs in Provision).
type Module interface {
	ModuleInfo() Info
}

// Info describes a registered module: its ID and a constructor that returns
// a new, empty instance. Configuration is decoded into that instance before
// Provision is called.
type Info struct {
	ID  ID
	New func() Module
}

// Provisioner is implemented by modules that need setup after their
// configuration has been decoded: parse strings into typed values, load
// guest modules, open connections, start goroutines. Provision runs once per
// Context.
type Provisioner interface {
	Provision(ctx *Context) error
}

// Validator is implemented by modules that can check their configuration.
// Validate runs after Provision and must be fast; returning an error aborts
// the whole load.
type Validator interface {
	Validate() error
}

// Cleaner is implemented by modules that hold resources (files, goroutines,
// connections). Cleanup runs when the Context closes, in reverse load order,
// and also when Provision or Validate fail part-way, so it must tolerate
// partial provisioning.
type Cleaner interface {
	Cleanup() error
}

// Registry maps module IDs to constructors. Registration happens at init
// time (import for side effect) or at startup; lookups are concurrent-safe.
type Registry struct {
	mu       sync.RWMutex
	modules  map[ID]Info
	reserved map[ID]struct{}
}

// New returns an empty registry. IDs listed in reserved cannot be registered
// (Caddy reserves "caddy" and "admin"; a gateway might reserve "gateway").
func New(reserved ...ID) *Registry {
	r := &Registry{modules: map[ID]Info{}, reserved: map[ID]struct{}{}}
	for _, id := range reserved {
		r.reserved[id] = struct{}{}
	}
	return r
}

// Default is the process-wide registry used by Register and Lookup.
var Default = New()

// Register adds a module to the registry. It panics on an invalid, reserved
// or duplicate ID, on a nil constructor, or on a constructor returning nil:
// these are programming errors and must fail at startup, never at request
// time.
func (r *Registry) Register(m Module) {
	info := m.ModuleInfo()
	if err := info.ID.Validate(); err != nil {
		panic(err)
	}
	if _, ok := r.reserved[info.ID]; ok {
		panic(fmt.Sprintf("registry: module ID %q is reserved", info.ID))
	}
	if info.New == nil {
		panic(fmt.Sprintf("registry: module %q has no constructor", info.ID))
	}
	if info.New() == nil {
		panic(fmt.Sprintf("registry: constructor for %q returned nil", info.ID))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.modules[info.ID]; dup {
		panic(fmt.Sprintf("registry: module already registered: %s", info.ID))
	}
	r.modules[info.ID] = info
}

// Register adds m to the Default registry.
func Register(m Module) { Default.Register(m) }

// Lookup returns the Info for id.
func (r *Registry) Lookup(id ID) (Info, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.modules[id]
	return info, ok
}

// Lookup returns the Info for id from the Default registry.
func Lookup(id ID) (Info, bool) { return Default.Lookup(id) }

// Namespace returns every module registered directly in ns, sorted by ID.
// This is what a discovery endpoint or `list-modules` command serves.
func (r *Registry) Namespace(ns string) []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Info
	for id, info := range r.modules {
		if id.Namespace() == ns {
			out = append(out, info)
		}
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out
}

// IDs returns every registered ID, sorted.
func (r *Registry) IDs() []ID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]ID, 0, len(r.modules))
	for id := range r.modules {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Conforms checks that every module registered in ns can be used as T, the
// interface the host expects for that namespace ("if your module is in
// <namespace>, it must compile as <type>"). Call it from a startup test.
func Conforms[T any](r *Registry, ns string) error {
	var bad []string
	for _, info := range r.Namespace(ns) {
		if _, ok := info.New().(T); !ok {
			bad = append(bad, string(info.ID))
		}
	}
	if len(bad) > 0 {
		var zero T
		return fmt.Errorf("registry: modules in namespace %q do not implement %T: %s", ns, &zero, strings.Join(bad, ", "))
	}
	return nil
}

// Errors returned by Context.Load and LoadAs.
var (
	ErrUnknownModule = errors.New("registry: unknown module")
	ErrWrongType     = errors.New("registry: module does not implement the namespace interface")
	ErrClosed        = errors.New("registry: context is closed")
)

// Context owns one generation of loaded modules. Everything loaded through
// it is cleaned up, in reverse order, by Close. It carries a context.Context
// that is cancelled on Close so goroutines started in Provision stop.
type Context struct {
	ctx      context.Context
	cancel   context.CancelFunc
	registry *Registry

	mu       sync.Mutex
	closed   bool
	cleanups []func() error
	loaded   []ID
}

// NewContext returns a Context whose loads resolve against reg (Default if
// nil) and whose context.Context is derived from parent.
func NewContext(parent context.Context, reg *Registry) *Context {
	if reg == nil {
		reg = Default
	}
	ctx, cancel := context.WithCancel(parent)
	return &Context{ctx: ctx, cancel: cancel, registry: reg}
}

// Context returns the cancellable context for this generation.
func (c *Context) Context() context.Context { return c.ctx }

// Registry returns the registry loads resolve against.
func (c *Context) Registry() *Registry { return c.registry }

// Loaded returns the IDs loaded so far, in load order.
func (c *Context) Loaded() []ID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.loaded)
}

// Load instantiates module id, decodes raw into it (unknown fields are an
// error; empty raw means defaults), runs Provision then Validate, and
// records Cleanup for Close. On any failure the module's Cleanup runs and
// the error is returned; nothing is left half-loaded.
func (c *Context) Load(id ID, raw json.RawMessage) (Module, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.mu.Unlock()

	info, ok := c.registry.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownModule, id)
	}
	mod := info.New()
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(mod); err != nil {
			return nil, fmt.Errorf("registry: decoding %s: %w", id, err)
		}
	}
	if p, ok := mod.(Provisioner); ok {
		if err := p.Provision(c); err != nil {
			return nil, c.failLoad(id, mod, "provision", err)
		}
	}
	if v, ok := mod.(Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, c.failLoad(id, mod, "validate", err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		// Closed while we were provisioning: undo immediately.
		if cu, ok := mod.(Cleaner); ok {
			_ = cu.Cleanup()
		}
		return nil, ErrClosed
	}
	if cu, ok := mod.(Cleaner); ok {
		c.cleanups = append(c.cleanups, cu.Cleanup)
	}
	c.loaded = append(c.loaded, id)
	return mod, nil
}

func (c *Context) failLoad(id ID, mod Module, phase string, err error) error {
	err = fmt.Errorf("registry: %s %s: %w", phase, id, err)
	if cu, ok := mod.(Cleaner); ok {
		if cerr := cu.Cleanup(); cerr != nil {
			err = fmt.Errorf("%w; additionally, cleanup: %v", err, cerr)
		}
	}
	return err
}

// LoadAs loads id and asserts it to T, the interface for its namespace. A
// module that loads but does not implement T is cleaned up and ErrWrongType
// is returned.
func LoadAs[T any](c *Context, id ID, raw json.RawMessage) (T, error) {
	var zero T
	mod, err := c.Load(id, raw)
	if err != nil {
		return zero, err
	}
	t, ok := mod.(T)
	if !ok {
		c.unload(mod)
		return zero, fmt.Errorf("%w: %s is not %T", ErrWrongType, id, &zero)
	}
	return t, nil
}

// LoadInline loads a module whose name is carried inline in its own JSON
// object under key (Caddy's `"handler": "reverse_proxy"` convention). The
// key is removed before decoding so module types need not declare it.
func LoadInline(c *Context, ns, key string, raw json.RawMessage) (Module, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("registry: inline module in %q: %w", ns, err)
	}
	var name string
	if err := json.Unmarshal(fields[key], &name); err != nil || name == "" {
		return nil, fmt.Errorf("registry: inline module in %q: missing or invalid %q", ns, key)
	}
	delete(fields, key)
	rest, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	id := ID(name)
	if ns != "" {
		id = ID(ns + "." + name)
	}
	return c.Load(id, rest)
}

// LoadInlineAs is LoadInline followed by a type assertion to T.
func LoadInlineAs[T any](c *Context, ns, key string, raw json.RawMessage) (T, error) {
	var zero T
	mod, err := LoadInline(c, ns, key, raw)
	if err != nil {
		return zero, err
	}
	t, ok := mod.(T)
	if !ok {
		c.unload(mod)
		return zero, fmt.Errorf("%w: %s is not %T", ErrWrongType, mod.ModuleInfo().ID, &zero)
	}
	return t, nil
}

// unload removes the most recent registration of mod's cleanup and runs it.
func (c *Context) unload(mod Module) {
	cu, ok := mod.(Cleaner)
	if !ok {
		return
	}
	c.mu.Lock()
	if n := len(c.cleanups); n > 0 {
		c.cleanups = c.cleanups[:n-1]
	}
	if n := len(c.loaded); n > 0 {
		c.loaded = c.loaded[:n-1]
	}
	c.mu.Unlock()
	_ = cu.Cleanup()
}

// Close cancels the context and runs every recorded Cleanup in reverse load
// order, collecting errors. It is idempotent.
func (c *Context) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cleanups := c.cleanups
	c.cleanups = nil
	c.mu.Unlock()

	c.cancel()
	var errs []error
	for i := len(cleanups) - 1; i >= 0; i-- {
		if err := cleanups[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
