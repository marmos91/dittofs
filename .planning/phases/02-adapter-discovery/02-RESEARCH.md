# Phase 2: Adapter Discovery - Research

**Researched:** 2026-02-10
**Domain:** K8s operator polling loop, configurable CRD intervals, controller-runtime reconciliation patterns
**Confidence:** HIGH

## Summary

Phase 2 adds adapter discovery to the DittoFS K8s operator. The operator must poll `GET /api/v1/adapters` at a configurable interval (default 30s), store the result for use by the service reconciler (Phase 3), and never delete or modify existing Services when the API returns an error or empty response.

The infrastructure from Phase 1 provides everything needed: the DittoFSClient (with auth token management), the operator credentials Secret, and the Authenticated condition. Phase 2 wires these together into a polling loop that feeds adapter state into the reconciler. This is a single-plan phase focused on: (1) adding a `ListAdapters` method to DittoFSClient, (2) adding a `spec.adapterDiscovery.pollingInterval` field to the CRD, (3) implementing the polling logic with safety guards, and (4) storing discovered adapter state for Phase 3 consumption.

**Primary recommendation:** Use `RequeueAfter` for periodic polling (not `source.Channel`). The polling interval is read from the CRD spec each reconcile, so changes take effect at the next requeue without restart. Store the last known good adapter list in a dedicated "adapter reconciler" that the main Reconcile loop calls after successful auth.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `sigs.k8s.io/controller-runtime` | v0.22.4 | Reconciler, `ctrl.Result{RequeueAfter}`, conditions | Already in use |
| `k8s.io/apimachinery` | v0.34.1 | `metav1.Duration`, condition types | Already in use |
| DittoFSClient | (internal) | HTTP client for `GET /api/v1/adapters` | Created in Phase 1, self-contained in operator module |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `encoding/json` | stdlib | JSON decoding of adapter response | Parse API response |
| `time` | stdlib | Duration parsing for polling interval | CRD field default and validation |
| `net/http` | stdlib | HTTP GET (via DittoFSClient.do) | Already in DittoFSClient |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| `RequeueAfter` periodic polling | `source.Channel` with goroutine timer | source.Channel adds complexity (goroutine management, channel lifecycle, shutdown coordination) for no benefit -- there's no external event source, just a timer. RequeueAfter is the idiomatic pattern for periodic checks on external systems |
| CRD spec field for interval | ConfigMap or env var | CRD spec is consistent with all other operator configuration and supports live changes via the reconcile loop |
| In-memory adapter cache | ConfigMap or annotation | In-memory is simpler, lower latency, and the data is ephemeral (repopulated on each poll). No persistence needed -- stale data is acceptable during operator restart |

## Architecture Patterns

### Recommended Approach: RequeueAfter-Based Polling

The decision between `source.Channel` and `RequeueAfter` was flagged as open in prior research. After analysis:

**Use `RequeueAfter`.** Reasons:

1. **No external event source exists.** `source.Channel` is designed for events originating outside the cluster (webhooks, callbacks). Adapter polling is a timer, not an event source.
2. **RequeueAfter is idiomatic for periodic external system checks.** The [controller-runtime documentation](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile) explicitly says RequeueAfter is for "situations where periodic checks are required."
3. **Simpler lifecycle.** No goroutine management, channel creation, shutdown coordination, or buffer sizing. The reconcile loop already handles all of this.
4. **Interval changes are automatic.** Each reconcile reads the current CRD spec, so changing `spec.adapterDiscovery.pollingInterval` takes effect at the next requeue without any goroutine restart logic.
5. **The auth reconciler already uses RequeueAfter** for token refresh (80% TTL). Adapter polling naturally layers on top.

### CRD Spec Extension

Add a new optional section to `DittoServerSpec`:

```go
// AdapterDiscoverySpec configures adapter discovery polling
type AdapterDiscoverySpec struct {
    // PollingInterval is how often the operator polls the adapter list API.
    // Supports Go duration strings (e.g., "30s", "1m", "5m").
    // +kubebuilder:default="30s"
    // +kubebuilder:validation:Pattern=`^[0-9]+(s|m|h)$`
    // +optional
    PollingInterval string `json:"pollingInterval,omitempty"`
}
```

In `DittoServerSpec`:

```go
// AdapterDiscovery configures adapter discovery polling
// +optional
AdapterDiscovery *AdapterDiscoverySpec `json:"adapterDiscovery,omitempty"`
```

