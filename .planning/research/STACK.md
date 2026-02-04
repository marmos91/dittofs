# Stack Research: DittoFS Kubernetes Operator

**Domain:** Kubernetes Operator Development (Go)
**Researched:** 2026-02-04
**Confidence:** HIGH

## Executive Summary

The standard 2025/2026 stack for building production Kubernetes operators in Go is well-established and centers on the **Operator SDK** (which wraps **Kubebuilder** and **controller-runtime**). The ecosystem has matured significantly, with Kubernetes 1.30+ and controller-runtime v0.23+ providing stable foundations. Key shifts include the move to Go 1.24, adoption of CEL validation for CRDs, and replacement of kube-rbac-proxy with controller-runtime's native authn/authz.

For DittoFS specifically, the operator will need to orchestrate multiple sub-operators (Percona for PostgreSQL), manage PVCs, and expose TCP services (NFS/SMB) - all well-supported patterns in the current ecosystem.

---

## Recommended Stack

### Core Framework

| Technology | Version | Purpose | Why Recommended | Confidence |
|------------|---------|---------|-----------------|------------|
| Operator SDK | v1.42.0 | Operator scaffolding, OLM integration | Industry standard, wraps Kubebuilder with OLM/OperatorHub integration. More polished UX than raw Kubebuilder. | HIGH |
| controller-runtime | v0.23.1 | Controller logic, reconciliation loops | Kubernetes-SIG maintained, powers all Go operators. Latest has subresource Apply support, generic webhooks. | HIGH |
| controller-gen | v0.18.0 | CRD/RBAC/DeepCopy generation | Required for API code generation. Integrated via `make manifests` and `make generate`. | HIGH |
| Go | 1.24+ | Language runtime | Operator SDK v1.41+ requires Go 1.24. Provides log/slog for structured logging. | HIGH |

### Kubernetes Client Libraries

| Library | Version | Purpose | Why Recommended | Confidence |
|---------|---------|---------|-----------------|------------|
| k8s.io/client-go | v0.33.x | Kubernetes API client | Required dependency. Tied to controller-runtime version. | HIGH |
| k8s.io/api | v0.33.x | Kubernetes API types | Core API types for Pods, Services, PVCs, etc. | HIGH |
| k8s.io/apimachinery | v0.33.x | API machinery utilities | Conditions helpers, runtime.Object, meta/v1 types. | HIGH |
| sigs.k8s.io/controller-tools | v0.18.0 | Code generation tools | controller-gen binary source. | HIGH |

### Testing Framework

| Library | Version | Purpose | Why Recommended | Confidence |
|---------|---------|---------|-----------------|------------|
| sigs.k8s.io/controller-runtime/pkg/envtest | (matches controller-runtime) | Integration testing | Spins up etcd + kube-apiserver without kubelet. Official recommendation for operator testing. | HIGH |
| github.com/onsi/ginkgo/v2 | v2.x (latest) | BDD test framework | Operator SDK scaffolded tests use Ginkgo. Good for async assertions. | HIGH |
| github.com/onsi/gomega | (latest) | Matcher library | Pairs with Ginkgo. `Eventually` for async assertions critical for controller tests. | HIGH |

### Observability

| Library | Version | Purpose | Why Recommended | Confidence |
|---------|---------|---------|-----------------|------------|
| sigs.k8s.io/controller-runtime/pkg/metrics | (matches controller-runtime) | Prometheus metrics | Built-in metrics server. Exposes controller reconciliation metrics. | HIGH |
| sigs.k8s.io/controller-runtime/pkg/log | (matches controller-runtime) | Structured logging | Use with Zap backend. Integrates with controller context. | HIGH |
| github.com/go-logr/zapr | v1.3.0 | Zap adapter for logr | Production-grade structured logging. | MEDIUM |

### Deployment & Packaging

| Tool | Version | Purpose | Why Recommended | Confidence |
|------|---------|---------|-----------------|------------|
| Kustomize | v5.x | Manifest management | Built into kubectl. Operator SDK uses for base/overlay structure. | HIGH |
| OLM (Operator Lifecycle Manager) | v1.x | Operator deployment/upgrades | Standard for production operator distribution. Handles dependencies, upgrades. | HIGH |
| Helm | v3.x | Alternative packaging | For non-OLM deployments. Can combine with Kustomize. | MEDIUM |

---

## Supporting Libraries

