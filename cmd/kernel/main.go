// Command kernel is the reference wiring of the control-plane principles
// (docs/00-principles 01-04, 06, 11, 12) into one small process:
//
//   - Capabilities are resources (patterns/resource): PUT/GET/LIST/DELETE with
//     ETag / If-Match, a separate /status write path, generations, finalizers.
//   - Providers are registered modules in the "provider" namespace
//     (patterns/registry), loaded as one generation at startup; the kernel
//     never switches on a provider kind.
//   - A controller (patterns/reconcile) converges capabilities into an
//     immutable routing snapshot (patterns/config) and reports Ready /
//     Accepted conditions with observedGeneration.
//   - The data path reads the snapshot with one atomic load and never
//     touches the store: POST /v1/capabilities/{name}/execute.
//   - Results are self-describing: every result carries its schema.
//
// Run:  go run ./cmd/kernel -addr :8090
//
//	curl -X PUT localhost:8090/v1/capabilities/rates -d '{"spec":{"provider":"provider.static_rows","summary":"demo curve"}}'
//	curl localhost:8090/v1/capabilities/rates           # status.conditions → Ready=True once reconciled
//	curl -X POST localhost:8090/v1/capabilities/rates/execute -d '{"tenor":"10Y"}'
//	curl localhost:8090/v1/catalog                      # level-1 discovery
//	curl localhost:8090/v1/providers                    # the provider namespace
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/config"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/httpserver"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lifecycle"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/middleware"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/reconcile"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/registry"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/resource"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/telemetry"
)

// ---- the resource (principle 01) --------------------------------------------

type CapabilitySpec struct {
	Provider string         `json:"provider"`          // registry ID in the "provider" namespace
	Summary  string         `json:"summary,omitempty"` // level-1 discovery text
	Config   map[string]any `json:"config,omitempty"`  // passed to the provider on execute
}

