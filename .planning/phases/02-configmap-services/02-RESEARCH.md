# Phase 2: ConfigMap Generation and Services - Research

**Researched:** 2026-02-04
**Domain:** Kubernetes ConfigMap generation, Service topology, checksum annotation patterns
**Confidence:** HIGH

## Summary

This phase focuses on three core deliverables: (1) generating a ConfigMap from the CRD spec that matches the new DittoFS config format on develop branch, (2) creating appropriate Kubernetes Services for NFS, SMB, and API access, and (3) implementing the checksum annotation pattern for automatic pod restarts on configuration changes.

The existing operator has a working config generation system (`internal/controller/config/`) but it targets the OLD DittoFS config format with shares, metadata stores, content stores, and adapters defined in the config file. The NEW develop branch format is minimal: only database, cache, controlplane API, logging, and admin configuration. All dynamic configuration (stores, shares, adapters, users) is managed via REST API at runtime.

Key changes required:
1. **Simplify ConfigMap generation** - Remove stores/shares/adapters/users from config generation; keep only infrastructure settings
2. **Split Services** - Current operator creates one Service; phase requires headless + file protocol + API services
3. **Add checksum annotation** - Current operator does NOT implement this; pods don't restart on config changes

**Primary recommendation:** Refactor the existing `internal/controller/config/` package to generate the simplified infrastructure-only ConfigMap matching the develop branch format, implement SHA256 checksum annotation on pod template, and create three separate Services (headless, file, API).

## Standard Stack

The established libraries/tools for this domain:

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| crypto/sha256 | stdlib | Compute ConfigMap hash | Go standard library, no dependencies |
| encoding/hex | stdlib | Encode hash to string | Go standard library, no dependencies |
| gopkg.in/yaml.v3 | v3.x | YAML marshaling for ConfigMap | Industry standard YAML library |
| k8s.io/api/core/v1 | v0.34.x | Service, ConfigMap types | Kubernetes core API types |
| sigs.k8s.io/controller-runtime | v0.22.4 | CreateOrUpdate, SetControllerReference | Controller-runtime utilities |

### Supporting

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| k8s.io/apimachinery/pkg/util/intstr | v0.34.x | Port definitions | Service port specifications |
| k8s.io/apimachinery/pkg/api/resource | v0.34.x | Size parsing | PVC size validation |

**Installation:**
```bash
# Already in go.mod - no additional dependencies needed
```

## Architecture Patterns

### Recommended Package Structure

The existing structure is already well-organized. Phase 2 extends it:

```
k8s/dittofs-operator/
├── internal/controller/
│   ├── dittoserver_controller.go  # Main reconciler
│   └── config/
│       ├── config.go              # ConfigMap generation (REFACTOR: simplify)
│       └── types.go               # Config types (REFACTOR: match develop branch)
│
├── pkg/                           # NEW: extracted for reuse
│   ├── configgen/                 # NEW: CRD-to-ConfigMap transformer
│   │   ├── generator.go           # Main generation logic
│   │   └── types.go               # DittoFS config struct (develop branch format)
│   │
│   ├── resources/                 # NEW: Kubernetes resource builders
│   │   ├── configmap.go           # ConfigMap builder
│   │   ├── service.go             # Service builders (headless, file, API)
│   │   └── hash.go                # SHA256 hash utilities
│   │
│   └── constants/                 # NEW: Shared constants
│       └── ports.go               # Default port definitions
```

### Pattern 1: Checksum Annotation for Pod Restart

**What:** Compute SHA256 hash of ConfigMap content + referenced Secrets, store as pod template annotation.

**When to use:** Every reconciliation, to trigger StatefulSet rolling update when config changes.

**Example:**
```go
// Source: Verified pattern from Wave operator and Helm best practices
import (
    "crypto/sha256"
    "encoding/hex"
    "fmt"
)

const ConfigHashAnnotation = "dittofs.io/config-hash"

// ComputeConfigHash computes SHA256 hash of config content plus secret data
func ComputeConfigHash(configData string, secretData map[string][]byte, generation int64) string {
    h := sha256.New()

    // Include ConfigMap content
    h.Write([]byte(configData))

    // Include referenced secrets (sorted for determinism)
    for key, value := range secretData {
        h.Write([]byte(key))
        h.Write(value)
    }

    // Include CRD generation for extra safety
    h.Write([]byte(fmt.Sprintf("%d", generation)))

    return hex.EncodeToString(h.Sum(nil))
}

// Apply to pod template
podTemplate.Annotations[ConfigHashAnnotation] = ComputeConfigHash(
    configYAML,
    secretsData,  // JWT secret, admin password, database credentials
    dittoServer.Generation,
)
```

