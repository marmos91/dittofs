# Phase 3: Storage Management - Research

**Researched:** 2026-02-05
**Domain:** Kubernetes StatefulSet volumeClaimTemplates, PVC lifecycle, S3 credentials management
**Confidence:** HIGH

## Summary

This phase implements storage management for the DittoFS Kubernetes operator, focusing on three main areas: (1) VolumeClaimTemplates for persistent storage of metadata, payload, and cache data, (2) StorageClass validation to catch configuration errors early, and (3) S3 credentials handling via Secret references for Cubbit DS3 and other S3-compatible stores.

The existing operator already has a partial implementation with `VolumeClaimTemplates` for metadata and content volumes in the StatefulSet. The key gap is that the **cache volume currently uses EmptyDir** (ephemeral), which violates the WAL persistence requirement. Additionally, there's no StorageClass validation and no S3 credentials Secret reference support in the CRD.

Key changes required:
1. **Add cache VolumeClaimTemplate** - Cache WAL must persist across pod restarts for crash recovery
2. **Memory store detection** - Skip PVC creation when using memory stores (no persistence needed)
3. **StorageClass validation webhook** - Validate StorageClass exists before StatefulSet creation
4. **S3 credentials Secret reference** - Add CRD fields for S3 credentials Secret reference
5. **PVC retention policy** - Configure `persistentVolumeClaimRetentionPolicy` for proper cleanup

**Primary recommendation:** Convert cache from EmptyDir to VolumeClaimTemplate, add StorageClass validation in the existing webhook, and add S3 credentials Secret reference support in the CRD.

## Standard Stack

The established libraries/tools for this domain:

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| k8s.io/api/core/v1 | v0.34.x | PersistentVolumeClaim, Volume types | Kubernetes core API |
| k8s.io/api/storage/v1 | v0.34.x | StorageClass type | Required for validation |
| k8s.io/apimachinery/pkg/api/resource | v0.34.x | Quantity parsing (10Gi) | Standard resource parsing |
| sigs.k8s.io/controller-runtime | v0.22.4 | Client for StorageClass lookup | Already in use by operator |

### Supporting

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| k8s.io/apimachinery/pkg/types | v0.34.x | NamespacedName for client.Get | StorageClass lookup in webhook |

**Installation:**
```bash
# Already in go.mod - no additional dependencies needed
```

## Architecture Patterns

### Current Storage Implementation (Gap Analysis)

The existing operator creates these volumes:

| Volume | Mount Path | Current Type | Issue |
|--------|------------|--------------|-------|
| metadata | /data/metadata | VolumeClaimTemplate | OK |
| content | /data/content | VolumeClaimTemplate | OK (optional) |
| cache | /data/cache | **EmptyDir** | **PROBLEM: Not persistent** |
| config | /config | ConfigMap | OK |

The cache **must be a VolumeClaimTemplate** for WAL persistence across pod restarts.

### Recommended Storage Architecture

```
StatefulSet volumeClaimTemplates:
├── metadata (required, ReadWriteOnce)
│   └── /data/metadata - BadgerDB/PostgreSQL data
├── content (optional, ReadWriteOnce)
│   └── /data/content - Filesystem payload store
└── cache (required, ReadWriteOnce)
    └── /data/cache - WAL file for crash recovery

Volumes from ConfigMap:
└── config - config.yaml mounted at /config
```

### Pattern 1: Conditional VolumeClaimTemplate Generation

**What:** Generate VolumeClaimTemplates only when needed based on store type.

**When to use:** Always - avoids creating PVCs for memory-only configurations.

**Logic Matrix:**

| Use Case | Metadata PVC | Content PVC | Cache PVC |
|----------|--------------|-------------|-----------|
| Memory metadata + S3 payload | No | No | Yes |
| BadgerDB metadata + S3 payload | Yes | No | Yes |
| BadgerDB metadata + Filesystem payload | Yes | Yes | Yes |
| Memory metadata + Memory payload | No | No | Yes |