### DittoFSClient Extension: ListAdapters

Add a `ListAdapters` method to the existing `DittoFSClient`:

```go
// AdapterInfo represents an adapter returned by the DittoFS API.
type AdapterInfo struct {
    ID      string `json:"id"`
    Type    string `json:"type"`
    Enabled bool   `json:"enabled"`
    Running bool   `json:"running"`
    Port    int    `json:"port"`
}

// ListAdapters calls GET /api/v1/adapters and returns the adapter list.
func (c *DittoFSClient) ListAdapters() ([]AdapterInfo, error) {
    var adapters []AdapterInfo
    if err := c.do(http.MethodGet, "/api/v1/adapters", nil, &adapters); err != nil {
        return nil, err
    }
    return adapters, nil
}
```

Key design notes:
- `AdapterInfo` is a minimal struct -- only the fields the operator needs (type, enabled, running, port)
- `Config`, `CreatedAt`, `UpdatedAt` are intentionally omitted (not needed by operator)
- The `Running` field is populated server-side by `runtime.IsAdapterRunning()` -- confirmed in the handler code

### Adapter Reconciler: Polling + Safety Guards

Create `adapter_reconciler.go` following the same sub-reconciler pattern as `auth_reconciler.go`:

```go
// reconcileAdapters polls the DittoFS API for adapter state and stores
// the result for use by service reconciliation (Phase 3).
// Returns ctrl.Result with RequeueAfter set to the polling interval.
//
// Safety contract (DISC-03):
//   - On API error: preserve lastKnownAdapters, do NOT clear or modify
//   - On empty response: preserve lastKnownAdapters (empty is valid, but
//     treat as "no change" for safety -- Phase 3 will handle the diff)
//   - On success: update lastKnownAdapters with fresh data
func (r *DittoServerReconciler) reconcileAdapters(
    ctx context.Context,
    dittoServer *DittoServer,
) (ctrl.Result, error) {
    // 1. Read polling interval from CRD spec
    pollingInterval := getPollingInterval(dittoServer)

    // 2. Get authenticated client
    //    (reads operator credentials Secret, uses access token)
    client, err := r.getAuthenticatedClient(ctx, dittoServer)
    if err != nil {
        // Cannot authenticate -- preserve existing state, requeue
        return ctrl.Result{RequeueAfter: pollingInterval}, nil
    }

    // 3. Poll adapter list
    adapters, err := client.ListAdapters()
    if err != nil {
        // API error -- preserve existing state (DISC-03)
        logger.Info("Adapter polling failed, preserving existing state",
            "error", err.Error())
        return ctrl.Result{RequeueAfter: pollingInterval}, nil
    }

    // 4. Store adapter state (for Phase 3 to consume)
    r.setLastKnownAdapters(dittoServer, adapters)

    // 5. Requeue at polling interval
    return ctrl.Result{RequeueAfter: pollingInterval}, nil
}
```

### Reconcile Loop Integration

The adapter reconciler inserts into the main Reconcile loop after auth succeeds:

```
... existing steps 1-8 ...
9.  reconcileAuth() -- Phase 1 (already exists)
10. reconcileAdapters() -- Phase 2 (NEW)
    - Only runs if Authenticated=True
    - Returns RequeueAfter = polling interval
11. Update Status (existing)
```

### RequeueAfter Priority

The Reconcile function may receive multiple RequeueAfter values from different sub-reconcilers:
- Auth: 80% of token TTL (~12 minutes for default 15m tokens)
- Adapters: polling interval (default 30s)

**Use the minimum.** The adapter polling interval (30s) will typically be the shortest, so it drives the reconcile cadence. Auth refresh will happen when the token approaches expiry (reconcile is happening frequently enough).

```go
// In Reconcile(), after both sub-reconcilers return:
result := ctrl.Result{}
if authResult.RequeueAfter > 0 {
    result.RequeueAfter = authResult.RequeueAfter
}
if adapterResult.RequeueAfter > 0 {
    if result.RequeueAfter == 0 || adapterResult.RequeueAfter < result.RequeueAfter {
        result.RequeueAfter = adapterResult.RequeueAfter
    }
}
return result, nil
```

### Adapter State Storage

The adapter list is stored in-memory on the reconciler for Phase 3 to consume. Options considered:

| Approach | Pros | Cons | Decision |
|----------|------|------|----------|
| In-memory map on reconciler | Simple, fast, no K8s API calls | Lost on restart (repopulated next poll) | **Selected** |
| Annotation on DittoServer CR | Persists across restarts | K8s API call every poll, 256KB limit, status update conflicts | Rejected |
| ConfigMap | Persists, no size limit concern | Extra K8s resource, reconcile complexity | Rejected |

The in-memory approach is correct because:
- Adapter state is ephemeral and cheap to repopulate (one API call)
- Phase 3 will consume this data immediately during the same reconcile
- No persistence needed -- on operator restart, the next poll repopulates

Implementation: Store as a field on `DittoServerReconciler` with a per-CR map:

```go
type DittoServerReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder

    // lastKnownAdapters stores the last successful adapter poll result per CR.
    // Key is namespace/name of the DittoServer CR.
    // Protected by adaptersMu for concurrent reconcile safety.
    adaptersMu      sync.RWMutex
    lastKnownAdapters map[string][]AdapterInfo
}
```

### Getting an Authenticated Client

The adapter reconciler needs to use the operator's JWT token from the credentials Secret:

```go
func (r *DittoServerReconciler) getAuthenticatedClient(
    ctx context.Context,
    ds *DittoServer,
) (*DittoFSClient, error) {
    // Read operator credentials Secret
    secret := &corev1.Secret{}
    err := r.Get(ctx, client.ObjectKey{
        Namespace: ds.Namespace,
        Name:      ds.GetOperatorCredentialsSecretName(),
    }, secret)
    if err != nil {
        return nil, fmt.Errorf("operator credentials not found: %w", err)
    }

    apiURL := ds.GetAPIServiceURL()
    client := NewDittoFSClient(apiURL)
    client.SetToken(string(secret.Data["access-token"]))
    return client, nil
}
```

### Anti-Patterns to Avoid

- **Deleting Services on API error:** DISC-03 is explicit -- never delete or modify existing Services based on failed or empty responses. The adapter reconciler must be a "last known good" cache.
- **Using `source.Channel` for a timer:** source.Channel is for external event sources, not self-generated timers. RequeueAfter is the correct pattern.
- **Storing adapter state in CRD status:** Status updates trigger reconcile watches, creating feedback loops. Use in-memory storage.
- **Blocking reconcile on adapter poll failure:** The adapter poll is best-effort. If it fails, preserve existing state and requeue. Never block infrastructure reconciliation.
- **Separate goroutine for polling:** The reconcile loop IS the polling mechanism. A separate goroutine adds complexity and races with no benefit.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Periodic timer | `time.Ticker` in goroutine | `ctrl.Result{RequeueAfter}` | Controller-runtime manages scheduling, rate limiting, shutdown |
| HTTP client | Raw `net/http` calls | `DittoFSClient.ListAdapters()` | Consistent auth header handling, error types |
| Duration parsing | Custom parser | `time.ParseDuration()` | Standard library, handles all Go duration formats |
| Condition management | Manual slice ops | `conditions.SetCondition()` | Already used, handles LastTransitionTime |
| Service building | Manual corev1.Service construction | `resources.NewServiceBuilder()` | Already exists, fluent API |

**Key insight:** Phase 2 is glue code. It wires the Phase 1 auth client to a polling loop and stores results for Phase 3. No new infrastructure patterns needed.

## Common Pitfalls

### Pitfall 1: RequeueAfter Overwrite Between Sub-Reconcilers
**What goes wrong:** Auth returns `RequeueAfter: 12m` (80% of 15m TTL), adapter returns `RequeueAfter: 30s`. If the code just uses `adapterResult` it drops the auth refresh schedule.
**Why it happens:** ctrl.Result from each sub-reconciler overwrites the previous one.
**How to avoid:** Use the minimum RequeueAfter from all sub-reconcilers. At 30s polling, the reconcile runs frequently enough that auth refresh at 12m is guaranteed to happen.
**Warning signs:** Token expiry errors appearing despite auth reconciler working correctly.