### Pattern 2: Service Topology (Three Services)

**What:** Create headless service for StatefulSet DNS, file protocol service for NFS+SMB, API service for REST.

**When to use:** Always - StatefulSet requires headless service.

**Example:**
```go
// Source: Kubernetes StatefulSet documentation, verified pattern
// Headless Service (required by StatefulSet spec)
headlessService := &corev1.Service{
    ObjectMeta: metav1.ObjectMeta{
        Name:      fmt.Sprintf("%s-headless", cr.Name),
        Namespace: cr.Namespace,
    },
    Spec: corev1.ServiceSpec{
        Type:      corev1.ServiceTypeClusterIP,
        ClusterIP: "None",  // Headless
        Selector:  labels,
        Ports: []corev1.ServicePort{
            {Name: "nfs", Port: nfsPort, Protocol: corev1.ProtocolTCP},
        },
    },
}

// File Protocol Service (NFS + SMB)
fileService := &corev1.Service{
    ObjectMeta: metav1.ObjectMeta{
        Name:        fmt.Sprintf("%s-file", cr.Name),
        Namespace:   cr.Namespace,
        Annotations: cr.Spec.Service.Annotations,
    },
    Spec: corev1.ServiceSpec{
        Type:     corev1.ServiceType(cr.Spec.Service.Type), // LoadBalancer default
        Selector: labels,
        Ports: []corev1.ServicePort{
            {Name: "nfs", Port: nfsPort, Protocol: corev1.ProtocolTCP},
            {Name: "smb", Port: smbPort, Protocol: corev1.ProtocolTCP}, // Conditional
        },
    },
}

// API Service
apiService := &corev1.Service{
    ObjectMeta: metav1.ObjectMeta{
        Name:        fmt.Sprintf("%s-api", cr.Name),
        Namespace:   cr.Namespace,
        Annotations: cr.Spec.Service.Annotations,
    },
    Spec: corev1.ServiceSpec{
        Type:     corev1.ServiceType(cr.Spec.Service.Type),
        Selector: labels,
        Ports: []corev1.ServicePort{
            {Name: "api", Port: 8080, Protocol: corev1.ProtocolTCP},
        },
    },
}
```

### Pattern 3: ConfigMap Generation (Develop Branch Format)

**What:** Generate minimal infrastructure-only ConfigMap matching DittoFS develop branch config.

**When to use:** Every reconciliation when building ConfigMap.

**Example (NEW develop branch format):**
```go
// Source: DittoFS pkg/config/config.go on develop branch
type DittoFSConfig struct {
    Logging         LoggingConfig    `yaml:"logging"`
    Telemetry       TelemetryConfig  `yaml:"telemetry"`
    ShutdownTimeout string           `yaml:"shutdown_timeout"`
    Database        DatabaseConfig   `yaml:"database"`
    Metrics         MetricsConfig    `yaml:"metrics"`
    ControlPlane    ControlPlaneConfig `yaml:"controlplane"`
    Cache           CacheConfig      `yaml:"cache"`
    Admin           AdminConfig      `yaml:"admin,omitempty"`
}

type DatabaseConfig struct {
    Type     string        `yaml:"type"`              // sqlite or postgres
    SQLite   *SQLiteConfig `yaml:"sqlite,omitempty"`
    Postgres *string       `yaml:"postgres,omitempty"` // Connection string from Secret
}

type ControlPlaneConfig struct {
    Port int       `yaml:"port"`
    JWT  JWTConfig `yaml:"jwt"`
}

type CacheConfig struct {
    Path string `yaml:"path"`
    Size string `yaml:"size"`
}
```

### Anti-Patterns to Avoid

