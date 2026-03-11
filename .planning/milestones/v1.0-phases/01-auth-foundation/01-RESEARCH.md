# Phase 1: Auth Foundation - Research

**Researched:** 2026-02-10
**Domain:** DittoFS RBAC extension + K8s operator auth lifecycle
**Confidence:** HIGH

## Summary

This phase requires two parallel workstreams that must integrate cleanly:

1. **DittoFS server-side**: Add an `operator` role to the existing role system, add central authorization middleware, and restrict the operator role to `GET /api/v1/adapters` only. The server already has a complete JWT auth system (HMAC-SHA256, access/refresh tokens), a role model (`admin`, `user`), and user CRUD via a GORM-backed store. The new role slots in naturally alongside the existing ones.

2. **K8s operator-side**: Add an auth reconciliation step to the existing `DittoServerReconciler` that provisions a service account on the DittoFS API, stores credentials in a K8s Secret, and handles token refresh. The operator already uses controller-runtime v0.22.4, has a Reconcile loop, conditions system, and Secret management. The auth step plugs in as a new reconciliation phase.

**Primary recommendation:** Add the `operator` role as a third value in `UserRole`, implement a single `RequireRole` middleware that replaces `RequireAdmin` for role-based route authorization (fail-closed), and implement operator-side auth as a sub-reconciler or helper called from the main Reconcile loop with its own `Authenticated` condition.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Operator auto-creates a DittoFS service account with a fixed username (e.g., `k8s-operator`) on startup
- Admin credentials for bootstrap: Claude's discretion on how to source them (K8s Secret ref or CRD field)
- Password is auto-generated (strong random) by the operator, not user-provided
- If the service account already exists: reuse it, log in with stored credentials. Do not recreate or rotate.
- If provisioning fails on first boot (DittoFS not ready): block and retry with backoff. Operator does not become ready until authenticated.
- User creation API already exists in DittoFS -- operator calls it to create the service account
- On DittoServer CR deletion: operator deletes the DittoFS service account as part of teardown
- K8s Secret named with fixed convention: `{cr-name}-operator-credentials`
- Secret has ownerReference to the DittoServer CR -- garbage collected on CR deletion
- Secret stores both the JWT token and the auto-generated password
- Storing the password enables re-login if the JWT becomes invalid (e.g., after DittoFS server restart)
- Token refresh strategy: Claude's discretion (proactive at ~80% TTL or reactive on 401, or both)
- If stored JWT is invalid on operator restart: re-login with stored password to get a fresh JWT
- Updated JWT is written back to the K8s Secret
- When DittoFS API is unreachable during normal operation: Claude's discretion on retry strategy (exponential backoff recommended)
- Never delete existing K8s resources when API is unreachable -- preserve all state
- DittoServer CR gets an `Authenticated` status condition reflecting auth health
- Operator readiness probe is auth-aware: not-ready until authentication succeeds
- "operator" role is a built-in role in DittoFS, hardcoded in the server (like admin, user)
- Role scope: strictly GET /api/v1/adapters only -- absolute minimum privilege
- Role enforcement: central authorization middleware (fail-closed), not per-handler checks
- DittoFS already has a role system -- operator is a new role added to the existing system

### Claude's Discretion
- Admin credential bootstrapping mechanism (K8s Secret ref vs CRD spec field)
- Token refresh strategy details (proactive, reactive, or hybrid)
- Retry/backoff intervals and caps for API unavailability
- K8s event emission on provisioning failures (events + logs vs logs only)
- Exact error handling and logging levels

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

## Standard Stack

### Core (DittoFS Server Side)
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `golang-jwt/jwt/v5` | v5.3.0 | JWT token generation/validation | Already in use, HMAC-SHA256 signing |
| `go-chi/chi/v5` | v5.1.0 | HTTP router with middleware chain | Already in use, supports nested route groups |
| `gorm.io/gorm` | v1.31.1 | ORM for user/role persistence | Already in use for all control plane models |
| `golang.org/x/crypto` | v0.45.0 | bcrypt password hashing | Already in use for user passwords |