Cache PVC is **always required** because DittoFS requires WAL persistence for crash recovery.

**Example:**
```go
// Source: Kubernetes StatefulSet patterns
func buildVolumeClaimTemplates(spec *DittoServerSpec) []corev1.PersistentVolumeClaim {
    var templates []corev1.PersistentVolumeClaim

    // Metadata PVC - required for persistent metadata stores
    if needsMetadataPVC(spec) {
        templates = append(templates, corev1.PersistentVolumeClaim{
            ObjectMeta: metav1.ObjectMeta{Name: "metadata"},
            Spec: corev1.PersistentVolumeClaimSpec{
                AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
                StorageClassName: spec.Storage.StorageClassName,
                Resources: corev1.VolumeResourceRequirements{
                    Requests: corev1.ResourceList{
                        corev1.ResourceStorage: resource.MustParse(spec.Storage.MetadataSize),
                    },
                },
            },
        })
    }

    // Content PVC - only for filesystem payload store
    if spec.Storage.ContentSize != "" {
        templates = append(templates, corev1.PersistentVolumeClaim{
            ObjectMeta: metav1.ObjectMeta{Name: "content"},
            Spec: corev1.PersistentVolumeClaimSpec{
                AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
                StorageClassName: spec.Storage.StorageClassName,
                Resources: corev1.VolumeResourceRequirements{
                    Requests: corev1.ResourceList{
                        corev1.ResourceStorage: resource.MustParse(spec.Storage.ContentSize),
                    },
                },
            },
        })
    }

    // Cache PVC - ALWAYS required for WAL persistence
    templates = append(templates, corev1.PersistentVolumeClaim{
        ObjectMeta: metav1.ObjectMeta{Name: "cache"},
        Spec: corev1.PersistentVolumeClaimSpec{
            AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
            StorageClassName: spec.Storage.StorageClassName,
            Resources: corev1.VolumeResourceRequirements{
                Requests: corev1.ResourceList{
                    corev1.ResourceStorage: resource.MustParse(spec.Storage.CacheSize),
                },
            },
        },
    })

    return templates
}
```

### Pattern 2: StorageClass Validation in Webhook

**What:** Validate StorageClass exists before CR creation using client.Get in webhook.

**When to use:** When `storageClassName` is specified (skip for default StorageClass).

**Example:**
```go
// Source: kubebuilder webhook patterns + Kubernetes API
// In api/v1alpha1/dittoserver_webhook.go

// DittoServerValidator implements webhook.CustomValidator with client access
type DittoServerValidator struct {
    Client  client.Client
}

// Inject client via SetupWebhookWithManager
func SetupDittoServerWebhookWithManager(mgr ctrl.Manager) error {
    validator := &DittoServerValidator{
        Client: mgr.GetClient(),
    }
    return ctrl.NewWebhookManagedBy(mgr).
        For(&DittoServer{}).
        WithValidator(validator).
        Complete()
}

func (v *DittoServerValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
    ds := obj.(*DittoServer)
    var warnings admission.Warnings

    // Only validate if storageClassName is explicitly set
    if ds.Spec.Storage.StorageClassName != nil && *ds.Spec.Storage.StorageClassName != "" {
        storageClass := &storagev1.StorageClass{}
        err := v.Client.Get(ctx, types.NamespacedName{Name: *ds.Spec.Storage.StorageClassName}, storageClass)
        if err != nil {
            if apierrors.IsNotFound(err) {
                return warnings, fmt.Errorf("StorageClass %q does not exist", *ds.Spec.Storage.StorageClassName)
            }
            // Log warning but don't block - might be a transient error
            warnings = append(warnings,
                fmt.Sprintf("Could not verify StorageClass %q: %v", *ds.Spec.Storage.StorageClassName, err))
        }
    }

    return warnings, nil
}
```

### Pattern 3: S3 Credentials Secret Reference

**What:** Reference S3 credentials from a Kubernetes Secret for Cubbit DS3 or AWS S3.

**When to use:** When deploying with S3-compatible payload stores.