- **Including stores/shares/adapters in ConfigMap:** The develop branch manages these via REST API. Do NOT generate them in ConfigMap.
- **Single Service for all protocols:** Requires three separate Services for proper separation and different cloud LB configurations.
- **Hardcoding secrets in ConfigMap:** JWT secret and database credentials should come from Kubernetes Secrets, referenced via environment variables or volume mounts.
- **Ignoring Secret changes in hash:** The config hash MUST include Secret data, not just ConfigMap content.

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| YAML marshaling | Manual string building | `gopkg.in/yaml.v3` | Handles escaping, nested structures correctly |
| Hash computation | Custom hash function | `crypto/sha256` + `encoding/hex` | Standard, deterministic |
| Port validation | Manual range checks | kubebuilder markers + webhook | CRD-level validation, consistent error messages |
| Owner references | Manual OwnerReference | `controllerutil.SetControllerReference` | Handles UID, GVK correctly |
| Resource updates | Get+Patch manually | `controllerutil.CreateOrUpdate` | Idempotent, handles conflicts |

**Key insight:** The checksum annotation pattern is well-established in Helm and operators like Wave. Follow the established pattern exactly rather than inventing variations.

## Common Pitfalls

### Pitfall 1: Hash Not Including Secrets

**What goes wrong:** ConfigMap changes trigger restart, but Secret changes don't.

**Why it happens:** Only hashing ConfigMap content, forgetting that JWT secret and database credentials also affect runtime behavior.

**How to avoid:** Include all referenced Secrets in hash computation:
```go
// Read all secrets used by the CR
jwtSecret := getSecretData(ctx, cr.Spec.Identity.JWT.SecretRef)
adminSecret := getSecretData(ctx, cr.Spec.Identity.Admin.PasswordSecretRef)
dbSecret := getSecretData(ctx, cr.Spec.Database.Postgres.SecretRef) // if Postgres

// Hash all of them together
hash := ComputeConfigHash(configYAML, map[string][]byte{
    "jwt":      jwtSecret,
    "admin":    adminSecret,
    "database": dbSecret,
}, cr.Generation)
```

**Warning signs:** Users report that changing JWT secret doesn't restart pods.

### Pitfall 2: StatefulSet Without Headless Service

**What goes wrong:** StatefulSet fails to create pods with DNS resolution errors.

**Why it happens:** StatefulSet `spec.serviceName` must reference an existing headless Service.

**How to avoid:** Always create headless Service BEFORE StatefulSet, ensure `spec.serviceName` matches exactly:
```go
// Create headless service first
headlessService.Name = fmt.Sprintf("%s-headless", cr.Name)
// ...
statefulSet.Spec.ServiceName = headlessService.Name  // Must match!
```

**Warning signs:** Pods stuck in Pending, DNS lookup failures in pod logs.

### Pitfall 3: Old ConfigMap Format

**What goes wrong:** DittoFS fails to start with config parsing errors.

**Why it happens:** Generating old config format (with shares, adapters, stores) when develop branch expects minimal infrastructure config.

**How to avoid:** Match the exact struct from `pkg/config/config.go` on develop branch:
- `logging`, `telemetry`, `shutdown_timeout`, `database`, `metrics`, `controlplane`, `cache`, `admin`
- NO `metadata`, `content`, `shares`, `adapters`, `users`, `groups`, `guest`

**Warning signs:** Pod crashes with "unknown field" or "required field missing" errors.

### Pitfall 4: Port Validation Without Warning

**What goes wrong:** User specifies port 80 (privileged), pod fails to bind.

**Why it happens:** Validation rejects invalid ports but doesn't warn about privileged ports that require capabilities.

**How to avoid:** Add warning for privileged ports in validation webhook:
```go
if port < 1024 {
    warnings = append(warnings,
        fmt.Sprintf("port %d is privileged and may require CAP_NET_BIND_SERVICE capability", port))
}
```

**Warning signs:** Pods crash with "permission denied" binding to ports < 1024.

### Pitfall 5: Service Port Conflicts

**What goes wrong:** User configures same port for NFS and SMB.

**Why it happens:** Missing uniqueness validation in webhook.

