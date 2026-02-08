# DittoFS Operator Installation Guide

This guide covers installing the DittoFS operator on your Kubernetes cluster.

## Prerequisites

Before installing the operator, ensure you have:

| Requirement | Minimum Version | Check Command |
|-------------|-----------------|---------------|
| Kubernetes cluster | 1.25+ | `kubectl version` |
| kubectl | 1.25+ | `kubectl version --client` |
| Helm (for Helm installation) | 3.0+ | `helm version` |
| StorageClass available | - | `kubectl get storageclass` |

**Optional prerequisites:**

- **Percona Operator** (for managed PostgreSQL): See [PERCONA.md](PERCONA.md) for installation
- **Cert-Manager** (for webhook TLS): Required if using admission webhooks

### Verify StorageClass

The operator creates PersistentVolumeClaims for DittoFS storage. Verify you have a StorageClass:

```bash
kubectl get storageclass

# Example output:
# NAME                 PROVISIONER             RECLAIMPOLICY   VOLUMEBINDINGMODE
# standard (default)   rancher.io/local-path   Delete          WaitForFirstConsumer
# fast-ssd             kubernetes.io/gce-pd    Delete          WaitForFirstConsumer
```

## Installation Methods

### Method 1: kubectl (Recommended)

The simplest installation method using raw Kubernetes manifests.

```mermaid
graph LR
    A[Apply CRD] --> B[Apply Operator]
    B --> C[Create DittoServer]
    C --> D[Operator Creates Resources]
```

**Step 1: Install the CRD**

```bash
kubectl apply -f https://raw.githubusercontent.com/marmos91/dittofs/main/k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml
```

**Step 2: Install the operator**

```bash
kubectl apply -f https://raw.githubusercontent.com/marmos91/dittofs/main/k8s/dittofs-operator/dist/install.yaml
```

The operator is installed in the `dittofs-system` namespace by default.

### Method 2: Helm

For more customization options, use the Helm chart.

**Option A: Install from local chart**

```bash
# Clone the repository
git clone https://github.com/marmos91/dittofs.git
cd dittofs/k8s/dittofs-operator

# Install with default values
helm install dittofs-operator ./chart \
  -n dittofs-system \
  --create-namespace

# Or with custom values
helm install dittofs-operator ./chart \
  -n dittofs-system \
  --create-namespace \
  --set controllerManager.manager.resources.limits.memory=256Mi \
  --set controllerManager.replicas=1
```

**Option B: Install with custom values file**

```bash
# Create custom values file
cat > my-values.yaml << 'EOF'
controllerManager:
  manager:
    image:
      repository: marmos91c/dittofs-operator
      tag: v0.1.0
    resources:
      limits:
        cpu: 500m
        memory: 256Mi
      requests:
        cpu: 100m
        memory: 128Mi
  replicas: 1
  nodeSelector:
    kubernetes.io/arch: amd64
EOF

# Install with custom values
helm install dittofs-operator ./chart \
  -n dittofs-system \
  --create-namespace \
  -f my-values.yaml
```

## Verify Installation

After installation, verify the operator is running:

```bash
# Check the operator pod
kubectl get pods -n dittofs-system

# Expected output:
# NAME                                           READY   STATUS    RESTARTS   AGE
# dittofs-operator-controller-manager-xxx-yyy    1/1     Running   0          30s

# Check the CRD is installed
kubectl get crd dittoservers.dittofs.dittofs.com

# Expected output:
# NAME                              CREATED AT
# dittoservers.dittofs.dittofs.com  2024-01-15T10:00:00Z

# Check operator logs
kubectl logs -n dittofs-system deployment/dittofs-operator-controller-manager
```

## Quick Start: Deploy Your First DittoServer

Once the operator is running, create a minimal DittoServer:

**Step 1: Create the DittoServer resource**

```bash
cat << 'EOF' | kubectl apply -f -
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: my-dittofs
spec:
  storage:
    metadataSize: "5Gi"
    cacheSize: "5Gi"
EOF
```

**Step 2: Watch the resources being created**

```bash
# Watch DittoServer status
kubectl get dittoserver my-dittofs -w

# Expected progression:
# NAME         REPLICAS   READY   AVAILABLE   STATUS    AGE
# my-dittofs   1          0       0           Pending   0s
# my-dittofs   1          1       0           Pending   10s
# my-dittofs   1          1       1           Running   30s
```

**Step 3: Verify all resources**

```bash
# Check created resources
kubectl get all -l app.kubernetes.io/instance=my-dittofs

# Check PVCs
kubectl get pvc -l app.kubernetes.io/instance=my-dittofs

# Check ConfigMap
kubectl get configmap my-dittofs-config -o yaml
```

**Step 4: Get the NFS endpoint**

```bash
kubectl get dittoserver my-dittofs -o jsonpath='{.status.nfsEndpoint}'
# Output: my-dittofs-file.default.svc.cluster.local:2049
```

## NFS Client Configuration

To mount the DittoFS share from another pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: nfs-client
spec:
  containers:
    - name: app
      image: busybox
      command: ["sleep", "infinity"]
      volumeMounts:
        - name: nfs-volume
          mountPath: /mnt/dittofs
  volumes:
    - name: nfs-volume
      nfs:
        server: my-dittofs-file.default.svc.cluster.local
        path: /