| Library | Version | Purpose | When to Use | Confidence |
|---------|---------|---------|-------------|------------|
| k8s.io/apimachinery/pkg/api/meta | v0.33.x | Condition helpers | `SetStatusCondition`, `FindStatusCondition` - use for CR status management | HIGH |
| sigs.k8s.io/controller-runtime/pkg/controller/controllerutil | (matches controller-runtime) | Finalizer helpers | `AddFinalizer`, `RemoveFinalizer`, `SetControllerReference` | HIGH |
| sigs.k8s.io/controller-runtime/pkg/predicate | (matches controller-runtime) | Event filtering | Filter reconcile triggers (generation changes, label selectors) | HIGH |
| github.com/percona/percona-postgresql-operator/pkg/apis | (check latest) | Percona PG CRD types | For creating/managing PostgreSQL clusters via Percona operator | MEDIUM |

---

## Installation

```bash
# Install Operator SDK CLI (macOS)
brew install operator-sdk

# Or download binary directly
export ARCH=$(case $(uname -m) in x86_64) echo -n amd64 ;; aarch64) echo -n arm64 ;; *) echo -n $(uname -m) ;; esac)
export OS=$(uname | awk '{print tolower($0)}')
export OPERATOR_SDK_DL_URL=https://github.com/operator-framework/operator-sdk/releases/download/v1.42.0
curl -LO ${OPERATOR_SDK_DL_URL}/operator-sdk_${OS}_${ARCH}
chmod +x operator-sdk_${OS}_${ARCH}
sudo mv operator-sdk_${OS}_${ARCH} /usr/local/bin/operator-sdk

# Verify
operator-sdk version

# Initialize new operator project
operator-sdk init --domain dittofs.io --repo github.com/yourusername/dittofs-operator

# Create API (CRD + controller)
operator-sdk create api --group storage --version v1alpha1 --kind DittoFS --resource --controller

# Install OLM (for testing)
operator-sdk olm install

# Go dependencies (automatically managed via go mod)
go mod tidy
```

---

## Alternatives Considered

| Category | Recommended | Alternative | When to Use Alternative |
|----------|-------------|-------------|-------------------------|
| Framework | Operator SDK | Kubebuilder (raw) | If you don't need OLM integration or want simpler setup. Operator SDK uses Kubebuilder internally. |
| Framework | Operator SDK | KUDO | For simpler, declarative-only operators. Not suitable for complex Go logic. |
| Framework | Operator SDK | Metacontroller | For simple webhook-based operators without Go. Not production-grade for complex needs. |
| Testing | envtest | kind (Kubernetes in Docker) | For full cluster testing including CNI/storage. Slower but more realistic. |
| Logging | controller-runtime/log + Zap | log/slog (Go stdlib) | For minimal dependencies. But Zap is already integrated and battle-tested. |
| Metrics | controller-runtime/metrics | Custom Prometheus client | Only if you need metrics outside controller-runtime's patterns. |

---

## What NOT to Use

