# 02. Declarative intent and reconciliation

**Principle:** clients write *what they want* (the `spec`); the platform runs controllers that observe actual state, compare it with the desired state and act to converge, recording what they observed in `status`. Controllers are level-triggered (they act on the current state, not on the event that woke them), idempotent (running twice is harmless), work on one key at a time, and retry with per-key exponential backoff. Nothing is lost if an event is missed because every object is periodically re-examined.

**Why it matters at scale:** imperative APIs ("create the route, then register the policy, then warm the cache") put the orchestration in every client and break the moment one step fails half-way. Declarative APIs put the orchestration in one controller, make partial failure a normal state (`status` says `Ready=False, reason=UpstreamUnreachable`), and make the system self-healing after restarts, crashes and missed notifications. This is how Kubernetes keeps millions of objects converged with controllers that are each a hundred lines of real logic.

## Evidence

**Spec and status, level-based** ([api-conventions.md §Spec and Status](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api-conventions.md#L280)):

> When a new version of an object is POSTed or PUT, the `spec` is updated and available immediately. Over time the system will work to bring the `status` into line with the `spec`. The system will drive toward the most recent `spec` regardless of previous versions of that stanza. For example, if a value is changed from 2 to 5 in one PUT and then back down to 3 in another PUT the system is not required to 'touch base' at 5 before changing the `status` to 3. In other words, the system's behavior is *level-based* rather than *edge-based*. This enables robust behavior in the presence of missed intermediate state changes.
>
> The PUT and POST verbs on objects MUST ignore the `status` values … A `/status` subresource MUST be provided to enable system components to update statuses of resources they manage.

Fields in spec "should have declarative rather than imperative names and semantics -- they represent the desired state, not actions intended to yield the desired state."

**Conditions and observedGeneration** ([§Typical status properties](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api-conventions.md#L352), [meta/v1 `Condition`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go#L1653)): `type`, `status` (True/False/Unknown), `observedGeneration` ("if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date"), `lastTransitionTime` (changes only when status changes), `reason` (CamelCase, programmatic), `message`.

**Controller guidelines** ([controllers.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-api-machinery/controllers.md#L17)), quoted because every line is a rule:

> 1. Operate on one item at a time. If you use a `workqueue.Interface`, you'll be able to queue references to particular objects and later pop them in multiple "worker" goroutines with a guarantee that no two goroutines will work on the same item at the same time.
> 2. Random ordering between resources … Distinct watches are updated independently.
> 3. Level driven, not edge driven … If an API object appears with a marker value of `true`, you can't count on having seen it turn from `false` to `true`, only that you now observe it being `true`.
> 5. Never mutate original objects! Caches are shared across controllers.
> 6. Wait for your secondary caches.
> 7. There are other actors in the system. Just because you haven't changed an object doesn't mean that somebody else hasn't.
> 9. Percolate errors to the top level for consistent re-queuing … Your main controller func should return an error when requeuing is necessary.
> 10. Informers can periodically "resync" … If you ever skip requeuing your item on failures, you could fail, not requeue, and then never retry that item again.
> 11. If the primary resource your controller is reconciling supports ObservedGeneration in its status, make sure you correctly set it to metadata.Generation whenever the values between the two fields mismatches.
> 12. Consider using owner references … children resources are going to be garbage-collected once a resource managed by your controller is deleted.

The loop itself, from [sample-controller `processNextWorkItem`](https://github.com/kubernetes/sample-controller/blob/940ca24/controller.go#L202):

```go
objRef, shutdown := c.workqueue.Get()
if shutdown { return false }
defer c.workqueue.Done(objRef)
err := c.syncHandler(ctx, objRef)
if err == nil {
    c.workqueue.Forget(objRef)
    return true
}
utilruntime.HandleErrorWithContext(ctx, err, "Error syncing; requeuing for later retry", "objectReference", objRef)
c.workqueue.AddRateLimited(objRef)
```

The queue ([workqueue/queue.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/client-go/util/workqueue/queue.go#L199)) keeps a `dirty` set ("all of the items that need to be processed") and a `processing` set ("may be simultaneously in the dirty set. When we finish processing something and remove it from this set, we'll check if it's in the dirty set, and if so, add it to the queue"). The default limiter ([default_rate_limiters.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/client-go/util/workqueue/default_rate_limiters.go#L50)) is the max of a per-item exponential 5 ms → 1000 s and an overall bucket of 10 qps / burst 100.

**Object lifecycle and finalizers** ([object-lifecycle.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/object-lifecycle.md#L28)): objects are `DOES_NOT_EXIST`, `ACTIVE` or `DELETING`; "the transition from ACTIVE to DELETING appears as a normal update operation which sets the metadata.deletionTimestamp field"; controllers "must consider what semantics to apply to objects in the DELETING state".

**ARM** expresses the same thing as `provisioningState` ([Addendum.md](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/Addendum.md#L136)): PUT returns 201 with a non-terminal `provisioningState`, clients "poll at the resourceUri until the provisioningState enters a terminal state" (`Succeeded`, `Failed`, `Canceled`), and the template engine "would not create dependent resources until the resource creation is completed". The desired state is the PUT body; the observed state is `provisioningState`.

**Terraform** separates desired, planned and actual explicitly: [`ResourceServer`](https://github.com/hashicorp/terraform-plugin-go/blob/5376f1b/tfprotov6/resource.go#L21) has `ReadResource` (observe), `PlanResourceChange` (compute the diff, including `RequiresReplace`), `ApplyResourceChange` (converge), `ImportResourceState` and `UpgradeResourceState`. A provider cannot "just do it"; it must be able to say what it would do.

## The rules

1. **Spec is desired, status is observed, written by different actors through different paths.** `PUT`/`PATCH` ignore `status`; controllers write `/status` and never `spec`.
2. **Every controller is a loop over keys, not a handler for events.** Events only enqueue a key. The sync function reads the current spec and the current world and converges.
3. **Idempotent sync.** Running sync on an already-converged object changes nothing and writes nothing (compare before writing status; `SetCondition` reports `changed`).
4. **Record what you saw.** `status.observedGeneration = metadata.generation` on every successful sync; conditions carry `observedGeneration` and `reason`.
5. **Fail by returning an error; retry by backoff.** Never swallow an error and never retry inline. Transient waits use `Requeue{After}` without counting as failures.
6. **Resync periodically.** A full re-enqueue every N minutes is the floor under missed events and bugs.
7. **Handle deletion as a state.** Finalizers hold the object until external cleanup (routes, tickets, caches, downstream registrations) is done; the controller must remove its own finalizer and tolerate seeing a deleting object repeatedly.
8. **Children are owned.** Derived objects (routes derived from a capability, a schema version derived from a manifest) reference their owner and are garbage-collected with it.
9. **Plan before apply where the change is risky.** Expose a dry-run (`?dryRun=All` in Kubernetes, `PlanResourceChange` in Terraform) that validates and reports the diff without acting.

## In this repository

| Mechanism | Where |
| --- | --- |
| Work queue (dirty/processing sets, re-queue after Done, per-key exponential retry, `AddAfter`, shutdown) | `patterns/reconcile.Queue` |
| Controller loop (workers, `Forget` on success, `Requeue` without failure count, panic → failed sync, resync) | `patterns/reconcile.Controller`, `reconcile.Resync` |
| Spec/status separation, generation vs resourceVersion, conditions, finalizers, deleting state | `patterns/resource` |
| Operations as the reconciled record of a long task | `patterns/lro` (`Operation` is a resource whose status converges to a terminal state) |
| Desired configuration as an immutable snapshot that listeners converge onto | `patterns/config.Hot`, `docs/03-traffic/dynamic-configuration.md` |

```go
q := reconcile.New[string](reconcile.Options{})
ctl := &reconcile.Controller[string]{
    Queue: q, Workers: 4, Log: log,
    Sync: func(ctx context.Context, name string) error {
        cap, err := store.Get(ctx, name)
        if errors.Is(err, resource.ErrNotFound) { routes.Remove(name); return nil } // level-triggered: absent means remove
        if cap.Metadata.Deleting() {
            routes.Remove(name); tickets.RevokeAll(ctx, name)
            if cap.Metadata.RemoveFinalizer("gateway/route") { _, err = store.Update(ctx, cap) }
            return err
        }
        if cap.Metadata.AddFinalizer("gateway/route") {
            if cap, err = store.Update(ctx, cap); err != nil { return err }
        }
        compat, err := schemas.Check(ctx, cap.Spec)           // admission-style check against the previous version
        if err != nil { return reconcile.Requeue{After: 30 * time.Second} } // registry unreachable: wait, not a failure
        status := cap.Status
        status.ObservedGeneration = cap.Metadata.Generation
        changed := resource.SetCondition(&status.Conditions, resource.Condition{
            Type: "Compatible", Status: boolStatus(compat.OK), Reason: compat.Reason, Message: compat.Detail,
            ObservedGeneration: cap.Metadata.Generation,
        }, time.Now())
        if compat.OK { routes.Upsert(name, cap.Spec) }
        changed = resource.SetCondition(&status.Conditions, readyCondition(cap, compat), time.Now()) || changed
        if !changed { return nil }                              // idempotent: nothing to write
        cap.Status = status
        _, err = store.UpdateStatus(ctx, cap)
        return err
    },
}
go reconcile.Resync(ctx, q, 10*time.Minute, func(ctx context.Context) ([]string, error) {
    objs, err := store.List(ctx)                                    // level-triggered floor: every object, every 10 minutes
    names := make([]string, 0, len(objs))
    for _, o := range objs { names = append(names, o.Metadata.Name) }
    return names, err
})
return ctl.Run(ctx) // helpers boolStatus, readyCondition, routes, tickets, schemas are the service's own
```

## Applying it to a capability gateway

* **Registration is declarative.** A provider does not call `POST /register` and hope. It `PUT`s a `Capability` (or the gateway syncs manifests from the provider's `/.well-known/capabilities`); the capability controller validates, checks schema compatibility against the previous generation, builds routes and policies, warms discovery, and reports `Accepted`, `Compatible`, `Ready` conditions. A provider can tell exactly why it is not live by reading `status`.
* **Change is a new generation.** Changing a result schema bumps `metadata.generation`; until `status.observedGeneration` catches up and `Compatible=True`, the old generation keeps serving (make-before-break, principle 13).
* **Operations are reconciled, not orchestrated.** An `execute` call creates an `Operation` whose spec is the request and whose status converges through `Running` to a terminal state; the worker that runs the query is a controller over operation keys, so a crashed replica's operations are picked up by another via resync.
* **Deletion is graceful.** Deleting a capability sets `deletionAt`; finalizers hold it until outstanding handoff tickets are revoked and routes removed; agents see `deletionAt` in discovery and stop offering it.
* **Dry-run exists.** `PUT /v1/capabilities/{name}?dryRun=All` runs validation and the compatibility diff and returns the would-be status without storing, so a CI pipeline can gate a provider release.

## Anti-patterns

* Handlers that call three downstream systems in sequence and return 500 if the second fails, leaving the first done.
* Controllers that act only on "created" events and never notice an object they missed while restarting.
* Retrying inside the sync with `time.Sleep`, or logging an error and returning `nil`.
* Writing `status` with the same `PUT` as `spec`.
* Deleting rows directly and leaving routes, tickets and caches pointing at nothing.