**CRD Schema:**
```go
// S3CredentialsSecretRef references a Secret containing S3 credentials
type S3CredentialsSecretRef struct {
    // Name of the Secret containing S3 credentials
    // +kubebuilder:validation:Required
    SecretName string `json:"secretName"`

    // Key in the Secret for the access key ID (default: "accessKeyId")
    // +kubebuilder:default="accessKeyId"
    AccessKeyIDKey string `json:"accessKeyIdKey,omitempty"`

    // Key in the Secret for the secret access key (default: "secretAccessKey")
    // +kubebuilder:default="secretAccessKey"
    SecretAccessKeyKey string `json:"secretAccessKeyKey,omitempty"`

    // Key in the Secret for the S3 endpoint (default: "endpoint")
    // For Cubbit DS3: https://s3.cubbit.eu or https://s3.[tenant].cubbit.eu
    // +kubebuilder:default="endpoint"
    EndpointKey string `json:"endpointKey,omitempty"`
}

// S3StoreConfig contains S3-compatible store configuration hints
// Actual store creation is done via REST API at runtime
type S3StoreConfig struct {
    // CredentialsSecretRef references a Secret containing S3 credentials
    // +optional
    CredentialsSecretRef *S3CredentialsSecretRef `json:"credentialsSecretRef,omitempty"`

    // Region for the S3 bucket (e.g., "eu-west-1")
    // For Cubbit DS3: use "eu-west-1"
    // +optional
    Region string `json:"region,omitempty"`

    // Bucket name
    // +optional
    Bucket string `json:"bucket,omitempty"`
}
```

**Secret Format:**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: dittofs-s3-credentials
type: Opaque
stringData:
  accessKeyId: "your-cubbit-access-key"
  secretAccessKey: "your-cubbit-secret-key"
  endpoint: "https://s3.cubbit.eu"  # or https://s3.[tenant].cubbit.eu
```

### Pattern 4: PVC Retention Policy

**What:** Configure `persistentVolumeClaimRetentionPolicy` for automatic PVC cleanup.

**When to use:** Always - prevents orphaned PVCs from accumulating.

**Example:**
```go
// Source: Kubernetes 1.27+ PVC retention feature
statefulSet.Spec = appsv1.StatefulSetSpec{
    // ... other fields ...
    PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
        WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType, // Keep data on deletion
        WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType, // Keep data on scale-down
    },
    VolumeClaimTemplates: templates,
}
```

**Policy Options:**

| whenDeleted | whenScaled | Use Case |
|-------------|------------|----------|
| Retain | Retain | **Default/Recommended** - Preserve data always |
| Delete | Retain | Delete on CR deletion, keep on scale-down |
| Retain | Delete | Keep on deletion, clean on scale-down |
| Delete | Delete | Full cleanup - for ephemeral workloads only |

For DittoFS, use **Retain/Retain** to prevent accidental data loss.

### Anti-Patterns to Avoid

- **EmptyDir for WAL cache:** The current implementation uses EmptyDir for cache - this loses WAL data on pod restart, breaking crash recovery.
- **Hardcoded StorageClass:** Always use the spec field; never hardcode a StorageClass name.
- **Missing StorageClass validation:** Let users know early if their StorageClass doesn't exist.
- **Inline S3 credentials in CRD:** Use Secret references, never store credentials directly in CR spec.
- **Delete/Delete retention policy:** Unless explicitly needed, always use Retain to prevent data loss.

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Resource quantity parsing | Manual string parsing | `resource.ParseQuantity()` | Handles Gi, Mi, G, M formats |
| StorageClass existence check | Manual API calls | `client.Get()` with StorageClass type | Standard controller-runtime pattern |
| PVC naming | Custom naming scheme | VolumeClaimTemplates | Kubernetes handles `{name}-{sts}-{ordinal}` |
| PVC cleanup | Manual deletion logic | `persistentVolumeClaimRetentionPolicy` | Built into StatefulSet since K8s 1.27 |

**Key insight:** StatefulSets with `volumeClaimTemplates` handle PVC creation automatically. The operator only needs to generate the template spec; Kubernetes handles naming, binding, and lifecycle.

## Common Pitfalls

### Pitfall 1: EmptyDir for WAL Cache

**What goes wrong:** Pod restart loses WAL data, corrupting any in-flight writes and breaking crash recovery.

**Why it happens:** EmptyDir seems simpler; developers may not realize WAL requires persistence.

**How to avoid:** Always use VolumeClaimTemplate for cache volume:
```go
// BAD: Current implementation
{
    Name: "cache",
    VolumeSource: corev1.VolumeSource{
        EmptyDir: &corev1.EmptyDirVolumeSource{},  // WRONG
    },
}

