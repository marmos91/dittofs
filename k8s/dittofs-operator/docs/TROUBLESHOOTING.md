# Troubleshooting Guide

This guide covers common issues with the DittoFS Kubernetes Operator and how to resolve them.

## Quick Diagnostics

Before diving into specific issues, run these commands to gather diagnostic information:

```bash
# Check DittoServer status
kubectl get dittoserver -A

# Check operator logs
kubectl logs -n dittofs deployment/dittofs-operator-controller-manager

# Check pod status
kubectl get pods -l app.kubernetes.io/name=dittofs

# Check events
kubectl get events --sort-by=.lastTimestamp
```

---

## Common Issues

### LoadBalancer External IP Pending

**Symptom:**

The Service shows `<pending>` in the EXTERNAL-IP column:

```bash
kubectl get svc dittofs-sample-file

# NAME                  TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)
# dittofs-sample-file   LoadBalancer   10.96.100.50    <pending>     2049:31234/TCP
```

**Cause:**

LoadBalancer Services require external infrastructure to assign IP addresses. This happens when:
- Running on bare-metal or local clusters (minikube, kind, k3d)
- Cloud controller is not running or misconfigured
- Cloud provider quota exceeded
- Network policy blocking LoadBalancer creation

**Solution:**

**Option 1: Use NodePort instead**

```yaml
spec:
  service:
    type: NodePort
```

Access via any node IP and the allocated NodePort.

**Option 2: Install MetalLB (bare-metal clusters)**

```bash
# Install MetalLB
kubectl apply -f https://raw.githubusercontent.com/metallb/metallb/v0.14.8/config/manifests/metallb-native.yaml

# Configure IP address pool (example for 192.168.1.240-250)
kubectl apply -f - <<EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: default-pool
  namespace: metallb-system
spec:
  addresses:
  - 192.168.1.240-192.168.1.250
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: default-l2
  namespace: metallb-system
EOF
```

**Option 3: Use port-forward for development**

```bash
kubectl port-forward svc/dittofs-sample-file 2049:2049
```

**Debug commands:**

```bash
# Check Service events
kubectl describe svc dittofs-sample-file

# Check cloud controller logs (cloud providers)
kubectl logs -n kube-system -l component=cloud-controller-manager

# Check MetalLB status (if installed)
kubectl get ipaddresspool -n metallb-system
kubectl logs -n metallb-system -l app=metallb
```

---

### PVC Stuck in Pending

**Symptom:**

StatefulSet pods are stuck in Pending, and PVCs are not bound:

```bash
kubectl get pvc

# NAME                            STATUS    VOLUME   CAPACITY   STORAGECLASS
# metadata-dittofs-sample-0       Pending                       standard
```

```bash
kubectl get pod dittofs-sample-0

# NAME               READY   STATUS    RESTARTS   AGE
# dittofs-sample-0   0/1     Pending   0          5m
```

**Cause:**

PVCs cannot bind when:
- No StorageClass exists or the specified one doesn't exist
- CSI driver not installed for the StorageClass
- No PersistentVolumes available (static provisioning)
- Capacity exceeded or quota limits reached
- Node affinity constraints don't match

**Solution:**

**Step 1: Check StorageClass existence**

```bash
kubectl get storageclass

# If empty or missing the expected class:
kubectl describe storageclass <name>
```

**Step 2: Verify CSI driver**

```bash
# List CSI drivers
kubectl get csidrivers

# Check CSI controller pods
kubectl get pods -n kube-system -l app=csi-controller
```

**Step 3: Check PVC events**

```bash
kubectl describe pvc metadata-dittofs-sample-0

# Look for events like:
# - "no persistent volumes available"
# - "storageclass not found"
# - "waiting for volume to be created"
```

**Step 4: Fix StorageClass reference**

```yaml
spec:
  storage:
    metadataSize: "10Gi"
    cacheSize: "5Gi"
    storageClassName: "standard"  # Use an existing StorageClass
```

**Debug commands:**

```bash
# List all StorageClasses
kubectl get storageclass

# Check PV availability (for static provisioning)
kubectl get pv

# Check storage capacity (CSI drivers that support it)
kubectl describe csistoragecapacities -A

# Check PVC events
kubectl describe pvc metadata-dittofs-sample-0
```

---

### Operator CrashLoopBackOff

**Symptom:**

The operator pod is in CrashLoopBackOff:

```bash
kubectl get pods -n dittofs

# NAME                                               READY   STATUS             RESTARTS
# dittofs-operator-controller-manager-xxx            0/1     CrashLoopBackOff   5
```

**Cause:**

Common reasons for operator crashes:
- CRD not installed before operator
- RBAC permissions insufficient
- Webhook certificate issues
- Missing environment variables
- Go panic from unhandled error

**Solution:**

**Step 1: Check operator logs**

```bash
kubectl logs -n dittofs deployment/dittofs-operator-controller-manager --previous
```

**Step 2: Verify CRD installed**

```bash
kubectl get crd dittoservers.dittofs.dittofs.com

# If not found, install CRDs first:
kubectl apply -f config/crd/bases/
```

**Step 3: Check RBAC**

```bash
kubectl auth can-i --as=system:serviceaccount:dittofs:dittofs-operator-controller-manager \
  list pods -n default

# If denied, check ClusterRole/RoleBinding
kubectl describe clusterrole dittofs-operator-manager-role
kubectl describe clusterrolebinding dittofs-operator-manager-rolebinding
```

**Step 4: Check webhook certificates**

```bash
# If using cert-manager
kubectl get certificate -n dittofs
kubectl describe certificate -n dittofs dittofs-operator-serving-cert

# Check webhook configuration
kubectl get validatingwebhookconfigurations dittofs-operator-validating-webhook-configuration
```

**Debug commands:**

```bash
# Full operator logs
kubectl logs -n dittofs deployment/dittofs-operator-controller-manager -f

# Previous container logs
kubectl logs -n dittofs deployment/dittofs-operator-controller-manager --previous

# Describe operator pod
kubectl describe pod -n dittofs -l control-plane=controller-manager

# Check events
kubectl get events -n dittofs --sort-by=.lastTimestamp
```

---

### DittoServer Stuck in Pending

**Symptom:**

DittoServer CR shows phase Pending for an extended period:

```bash
kubectl get dittoserver

# NAME              REPLICAS   READY   AVAILABLE   STATUS    AGE
# dittofs-sample    1          0       0           Pending   10m
```

**Cause:**

DittoServer remains in Pending when:
- PVCs are not bound
- Percona PostgreSQL not ready (when enabled)
- Image pull errors
- Insufficient node resources
- Init container failures

**Solution:**

**Step 1: Check DittoServer conditions**

```bash
kubectl get dittoserver dittofs-sample -o jsonpath='{.status.conditions}' | jq

# Look for:
# - ConfigReady: False
# - DatabaseReady: False (if Percona enabled)
# - Available: False
```

**Step 2: Check pod status**

```bash
kubectl get pods -l app.kubernetes.io/name=dittofs

kubectl describe pod dittofs-sample-0
```

**Step 3: Check init containers (Percona)**

```bash
# If Percona enabled, check init container
kubectl logs dittofs-sample-0 -c wait-for-postgres
```

**Step 4: Check PVC binding**

```bash
kubectl get pvc -l app.kubernetes.io/instance=dittofs-sample
```

**Debug commands:**

```bash
# Full DittoServer status
kubectl describe dittoserver dittofs-sample

# Pod events
kubectl get events --field-selector involvedObject.name=dittofs-sample-0

# Operator logs for this resource
kubectl logs -n dittofs deployment/dittofs-operator-controller-manager | grep dittofs-sample
```

---

### Percona CRD Not Found

**Symptom:**

Operator logs show error about PerconaPGCluster CRD:

```
Error: no matches for kind "PerconaPGCluster" in version "pgv2.percona.com/v2"
```

Or webhook validation fails with:

```
Percona integration requires Percona Operator to be installed (CRD perconapgclusters.pgv2.percona.com not found)
```

**Cause:**

The Percona Operator for PostgreSQL is not installed in the cluster, but `percona.enabled: true` is set in the DittoServer spec.

**Solution:**

**Step 1: Install Percona Operator**

```bash
# Using kubectl
kubectl apply --server-side -f https://raw.githubusercontent.com/percona/percona-postgresql-operator/v2.6.0/deploy/bundle.yaml

# Or using Helm
helm repo add percona https://percona.github.io/percona-helm-charts/
helm install percona-pg-operator percona/pg-operator --namespace percona-system --create-namespace
```

**Step 2: Verify CRD exists**