type CapabilityStatus struct {
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`
	Conditions         []resource.Condition `json:"conditions,omitempty"`
}

type Capability = resource.Object[CapabilitySpec, CapabilityStatus]

const finalizer = "kernel/route"

// ---- the extension point (principle 04) -------------------------------------

// Provider is the namespace interface for "provider.*". Every module in that
// namespace must implement it (checked at startup by registry.Conforms).
type Provider interface {
	Execute(ctx context.Context, cfg map[string]any, params map[string]any) (Result, error)
}

// Result is self-describing: schema travels with rows (principle 06).
type Result struct {
	Schema []Field          `json:"schema"`
	Rows   []map[string]any `json:"rows"`
}

type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// echoProvider returns its parameters as one row.
type echoProvider struct{}

func (echoProvider) ModuleInfo() registry.Info {
	return registry.Info{ID: "provider.echo", New: func() registry.Module { return echoProvider{} }}
}

func (echoProvider) Execute(_ context.Context, _ map[string]any, params map[string]any) (Result, error) {
	var schema []Field
	for k := range params {
		schema = append(schema, Field{Name: k, Type: "string"})
	}
	return Result{Schema: schema, Rows: []map[string]any{params}}, nil
}

// staticRowsProvider serves a fixed table, optionally filtered by "tenor".
type staticRowsProvider struct {
	Rows []map[string]any `json:"rows"`
}

func (*staticRowsProvider) ModuleInfo() registry.Info {
	return registry.Info{ID: "provider.static_rows", New: func() registry.Module {
		return &staticRowsProvider{Rows: []map[string]any{
			{"tenor": "2Y", "yield": 3.91}, {"tenor": "10Y", "yield": 4.27}, {"tenor": "30Y", "yield": 4.62},
		}}
	}}
}

func (p *staticRowsProvider) Validate() error {
	if len(p.Rows) == 0 {
		return errors.New("static_rows: no rows configured")
	}
	return nil
}

func (p *staticRowsProvider) Execute(_ context.Context, _ map[string]any, params map[string]any) (Result, error) {
	out := Result{Schema: []Field{{Name: "tenor", Type: "string"}, {Name: "yield", Type: "float64"}}}
	want, _ := params["tenor"].(string)
	for _, r := range p.Rows {
		if want == "" || r["tenor"] == want {
			out.Rows = append(out.Rows, r)
		}
	}
	return out, nil
}

func init() {
	registry.Register(echoProvider{})
	registry.Register(&staticRowsProvider{})
}

// ---- the snapshot the data plane reads (principle 03) -----------------------

type Route struct {
	Provider   registry.ID `json:"provider"`
	Generation int64       `json:"generation"`
	Config     map[string]any
}

type Snapshot struct {
	Routes map[string]Route `json:"routes"`
}

func (s Snapshot) consistent(loaded map[registry.ID]Provider) error {
	for name, r := range s.Routes {
		if _, ok := loaded[r.Provider]; !ok {
			return fmt.Errorf("route %q references unloaded provider %q", name, r.Provider)
		}
	}
	return nil
}

// ---- main -------------------------------------------------------------------

func main() {
	addr := flag.String("addr", ":8090", "listen address")
	flag.Parse()
	log := telemetry.NewLogger(os.Stdout, slog.LevelInfo, "kernel", "dev", os.Getenv("ENV"))
	ctx := telemetry.WithLogger(context.Background(), log)

	// Providers: one generation, loaded from configuration, conformance-checked.
	if err := registry.Conforms[Provider](registry.Default, "provider"); err != nil {
		log.Error("provider namespace is not conformant", "err", err)
		os.Exit(1)
	}
	gen := registry.NewContext(ctx, registry.Default)
	defer gen.Close()
	loaded := map[registry.ID]Provider{}
	for _, info := range registry.Default.Namespace("provider") {
		p, err := registry.LoadAs[Provider](gen, info.ID, nil) // nil = defaults; real systems pass JSON config
		if err != nil {
			log.Error("provider failed to load", "id", info.ID, "err", err)
			os.Exit(1)
		}
		loaded[info.ID] = p
	}

	// Store with admission-style validation (mutate-then-validate would be a chain; one validator here).
	store := resource.NewMemoryStore[CapabilitySpec, CapabilityStatus]()
	store.Validate = func(c *Capability) error {
		if c.Spec.Provider == "" {
			return errors.New("spec.provider is required")
		}
		if registry.ID(c.Spec.Provider).Namespace() != "provider" {
			return fmt.Errorf("spec.provider %q must be in the provider namespace", c.Spec.Provider)
		}
		return nil
	}

	// Snapshot: validated, deduplicated, atomically swapped; one load per request on the data path.
	snap, err := config.New(Snapshot{Routes: map[string]Route{}}, func(s Snapshot) error { return s.consistent(loaded) })
	if err != nil {
		log.Error("initial snapshot invalid", "err", err)
		os.Exit(1)
	}

	// Controller: converges capabilities → snapshot and reports status (principle 02).
	queue := reconcile.New[string](reconcile.Options{})
	ctl := &reconcile.Controller[string]{
		Queue: queue, Workers: 2, Log: log,
		Sync: func(ctx context.Context, name string) error {
			cap, err := store.Get(ctx, name)
			if errors.Is(err, resource.ErrNotFound) {
				return updateRoutes(snap, func(r map[string]Route) { delete(r, name) }) // absent ⇒ no route (level-triggered)
			}
			if err != nil {
				return err
			}
			if cap.Metadata.Deleting() {
				if err := updateRoutes(snap, func(r map[string]Route) { delete(r, name) }); err != nil {
					return err
				}
				if cap.Metadata.RemoveFinalizer(finalizer) {
					_, err = store.Update(ctx, cap) // removing the last finalizer removes the object
				}
				return err
			}
			if cap.Metadata.AddFinalizer(finalizer) {
				if cap, err = store.Update(ctx, cap); err != nil {
					return err
				}
			}
			now := time.Now()
			status := cap.Status
			status.ObservedGeneration = cap.Metadata.Generation
			_, known := loaded[registry.ID(cap.Spec.Provider)]
			accepted := resource.Condition{Type: "Accepted", Status: resource.ConditionTrue, Reason: "ProviderLoaded", ObservedGeneration: cap.Metadata.Generation}
			if !known {
				accepted.Status, accepted.Reason = resource.ConditionFalse, "UnknownProvider"
				accepted.Message = fmt.Sprintf("provider %q is not loaded in this generation", cap.Spec.Provider)
			}
			changed := resource.SetCondition(&status.Conditions, accepted, now)
			ready := resource.Condition{Type: "Ready", Status: resource.ConditionFalse, Reason: "NotRouted", ObservedGeneration: cap.Metadata.Generation}
			if known {
				if err := updateRoutes(snap, func(r map[string]Route) {
					r[name] = Route{Provider: registry.ID(cap.Spec.Provider), Generation: cap.Metadata.Generation, Config: cap.Spec.Config}
				}); err != nil {
					return err
				}
				ready.Status, ready.Reason = resource.ConditionTrue, "Routed"
			} else {
				_ = updateRoutes(snap, func(r map[string]Route) { delete(r, name) })
			}
			changed = resource.SetCondition(&status.Conditions, ready, now) || changed
			if !changed && cap.Status.ObservedGeneration == status.ObservedGeneration {
				return nil // already converged: write nothing
			}
			cap.Status = status
			_, err = store.UpdateStatus(ctx, cap)
			return err
		},
	}

	// ---- HTTP surface ----
	mux := http.NewServeMux()
	ops := "/v1/capabilities/"

	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		items, _ := store.List(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /v1/capabilities/{name}", func(w http.ResponseWriter, r *http.Request) {
		cap, err := store.Get(r.Context(), r.PathValue("name"))
		if err != nil {
			apierror.Write(w, resource.APIError(err))
			return
		}
		if err := resource.CheckPreconditions(r, cap.Metadata.ResourceVersion); errors.Is(err, resource.ErrNotModified) {
			resource.SetETag(w, cap.Metadata.ResourceVersion)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		resource.SetETag(w, cap.Metadata.ResourceVersion)
		writeJSON(w, http.StatusOK, cap)
	})
	mux.HandleFunc("PUT /v1/capabilities/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var in Capability
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			apierror.Write(w, apierror.New(http.StatusBadRequest, apierror.CodeMalformedRequest, "body must be a Capability object"))
			return
		}
		in.APIVersion, in.Kind, in.Metadata.Name = "kernel/v1", "Capability", name // name comes from the URL
		cur, err := store.Get(r.Context(), name)
		var out *Capability
		switch {
		case errors.Is(err, resource.ErrNotFound):
			if im := r.Header.Get("If-Match"); im != "" && im != "*" {
				apierror.Write(w, resource.APIError(resource.ErrPreconditionFailed))
				return
			}
			out, err = store.Create(r.Context(), &in)
			if err == nil {
				w.Header().Set("Location", ops+name)
				defer queue.Add(name)
			}
			if err == nil {
				resource.SetETag(w, out.Metadata.ResourceVersion)
				writeJSON(w, http.StatusCreated, out)
				return
			}
		case err == nil:
			if perr := resource.CheckPreconditions(r, cur.Metadata.ResourceVersion); perr != nil {
				apierror.Write(w, resource.APIError(perr))
				return
			}
			if in.Metadata.ResourceVersion == "" {
				in.Metadata.ResourceVersion = strings.Trim(r.Header.Get("If-Match"), `"`) // "" ⇒ unconditional
			}
			in.Metadata.Finalizers = cur.Metadata.Finalizers // clients do not manage the kernel's finalizer
			out, err = store.Update(r.Context(), &in)
			if err == nil {
				defer queue.Add(name)
			}
		}
		if err != nil {
			apierror.Write(w, resource.APIError(err))
			return
		}
		resource.SetETag(w, out.Metadata.ResourceVersion)
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("DELETE /v1/capabilities/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		out, err := store.Delete(r.Context(), name, strings.Trim(r.Header.Get("If-Match"), `"`))
		if err != nil {
			apierror.Write(w, resource.APIError(err))
			return
		}
		queue.Add(name)
		if out.Metadata.Deleting() {
			writeJSON(w, http.StatusAccepted, out) // graceful: finalizer still held; controller finishes it
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Custom action (principle 01): the data path. One atomic snapshot load, no store access.
	mux.HandleFunc("POST /v1/capabilities/{name}/execute", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		route, ok := snap.Load().Routes[name]
		if !ok {
			apierror.Write(w, apierror.New(http.StatusNotFound, apierror.CodeNotFound, "capability is not routable (missing or not Ready)"))
			return
		}
		params := map[string]any{}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
				apierror.Write(w, apierror.New(http.StatusBadRequest, apierror.CodeMalformedRequest, "parameters must be a JSON object"))
				return
			}
		}
		res, err := loaded[route.Provider].Execute(r.Context(), route.Config, params)
		if err != nil {
			apierror.Write(w, err)
			return
		}
		w.Header().Set("X-Capability-Generation", fmt.Sprint(route.Generation))
		writeJSON(w, http.StatusOK, res)
	})
	// Discovery (principle 06): level 1 (catalog) and the provider namespace.
	mux.HandleFunc("GET /v1/catalog", func(w http.ResponseWriter, r *http.Request) {
		items, _ := store.List(r.Context())
		type entry struct {
			Name, Kind, Summary, Provider string
			Ready                         bool
			Describe                      string
		}
		out := make([]entry, 0, len(items))
		for _, c := range items {
			out = append(out, entry{c.Metadata.Name, c.Kind, c.Spec.Summary, c.Spec.Provider,
				resource.IsConditionTrue(c.Status.Conditions, "Ready"), ops + c.Metadata.Name})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out, "snapshot": snap.CurrentVersion()})
	})
	mux.HandleFunc("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) {
		var ids []registry.ID
		for _, info := range registry.Default.Namespace("provider") {
			ids = append(ids, info.ID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"namespace": "provider", "interface": "Provider", "modules": ids})
	})

	handler := middleware.Chain(mux,
		middleware.Recover(log),
		middleware.RequestID("X-Request-Id"),
		telemetry.Propagate(false),
		telemetry.AccessLog(log, telemetry.AccessLogOptions{}),
		middleware.MaxBytes(1<<20),
		middleware.Deadline("X-Request-Deadline", 10*time.Second, time.Minute),
	)

	cfg := httpserver.Defaults()
	cfg.Addr = *addr
	cfg.ShutdownDelay = 0
	var ready lifecycle.Readiness
	srv := httpserver.New(ctx, cfg, handler, log)

	var g lifecycle.Group
	g.Add(lifecycle.SignalActor(ctx))
	g.Add(lifecycle.ContextActor(ctx, func(ctx context.Context) error { return httpserver.Run(ctx, srv, cfg, &ready, log) }))
	g.Add(lifecycle.ContextActor(ctx, ctl.Run))
	g.Add(lifecycle.ContextActor(ctx, func(ctx context.Context) error {
		return reconcile.Resync(ctx, queue, time.Minute, func(ctx context.Context) ([]string, error) {
			items, err := store.List(ctx)
			names := make([]string, 0, len(items))
			for _, c := range items {
				names = append(names, c.Metadata.Name)
			}
			return names, err
		})
	}))
	if err := g.Run(); err != nil && !lifecycle.IsSignal(err) && !errors.Is(err, context.Canceled) {
		log.Error("kernel stopped with error", "err", err)
		os.Exit(1)
	}
	log.Info("kernel stopped")
}

// updateRoutes applies a mutation to a copy of the current routes and swaps
// the snapshot (validated, deduplicated by config.Hot).
func updateRoutes(snap *config.Hot[Snapshot], mutate func(map[string]Route)) error {
	cur := snap.Load()
	next := Snapshot{Routes: make(map[string]Route, len(cur.Routes)+1)}
	for k, v := range cur.Routes {
		next.Routes[k] = v
	}
	mutate(next.Routes)
	_, err := snap.Swap(next)
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