// GOOD: Add to VolumeClaimTemplates
corev1.PersistentVolumeClaim{
    ObjectMeta: metav1.ObjectMeta{Name: "cache"},
    Spec: corev1.PersistentVolumeClaimSpec{
        AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
        // ...
    },
}
```

**Warning signs:** Data loss after pod restart, "corrupt WAL" errors in logs.

### Pitfall 2: Missing CacheSize Field

**What goes wrong:** Users can't configure cache PVC size; operator uses hardcoded value.

**Why it happens:** StorageSpec only has MetadataSize and ContentSize fields.

**How to avoid:** Add CacheSize field to StorageSpec:
```go
type StorageSpec struct {
    MetadataSize string `json:"metadataSize"`
    ContentSize  string `json:"contentSize,omitempty"`
    CacheSize    string `json:"cacheSize"`  // ADD THIS
    // ...
}
```

**Warning signs:** Users asking how to configure cache storage size.

### Pitfall 3: VolumeClaimTemplates Immutability

**What goes wrong:** Trying to change VolumeClaimTemplates after StatefulSet creation fails.

**Why it happens:** Kubernetes prohibits changes to volumeClaimTemplates.

**How to avoid:**
- Document that storage changes require StatefulSet recreation
- Consider adding validation to reject changes to storage spec

**Warning signs:** "field is immutable" errors when updating CR.

### Pitfall 4: StorageClass Validation Blocking on Transient Errors

**What goes wrong:** Webhook rejects valid CRs due to temporary API server errors.

**Why it happens:** Using hard error for client.Get failures.

**How to avoid:** Return warning for transient errors, only error on NotFound:
```go
err := v.Client.Get(ctx, ...)
if err != nil {
    if apierrors.IsNotFound(err) {
        return nil, fmt.Errorf("StorageClass does not exist")  // Hard error
    }
    warnings = append(warnings, fmt.Sprintf("Could not verify StorageClass: %v", err))
    return warnings, nil  // Soft warning, allow creation
}
```

**Warning signs:** Intermittent CR creation failures.

### Pitfall 5: Missing S3 Secret Validation

**What goes wrong:** CR created but DittoFS fails at runtime due to missing S3 credentials.

**Why it happens:** Webhook doesn't validate that referenced Secret exists.

**How to avoid:** Validate Secret existence in webhook (with warning, not error - Secret might be created later):
```go
if spec.S3.CredentialsSecretRef != nil {
    secret := &corev1.Secret{}
    err := v.Client.Get(ctx, types.NamespacedName{
        Name:      spec.S3.CredentialsSecretRef.SecretName,
        Namespace: ds.Namespace,
    }, secret)
    if err != nil && apierrors.IsNotFound(err) {
        warnings = append(warnings,
            fmt.Sprintf("S3 credentials Secret %q not found - ensure it exists before DittoFS starts",
                spec.S3.CredentialsSecretRef.SecretName))
    }
}
```

**Warning signs:** Pod CrashLoopBackOff with "S3 credentials not found" in logs.

## Code Examples

Verified patterns for Phase 3 implementation:

### Complete VolumeClaimTemplates Builder

```go
// Source: Kubernetes StatefulSet API + existing operator patterns
func (r *DittoServerReconciler) buildVolumeClaimTemplates(
    spec *dittoiov1alpha1.StorageSpec,
) ([]corev1.PersistentVolumeClaim, error) {
    var templates []corev1.PersistentVolumeClaim

    // Metadata PVC
    metadataSize, err := resource.ParseQuantity(spec.MetadataSize)
    if err != nil {
        return nil, fmt.Errorf("invalid metadataSize: %w", err)
    }
    templates = append(templates, corev1.PersistentVolumeClaim{
        ObjectMeta: metav1.ObjectMeta{Name: "metadata"},
        Spec: corev1.PersistentVolumeClaimSpec{
            AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
            StorageClassName: spec.StorageClassName,
            Resources: corev1.VolumeResourceRequirements{
                Requests: corev1.ResourceList{
                    corev1.ResourceStorage: metadataSize,
                },
            },
        },
    })

    // Content PVC (optional)
    if spec.ContentSize != "" {
        contentSize, err := resource.ParseQuantity(spec.ContentSize)
        if err != nil {
            return nil, fmt.Errorf("invalid contentSize: %w", err)
        }
        templates = append(templates, corev1.PersistentVolumeClaim{
            ObjectMeta: metav1.ObjectMeta{Name: "content"},
            Spec: corev1.PersistentVolumeClaimSpec{
                AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
                StorageClassName: spec.StorageClassName,
                Resources: corev1.VolumeResourceRequirements{
                    Requests: corev1.ResourceList{
                        corev1.ResourceStorage: contentSize,
                    },
                },
            },
        })
    }

    // Cache PVC (ALWAYS required)
    cacheSize, err := resource.ParseQuantity(spec.CacheSize)
    if err != nil {
        return nil, fmt.Errorf("invalid cacheSize: %w", err)
    }
    templates = append(templates, corev1.PersistentVolumeClaim{
        ObjectMeta: metav1.ObjectMeta{Name: "cache"},
        Spec: corev1.PersistentVolumeClaimSpec{
            AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
            StorageClassName: spec.StorageClassName,
            Resources: corev1.VolumeResourceRequirements{
                Requests: corev1.ResourceList{
                    corev1.ResourceStorage: cacheSize,
                },
            },
        },
    })

    return templates, nil
}
```

### StorageClass Validation Webhook

```go
// Source: kubebuilder webhook patterns, controller-runtime client
package v1alpha1