| Avoid | Why | Use Instead | Confidence |
|-------|-----|-------------|------------|
| kube-rbac-proxy | Discontinued from Kubebuilder/Operator SDK (March 2025). GCR images unavailable. | controller-runtime's `WithAuthenticationAndAuthorization` feature | HIGH |
| gcr.io/kubebuilder/* images | GCR went away March 2025 | Use official quay.io or custom registry images | HIGH |
| Logrus | Maintenance mode. Not integrated with controller-runtime | log/slog or Zap via controller-runtime/pkg/log | MEDIUM |
| client-go rate limiter (default) | Disabled by default in recent controller-runtime. Previous behavior caused issues. | Set QPS 20, Burst 30 explicitly if you need old behavior | MEDIUM |
| OLM v0 concepts exclusively | OLM v1 available with simpler API, GitOps support | Adopt OLM v1 patterns where possible | MEDIUM |

---

## Stack Patterns by Variant

### If deploying via OLM (OperatorHub):

- Use `operator-sdk bundle create` for packaging
- Include `ClusterServiceVersion` manifest
- Follow OLM v1 patterns (simpler API, Helm/GitOps support)
- Use bundle format (not deprecated package manifests)

### If deploying via Helm/Kustomize (no OLM):

- Use `config/default` Kustomize base
- Add Helm chart wrapper in `helm/` directory
- Consider Kustomize + Helm inflation for environment customization
- Deploy CRDs separately from operator

### If managing sub-operators (e.g., Percona PostgreSQL):

- Import Percona CRD types as Go dependency
- Use `controllerutil.SetControllerReference` for owned resources
- Watch for Percona CR status changes in your reconciler
- Handle dependency ordering in OLM bundle

### If exposing TCP services (NFS/SMB):

- Use `LoadBalancer` or `NodePort` Services
- Consider MetalLB for bare-metal LoadBalancer
- For NFS: port 2049 (or custom), consider StatefulSet for stable network identity
- For REST API: standard ClusterIP + Ingress pattern

---

## Version Compatibility Matrix

| Operator SDK | controller-runtime | Go | Kubernetes (client) | Kubebuilder Base |
|--------------|-------------------|----|---------------------|------------------|
| v1.42.0 | v0.21.0 | 1.24 | v0.33.x | v4.6.0 |
| v1.41.1 | v0.21.0 | 1.24 | v0.33.x | v4.6.0 |
| v1.40.0 | v0.20.4 | 1.23 | v0.32.x | v4.5.2 |

**Note:** controller-runtime v0.23.1 is the latest release (January 2025), but Operator SDK v1.42.0 ships with v0.21.0. You can upgrade controller-runtime independently if needed.

---

## Key Configuration Snippets

### go.mod (core dependencies)

```go
module github.com/yourorg/dittofs-operator

go 1.24

require (
    k8s.io/api v0.33.2
    k8s.io/apimachinery v0.33.2
    k8s.io/client-go v0.33.2
    sigs.k8s.io/controller-runtime v0.21.0
)
```

### Makefile targets (auto-generated by Operator SDK)

```makefile
# Generate code (DeepCopy, runtime.Object)
generate:
    $(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

# Generate CRDs, RBAC, webhooks
manifests:
    $(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

# Run tests with envtest
test:
    KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out
```

### RBAC markers example

```go
//+kubebuilder:rbac:groups=storage.dittofs.io,resources=dittofses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=storage.dittofs.io,resources=dittofses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=storage.dittofs.io,resources=dittofses/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
```

### Status Conditions pattern

```go
import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/api/meta"
)

// DittoFSStatus defines the observed state of DittoFS
type DittoFSStatus struct {
    // Conditions represent the latest available observations of the DittoFS state
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    // Phase represents the current lifecycle phase
    Phase string `json:"phase,omitempty"`
}

// In reconciler:
meta.SetStatusCondition(&dittofs.Status.Conditions, metav1.Condition{
    Type:               "Ready",
    Status:             metav1.ConditionTrue,
    Reason:             "AllComponentsHealthy",
    Message:            "All DittoFS components are running",
    LastTransitionTime: metav1.Now(),
})
```

---

## Sources

### HIGH Confidence (Official Documentation)
- [Operator SDK Official Documentation](https://sdk.operatorframework.io/) - Tutorial, best practices, advanced topics
- [Operator SDK GitHub Releases](https://github.com/operator-framework/operator-sdk/releases) - v1.42.0 release notes, version requirements
- [controller-runtime GitHub Releases](https://github.com/kubernetes-sigs/controller-runtime/releases) - v0.23.1 release notes
- [Kubebuilder Book](https://book.kubebuilder.io/) - controller-gen CLI, RBAC markers, CRD generation
- [Operator SDK Best Practices](https://sdk.operatorframework.io/docs/best-practices/best-practices/) - Architecture, CRD management, security
- [Operator SDK Observability Best Practices](https://sdk.operatorframework.io/docs/best-practices/observability-best-practices/) - Metrics, conditions
- [Operator SDK Testing Documentation](https://sdk.operatorframework.io/docs/building-operators/golang/testing/) - envtest, Ginkgo patterns
- [OLM Documentation](https://olm.operatorframework.io/) - OLM v1 improvements, bundle format

### MEDIUM Confidence (Verified Community Sources)
- [Kubernetes Operators in 2025 Guide](https://outerbyte.com/kubernetes-operators-2025-guide/) - Best practices, patterns, 2025 trends
- [IBM Operator Sample Go](https://github.com/IBM/operator-sample-go) - Reference implementation with best practices
- [Go Ecosystem 2025 Trends](https://blog.jetbrains.com/go/2025/11/10/go-language-trends-ecosystem-2025/) - Go version trends, logging (log/slog)
- [Standardizing CRD Condition Metrics](https://sourcehawk.medium.com/kubernetes-operator-metrics-411ca81833ab) - Prometheus metrics patterns

### LOW Confidence (Needs Validation)
- Specific Percona PostgreSQL Operator API versions - verify against current Percona releases
- OLM v1 GA status - confirm current stability for production use

---

*Stack research for: DittoFS Kubernetes Operator*
*Researched: 2026-02-04*