### Core (K8s Operator Side)
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `sigs.k8s.io/controller-runtime` | v0.22.4 | Operator reconciliation framework | Already in use, provides Client, Reconciler, conditions |
| `k8s.io/api` | v0.34.1 | K8s API types (Secret, Service, etc.) | Already in use for all K8s resources |
| `k8s.io/apimachinery` | v0.34.1 | K8s meta types, conditions | Already in use for owner references, conditions |
| DittoFS `pkg/apiclient` | (internal) | REST API client for DittoFS | Already exists, has Login/CreateUser/ListAdapters/RefreshToken |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `crypto/rand` | stdlib | Random password generation | Operator generates service account password |
| `encoding/base64` | stdlib | Password encoding | Same pattern as `models.GenerateRandomPassword()` |
| `time` | stdlib | Token TTL calculation, backoff timing | Refresh scheduling and retry backoff |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Middleware-based role authorization | Per-handler role checks | Violates fail-closed requirement; middleware is correct |
| Single `RequireRole` middleware | Separate `RequireOperator` middleware | RequireRole is more extensible, takes allowed roles as params |

## Architecture Patterns

### DittoFS Server: Role Model Extension

The current role model lives in `pkg/controlplane/models/user.go`:

```go
// Current state
type UserRole string
const (
    RoleUser  UserRole = "user"
    RoleAdmin UserRole = "admin"
)
func (r UserRole) IsValid() bool {
    return r == RoleUser || r == RoleAdmin
}
```

**Change required:** Add `RoleOperator` and update `IsValid()`.

```go
const (
    RoleUser     UserRole = "user"
    RoleAdmin    UserRole = "admin"
    RoleOperator UserRole = "operator"
)
func (r UserRole) IsValid() bool {
    return r == RoleUser || r == RoleAdmin || r == RoleOperator
}
```

### DittoFS Server: Authorization Middleware

Current middleware in `internal/controlplane/api/middleware/auth.go`:
- `JWTAuth()` -- validates token, extracts claims to context
- `RequireAdmin()` -- checks `claims.IsAdmin()`
- `RequirePasswordChange()` -- blocks until password changed

**New middleware needed:** `RequireRole(allowedRoles ...string)` -- a generalized, fail-closed authorization check.

**Key design:** Fail-closed means if no role middleware is applied to a route, the route should be denied. This is achieved by applying `RequireRole()` at the appropriate router group level. Any new endpoint added to a group with `RequireRole("admin")` is automatically admin-only.

```go
// RequireRole blocks requests from users whose role is not in the allowed list.
// Must be used after JWTAuth middleware. Fail-closed: if claims are nil or role
// doesn't match any allowed role, the request is rejected.
func RequireRole(allowedRoles ...string) func(http.Handler) http.Handler {
    roleSet := make(map[string]bool, len(allowedRoles))
    for _, role := range allowedRoles {
        roleSet[role] = true
    }
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            claims := GetClaimsFromContext(r.Context())
            if claims == nil {
                http.Error(w, "Authentication required", http.StatusUnauthorized)
                return
            }
            if !roleSet[claims.Role] {
                http.Error(w, "Insufficient permissions", http.StatusForbidden)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

### DittoFS Server: Router Changes

Currently, all protected routes use `RequireAdmin()`. The adapter routes need to be split:

```go
// Current: all adapter routes are admin-only
r.Route("/adapters", func(r chi.Router) {
    r.Use(apiMiddleware.RequireAdmin())
    r.Post("/", adapterHandler.Create)
    r.Get("/", adapterHandler.List)       // Operator needs this
    r.Get("/{type}", adapterHandler.Get)
    r.Put("/{type}", adapterHandler.Update)
    r.Delete("/{type}", adapterHandler.Delete)
})

// New: split read vs. write access
r.Route("/adapters", func(r chi.Router) {
    // Read access: admin + operator
    r.Group(func(r chi.Router) {
        r.Use(apiMiddleware.RequireRole("admin", "operator"))
        r.Get("/", adapterHandler.List)
    })
    // Write access: admin only
    r.Group(func(r chi.Router) {
        r.Use(apiMiddleware.RequireAdmin())
        r.Post("/", adapterHandler.Create)
        r.Get("/{type}", adapterHandler.Get)  // Keep admin-only or open to operator too
        r.Put("/{type}", adapterHandler.Update)
        r.Delete("/{type}", adapterHandler.Delete)
    })
})
```

**Note:** The decision says "strictly GET /api/v1/adapters only". This means the List endpoint. The Get by type endpoint (`GET /api/v1/adapters/{type}`) could also be useful for the operator but the phase boundary says list only. Keep Get admin-only unless explicitly needed.

### DittoFS Server: Claims Extension

The JWT claims already have a `Role` field. The `IsAdmin()` helper exists. We should add:

```go
func (c *Claims) IsOperator() bool {
    return c.Role == "operator"
}

