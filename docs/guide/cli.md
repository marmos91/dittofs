# CLI Reference

DittoFS ships two binaries:

- **`dfs`** — the server daemon. Runs the protocol adapters and the control-plane API; manages the local config file and the server process.
- **`dfsctl`** — the REST client. Talks to a running `dfs` over its control-plane API to manage users, groups, shares, stores, and adapters.

This page is generated from the command definitions (`go run ./cmd/gendocs`). Do not edit it by hand. Run `dfs <command> --help` or `dfsctl <command> --help` for the same content at the terminal.

## `dfs`

- [`dfs`](#dfs) — DittoFS - Modular virtual filesystem
  - [`dfs completion`](#dfs-completion) — Generate shell completion script
  - [`dfs config`](#dfs-config) — Configuration management
    - [`dfs config edit`](#dfs-config-edit) — Open configuration in editor
    - [`dfs config schema`](#dfs-config-schema) — Generate JSON schema for configuration
    - [`dfs config show`](#dfs-config-show) — Display current configuration
    - [`dfs config validate`](#dfs-config-validate) — Validate configuration file
  - [`dfs init`](#dfs-init) — Initialize a sample configuration file
  - [`dfs logs`](#dfs-logs) — Tail server logs
  - [`dfs migrate`](#dfs-migrate) — Run database migrations
  - [`dfs start`](#dfs-start) — Start the DittoFS server
  - [`dfs status`](#dfs-status) — Show server status
  - [`dfs stop`](#dfs-stop) — Stop the DittoFS server
  - [`dfs version`](#dfs-version) — Show version information


### `dfs`

DittoFS - Modular virtual filesystem

DittoFS is an experimental modular virtual filesystem that decouples
file interfaces from storage backends. It implements NFSv3 and SMB protocols
in pure Go (userspace, no FUSE required) with pluggable metadata and content stores.

Use "dfs [command] --help" for more information about a command.

Flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs completion`

Generate shell completion script

Generate shell completion script for dfs.

The generated script enables tab-completion for dfs commands, subcommands, and
flags in your shell. Pick the snippet for your shell and load it once.

```
dfs completion [bash|zsh|fish|powershell]
```

**Examples:**

```bash
# Bash (Linux): install system-wide
dfs completion bash > /etc/bash_completion.d/dfs

# Bash (macOS, requires the Homebrew bash-completion package)
dfs completion bash > $(brew --prefix)/etc/bash_completion.d/dfs

# Zsh: enable completion once (if not already enabled), then install
echo "autoload -U compinit; compinit" >> ~/.zshrc
dfs completion zsh > "${fpath[1]}/_dfs"

# Zsh (macOS, Homebrew)
dfs completion zsh > $(brew --prefix)/share/zsh/site-functions/_dfs

# Fish
dfs completion fish > ~/.config/fish/completions/dfs.fish

# PowerShell: load for the current session
dfs completion powershell | Out-String | Invoke-Expression

# PowerShell: persist across sessions by sourcing from your profile
dfs completion powershell > dfs.ps1
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config`

Configuration management

Manage DittoFS configuration files.

Use 'dfs init' to create a new configuration file.

Subcommands:

```
edit      Open configuration in editor
validate  Validate configuration file
show      Display current configuration
schema    Generate JSON schema for IDE/validation
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config edit`

Open configuration in editor

Open the configuration file in your default editor.

Uses the EDITOR environment variable, falling back to 'vi' if not set.

```
dfs config edit
```

**Examples:**

```bash
# Edit default config
dfs config edit

# Edit specific config file
dfs config edit --config /etc/dittofs/config.yaml
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config schema`

Generate JSON schema for configuration

Generate a JSON schema for the DittoFS configuration file.

The schema can be used for:

```
- IDE autocompletion (VS Code, IntelliJ, etc.)
- Configuration file validation
- Documentation generation
```

```
dfs config schema [flags]
```

**Examples:**

```bash
# Print schema to stdout
dfs config schema

# Save schema to file
dfs config schema --output config.schema.json
```

Flags:

```
  -o, --output string   Output file (default: stdout)
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config show`

Display current configuration

Display the current DittoFS configuration.

By default outputs YAML format. Use --output to change format.
Use --deduced to show auto-deduced block store defaults based on system resources.

```
dfs config show [flags]
```

**Examples:**

```bash
# Show default config as YAML
dfs config show

# Show as JSON
dfs config show --output json

# Show specific config file
dfs config show --config /etc/dittofs/config.yaml

# Show auto-deduced block store defaults
dfs config show --deduced
```

Flags:

```
      --deduced         Show auto-deduced block store defaults based on system resources
  -o, --output string   Output format (yaml|json) (default "yaml")
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config validate`

Validate configuration file

Validate the DittoFS configuration file.

Checks for syntax errors, missing required fields, and invalid values.

```
dfs config validate
```

**Examples:**

```bash
# Validate default config
dfs config validate

# Validate specific config file
dfs config validate --config /etc/dittofs/config.yaml
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs init`

Initialize a sample configuration file

Initialize a sample DittoFS server configuration file.

Creates a commented YAML configuration template at
$XDG_CONFIG_HOME/dittofs/config.yaml (typically ~/.config/dittofs/config.yaml).
The generated file includes a randomly generated JWT secret suitable for
development; replace it with a strong secret (or use DITTOFS_CONTROLPLANE_SECRET)
before deploying to production. Use --config to write to a non-default path.

```
dfs init [flags]
```

**Examples:**

```bash
# Create config at the default location
dfs init

# Create config at a custom path
dfs init --config /etc/dittofs/config.yaml

# Overwrite an existing config file
dfs init --force
```

Flags:

```
      --force   Force overwrite existing config file
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs logs`

Tail server logs

Display and optionally follow the DittoFS server log file.

Reads the log file path configured under logging.output in the active config
and prints the most recent lines. Pass -f to stream new entries as they are
written (similar to tail -f). The --since flag filters output to entries
timestamped at or after a given RFC3339 instant. Note: this command requires
logging.output to be a file path; it returns an error when the server is
configured to log to stdout or stderr.

```
dfs logs [flags]
```

**Examples:**

```bash
# Show the last 100 log lines (default)
dfs logs

# Show the last 50 lines
dfs logs -n 50

# Stream new log entries in real-time
dfs logs -f

# Show entries written since a specific time
dfs logs --since "2024-01-15T10:00:00Z"
```

Flags:

```
  -f, --follow         Follow log output
  -n, --lines int      Number of lines to show (default 100)
      --since string   Show logs since timestamp (RFC3339 format)
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs migrate`

Run database migrations

Run database migrations for the control plane database.

This command applies pending schema migrations to the configured control-plane
database (SQLite by default, or PostgreSQL). Run it once after upgrading DittoFS
to a new version that includes schema changes; it is safe to run multiple times.

```
dfs migrate
```

**Examples:**

```bash
# Run migrations with default config
dfs migrate

# Run migrations with a custom config file
dfs migrate --config /etc/dittofs/config.yaml
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs start`

Start the DittoFS server

Start the DittoFS server with the specified configuration.

By default, the server daemonizes into the background and writes its PID to
$XDG_STATE_HOME/dittofs/dittofs.pid. Use --foreground when running under a
process supervisor (systemd, Docker) or for interactive debugging. The NFS
adapter listens on port 12049 and the SMB adapter on port 12445 by default;
the control-plane REST API is available at http://localhost:8080.

```
dfs start [flags]
```

**Examples:**

```bash
# Start in background (daemon mode)
dfs start

# Start in foreground with debug logging
DITTOFS_LOGGING_LEVEL=DEBUG dfs start --foreground

# Start with a custom config file and explicit PID file path
dfs start --config /etc/dittofs/config.yaml --pid-file /var/run/dittofs.pid

# Set admin password via environment on first boot instead of the generated one
DITTOFS_ADMIN_INITIAL_PASSWORD=changeme dfs start --foreground
```

Flags:

```
  -f, --foreground        Run in foreground (default: background/daemon mode)
      --log-file string   Path to log file for daemon mode (default: $XDG_STATE_HOME/dittofs/dittofs.log)
      --pid-file string   Path to PID file (default: $XDG_STATE_HOME/dittofs/dittofs.pid)
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs status`

Show server status

Display the current status of the DittoFS server.

Checks the health endpoint at http://localhost:8080/health and reports whether
the server is running, how long it has been up, and whether the control-plane
database is reachable. When an API token is provided, per-entity status (shares,
adapters, stores) is also fetched and rendered as a color-coded table.

```
dfs status [flags]
```

**Examples:**

```bash
# Check status using default settings
dfs status

# Check status when the control-plane API runs on a non-default port
dfs status --api-port 9080

# Include per-entity (share, adapter, store) detail
dfs status --api-token <token>

# Emit machine-readable JSON output
dfs status --output json
```

Flags:

```
      --api-port int       API server port (default 8080)
      --api-token string   API token for per-entity status (or set DFS_API_TOKEN)
  -o, --output string      Output format (table|json|yaml) (default "table")
      --pid-file string    Path to PID file (default: $XDG_STATE_HOME/dittofs/dittofs.pid)
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs stop`

Stop the DittoFS server

Stop a running DittoFS server.

Sends SIGTERM to the server process identified by the PID file, which triggers
a graceful shutdown: in-flight NFS/SMB requests are drained, snapshot jobs are
flushed, and block stores are closed before the process exits. Use --force to
send SIGKILL instead when a graceful stop is not responding.

```
dfs stop [flags]
```

**Examples:**

```bash
# Graceful stop (reads default PID file)
dfs stop

# Stop using a custom PID file location
dfs stop --pid-file /var/run/dittofs.pid

# Immediately kill the server process
dfs stop --force
```

Flags:

```
  -f, --force             Force kill instead of graceful shutdown
      --pid-file string   Path to PID file (default: $XDG_STATE_HOME/dittofs/dittofs.pid)
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs version`

Show version information

Display the DittoFS server build version and system information.

Shows the full semantic version, git commit, build date, Go toolchain version,
and OS/architecture. Use --short to emit only the version string for scripting.

```
dfs version [flags]
```

**Examples:**

```bash
# Show full version information
dfs version

# Print only the version number (useful in scripts)
dfs version --short
```

Flags:

```
      --short   Show only version number
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

## `dfsctl`

- [`dfsctl`](#dfsctl) — DittoFS Control - Remote management client
  - [`dfsctl adapter`](#dfsctl-adapter) — Protocol adapter management
    - [`dfsctl adapter disable`](#dfsctl-adapter-disable) — Disable an adapter
    - [`dfsctl adapter edit`](#dfsctl-adapter-edit) — Edit an adapter
    - [`dfsctl adapter enable`](#dfsctl-adapter-enable) — Enable an adapter
    - [`dfsctl adapter list`](#dfsctl-adapter-list) — List protocol adapters
    - [`dfsctl adapter settings`](#dfsctl-adapter-settings) — Manage adapter settings
      - [`dfsctl adapter settings nfs`](#dfsctl-adapter-settings-nfs) — Manage NFS adapter settings
        - [`dfsctl adapter settings nfs reset`](#dfsctl-adapter-settings-nfs-reset) — Reset adapter settings to defaults
        - [`dfsctl adapter settings nfs show`](#dfsctl-adapter-settings-nfs-show) — Show current adapter settings
        - [`dfsctl adapter settings nfs update`](#dfsctl-adapter-settings-nfs-update) — Update adapter settings
      - [`dfsctl adapter settings smb`](#dfsctl-adapter-settings-smb) — Manage SMB adapter settings
        - [`dfsctl adapter settings smb reset`](#dfsctl-adapter-settings-smb-reset) — Reset adapter settings to defaults
        - [`dfsctl adapter settings smb show`](#dfsctl-adapter-settings-smb-show) — Show current adapter settings
        - [`dfsctl adapter settings smb update`](#dfsctl-adapter-settings-smb-update) — Update adapter settings
  - [`dfsctl bench`](#dfsctl-bench) — Run filesystem benchmarks
    - [`dfsctl bench compare`](#dfsctl-bench-compare) — Compare benchmark results from multiple systems
    - [`dfsctl bench run`](#dfsctl-bench-run) — Run filesystem benchmarks
    - [`dfsctl bench storage-tiers`](#dfsctl-bench-storage-tiers) — Benchmark storage tier performance (cold/warm/local-only)
  - [`dfsctl client`](#dfsctl-client) — Manage connected clients
    - [`dfsctl client disconnect`](#dfsctl-client-disconnect) — Disconnect a client
    - [`dfsctl client list`](#dfsctl-client-list) — List connected clients
    - [`dfsctl client sessions`](#dfsctl-client-sessions) — Manage NFS client sessions
      - [`dfsctl client sessions destroy`](#dfsctl-client-sessions-destroy) — Force-destroy a session
      - [`dfsctl client sessions list`](#dfsctl-client-sessions-list) — List sessions for a client
  - [`dfsctl completion`](#dfsctl-completion) — Generate shell completion script
  - [`dfsctl context`](#dfsctl-context) — Manage server contexts
    - [`dfsctl context current`](#dfsctl-context-current) — Show current context
    - [`dfsctl context list`](#dfsctl-context-list) — List all configured contexts
    - [`dfsctl context remove`](#dfsctl-context-remove) — Remove a context
    - [`dfsctl context rename`](#dfsctl-context-rename) — Rename a context
    - [`dfsctl context use`](#dfsctl-context-use) — Switch to a different context
  - [`dfsctl grace`](#dfsctl-grace) — Manage NFSv4 grace period
    - [`dfsctl grace end`](#dfsctl-grace-end) — Force-end the grace period
    - [`dfsctl grace status`](#dfsctl-grace-status) — Show grace period status
  - [`dfsctl group`](#dfsctl-group) — Group management
    - [`dfsctl group add-user`](#dfsctl-group-add-user) — Add a user to a group
    - [`dfsctl group create`](#dfsctl-group-create) — Create a new group
    - [`dfsctl group edit`](#dfsctl-group-edit) — Edit a group
    - [`dfsctl group get`](#dfsctl-group-get) — Get group details
    - [`dfsctl group list`](#dfsctl-group-list) — List all groups
    - [`dfsctl group remove`](#dfsctl-group-remove) — Remove a group
    - [`dfsctl group remove-user`](#dfsctl-group-remove-user) — Remove a user from a group
  - [`dfsctl identity-provider`](#dfsctl-identity-provider) — Identity provider (LDAP/AD, Kerberos) management
    - [`dfsctl identity-provider configure`](#dfsctl-identity-provider-configure) — Configure Kerberos machine-account settings
    - [`dfsctl identity-provider get`](#dfsctl-identity-provider-get) — Show an identity provider's configuration (secrets redacted)
    - [`dfsctl identity-provider list`](#dfsctl-identity-provider-list) — List identity providers and their state
    - [`dfsctl identity-provider set`](#dfsctl-identity-provider-set) — Create or replace an identity provider's configuration
    - [`dfsctl identity-provider test`](#dfsctl-identity-provider-test) — Test an identity provider's configuration without persisting it
  - [`dfsctl idmap`](#dfsctl-idmap) — Manage identity mappings
    - [`dfsctl idmap add`](#dfsctl-idmap-add) — Add an identity mapping
    - [`dfsctl idmap list`](#dfsctl-idmap-list) — List identity mappings
    - [`dfsctl idmap remove`](#dfsctl-idmap-remove) — Remove an identity mapping
    - [`dfsctl idmap sid`](#dfsctl-idmap-sid) — Manage foreign-SID UID/GID allocations
      - [`dfsctl idmap sid list`](#dfsctl-idmap-sid-list) — List foreign-SID UID/GID allocations
      - [`dfsctl idmap sid remove`](#dfsctl-idmap-sid-remove) — Remove a foreign-SID UID/GID allocation
  - [`dfsctl login`](#dfsctl-login) — Authenticate with DittoFS server
  - [`dfsctl logout`](#dfsctl-logout) — Clear stored credentials
  - [`dfsctl netgroup`](#dfsctl-netgroup) — Manage netgroups (IP access control)
    - [`dfsctl netgroup add-member`](#dfsctl-netgroup-add-member) — Add a member to a netgroup
    - [`dfsctl netgroup create`](#dfsctl-netgroup-create) — Create a new netgroup
    - [`dfsctl netgroup list`](#dfsctl-netgroup-list) — List all netgroups
    - [`dfsctl netgroup remove`](#dfsctl-netgroup-remove) — Remove a netgroup
    - [`dfsctl netgroup remove-member`](#dfsctl-netgroup-remove-member) — Remove a member from a netgroup
    - [`dfsctl netgroup show`](#dfsctl-netgroup-show) — Show netgroup details
  - [`dfsctl quota`](#dfsctl-quota) — Per-identity quota management
    - [`dfsctl quota list`](#dfsctl-quota-list) — List all quotas on a share
    - [`dfsctl quota remove`](#dfsctl-quota-remove) — Remove a per-identity quota
    - [`dfsctl quota set`](#dfsctl-quota-set) — Create or update a per-identity quota
  - [`dfsctl settings`](#dfsctl-settings) — Server settings management
    - [`dfsctl settings get`](#dfsctl-settings-get) — Get a setting value
    - [`dfsctl settings list`](#dfsctl-settings-list) — List all settings
    - [`dfsctl settings set`](#dfsctl-settings-set) — Set a setting value
  - [`dfsctl share`](#dfsctl-share) — Share management
    - [`dfsctl share create`](#dfsctl-share-create) — Create a new share
    - [`dfsctl share disable`](#dfsctl-share-disable) — Disable a share (drain clients, block new connections)
    - [`dfsctl share edit`](#dfsctl-share-edit) — Edit a share
    - [`dfsctl share enable`](#dfsctl-share-enable) — Enable a share (accept new connections)
    - [`dfsctl share list`](#dfsctl-share-list) — List all shares
    - [`dfsctl share list-mounts`](#dfsctl-share-list-mounts) — List mounted DittoFS shares
    - [`dfsctl share mount`](#dfsctl-share-mount) — Mount a share via NFS or SMB
    - [`dfsctl share nfs-config`](#dfsctl-share-nfs-config) — Manage per-share NFS adapter configuration
      - [`dfsctl share nfs-config set`](#dfsctl-share-nfs-config-set) — Update a share's NFS adapter configuration
      - [`dfsctl share nfs-config show`](#dfsctl-share-nfs-config-show) — Show a share's NFS adapter configuration
    - [`dfsctl share permission`](#dfsctl-share-permission) — Manage share permissions
      - [`dfsctl share permission grant`](#dfsctl-share-permission-grant) — Grant permission on a share
      - [`dfsctl share permission list`](#dfsctl-share-permission-list) — List permissions on a share
      - [`dfsctl share permission revoke`](#dfsctl-share-permission-revoke) — Revoke permission from a share
    - [`dfsctl share remove`](#dfsctl-share-remove) — Remove a share
    - [`dfsctl share show`](#dfsctl-share-show) — Show share details
    - [`dfsctl share snapshot`](#dfsctl-share-snapshot) — Manage share snapshots (create, list, show, remove, restore)
      - [`dfsctl share snapshot create`](#dfsctl-share-snapshot-create) — Create a snapshot of a share
      - [`dfsctl share snapshot list`](#dfsctl-share-snapshot-list) — List snapshots for a share
      - [`dfsctl share snapshot remove`](#dfsctl-share-snapshot-remove) — Remove a snapshot
      - [`dfsctl share snapshot restore`](#dfsctl-share-snapshot-restore) — Restore a snapshot into a (disabled) share
      - [`dfsctl share snapshot show`](#dfsctl-share-snapshot-show) — Show details of a snapshot
    - [`dfsctl share snapshot-policy`](#dfsctl-share-snapshot-policy) — Manage scheduled snapshot policies (schedule + retention)
      - [`dfsctl share snapshot-policy list`](#dfsctl-share-snapshot-policy-list) — List all snapshot policies
      - [`dfsctl share snapshot-policy remove`](#dfsctl-share-snapshot-policy-remove) — Remove a share's snapshot policy
      - [`dfsctl share snapshot-policy run`](#dfsctl-share-snapshot-policy-run) — Trigger a share's snapshot policy now (manual override)
      - [`dfsctl share snapshot-policy set`](#dfsctl-share-snapshot-policy-set) — Create or update a share's snapshot policy
      - [`dfsctl share snapshot-policy show`](#dfsctl-share-snapshot-policy-show) — Show a share's snapshot policy
    - [`dfsctl share unmount`](#dfsctl-share-unmount) — Unmount a mounted share
    - [`dfsctl share warm`](#dfsctl-share-warm) — Warm a share's local block cache
  - [`dfsctl status`](#dfsctl-status) — Show server status
  - [`dfsctl store`](#dfsctl-store) — Store management
    - [`dfsctl store block`](#dfsctl-store-block) — Block store management
      - [`dfsctl store block audit-refcounts`](#dfsctl-store-block-audit-refcounts) — Verify every manifest block reference has a backing FileChunk row
      - [`dfsctl store block evict`](#dfsctl-store-block-evict) — Evict block store data
      - [`dfsctl store block gc`](#dfsctl-store-block-gc) — Run garbage collection for a block store share
      - [`dfsctl store block gc-status`](#dfsctl-store-block-gc-status) — Show the last block-store GC run summary for a share
      - [`dfsctl store block health`](#dfsctl-store-block-health) — Check block store health
      - [`dfsctl store block local`](#dfsctl-store-block-local) — Local block store management
        - [`dfsctl store block local add`](#dfsctl-store-block-local-add) — Add a local block store
        - [`dfsctl store block local edit`](#dfsctl-store-block-local-edit) — Edit a local block store
        - [`dfsctl store block local list`](#dfsctl-store-block-local-list) — List local block stores
        - [`dfsctl store block local remove`](#dfsctl-store-block-local-remove) — Remove a local block store
      - [`dfsctl store block reconcile`](#dfsctl-store-block-reconcile) — Report orphaned block storage (read-only; no deletes)
      - [`dfsctl store block remote`](#dfsctl-store-block-remote) — Remote block store management
        - [`dfsctl store block remote add`](#dfsctl-store-block-remote-add) — Add a remote block store
        - [`dfsctl store block remote edit`](#dfsctl-store-block-remote-edit) — Edit a remote block store
        - [`dfsctl store block remote list`](#dfsctl-store-block-remote-list) — List remote block stores
        - [`dfsctl store block remote remove`](#dfsctl-store-block-remote-remove) — Remove a remote block store
      - [`dfsctl store block stats`](#dfsctl-store-block-stats) — Show block store statistics
    - [`dfsctl store metadata`](#dfsctl-store-metadata) — Manage metadata stores
      - [`dfsctl store metadata add`](#dfsctl-store-metadata-add) — Add a metadata store
      - [`dfsctl store metadata edit`](#dfsctl-store-metadata-edit) — Edit a metadata store
      - [`dfsctl store metadata health`](#dfsctl-store-metadata-health) — Check metadata store health
      - [`dfsctl store metadata list`](#dfsctl-store-metadata-list) — List metadata stores
      - [`dfsctl store metadata remove`](#dfsctl-store-metadata-remove) — Remove a metadata store
  - [`dfsctl switch-user`](#dfsctl-switch-user) — Switch to a different user on the current server
  - [`dfsctl system`](#dfsctl-system) — System operations
    - [`dfsctl system drain-uploads`](#dfsctl-system-drain-uploads) — Wait for all pending uploads to complete
  - [`dfsctl trash`](#dfsctl-trash) — Recycle-bin management
    - [`dfsctl trash empty`](#dfsctl-trash-empty) — Empty a share's recycle bin
    - [`dfsctl trash list`](#dfsctl-trash-list) — List recycle-bin entries for a share
    - [`dfsctl trash restore`](#dfsctl-trash-restore) — Restore a recycled file or directory
    - [`dfsctl trash status`](#dfsctl-trash-status) — Show recycle-bin status for a share
  - [`dfsctl user`](#dfsctl-user) — User management
    - [`dfsctl user change-password`](#dfsctl-user-change-password) — Change your own password
    - [`dfsctl user create`](#dfsctl-user-create) — Create a new user
    - [`dfsctl user edit`](#dfsctl-user-edit) — Edit a user
    - [`dfsctl user get`](#dfsctl-user-get) — Get user details
    - [`dfsctl user list`](#dfsctl-user-list) — List all users
    - [`dfsctl user password`](#dfsctl-user-password) — Reset a user's password
    - [`dfsctl user remove`](#dfsctl-user-remove) — Remove a user
  - [`dfsctl version`](#dfsctl-version) — Show version information


### `dfsctl`

DittoFS Control - Remote management client

dfsctl is the command-line client for managing DittoFS servers remotely.

It communicates with the DittoFS control-plane REST API (default port 8080) and
persists credentials in ~/.config/dfsctl/config.json. Run dfsctl login first to
authenticate, then use the subcommands to manage users, groups, shares, stores,
adapters, and server settings. Multiple server contexts are supported via the
context subcommand.

Use "dfsctl [command] --help" for more information about a command.

Flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter`

Protocol adapter management

Manage protocol adapters (NFS and SMB) on the DittoFS server.

Protocol adapters control which wire protocols the server accepts connections on and on which ports. Use these commands to enable, disable, or reconfigure adapters without restarting the server. All operations require admin privileges.

**Examples:**

```bash
# List all adapters with their current status and ports
dfsctl adapter list

# Enable the NFS adapter on the default port
dfsctl adapter enable nfs

# Enable the SMB adapter on port 12445
dfsctl adapter enable smb --port 12445

# Disable the NFS adapter
dfsctl adapter disable nfs

# Tune NFS adapter settings (portmapper, lease time, etc.)
dfsctl adapter settings nfs update --portmapper-enabled --portmapper-port 10111
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter disable`

Disable an adapter

Disable a protocol adapter on the DittoFS server.

Disabling an adapter stops accepting new connections on that protocol; existing sessions are closed gracefully. The adapter configuration is preserved so it can be re-enabled later with the same settings.

```
dfsctl adapter disable <type>
```

**Examples:**

```bash
# Disable the NFS adapter
dfsctl adapter disable nfs

# Disable the SMB adapter
dfsctl adapter disable smb
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter edit`

Edit an adapter

Edit an existing protocol adapter on the DittoFS server.

Without flags the command opens an interactive prompt to update the adapter's port and enabled state. When flags are provided only the specified fields are changed; all other adapter settings remain untouched.

```
dfsctl adapter edit <type> [flags]
```

**Examples:**

```bash
# Interactively edit the NFS adapter
dfsctl adapter edit nfs

# Move the NFS adapter to port 12049
dfsctl adapter edit nfs --port 12049

# Temporarily disable the SMB adapter non-interactively
dfsctl adapter edit smb --enabled false

# Pass a JSON config blob directly
dfsctl adapter edit nfs --config '{"max_read_size":131072}'
```

Flags:

```
      --config string    Adapter configuration as JSON
      --enabled string   Enable/disable adapter (true|false)
      --port int         Listen port
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter enable`

Enable an adapter

Enable a protocol adapter on the DittoFS server.

If the adapter record does not yet exist it is created automatically. Use --port to override the default listen port (NFS defaults to 12049, SMB to 12445). Changes take effect immediately without a server restart.

```
dfsctl adapter enable <type> [flags]
```

**Examples:**

```bash
# Enable the NFS adapter on the default port (12049)
dfsctl adapter enable nfs

# Enable the NFS adapter on a custom port
dfsctl adapter enable nfs --port 12049

# Enable the SMB adapter on the default port (12445)
dfsctl adapter enable smb --port 12445
```

Flags:

```
      --port int   Listen port (uses default if not specified)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter list`

List protocol adapters

List all protocol adapters configured on the DittoFS server.

Each row shows the adapter type (nfs or smb), the port it listens on, and whether it is currently enabled. Use this command to quickly confirm which protocols are active before connecting clients.

```
dfsctl adapter list
```

**Examples:**

```bash
# List adapters as a table
dfsctl adapter list

# List adapters as JSON (useful for scripting)
dfsctl adapter list -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter settings`

Manage adapter settings

Manage protocol adapter settings on the DittoFS server.

Fine-grained adapter tuning lives here: NFS lease times, delegation settings, portmapper, SMB dialect range, encryption, and more. Settings are applied immediately (except where noted) without a server restart. Use the nfs or smb sub-group to target the right adapter.

```
dfsctl adapter settings <type>
```

**Examples:**

```bash
# Show current NFS settings (non-default values are marked with *)
dfsctl adapter settings nfs show

# Update the NFS lease time
dfsctl adapter settings nfs update --lease-time 90

# Enable the embedded portmapper on port 10111
dfsctl adapter settings nfs update --portmapper-enabled --portmapper-port 10111

# Reset all SMB settings to defaults
dfsctl adapter settings smb reset --force
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter settings nfs`

Manage NFS adapter settings

Manage NFS protocol adapter settings.

Use show to inspect current values, update to change individual settings, and reset to restore defaults. NFS-specific knobs include lease times, delegation parameters, portmapper, NFSv4 minor version range, and transport limits.

```
dfsctl adapter settings nfs
```

**Examples:**

```bash
# Inspect current NFS settings (modified values are marked with *)
dfsctl adapter settings nfs show

# Restrict the server to NFSv4.1 only
dfsctl adapter settings nfs update --v4-min-minor-version 1 --v4-max-minor-version 1

# Enable the embedded portmapper on port 10111
dfsctl adapter settings nfs update --portmapper-enabled --portmapper-port 10111
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter settings nfs reset`

Reset adapter settings to defaults

Reset adapter settings to their factory default values.

Without --setting, every NFS or SMB setting is restored to its default. Pass --setting with the snake_case setting name to reset only that one field. A confirmation prompt is shown unless --force is passed.

```
dfsctl adapter settings nfs reset [flags]
```

**Examples:**

```bash
# Reset all NFS settings to defaults (with confirmation prompt)
dfsctl adapter settings nfs reset

# Reset only the NFS lease time to its default, no prompt
dfsctl adapter settings nfs reset --setting lease_time --force

# Reset all SMB settings non-interactively
dfsctl adapter settings smb reset --force
```

Flags:

```
  -f, --force            Skip confirmation prompt
      --setting string   Reset a specific setting (omit to reset all)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter settings nfs show`

Show current adapter settings

Show the current adapter settings grouped by category, compared against their defaults.

Values that differ from the default are marked with an asterisk (*) so you can quickly identify non-standard tuning. Use -o json to get the raw configuration object for scripting or backup.

```
dfsctl adapter settings nfs show
```

**Examples:**

```bash
# Show NFS settings in human-readable grouped format
dfsctl adapter settings nfs show

# Export current SMB settings as JSON
dfsctl adapter settings smb show -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter settings nfs update`

Update adapter settings

Update one or more adapter settings in a single API call.

Only flags that are explicitly passed are sent to the server; all other settings remain unchanged. Use --dry-run to validate flag values without applying them, and --force to bypass server-side range validation.

```
dfsctl adapter settings nfs update [flags]
```

**Examples:**

```bash
# Increase the NFSv4 lease time to 90 seconds
dfsctl adapter settings nfs update --lease-time 90

# Enable delegations and set the portmapper port
dfsctl adapter settings nfs update --delegations-enabled --portmapper-enabled --portmapper-port 10111

# Validate a change without applying it
dfsctl adapter settings nfs update --lease-time 300 --dry-run
```

Flags:

```
      --blocked-operations string            Comma-separated list of blocked operations
      --callback-timeout int                 Callback timeout in seconds
      --delegation-recall-timeout int        Delegation recall timeout in seconds
      --delegations-enabled                  Enable NFSv4 delegations
      --dir-deleg-batch-window-ms int        Directory delegation notification batch window in milliseconds
      --dry-run                              Validate without applying changes
      --force                                Bypass range validation
      --grace-period int                     NFSv4 grace period in seconds
      --lease-break-timeout int              Lease break timeout in seconds
      --lease-time int                       NFSv4 lease time in seconds
      --max-clients int                      Maximum concurrent clients
      --max-compound-ops int                 Maximum compound operations per request
      --max-connections int                  Maximum concurrent connections
      --max-delegations int                  Maximum total outstanding delegations (0=unlimited)
      --max-read-size int                    Maximum read size in bytes
      --max-version string                   Maximum NFS version (e.g., 4.1)
      --max-write-size int                   Maximum write size in bytes
      --min-version string                   Minimum NFS version (e.g., 3)
      --portmapper-enabled                   Enable embedded portmapper
      --portmapper-port int                  Portmapper listen port
      --portmapper-register-with-system      Register NFS/MOUNT/NLM services with the host's system rpcbind on port 111, so kernel NFSv3 clients can lock without 'nolock' (restart to apply)
      --preferred-transfer-size int          Preferred transfer size in bytes
      --udp-enabled                          Serve NLM/NSM/MOUNT over UDP (needed for NFSv3 locking from macOS/BSD; restart to apply)
      --v4-max-connections-per-session int   Maximum connections per NFSv4.1 session (0=unlimited)
      --v4-max-minor-version int             Maximum NFSv4 minor version (0=v4.0, 1=v4.1)
      --v4-min-minor-version int             Minimum NFSv4 minor version (0=v4.0, 1=v4.1)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter settings smb`

Manage SMB adapter settings

Manage SMB protocol adapter settings.

Use show to inspect current values, update to change individual settings, and reset to restore defaults. SMB-specific knobs include dialect range (SMB 2.1 to 3.1.1), session and oplock timeouts, connection limits, and at-rest encryption.

```
dfsctl adapter settings smb
```

**Examples:**

```bash
# Inspect current SMB settings
dfsctl adapter settings smb show

# Require SMB 3.x and enable encryption
dfsctl adapter settings smb update --min-dialect SMB3.0 --enable-encryption

# Relax SMB signing (offer but do not require)
dfsctl adapter settings smb update --signing enabled

# Reset the SMB session timeout to its default
dfsctl adapter settings smb reset --setting session_timeout
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter settings smb reset`

Reset adapter settings to defaults

Reset adapter settings to their factory default values.

Without --setting, every NFS or SMB setting is restored to its default. Pass --setting with the snake_case setting name to reset only that one field. A confirmation prompt is shown unless --force is passed.

```
dfsctl adapter settings smb reset [flags]
```

**Examples:**

```bash
# Reset all NFS settings to defaults (with confirmation prompt)
dfsctl adapter settings nfs reset

# Reset only the NFS lease time to its default, no prompt
dfsctl adapter settings nfs reset --setting lease_time --force

# Reset all SMB settings non-interactively
dfsctl adapter settings smb reset --force
```

Flags:

```
  -f, --force            Skip confirmation prompt
      --setting string   Reset a specific setting (omit to reset all)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter settings smb show`

Show current adapter settings

Show the current adapter settings grouped by category, compared against their defaults.

Values that differ from the default are marked with an asterisk (*) so you can quickly identify non-standard tuning. Use -o json to get the raw configuration object for scripting or backup.

```
dfsctl adapter settings smb show
```

**Examples:**

```bash
# Show NFS settings in human-readable grouped format
dfsctl adapter settings nfs show

# Export current SMB settings as JSON
dfsctl adapter settings smb show -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl adapter settings smb update`

Update adapter settings

Update one or more adapter settings in a single API call.

Only flags that are explicitly passed are sent to the server; all other settings remain unchanged. Use --dry-run to validate flag values without applying them, and --force to bypass server-side range validation.

```
dfsctl adapter settings smb update [flags]
```

**Examples:**

```bash
# Increase the NFSv4 lease time to 90 seconds
dfsctl adapter settings nfs update --lease-time 90

# Enable delegations and set the portmapper port
dfsctl adapter settings nfs update --delegations-enabled --portmapper-enabled --portmapper-port 10111

# Validate a change without applying it
dfsctl adapter settings nfs update --lease-time 300 --dry-run
```

Flags:

```
      --blocked-operations string   Comma-separated list of blocked operations
      --dry-run                     Validate without applying changes
      --enable-encryption           Enable SMB encryption
      --force                       Bypass range validation
      --max-connections int         Maximum concurrent connections
      --max-dialect string          Maximum SMB dialect
      --max-sessions int            Maximum concurrent SMB sessions
      --min-dialect string          Minimum SMB dialect
      --oplock-break-timeout int    Oplock break timeout in seconds
      --session-timeout int         SMB session timeout in seconds
      --signing string              SMB message signing mode: disabled|enabled|required
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl bench`

Run filesystem benchmarks

Run I/O and metadata benchmarks against any mounted filesystem path.

The benchmark suite operates directly on the filesystem; no API authentication is needed for basic workloads. Use 'bench run' to collect results and save them as JSON, then 'bench compare' to render a side-by-side comparison across systems. The 'bench storage-tiers' subcommand requires admin authentication to evict cache layers between reads.

**Examples:**

```bash
# Run all benchmark workloads on a mounted NFS share
dfsctl bench run /mnt/bench

# Run with 8 threads and 512 MiB files
dfsctl bench run /mnt/bench --threads 8 --file-size 512MiB --duration 30s

# Run and save results for later comparison
dfsctl bench run /mnt/bench --system dittofs --save results/dittofs.json

# Compare saved results from two systems
dfsctl bench compare results/dittofs.json results/kernel-nfs.json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl bench compare`

Compare benchmark results from multiple systems

Load two or more JSON result files produced by 'bench run' and render a side-by-side comparison table.

Each column in the output represents one system. Workloads and metrics are aligned across rows so you can directly compare throughput and IOPS between implementations. Pass -o json to get the raw comparison data for scripting.

```
dfsctl bench compare FILE [FILE...]
```

**Examples:**

```bash
# Compare DittoFS against kernel NFS
dfsctl bench compare results/dittofs.json results/kernel-nfs.json

# Compare all result files in a directory
dfsctl bench compare results/*.json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl bench run`

Run filesystem benchmarks

Run I/O and metadata benchmarks against the given directory path.

The runner creates test files under the target directory and measures throughput, IOPS, and latency for each workload. When no --workload is specified every available workload runs. Results are printed as a table by default; use --save to persist the JSON for later comparison with 'bench compare'.

```
dfsctl bench run PATH [flags]
```

**Examples:**

```bash
# Run all workloads with default parameters
dfsctl bench run /mnt/bench

# Run only sequential-read and sequential-write with 8 threads
dfsctl bench run /mnt/bench --workload seq-write,seq-read --threads 8

# Run with larger files and a longer duration
dfsctl bench run /mnt/bench --file-size 4GiB --duration 120s

# Save results and label this system for comparison
dfsctl bench run /mnt/bench --system dittofs --save results/dittofs.json --clean
```

Flags:

```
      --block-size string      I/O block size for random workloads (default "4KiB")
      --clean                  Remove test files after benchmark (default: keep for cold read reruns)
      --duration string        Time limit for duration-based workloads (default "60s")
      --file-size string       Size of each test file (default "1GiB")
      --meta-files int         Number of files for metadata workload (default 1000)
      --save string            Save results to JSON file
      --small-file-count int   Number of files for small-files workload (default 10000)
      --system string          Label identifying the system under test
      --threads int            Number of concurrent I/O workers (default 4)
      --workload string        Comma-separated workloads (default: all)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl bench storage-tiers`

Benchmark storage tier performance (cold/warm/local-only)

Benchmark DittoFS storage tier performance by measuring read throughput at each caching layer.

The workload writes a file through the NFS/SMB mount, then reads it back three times — evicting a different cache layer before each read — to isolate cold (remote store), warm (local + read buffer), and local-only performance. Admin authentication is required to call the eviction API between reads. The share must have a remote block store configured for cold-read testing.

Steps executed per file size:

```
1. Write via mount
2. Evict all (read buffer + local store)
3. Cold read (data fetched from remote store)
4. Warm read (data in read buffer + local store)
5. Evict read buffer only
6. Local-only read (data served from local FS store)
```

```
dfsctl bench storage-tiers [flags]
```

**Examples:**

```bash
# Run with default file sizes (10MB, 100MB, 1GB)
dfsctl bench storage-tiers --share myshare --mount /mnt/test

# Run with custom file sizes
dfsctl bench storage-tiers --share myshare --mount /mnt/test --sizes 1MB,10MB,50MB
```

Flags:

```
      --mount string   Mount point for file I/O (default "/mnt/test")
      --share string   Share name for block store API operations (required)
      --sizes string   Comma-separated file sizes (default: 10MB,100MB,1GB)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl client`

Manage connected clients

Manage connected NFS and SMB clients on the DittoFS server.

Use these commands to inspect which clients are currently connected, filter by protocol or share, and forcefully disconnect misbehaving sessions. All operations require admin privileges.

**Examples:**

```bash
# List all connected clients across NFS and SMB
dfsctl client list

# Show only NFS clients
dfsctl client list --protocol nfs

# Show clients connected to a specific share
dfsctl client list --share myshare

# Disconnect a specific client by its ID
dfsctl client disconnect nfs-42 --force
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl client disconnect`

Disconnect a client

Disconnect a connected client by its ID.

For NFS clients this closes the TCP connection and triggers NFSv4 state revocation; for SMB clients it tears down all sessions and cleans up associated state. Use the client ID from 'client list'. This action may cause in-progress I/O to fail on the client side.

```
dfsctl client disconnect <client-id> [flags]
```

**Examples:**

```bash
# Disconnect a client with a confirmation prompt
dfsctl client disconnect nfs-42

# Disconnect without a confirmation prompt (e.g. in a script)
dfsctl client disconnect nfs-42 --force
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl client list`

List connected clients

List all clients currently connected to the DittoFS server.

Each row shows the client ID, protocol (NFS or SMB), remote address, authenticated user, mounted shares, and how long the client has been connected. Use --protocol or --share to narrow the output.

```
dfsctl client list [flags]
```

**Examples:**

```bash
# List all connected clients
dfsctl client list

# Show only NFS clients
dfsctl client list --protocol nfs

# Show only clients connected to a specific share
dfsctl client list --share myshare

# Get the client list as JSON
dfsctl client list -o json
```

Flags:

```
      --protocol string   Filter by protocol (nfs, smb)
      --share string      Filter by share name
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl client sessions`

Manage NFS client sessions

Manage NFSv4.1 sessions for a specific connected NFS client.

Each NFSv4.1 client may have one or more sessions, each with its own fore and back channel slot tables. Use these commands to inspect session state and force-destroy sessions that are stuck or misbehaving. Admin privileges are required.

**Examples:**

```bash
# List all sessions for a client (use the hex client ID from 'client list')
dfsctl client sessions list 0000000100000001

# Force-destroy a session that is stuck
dfsctl client sessions destroy 0000000100000001 a1b2c3d4e5f6a7b8 --force
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl client sessions destroy`

Force-destroy a session

Force-destroy a specific NFSv4.1 session by client ID and session ID.

This tears down the session immediately, bypassing any in-flight request checks. The NFS client will receive errors and may need to remount. Use 'client sessions list' to find the session ID. A confirmation prompt is shown unless --force is passed.

```
dfsctl client sessions destroy <client-id> <session-id> [flags]
```

**Examples:**

```bash
# Destroy a session with a confirmation prompt
dfsctl client sessions destroy 0000000100000001 a1b2c3d4e5f6a7b8

# Destroy a session without the confirmation prompt
dfsctl client sessions destroy 0000000100000001 a1b2c3d4e5f6a7b8 --force
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl client sessions list`

List sessions for a client

List all NFSv4.1 sessions for a given client, identified by its hex client ID.

Each session entry shows the session ID, fore/back channel slot counts, back channel status, total connection count, and creation time. The session ID returned here is used with 'sessions destroy' to tear down a specific session.

```
dfsctl client sessions list <client-id>
```

**Examples:**

```bash
# List sessions for a client (hex client ID from 'client list')
dfsctl client sessions list 0000000100000001

# Get sessions as JSON for scripting
dfsctl client sessions list 0000000100000001 -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl completion`

Generate shell completion script

Generate shell completion script for dfsctl.

The generated script enables tab-completion for dfsctl commands, subcommands,
and flags in your shell. Pick the snippet for your shell and load it once.

```
dfsctl completion [bash|zsh|fish|powershell]
```

**Examples:**

```bash
# Bash (Linux): install system-wide
dfsctl completion bash > /etc/bash_completion.d/dfsctl

# Bash (macOS, requires the Homebrew bash-completion package)
dfsctl completion bash > $(brew --prefix)/etc/bash_completion.d/dfsctl

# Zsh: enable completion once (if not already enabled), then install
echo "autoload -U compinit; compinit" >> ~/.zshrc
dfsctl completion zsh > "${fpath[1]}/_dfsctl"

# Zsh (macOS, Homebrew)
dfsctl completion zsh > $(brew --prefix)/share/zsh/site-functions/_dfsctl

# Fish
dfsctl completion fish > ~/.config/fish/completions/dfsctl.fish

# PowerShell: load for the current session
dfsctl completion powershell | Out-String | Invoke-Expression

# PowerShell: persist across sessions by sourcing from your profile
dfsctl completion powershell > dfsctl.ps1
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl context`

Manage server contexts

Manage named connection contexts for one or more DittoFS servers.

Each context stores a server URL, authentication credentials, and a display name. Contexts work similarly to kubectl contexts: log in once per server, then switch between them with 'context use'. All subsequent dfsctl commands use the active context automatically.

**Examples:**

```bash
# List all saved contexts
dfsctl context list

# Switch to a context named "production"
dfsctl context use production

# Show which context is currently active
dfsctl context current

# Remove a context that is no longer needed
dfsctl context remove staging
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl context current`

Show current context

Display the name and connection details of the currently active context.

The output shows the server URL, logged-in user, and authentication status. Use this command to confirm which server subsequent dfsctl commands will target before running destructive operations.

```
dfsctl context current [flags]
```

**Examples:**

```bash
# Show the active context as a human-readable summary
dfsctl context current

# Get the active context as JSON for scripting
dfsctl context current --output json
```

Flags:

```
  -o, --output string   Output format (table|json|yaml) (default "table")
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl context list`

List all configured contexts

List all configured server contexts stored in the local credential file.

Each row shows the context name, server URL, username, and login status. The active context is marked with an asterisk (*). Use this to identify which servers you have credentials for and which context is currently selected.

```
dfsctl context list
```

**Examples:**

```bash
# List all contexts as a table
dfsctl context list

# List contexts as JSON for scripting
dfsctl context list -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl context remove`

Remove a context

Remove a saved server context and its stored credentials.

The context's configuration and access token are removed from the local credential store. Use this to clean up after decommissioning a server or when a context was created by mistake.

```
dfsctl context remove <name> [flags]
```

**Examples:**

```bash
# Remove the "staging" context with a confirmation prompt
dfsctl context remove staging

# Remove without the confirmation prompt (e.g. in a script)
dfsctl context remove staging --force
```

Flags:

```
  -f, --force   Skip confirmation
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl context rename`

Rename a context

Rename an existing server context to a new name.

All stored credentials and the active-context pointer are updated atomically; no re-authentication is needed. Use this after promoting a staging server to production, or simply to give a context a more descriptive name.

```
dfsctl context rename <old-name> <new-name>
```

**Examples:**

```bash
# Rename the "default" context to "production"
dfsctl context rename default production

# Rename a development context to something more descriptive
dfsctl context rename dev local-dev
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl context use`

Switch to a different context

Switch the active context so that subsequent dfsctl commands target a different server.

The new active context is saved to the local credential file. Run 'context current' afterwards to confirm the switch, or 'context list' to see all available context names.

```
dfsctl context use <name>
```

**Examples:**

```bash
# Switch to the "production" context
dfsctl context use production

# Switch to a local development server context
dfsctl context use local-dev
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl grace`

Manage NFSv4 grace period

Manage the NFSv4 grace period on the DittoFS server.

Grace period commands allow you to monitor and control the NFSv4 grace
period that occurs after server restart. During the grace period, clients
reclaim their previously-held state (open files, locks).

**Examples:**

```bash
# Check grace period status
dfsctl grace status

# Check status in JSON format
dfsctl grace status -o json

# Force-end the grace period
dfsctl grace end
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl grace end`

Force-end the grace period

Force-end the NFSv4 grace period immediately.

This admin-only command terminates the grace period before it expires
naturally, allowing clients to create new state (open files, locks)
without waiting. Use it to accelerate recovery after a confirmed server
restart in development environments, or when all expected clients have
already reclaimed their state.

```
dfsctl grace end
```

**Examples:**

```bash
# Force-end the grace period
dfsctl grace end

# Verify the period has ended after forcing it
dfsctl grace status
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl grace status`

Show grace period status

Display the current NFSv4 grace period status.

Shows whether a grace period is active, time remaining, and client
reclaim progress. The grace period occurs after server restart to
allow clients to reclaim their previously-held state.

```
dfsctl grace status
```

**Examples:**

```bash
# Show status as table
dfsctl grace status

# Show status as JSON
dfsctl grace status -o json

# Show status as YAML
dfsctl grace status -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl group`

Group management

Manage Unix groups on the DittoFS server. Groups bundle users together so
that share permissions can be granted to multiple users at once using a single
group reference. Each group carries a Unix GID used for NFS uid/gid resolution.
All subcommands require admin privileges.

**Examples:**

```bash
# List all groups
dfsctl group list

# Get group details including current members
dfsctl group get editors

# Create a group with an explicit GID
dfsctl group create --name editors --gid 1001

# Edit a group interactively
dfsctl group edit editors

# Add a user to a group
dfsctl group add-user editors alice

# Remove a user from a group
dfsctl group remove-user editors alice

# Remove a group (prompts for confirmation)
dfsctl group remove editors
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl group add-user`

Add a user to a group

Add a user to a group on the DittoFS server. The user will immediately
gain any permissions associated with that group on shares and other resources.
Both the group name and the username are required positional arguments.

```
dfsctl group add-user <group> <username>
```

**Examples:**

```bash
# Add alice to the editors group
dfsctl group add-user editors alice

# Add a user to the admins group
dfsctl group add-user admins bob

# Verify membership after adding
dfsctl group get editors
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl group create`

Create a new group

Create a new group on the DittoFS server. Groups are used to organise
users and can be referenced in share permissions to grant access to multiple
users at once. The group's Unix GID is auto-assigned from the server's
allocation range unless you provide one explicitly with --gid.

```
dfsctl group create [flags]
```

**Examples:**

```bash
# Create a group with an auto-assigned GID
dfsctl group create --name editors

# Create a group with an explicit GID
dfsctl group create --name editors --gid 1001

# Create a group with a description
dfsctl group create --name editors --gid 1001 --description "Content editors"
```

Flags:

```
      --description string   Group description
      --gid uint32           Group ID (auto-generated if not set)
      --name string          Group name (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl group edit`

Edit a group

Edit an existing group on the DittoFS server. When run without flags, an
interactive prompt walks you through each editable field (GID and description),
showing the current value so you can press Enter to keep it. When flags are
provided, only those fields are updated and no prompt appears.

```
dfsctl group edit <name> [flags]
```

**Examples:**

```bash
# Edit the group interactively (shows current values)
dfsctl group edit editors

# Change the group's GID to a specific value
dfsctl group edit editors --gid 1002

# Update only the description
dfsctl group edit editors --description "Senior content editors"

# Update both GID and description in one command
dfsctl group edit editors --gid 1002 --description "Senior content editors"
```

Flags:

```
      --description string   Group description
      --gid uint32           Group ID
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl group get`

Get group details

Get detailed information about a specific group on the DittoFS server.
Accepts either the group name or the group's full ID (see 'dfsctl group list').
The output includes the group's GID, description, member list, and creation
timestamp. Use -o json or -o yaml for machine-readable output.

```
dfsctl group get <name|id>
```

**Examples:**

```bash
# Show group details as a table
dfsctl group get editors

# By ID
dfsctl group get 7a1e9f02-...

# Output as JSON (useful for scripting)
dfsctl group get editors -o json

# Output as YAML
dfsctl group get editors -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl group list`

List all groups

List all groups registered on the DittoFS server. The table view
shows each group's name, GID, member count, and description. Use -o json or
-o yaml to get machine-readable output suitable for piping into other tools.

```
dfsctl group list
```

**Examples:**

```bash
# List all groups as a table
dfsctl group list

# Output the full group list as JSON
dfsctl group list -o json

# Output as YAML
dfsctl group list -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl group remove`

Remove a group

Remove a group from the DittoFS server. Accepts either the group name
or the group's full ID (see 'dfsctl group list'). This action is irreversible:
the group record is permanently removed and any users that had it as their
primary group will lose that association. You will be prompted for
confirmation unless --force is specified.

```
dfsctl group remove <name|id> [flags]
```

**Examples:**

```bash
# Remove a group (prompts for confirmation)
dfsctl group remove editors

# Remove a group non-interactively (for scripts and automation)
dfsctl group remove editors --force
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl group remove-user`

Remove a user from a group

Remove a user from a group on the DittoFS server. The user loses any
permissions that were derived solely from that group membership. Both the
group name and the username are required positional arguments.

```
dfsctl group remove-user <group> <username>
```

**Examples:**

```bash
# Remove alice from the editors group
dfsctl group remove-user editors alice

# Remove a user from the admins group
dfsctl group remove-user admins bob

# Verify membership after removal
dfsctl group get editors
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl identity-provider`

Identity provider (LDAP/AD, Kerberos) management

Manage DittoFS identity providers (LDAP/AD and Kerberos) over the API.

LDAP changes are hot-reloaded by the live identity resolver without a server restart. Kerberos machine-account changes are saved immediately but take effect only on the next server restart. Secret material (bind password, machine secret) is write-only and is never returned by the API.

**Examples:**

```bash
# List all identity providers and their status
dfsctl identity-provider list

# Show the current LDAP configuration (secrets redacted)
dfsctl identity-provider get ldap

# Test an LDAP configuration without saving it
dfsctl identity-provider test ldap --config '{"enabled":true,"url":"ldap://dc.corp.example:389","base_dn":"DC=corp,DC=example","bind_dn":"CN=svc,DC=corp,DC=example","bind_password":"s3cret","idmap":"rfc2307"}'
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl identity-provider configure`

Configure Kerberos machine-account settings

Set individual Kerberos machine-account flags without replacing the full configuration.

The current configuration is read from the API, the specified flags are applied,
and the result is written back. Fields not specified on the command line are
preserved unchanged.

--machine-secret is write-only: omit it to keep the currently stored credential;
provide a new value to rotate it. Submitting the redacted placeholder ("********")
also preserves the stored secret.

Changes take effect on the next server restart.

```
dfsctl identity-provider configure kerberos [flags]
```

**Examples:**

```bash
dfsctl identity-provider configure kerberos --machine-account-enabled --machine-account-name MYHOST$ --machine-secret 'p@ss' --machine-keytab /etc/krb5.keytab --dc-address 192.0.2.10 --dc-address 192.0.2.11
```

Flags:

```
      --dc-address stringArray        Domain controller address (repeatable; pass once per address)
      --machine-account-enabled       Enable machine-account authentication for NETLOGON
      --machine-account-name string   Machine account name (e.g. MYHOST$)
      --machine-keytab string         Path to the machine-account keytab file
      --machine-secret string         Machine account password (write-only; omit to keep the stored value)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl identity-provider get`

Show an identity provider's configuration (secrets redacted)

Show the current configuration of an identity provider with all secret fields redacted.

Use this to verify LDAP or Kerberos settings without exposing sensitive credentials. The output uses the same JSON schema accepted by 'identity-provider set', making it easy to copy-edit and resubmit.

```
dfsctl identity-provider get <ldap|kerberos>
```

**Examples:**

```bash
# Show the current LDAP configuration
dfsctl identity-provider get ldap

# Show the current Kerberos configuration as JSON
dfsctl identity-provider get kerberos -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl identity-provider list`

List identity providers and their state

List all identity providers configured on the DittoFS server and their current state.

Each row shows the provider type (ldap or kerberos), whether it has been configured, and whether it is enabled. Use 'identity-provider get <type>' to inspect the full configuration for a specific provider.

```
dfsctl identity-provider list
```

**Examples:**

```bash
# Show all identity providers as a table
dfsctl identity-provider list

# Get identity provider status as JSON
dfsctl identity-provider list -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl identity-provider set`

Create or replace an identity provider's configuration

Create or replace an identity provider's configuration from a JSON body.

The JSON shape matches the API config schema. For LDAP, set "bind_password" to
the real password (or "********" / omit to keep the stored one). LDAP changes
apply live; Kerberos changes apply on the next server restart.

```
dfsctl identity-provider set <ldap|kerberos> --config '<json>' [flags]
```

**Examples:**

```bash
dfsctl identity-provider set ldap --config '{"enabled":true,"url":"ldaps://dc:636","base_dn":"DC=x,DC=y","bind_dn":"CN=svc,DC=x,DC=y","bind_password":"s3cret","idmap":"rfc2307"}'
dfsctl identity-provider set kerberos --config @/path/to/krb.json
```

Flags:

```
      --config string   configuration as a JSON string, or @file to read from a file (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl identity-provider test`

Test an identity provider's configuration without persisting it

Validate and test an identity provider configuration against the live server without saving it.

The server attempts to connect to the LDAP or Kerberos endpoint and runs basic reachability checks. On success the command exits 0; on failure it exits non-zero with a description of which stage failed. Use this before 'identity-provider set' to avoid applying a broken configuration.

```
dfsctl identity-provider test <ldap|kerberos> --config '<json>' [flags]
```

**Examples:**

```bash
# Test an LDAP configuration inline
dfsctl identity-provider test ldap --config '{"enabled":true,"url":"ldap://dc.corp.example:389","base_dn":"DC=corp,DC=example","bind_dn":"CN=svc,DC=corp,DC=example","bind_password":"s3cret","idmap":"rfc2307"}'

# Test a Kerberos configuration loaded from a file
dfsctl identity-provider test kerberos --config @/etc/dittofs/krb5-config.json
```

Flags:

```
      --config string   configuration to test as a JSON string, or @file (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl idmap`

Manage identity mappings

Manage identity mappings that link external authentication principals to
local DittoFS user accounts. Mappings are shared across NFS and SMB, ensuring
consistent uid/gid resolution in mixed-protocol deployments. Supported principal
formats include:

```
NFS/Kerberos:  alice@EXAMPLE.COM
SMB/NTLM:      CORP\alice
SMB/Kerberos:  alice@CORP.COM
```

Use "dfsctl idmap sid" to inspect the separate table of foreign-SID to
Unix UID/GID allocations managed automatically by Active Directory resolution.

**Examples:**

```bash
# List all identity mappings
dfsctl idmap list

# Map a Kerberos principal (works for both NFS and SMB)
dfsctl idmap add --principal alice@EXAMPLE.COM --username alice

# Map an NTLM domain user to the same local account
dfsctl idmap add --provider ad --principal 'CORP\alice' --username alice

# Remove a mapping (prompts for confirmation)
dfsctl idmap remove --principal alice@EXAMPLE.COM
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl idmap add`

Add an identity mapping

Add a new identity mapping that links an external authentication principal
to a local DittoFS user account. This is how Kerberos, OIDC, and Active Directory
identities are mapped to the local user that owns their files and holds their
permissions. The --provider flag selects the identity provider and defaults to
"kerberos".

```
dfsctl idmap add [flags]
```

**Examples:**

```bash
# Map a Kerberos principal to a local user (default provider)
dfsctl idmap add --principal alice@EXAMPLE.COM --username alice

# Map an NTLM domain user with the AD provider
dfsctl idmap add --provider ad --principal CORP\\alice --username alice

# Map an OIDC subject claim to a local user
dfsctl idmap add --provider oidc --principal sub:abc123 --username bob

# Map a Kerberos admin principal to the local admin account
dfsctl idmap add --principal admin@CORP.COM --username admin
```

Flags:

```
      --principal string   External identity (e.g., alice@EXAMPLE.COM)
      --provider string    Identity provider (e.g., kerberos, oidc, ad) (default "kerberos")
      --username string    DittoFS username
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl idmap list`

List identity mappings

List identity mappings registered on the DittoFS server. Each row shows the
provider name, the external principal, and the local DittoFS username it maps to.
Use --provider to filter the list to a single identity provider, and -o json or
-o yaml for machine-readable output.

```
dfsctl idmap list [flags]
```

**Examples:**

```bash
# List all identity mappings
dfsctl idmap list

# Show only Kerberos mappings
dfsctl idmap list --provider kerberos

# Show only AD mappings as JSON
dfsctl idmap list --provider ad -o json

# Output all mappings as YAML
dfsctl idmap list -o yaml
```

Flags:

```
      --provider string   Filter by identity provider (e.g., kerberos, oidc, ad)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl idmap remove`

Remove an identity mapping

Remove an identity mapping by provider and principal. Once removed, the
external principal will no longer be automatically resolved to the local user,
and connections authenticated with that principal will be rejected or treated
as anonymous. This action is irreversible. You will be prompted for
confirmation unless --force is specified.

```
dfsctl idmap remove [flags]
```

**Examples:**

```bash
# Remove a Kerberos mapping (prompts for confirmation)
dfsctl idmap remove --principal alice@EXAMPLE.COM

# Remove an AD mapping with explicit provider
dfsctl idmap remove --provider ad --principal CORP\\alice

# Remove a mapping non-interactively (for scripts)
dfsctl idmap remove --principal alice@EXAMPLE.COM --force
```

Flags:

```
  -f, --force              Skip confirmation prompt
      --principal string   External identity to remove
      --provider string    Identity provider (e.g., kerberos, oidc, ad) (default "kerberos")
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl idmap sid`

Manage foreign-SID UID/GID allocations

Manage durable foreign-SID to Unix UID/GID allocations. When DittoFS
resolves Active Directory or LDAP principals, foreign domain SIDs (of the form
`S-1-5-21-<domain>-<rid>`) are bound to stable Unix UIDs and GIDs exactly
once and never remapped, ensuring a foreign SID always resolves to the same
numeric identity across restarts.

This subcommand surfaces that allocation table for administrative inspection and
cleanup. It is distinct from "dfsctl idmap add/list/remove", which manages the
authentication-principal to local-user mappings used during login.

**Examples:**

```bash
# List all foreign-SID allocations
dfsctl idmap sid list

# Output the allocation table as JSON
dfsctl idmap sid list -o json

# Remove a misallocated SID entry (use with care)
dfsctl idmap sid remove S-1-5-21-111-222-333-1107
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl idmap sid list`

List foreign-SID UID/GID allocations

List all durable foreign-SID to Unix UID/GID allocations on the DittoFS
server. Each row shows the Windows SID, whether it represents a user or group,
the allocated Unix ID, and a display name if one was resolved at allocation time.
Use this command to audit which AD/LDAP principals have been seen by the server
and what Unix IDs they were assigned.

```
dfsctl idmap sid list
```

**Examples:**

```bash
# List all foreign-SID allocations as a table
dfsctl idmap sid list

# Output as JSON (includes full SID details)
dfsctl idmap sid list -o json

# Output as YAML
dfsctl idmap sid list -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl idmap sid remove`

Remove a foreign-SID UID/GID allocation

Remove a durable foreign-SID to Unix UID/GID allocation. This is an
administrative escape hatch: once removed, the SID will be re-allocated to a
potentially different UID/GID on its next resolution, which can re-attribute
files owned by the old Unix ID to a different numeric owner. Use only when
correcting a misallocated SID, and be aware that in-flight NFS/SMB sessions
may cache the old mapping until they reconnect. You will be prompted for
confirmation unless --force is specified.

```
dfsctl idmap sid remove <sid> [flags]
```

**Examples:**

```bash
# Remove a SID allocation (prompts for confirmation)
dfsctl idmap sid remove S-1-5-21-111-222-333-1107

# Remove without confirmation (for automated cleanup scripts)
dfsctl idmap sid remove S-1-5-21-111-222-333-1107 --force
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl login`

Authenticate with DittoFS server

Authenticate with a DittoFS server and store credentials locally.

Contacts the control-plane API at the given server URL, exchanges credentials
for an access/refresh token pair, and saves them to ~/.config/dfsctl/config.json
under a named context (default: auto-derived from the server URL). Subsequent
commands reuse the stored token and refresh it automatically when it expires.
TLS client certificates and a custom CA bundle can be pinned at login time and
are persisted into the context for later commands.

```
dfsctl login [flags]
```

**Examples:**

```bash
# First login to a local server (prompts for password)
dfsctl login --server http://localhost:8080 --username admin

# Login to a remote server with mutual TLS
dfsctl login --server https://dfs.example.com --username admin --cacert ca.pem --client-cert client.pem --client-key client.key

# Login passing password on the command line (less secure; avoid in shared environments)
dfsctl login --server http://localhost:8080 -u admin -p secret

# Re-login to the already-stored server (password prompt only)
dfsctl login
```

Flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate
      --client-cert string   Path to a PEM client certificate for mutual TLS
      --client-key string    Path to the PEM client private key for mutual TLS
  -c, --context string       Context name (defaults to current or auto-generated)
  -p, --password string      Password
      --server string        Server URL (default "http://localhost:8080")
      --tls-skip-verify      Disable TLS certificate verification (insecure)
  -u, --username string      Username
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl logout`

Clear stored credentials

Clear stored credentials for the current context.

Removes the access and refresh tokens from the active context in
~/.config/dfsctl/config.json but preserves the server URL and context name so
you can re-authenticate quickly with dfsctl login. To switch between contexts
or remove a context entirely, use the dfsctl context subcommand.

```
dfsctl logout
```

**Examples:**

```bash
# Logout from the current context (clears tokens, keeps server URL)
dfsctl logout

# Logout then immediately log back in as a different user
dfsctl logout && dfsctl login --username operator
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl netgroup`

Manage netgroups (IP access control)

Create and manage netgroups for IP-based share access control. A netgroup
is a named set of IP addresses, CIDR ranges, or hostnames that can be referenced
from share security policies to allow or restrict which network endpoints can
access a share. All subcommands require admin privileges.

**Examples:**

```bash
# List all netgroups
dfsctl netgroup list

# Create a netgroup and populate it
dfsctl netgroup create --name office-network
dfsctl netgroup add-member office-network --type cidr --value 192.168.1.0/24

# Show a netgroup and its members (including member IDs)
dfsctl netgroup show office-network

# Remove a specific member by UUID
dfsctl netgroup remove-member office-network --member-id <uuid>

# Remove a netgroup (fails if still in use by shares)
dfsctl netgroup remove office-network
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl netgroup add-member`

Add a member to a netgroup

Add a network endpoint to a netgroup. The endpoint can be a single IP
address, a CIDR range, or a hostname. Valid types are "ip", "cidr", and
"hostname". Each entry receives a unique ID that you use when removing it
with "dfsctl netgroup remove-member".

```
dfsctl netgroup add-member <name> [flags]
```

**Examples:**

```bash
# Add a single IP address to the netgroup
dfsctl netgroup add-member office-network --type ip --value 192.168.1.100

# Add an entire subnet via CIDR
dfsctl netgroup add-member office-network --type cidr --value 10.0.0.0/8

# Add a specific hostname
dfsctl netgroup add-member office-network --type hostname --value server1.example.com

# Add a /24 subnet for a datacenter hosts group
dfsctl netgroup add-member datacenter-hosts --type cidr --value 172.16.0.0/24
```

Flags:

```
      --type string    Member type: ip, cidr, or hostname (required)
      --value string   Member value (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl netgroup create`

Create a new netgroup

Create a new netgroup on the DittoFS server. Netgroups are named sets of
IP addresses, CIDR ranges, or hostnames that can be referenced in share security
policies to control which network endpoints are allowed access. After creating a
netgroup, use "dfsctl netgroup add-member" to populate it.

```
dfsctl netgroup create [flags]
```

**Examples:**

```bash
# Create a netgroup for the office subnet
dfsctl netgroup create --name office-network

# Create a netgroup and output the result as JSON
dfsctl netgroup create --name datacenter-hosts -o json
```

Flags:

```
      --name string   Netgroup name (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl netgroup list`

List all netgroups

List all netgroups registered on the DittoFS server. The table view shows
each netgroup's name, total member count, and creation time. Use "dfsctl
netgroup show <name>" to see the individual members of a specific netgroup.

```
dfsctl netgroup list
```

**Examples:**

```bash
# List all netgroups as a table
dfsctl netgroup list

# Output the full netgroup list as JSON
dfsctl netgroup list -o json

# Output as YAML
dfsctl netgroup list -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl netgroup remove`

Remove a netgroup

Remove a netgroup from the DittoFS server. This action is irreversible.
If the netgroup is still referenced by one or more shares, the deletion fails
with a conflict error that lists the affected shares — remove those references
first. You will be prompted for confirmation unless --force is specified.

```
dfsctl netgroup remove <name> [flags]
```

**Examples:**

```bash
# Remove a netgroup (prompts for confirmation)
dfsctl netgroup remove office-network

# Remove a netgroup non-interactively (for scripts and automation)
dfsctl netgroup remove office-network --force
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl netgroup remove-member`

Remove a member from a netgroup

Remove a member from a netgroup by its member ID. Members are identified
by UUID, not by their IP/CIDR/hostname value — run "dfsctl netgroup show
<name>" to find the ID of the entry you want to remove. The removal takes
effect immediately for subsequent share access checks.

```
dfsctl netgroup remove-member <name> [flags]
```

**Examples:**

```bash
# Find the member ID first
dfsctl netgroup show office-network

# Remove a member by its UUID
dfsctl netgroup remove-member office-network --member-id 550e8400-e29b-41d4-a716-446655440000

# Remove a member non-interactively in a script
dfsctl netgroup remove-member datacenter-hosts --member-id a1b2c3d4-e5f6-7890-abcd-ef1234567890
```

Flags:

```
      --member-id string   Member ID to remove (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl netgroup show`

Show netgroup details

Show detailed information about a netgroup, including all of its
members with their IDs, types, and values. Member IDs shown here are needed
when removing a member with "dfsctl netgroup remove-member". Use -o json or
-o yaml for machine-readable output.

```
dfsctl netgroup show <name>
```

**Examples:**

```bash
# Show all details and members of a netgroup
dfsctl netgroup show office-network

# Output the full netgroup structure as JSON (includes member IDs)
dfsctl netgroup show office-network -o json

# Output as YAML
dfsctl netgroup show office-network -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl quota`

Per-identity quota management

Manage per-identity (user/group/default-user) storage quotas on a share.

Quotas bound both bytes and inode (file) count, with optional soft thresholds
and a grace period before a soft threshold is enforced as hard. These operations
require admin privileges.

**Examples:**

```bash
# List all quotas on a share
dfsctl quota list /archive

# Set a per-user quota (uid 1000)
dfsctl quota set /archive --scope user --id 1000 --limit-bytes 10GiB --limit-files 100000

# Set the default-user fallback quota (applies to users without an explicit quota)
dfsctl quota set /archive --scope default-user --limit-bytes 1GiB

# Set a per-group quota with soft thresholds and a grace period
dfsctl quota set /archive --scope group --id 2000 --limit-bytes 50GiB --soft-bytes 45GiB --grace-seconds 604800

# Remove a per-user quota
dfsctl quota remove /archive --scope user --id 1000
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl quota list`

List all quotas on a share

List all per-identity quotas configured on a share.

Outputs one row per quota entry, showing the scope (user/group/default-user),
identity ID, hard and soft byte/file limits, current usage, and the grace
period. Use this to audit who has explicit limits before adjusting or removing
them.

```
dfsctl quota list <share>
```

**Examples:**

```bash
# List quotas as a table
dfsctl quota list /archive

# List as JSON
dfsctl quota list /archive -o json

# List as YAML
dfsctl quota list /archive -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl quota remove`

Remove a per-identity quota

Remove a per-identity quota from a share.

Once removed, the identity reverts to the default-user fallback quota (if
one exists) or becomes unlimited. The operation is irreversible and requires
confirmation unless --force is specified.

```
dfsctl quota remove <share> [flags]
```

**Examples:**

```bash
# Remove a per-user quota (uid 1000)
dfsctl quota remove /archive --scope user --id 1000

# Remove the default-user fallback quota
dfsctl quota remove /archive --scope default-user

# Remove a per-group quota (gid 2000) without prompting
dfsctl quota remove /archive --scope group --id 2000 --force
```

Flags:

```
  -f, --force          Skip confirmation prompt
      --id int         Identity id (uid for user, gid for group). Required for user/group; omit for default-user. (default -1)
      --scope string   Quota scope (user|group|default-user) (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl quota set`

Create or update a per-identity quota

Create or update a per-identity quota on a share.

The --scope flag selects user, group, or default-user. For user/group scopes an
identity --id (uid or gid) is required. The default-user scope is a fallback
applied to any user without an explicit user quota and takes no --id.

A byte or file limit of 0 (the default) means "no limit on that dimension".

```
dfsctl quota set <share> [flags]
```

**Examples:**

```bash
# Per-user quota (uid 1000): 10GiB / 100k files
dfsctl quota set /archive --scope user --id 1000 --limit-bytes 10GiB --limit-files 100000

# Default-user fallback quota
dfsctl quota set /archive --scope default-user --limit-bytes 1GiB

# Per-group quota with soft thresholds and a 7-day grace period
dfsctl quota set /archive --scope group --id 2000 --limit-bytes 50GiB --soft-bytes 45GiB --grace-seconds 604800
```

Flags:

```
      --grace-seconds int    Seconds usage may exceed a soft threshold before it is enforced as hard. 0 = grace disabled.
      --id int               Identity id (uid for user, gid for group). Required for user/group; omit for default-user. (default -1)
      --limit-bytes string   Hard byte ceiling (e.g., '10GiB', '500MiB'). 0/empty = unlimited.
      --limit-files int      Hard inode (file-count) ceiling. 0 = unlimited.
      --scope string         Quota scope (user|group|default-user) (required)
      --soft-bytes string    Soft byte threshold (e.g., '8GiB'). 0/empty = none.
      --soft-files int       Soft inode threshold. 0 = none.
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl settings`

Server settings management

Manage live server settings on the DittoFS server.

Server settings are key-value pairs that control runtime behaviour (logging level, feature flags, etc.) without requiring a restart. List all available keys with 'settings list', inspect a single value with 'settings get', and change it with 'settings set'. All operations require admin privileges.

**Examples:**

```bash
# List every setting with its current value and description
dfsctl settings list

# Inspect the current logging level
dfsctl settings get logging.level

# Switch to debug logging at runtime
dfsctl settings set logging.level DEBUG
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl settings get`

Get a setting value

Get the current value of a single server setting by its dot-separated key.

The default output prints key = value to stdout. Pass -o json or -o yaml to get a structured response including the setting description, useful for automation.

```
dfsctl settings get <key>
```

**Examples:**

```bash
# Print the current logging level
dfsctl settings get logging.level

# Get the setting as JSON for scripting
dfsctl settings get logging.level -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl settings list`

List all settings

List all available server settings and their current values.

The table shows each setting's key, current value, and a human-readable description. Use this command to discover what can be tuned with 'settings set'. Pass -o json to get the full list in machine-readable form.

```
dfsctl settings list
```

**Examples:**

```bash
# List all settings as a table
dfsctl settings list

# Dump all settings as JSON for scripting
dfsctl settings list -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl settings set`

Set a setting value

Set the value of a server setting identified by its dot-separated key.

The change is applied immediately at runtime without a server restart. Use 'settings list' to discover available keys and their expected value types.

```
dfsctl settings set <key> <value>
```

**Examples:**

```bash
# Switch the server to DEBUG logging immediately
dfsctl settings set logging.level DEBUG

# Reset logging to the default level
dfsctl settings set logging.level INFO
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share`

Share management

Manage shares on the DittoFS server.

The `share` tree uses `share <verb> <name>` — Cobra
resolves the verb as the subcommand and `<name>` as its positional
argument. These operations require admin privileges.

**Examples:**

```bash
# List all shares
dfsctl share list

# Create a new share
dfsctl share create --name /archive --metadata default --local fs-cache --remote s3-store

# Show share details
dfsctl share show /archive

# Edit a share interactively
dfsctl share edit /archive

# Edit a share with flags
dfsctl share edit /archive --read-only true

# Disable a share (drain clients, block new connections)
dfsctl share disable /archive

# Re-enable a share
dfsctl share enable /archive

# Remove a share
dfsctl share remove /archive

# Grant permission
dfsctl share permission grant /archive --user alice --level read-write
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share create`

Create a new share

Create a new share on the DittoFS server.

A share requires a metadata store and a local block store. A remote block store
is optional and enables tiered storage (local cache + remote durable storage).

```
dfsctl share create [flags]
```

**Examples:**

```bash
# Create a share with local block store only
dfsctl share create --name /data --metadata default --local fs-cache

# Create a share with local and remote block stores
dfsctl share create --name /archive --metadata default --local fs-cache --remote s3-store

# Create a read-only share
dfsctl share create --name /readonly --metadata default --local fs-cache --read-only

# Create with default permission allowing all users read-write access
dfsctl share create --name /shared --metadata default --local fs-cache --remote s3-store --default-permission read-write

# Create with description
dfsctl share create --name /docs --metadata default --local fs-cache --description "Documentation files"

# Create a pinned share (blocks never evicted)
dfsctl share create --name /edge-data --metadata default --local fs-cache --retention pin

# Create with TTL retention (evict after 72 hours of no access)
dfsctl share create --name /logs --metadata default --local fs-cache --retention ttl --retention-ttl 72h

# Create with per-share cache size overrides
dfsctl share create --name /bigdata --metadata default --local fs-cache --local-store-size 10GiB --read-buffer-size 2GiB

# Create with per-share quota
dfsctl share create --name /limited --metadata default --local fs-cache --quota-bytes 10GiB
```

Flags:

```
      --access-based-enumeration        Enable Windows access-based enumeration (SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM). When true, SMB clients only see directory entries they can read.
      --acl-canonicalize-inherited      When false, preserves the SE_DACL_AUTO_INHERITED control bit verbatim on SET_INFO Security instead of applying MS-DTYP §2.5.3.4.2 canonicalization (Samba "acl flag inherited canonicalization = no"). Default true matches Windows. (default true)
      --allow-mfsymlink                 Convert 1067-byte XSym (Minshall+French) symlink files written by macOS/Windows SMB clients into real symlinks on CLOSE. Off by default (XSym files are stored as regular files).
      --change-notify-disabled          Reject SMB2 CHANGE_NOTIFY with STATUS_NOT_IMPLEMENTED on this share (mirrors Samba 'kernel change notify = no').
      --continuous-availability         Advertise SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY and allow SMB3 persistent durable handles on this share.
      --default-permission string       Default permission for unmapped UIDs (none|read|read-write|admin) (default "none")
      --description string              Share description
      --enable-trash                    Enable the per-share recycle bin so deletes move to #recycle instead of being permanent.
      --encrypt-data                    Require SMB3 encryption for this share
      --local string                    Local block store name (required)
      --local-store-size string         Per-share disk cache size override (e.g., 10GiB, 500MiB)
      --metadata string                 Metadata store name (required)
      --name string                     Share name/path (required)
      --owner string                    Username that owns the share's root directory (defaults to root). The owner can write at the share root; other principals are governed by POSIX mode plus their share permission grant.
      --quota-bytes string              Per-share byte quota (e.g., '10GiB', '500MiB'). 0 = unlimited (default)
      --read-buffer-size string         Per-share read buffer size override (e.g., 2GiB, 256MiB)
      --read-only                       Make share read-only
      --remote string                   Remote block store name (optional)
      --retention string                Retention policy (pin|ttl|lru)
      --retention-ttl string            Retention TTL duration (e.g., 72h, 24h)
      --streams-disabled                Reject SMB2 Alternate Data Stream opens with STATUS_OBJECT_NAME_INVALID on this share (mirrors Samba 'smbd:streams = no').
      --trash-exclude strings           Glob patterns whose deletions bypass the recycle bin (repeatable).
      --trash-max-size int              Max bytes the recycle bin may hold before the reaper evicts oldest items (0 = unbounded).
      --trash-restrict-empty-to-admin   Restrict emptying the recycle bin to admins.
      --trash-retention-days int        Days to retain recycled items before the reaper purges them (0 = keep forever).
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share disable`

Disable a share (drain clients, block new connections)

Disable a share on the DittoFS server.

Disabling a share drains connected clients synchronously (NFS MOUNT / NFSv4
PUTFH / SMB TREE_CONNECT are refused for disabled shares) and blocks new
connections until the share is re-enabled. This is the safety gate that
must precede a metadata-store restore.

The command blocks until the drain completes (or the server's
lifecycle shutdown timeout fires). Exit code is 0 when the share has been
marked disabled and all in-flight clients have been notified.

```
dfsctl share disable <name>
```

**Examples:**

```bash
# Disable a share before restoring its metadata store
dfsctl share disable /archive

# Emit the updated Share record as JSON
dfsctl share disable /archive -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share edit`

Edit a share

Edit an existing share on the DittoFS server.

When run without flags, opens an interactive editor to modify share properties.
When flags are provided, only the specified fields are updated.

```
dfsctl share edit <name> [flags]
```

**Examples:**

```bash
# Edit share interactively
dfsctl share edit /archive

# Update local block store reference
dfsctl share edit /archive --local new-fs-cache

# Update remote block store reference
dfsctl share edit /archive --remote new-s3-store

# Make share read-only
dfsctl share edit /archive --read-only true

# Make share writable
dfsctl share edit /archive --read-only false

# Set default permission to allow all users read-write access
dfsctl share edit /archive --default-permission read-write

# Update description
dfsctl share edit /archive --description "New description"

# Change retention policy to pin (blocks never evicted)
dfsctl share edit /archive --retention pin

# Change retention policy to TTL with 72-hour window
dfsctl share edit /archive --retention ttl --retention-ttl 72h

# Override per-share disk cache size
dfsctl share edit /archive --local-store-size 10GiB

# Override per-share read buffer size
dfsctl share edit /archive --read-buffer-size 2GiB

# Set per-share quota
dfsctl share edit /archive --quota-bytes 10GiB

# Remove quota (set to unlimited)
dfsctl share edit /archive --quota-bytes 0
```

Flags:

```
      --access-based-enumeration string        Enable/disable Windows access-based enumeration (true|false). Takes effect on adapter restart.
      --acl-canonicalize-inherited string      When false, preserves the SE_DACL_AUTO_INHERITED control bit verbatim on SET_INFO Security instead of applying MS-DTYP §2.5.3.4.2 canonicalization (Samba "acl flag inherited canonicalization = no"). Default true matches Windows. Takes effect on adapter restart.
      --default-permission string              Default permission (none|read|read-write|admin)
      --description string                     Share description
      --enable-trash string                    Enable/disable the per-share recycle bin (true|false). Applied live; disabling auto-empties the bin.
      --encrypt-data string                    Require SMB3 encryption (true|false)
      --local string                           Local block store name
      --local-store-size string                Per-share disk cache size override (e.g., 10GiB, 500MiB)
      --quota-bytes string                     Per-share byte quota (e.g., '10GiB'). 0 = remove quota
      --read-buffer-size string                Per-share read buffer size override (e.g., 2GiB, 256MiB)
      --read-only string                       Set read-only (true|false)
      --remote string                          Remote block store name
      --retention string                       Retention policy (pin|ttl|lru)
      --retention-ttl string                   Retention TTL duration (e.g., 72h)
      --trash-exclude strings                  Glob patterns whose deletions bypass the recycle bin (repeatable).
      --trash-max-size int                     Max bytes the recycle bin may hold before the reaper evicts oldest items (0 = unbounded). -1 leaves unchanged. (default -1)
      --trash-restrict-empty-to-admin string   Restrict emptying the recycle bin to admins (true|false).
      --trash-retention-days int               Days to retain recycled items before the reaper purges them (0 = keep forever). -1 leaves unchanged. (default -1)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share enable`

Enable a share (accept new connections)

Enable a share on the DittoFS server.

Re-enabling a share allows new client connections and lifts the drain state
set by 'share disable'. Re-enabling is a deliberate operator act; no
mid-restore safety check is performed — the operator owns the timing.

```
dfsctl share enable <name>
```

**Examples:**

```bash
# Enable a share after a completed metadata-store restore
dfsctl share enable /archive

# Emit the updated Share record as JSON
dfsctl share enable /archive -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share list`

List all shares

List all shares configured on the DittoFS server.

Each row shows the share's name, the metadata and block stores it uses, its
storage quota and current usage, the default permission level, the block
retention policy, and whether the share is currently enabled. Use this command
to get a quick overview of all shares before running share-specific commands.

```
dfsctl share list
```

**Examples:**

```bash
# List all shares as a table
dfsctl share list

# List shares and pipe to grep to find disabled ones
dfsctl share list | grep " -$"

# Output the full share list as JSON for scripting
dfsctl share list -o json

# Output as YAML
dfsctl share list -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share list-mounts`

List mounted DittoFS shares

List all currently mounted DittoFS shares.

This command shows NFS and SMB mounts from localhost that are likely DittoFS shares.
Optionally filter by share name.

```
dfsctl share list-mounts [share]
```

**Examples:**

```bash
# List all mounted DittoFS shares
dfsctl share list-mounts

# Filter by share name
dfsctl share list-mounts /export

# Short alias
dfsctl share mounts
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share mount`

Mount a share via NFS or SMB

Mount a DittoFS share at a local mount point using NFS or SMB protocol.

For SMB mounts, credentials are resolved in order:

1. --username/--password flags
2. DITTOFS_PASSWORD environment variable (for password)
3. Current login context username
4. Interactive password prompt

Mount commands typically require sudo/root privileges on Unix systems.

Platform differences for SMB with sudo:

- Linux: mount owner set to your user via uid/gid options (default mode 0755)
- macOS: mount owned by root (uid/gid removed in Catalina), default mode 0777
- macOS alternative: mount to ~/mnt without sudo for a user-owned mount
- Windows: uses 'net use' to map network drives (e.g. dfsctl share mount /export --protocol smb Z:)

```
dfsctl share mount [share] [mountpoint] [flags]
```

**Examples:**

```bash
# Mount via NFS
dfsctl share mount /export --protocol nfs /mnt/dittofs

# Mount via SMB
dfsctl share mount /export --protocol smb /mnt/dittofs

# Mount via SMB with explicit credentials
dfsctl share mount /export --protocol smb --username alice /mnt/dittofs

# Mount via SMB with password from environment
DITTOFS_PASSWORD=secret dfsctl share mount /export --protocol smb /mnt/dittofs

# Mount to user directory without sudo (macOS only, recommended)
mkdir -p ~/mnt/dittofs && dfsctl share mount /export --protocol smb ~/mnt/dittofs
```

Flags:

```
      --dir-mode string      Directory permissions for SMB mount (octal) (default "0777")
      --file-mode string     File permissions for SMB mount (octal, default 0777 on macOS since uid/gid not supported) (default "0777")
      --nfs-version string   NFS protocol version for NFS mounts (3, 4, 4.0, 4.1, 4.2). v4 carries locking in-protocol; v3 locking needs the server UDP transport + portmapper (default "3")
  -P, --password string      Password for SMB mount (will prompt if not provided)
  -p, --protocol string      Protocol to use (nfs or smb) (required)
  -u, --username string      Username for SMB mount (defaults to login username)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share nfs-config`

Manage per-share NFS adapter configuration

View and update the NFS adapter configuration for a share.

This controls protocol-specific NFS export settings such as the squash mode,
auth flavor, and the netgroup that restricts which clients may access the
export. Netgroup changes take effect immediately; other fields apply on the
next adapter restart.

**Examples:**

```bash
# Show a share's NFS config
dfsctl share nfs-config show /export

# Associate a netgroup with the share's NFS export
dfsctl share nfs-config set /export --netgroup office-network

# Remove the netgroup association (allow all clients)
dfsctl share nfs-config set /export --netgroup ""
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share nfs-config set`

Update a share's NFS adapter configuration

Update the NFS adapter configuration for a share.

Only the flags you supply are changed; omitted flags leave the existing values
intact. Netgroup changes take effect immediately. Changes to squash mode and
authentication flavors (--allow-auth-sys, --require-kerberos) apply on the
next NFS adapter restart.

```
dfsctl share nfs-config set <name> [flags]
```

**Examples:**

```bash
# Restrict access to a specific netgroup
dfsctl share nfs-config set /export --netgroup office-network

# Remove the netgroup restriction (allow all NFS clients)
dfsctl share nfs-config set /export --netgroup ""

# Map root UID to guest on this export
dfsctl share nfs-config set /export --squash root_to_guest

# Require Kerberos authentication and disallow AUTH_SYS
dfsctl share nfs-config set /export --require-kerberos true --allow-auth-sys false
```

Flags:

```
      --allow-auth-sys string     Allow AUTH_SYS flavor (true|false)
      --netgroup string           Netgroup name to associate (empty string clears the association)
      --require-kerberos string   Require Kerberos auth (true|false)
      --squash string             Squash mode (none|root_to_admin|root_to_guest|all_to_admin|all_to_guest)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share nfs-config show`

Show a share's NFS adapter configuration

Show the NFS adapter configuration for a share.

Displays the netgroup association, squash mode, and authentication settings
(AUTH_SYS and Kerberos). Use this command to inspect the current NFS export
settings before making changes with 'nfs-config set'.

```
dfsctl share nfs-config show <name>
```

**Examples:**

```bash
# Show NFS config for a share
dfsctl share nfs-config show /export

# Emit as JSON
dfsctl share nfs-config show /export -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share permission`

Manage share permissions

Manage permissions on shares.

Permission commands allow you to grant, revoke, and list permissions
for users and groups on shares.

**Examples:**

```bash
# Grant read-write permission to a user
dfsctl share permission grant /archive --user alice --level read-write

# Grant read permission to a group
dfsctl share permission grant /archive --group editors --level read

# Revoke permission from a user
dfsctl share permission revoke /archive --user alice

# List permissions on a share
dfsctl share permission list /archive
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share permission grant`

Grant permission on a share

Grant a permission level to a user or group on a share.

Specify exactly one of --user or --group together with --level. Re-running
the command on a principal that already has a permission replaces the existing
level. Permission levels in order of increasing access:

```
- none:       No access (explicitly blocks the principal)
- read:       Read-only access
- read-write: Read and write access
- admin:      Full administrative access including ACL management
```

```
dfsctl share permission grant <share> [flags]
```

**Examples:**

```bash
# Grant read-write access to a specific user
dfsctl share permission grant /archive --user alice --level read-write

# Grant read-only access to a group
dfsctl share permission grant /archive --group editors --level read

# Block a specific user despite a permissive share default
dfsctl share permission grant /archive --user bob --level none

# Grant admin access to a service account
dfsctl share permission grant /archive --user svc-backup --level admin
```

Flags:

```
      --group string   Group name to grant permission to
      --level string   Permission level (none|read|read-write|admin)
      --user string    Username to grant permission to
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share permission list`

List permissions on a share

List all user and group permissions configured on a share.

Each row shows the principal type (user or group), the principal's name, and
the permission level (none, read, read-write, or admin). Note that these are
per-principal overrides; clients without an explicit entry fall back to the
share's default permission (see 'dfsctl share show').

```
dfsctl share permission list <share>
```

**Examples:**

```bash
# List all permissions on a share
dfsctl share permission list /archive

# Emit permissions as JSON for scripting
dfsctl share permission list /archive -o json

# Emit as YAML
dfsctl share permission list /archive -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share permission revoke`

Revoke permission from a share

Remove a per-principal permission entry from a share.

After revoking, the user or group falls back to the share's default permission
level (see 'dfsctl share show'). To explicitly block a principal rather than
fall back to the default, use 'dfsctl share permission grant ... --level none'
instead. Specify exactly one of --user or --group.

```
dfsctl share permission revoke <share> [flags]
```

**Examples:**

```bash
# Revoke a user's explicit permission (they fall back to the share default)
dfsctl share permission revoke /archive --user alice

# Revoke a group's explicit permission
dfsctl share permission revoke /archive --group editors
```

Flags:

```
      --group string   Group name to revoke permission from
      --user string    Username to revoke permission from
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share remove`

Remove a share

Permanently remove a share from the DittoFS server.

Removing a share removes its configuration from the control plane. The
underlying block and metadata stores are NOT deleted — only the share record
that ties them together. This operation is irreversible: you will be prompted
for confirmation unless --force is specified. Disable the share first
('dfsctl share disable') if you want to drain active clients before removing.

```
dfsctl share remove <name> [flags]
```

**Examples:**

```bash
# Remove a share, prompted for confirmation
dfsctl share remove /archive

# Remove without a confirmation prompt (useful in scripts)
dfsctl share remove /archive --force

# Drain clients first, then remove without prompting
dfsctl share disable /archive && dfsctl share remove /archive --force
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share show`

Show share details

Show a detailed, field-by-field view of a single share.

Unlike 'share list', which shows summary columns, 'share show' displays every
attribute of the share: store IDs, read-only state, ACL settings, retention
policy and TTL, cache size overrides, quota, trash (recycle bin) settings, and
creation/update timestamps. Use this command when debugging a misconfigured
share or before editing it.

```
dfsctl share show <name>
```

**Examples:**

```bash
# Show all fields for a share
dfsctl share show /archive

# Emit the full share record as JSON (useful for scripting or diffing)
dfsctl share show /archive -o json

# Emit as YAML
dfsctl share show /archive -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot`

Manage share snapshots (create, list, show, remove, restore)

Manage share snapshots.

A snapshot captures the full state of a share at a point in time. It can
be inspected, listed, removed, or restored back onto a (disabled) share.

**Examples:**

```bash
# Create a snapshot and wait for it to be ready
dfsctl share snapshot create /archive --name weekly

# List snapshots for a share
dfsctl share snapshot list /archive

# Show details of a single snapshot
dfsctl share snapshot show /archive snap-abc123

# Remove a snapshot (prompts for confirmation)
dfsctl share snapshot remove /archive snap-abc123

# Restore a snapshot onto a disabled share
dfsctl share disable /archive
dfsctl share snapshot restore /archive snap-abc123
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot create`

Create a snapshot of a share

Create a snapshot of a share.

By default the command blocks until the snapshot reaches a terminal
state (ready or failed). Use --no-wait to return immediately after the
snapshot is enqueued.

```
dfsctl share snapshot create <share> [flags]
```

**Examples:**

```bash
# Block until snapshot is ready
dfsctl share snapshot create /archive --name weekly

# Return immediately with the new snapshot ID
dfsctl share snapshot create /archive --no-wait

# Skip the remote-durability verify step
dfsctl share snapshot create /archive --no-verify

# Retry a failed previous snapshot
dfsctl share snapshot create /archive --retry snap-prev123
```

Flags:

```
      --name string    Human-readable name for the snapshot
      --no-verify      Skip the remote-durability verify step
      --no-wait        Return immediately instead of waiting for completion
      --retry string   Retry a previous failed snapshot by ID
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot list`

List snapshots for a share

List snapshots for a share, newest-first.

```
dfsctl share snapshot list <share> [flags]
```

**Examples:**

```bash
# List as table
dfsctl share snapshot list /archive

# Filter by state
dfsctl share snapshot list /archive --state ready

# Filter by name prefix
dfsctl share snapshot list /archive --name-prefix weekly

# JSON output
dfsctl share snapshot list /archive -o json
```

Flags:

```
      --name-prefix string   Filter by name prefix
      --no-relative          Print absolute timestamps instead of relative
      --state string         Filter by state (creating|ready|failed)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot remove`

Remove a snapshot

Remove a snapshot. This is irreversible.

```
dfsctl share snapshot remove <share> <id> [flags]
```

**Examples:**

```bash
# Remove with prompt
dfsctl share snapshot remove /archive snap-abc123

# Remove without prompt
dfsctl share snapshot remove /archive snap-abc123 --yes
```

Flags:

```
      --yes   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot restore`

Restore a snapshot into a (disabled) share

Restore a snapshot into a share.

The target share must be disabled first; the command refuses with a
hint when the share is still enabled. Restore is destructive: a safety
snapshot is taken before the reset and its ID is printed on success
(delete it once you have verified the restored share).

```
dfsctl share snapshot restore <share> <id> [flags]
```

**Examples:**

```bash
# Restore with prompt
dfsctl share disable /archive
dfsctl share snapshot restore /archive snap-abc123

# Restore without prompt
dfsctl share snapshot restore /archive snap-abc123 --yes

# Restore a snapshot that is not remotely durable
dfsctl share snapshot restore /archive snap-abc123 --yes --force
```

Flags:

```
      --force   Allow restoring a snapshot that is not remotely durable
      --yes     Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot show`

Show details of a snapshot

Show the full detail record for a single snapshot.

Displays state, remote-durability flag, manifest block count, total dump
size, the ID of any snapshot this was a retry of, and any error message from a
failed run. The snapshot ID can be a full UUID or the 8-character prefix shown
by 'share snapshot list'. Use this to investigate a failed or pending snapshot
before deciding whether to retry or delete it.

```
dfsctl share snapshot show <share> <id>
```

**Examples:**

```bash
# Show a snapshot by its short ID
dfsctl share snapshot show /archive snap-abc1

# Show using the full UUID
dfsctl share snapshot show /archive 3f2a1b4c-0000-0000-0000-000000000001

# Emit the snapshot record as JSON
dfsctl share snapshot show /archive snap-abc1 -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot-policy`

Manage scheduled snapshot policies (schedule + retention)

Manage per-share snapshot policies.

A snapshot policy makes a share snapshot itself automatically on a fixed
interval and prunes old scheduler-created snapshots past the retention
bounds (keep-last and/or ttl). Manually-created snapshots are never pruned.

**Examples:**

```bash
# Daily snapshots, keep the newest 7, drop anything older than 30 days
dfsctl share snapshot-policy set /archive --interval @daily --keep-last 7 --ttl 720h

# Show a share's policy
dfsctl share snapshot-policy show /archive

# List every policy
dfsctl share snapshot-policy list

# Trigger the policy immediately, ignoring its interval
dfsctl share snapshot-policy run /archive

# Remove a share's policy
dfsctl share snapshot-policy remove /archive
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot-policy list`

List all snapshot policies

List every snapshot policy configured across all shares.

Each row shows the share name, interval, keep-last count, TTL age bound, the
name prefix used for scheduler-created snapshots, whether the policy is enabled,
and the next scheduled run time. Use this command to audit which shares have
automatic snapshots active and when they last ran.

```
dfsctl share snapshot-policy list
```

**Examples:**

```bash
# List all snapshot policies as a table
dfsctl share snapshot-policy list

# Emit the full list as JSON for scripting
dfsctl share snapshot-policy list -o json

# Emit as YAML
dfsctl share snapshot-policy list -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot-policy remove`

Remove a share's snapshot policy

Remove the snapshot policy for a share.

Existing snapshots are not removed; only the schedule and automatic pruning
stop. After removal, no new scheduled snapshots will be created and old
scheduled snapshots will no longer be pruned. Use 'snapshot-policy set' to
recreate a policy at any time.

```
dfsctl share snapshot-policy remove <share> [flags]
```

**Examples:**

```bash
# Remove the policy, with a confirmation prompt
dfsctl share snapshot-policy remove /archive

# Remove without a confirmation prompt (useful in scripts)
dfsctl share snapshot-policy remove /archive --yes
```

Flags:

```
      --yes   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot-policy run`

Trigger a share's snapshot policy now (manual override)

Run a share's snapshot policy immediately, ignoring its interval.

This creates a scheduled snapshot now, advances the policy's run clock, and
prunes per the retention bounds (keep-last / TTL). It is useful to take an
out-of-band snapshot before a maintenance window without changing the schedule
or creating a permanent manual snapshot that will never be pruned.

```
dfsctl share snapshot-policy run <share>
```

**Examples:**

```bash
# Trigger the policy for a share immediately
dfsctl share snapshot-policy run /archive

# Trigger and then confirm the snapshot was created
dfsctl share snapshot-policy run /archive && dfsctl share snapshot list /archive --state ready
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot-policy set`

Create or update a share's snapshot policy

Create or update a share's snapshot policy.

--interval accepts a Go duration ("24h", "6h", "1h30m") or a shorthand
(@hourly, @daily, @weekly). Retention is bounded by --keep-last (0 = no
count bound) and --ttl (Go duration, empty = no age bound); a snapshot is
pruned when it falls outside the newest keep-last OR is older than ttl.

Re-running set on an existing policy updates the config but preserves the
run clock (it does not reset the next-run time).

```
dfsctl share snapshot-policy set <share> [flags]
```

**Examples:**

```bash
dfsctl share snapshot-policy set /archive --interval @daily --keep-last 7 --ttl 720h
dfsctl share snapshot-policy set /archive --interval 6h --disabled
```

Flags:

```
      --disabled             Create the policy disabled (no automatic snapshots)
      --interval string      Snapshot cadence: Go duration or @hourly/@daily/@weekly (required)
      --keep-last int        Keep only the newest N scheduled snapshots (0 = unlimited)
      --name-prefix string   Name prefix for scheduler-created snapshots (default "scheduled")
      --ttl string           Prune scheduled snapshots older than this Go duration (empty = no age bound)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share snapshot-policy show`

Show a share's snapshot policy

Show the snapshot policy configured for a share.

Displays the interval, retention bounds (keep-last and TTL), name prefix,
enabled state, and next/last run times. Use this command before editing a
policy to review its current configuration, or to confirm that a policy is
active and when it is next scheduled to run.

```
dfsctl share snapshot-policy show <share>
```

**Examples:**

```bash
# Show the policy for a share
dfsctl share snapshot-policy show /archive

# Emit the policy record as JSON
dfsctl share snapshot-policy show /archive -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share unmount`

Unmount a mounted share

Unmount a DittoFS share from a local mount point.

```
dfsctl share unmount [mountpoint] [flags]
```

**Examples:**

```bash
  # Unmount a share (positional argument is the mount point path)
  dfsctl share unmount /mnt/dittofs

  # Force unmount if busy
  dfsctl share unmount --force /mnt/dittofs

  # Windows: unmount a mapped drive
  dfsctl share unmount Z:

Note: Unmount commands typically require sudo/root privileges on Unix systems.
Unmount identifies the target by mount-point path rather than share name
because a single share can be mounted to multiple local paths; the
`share <name> <verb>` shape therefore does not extend to unmount.
```

Flags:

```
  -f, --force   Force unmount even if busy
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl share warm`

Warm a share's local block cache

Proactively materialize a share's blocks onto the local disk tier.

Starts an asynchronous job that downloads every remote block of the share into
the local cache so subsequent reads are served locally. The command prints the
job id and exits; use --watch to poll until the job completes.

The share must have a remote tier configured. A pinned share with a bounded
local tier may fail with a disk-full error if its working set exceeds the tier.

```
dfsctl share warm <name> [flags]
```

**Examples:**

```bash
# Start a warm job and exit
dfsctl share warm /archive

# Start and follow progress until done
dfsctl share warm --watch /archive
```

Flags:

```
      --watch   Poll the job until it reaches a terminal state
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl status`

Show server status

Display the status of the connected DittoFS server.

Calls the /health endpoint on the server configured in the current context and
reports whether the server is running, how long it has been up, and whether the
control-plane database is reachable. When a valid token is present, per-entity
detail (shares, adapters, stores) is fetched from the API and rendered as a
color-coded table. Use -o json or -o yaml for machine-readable output.

```
dfsctl status
```

**Examples:**

```bash
# Show status of the currently active server
dfsctl status

# Emit machine-readable JSON output
dfsctl status -o json

# Check a specific server without logging in (token fetched from stored context)
dfsctl status --server http://dfs.example.com:8080
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store`

Store management

Manage metadata and block stores on the DittoFS server.

Store commands allow you to create, list, update, and delete stores.
These operations require admin privileges.

**Examples:**

```bash
# List metadata stores
dfsctl store metadata list

# Add a new metadata store
dfsctl store metadata add --name new-meta --type memory

# List local block stores
dfsctl store block local list

# List remote block stores
dfsctl store block remote list

# Add a local block store
dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

# Add a remote block store
dfsctl store block remote add --name s3-store --type s3 --config '{"bucket":"my-bucket"}'
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block`

Block store management

Manage local and remote block stores on the DittoFS server.

Block stores hold file content data as blocks. Local block stores provide
fast disk-backed storage, while remote block stores provide durable cloud
storage (e.g., S3).

**Examples:**

```bash
# List local block stores
dfsctl store block local list

# Add a local filesystem block store
dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

# List remote block stores
dfsctl store block remote list

# Add an S3 remote block store
dfsctl store block remote add --name s3-store --type s3 --config '{"bucket":"my-bucket","region":"us-east-1"}'
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block audit-refcounts`

Verify every manifest block reference has a backing FileChunk row

Run the CAS manifest-consistency audit for the named share.

Walks every file in the share and checks that each block referenced by the
file's manifest (FileAttr.Blocks) has a backing FileChunk row in the
metadata store. A manifest reference with no backing row is a genuine
DANGLING reference — the file claims a chunk the store has no record of, so
a read would return zeros or fail (the silent-data-loss class). The
invariant is "dangling refs == 0"; a non-zero count is real corruption
worth alerting on, so the command exits non-zero (use it as
`audit-refcounts <share> || alert`).

The legacy per-hash RefCount metric (∑ FileChunk.RefCount) was removed:
RefCount is not maintained in the content-addressed-store model (CAS blocks
are written Pending and never transition to Remote), so that sum was
structurally always 0 and produced false-positive "delta" alarms.

Persists last-run summary at <localStore>/audit-state/last-inv02.json
analogously to GC's last-run.json. Operator-invokable; no periodic schedule.

```
dfsctl store block audit-refcounts <share>
```

**Examples:**

```bash
dfsctl store block audit-refcounts myshare
dfsctl store block audit-refcounts myshare -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block evict`

Evict block store data

Evict block store data from local storage.

By default, evicts both read buffer and local disk data for all shares.
Use --read-buffer-only to evict only the read buffer (in-memory).
Use --local-only to evict only local disk data (preserves read buffer).
Use --share to evict a specific share only.

Safety: Eviction of local blocks is refused if no remote store is
configured for a share, since that would cause data loss.

```
dfsctl store block evict [flags]
```

**Examples:**

```bash
# Evict all storage tiers for all shares
dfsctl store block evict

# Evict only read buffer
dfsctl store block evict --read-buffer-only

# Evict only local disk data
dfsctl store block evict --local-only

# Evict data for a specific share
dfsctl store block evict --share /export

# Verbose output
dfsctl store block evict -v
```

Flags:

```
      --local-only         Evict only local disk data (preserves read buffer)
      --read-buffer-only   Evict only read buffer (in-memory)
      --share string       Evict data for a specific share only
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block gc`

Run garbage collection for a block store share

Trigger an on-demand GC run for the named share.

The mark phase enumerates every live ContentHash across all shares whose
remote-store config matches the named share (cross-share aggregation).
The sweep phase reclaims storage absent from the live set: it decrements
the refcount of each dead chunk's enclosing packed block and, once a
block holds no live chunks, deletes it from the remote and evicts its
local copy. Chunks must be dead and older than the configured grace
period (default 1h) before their block is decremented. The
last-run.json summary is persisted under the
share's gc-state directory and can be inspected with:

```
dfsctl store block gc-status <share>
```

The run executes asynchronously on the server (the mark phase can take
minutes on a large or snapshot-heavy deployment). By default this command
polls until the job finishes, rendering progress; pass --no-wait to print
the job id and return immediately.

Use --dry-run to skip deletes and print up to dry_run_sample_size
candidate keys (default 1000). Recommended for first-time deployment
confidence and for debugging suspected mark-phase bugs.

Use --reconcile to additionally reap stranded file_blocks rows — rows
whose owning file was deleted before the unlink-refcount fix, which a
plain GC cannot reclaim because they keep their hashes in the live set.
Reconcile is server-wide (all shares) and the recommended way to recover
space leaked by older versions. Combine with --dry-run to preview.

Use --grace-period to override the configured sweep grace for this run
only. A zero grace (--grace-period 0) reaps every eligible orphan with no
age guard, bypassing the server's 5-minute floor — useful to reclaim
just-orphaned chunks immediately in tests or one-off cleanups. Cannot be
combined with --reconcile.

```
dfsctl store block gc <share> [flags]
```

**Examples:**

```bash
dfsctl store block gc myshare
dfsctl store block gc myshare --dry-run
dfsctl store block gc myshare --reconcile
dfsctl store block gc myshare --grace-period 0
dfsctl store block gc myshare --grace-period 30m
dfsctl store block gc myshare --no-wait
dfsctl store block gc myshare -o json
```

Flags:

```
      --dry-run                 Run mark + sweep enumeration but skip deletes; print candidate keys
      --grace-period duration   Override the sweep grace for this run (e.g. 30m, 0 to reap immediately); bypasses the server's 5m floor. Unset = server default
      --no-wait                 Start the job and print its id without waiting for completion
      --reconcile               Also reap stranded file_blocks rows leaked by older versions (server-wide), then sweep both tiers
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block gc-status`

Show the last block-store GC run summary for a share

Print the most recent garbage collection run summary for the named share.

Reads last-run.json persisted by the most recent completed GC run. Use
this to confirm that the last run swept objects cleanly (ErrorCount == 0),
check how many bytes were freed, and review the duration without tailing
logs. Returns a non-zero exit if no run has been recorded yet (the share
has never been GC'd or its local store has no persistent root).

```
dfsctl store block gc-status <share>
```

**Examples:**

```bash
# Show the last GC summary as a table
dfsctl store block gc-status myshare

# Show as JSON for scripting
dfsctl store block gc-status myshare -o json

# Show as YAML
dfsctl store block gc-status myshare -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block health`

Check block store health

Perform a health check on a block store configuration.

For local filesystem stores, checks if the path exists and is writable.
For local memory stores, always reports healthy.
For remote S3 stores, performs a HeadBucket call to verify connectivity.
For remote memory stores, always reports healthy.

```
dfsctl store block health [flags]
```

**Examples:**

```bash
# Check health of a local block store
dfsctl store block health --kind local --name fs-cache

# Check health of a remote block store
dfsctl store block health --kind remote --name s3-store

# Output as JSON
dfsctl store block health --kind remote --name s3-store -o json
```

Flags:

```
      --kind string   Block store kind: local or remote (required)
      --name string   Block store name (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block local`

Local block store management

Manage local block stores on the DittoFS server.

Local block stores provide fast disk-backed storage for file content blocks.
Supported types: fs (filesystem), memory (testing)

**Examples:**

```bash
# List local block stores
dfsctl store block local list

# Add a filesystem block store
dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

# Add a memory block store (for testing)
dfsctl store block local add --name test-local --type memory
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block local add`

Add a local block store

Add a new local block store to the DittoFS server.

Supported types:

```
- fs: Filesystem-backed block store (fast, persistent)
- memory: In-memory block store (fast, ephemeral, for testing)
```

Type-specific options:

```
fs:
  --path: Block directory path (or prompted interactively)
```

```
dfsctl store block local add [flags]
```

**Examples:**

```bash
# Add a filesystem block store
dfsctl store block local add --name fs-cache --type fs --path /data/blocks

# Add with JSON config
dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

# Add a memory store (for testing)
dfsctl store block local add --name test-local --type memory

# Add interactively (prompts for path)
dfsctl store block local add --name fs-cache --type fs
```

Flags:

```
      --config string   Store configuration as JSON
      --name string     Store name (required)
      --path string     Block directory path (for fs type)
      --type string     Store type: fs, memory (default "fs")
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block local edit`

Edit a local block store

Edit an existing local block store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

```
dfsctl store block local edit <name> [flags]
```

**Examples:**

```bash
# Edit interactively
dfsctl store block local edit default-local

# Update config with JSON
dfsctl store block local edit default-local --config '{"path":"/new/path"}'

# Update path for fs store
dfsctl store block local edit default-local --path /new/path
```

Flags:

```
      --config string   Store configuration as JSON
      --path string     Block directory path (for fs type)
      --type string     Store type: fs, memory
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block local list`

List local block stores

List all local block stores on the DittoFS server.

Shows the name, ID, type (fs or memory), and configuration of each registered
local block store. Other sub-commands accept either form, so this is where you
find both. Use it to confirm which stores exist before adding, editing, or
running health checks against one, or to map the store IDs emitted by
'share show -o json' back to a store name ('share show' table output already
resolves them to names).

```
dfsctl store block local list
```

**Examples:**

```bash
# List as table
dfsctl store block local list

# List as JSON
dfsctl store block local list -o json

# List as YAML
dfsctl store block local list -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block local remove`

Remove a local block store

Remove a local block store from the DittoFS server.

The server refuses removal if any share currently references the store.
Detach the store from all shares first, then remove it. Data on disk is
not deleted by this command. You will be prompted for confirmation unless
--force is specified.

```
dfsctl store block local remove <name> [flags]
```

**Examples:**

```bash
# Remove with confirmation prompt
dfsctl store block local remove fs-cache

# Remove without confirmation
dfsctl store block local remove fs-cache --force

# Verify the store is gone afterward
dfsctl store block local list
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block reconcile`

Report orphaned block storage (read-only; no deletes)

Scan every remote-backed share for orphaned block storage and print a
classified report. This is READ-ONLY: it deletes nothing, decrements nothing,
and changes no markers. Use it to review what the later reclaim stages would
act on before running them.

Four orphan classes are reported:

```
Zero-ref records       Block records with no live locator and a zero live
                       chunk count — a crash between decrementing the count
                       and deleting the record.
Leaked blocks          Block records with no live locator but a non-zero live
                       chunk count — a re-carve moved the hash onto a new
                       block without decrementing this one.
Orphan remote objects  blocks/<id> objects with no backing record, older than
                       the grace window — the upload succeeded but the commit
                       failed. Objects within the grace window are preserved
                       (they may be freshly uploaded, commit pending).
Stranded local chunks  Unsynced, local-durable chunks awaiting upload.
```

Each class reports an exact count plus a bounded sample of IDs (truncated is
flagged when the full set is larger than the sample).

```
dfsctl store block reconcile
```

**Examples:**

```bash
# Report orphans as a table
dfsctl store block reconcile

# As JSON for scripting
dfsctl store block reconcile -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block remote`

Remote block store management

Manage remote block stores on the DittoFS server.

Remote block stores provide durable cloud storage for file content blocks.
Supported types: s3 (AWS S3 or S3-compatible), memory (testing)

**Examples:**

```bash
# List remote block stores
dfsctl store block remote list

# Add an S3 block store
dfsctl store block remote add --name s3-store --type s3 --config '{"bucket":"my-bucket","region":"us-east-1"}'

# Add a memory block store (for testing)
dfsctl store block remote add --name test-remote --type memory
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block remote add`

Add a remote block store

Add a new remote block store to the DittoFS server.

Supported types:

```
- s3: AWS S3 or S3-compatible store (durable, production)
- memory: In-memory store (fast, ephemeral, for testing)
```

Type-specific options:

```
s3:
  --bucket: S3 bucket name (or prompted interactively)
  --region: AWS region (default: us-east-1)
  --endpoint: Custom endpoint for S3-compatible stores
  --prefix: Key prefix within the bucket
  --access-key: AWS access key ID
  --secret-key: AWS secret access key
```

```
dfsctl store block remote add [flags]
```

**Examples:**

```bash
# Add an S3 store with flags
dfsctl store block remote add --name s3-store --type s3 --bucket my-bucket --region us-west-2

# Add an S3 store interactively
dfsctl store block remote add --name s3-store --type s3

# Add a MinIO store (S3-compatible)
dfsctl store block remote add --name minio-store --type s3 --bucket data --endpoint http://localhost:9000

# Add an S3 store with zstd block compression
dfsctl store block remote add --name prod-s3 --type s3 --bucket my-bucket --compression zstd

# Add a memory store (for testing)
dfsctl store block remote add --name test-remote --type memory
```

Flags:

```
      --access-key string                 AWS access key ID (for s3)
      --bucket string                     S3 bucket name (required for s3)
      --compression string                Enable per-block compression: zstd, lz4 (default: off)
      --config string                     Store configuration as JSON
      --encryption-aead string            Enable client-side encryption with the given AEAD: aes-256-gcm, chacha20-poly1305, xchacha20-poly1305
      --encryption-key-file string        Path to local key file (kind=local)
      --encryption-key-kind string        Key provider: local | kmip (required when --encryption-aead is set)
      --encryption-kmip-ca string         KMIP server CA bundle (kind=kmip, optional)
      --encryption-kmip-cert string       KMIP client certificate (kind=kmip)
      --encryption-kmip-endpoint string   KMIP server endpoint host:port (kind=kmip)
      --encryption-kmip-key string        KMIP client private key (kind=kmip)
      --encryption-kmip-key-uid string    KMIP managed symmetric key UID (kind=kmip)
      --endpoint string                   Custom S3 endpoint (for S3-compatible stores)
      --name string                       Store name (required)
      --parallel-uploads int              Max parallel chunk uploads to this remote (0 = adaptive: auto-tune to saturate the uplink)
      --prefix string                     Key prefix within the bucket (for s3)
      --region string                     AWS region (for s3) (default "us-east-1")
      --secret-key string                 AWS secret access key (for s3)
      --type string                       Store type: s3, memory (default "s3")
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block remote edit`

Edit a remote block store

Edit an existing remote block store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

```
dfsctl store block remote edit <name> [flags]
```

**Examples:**

```bash
# Edit interactively
dfsctl store block remote edit s3-store

# Update config with JSON
dfsctl store block remote edit s3-store --config '{"bucket":"new-bucket"}'

# Update S3 settings
dfsctl store block remote edit s3-store --bucket new-bucket --region us-west-2
```

Flags:

```
      --access-key string      AWS access key ID (for s3)
      --bucket string          S3 bucket name (for s3)
      --config string          Store configuration as JSON
      --endpoint string        Custom S3 endpoint
      --parallel-uploads int   Max parallel chunk uploads to this remote (0 = adaptive: auto-tune to saturate the uplink)
      --region string          AWS region (for s3)
      --secret-key string      AWS secret access key (for s3)
      --type string            Store type: s3, memory
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block remote list`

List remote block stores

List all remote block stores on the DittoFS server.

Shows the name, ID, type (s3 or memory), and configuration of each registered
remote block store. Other sub-commands accept either form, so this is where you
find both. Use it to confirm which stores exist before adding, editing, or
running health checks against one, or to map the store IDs emitted by
'share show -o json' back to a store name ('share show' table output already
resolves them to names).

```
dfsctl store block remote list
```

**Examples:**

```bash
# List as table
dfsctl store block remote list

# List as JSON
dfsctl store block remote list -o json

# List as YAML
dfsctl store block remote list -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block remote remove`

Remove a remote block store

Remove a remote block store from the DittoFS server.

The server refuses removal if any share currently references the store.
Detach the store from all shares first, then remove it. No objects are
deleted from the remote bucket by this command. You will be prompted for
confirmation unless --force is specified.

```
dfsctl store block remote remove <name> [flags]
```

**Examples:**

```bash
# Remove with confirmation prompt
dfsctl store block remote remove s3-store

# Remove without confirmation
dfsctl store block remote remove s3-store --force

# Verify the store is gone afterward
dfsctl store block remote list
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store block stats`

Show block store statistics

Display block store statistics.

Without --share, shows aggregated totals across all shares with a per-share breakdown.
With --share, shows statistics for a single share only.

```
dfsctl store block stats [flags]
```

**Examples:**

```bash
# Show aggregated block store stats
dfsctl store block stats

# Show stats for a specific share
dfsctl store block stats --share /export

# Output as JSON
dfsctl store block stats -o json
```

Flags:

```
      --share string   Show stats for a specific share only
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store metadata`

Manage metadata stores

Manage metadata stores on the DittoFS server.

Metadata stores hold file system structure, attributes, and permissions.
Supported types: memory, badger, postgres

**Examples:**

```bash
# List metadata stores
dfsctl store metadata list

# Add a memory store
dfsctl store metadata add --name fast-meta --type memory

# Add a BadgerDB store
dfsctl store metadata add --name persistent-meta --type badger --config '{"path":"/data/meta"}'
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store metadata add`

Add a metadata store

Add a new metadata store to the DittoFS server.

Supported types:

```
- memory: In-memory store (fast, ephemeral)
- badger: BadgerDB store (persistent, embedded)
- postgres: PostgreSQL store (persistent, distributed)
```

Type-specific options:

```
badger:
  --db-path: Path to BadgerDB directory (or prompted interactively)

postgres:
  --config: JSON with connection settings, or omit for interactive prompts
```

```
dfsctl store metadata add [flags]
```

**Examples:**

```bash
# Add a memory store
dfsctl store metadata add --name fast-meta --type memory

# Add a BadgerDB store with flags
dfsctl store metadata add --name persistent-meta --type badger --db-path /data/meta

# Add a BadgerDB store interactively
dfsctl store metadata add --name persistent-meta --type badger

# Add a PostgreSQL store with JSON config
dfsctl store metadata add --name pg-meta --type postgres --config '{"host":"localhost","dbname":"dittofs"}'

# Add a PostgreSQL store interactively
dfsctl store metadata add --name pg-meta --type postgres
```

Flags:

```
      --config string    Store configuration as JSON (for advanced config)
      --db-path string   Database path (required for badger)
      --name string      Store name (required)
      --type string      Store type: memory, badger, postgres (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store metadata edit`

Edit a metadata store

Edit an existing metadata store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

```
dfsctl store metadata edit <name> [flags]
```

**Examples:**

```bash
# Edit interactively (default)
dfsctl store metadata edit default

# Update config with JSON
dfsctl store metadata edit default --config '{"path":"/new/path"}'

# Update type
dfsctl store metadata edit default --type badger

# Update BadgerDB path
dfsctl store metadata edit default --db-path /new/path
```

Flags:

```
      --config string    Store configuration as JSON
      --db-path string   Database path (for badger)
      --type string      Store type: memory, badger, postgres
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store metadata health`

Check metadata store health

Perform a health check on a metadata store.

If the store is loaded in the runtime, calls its native health check method.
Otherwise, reports that the store is not loaded.

```
dfsctl store metadata health [flags]
```

**Examples:**

```bash
# Check health of a metadata store
dfsctl store metadata health --name fast-meta

# Output as JSON
dfsctl store metadata health --name fast-meta -o json
```

Flags:

```
      --name string   Metadata store name (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store metadata list`

List metadata stores

List all metadata stores on the DittoFS server.

Displays the name, ID, and type of every registered metadata store. Other
sub-commands accept either form, so this is where you find both. Use it to
confirm which stores are configured before adding or removing one, or to map the
store IDs emitted by 'share show -o json' back to a store name ('share show'
table output already resolves them to names).

```
dfsctl store metadata list
```

**Examples:**

```bash
# List as table
dfsctl store metadata list

# List as JSON
dfsctl store metadata list -o json

# List as YAML
dfsctl store metadata list -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl store metadata remove`

Remove a metadata store

Remove a metadata store from the DittoFS server.

The server refuses removal if any share currently references the store.
Detach the store from all shares first, then remove it. You will be prompted
for confirmation unless --force is specified.

```
dfsctl store metadata remove <name> [flags]
```

**Examples:**

```bash
# Remove with confirmation prompt
dfsctl store metadata remove fast-meta

# Remove without confirmation
dfsctl store metadata remove fast-meta --force

# Verify the store is gone afterward
dfsctl store metadata list
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl switch-user`

Switch to a different user on the current server

Switch to a different user on the current server without changing the server URL.

Authenticates as the given username against the server in the active context and
stores the resulting tokens under a new context named username@host. If a context
for that user already exists with a non-expired token, it activates it immediately
without re-authenticating. Use dfsctl context to inspect or remove stored contexts.

```
dfsctl switch-user <username> [flags]
```

**Examples:**

```bash
# Switch to a different user (prompts for password)
dfsctl switch-user operator

# Switch to a user providing the password inline
dfsctl switch-user operator -p secret

# Switch back to admin on the same server
dfsctl switch-user admin
```

Flags:

```
  -p, --password string   Password (will prompt if not provided)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl system`

System operations

System-level operations for managing the DittoFS server.

These commands expose low-level server controls that are not tied to a specific share or protocol. Currently available: drain-uploads, which blocks until all queued block-store uploads have completed.

Note: garbage collection (reclaiming space from deleted files) is NOT here — it
runs automatically in the background and is also available on demand via
"dfsctl store block gc <share>" (see the Garbage Collection guide).

**Examples:**

```bash
# Wait for all in-flight uploads to finish (useful before benchmarking)
dfsctl system drain-uploads
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl system drain-uploads`

Wait for all pending uploads to complete

Wait for all in-flight block store uploads to complete across every share.

The command blocks until the server confirms that no blocks are queued for remote upload, or until the server-side timeout (5 minutes) is reached. Use this before running benchmarks or taking snapshots to ensure a clean data boundary.

```
dfsctl system drain-uploads
```

**Examples:**

```bash
# Block until all pending uploads are flushed
dfsctl system drain-uploads

# Get drain result as JSON (includes duration)
dfsctl system drain-uploads -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl trash`

Recycle-bin management

Manage a share's recycle bin (the #recycle virtual directory).

When trash is enabled on a share, deleted files and directories are moved to a per-share recycle bin instead of being permanently purged. Use these commands to inspect what is in the bin, restore individual items to their original location, or purge the bin entirely.

**Examples:**

```bash
# See what is in the recycle bin for a share
dfsctl trash list myshare

# Restore a recycled file to its original path
dfsctl trash restore myshare "#recycle/2026-06-01/report.txt"

# Restore a file to a different path
dfsctl trash restore myshare "#recycle/2026-06-01/report.txt" --to /archive/report.txt

# Permanently empty the recycle bin
dfsctl trash empty myshare --force
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl trash empty`

Empty a share's recycle bin

Permanently remove every entry from a share's recycle bin.

This operation is irreversible — all recycled files and directories are deleted from the server. A confirmation prompt is shown by default; use --force to skip it in non-interactive scripts.

```
dfsctl trash empty <share> [flags]
```

**Examples:**

```bash
# Empty the recycle bin with an interactive confirmation prompt
dfsctl trash empty myshare

# Empty non-interactively (e.g. in a cron job)
dfsctl trash empty myshare --force
```

Flags:

```
      --force   Force empty, skipping server-side safety checks
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl trash list`

List recycle-bin entries for a share

List the recycled entries in a share's recycle bin.

Each row shows the current path under #recycle, the original path before deletion, who deleted it, when, its size, and whether it is a file or directory subtree. Use -o json to get structured output for scripting.

```
dfsctl trash list <share>
```

**Examples:**

```bash
# List the recycle bin for a share as a table
dfsctl trash list myshare

# Get the bin contents as JSON
dfsctl trash list myshare -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl trash restore`

Restore a recycled file or directory

Restore the recycled entry at <bin-path> back into the share.

Without --to the entry is moved back to the path it occupied before deletion. If that location is now taken, use --to to restore it elsewhere. The bin-path argument is the value shown in the PATH column of 'trash list'.

```
dfsctl trash restore <share> <bin-path> [flags]
```

**Examples:**

```bash
# Restore a file to its original location
dfsctl trash restore myshare "#recycle/2026-06-01/report.txt"

# Restore to a different path when the original location is occupied
dfsctl trash restore myshare "#recycle/2026-06-01/report.txt" --to /archive/report.txt
```

Flags:

```
      --to string   Restore to this share-relative path instead of the original location
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl trash status`

Show recycle-bin status for a share

Print a summary of a share's recycle bin: whether trash is enabled, the number of recycled entries, their combined size, and the timestamp of the oldest deletion.

Use this command for a quick health check before deciding whether to empty the bin or restore items. Pass -o json for machine-readable output.

```
dfsctl trash status <share>
```

**Examples:**

```bash
# Show recycle bin status as a summary table
dfsctl trash status myshare

# Get status as JSON for scripting
dfsctl trash status myshare -o json
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl user`

User management

Manage local user accounts on the DittoFS server. Local users are
distinct from identities resolved via Kerberos or LDAP — they are accounts
stored in the DittoFS control plane and used for direct authentication.
Most subcommands require admin privileges; "change-password" operates on the
currently authenticated account and is available to all users.

**Examples:**

```bash
# List all registered users
dfsctl user list

# Create a new user interactively
dfsctl user create

# Create a user with an explicit UID for NFS access
dfsctl user create --username alice --password secret --uid 1000 --role user

# Edit a user's group membership
dfsctl user edit alice --groups editors,viewers

# Reset a user's password as an admin
dfsctl user password alice

# Remove a user (prompts for confirmation)
dfsctl user remove alice
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl user change-password`

Change your own password

Change your own password as the currently authenticated user. This is
distinct from the admin-only "dfsctl user password" command: it verifies your
current password before accepting the new one, and updates the stored session
tokens automatically. You will be prompted for both passwords unless flags are
provided (flags are less secure as passwords may appear in shell history).

```
dfsctl user change-password [flags]
```

**Examples:**

```bash
# Change password interactively (recommended — passwords are not echoed)
dfsctl user change-password

# Change password non-interactively (use with caution)
dfsctl user change-password --current oldpass --new newpass
```

Flags:

```
  -c, --current string   Current password (prompts if not provided)
  -n, --new string       New password (prompts if not provided)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl user create`

Create a new user

Create a new local user on the DittoFS server. The user can be assigned
a Unix UID and primary GID for NFS uid/gid resolution; omitting these causes
the server to auto-assign them from its allocation range. If --username or
--password are not provided as flags, you will be prompted interactively.

```
dfsctl user create [flags]
```

**Examples:**

```bash
# Create a user interactively (prompted for username, password, role, groups)
dfsctl user create

# Create a regular user with username and password
dfsctl user create --username alice --password secret

# Create an admin user belonging to the editors group
dfsctl user create --username admin2 --password secret --role admin --groups editors

# Create a user whose UID/GID match the current host user (useful for NFS mounts)
dfsctl user create --username alice --password secret --host-uid --host-gid

# Create a user with an explicit UID, GID, and email
dfsctl user create --username bob --password secret --uid 1000 --gid 1001 --email bob@example.com
```

Flags:

```
      --email string      Email address
      --enabled           Enable account (default true)
      --gid uint32        Unix primary group ID (auto-assigned if not specified)
      --groups string     Comma-separated list of groups
      --host-gid          Use current host user's GID (for NFS access)
      --host-uid          Use current host user's UID (for NFS access)
  -p, --password string   Password (prompts if not provided)
      --role string       Role (user|admin) (default "user")
      --uid uint32        Unix user ID (auto-assigned if not specified)
  -u, --username string   Username (required)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl user edit`

Edit a user

Edit an existing user on the DittoFS server. When run without flags, an
interactive prompt walks you through each editable field, showing the current
value so you can press Enter to keep it unchanged. When flags are provided,
only the specified fields are updated and no prompt appears.

```
dfsctl user edit <username> [flags]
```

**Examples:**

```bash
# Edit all fields interactively (shows current values)
dfsctl user edit alice

# Move alice to the admin role
dfsctl user edit alice --role admin

# Update alice's primary GID to match a new group
dfsctl user edit alice --gid 1002

# Disable an account and change its group membership in one command
dfsctl user edit alice --enabled false --groups viewers

# Update display name and email
dfsctl user edit alice --display-name "Alice Smith" --email alice@newdomain.com
```

Flags:

```
      --display-name string   Display name
      --email string          Email address
      --enabled string        Enable/disable account (true|false)
      --gid uint32            Unix primary group ID
      --groups string         Comma-separated list of groups
      --role string           Role (user|admin)
      --uid uint32            Unix user ID
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl user get`

Get user details

Get detailed information about a specific user on the DittoFS server.
Accepts either the username or the user's full ID (see 'dfsctl user list').
The output includes the user's role, UID, group memberships, account status,
and last-login timestamp. Use -o json or -o yaml for machine-readable output.

```
dfsctl user get <username|id>
```

**Examples:**

```bash
# Show user details as a table
dfsctl user get alice

# By ID
dfsctl user get 3f2b1c4d-...

# Output as JSON (useful for scripting)
dfsctl user get alice -o json

# Output as YAML
dfsctl user get alice -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl user list`

List all users

List all local users registered on the DittoFS server. The table view
shows each user's username, UID, role, email, group memberships, and whether
the account is enabled. Use -o json or -o yaml to get machine-readable output
suitable for piping into other tools.

```
dfsctl user list
```

**Examples:**

```bash
# List all users as a table
dfsctl user list

# Output the full user list as JSON
dfsctl user list -o json

# Output as YAML
dfsctl user list -o yaml
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl user password`

Reset a user's password

Reset a user's password (admin operation). Unlike "dfsctl user
change-password", this command does not require knowledge of the current
password and is intended for administrators recovering access for a user.
After the reset the account is marked as must-change-password, requiring
the user to set a new password on their next login.

```
dfsctl user password <username> [flags]
```

**Examples:**

```bash
# Reset a user's password interactively (password not echoed)
dfsctl user password alice

# Reset password non-interactively (use with caution)
dfsctl user password alice --password newsecret
```

Flags:

```
  -p, --password string   New password (prompts if not provided)
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl user remove`

Remove a user

Remove a user from the DittoFS server. Accepts either the username or
the user's full ID (see 'dfsctl user list'). This action is irreversible:
the account and its authentication tokens are permanently removed, though
files the user owns are not deleted. You will be prompted for confirmation
unless --force is specified.

```
dfsctl user remove <username|id> [flags]
```

**Examples:**

```bash
# Remove a user (prompts for confirmation)
dfsctl user remove alice

# Remove a user non-interactively (for scripts and automation)
dfsctl user remove alice --force
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

### `dfsctl version`

Show version information

Display the dfsctl build version and system information.

Shows the full semantic version, git commit, build date, Go toolchain version,
and OS/architecture. Use --short to emit only the version string for scripting.

```
dfsctl version [flags]
```

**Examples:**

```bash
# Show full version information
dfsctl version

# Print only the version number (useful in scripts)
dfsctl version --short
```

Flags:

```
      --short   Show only version number
```

Global flags:

```
      --cacert string        Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --client-cert string   Path to a PEM client certificate for mutual TLS (overrides stored)
      --client-key string    Path to the PEM client private key for mutual TLS (overrides stored)
      --no-color             Disable colored output
  -o, --output string        Output format (table|json|yaml) (default "table")
      --server string        Server URL (overrides stored credential)
      --tls-skip-verify      Disable TLS certificate verification (insecure; overrides stored)
      --token string         Bearer token (overrides stored credential)
  -v, --verbose              Enable verbose output
```