```bash
kubectl get crd | grep percona

# Expected:
# perconapgclusters.pgv2.percona.com         2026-02-05T10:00:00Z
```

**Step 3: Verify operator is running**

```bash
kubectl get pods -n percona-system
```

**Debug commands:**

```bash
# List all CRDs
kubectl get crd

# Check for Percona CRD specifically
kubectl get crd perconapgclusters.pgv2.percona.com

# Check Percona operator namespace
kubectl get pods -n percona-system
kubectl logs -n percona-system deployment/percona-postgresql-operator
```

---

### NFS Mount Fails

**Symptom:**

NFS mount command fails with various errors:

```bash
sudo mount -t nfs -o tcp,port=2049,mountport=2049 10.0.0.50:/export /mnt/dittofs

# Possible errors:
# mount.nfs: Connection refused
# mount.nfs: No route to host
# mount.nfs: access denied by server
# mount.nfs: Operation timed out
```

**Cause:**

- Service not exposed or wrong IP
- Firewall blocking NFS port
- Wrong port number
- Export path doesn't exist
- Network policy blocking traffic

**Solution:**

**Step 1: Verify Service endpoint**

```bash
# Get the correct IP/hostname
kubectl get svc dittofs-sample-file

# For LoadBalancer
EXTERNAL_IP=$(kubectl get svc dittofs-sample-file -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
echo $EXTERNAL_IP

# For NodePort
NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
NODE_PORT=$(kubectl get svc dittofs-sample-file -o jsonpath='{.spec.ports[?(@.name=="nfs")].nodePort}')
echo "$NODE_IP:$NODE_PORT"
```

**Step 2: Test connectivity**

```bash
# Check port is reachable
nc -zv $EXTERNAL_IP 2049

# Check NFS service responds
showmount -e $EXTERNAL_IP
```

**Step 3: Verify pod is running**

```bash
kubectl get pods -l app.kubernetes.io/name=dittofs

kubectl logs dittofs-sample-0 | grep -i "nfs\|listen"
```

**Step 4: Check NFS port configuration**

```yaml
spec:
  nfsPort: 2049  # Ensure this matches your mount command
```

**Debug commands:**

```bash
# Check all DittoFS services
kubectl get svc -l app.kubernetes.io/name=dittofs

# Test connectivity from inside cluster
kubectl run nfs-test --rm -it --image=busybox -- nc -zv dittofs-sample-file 2049

# Check firewall/network policies
kubectl get networkpolicy -A

# Check DittoFS logs for NFS server startup
kubectl logs dittofs-sample-0 | grep -i "nfs\|mount\|export"
```

---

### ConfigMap Not Updating Pod

**Symptom:**

Changes to DittoServer spec don't affect the running pod:

```bash
# After updating spec.cache.size from "1GB" to "2GB"
kubectl get pod dittofs-sample-0 -o yaml | grep -A5 "config-hash"

# Pod still has old config-hash annotation
```

**Cause:**

The pod should restart when configuration changes because the operator adds a config hash annotation. If this isn't happening:
- Operator not reconciling (check operator logs)
- No actual configuration change detected
- Operator error during reconciliation

**Solution:**

**Step 1: Check config hash annotation**

```bash
kubectl get pod dittofs-sample-0 -o jsonpath='{.metadata.annotations.dittofs\.io/config-hash}'

# Compare with expected hash
kubectl get configmap dittofs-sample-config -o yaml | md5sum
```

**Step 2: Check operator is reconciling**

```bash
kubectl logs -n dittofs deployment/dittofs-operator-controller-manager | grep dittofs-sample
```

**Step 3: Force pod restart manually**

```bash
kubectl delete pod dittofs-sample-0

# StatefulSet will recreate it with new config
```

**Step 4: Verify ConfigMap updated**

```bash
kubectl get configmap dittofs-sample-config -o yaml
```

**Debug commands:**

```bash
# Check StatefulSet spec
kubectl get statefulset dittofs-sample -o yaml | grep -A10 "annotations"

# Check ConfigMap content
kubectl get configmap dittofs-sample-config -o yaml

# View operator reconciliation
kubectl logs -n dittofs deployment/dittofs-operator-controller-manager -f | grep -i reconcile
```

---

### S3 Secret Not Found or Invalid

**Symptom:**

Webhook validation returns a warning about S3 Secret:

```
Warning: S3 credentials secret "s3-credentials" not found in namespace "default"
```