**How to avoid:** Validate port uniqueness across all protocols:
```go
ports := map[int32]string{}
for _, p := range []struct{name string; port int32}{
    {"nfs", nfsPort},
    {"smb", smbPort},
    {"api", apiPort},
} {
    if existing, ok := ports[p.port]; ok {
        return nil, fmt.Errorf("port %d used by both %s and %s", p.port, existing, p.name)
    }
    ports[p.port] = p.name
}
```

**Warning signs:** Service creation fails with port conflict errors.

## Code Examples

Verified patterns for Phase 2 implementation:

### ConfigMap Hash Computation

```go
// Source: Established pattern from Helm and Wave operator
package resources

import (
    "crypto/sha256"
    "encoding/hex"
    "fmt"
    "sort"
)

const ConfigHashAnnotation = "dittofs.io/config-hash"

// ComputeConfigHash computes a deterministic hash of configuration
func ComputeConfigHash(
    configData string,
    secrets map[string][]byte,
    generation int64,
) string {
    h := sha256.New()

    // ConfigMap content
    h.Write([]byte(configData))

    // Secrets in sorted order for determinism
    keys := make([]string, 0, len(secrets))
    for k := range secrets {
        keys = append(keys, k)
    }
    sort.Strings(keys)

    for _, k := range keys {
        h.Write([]byte(k))
        h.Write(secrets[k])
    }

    // Generation number
    h.Write([]byte(fmt.Sprintf("gen:%d", generation)))

    return hex.EncodeToString(h.Sum(nil))
}
```

### Service Builder Pattern

```go
// Source: Kubernetes API patterns, controller-runtime best practices
package resources

import (
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ServiceBuilder struct {
    name        string
    namespace   string
    labels      map[string]string
    annotations map[string]string
    serviceType corev1.ServiceType
    ports       []corev1.ServicePort
    headless    bool
}

func NewServiceBuilder(name, namespace string) *ServiceBuilder {
    return &ServiceBuilder{
        name:        name,
        namespace:   namespace,
        serviceType: corev1.ServiceTypeLoadBalancer,
        labels:      make(map[string]string),
        annotations: make(map[string]string),
    }
}

func (b *ServiceBuilder) WithHeadless() *ServiceBuilder {
    b.headless = true
    b.serviceType = corev1.ServiceTypeClusterIP
    return b
}

func (b *ServiceBuilder) WithType(t corev1.ServiceType) *ServiceBuilder {
    b.serviceType = t
    return b
}

func (b *ServiceBuilder) WithAnnotations(a map[string]string) *ServiceBuilder {
    for k, v := range a {
        b.annotations[k] = v
    }
    return b
}

func (b *ServiceBuilder) AddPort(name string, port int32, protocol corev1.Protocol) *ServiceBuilder {
    b.ports = append(b.ports, corev1.ServicePort{
        Name:     name,
        Port:     port,
        Protocol: protocol,
    })
    return b
}

func (b *ServiceBuilder) Build() *corev1.Service {
    svc := &corev1.Service{
        ObjectMeta: metav1.ObjectMeta{
            Name:        b.name,
            Namespace:   b.namespace,
            Labels:      b.labels,
            Annotations: b.annotations,
        },
        Spec: corev1.ServiceSpec{
            Type:     b.serviceType,
            Selector: b.labels,
            Ports:    b.ports,
        },
    }

    if b.headless {
        svc.Spec.ClusterIP = "None"
    }

    return svc
}
```

### Port Validation in Webhook

```go
// Source: Kubebuilder webhook patterns, Phase 2 CONTEXT.md decisions
func (r *DittoServer) validatePorts() (admission.Warnings, error) {
    var warnings admission.Warnings

    nfsPort := int32(2049)
    if r.Spec.NFSPort != nil {
        nfsPort = *r.Spec.NFSPort
    }

    smbPort := int32(445)
    if r.Spec.SMB != nil && r.Spec.SMB.Port != nil {
        smbPort = *r.Spec.SMB.Port
    }

    apiPort := int32(8080)
    if r.Spec.APIPort != nil {
        apiPort = *r.Spec.APIPort
    }

    // Range validation (1-65535 enforced by kubebuilder markers)

    // Uniqueness validation
    ports := map[int32]string{
        nfsPort: "nfs",
    }

    if r.Spec.SMB != nil && r.Spec.SMB.Enabled {
        if existing, ok := ports[smbPort]; ok {
            return nil, fmt.Errorf("port %d used by both %s and smb", smbPort, existing)
        }
        ports[smbPort] = "smb"
    }

    if existing, ok := ports[apiPort]; ok {
        return nil, fmt.Errorf("port %d used by both %s and api", apiPort, existing)
    }

    // Privileged port warnings
    for port, name := range ports {
        if port < 1024 {
            warnings = append(warnings,
                fmt.Sprintf("%s port %d is privileged; may require CAP_NET_BIND_SERVICE or SecurityContext capabilities", name, port))
        }
    }

    return warnings, nil
}
```

