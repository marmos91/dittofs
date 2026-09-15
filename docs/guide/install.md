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
  -v dittofs-blocks:/data/blocks \
  -v dittofs-state:/data/state \
  -v dittofs-cache:/data/cache \
  marmos91c/dittofs:latest

curl http://localhost:8080/health
# Always set DITTOFS_ADMIN_INITIAL_PASSWORD (above): the container's stdout is a pipe, so an
# auto-generated password is NOT printed to `docker logs` and cannot be recovered.
```

`dittofs-blocks` carries the journal, which holds the only copy of every byte that has not
reached the block store yet — never leave it in the container's writable layer. These
volumes only receive data if the config points at them: set `blockstore.journal.path` to
`/data/blocks` and `database.sqlite.path` to `/data/state/controlplane.db`, and give the
metadata store `/data/metadata` when you create it. The container starts with no stores
and no shares; create them with `dfsctl` (see the [CLI reference](cli.md)) or use the
Compose stack below, which does it for you.

**Image tags:**

- `marmos91c/dittofs:latest` — latest stable release
- `marmos91c/dittofs:vX.Y.Z` — a specific version
- `marmos91c/dittofs:vX.Y` — latest patch of a minor version
- `marmos91c/dittofs:vX` — latest minor of a major version

### Docker Compose

The repository ships a `docker-compose.yml` that brings up a server **and provisions it** —
a metadata store, a block store, one share `/export`, and the NFS and SMB adapters — so the
stack is usable the moment it is healthy. Clone the repository and run it from the root:

```bash
docker compose up -d                                       # BadgerDB + memory block store
COMPOSE_PROFILES=s3-backend docker compose up -d           # S3 content via Localstack
COMPOSE_PROFILES=postgres-backend docker compose up -d     # PostgreSQL metadata
docker compose logs -f dittofs bootstrap
```

Select the backend through `COMPOSE_PROFILES` rather than `--profile`: the bootstrap
service reads that variable to decide which stores to create, so `--profile` would start
the extra container without provisioning against it.

| Profile | Metadata | Block store |
|---------|----------|-------------|
| default | BadgerDB | `memory` |
| `s3-backend` | BadgerDB | `s3` (Localstack) |
| `postgres-backend` | PostgreSQL | `memory` |

There is no filesystem block store: a share's durable home is `s3` or `memory`, always
fronted by an on-disk journal. The journal, the metadata store and the control-plane
database each get a named volume, so nothing that has to survive a restart lives in a
container's writable layer.

**None of these profiles is durable storage**, `s3-backend` included — and what the journal
actually does is worth stating, because it is easy to mistake for a second copy. A write
lands in the journal first and the syncer copies it to the block store in the background.
The journal reclaims a segment's local bytes only when a write would push it past its size
ceiling, or when an operator drains it explicitly, and only for segments that are fully
synced. So an already-offloaded payload *may* still have a local copy — for exactly as long
as nothing has needed the space — and nothing guarantees it. Once the journal reclaims that
segment the bytes exist only in the block store.

Localstack holds its bucket for the life of its container, so `docker compose down`
followed by `up` comes back to an **empty bucket** while the metadata, which is on a volume,
still names the objects that were in it. That destroys every already-offloaded payload
outright; the journal is not a fallback for them, it has merely not been asked for the
space yet. The two `memory` profiles are the same story with the block store in RAM.
Localstack is an S3 stand-in for exercising the S3 path, not a place to keep bytes — point
the block store at a real bucket (`dfsctl store block add --type s3 …`) for anything you
care about.

The stack is built from the checkout rather than pulled, because the compose file and the
server have to agree on the store model and the published image trails the repository
between releases.

Once it is up:

```bash
# NFS (the share exports with root kept as root, so a root mount can write immediately)
sudo mount -t nfs -o vers=4.1,tcp,port=12049 localhost:/export /mnt/point

# SMB — the admin password is the one the stack was started with
smbclient //localhost/export -p 12445 -U admin
```

The stack ships fixed development credentials — the admin password defaults to
`dittofs-compose-dev-password`, the JWT secret to a fixed string — and exports the share
with root kept as root, so a client claiming root over NFS writes as root without a
password. Every published port is therefore bound to `127.0.0.1`; reaching the stack from
another host means removing that prefix in `docker-compose.yml`, and an unattended export
on a shared network is not what these defaults are for. Override
`DITTOFS_ADMIN_INITIAL_PASSWORD` and `DITTOFS_CONTROLPLANE_SECRET` first. Localstack and
PostgreSQL publish no host port at all, so the stack cannot collide with a PostgreSQL you
already run.

`docker compose down` stops the stack and keeps the volumes; `docker compose down -v`
discards them too. Running `up` again re-runs the provisioning, which skips whatever is
already there.

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