func (c *Claims) HasRole(role string) bool {
    return c.Role == role
}
```

### K8s Operator: Auth Reconciliation Flow

The operator reconcile loop (`Reconcile()` in `dittoserver_controller.go`) currently follows this sequence:
1. Handle deletion (finalizer)
2. Add finalizer
3. Reconcile JWT Secret
4. Reconcile ConfigMap
5. Reconcile Services (headless, file, API, metrics)
6. Reconcile PerconaPGCluster
7. Reconcile StatefulSet
8. Update Status

**Auth reconciliation inserts after StatefulSet is ready** (because DittoFS API must be running). The recommended insertion point:

```
... existing steps 1-7 ...
8. Wait for StatefulSet ready (existing check)
9. reconcileAuth(ctx, dittoServer)  <-- NEW
   a. Check if {cr-name}-operator-credentials Secret exists
   b. If no Secret: provision new service account
      - Login as admin
      - Create user "k8s-operator" with role "operator" and random password
      - Login as operator to get JWT
      - Create Secret with JWT + password
   c. If Secret exists: validate/refresh JWT
      - Try using stored JWT
      - If expired/invalid: re-login with stored password
      - Update Secret with fresh JWT
   d. Set Authenticated condition
10. Update Status (existing, now includes Authenticated condition)
```

### K8s Operator: Credential Secret Structure

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: {cr-name}-operator-credentials
  namespace: {namespace}
  ownerReferences:
    - apiVersion: dittofs.dittofs.com/v1alpha1
      kind: DittoServer
      name: {cr-name}
      uid: {cr-uid}
type: Opaque
data:
  username: azhzLW9wZXJhdG9y           # k8s-operator
  password: <base64-encoded-random>     # auto-generated password
  access-token: <base64-encoded-jwt>    # current JWT access token
  refresh-token: <base64-encoded-jwt>   # current JWT refresh token
  server-url: <base64-encoded-url>      # DittoFS API URL (for re-login)
```

### K8s Operator: Admin Credential Bootstrap

