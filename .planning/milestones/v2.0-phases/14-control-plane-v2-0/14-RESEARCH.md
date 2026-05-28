# Phase 14: Control Plane v2.0 - Research

**Researched:** 2026-02-16
**Domain:** Control Plane API, Adapter Settings, Share Security Policy, Netgroups, CLI
**Confidence:** HIGH

## Summary

This phase extends the existing DittoFS control plane with three major additions: (1) per-adapter typed settings for NFSv4 and SMB with hot-reload via DB polling, (2) per-share security policies with auth flavor toggles and squash controls extending the existing ShareAccessPolicy, and (3) netgroups as a first-class API resource for IP-based access control. All exposed through REST API and dittofsctl CLI.

The existing codebase provides a solid foundation. The control plane already has GORM-backed persistence (SQLite/PostgreSQL), a chi-based REST API with JWT auth and RFC 7807 error responses, a runtime that bridges persistent config with in-memory state, and Cobra-based CLI commands. The adapter model already stores JSON config blobs but lacks typed settings. The share model has squash modes and access rules but lacks auth flavor controls and netgroup references. The adapter factory currently hardcodes minimal config extraction from the JSON blob.

**Primary recommendation:** Use GORM models with typed fields for adapter settings and security policy (not JSON blobs), add a settings polling goroutine in the runtime, and extend the existing chi router patterns for new endpoints. The apiclient needs a `patch` method for PATCH support.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Security policy uses **boolean toggles** (not flavor lists) for cross-adapter compatibility: `allow_auth_sys`, `require_kerberos`, `min_kerberos_level`
- Default posture: **AUTH_SYS allowed** for shares without explicit security config
- Security + squashing grouped together as the share's "access policy" (extends existing ShareAccessPolicy)
- Kerberos required on a share but no keytab configured: **refuse to start share** (fail-fast)
- Security policy changes take effect **immediately** (hot-reload). New connections use updated policy
- Existing connections are **grandfathered** when policy tightens
- **Audit trail** via structured server log entries (INFO level) for all security policy changes
- Netgroups as a **first-class API resource** with CRUD operations (stored in DB)
- Support **IPs, CIDRs, and DNS hostnames** (including wildcards like *.example.com)
- Hostname matching via **reverse DNS only** (PTR lookup, not FCRDNS)
- Empty allowlist = **allow all** (no IP restrictions). Add entries to restrict
- Security flavors are **share-level only**, not per-netgroup
- Netgroups are shared resources -- one netgroup can be referenced by multiple shares
- NFSv4 version negotiation: **min/max version range** (min_version, max_version)
- Lease and grace period: **per-adapter (global)**, not per-share
- **Extended timeout set**: lease_time (90s), grace_period (90s), delegation_recall_timeout (90s), callback_timeout (5s), lease_break_timeout (35s), max_connections
- max_compound_ops: **configurable** (default 50), DoS protection
- max_clients: **configurable** (default 10000), memory protection
- Transport tuning: max_read_size, max_write_size, preferred_transfer_size
- Pseudo-fs root always '/' (not configurable)
- Global log level only (no per-adapter log level override)
- SMB gets **equivalent knobs**: max_connections, session_timeout, oplock/lease_break_timeout, max_sessions
- **Min/max dialect** negotiation control (SMB2.0, SMB2.1, SMB3.0, SMB3.1.1)
- **enable_encryption toggle (stub)** -- config knob present, logs "not yet implemented"
- Delegation policy: **configurable** at adapter level: delegations_enabled=true/false (default true)
- Per-adapter **and** per-share operation blocklist. Adapter sets baseline, shares add to it
- Disabled ops return NFS4ERR_NOTSUPP
- Adapter settings: **nested under adapter** -- `PUT /adapters/nfs/settings`
- Security policy: **part of share CRUD** body (extends existing share model)
- Netgroups: top-level resource -- `GET/POST/DELETE /netgroups`
- Both **PATCH (partial) + PUT (full replace)** for adapter settings
- Validation returns **per-field errors**: `{ "errors": { "lease_time": "must be >= 30s" } }`
- **Strict range validation** with **--force flag** to bypass (logs warning)
- **Defaults endpoint**: `GET /adapters/nfs/settings/defaults`
- **Granular permissions**: new `manage-settings` permission (Phase 14.1 integrates into RBAC)
- Admin users/groups automatically have manage-settings permission
- CLI: `dittofsctl adapter nfs settings show/update/reset`
- CLI: `dittofsctl netgroup create/list/delete/add-member/remove-member` (top-level)
- Settings display: **config-style view** (grouped key-values with sections) for `show`, table for `list`
- Non-default values **marked with '*'** in CLI output
- `dittofsctl adapter nfs settings reset [--setting lease_time]` -- reset all or specific
- **--dry-run flag** for settings updates (validates without applying)
- No import/export -- backup handled by existing `dittofs backup controlplane`
- Adapter creation via API **automatically creates** default settings record
- Existing adapters: **DB migration auto-populates** default settings
- Existing shares: **migration auto-populates** default security policy
- GORM auto-migrate for schema changes
- API is sole source of truth (config file does not define adapters)
- **Polling interval: 10 seconds** -- adapter checks DB for setting changes
- New connections use updated settings; existing connections grandfathered
- Full **E2E tests with real NFS + SMB mounts** for both adapters
- One **full lifecycle test**: create adapter -> set settings -> create share with security policy -> mount -> verify behavior
- Plus **focused scenario tests**: Kerberos required rejects AUTH_SYS, lease_time change takes effect, etc.
- Extend existing adapter model with richer settings fields
- Extend existing **ShareAccessPolicy** with security fields
- Add **Netgroup** and **NetgroupMember** as new GORM models

