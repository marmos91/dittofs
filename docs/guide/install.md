# Deployment

This guide covers running DittoFS beyond a local source build: package managers, Docker,
and the Kubernetes operator. For the quick local path (Nix / Homebrew / source build), see
the [README](../../README.md). For configuration details, see [CONFIGURATION.md](configuration.md).

DittoFS ships two binaries — `dfs` (the server daemon) and `dfsctl` (the REST client).
Most package managers install both.

## Default ports

| Port | Service |
|------|---------|
| `12049` | NFS |
| `12445` | SMB |
| `8080`  | Control-plane REST API (health checks, management) |
| `9090`  | Prometheus metrics |

DittoFS defaults to these **non-privileged** ports (>1024) so it starts without
root. The portmapper, when enabled, defaults to `10111`.

## Running on standard ports (production)

In production we recommend running on the **standard, well-known ports** so
clients connect with no special options:

| Service | Default (unprivileged) | Standard (production) |
|---------|------------------------|-----------------------|
| NFS | `12049` | `2049` |
| SMB | `12445` | `445` |
| Portmapper (rpcbind) | `10111` | `111` |

On standard ports, mount commands drop the `port=` / `mountport=` options and
NFSv3 clients can auto-discover via the portmapper:

```bash
# Non-standard (default)
sudo mount -t nfs -o vers=4.1,tcp,port=12049 server:/export /mnt/point
# Standard ports — no port option needed
sudo mount -t nfs -o vers=4.1 server:/export /mnt/point
```

Two things are required: **(1)** free the standard ports on the host, and
**(2)** let DittoFS bind a port below 1024.

### 1. Free the standard ports on the host

Ports 2049 / 445 / 111 are usually claimed by the OS's own NFS, SMB, and
RPC-bind services. Stop and disable them, or DittoFS cannot bind:

```bash
# Linux (systemd) — disable the kernel NFS server, Samba, and rpcbind
sudo systemctl disable --now nfs-server rpcbind rpcbind.socket smbd nmbd

# Confirm nothing else holds the ports
sudo ss -tulpn | grep -E ':(2049|445|111)\b'
```

```bash
# macOS — turn off built-in File Sharing (SMB) and nfsd
sudo nfsd stop && sudo nfsd disable
# Disable SMB file sharing in System Settings → General → Sharing, or:
sudo launchctl disable system/com.apple.smbd
```

### 2. Bind the privileged ports — by deployment mode

#### Single binary

Set the ports in `config.yaml` (or via `DITTOFS_*` env vars), then grant the
binary permission to bind low ports.

```yaml
adapters:
  nfs:
    port: 2049
    portmapper:
      enabled: true
      port: 111        # enables NFSv3 client auto-discovery
  smb:
    port: 445
```

```bash
# Equivalent env vars
export DITTOFS_ADAPTERS_NFS_PORT=2049
export DITTOFS_ADAPTERS_NFS_PORTMAPPER_ENABLED=true
export DITTOFS_ADAPTERS_NFS_PORTMAPPER_PORT=111
export DITTOFS_ADAPTERS_SMB_PORT=445
```

Binding a port below 1024 needs privilege. Either run as root, or — preferred —
grant just the bind capability so the process stays unprivileged:

```bash
# Grant the capability to the binary (no root at runtime)
sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/dfs

# Or, under systemd, add to the [Service] section:
#   AmbientCapabilities=CAP_NET_BIND_SERVICE
```

#### Docker

The process is root **inside** the container, so it can bind low ports there
directly. Set the adapter ports to the standard values and publish them 1:1:

```bash
docker run -d \
  -e DITTOFS_ADAPTERS_NFS_PORT=2049 \
  -e DITTOFS_ADAPTERS_NFS_PORTMAPPER_ENABLED=true \
  -e DITTOFS_ADAPTERS_NFS_PORTMAPPER_PORT=111 \
  -e DITTOFS_ADAPTERS_SMB_PORT=445 \
  -p 2049:2049 -p 445:445 -p 111:111/tcp -p 111:111/udp \
  -p 8080:8080 \
  marmos91c/dittofs:latest
```

> **Publish the same number on both sides.** NFSv3's portmapper advertises the
> port the server *listens* on, so a mismatched mapping like `-p 2049:12049`
> breaks v3 auto-discovery (it works for v4, which has no portmapper). Set the
> container to listen on the standard port and map `2049:2049`.