### Pitfall 2: Empty Adapter List vs API Error
**What goes wrong:** Operator treats an empty adapter list (no adapters configured yet) the same as an API error, and refuses to act on it.
**Why it happens:** Both return zero-length data, but the semantics are different.
**How to avoid:** Distinguish by error value. `err != nil` = API error (preserve state). `err == nil && len(adapters) == 0` = legitimate empty list (store it -- Phase 3 will handle cleanup). DISC-03 says "never deletes Services based on failed/empty responses" -- this refers to API ERRORS, not a successful response with zero adapters. However, for maximum safety in Phase 2, we should store the empty list but let Phase 3 decide what to do with it. For now, an explicit success with empty list is still stored.
**Warning signs:** Stale adapter Services persisting even after all adapters are removed via API.

### Pitfall 3: Polling Before Auth is Ready
**What goes wrong:** Adapter polling runs before the operator has authenticated, getting 401 errors every cycle.
**Why it happens:** The reconcile loop might reach the adapter polling step before auth is complete.
**How to avoid:** Guard adapter polling with `if Authenticated != True, skip`. The current code already gates auth reconciliation on `ReadyReplicas >= 1`. Adapter polling adds another guard: only poll if the Authenticated condition is True.
**Warning signs:** Flood of 401 errors in operator logs during startup.

### Pitfall 4: Interval Change Not Taking Effect
**What goes wrong:** User changes `spec.adapterDiscovery.pollingInterval` but the operator keeps using the old value.
**Why it happens:** If the interval is cached in a field rather than read from the spec each time.
**How to avoid:** Read the polling interval from the DittoServer spec on every reconcile invocation, never cache it. Since the CRD change triggers a reconcile (the controller watches its own CRD), the new interval takes effect on the very next reconcile.
**Warning signs:** Polling frequency doesn't change after CRD spec update.

### Pitfall 5: Concurrent Reconcile Races on In-Memory State
**What goes wrong:** Two reconcile goroutines write to `lastKnownAdapters` simultaneously, causing data corruption.
**Why it happens:** Controller-runtime may run concurrent reconciles for different CRs (or even the same CR under certain conditions).
**How to avoid:** Use a `sync.RWMutex` to protect the adapter cache. Write-lock when updating, read-lock when Phase 3 reads.
**Warning signs:** Intermittent panics or garbled adapter state.

### Pitfall 6: 401 During Adapter Poll (Token Expired Mid-Cycle)
**What goes wrong:** Token was valid when auth reconciled but expired by the time adapter polling runs.
**Why it happens:** Auth runs first, gets 80% TTL requeue. But if the reconcile is triggered for another reason (spec change), auth may not re-check.
**How to avoid:** If ListAdapters returns an auth error (401/403), treat it as transient. The next reconcile will refresh the token via auth reconciler first, then retry the adapter poll. Do NOT attempt token refresh inline in the adapter reconciler.
**Warning signs:** Adapter polling returns 401 intermittently, especially near token expiry.

## Code Examples

### CRD Spec Field

File: `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go`
```go
type DittoServerSpec struct {
    // ... existing fields ...

    // AdapterDiscovery configures adapter discovery polling
    // +optional
    AdapterDiscovery *AdapterDiscoverySpec `json:"adapterDiscovery,omitempty"`
}

// AdapterDiscoverySpec configures adapter discovery polling
type AdapterDiscoverySpec struct {
    // PollingInterval is how often the operator polls the adapter list API.
    // +kubebuilder:default="30s"
    // +optional
    PollingInterval string `json:"pollingInterval,omitempty"`
}
```

### Helper to Get Polling Interval

File: `k8s/dittofs-operator/internal/controller/adapter_reconciler.go`
```go
const defaultPollingInterval = 30 * time.Second

func getPollingInterval(ds *DittoServer) time.Duration {
    if ds.Spec.AdapterDiscovery != nil && ds.Spec.AdapterDiscovery.PollingInterval != "" {
        d, err := time.ParseDuration(ds.Spec.AdapterDiscovery.PollingInterval)
        if err == nil && d > 0 {
            return d
        }
    }
    return defaultPollingInterval
}
```

### DittoFSClient.ListAdapters

File: `k8s/dittofs-operator/internal/controller/dittofs_client.go`
```go
// AdapterInfo represents an adapter returned by the DittoFS API.
type AdapterInfo struct {
    ID      string `json:"id"`
    Type    string `json:"type"`
    Enabled bool   `json:"enabled"`
    Running bool   `json:"running"`
    Port    int    `json:"port"`
}

// ListAdapters calls GET /api/v1/adapters and returns the adapter list.
func (c *DittoFSClient) ListAdapters() ([]AdapterInfo, error) {
    var adapters []AdapterInfo
    if err := c.do(http.MethodGet, "/api/v1/adapters", nil, &adapters); err != nil {
        return nil, err
    }
    return adapters, nil
}
```

