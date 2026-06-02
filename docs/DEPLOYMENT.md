# Deployment

This guide covers running DittoFS beyond a local source build: package managers, Docker,
and the Kubernetes operator. For the quick local path (Nix / Homebrew / source build), see
the [README](../README.md). For configuration details, see [CONFIGURATION.md](CONFIGURATION.md).

DittoFS ships two binaries — `dfs` (the server daemon) and `dfsctl` (the REST client).
Most package managers install both.

## Default ports

| Port | Service |
|------|---------|
| `12049` | NFS |
| `12445` | SMB |
| `8080`  | Control-plane REST API (health checks, management) |
| `9090`  | Prometheus metrics |

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
[README](../README.md#first-run--admin-password)); otherwise an auto-generated password is
written to the service log.

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
docker logs dittofs | head -20    # auto-generated admin password lands here if not pre-set
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
monitoring stack, see [`monitoring/README.md`](../monitoring/README.md).

## Kubernetes operator

DittoFS has an official operator that manages the deployment lifecycle, configuration via
Custom Resources, persistent volume claims for metadata and block stores, and service
exposure for the NFS/SMB protocols.

```bash
# From the operator directory
cd operator
make deploy

# Create a DittoFS instance
kubectl apply -f config/samples/dittofs_v1alpha1_dittofs.yaml

# Check status
kubectl get dittofs
```

See the [`operator/`](../operator/) directory for the CRD reference, RBAC, and Helm chart
configuration.