The Docker **host** must still not run its own `nfsd` / `smbd` / `rpcbind` on
those ports — see [step 1](#1-free-the-standard-ports-on-the-host).

#### Kubernetes (operator)

In-cluster there is no host service to disable, and the pod never needs to bind
a privileged port: set the adapter ports in the `DittoServer` CR and let the
operator's **Service** publish them. With a `LoadBalancer` (or `NodePort`)
service, external clients reach the standard ports through the load balancer.

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs
spec:
  nfs:
    port: 2049
  smb:
    enabled: true
    port: 445
  service:
    type: LoadBalancer   # publishes the adapter ports externally
```

The operator wires each adapter port into the Service. For NFSv3 portmapper
exposure (port 111) and the complete CR schema, see the
[Kubernetes operator](#kubernetes-operator) section and the chart under
`k8s/dittofs-operator/`.

## Package managers

### Debian / Ubuntu (APT)

```bash
curl -fsSL https://s3.cubbit.eu/dittofs-binaries/apt/dittofs.gpg.key \
  | gpg --dearmor --yes | sudo tee /usr/share/keyrings/dittofs.gpg > /dev/null
echo "deb [signed-by=/usr/share/keyrings/dittofs.gpg] https://s3.cubbit.eu/dittofs-binaries/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/dfs.list
sudo apt update && sudo apt install dfs
sudo systemctl enable --now dfs
```

### RHEL / Fedora (YUM)

```bash
sudo curl -fsSLo /etc/yum.repos.d/dfs.repo https://s3.cubbit.eu/dittofs-binaries/rpm/dfs.repo
sudo yum install dfs
sudo systemctl enable --now dfs
```

### Arch Linux

```bash
# Download the latest .pkg.tar.zst from GitHub Releases, then:
sudo pacman -U dfs_<version>_amd64.pkg.tar.zst
sudo systemctl enable --now dfs
```

### Windows (Scoop)

```powershell
scoop bucket add marmos91 https://github.com/marmos91/scoop-bucket
scoop install dfs       # server daemon
scoop install dfsctl    # client CLI
```

When installed via the system package managers, the server runs under systemd as the `dfs`
service. Set the admin password before the first start with the
`DITTOFS_ADMIN_INITIAL_PASSWORD` environment variable (see the
[README](../../README.md#first-run--admin-password)). Under systemd the server's stdout is
not a terminal, so an auto-generated password would **not** be shown or written to the
service log — pre-setting it is the only way to know the credential.

## Docker

Pre-built multi-architecture images (`linux/amd64`, `linux/arm64`) are published on Docker Hub.

### Single container

```bash
docker pull marmos91c/dittofs:latest

# Create a config file first
mkdir -p ~/.config/dittofs
docker run --rm -v ~/.config/dittofs:/config \
  marmos91c/dittofs:latest init --config /config/config.yaml

# Run the server (set the admin password via env var — you can't read interactive output)
docker run -d \
  --name dittofs \
  -p 12049:12049 \
  -p 12445:12445 \
  -p 8080:8080 \
  -p 9090:9090 \
  -e DITTOFS_ADMIN_INITIAL_PASSWORD=my-secure-password \
  -v ~/.config/dittofs/config.yaml:/config/config.yaml:ro \
  -v dittofs-metadata:/data/metadata \
  -v dittofs-content:/data/content \
  -v dittofs-cache:/data/cache \
  marmos91c/dittofs:latest

curl http://localhost:8080/health
# Always set DITTOFS_ADMIN_INITIAL_PASSWORD (above): the container's stdout is a pipe, so an
# auto-generated password is NOT printed to `docker logs` and cannot be recovered.
```

**Image tags:**

- `marmos91c/dittofs:latest` — latest stable release
- `marmos91c/dittofs:vX.Y.Z` — a specific version
- `marmos91c/dittofs:vX.Y` — latest patch of a minor version
- `marmos91c/dittofs:vX` — latest minor of a major version

### Docker Compose

The repository ships a `docker-compose.yml` with backend profiles:

```bash
docker compose up -d                              # local filesystem backend (default)
docker compose --profile s3-backend up -d         # S3 content via localstack
docker compose --profile postgres-backend up -d   # PostgreSQL metadata
docker compose logs -f dittofs
```

| Profile | Metadata | Content |
|---------|----------|---------|
| default | BadgerDB | local filesystem |
| `s3-backend` | BadgerDB | S3 (localstack) |
| `postgres-backend` | PostgreSQL | local filesystem |

Make sure your `config.yaml` matches the profile you start. For a Prometheus + Grafana
monitoring stack, enable the `monitoring` profile in the repository's `docker-compose.yml`.

## Kubernetes operator

DittoFS has an official operator that manages the deployment lifecycle, configuration via
Custom Resources, persistent volume claims for metadata and block stores, and service
exposure for the NFS/SMB protocols.

```bash
# From the operator directory
cd k8s/dittofs-operator
make deploy

# Create a DittoFS instance
kubectl apply -f config/samples/dittofs_v1alpha1_dittofs.yaml

# Check status
kubectl get dittofs
```

`make deploy` uses the published operator image (`marmos91c/dittofs-operator:latest`) by
default. To build and deploy from source instead, point `IMG` at your own registry:
`make docker-build docker-push IMG=<your-registry>/dittofs-operator:tag` then
`make deploy IMG=<your-registry>/dittofs-operator:tag`.

See the [`k8s/dittofs-operator/`](../k8s/dittofs-operator/) directory for the CRD reference,
RBAC, and Helm chart configuration.