**Recommendation (Claude's discretion):** Use a K8s Secret reference in the CRD spec.

The CRD already has `spec.identity.admin.passwordSecretRef`. The operator reads the admin password from this Secret to bootstrap the service account. This is consistent with how the operator already handles JWT secrets and S3 credentials.

However, there's a subtlety: the `passwordSecretRef` currently stores a bcrypt **hash**, not a plaintext password. The operator needs a plaintext password to call the login API. Options:

1. **Add a new field** `spec.identity.admin.loginSecretRef` that references a Secret containing the plaintext admin password. This is the cleanest separation.
2. **Use the `DITTOFS_ADMIN_INITIAL_PASSWORD` env var pattern** -- the admin password can be set via this env var, and the operator could also read it from the same Secret used to inject it.
3. **Rely on the generated password** -- the DittoFS server logs the generated admin password at startup. This is not programmatically accessible.

**Recommendation:** Option 1 -- a dedicated admin credentials Secret ref. The operator reads `admin-username` and `admin-password` keys from a referenced Secret. This Secret can be created by the user or auto-generated by the operator (similar to how JWT secret auto-generation works). If the Secret doesn't exist, the operator creates it with the same password that was used for `DITTOFS_ADMIN_INITIAL_PASSWORD`.

Actually, looking more carefully at the existing code: `buildSecretEnvVars` injects `DITTOFS_ADMIN_PASSWORD_HASH` (a bcrypt hash) or `DITTOFS_ADMIN_INITIAL_PASSWORD` (plaintext) as env vars. The operator already has the ability to reference the admin password Secret.

**Simplest approach:** The operator reads the admin password from the same Secret referenced by `spec.identity.admin.passwordSecretRef`, but uses a different key that contains the **plaintext** password (e.g., key `password` alongside the existing `password-hash` key). OR, the operator creates a dedicated Secret `{cr-name}-admin-credentials` containing the plaintext admin password (auto-generated on first run, similar to JWT secret auto-generation).

**Final recommendation:** Auto-generate an admin credentials Secret (`{cr-name}-admin-credentials`) if none exists, inject the plaintext password as `DITTOFS_ADMIN_INITIAL_PASSWORD` env var into the DittoFS pod. The operator then uses this same Secret to read the admin password for its own login. This is self-contained and requires no user input.

### K8s Operator: Token Refresh Strategy

**Recommendation (Claude's discretion):** Hybrid approach -- proactive refresh with reactive fallback.

1. **Proactive:** After successful auth, schedule a requeue at ~80% of the access token TTL. The default access token is 15 minutes, so requeue at ~12 minutes. On requeue, call the refresh endpoint.
2. **Reactive:** If any API call returns 401, attempt re-login with stored password before failing the reconcile.

Implementation: The reconcileAuth function returns `(ctrl.Result, error)`. When auth succeeds and a fresh token is obtained, it returns `ctrl.Result{RequeueAfter: tokenTTL * 80 / 100}` to schedule the next refresh.

### K8s Operator: Retry/Backoff Strategy

**Recommendation (Claude's discretion):** Exponential backoff with jitter, capped at 5 minutes.

```
Retry intervals: 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s, 300s (cap)
```

When DittoFS API is unreachable:
- Log warning with retry count
- Emit K8s Event on first failure and periodically (not every retry)
- Set `Authenticated` condition to `False` with reason `APIUnreachable`
- Requeue with backoff
- Never delete existing K8s resources

### K8s Operator: Service Account Deletion on CR Teardown

The existing `performCleanup()` handles Percona cleanup. Add DittoFS service account cleanup:

```go
func (r *DittoServerReconciler) performCleanup(ctx context.Context, dittoServer *DittoServer) error {
    // ... existing Percona cleanup ...

    // Delete DittoFS service account (best-effort)
    if err := r.cleanupOperatorServiceAccount(ctx, dittoServer); err != nil {
        logger.Error(err, "Failed to delete operator service account (best-effort)")
        // Don't return error -- this is best-effort cleanup
    }

    return nil
}
```

The cleanup function:
1. Reads the operator credentials Secret to get the admin password
2. Logs in as admin
3. Calls DELETE /api/v1/users/k8s-operator
4. If the API is unreachable, logs and continues (the Secret will be GC'd by owner reference)

### K8s Operator: Authenticated Condition

New condition type added to the conditions package:

```go
const ConditionAuthenticated = "Authenticated"
```

States:
- `True` / `AuthenticationSucceeded` -- "Operator service account authenticated successfully"
- `False` / `AuthenticationPending` -- "Waiting for DittoFS API to become available"
- `False` / `AuthenticationFailed` -- "Failed to authenticate: {error}"
- `False` / `ServiceAccountCreationFailed` -- "Failed to create operator service account: {error}"

This condition feeds into the aggregate `Ready` condition.

### K8s Operator: Readiness Probe

The operator itself (the controller manager) uses leader election health checks. Auth-awareness means:
- The `Authenticated` condition on the DittoServer CR reflects auth health
- The operator pod's readiness should NOT be gated on DittoFS auth (the operator manages multiple CRs potentially)
- Instead, the DittoServer CR's `Ready` condition incorporates `Authenticated`

Correction: Re-reading the decision -- "Operator readiness probe is auth-aware: not-ready until authentication succeeds." This means the operator pod itself should not report ready until auth is done. For a single-CR operator, this makes sense. Implementation: add a health check endpoint that checks if any managed DittoServer has `Authenticated=True`.

### Anti-Patterns to Avoid
- **Per-handler authorization checks**: The decision explicitly says "central authorization middleware, not per-handler checks." Never add `if !claims.IsAdmin()` inside handler functions.
- **Storing cleartext passwords in ConfigMaps**: Passwords go in Secrets only.
- **Polling for auth status**: Use the reconcile loop's requeue mechanism, not a separate goroutine.
- **Deleting K8s resources on API failure**: The decision says never delete when API is unreachable.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| JWT generation/validation | Custom JWT library | `golang-jwt/jwt/v5` (existing `auth.JWTService`) | Already implemented, battle-tested |
| Password hashing | Custom hash function | `models.HashPasswordWithNT()` | bcrypt + NT hash already implemented |
| Random password generation | Custom random string | `models.GenerateRandomPassword()` | 18 bytes entropy, URL-safe base64 |
| HTTP client for DittoFS API | Raw `net/http` calls | `pkg/apiclient.Client` | Already has Login, CreateUser, ListAdapters, RefreshToken |
| K8s condition management | Manual slice manipulation | `utils/conditions.SetCondition()` | Generic, typed, handles LastTransitionTime |
| Owner reference management | Manual OwnerReference construction | `controllerutil.SetControllerReference()` | Already used throughout the operator |

**Key insight:** Both the DittoFS server and operator already have all the building blocks. This phase is about wiring them together, not building new infrastructure.

## Common Pitfalls

### Pitfall 1: GORM Zero-Value Boolean Handling
**What goes wrong:** Creating a user with `Enabled: false` doesn't work because GORM applies `default:true`.
**Why it happens:** GORM cannot distinguish between "field not set" and "field set to zero value."
**How to avoid:** Create the user with `Enabled: true`, then update if needed. Or use pointer `*bool` fields. The existing `User` struct uses value bool with `gorm:"default:true"`, so always create with enabled=true.
**Warning signs:** Tests pass but user is always enabled regardless of what you set.

### Pitfall 2: Middleware Ordering in chi Router
**What goes wrong:** Role check middleware runs before JWT auth middleware, causing nil claims panic.
**Why it happens:** Chi middleware executes in the order it's registered via `r.Use()`.
**How to avoid:** Always register `JWTAuth()` before `RequireRole()` or `RequireAdmin()`. The current codebase does this correctly -- maintain the pattern.
**Warning signs:** Panic on nil pointer dereference in middleware, or 401 when 403 is expected.

### Pitfall 3: Token Expiry Race During Reconcile
**What goes wrong:** Token is valid when reconcile starts but expires mid-reconcile.
**Why it happens:** Access tokens default to 15 minutes, but a reconcile with slow API calls could take longer.
**How to avoid:** Check token validity before each API call, or implement retry-on-401 in the apiclient. The reactive part of the hybrid refresh strategy handles this.
**Warning signs:** Intermittent 401 errors during normal operation.

### Pitfall 4: Reconcile Loop Storms
**What goes wrong:** Auth failure causes rapid requeue, overwhelming the API server.
**Why it happens:** Returning `ctrl.Result{Requeue: true}` without delay causes immediate re-reconcile.
**How to avoid:** Always use `RequeueAfter` with exponential backoff. Never bare `Requeue: true` for error recovery.
**Warning signs:** CPU spike in operator, excessive API server logs.

### Pitfall 5: Secret Update Triggering Reconcile Loop
**What goes wrong:** Updating the operator-credentials Secret triggers a reconcile (because the operator Owns Secrets), which re-reads the Secret, finds a valid token, and does nothing. Harmless but wasteful.
**Why it happens:** The operator watches all owned resources.
**How to avoid:** Don't `Owns(&corev1.Secret{})` for the credentials Secret, OR add generation/annotation checks to skip no-op reconciles. The current operator already Owns Secrets (for JWT auto-generation), so this is a known pattern. The extra reconcile is cheap if auth is already valid (just reads the Secret and returns).
**Warning signs:** Double reconcile after every token refresh.

### Pitfall 6: Admin Password Change Breaks Operator
**What goes wrong:** Someone changes the admin password; operator can no longer bootstrap new service accounts.
**Why it happens:** The operator stores the initial admin password and uses it for service account provisioning.
**How to avoid:** The operator only needs admin credentials for initial service account creation. After that, it uses its own operator credentials. If the operator credentials Secret already exists, admin credentials are not needed. Only fail if both the service account doesn't exist AND admin credentials are invalid.
**Warning signs:** Operator cannot recover after admin password rotation.

### Pitfall 7: MustChangePassword Flag on Operator Service Account
**What goes wrong:** Operator creates a service account, but the user creation handler sets `MustChangePassword: true` for admin users. The operator role isn't admin, so this should be false, but verify.
**Why it happens:** The Create handler sets `mustChangePassword := role == models.RoleAdmin`. For `role == "operator"`, this evaluates to false. Correct behavior.
**How to avoid:** Verify in tests that operator-role users don't get `MustChangePassword: true`.
**Warning signs:** Operator gets 403 on all requests after login because `RequirePasswordChange` middleware blocks.

## Code Examples

### DittoFS: Adding the Operator Role

File: `pkg/controlplane/models/user.go`
```go
const (
    RoleUser     UserRole = "user"
    RoleAdmin    UserRole = "admin"
    RoleOperator UserRole = "operator"
)

func (r UserRole) IsValid() bool {
    return r == RoleUser || r == RoleAdmin || r == RoleOperator
}
```

### DittoFS: RequireRole Middleware

File: `internal/controlplane/api/middleware/auth.go`
```go
func RequireRole(allowedRoles ...string) func(http.Handler) http.Handler {
    roleSet := make(map[string]bool, len(allowedRoles))
    for _, role := range allowedRoles {
        roleSet[role] = true
    }
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            claims := GetClaimsFromContext(r.Context())
            if claims == nil {
                http.Error(w, "Authentication required", http.StatusUnauthorized)
                return
            }
            if !roleSet[claims.Role] {
                http.Error(w, "Insufficient permissions", http.StatusForbidden)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

### DittoFS: Router with Split Adapter Access

File: `pkg/controlplane/api/router.go`
```go
// Adapter configuration - split read/write
r.Route("/adapters", func(r chi.Router) {
    // Read endpoints: admin + operator
    r.Group(func(r chi.Router) {
        r.Use(apiMiddleware.RequireRole("admin", "operator"))
        r.Get("/", adapterHandler.List)
    })
    // Write endpoints: admin only
    r.Group(func(r chi.Router) {
        r.Use(apiMiddleware.RequireAdmin())
        r.Post("/", adapterHandler.Create)
        r.Get("/{type}", adapterHandler.Get)
        r.Put("/{type}", adapterHandler.Update)
        r.Delete("/{type}", adapterHandler.Delete)
    })
})
```

### Operator: Auth Reconciliation

File: `k8s/dittofs-operator/internal/controller/dittoserver_controller.go`
```go
func (r *DittoServerReconciler) reconcileAuth(ctx context.Context, ds *DittoServer) (ctrl.Result, error) {
    logger := logf.FromContext(ctx)
    secretName := ds.Name + "-operator-credentials"

    // Build API URL from the API service
    apiURL := fmt.Sprintf("http://%s-api.%s.svc.cluster.local:%d",
        ds.Name, ds.Namespace, getAPIPort(ds))

    // Check if credentials Secret exists
    secret := &corev1.Secret{}
    err := r.Get(ctx, client.ObjectKey{Namespace: ds.Namespace, Name: secretName}, secret)

    if apierrors.IsNotFound(err) {
        // First-time provisioning
        return r.provisionOperatorAccount(ctx, ds, apiURL)
    }
    if err != nil {
        return ctrl.Result{}, fmt.Errorf("failed to get credentials secret: %w", err)
    }

    // Secret exists -- validate/refresh token
    return r.refreshOperatorToken(ctx, ds, secret, apiURL)
}
```

### Operator: Credential Secret Creation

```go
func (r *DittoServerReconciler) createCredentialsSecret(
    ctx context.Context, ds *DittoServer,
    username, password, accessToken, refreshToken, serverURL string,
) error {
    secret := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:      ds.Name + "-operator-credentials",
            Namespace: ds.Namespace,
        },
        Type: corev1.SecretTypeOpaque,
        Data: map[string][]byte{
            "username":      []byte(username),
            "password":      []byte(password),
            "access-token":  []byte(accessToken),
            "refresh-token": []byte(refreshToken),
            "server-url":    []byte(serverURL),
        },
    }

    if err := controllerutil.SetControllerReference(ds, secret, r.Scheme); err != nil {
        return err
    }

    return r.Create(ctx, secret)
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Per-handler auth checks | Middleware-based authorization | Standard since chi/gorilla mux | Fail-closed by default, less error-prone |
| Shared admin credentials for API clients | Service account with least-privilege role | K8s operator pattern | Each component has own credentials, auditable |
| Token polling goroutine | Reconcile-loop-driven token refresh | controller-runtime pattern | No extra goroutines, uses K8s reconciliation model |

**Deprecated/outdated:**
- Nothing relevant -- all existing patterns in the codebase are current

## Open Questions

1. **Should `GET /api/v1/adapters/{type}` also be accessible to operator role?**
   - What we know: The decision says "strictly GET /api/v1/adapters only" (list endpoint)
   - What's unclear: Whether the operator will need individual adapter details in future phases
   - Recommendation: Start with list only (strict interpretation). Expand later if needed.

2. **How does the operator discover the DittoFS API URL?**
   - What we know: The operator creates an API service named `{cr-name}-api`
   - What's unclear: Whether to use in-cluster DNS or the service's ClusterIP
   - Recommendation: Use `http://{cr-name}-api.{namespace}.svc.cluster.local:{port}` -- standard K8s service DNS. Already have `getAPIPort()` function.

3. **Should `RequireAdmin()` be refactored to use `RequireRole("admin")`?**
   - What we know: `RequireAdmin()` currently checks `claims.IsAdmin()` directly
   - What's unclear: Whether to keep both or consolidate
   - Recommendation: Keep `RequireAdmin()` as a convenience wrapper that calls `RequireRole("admin")` internally. This maintains backward compatibility and readability while using the same underlying mechanism.

4. **What happens if the DittoFS server restarts and tokens are invalidated?**
   - What we know: JWT tokens are stateless (signed with HMAC secret). As long as the JWT secret persists (it does -- stored in K8s Secret), tokens survive server restarts.
   - What's unclear: Nothing -- this is actually not a problem. Tokens remain valid across restarts because the JWT secret is persistent.
   - Recommendation: No special handling needed. The reactive 401 handler covers edge cases.

## Sources

### Primary (HIGH confidence)
- DittoFS codebase direct inspection:
  - `pkg/controlplane/models/user.go` -- Role model, UserRole type, IsValid()
  - `internal/controlplane/api/middleware/auth.go` -- JWTAuth, RequireAdmin, RequirePasswordChange
  - `internal/controlplane/api/auth/claims.go` -- JWT Claims struct, IsAdmin()
  - `internal/controlplane/api/auth/jwt_service.go` -- JWTService, GenerateTokenPair, token durations
  - `pkg/controlplane/api/router.go` -- Route definitions, middleware chain
  - `pkg/controlplane/api/config.go` -- JWT config, default durations (15m access, 7d refresh)
  - `internal/controlplane/api/handlers/auth.go` -- Login, Refresh, Me handlers
  - `internal/controlplane/api/handlers/users.go` -- Create user, role assignment
  - `pkg/controlplane/store/interface.go` -- Store interface (CreateUser, DeleteUser, ValidateCredentials)
  - `pkg/controlplane/store/users.go` -- GORM user operations
  - `pkg/controlplane/models/admin.go` -- Admin user creation, password generation
  - `pkg/controlplane/models/credential.go` -- HashPasswordWithNT, GenerateRandomPassword
  - `pkg/apiclient/` -- Client, Login, CreateUser, ListAdapters, RefreshToken
  - `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` -- Full reconcile loop
  - `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go` -- CRD spec, IdentityConfig
  - `k8s/dittofs-operator/utils/conditions/conditions.go` -- Condition helpers
  - `k8s/dittofs-operator/go.mod` -- controller-runtime v0.22.4, k8s.io v0.34.1

### Secondary (MEDIUM confidence)
- controller-runtime reconciliation patterns -- based on existing operator code patterns (proven in this codebase)
- K8s Secret/owner reference patterns -- standard K8s patterns already used in this operator

### Tertiary (LOW confidence)
- None

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all libraries already in use, no new dependencies needed
- Architecture: HIGH -- patterns directly observed in existing codebase, straightforward extensions
- Pitfalls: HIGH -- derived from reading actual code, verified against existing tests and patterns

**Research date:** 2026-02-10
**Valid until:** 2026-03-10 (stable -- no fast-moving dependencies)