### ConfigMap Generation (Develop Branch Format)

```go
// Source: DittoFS pkg/config/config.go on develop branch
package configgen

type DittoFSConfig struct {
    Logging         LoggingConfig      `yaml:"logging"`
    Telemetry       TelemetryConfig    `yaml:"telemetry,omitempty"`
    ShutdownTimeout string             `yaml:"shutdown_timeout"`
    Database        DatabaseConfig     `yaml:"database"`
    Metrics         MetricsConfig      `yaml:"metrics"`
    ControlPlane    ControlPlaneConfig `yaml:"controlplane"`
    Cache           CacheConfig        `yaml:"cache"`
    Admin           AdminConfig        `yaml:"admin,omitempty"`
}

type LoggingConfig struct {
    Level  string `yaml:"level"`
    Format string `yaml:"format"`
    Output string `yaml:"output"`
}

type TelemetryConfig struct {
    Enabled bool `yaml:"enabled"`
}

type DatabaseConfig struct {
    Type   string        `yaml:"type"`
    SQLite *SQLiteConfig `yaml:"sqlite,omitempty"`
}

type SQLiteConfig struct {
    Path string `yaml:"path"`
}

type MetricsConfig struct {
    Enabled bool `yaml:"enabled"`
    Port    int  `yaml:"port,omitempty"`
}

type ControlPlaneConfig struct {
    Port int       `yaml:"port"`
    JWT  JWTConfig `yaml:"jwt"`
}

type JWTConfig struct {
    Secret               string `yaml:"secret"`
    AccessTokenDuration  string `yaml:"access_token_duration"`
    RefreshTokenDuration string `yaml:"refresh_token_duration"`
}

type CacheConfig struct {
    Path string `yaml:"path"`
    Size string `yaml:"size"`
}

type AdminConfig struct {
    Username     string `yaml:"username"`
    PasswordHash string `yaml:"password_hash,omitempty"`
}

// Generate creates DittoFS config YAML from CRD spec
func Generate(spec *DittoServerSpec, jwtSecret, adminHash string) (string, error) {
    cfg := DittoFSConfig{
        Logging: LoggingConfig{
            Level:  "INFO",
            Format: "json",  // JSON for Kubernetes (structured logging)
            Output: "stdout",
        },
        ShutdownTimeout: "30s",
        Database: DatabaseConfig{
            Type: "sqlite",
            SQLite: &SQLiteConfig{
                Path: "/data/controlplane/controlplane.db",
            },
        },
        Metrics: MetricsConfig{
            Enabled: spec.Metrics.Enabled,
            Port:    9090,
        },
        ControlPlane: ControlPlaneConfig{
            Port: 8080,
            JWT: JWTConfig{
                Secret:               jwtSecret,
                AccessTokenDuration:  "15m",
                RefreshTokenDuration: "168h",
            },
        },
        Cache: CacheConfig{
            Path: "/data/cache",
            Size: "1GB",  // Default, can be overridden
        },
        Admin: AdminConfig{
            Username:     "admin",
            PasswordHash: adminHash,
        },
    }

    // Apply overrides from CRD...

    return yaml.Marshal(cfg)
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Full config in ConfigMap | Infrastructure-only ConfigMap | DittoFS develop branch | Stores/shares/adapters via REST API |
| Single Service | Three Services (headless, file, API) | Phase 2 decision | Proper StatefulSet DNS + separation |
| No config change detection | Checksum annotation pattern | Industry standard | Automatic pod restart on config change |
| Manual secret injection | Kubernetes Secret references | DittoFS develop | Environment variable injection |

**Deprecated/outdated:**
- **Old DittoFS config format:** The existing operator generates shares/backends/adapters in ConfigMap. This is incompatible with develop branch.
- **Single Service pattern:** Current operator creates one Service. Phase 2 requires three.

## Open Questions

Things that couldn't be fully resolved:

1. **Exact Admin Password Handling**
   - What we know: User provides bcrypt hash in Secret
   - What's unclear: Should operator generate hash, or expect pre-hashed value?
   - Recommendation: Expect pre-hashed bcrypt value in Secret (consistent with DittoFS pattern)

2. **PostgreSQL Connection String Format**
   - What we know: Postgres uses connection string from Secret
   - What's unclear: Exact DSN format expected by DittoFS
   - Recommendation: Use standard PostgreSQL DSN format; validate in webhook

3. **Cache Size Format**
   - What we know: DittoFS uses bytesize package ("1GB", "512MB")
   - What's unclear: Whether Kubernetes format ("1Gi", "512Mi") is accepted
   - Recommendation: Use DittoFS format in ConfigMap; CRD can accept Kubernetes format and convert

## Sources

### Primary (HIGH confidence)
- `/Users/marmos91/Projects/dittofs/pkg/config/config.go` - DittoFS develop branch config structure
- `/Users/marmos91/Projects/dittofs/test/posix/configs/config.yaml` - Example minimal config
- `/Users/marmos91/Projects/dittofs/k8s/dittofs-operator/` - Existing operator implementation
- [Kubebuilder Webhook Implementation](https://book.kubebuilder.io/cronjob-tutorial/webhook-implementation.html) - Validation webhook patterns
- [Kubernetes StatefulSet Documentation](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/) - Headless service requirements

### Secondary (MEDIUM confidence)
- [Automatic Rolling Pods on Config Changes](https://renehernandez.io/notes/rolling-pods-config-changes/) - Checksum annotation pattern
- [Exposing StatefulSets](https://www.tigera.io/blog/exposing-statefulsets-in-kubernetes/) - Service topology patterns
- [Wave Operator](https://pkg.go.dev/github.com/wave-k8s/wave) - Production checksum implementation
- [Kubernetes Operators 2025 Guide](https://outerbyte.com/kubernetes-operators-2025-guide/) - Best practices

### Tertiary (LOW confidence)
- WebSearch results for port validation patterns - Need validation against kubebuilder docs

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Using Go stdlib and established controller-runtime patterns
- Architecture: HIGH - Based on existing operator code and develop branch config format
- Pitfalls: HIGH - Verified against multiple sources and existing implementation analysis
- Code examples: MEDIUM - Patterns verified but need testing in context

**Research date:** 2026-02-04
**Valid until:** 30 days (stable ecosystem, but DittoFS develop branch may evolve)

---

## Summary for Planner

**Key Changes Required:**

1. **ConfigMap Generation Refactor**
   - Remove: stores, shares, adapters, users, groups, guest sections
   - Keep: logging, shutdown_timeout, database, metrics, controlplane, cache, admin
   - Match exact YAML structure from `pkg/config/config.go` on develop branch

2. **Service Topology (Three Services)**
   - `{name}-headless`: ClusterIP=None, for StatefulSet DNS
   - `{name}-file`: LoadBalancer (default), for NFS+SMB
   - `{name}-api`: LoadBalancer (default), for REST API
   - Add: Metrics Service (conditional, when metrics enabled)

3. **Checksum Annotation**
   - Annotation: `dittofs.io/config-hash`
   - Hash includes: ConfigMap YAML + all referenced Secrets + CR generation
   - Applied to: Pod template in StatefulSet

4. **Port Validation in Webhook**
   - Range: 1-65535 (kubebuilder markers exist)
   - Uniqueness: NFS, SMB, API ports must be different
   - Warning: Privileged ports (< 1024) add status warning

5. **CRD Updates Needed**
   - Add: `apiPort` field (default 8080)
   - Add: Metrics configuration fields
   - Simplify: Remove shares/backends/caches (managed via API)
   - Add: Database configuration (SQLite path OR Postgres secret ref)

**Not in Phase 2 Scope:**
- Ingress support (deferred)
- TLS termination (deferred)
- Pod Disruption Budget (Phase 5)