import (
    "context"
    "fmt"

    storagev1 "k8s.io/api/storage/v1"
    apierrors "k8s.io/apimachinery/pkg/api/errors"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/types"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/webhook"
    "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// DittoServerValidator implements validation with cluster access
type DittoServerValidator struct {
    Client client.Client
}

var _ webhook.CustomValidator = &DittoServerValidator{}

// SetupDittoServerWebhookWithManager sets up the webhook with client injection
func SetupDittoServerWebhookWithManager(mgr ctrl.Manager) error {
    return ctrl.NewWebhookManagedBy(mgr).
        For(&DittoServer{}).
        WithValidator(&DittoServerValidator{Client: mgr.GetClient()}).
        Complete()
}

func (v *DittoServerValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
    ds := obj.(*DittoServer)
    return v.validateDittoServer(ctx, ds)
}

func (v *DittoServerValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
    ds := newObj.(*DittoServer)
    return v.validateDittoServer(ctx, ds)
}

func (v *DittoServerValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
    return nil, nil
}

func (v *DittoServerValidator) validateDittoServer(ctx context.Context, ds *DittoServer) (admission.Warnings, error) {
    var warnings admission.Warnings

    // Validate StorageClass if specified
    if ds.Spec.Storage.StorageClassName != nil && *ds.Spec.Storage.StorageClassName != "" {
        scName := *ds.Spec.Storage.StorageClassName
        storageClass := &storagev1.StorageClass{}
        err := v.Client.Get(ctx, types.NamespacedName{Name: scName}, storageClass)
        if err != nil {
            if apierrors.IsNotFound(err) {
                return warnings, fmt.Errorf("StorageClass %q does not exist in cluster", scName)
            }
            // Transient error - warn but allow
            warnings = append(warnings,
                fmt.Sprintf("Could not verify StorageClass %q exists: %v", scName, err))
        }
    }

    // Validate S3 Secret if configured
    if ds.Spec.S3 != nil && ds.Spec.S3.CredentialsSecretRef != nil {
        secretName := ds.Spec.S3.CredentialsSecretRef.SecretName
        secret := &corev1.Secret{}
        err := v.Client.Get(ctx, types.NamespacedName{
            Name:      secretName,
            Namespace: ds.Namespace,
        }, secret)
        if err != nil && apierrors.IsNotFound(err) {
            warnings = append(warnings,
                fmt.Sprintf("S3 credentials Secret %q not found; ensure it exists before DittoFS starts", secretName))
        }
    }

    // Include existing port validation
    portWarnings, err := ds.validatePorts()
    if err != nil {
        return warnings, err
    }
    warnings = append(warnings, portWarnings...)

    return warnings, nil
}
```

### Updated StorageSpec with CacheSize

```go
// Source: Phase 3 requirements
type StorageSpec struct {
    // Size for metadata store PVC (mounted at /data/metadata)
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
    MetadataSize string `json:"metadataSize"`

    // Size for content store PVC (mounted at /data/content)
    // Only needed for filesystem payload store
    // +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
    ContentSize string `json:"contentSize,omitempty"`

    // Size for cache PVC (mounted at /data/cache)
    // Required for WAL persistence - enables crash recovery
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
    // +kubebuilder:default="5Gi"
    CacheSize string `json:"cacheSize"`

    // StorageClass for all PVCs
    // If not specified, uses cluster default StorageClass
    StorageClassName *string `json:"storageClassName,omitempty"`
}
```

### S3 Credentials CRD Types

```go
// Source: AWS/Cubbit S3 patterns, External Secrets Operator patterns
// S3CredentialsSecretRef references a Secret containing S3-compatible credentials
type S3CredentialsSecretRef struct {
    // Name of the Secret in the same namespace
    // +kubebuilder:validation:Required
    SecretName string `json:"secretName"`

    // Key for access key ID (default: accessKeyId)
    // +kubebuilder:default="accessKeyId"
    AccessKeyIDKey string `json:"accessKeyIdKey,omitempty"`

    // Key for secret access key (default: secretAccessKey)
    // +kubebuilder:default="secretAccessKey"
    SecretAccessKeyKey string `json:"secretAccessKeyKey,omitempty"`

    // Key for S3 endpoint URL (default: endpoint)
    // For Cubbit DS3: https://s3.cubbit.eu
    // +kubebuilder:default="endpoint"
    EndpointKey string `json:"endpointKey,omitempty"`
}

// S3StoreConfig hints for S3-compatible payload stores
// Note: Actual store creation is done via REST API; this enables
// the operator to inject S3 env vars into the pod for SDK auth.
type S3StoreConfig struct {
    // CredentialsSecretRef references a Secret with S3 credentials
    CredentialsSecretRef *S3CredentialsSecretRef `json:"credentialsSecretRef,omitempty"`

    // Region for S3 bucket (e.g., "eu-west-1" for Cubbit)
    // +kubebuilder:default="eu-west-1"
    Region string `json:"region,omitempty"`

    // Bucket name (informational; actual config via REST API)
    Bucket string `json:"bucket,omitempty"`
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Manual PVC deletion | `persistentVolumeClaimRetentionPolicy` | K8s 1.27 (beta), 1.32 (GA on EKS) | Automatic cleanup, reduced orphaned resources |
| ReadWriteOnce | ReadWriteOncePod | K8s 1.22+ | Better pod isolation for storage |
| Always create all PVCs | Conditional based on store type | Current practice | Cost optimization for memory-only configs |
| Hardcoded EmptyDir | VolumeClaimTemplate for cache | **Phase 3** | WAL persistence, crash recovery |

**Deprecated/outdated:**
- **EmptyDir for stateful data:** Never use EmptyDir for data that must survive pod restarts.
- **Manual PVC cleanup:** Use retention policies instead of manual deletion.

## Open Questions

Things that couldn't be fully resolved:

1. **Memory-Only Configuration Detection**
   - What we know: Memory stores don't need PVCs (except cache WAL)
   - What's unclear: Should operator detect memory config from REST API or require explicit flag in CRD?
   - Recommendation: Always create metadata PVC (minimal cost), make contentSize optional (no PVC if empty)

2. **S3 Credentials Injection Method**
   - What we know: S3 credentials can be injected via env vars or mounted secret files
   - What's unclear: Which method does DittoFS expect?
   - Recommendation: Use env var injection (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_ENDPOINT_URL) - standard AWS SDK pattern

3. **StorageClass Provisioner Validation**
   - What we know: Can validate StorageClass exists
   - What's unclear: Should we also validate provisioner is running?
   - Recommendation: Only validate existence; provisioner issues will surface as PVC Pending state

## Sources

### Primary (HIGH confidence)
- [Kubernetes StatefulSet Documentation](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/) - VolumeClaimTemplates, persistence patterns
- [Kubernetes 1.27 StatefulSet PVC Auto-Deletion](https://kubernetes.io/blog/2023/05/04/kubernetes-1-27-statefulset-pvc-auto-deletion-beta/) - Retention policy feature
- `/Users/marmos91/Projects/dittofs/k8s/dittofs-operator/` - Existing operator implementation
- `/Users/marmos91/Projects/dittofs/pkg/config/config.go` - DittoFS cache configuration requirements
- [Kubebuilder Webhook Documentation](https://book.kubebuilder.io/reference/admission-webhook) - Webhook patterns

### Secondary (MEDIUM confidence)
- [Kubebuilder GitHub Issue #4218](https://github.com/kubernetes-sigs/kubebuilder/issues/4218) - Client access in webhooks
- [Cubbit DS3 Documentation](https://docs.cubbit.io/integrations/aws-cli) - S3 credential configuration
- [vcluster StatefulSet Best Practices](https://www.vcluster.com/blog/kubernetes-statefulset-examples-and-best-practices) - VolumeClaimTemplates patterns

### Tertiary (LOW confidence)
- WebSearch results for StorageClass validation patterns - Need validation against actual implementation

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Using established Kubernetes APIs already in the operator
- Architecture: HIGH - Based on existing operator code and Kubernetes StatefulSet docs
- Pitfalls: HIGH - Based on analysis of current EmptyDir issue and standard K8s patterns
- Code examples: MEDIUM - Patterns verified but need testing in context

**Research date:** 2026-02-05
**Valid until:** 60 days (stable Kubernetes APIs, mature patterns)

---

## Summary for Planner

**Key Changes Required:**

1. **Fix Cache Volume (Critical)**
   - Change cache from EmptyDir to VolumeClaimTemplate
   - Add `cacheSize` field to StorageSpec
   - Default cache size: 5Gi

2. **StorageSpec Updates**
   - Add `cacheSize` field (required)
   - Keep `metadataSize` (required)
   - Keep `contentSize` (optional - for filesystem payload)
   - Keep `storageClassName` (optional - uses default if not set)

3. **StorageClass Validation**
   - Inject client into webhook validator
   - Validate StorageClass exists when specified
   - Return hard error for NotFound, warning for transient errors

4. **S3 Credentials Support**
   - Add `S3StoreConfig` and `S3CredentialsSecretRef` types
   - Add `s3` field to DittoServerSpec
   - Include S3 Secret in config hash for pod restart
   - Inject credentials as environment variables in pod

5. **PVC Retention Policy**
   - Set `persistentVolumeClaimRetentionPolicy` to Retain/Retain
   - Document that storage changes require StatefulSet recreation

**Not in Phase 3 Scope:**
- Dynamic store type detection from REST API
- Multiple S3 stores per CR
- Cross-zone replication configuration