```

Or using a PersistentVolume:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: dittofs-pv
spec:
  capacity:
    storage: 100Gi
  accessModes:
    - ReadWriteMany
  nfs:
    server: my-dittofs-file.default.svc.cluster.local
    path: /
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: dittofs-pvc
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: 100Gi
  volumeName: dittofs-pv
```

## Uninstallation

### Uninstall DittoServer instances first

```bash
# Delete all DittoServer instances (this preserves PVCs by default)
kubectl delete dittoserver --all

# Or delete specific instance
kubectl delete dittoserver my-dittofs

# Optionally delete PVCs (WARNING: data loss)
kubectl delete pvc -l app.kubernetes.io/instance=my-dittofs
```

### Uninstall operator via kubectl

```bash
kubectl delete -f https://raw.githubusercontent.com/marmos91/dittofs/main/k8s/dittofs-operator/dist/install.yaml
kubectl delete -f https://raw.githubusercontent.com/marmos91/dittofs/main/k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml
```

### Uninstall operator via Helm

```bash
helm uninstall dittofs-operator -n dittofs-system

# Delete the namespace
kubectl delete namespace dittofs-system

# Delete CRD (Helm doesn't delete CRDs automatically)
kubectl delete crd dittoservers.dittofs.dittofs.com
```

## Upgrading

### Upgrade via kubectl

```bash
# Re-apply the latest manifests
kubectl apply -f https://raw.githubusercontent.com/marmos91/dittofs/main/k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml
kubectl apply -f https://raw.githubusercontent.com/marmos91/dittofs/main/k8s/dittofs-operator/dist/install.yaml
```

### Upgrade via Helm

```bash
# Update the chart
cd dittofs/k8s/dittofs-operator
git pull

# Upgrade the release
helm upgrade dittofs-operator ./chart -n dittofs-system

# IMPORTANT: Helm does not upgrade CRDs automatically!
# You must manually apply CRD updates:
kubectl apply -f config/crd/bases/dittofs.dittofs.com_dittoservers.yaml
```

## Configuration Reference

### Helm Chart Values

| Parameter | Description | Default |
|-----------|-------------|---------|
| `controllerManager.manager.image.repository` | Operator image repository | `marmos91c/dittofs-operator` |
| `controllerManager.manager.image.tag` | Operator image tag | `latest` |
| `controllerManager.manager.resources.limits.cpu` | CPU limit | `500m` |
| `controllerManager.manager.resources.limits.memory` | Memory limit | `128Mi` |
| `controllerManager.manager.resources.requests.cpu` | CPU request | `10m` |
| `controllerManager.manager.resources.requests.memory` | Memory request | `64Mi` |
| `controllerManager.replicas` | Number of operator replicas | `1` |
| `controllerManager.nodeSelector` | Node selector for operator pods | `{}` |
| `controllerManager.tolerations` | Tolerations for operator pods | `[]` |
| `serviceAccount.create` | Create service account | `true` |
| `serviceAccount.name` | Service account name | `""` (auto-generated) |

### Environment Variables

The operator supports the following environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `ENABLE_WEBHOOKS` | Enable admission webhooks | `true` |
| `METRICS_BIND_ADDRESS` | Metrics endpoint address | `:8443` |
| `HEALTH_PROBE_BIND_ADDRESS` | Health probe endpoint | `:8081` |

## Troubleshooting

### Operator pod not starting

```bash
# Check pod status
kubectl describe pod -n dittofs-system -l control-plane=controller-manager

# Check events
kubectl get events -n dittofs-system --sort-by='.lastTimestamp'
```

### DittoServer stuck in Pending

```bash
# Check conditions
kubectl get dittoserver my-dittofs -o jsonpath='{.status.conditions}' | jq

# Check operator logs
kubectl logs -n dittofs-system deployment/dittofs-operator-controller-manager

# Check if PVCs are bound
kubectl get pvc -l app.kubernetes.io/instance=my-dittofs
```

### Storage issues

```bash
# Verify StorageClass exists
kubectl get storageclass

# Check PVC events
kubectl describe pvc my-dittofs-metadata-my-dittofs-0

# Check if storage provisioner is running
kubectl get pods -n kube-system | grep provisioner
```

### Percona integration issues

```bash
# Verify Percona Operator is installed
kubectl get crd perconapgclusters.pgv2.percona.com

# Check PerconaPGCluster status
kubectl get perconapgcluster -l app.kubernetes.io/instance=my-dittofs

# Check Percona operator logs
kubectl logs -n percona-system deployment/percona-postgresql-operator
```

## Next Steps

- [CRD Reference](CRD_REFERENCE.md) - Complete API documentation
- [Percona Integration](PERCONA.md) - Set up managed PostgreSQL
- [DittoFS Documentation](https://github.com/marmos91/dittofs) - Main project docs

## See Also

- [Kubernetes Operators](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Helm Documentation](https://helm.sh/docs/)
- [Percona Operator](https://www.percona.com/doc/kubernetes-operator-for-postgresql/index.html)
