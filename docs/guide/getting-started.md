# Getting Started

This guide takes you from zero to a mounted DittoFS share in a few minutes. You'll
install the two binaries, start the server, create a share, and mount it over NFS and
SMB.

- **`dfs`** — the server daemon (protocol adapters + control-plane REST API).
- **`dfsctl`** — the command-line client that manages users, shares, stores, and adapters
  on a running server.

> ⚠️ **Experimental, pre-1.0.** Not production ready, no security audit. Don't use it for
> data you can't afford to lose. See the [FAQ](faq.md) for known limitations.

## 1. Install

Pick one:

```bash
# Nix (runs without installing)
nix run github:marmos91/dittofs -- init
nix run github:marmos91/dittofs -- start

# Homebrew (macOS / Linux)
brew tap marmos91/tap
brew install marmos91/tap/dfs marmos91/tap/dfsctl

# Quick install script (macOS / Linux)
curl -fsSL https://github.com/marmos91/dittofs/releases/latest/download/install.sh | sh
```

Docker, the Kubernetes operator, APT/YUM/Arch packages, and Scoop (Windows) are in the
[Installation guide](install.md).

**Build from source** (needs Go 1.25+):

```bash
git clone https://github.com/marmos91/dittofs.git
cd dittofs
go build -o dfs    cmd/dfs/main.go
go build -o dfsctl cmd/dfsctl/main.go
```

## 2. Initialize and start the server

```bash
dfs init      # writes ~/.config/dittofs/config.yaml
dfs start
```

On first start DittoFS creates an `admin` user. **Pre-set the password** with
`DITTOFS_ADMIN_INITIAL_PASSWORD` — this is the recommended path for every deployment and
the only reliable one for Docker/Kubernetes/CI and systemd:

```bash
# Choose your own password (also skips the forced first-login password change)
DITTOFS_ADMIN_INITIAL_PASSWORD=my-secure-password dfs start
```

If you don't pre-set it, a random password is generated — but it is **shown only when you
run `dfs start --foreground` in an interactive terminal**, printed once to that terminal.
In background mode (the default `dfs start`), and under Docker/systemd (where stdout is a
pipe, not a terminal), the generated password is **never written anywhere and cannot be
recovered** — the log only notes that one was created. If you get locked out this way, set
`DITTOFS_ADMIN_INITIAL_PASSWORD` (or `admin.password_hash` in the config) and restart.

By default the server listens on these ports:

| Port    | Service |
|---------|---------|
| `12049` | NFS |
| `12445` | SMB |
| `8080`  | Control-plane REST API (login, management, health checks) |
| `9090`  | Prometheus metrics (opt-in) |

## 3. Log in and set the admin password

```bash
dfsctl login --server http://localhost:8080 --username admin
```

On first login you **must** change the admin password before any other command will
work — until you do, the rest are rejected with HTTP 403:

```bash
dfsctl user change-password
```

(If you set `DITTOFS_ADMIN_INITIAL_PASSWORD` yourself, this forced change is already
cleared. You can disable it entirely with `controlplane.require_initial_password_change:
false` — see [Configuration](configuration.md).)

## 4. Create a user

NFS write access maps to a host UID, so create a user bound to your current one:

```bash
dfsctl user create --username $(whoami) --host-uid
```

## 5. Create stores

A share is built from a **metadata store** (where file metadata lives) and a **block
store** (where file content lives, split into a fast local tier and a durable remote
tier). Not sure which to pick? See [Choosing stores](choosing-stores.md).

```bash
# Metadata: badger (durable, single-node default)
dfsctl store metadata add --name default --type badger

# Block: a local filesystem cache backed by a durable S3 remote
dfsctl store block local add  --name local-cache --type fs
dfsctl store block remote add --name s3-remote   --type s3
```

> **Want zero dependencies for a quick test?** Use `--type memory` for both the metadata
> store and the block stores instead. Everything is then in-RAM and ephemeral — perfect
> for a smoke test, useless for real data.

## 6. Create a share and grant access

```bash
dfsctl share create --name /export --metadata default \
  --local local-cache --remote s3-remote
dfsctl share permission grant /export --user $(whoami) --level read-write
```

## 7. Mount over NFS

```bash
dfsctl adapter enable nfs

# Linux
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs

# macOS
sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport,nolock localhost:/export /tmp/nfs

echo "Hello DittoFS!" > /mnt/nfs/hello.txt
```

Writes land in the local cache first and sync to S3 in the background. More mount
options, Kerberos, and NFS-over-TLS are in the [NFS guide](nfs.md).

## 8. Mount over SMB

SMB always requires user authentication:

```bash
dfsctl adapter enable smb
dfsctl user create --username alice          # password prompted
dfsctl share permission grant /export --user alice --level read-write

# Linux (use a credentials file — never put passwords on the command line)
sudo mount -t cifs //localhost/export /mnt/smb \
  -o port=12445,credentials=$HOME/.smbcredentials,vers=3.1.1

# macOS (prompts for the password)
mount -t smbfs //alice@localhost:12445/export /tmp/smb
```

Connecting from **Windows**? See [Windows clients](windows.md). Dialects, encryption, and
signing are in the [SMB guide](smb.md).

## Next steps

- [Configuration](configuration.md) — every config key and flag, with defaults.
- [CLI reference](cli.md) — every `dfs` and `dfsctl` command.
- [Choosing stores](choosing-stores.md) — metadata and block store trade-offs.
- [Identity: AD / LDAP / Kerberos](identity.md) — connect to a directory service.
- [Snapshots](snapshots.md), [Quotas](quotas.md), [Encryption](encryption.md).
- [Troubleshooting](troubleshooting.md) — when a mount or permission won't cooperate.