Or pod fails to start with secret mount error.

**Cause:**

- Secret doesn't exist
- Secret exists but in wrong namespace
- Secret missing required keys

**Solution:**

**Step 1: Verify Secret exists**

```bash
kubectl get secret s3-credentials -n <namespace>
```

**Step 2: Check Secret has required keys**

```bash
kubectl get secret s3-credentials -o jsonpath='{.data}' | jq 'keys'

# Required keys (default names):
# - accessKeyId
# - secretAccessKey
# - endpoint (optional)
```

**Step 3: Create Secret if missing**

```bash
kubectl create secret generic s3-credentials \
  --from-literal=accessKeyId=YOUR_ACCESS_KEY \
  --from-literal=secretAccessKey=YOUR_SECRET_KEY \
  --from-literal=endpoint=https://s3.cubbit.eu
```

**Debug commands:**

```bash
# List secrets in namespace
kubectl get secrets

# Describe specific secret
kubectl describe secret s3-credentials

# Check pod environment variables
kubectl exec dittofs-sample-0 -- env | grep AWS
```

---

### Init Container wait-for-postgres Timeout

**Symptom:**

Pod stuck with init container not completing:

```bash
kubectl get pod dittofs-sample-0

# NAME               READY   STATUS     RESTARTS   AGE
# dittofs-sample-0   0/1     Init:0/1   0          6m
```

```bash
kubectl logs dittofs-sample-0 -c wait-for-postgres

# Waiting for PostgreSQL at dittofs-sample-postgres:5432...
# PostgreSQL is not ready yet, retrying in 5 seconds... (attempt 60/60)
# Timeout waiting for PostgreSQL
```

**Cause:**

PostgreSQL (Percona) didn't become ready within the 5-minute timeout:
- PerconaPGCluster still initializing
- Percona Operator not running
- PostgreSQL pod has errors
- Network issue between pods

**Solution:**

**Step 1: Check PerconaPGCluster status**

```bash
kubectl get perconapgcluster

# NAME                      STATUS        AGE
# dittofs-sample-postgres   ready         10m   <-- Should be "ready"
```

**Step 2: Check Percona pods**

```bash
kubectl get pods -l postgres-operator.crunchydata.com/cluster=dittofs-sample-postgres
```

**Step 3: Check Percona Operator logs**

```bash
kubectl logs -n percona-system deployment/percona-postgresql-operator
```

**Step 4: Manually test PostgreSQL connectivity**

```bash
kubectl run pg-test --rm -it --image=postgres:16 -- \
  pg_isready -h dittofs-sample-postgres -p 5432
```

**Debug commands:**

```bash
# Watch PerconaPGCluster status
kubectl get perconapgcluster -w

# Check PostgreSQL pod logs
kubectl logs dittofs-sample-postgres-instance1-0

# Describe PostgreSQL pod
kubectl describe pod dittofs-sample-postgres-instance1-0

# Check credentials secret exists
kubectl get secret dittofs-sample-postgres-pguser-dittofs
```

---

## Useful Commands Reference

### Status and Logs

```bash
# DittoServer status
kubectl get dittoserver -o wide
kubectl describe dittoserver <name>

# Pod logs
kubectl logs <pod-name>
kubectl logs <pod-name> -c <container>
kubectl logs <pod-name> --previous

# Operator logs
kubectl logs -n dittofs deployment/dittofs-operator-controller-manager -f

# Events
kubectl get events --sort-by=.lastTimestamp
kubectl get events --field-selector involvedObject.name=<resource-name>
```

### Resource Inspection

```bash
# Get full YAML
kubectl get dittoserver <name> -o yaml
kubectl get statefulset <name> -o yaml
kubectl get configmap <name> -o yaml

# JSON path queries
kubectl get dittoserver <name> -o jsonpath='{.status.conditions}'
kubectl get pod <name> -o jsonpath='{.status.containerStatuses}'
```

### Network Debugging

```bash
# Port connectivity
nc -zv <ip> <port>
telnet <ip> <port>

# DNS resolution
kubectl run dns-test --rm -it --image=busybox -- nslookup <service-name>

# Service endpoints
kubectl get endpoints <service-name>
```

### Storage Debugging

```bash
# PVC status
kubectl get pvc
kubectl describe pvc <name>

# StorageClass
kubectl get storageclass
kubectl describe storageclass <name>

# PV binding
kubectl get pv
```