### Claude's Discretion
- Exact API response format and HTTP status codes
- Internal settings storage schema design
- Polling implementation details
- Prometheus metrics for control plane operations (settings changes, RBAC events)
- Settings hot-reload thread safety approach
- E2E test helper design

### Deferred Ideas (OUT OF SCOPE)
- **Phase 14.1: RBAC Migration** -- Role-based access control (admin/operator/viewer), migrate from is_admin flag and legacy permissions, per-user and per-group roles, manage-settings permission folded into RBAC
- **Documentation update** (issue #120) -- Config file no longer defines adapters, docs need updating
</user_constraints>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| GORM | v1 (already in use) | ORM for adapter settings, netgroup models | Already used for all controlplane persistence |
| go-chi/chi/v5 | (already in use) | HTTP routing for new API endpoints | Already used for all API routes |
| spf13/cobra | (already in use) | CLI subcommands for settings and netgroups | Already used for all dittofsctl commands |
| google/uuid | (already in use) | UUID generation for new model IDs | Already used for all model IDs |
| net (stdlib) | Go stdlib | IP/CIDR parsing for netgroups | `net.ParseCIDR`, `net.ParseIP`, `net.LookupAddr` |
| time (stdlib) | Go stdlib | Duration parsing/validation for timeout settings | Already used throughout |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| encoding/json | Go stdlib | JSON serialization for API | Already used for all handlers |
| sync | Go stdlib | RWMutex for hot-reload thread safety | Settings cache in runtime |
| context | Go stdlib | Polling goroutine lifecycle | Settings watcher goroutine |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Typed GORM fields | JSON blob in AdapterConfig.Config | JSON blob already exists but lacks validation, defaults management, and migration support. Typed fields give compile-time safety and GORM auto-migrate |
| DB polling (10s) | GORM hooks/callbacks | Polling is simpler, decoupled, and works across processes. Hooks only fire in-process |
| Separate settings table | Extend adapters table with all columns | Separate table is cleaner: adapter config is immutable identity, settings change frequently |

## Architecture Patterns

### Recommended Project Structure
```
pkg/controlplane/models/
  adapter.go              # Existing - add AdapterSettings type reference
  adapter_settings.go     # NEW - NFSAdapterSettings, SMBAdapterSettings GORM models
  netgroup.go             # NEW - Netgroup, NetgroupMember GORM models
  share.go                # MODIFY - Add security policy fields to Share
  errors.go               # MODIFY - Add netgroup errors
  models.go               # MODIFY - Register new models in AllModels()

pkg/controlplane/store/
  interface.go            # MODIFY - Add adapter settings + netgroup operations
  adapter_settings.go     # NEW - GORM adapter settings operations
  netgroups.go            # NEW - GORM netgroup CRUD operations
  shares.go               # MODIFY - Handle new share security policy fields

pkg/controlplane/runtime/
  runtime.go              # MODIFY - Add settings cache, polling goroutine
  settings_watcher.go     # NEW - DB polling for settings hot-reload
  share.go                # MODIFY - Add security policy fields to Share/ShareConfig
  netgroups.go            # NEW - Netgroup membership checking at runtime

internal/controlplane/api/handlers/
  adapter_settings.go     # NEW - Settings CRUD handler
  netgroups.go            # NEW - Netgroup CRUD handler
  shares.go               # MODIFY - Handle security policy in share requests

pkg/controlplane/api/
  router.go               # MODIFY - Register new routes

pkg/apiclient/
  adapter_settings.go     # NEW - Client methods for settings API
  netgroups.go            # NEW - Client methods for netgroups API

cmd/dittofsctl/commands/
  adapter/
    settings.go           # NEW - settings show/update/reset subcommands
  netgroup/
    netgroup.go           # NEW - Parent command
    create.go             # NEW
    list.go               # NEW
    delete.go             # NEW
    add_member.go         # NEW
    remove_member.go      # NEW
```

### Pattern 1: Typed Adapter Settings Model (Separate Table)
**What:** Store adapter settings in a dedicated GORM table with typed fields rather than extending the JSON blob in AdapterConfig.Config.
**When to use:** When settings need validation, defaults, and migration support.
**Example:**
```go
// pkg/controlplane/models/adapter_settings.go

// NFSAdapterSettings stores NFSv4-specific adapter settings.
// Each NFS adapter has exactly one settings record (1:1 relationship).
type NFSAdapterSettings struct {
    ID                    string        `gorm:"primaryKey;size:36" json:"id"`
    AdapterID             string        `gorm:"uniqueIndex;not null;size:36" json:"adapter_id"`

    // Version negotiation
    MinVersion            string        `gorm:"default:3;size:10" json:"min_version"`          // "3", "4.0", "4.1"
    MaxVersion            string        `gorm:"default:4.1;size:10" json:"max_version"`

    // Timeouts (stored as seconds in DB, exposed as durations in API)
    LeaseTime             int           `gorm:"default:90" json:"lease_time"`                  // seconds
    GracePeriod           int           `gorm:"default:90" json:"grace_period"`                // seconds
    DelegationRecallTimeout int         `gorm:"default:90" json:"delegation_recall_timeout"`   // seconds
    CallbackTimeout       int           `gorm:"default:5" json:"callback_timeout"`             // seconds
    LeaseBreakTimeout     int           `gorm:"default:35" json:"lease_break_timeout"`         // seconds

    // Connection limits
    MaxConnections        int           `gorm:"default:0" json:"max_connections"`              // 0 = unlimited
    MaxClients            int           `gorm:"default:10000" json:"max_clients"`
    MaxCompoundOps        int           `gorm:"default:50" json:"max_compound_ops"`

    // Transport tuning
    MaxReadSize           int           `gorm:"default:1048576" json:"max_read_size"`          // 1MB
    MaxWriteSize          int           `gorm:"default:1048576" json:"max_write_size"`         // 1MB
    PreferredTransferSize int           `gorm:"default:1048576" json:"preferred_transfer_size"` // 1MB

    // Delegation policy
    DelegationsEnabled    bool          `gorm:"default:true" json:"delegations_enabled"`

    // Operation blocklist (stored as comma-separated, exposed as array in API)
    BlockedOperations     string        `gorm:"type:text" json:"-"`

    // Timestamps
    CreatedAt             time.Time     `gorm:"autoCreateTime" json:"created_at"`
    UpdatedAt             time.Time     `gorm:"autoUpdateTime" json:"updated_at"`
}
```

### Pattern 2: Share Security Policy Extension
**What:** Extend the existing Share model with security policy fields directly on the table.
**When to use:** Security policy is 1:1 with shares, so separate table adds unnecessary joins.
**Example:**
```go
// Fields added directly to models.Share struct

// Security Policy (auth flavor controls)
AllowAuthSys         bool   `gorm:"default:true" json:"allow_auth_sys"`
RequireKerberos      bool   `gorm:"default:false" json:"require_kerberos"`
MinKerberosLevel     string `gorm:"default:krb5;size:20" json:"min_kerberos_level"` // krb5, krb5i, krb5p

// Netgroup reference (nullable - nil means no IP restrictions)
NetgroupID           *string `gorm:"size:36" json:"netgroup_id,omitempty"`

// Per-share operation blocklist (comma-separated in DB, array in API)
BlockedOperations    string `gorm:"type:text" json:"-"`
```

### Pattern 3: Settings Hot-Reload via DB Polling
**What:** A background goroutine in the runtime polls the DB every 10 seconds for settings changes. Changed settings are applied to new connections.
**When to use:** When settings need to take effect without adapter restart.
**Example:**
```go
// pkg/controlplane/runtime/settings_watcher.go

type SettingsWatcher struct {
    mu           sync.RWMutex
    store        store.Store

    // Cached settings (read by adapters on new connections)
    nfsSettings  *models.NFSAdapterSettings
    smbSettings  *models.SMBAdapterSettings

    // Last known UpdatedAt for change detection
    nfsUpdatedAt time.Time
    smbUpdatedAt time.Time

    pollInterval time.Duration
    stopCh       chan struct{}
}

func (w *SettingsWatcher) Start(ctx context.Context) {
    ticker := time.NewTicker(w.pollInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-w.stopCh:
            return
        case <-ticker.C:
            w.pollSettings(ctx)
        }
    }
}

func (w *SettingsWatcher) pollSettings(ctx context.Context) {
    // Check NFS settings
    nfsSettings, err := w.store.GetNFSAdapterSettings(ctx)
    if err == nil && nfsSettings.UpdatedAt.After(w.nfsUpdatedAt) {
        w.mu.Lock()
        old := w.nfsSettings
        w.nfsSettings = nfsSettings
        w.nfsUpdatedAt = nfsSettings.UpdatedAt
        w.mu.Unlock()

        logger.Info("NFS adapter settings updated",
            "changed_at", nfsSettings.UpdatedAt,
            "lease_time", nfsSettings.LeaseTime)
    }
}

// GetNFSSettings returns the cached NFS settings (called by adapter on new connections)
func (w *SettingsWatcher) GetNFSSettings() *models.NFSAdapterSettings {
    w.mu.RLock()
    defer w.mu.RUnlock()
    return w.nfsSettings
}
```

### Pattern 4: Netgroup Model (First-Class Resource)
**What:** Netgroups as a standalone GORM model with members in a separate table.
**When to use:** Netgroups are shared across shares and have their own lifecycle.
**Example:**
```go
// pkg/controlplane/models/netgroup.go

type Netgroup struct {
    ID        string           `gorm:"primaryKey;size:36" json:"id"`
    Name      string           `gorm:"uniqueIndex;not null;size:255" json:"name"`
    Members   []NetgroupMember `gorm:"foreignKey:NetgroupID" json:"members,omitempty"`
    CreatedAt time.Time        `gorm:"autoCreateTime" json:"created_at"`
    UpdatedAt time.Time        `gorm:"autoUpdateTime" json:"updated_at"`
}

type NetgroupMember struct {
    ID         string `gorm:"primaryKey;size:36" json:"id"`
    NetgroupID string `gorm:"not null;size:36;index" json:"netgroup_id"`
    Type       string `gorm:"not null;size:20" json:"type"`    // "ip", "cidr", "hostname"
    Value      string `gorm:"not null;size:255" json:"value"`  // "192.168.1.100", "10.0.0.0/8", "*.example.com"
    CreatedAt  time.Time `gorm:"autoCreateTime" json:"created_at"`
}
```

### Pattern 5: Per-Field Validation Errors
**What:** Return validation errors keyed by field name instead of a single error string.
**When to use:** For adapter settings updates where multiple fields can be invalid.
**Example:**
```go
// internal/controlplane/api/handlers/adapter_settings.go

type ValidationErrors map[string]string

type ValidationErrorResponse struct {
    Type   string           `json:"type"`
    Title  string           `json:"title"`
    Status int              `json:"status"`
    Errors ValidationErrors `json:"errors"`
}

func WriteValidationErrors(w http.ResponseWriter, errors ValidationErrors) {
    resp := ValidationErrorResponse{
        Type:   "about:blank",
        Title:  "Validation Failed",
        Status: http.StatusUnprocessableEntity,
        Errors: errors,
    }
    w.Header().Set("Content-Type", ContentTypeProblemJSON)
    w.WriteHeader(http.StatusUnprocessableEntity)
    json.NewEncoder(w).Encode(resp)
}
```

### Anti-Patterns to Avoid
- **Storing typed settings in the JSON blob:** The existing `AdapterConfig.Config` JSON blob is untyped and lacks migration support. Use typed GORM models instead.
- **Restarting adapters on settings change:** The decision says "new connections use updated settings; existing connections grandfathered." Don't restart adapters -- poll and cache settings.
- **Per-share lease times:** Lease and grace periods are per-adapter (global), not per-share. Don't add these to the share model.
- **Blocking on DNS lookups in hot path:** Netgroup hostname matching uses reverse DNS. Cache PTR lookups with TTL to avoid blocking the connection accept path.
- **Using AdapterConfig.Config JSON for settings defaults:** The defaults endpoint and reset functionality need typed defaults, not JSON parsing.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| IP/CIDR matching | Custom IP parser | `net.ParseCIDR`, `net.IP.Mask` | Edge cases with IPv6, mapped addresses |
| Hostname wildcard matching | Custom glob | `path.Match` or simple prefix check | `*.example.com` is just suffix matching after the `*` |
| Reverse DNS | Custom DNS client | `net.LookupAddr` (stdlib) | Handles OS-level resolver config, caching |
| Duration validation | Custom parser | `time.ParseDuration` or integer seconds | Standard format, no ambiguity |
| UUID generation | Custom IDs | `github.com/google/uuid` | Already used throughout codebase |
| JSON PATCH semantics | Custom merge logic | Pointer fields with `omitempty` | Go's nil vs zero-value distinction handles partial updates cleanly |

**Key insight:** The existing codebase already has patterns for everything needed. The adapter handler has Create/Update/Delete, the share handler has permission management, the store has GORM CRUD. Follow these patterns exactly.

## Common Pitfalls

### Pitfall 1: GORM Zero-Value Boolean Default Trap
**What goes wrong:** Creating a record with `AllowAuthSys: false` when the GORM default is `true` -- GORM ignores the zero value and applies the default.
**Why it happens:** GORM auto-defaults cannot distinguish "not provided" from "explicitly set to zero."
**How to avoid:** The existing codebase already handles this pattern. For share security policy: migration sets defaults for existing shares. For new shares via API, the handler explicitly sets all fields from the request body. Use the same `*bool` pointer pattern used by `UpdateAdapterRequest.Enabled`.
**Warning signs:** Tests pass when creating with defaults but fail when creating with explicit `false` values.

### Pitfall 2: Settings Polling Race Condition
**What goes wrong:** Adapter reads partially-updated settings (e.g., new lease_time but old grace_period from previous poll).
**Why it happens:** If the watcher updates settings field by field instead of atomically swapping the entire struct.
**How to avoid:** Always replace the entire settings struct atomically under a write lock. Adapters read a snapshot under a read lock. Use pointer swapping pattern: `w.nfsSettings = newSettings` (not field-by-field mutation).
**Warning signs:** Intermittent test failures when settings are changed during active connections.

### Pitfall 3: Netgroup DNS Lookup Blocking
**What goes wrong:** Connection acceptance blocks on PTR lookup for every new connection when hostname-based netgroups are used.
**Why it happens:** `net.LookupAddr` is synchronous and can take seconds with DNS timeouts.
**How to avoid:** Cache reverse DNS lookups with a reasonable TTL (e.g., 5 minutes). Use a concurrent-safe LRU cache. Fall back to IP matching if DNS lookup fails or times out.
**Warning signs:** Connection latency spikes when using hostname-based netgroups with unreliable DNS.

### Pitfall 4: GORM Auto-Migrate Adding New Columns to Existing Tables
**What goes wrong:** GORM auto-migrate adds new columns with their defaults, but existing rows don't get the defaults applied -- they get the database-level default (typically zero value).
**Why it happens:** GORM's AutoMigrate only adds columns; it doesn't run UPDATE statements for existing rows.
**How to avoid:** After adding new fields to the Share model (e.g., `AllowAuthSys`), add an explicit migration step that runs `UPDATE shares SET allow_auth_sys = true WHERE allow_auth_sys IS NULL OR allow_auth_sys = 0`. Do this in the store's `New()` function or a dedicated migration.
**Warning signs:** Existing shares silently lose AUTH_SYS access after upgrade because `AllowAuthSys` defaults to `false` (zero value for bool) instead of `true`.

### Pitfall 5: Kerberos Keytab Validation at Share Start
**What goes wrong:** Share with `require_kerberos=true` is created via API but the server has no Kerberos keytab configured.
**Why it happens:** Share creation doesn't check server-level Kerberos state.
**How to avoid:** The decision says "refuse to start share" (fail-fast). When loading shares at startup or creating via API, if `require_kerberos=true`, check that the NFS adapter's Kerberos is actually configured. If not, reject the share creation (API) or log error and skip (startup).
**Warning signs:** Share appears "created" in API response but clients get authentication errors.

### Pitfall 6: PATCH vs PUT Semantics for Settings
**What goes wrong:** PATCH update inadvertently resets unmentioned fields to defaults.
**Why it happens:** Using the same request struct for both PATCH and PUT without distinguishing "field not provided" from "field set to zero."
**How to avoid:** For PATCH requests, use pointer fields (`*int`, `*bool`, `*string`). `nil` means "not provided" (keep current value). For PUT, use value fields -- all fields required. The handler checks which method was used and applies the correct merge strategy.
**Warning signs:** `PATCH { "lease_time": 120 }` resets `grace_period` to default.

### Pitfall 7: Blocked Operations String Serialization
**What goes wrong:** Operation blocklist stored as comma-separated string in DB leads to awkward parsing and doesn't handle edge cases (empty string vs "no operations blocked").
**Why it happens:** Trying to avoid a join table for a simple list.
**How to avoid:** Use a join table (`adapter_blocked_operations`, `share_blocked_operations`) for clean queries and proper validation. Alternatively, store as JSON array in text field with explicit empty array `[]` meaning "none blocked". The existing `Config` JSON blob pattern is a precedent.
**Warning signs:** Empty string interpreted differently than null, or trailing commas causing parse errors.

## Code Examples

### Example 1: Adapter Settings API Handler
```go
// internal/controlplane/api/handlers/adapter_settings.go

type AdapterSettingsHandler struct {
    runtime *runtime.Runtime
}

// PatchNFSSettings handles PATCH /api/v1/adapters/nfs/settings
func (h *AdapterSettingsHandler) PatchNFSSettings(w http.ResponseWriter, r *http.Request) {
    var req PatchNFSSettingsRequest
    if !decodeJSONBody(w, r, &req) {
        return
    }

    // Check --force flag (bypasses range validation)
    force := r.URL.Query().Get("force") == "true"

    // Check --dry-run flag
    dryRun := r.URL.Query().Get("dry_run") == "true"

    // Get current settings
    current, err := h.runtime.Store().GetNFSAdapterSettings(r.Context())
    if err != nil {
        InternalServerError(w, "Failed to get current settings")
        return
    }

    // Apply partial updates (nil = keep current)
    if req.LeaseTime != nil {
        current.LeaseTime = *req.LeaseTime
    }
    // ... repeat for all fields

    // Validate
    if !force {
        if errs := validateNFSSettings(current); len(errs) > 0 {
            WriteValidationErrors(w, errs)
            return
        }
    }

    if dryRun {
        WriteJSONOK(w, nfsSettingsToResponse(current))
        return
    }

    // Save to DB (triggers hot-reload on next poll cycle)
    if err := h.runtime.Store().UpdateNFSAdapterSettings(r.Context(), current); err != nil {
        InternalServerError(w, "Failed to update settings")
        return
    }

    // Audit log
    logger.Info("NFS adapter settings updated",
        "changed_by", middleware.GetClaimsFromContext(r.Context()).Username,
        "settings", current)

    WriteJSONOK(w, nfsSettingsToResponse(current))
}
```

### Example 2: Netgroup Membership Check
```go
// pkg/controlplane/runtime/netgroups.go

// CheckNetgroupAccess checks if a client IP is allowed by the share's netgroup.
// Returns true if:
//   - Share has no netgroup (empty allowlist = allow all)
//   - Client IP matches any member in the share's netgroup
func (r *Runtime) CheckNetgroupAccess(ctx context.Context, shareName string, clientIP net.IP) (bool, error) {
    share, err := r.GetShare(shareName)
    if err != nil {
        return false, err
    }

    // No netgroup = allow all
    if share.NetgroupID == "" {
        return true, nil
    }

    // Get netgroup members from cache/DB
    members, err := r.getNetgroupMembers(ctx, share.NetgroupID)
    if err != nil {
        return false, err
    }

    for _, member := range members {
        switch member.Type {
        case "ip":
            if net.ParseIP(member.Value).Equal(clientIP) {
                return true, nil
            }
        case "cidr":
            _, network, err := net.ParseCIDR(member.Value)
            if err == nil && network.Contains(clientIP) {
                return true, nil
            }
        case "hostname":
            if matchHostname(clientIP, member.Value) {
                return true, nil
            }
        }
    }

    return false, nil
}
```

### Example 3: CLI Settings Show Command
```go
// cmd/dittofsctl/commands/adapter/settings.go

var settingsShowCmd = &cobra.Command{
    Use:   "show",
    Short: "Show adapter settings",
    RunE: func(cmd *cobra.Command, args []string) error {
        adapterType := args[0] // "nfs" or "smb"

        client, err := cmdutil.GetAuthenticatedClient()
        if err != nil {
            return err
        }

        settings, defaults, err := client.GetAdapterSettings(adapterType)
        if err != nil {
            return err
        }

        // Config-style grouped output with '*' for non-default values
        fmt.Println("# Version Negotiation")
        printSetting("min_version", settings.MinVersion, defaults.MinVersion)
        printSetting("max_version", settings.MaxVersion, defaults.MaxVersion)
        fmt.Println()
        fmt.Println("# Timeouts")
        printSetting("lease_time", settings.LeaseTime, defaults.LeaseTime)
        // ...

        return nil
    },
}

func printSetting(name string, value, defaultValue any) {
    marker := " "
    if fmt.Sprint(value) != fmt.Sprint(defaultValue) {
        marker = "*"
    }
    fmt.Printf("  %s %s = %v\n", marker, name, value)
}
```

### Example 4: Share Security Policy in CreateShareRequest
```go
// Extended CreateShareRequest in share handler

type CreateShareRequest struct {
    Name              string  `json:"name"`
    MetadataStoreID   string  `json:"metadata_store_id"`
    PayloadStoreID    string  `json:"payload_store_id"`
    ReadOnly          bool    `json:"read_only,omitempty"`
    DefaultPermission string  `json:"default_permission,omitempty"`

    // Security policy (new fields)
    AllowAuthSys      *bool   `json:"allow_auth_sys,omitempty"`       // default: true
    RequireKerberos   *bool   `json:"require_kerberos,omitempty"`     // default: false
    MinKerberosLevel  *string `json:"min_kerberos_level,omitempty"`   // default: "krb5"
    NetgroupID        *string `json:"netgroup_id,omitempty"`          // default: nil (allow all)
    BlockedOperations []string `json:"blocked_operations,omitempty"` // default: empty
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Config-file adapter definitions | API-managed adapters (DB) | Issue #120 | Adapters created/managed entirely through REST API |
| Single JSON blob for adapter config | Typed settings + JSON blob | This phase | Typed fields for validated settings, JSON blob for opaque extension |
| No share-level auth control | Per-share security policy | This phase | AUTH_SYS/Kerberos toggles per share |
| ShareAccessRule for IP control | Netgroups (reusable) | This phase | Named IP groups shared across shares |

**Deprecated/outdated:**
- Config-file adapter definitions are already deprecated (issue #120). This phase does not add config-file support for new settings.
- The `AdapterConfig.Config` JSON blob remains for backward compatibility but typed settings fields are authoritative for all new settings.

## Open Questions

1. **Settings Update Timestamp Granularity**
   - What we know: Polling every 10s uses `UpdatedAt` comparison to detect changes.
   - What's unclear: SQLite datetime precision is only seconds. Two rapid updates within the same second could be missed.
   - Recommendation: Use a monotonic version counter (integer) instead of timestamp for change detection. Increment on every update. This is cheap, unambiguous, and works with all DB backends.

2. **Netgroup DNS Cache TTL**
   - What we know: Reverse DNS lookups should be cached to avoid blocking.
   - What's unclear: Optimal TTL for the cache. Too short = too many DNS queries. Too long = stale entries.
   - Recommendation: Default 5-minute TTL with configurable override. Use a simple `sync.Map` with expiry timestamps. Negative results (lookup failure) cached for 1 minute.

3. **Operation Blocklist Validation**
   - What we know: NFS operations have well-defined names (READ, WRITE, LOOKUP, etc.). Invalid operation names should be rejected.
   - What's unclear: Exact list of valid operation names for NFSv4 and SMB.
   - Recommendation: Define constants for all valid NFS and SMB operations. Validate blocklist entries against these constants. Return per-field validation error for invalid operation names.

4. **PATCH Method Support in API Client**
   - What we know: The existing `pkg/apiclient/client.go` has `get`, `post`, `put`, `delete` methods but no `patch`.
   - What's unclear: N/A -- this is a gap that needs filling.
   - Recommendation: Add a `patch` method to `Client` following the same pattern as `put`. Simple addition.

## Sources

### Primary (HIGH confidence)
- `/Users/marmos91/Projects/dittofs/pkg/controlplane/models/adapter.go` - Existing AdapterConfig model (JSON blob pattern)
- `/Users/marmos91/Projects/dittofs/pkg/controlplane/models/share.go` - Existing Share model with squash, access rules
- `/Users/marmos91/Projects/dittofs/pkg/controlplane/models/permission.go` - SquashMode, SharePermission patterns
- `/Users/marmos91/Projects/dittofs/pkg/controlplane/store/interface.go` - Full Store interface (405 lines, 60+ methods)
- `/Users/marmos91/Projects/dittofs/pkg/controlplane/store/gorm.go` - GORMStore with SQLite/PostgreSQL support
- `/Users/marmos91/Projects/dittofs/pkg/controlplane/store/adapters.go` - Adapter CRUD operations, EnsureDefaultAdapters
- `/Users/marmos91/Projects/dittofs/pkg/controlplane/runtime/runtime.go` - Runtime with adapter management, settings watcher patterns
- `/Users/marmos91/Projects/dittofs/pkg/controlplane/api/router.go` - Chi router with middleware patterns
- `/Users/marmos91/Projects/dittofs/internal/controlplane/api/handlers/adapters.go` - Adapter handler pattern (Create/List/Get/Update/Delete)
- `/Users/marmos91/Projects/dittofs/internal/controlplane/api/handlers/shares.go` - Share handler pattern (Create/Update/Delete/Permissions)
- `/Users/marmos91/Projects/dittofs/internal/controlplane/api/handlers/problem.go` - RFC 7807 error response pattern
- `/Users/marmos91/Projects/dittofs/internal/controlplane/api/middleware/auth.go` - JWT auth, RequireAdmin, RequireRole middleware
- `/Users/marmos91/Projects/dittofs/pkg/adapter/nfs/nfs_adapter.go` - NFSConfig, NFSAdapter with timeouts, connection management
- `/Users/marmos91/Projects/dittofs/pkg/adapter/smb/config.go` - SMBConfig with credits, signing, timeouts
- `/Users/marmos91/Projects/dittofs/cmd/dittofs/commands/start.go` - createAdapterFactory showing config -> adapter bridge
- `/Users/marmos91/Projects/dittofs/pkg/apiclient/adapters.go` - API client adapter methods
- `/Users/marmos91/Projects/dittofs/pkg/apiclient/client.go` - API client HTTP methods (get/post/put/delete, no patch)
- `/Users/marmos91/Projects/dittofs/cmd/dittofsctl/commands/adapter/` - Existing adapter CLI commands
- `/Users/marmos91/Projects/dittofs/internal/protocol/nfs/v4/state/manager.go` - NFSv4 StateManager with lease, grace, delegation
- `/Users/marmos91/Projects/dittofs/test/e2e/adapters_test.go` - E2E test pattern with helpers

### Secondary (MEDIUM confidence)
- Runtime share.go grep results - Share and ShareConfig structs with squash, anonymous UID/GID
- CLAUDE.md files across submodules - Conventions, gotchas, testing patterns

### Tertiary (LOW confidence)
- None - all findings verified from codebase inspection

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in use in the codebase
- Architecture: HIGH - Patterns derived directly from existing codebase implementation
- Pitfalls: HIGH - Based on actual code patterns observed (GORM zero-value, atomic settings swap, etc.)
- API design: HIGH - Extends existing patterns (chi router, RFC 7807, handler patterns)
- Hot-reload: MEDIUM - Polling is straightforward but version-counter vs timestamp needs validation
- Netgroup DNS caching: MEDIUM - Standard pattern but TTL tuning is domain-specific

**Research date:** 2026-02-16
**Valid until:** 2026-03-16 (stable domain, codebase-specific research)