### Adapter Reconciler (Full)

File: `k8s/dittofs-operator/internal/controller/adapter_reconciler.go`
```go
// reconcileAdapters polls the DittoFS API for adapter state.
// Safety contract (DISC-03): on API error, preserves existing adapter state.
func (r *DittoServerReconciler) reconcileAdapters(
    ctx context.Context,
    dittoServer *DittoServer,
) (ctrl.Result, error) {
    logger := logf.FromContext(ctx)
    pollingInterval := getPollingInterval(dittoServer)

    apiClient, err := r.getAuthenticatedClient(ctx, dittoServer)
    if err != nil {
        logger.Info("Cannot create authenticated client for adapter polling",
            "error", err.Error())
        return ctrl.Result{RequeueAfter: pollingInterval}, nil
    }

    adapters, err := apiClient.ListAdapters()
    if err != nil {
        logger.Info("Adapter polling failed, preserving existing state",
            "error", err.Error())
        // DISC-03: never act on failed response
        return ctrl.Result{RequeueAfter: pollingInterval}, nil
    }

    // Store successful result
    key := dittoServer.Namespace + "/" + dittoServer.Name
    r.adaptersMu.Lock()
    if r.lastKnownAdapters == nil {
        r.lastKnownAdapters = make(map[string][]AdapterInfo)
    }
    r.lastKnownAdapters[key] = adapters
    r.adaptersMu.Unlock()

    logger.V(1).Info("Adapter poll successful",
        "adapterCount", len(adapters),
        "nextPoll", pollingInterval.String())

    return ctrl.Result{RequeueAfter: pollingInterval}, nil
}

// getLastKnownAdapters returns the last successful adapter poll result.
// Returns nil if no successful poll has occurred.
func (r *DittoServerReconciler) getLastKnownAdapters(ds *DittoServer) []AdapterInfo {
    key := ds.Namespace + "/" + ds.Name
    r.adaptersMu.RLock()
    defer r.adaptersMu.RUnlock()
    if r.lastKnownAdapters == nil {
        return nil
    }
    return r.lastKnownAdapters[key]
}
```

### Reconcile Loop Integration

File: `k8s/dittofs-operator/internal/controller/dittoserver_controller.go`
```go
// In Reconcile(), after auth reconciliation:
if statefulSet.Status.ReadyReplicas >= 1 {
    // Auth reconciliation
    authResult, authErr := r.reconcileAuth(ctx, dittoServer)
    // ... existing error handling ...

    // Adapter discovery (only if authenticated)
    if conditions.IsConditionTrue(dittoServer.Status.Conditions, conditions.ConditionAuthenticated) {
        adapterResult, _ := r.reconcileAdapters(ctx, dittoServer)

        // Use minimum RequeueAfter
        result := mergeResults(authResult, adapterResult)
        return result, nil
    }

    if authResult.RequeueAfter > 0 {
        return authResult, nil
    }
}
```

### Test Pattern: Mock Adapter API

