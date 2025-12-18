# dittofs-operator

A Kubernetes operator for managing DittoFS servers, providing a declarative way to deploy and manage distributed file systems in Kubernetes clusters.

## Description

The DittoFS Operator automates the deployment, configuration, and lifecycle management of DittoFS servers on Kubernetes. It provides a native Kubernetes API through Custom Resource Definitions (CRDs) to manage DittoFS instances, handling StatefulSets, Services, ConfigMaps, and persistent storage automatically.

### Key Features

- **Declarative Management**: Define your DittoFS server configuration as Kubernetes resources
- **Automatic Resource Management**: Handles StatefulSets, Services, and ConfigMaps automatically
- **Storage Configuration**: Supports customizable metadata and content storage sizes
- **NFS Protocol Support**: Exposes NFS endpoints for distributed file access
- **High Availability**: Support for multiple replicas with proper resource allocation
- **Status Tracking**: Real-time status updates with condition reporting
- **Flexible Configuration**: Customize resources, security contexts, and service types

## API Reference

### DittoServer

The `DittoServer` resource represents a managed DittoFS server instance.

#### Spec Fields

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `image` | string | Container image for DittoFS | Yes |
| `replicas` | int32 | Number of replicas (default: 1) | No |
| `storage.metadataSize` | string | Size of metadata storage (e.g., "10Gi") | Yes |
| `storage.contentSize` | string | Size of content storage (e.g., "100Gi") | No |
| `storage.storageClassName` | string | Storage class for PVCs | No |
| `resources` | ResourceRequirements | CPU/Memory limits and requests | No |
| `service.type` | string | Service type (ClusterIP, LoadBalancer, NodePort) | No |
| `service.annotations` | map[string]string | Service annotations | No |
| `nfsPort` | int32 | NFS port (default: 2049) | No |
| `securityContext` | SecurityContext | Container security context | No |
| `podSecurityContext` | PodSecurityContext | Pod security context | No |

#### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | Current phase (Pending, Running, Stopped) |
| `nfsEndpoint` | string | NFS endpoint for mounting |
| `availableReplicas` | int32 | Number of ready replicas |
| `conditions` | []Condition | Status conditions |

## Architecture

The DittoFS Operator manages the following Kubernetes resources for each DittoServer:

```
DittoServer (CR)
    │
    ├─→ ConfigMap (dittofs configuration)
    │
    ├─→ Service (NFS and metrics endpoints)
    │
    └─→ StatefulSet
         ├─→ Pod(s) running DittoFS
         └─→ PersistentVolumeClaims
              ├─→ metadata volume
              └─→ content volume (optional)
```

### Components

- **ConfigMap**: Contains the generated DittoFS configuration file
- **Service**: Exposes NFS (default: 2049) and metrics (9090) ports
- **StatefulSet**: Manages DittoFS pods with stable network identities
- **PersistentVolumeClaims**: Provides storage for metadata and optionally content

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/dittofs/dittofs-operator:tag
```

or using `kubectl`:

```sh
kubectl -k config/default/
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/dittofs/dittofs-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

Or create a custom DittoServer resource:

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: my-dittofs
  namespace: default
spec:
  image: ghcr.io/marmos91/dittofs:latest
  replicas: 1
  storage:
    metadataSize: "10Gi"
    contentSize: "100Gi"
    storageClassName: "standard"
  resources:
    limits:
      cpu: "2"
      memory: "4Gi"
    requests:
      cpu: "500m"
      memory: "1Gi"
  service:
    type: ClusterIP
```

Apply the resource:

```sh
kubectl apply -f my-dittofs.yaml
```

Check the status:

```sh
kubectl get dittoserver my-dittofs
kubectl describe dittoserver my-dittofs
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/dittofs/dittofs-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/dittofs/dittofs-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

### Running Tests

```sh
# Run all tests
make test
```

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