```go
func TestReconcileAdapters_Success(t *testing.T) {
    ds := newTestDittoServer("test-server", "default")

    // Create operator credentials Secret
    credSecret := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:      ds.GetOperatorCredentialsSecretName(),
            Namespace: "default",
        },
        Data: map[string][]byte{
            "access-token": []byte("valid-token"),
            "server-url":   []byte("http://overridden"),
        },
    }

    r := setupAuthReconciler(t, ds, credSecret)

    server := mockDittoFSServer(t, map[string]http.HandlerFunc{
        "GET /api/v1/adapters": func(w http.ResponseWriter, req *http.Request) {
            json.NewEncoder(w).Encode([]AdapterInfo{
                {Type: "nfs", Enabled: true, Running: true, Port: 12049},
                {Type: "smb", Enabled: true, Running: true, Port: 12445},
            })
        },
    })

    // Override API URL in credentials
    credSecret.Data["server-url"] = []byte(server.URL)

    result, err := r.reconcileAdapters(context.Background(), ds)
    // ... verify result.RequeueAfter > 0
    // ... verify r.getLastKnownAdapters(ds) has 2 entries
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Goroutine timer + channel | `ctrl.Result{RequeueAfter}` | controller-runtime best practice | Simpler lifecycle, no goroutine management |
| Hardcoded poll interval | CRD spec field | K8s operator pattern | Live-configurable without restart |
| Persist poll results in ConfigMap | In-memory map with mutex | Modern operator pattern | Simpler, avoids K8s API call per poll |

**Deprecated/outdated:**
- `source.Channel` for self-generated timers: while not deprecated as an API, using it for simple polling is over-engineered. The controller-runtime docs recommend RequeueAfter for periodic external system checks.

## Open Questions

1. **Should the adapter polling set a dedicated condition (e.g., AdaptersDiscovered)?**
   - What we know: The requirements (DISC-01/02/03) don't mention a condition. Phase roadmap v2 mentions `AdaptersReady` condition but it's deferred.
   - What's unclear: Whether to add a condition now or wait for Phase 3.
   - Recommendation: Don't add a condition in Phase 2. The polling is transparent -- it either succeeds (stored in memory) or fails (preserved last state). Phase 3 can add `AdaptersReady` when it needs to track Service reconciliation status. Keep Phase 2 minimal.

2. **Should adapter polling handle 401 errors specially (trigger re-auth)?**
   - What we know: Auth reconciler handles token lifecycle. A 401 during adapter polling means the token expired between auth reconcile and adapter reconcile.
   - What's unclear: Whether to inline token refresh in the adapter reconciler.
   - Recommendation: No. Treat 401 like any other API error -- preserve state, requeue. The next reconcile will run auth reconciler first (refreshing the token), then adapter polling succeeds. This keeps the auth logic in one place and the adapter reconciler simple.

3. **Should empty successful responses be distinguished from non-empty ones?**
   - What we know: DISC-03 says "never deletes Services based on failed/empty responses." The "empty" here is ambiguous -- could mean API error or genuinely zero adapters.
   - What's unclear: Whether an empty adapter list is valid state or should be treated as an error.
   - Recommendation: Store the result regardless. A successful response with zero adapters is valid (server has no adapters configured). Phase 3 will interpret it. The safety guard in DISC-03 is about API failures, not about legitimate empty states. However, since Phase 2 does not manage Services, this question is Phase 3's concern. Phase 2 simply stores whatever the API returns on success.

## Sources

### Primary (HIGH confidence)
- DittoFS codebase direct inspection:
  - `internal/controlplane/api/handlers/adapters.go` -- AdapterResponse struct with `Running` field, List handler
  - `pkg/controlplane/api/router.go` -- GET /api/v1/adapters uses RequireRole("admin", "operator")
  - `pkg/controlplane/models/adapter.go` -- AdapterConfig model (Type, Enabled, Port)
  - `pkg/controlplane/runtime/runtime.go` -- IsAdapterRunning() confirms Running is server-side computed
  - `k8s/dittofs-operator/internal/controller/dittofs_client.go` -- DittoFSClient with do() method
  - `k8s/dittofs-operator/internal/controller/auth_reconciler.go` -- Auth reconciliation patterns
  - `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` -- Reconcile loop, RequeueAfter usage
  - `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go` -- CRD spec structure
  - `k8s/dittofs-operator/api/v1alpha1/helpers.go` -- GetAPIServiceURL(), GetOperatorCredentialsSecretName()
  - `k8s/dittofs-operator/utils/conditions/conditions.go` -- Condition types including Authenticated
  - `k8s/dittofs-operator/pkg/resources/service.go` -- ServiceBuilder for future Phase 3
- [controller-runtime source package docs](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/source) -- source.Channel for external event sources
- [controller-runtime reconcile package docs](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile) -- RequeueAfter semantics
- [Kubebuilder book - Watching Resources](https://book.kubebuilder.io/reference/watching-resources) -- source.Channel vs watches guidance

### Secondary (MEDIUM confidence)
- controller-runtime best practices for periodic polling -- based on documentation and community patterns confirmed against existing operator code patterns

### Tertiary (LOW confidence)
- None

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all libraries already in use, extending existing DittoFSClient
- Architecture: HIGH -- RequeueAfter pattern directly observed in existing codebase (auth reconciler), CRD extension follows established patterns
- Pitfalls: HIGH -- derived from reading actual reconcile loop code and understanding controller-runtime concurrency model

**Research date:** 2026-02-10
**Valid until:** 2026-03-10 (stable -- no fast-moving dependencies)
