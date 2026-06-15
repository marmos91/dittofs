# CLI Reference

DittoFS ships two binaries:

- **`dfs`** — the server daemon. Runs the protocol adapters and the control-plane API; manages the local config file and the server process.
- **`dfsctl`** — the REST client. Talks to a running `dfs` over its control-plane API to manage users, groups, shares, stores, and adapters.

This page is generated from the command definitions (`go run ./cmd/gendocs`). Do not edit it by hand. Run `dfs <command> --help` or `dfsctl <command> --help` for the same content at the terminal.

## `dfs`

```
dfs
  completion     Generate shell completion script
  config         Configuration management
    edit           Open configuration in editor
    schema         Generate JSON schema for configuration
    show           Display current configuration
    validate       Validate configuration file
  init           Initialize a sample configuration file
  logs           Tail server logs
  migrate        Run database migrations
  migrate-to-cas Migrate legacy .blk block layout to CAS (offline; required for v0.16+ servers)
  start          Start the DittoFS server
  status         Show server status
  stop           Stop the DittoFS server
  version        Show version information
```

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

To load completions:

Bash:
  # Linux:
  $ dfs completion bash > /etc/bash_completion.d/dfs
  # macOS:
  $ dfs completion bash > $(brew --prefix)/etc/bash_completion.d/dfs

Zsh:
  # If shell completion is not already enabled in your environment,
  # you will need to enable it. You can execute the following once:
  $ echo "autoload -U compinit; compinit" >> ~/.zshrc

  # To load completions for each session, execute once:
  # Linux:
  $ dfs completion zsh > "${fpath[1]}/_dfs"
  # macOS:
  $ dfs completion zsh > $(brew --prefix)/share/zsh/site-functions/_dfs

  # You will need to start a new shell for this setup to take effect.

Fish:
  $ dfs completion fish > ~/.config/fish/completions/dfs.fish

PowerShell:
  PS> dfs completion powershell | Out-String | Invoke-Expression

  # To load completions for every new session, run:
  PS> dfs completion powershell > dfs.ps1
  # and source this file from your PowerShell profile.


```
dfs completion [bash|zsh|fish|powershell]
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
  edit      Open configuration in editor
  validate  Validate configuration file
  show      Display current configuration
  schema    Generate JSON schema for IDE/validation

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config edit`

Open configuration in editor

Open the configuration file in your default editor.

Uses the EDITOR environment variable, falling back to 'vi' if not set.

Examples:
  # Edit default config
  dfs config edit

  # Edit specific config file
  dfs config edit --config /etc/dittofs/config.yaml

```
dfs config edit
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config schema`

Generate JSON schema for configuration

Generate a JSON schema for the DittoFS configuration file.

The schema can be used for:
  - IDE autocompletion (VS Code, IntelliJ, etc.)
  - Configuration file validation
  - Documentation generation

Examples:
  # Print schema to stdout
  dfs config schema

  # Save schema to file
  dfs config schema --output config.schema.json

```
dfs config schema [flags]
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

Examples:
  # Show default config as YAML
  dfs config show

  # Show as JSON
  dfs config show --output json

  # Show specific config file
  dfs config show --config /etc/dittofs/config.yaml

  # Show auto-deduced block store defaults
  dfs config show --deduced

```
dfs config show [flags]
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

Examples:
  # Validate default config
  dfs config validate

  # Validate specific config file
  dfs config validate --config /etc/dittofs/config.yaml

```
dfs config validate
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs init`

Initialize a sample configuration file

Initialize a sample DittoFS configuration file.

By default, the configuration file is created at $XDG_CONFIG_HOME/dittofs/config.yaml.
Use --config to specify a custom path.

Examples:
  # Initialize with default location
  dfs init

  # Initialize with custom path
  dfs init --config /etc/dittofs/config.yaml

  # Force overwrite existing config
  dfs init --force

```
dfs init [flags]
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

Display and optionally follow the DittoFS server logs.

This command reads the log file specified in the configuration and displays
the most recent entries. If the server logs to stdout/stderr, this command
will indicate that logs are not available in a file.

Examples:
  # Show last 100 lines (default)
  dfs logs

  # Show last 50 lines
  dfs logs -n 50

  # Follow logs in real-time
  dfs logs -f

  # Show logs since a specific time
  dfs logs --since "2024-01-15T10:00:00Z"

  # Combine options
  dfs logs -f -n 20

```
dfs logs [flags]
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

This command applies pending database migrations to the configured control plane
database (SQLite or PostgreSQL). It is required after upgrading DittoFS when
schema changes have been made.

Examples:
  # Run migrations with default config
  dittofs migrate

  # Run migrations with custom config
  dittofs migrate --config /etc/dittofs/config.yaml

```
dfs migrate
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs migrate-to-cas`

Migrate legacy .blk block layout to CAS (offline; required for v0.16+ servers)

Migrate a stopped DittoFS server's legacy .blk block layout to the
content-addressed (CAS) layout required by v0.16+.

The dfs server MUST be stopped before running this command — the
migration rewrites the on-disk layout in place and a concurrent server
would race the rename and corrupt the store.

Required flag: --storage-dir <root>. The storage root is expected to
contain a shares/<name>/blocks/ subtree per share (legacy v0.13 layout);
the command refuses to start if the path is missing or empty.

The command is idempotent: a per-share journal at
<storage-dir>/shares/<name>/.dittofs-migrate-to-cas.state lets you
resume after a crash or Ctrl-C without re-uploading already-migrated
chunks. Successful completion writes
<storage-dir>/shares/<name>/.cas-migrated-v1 via atomic rename;
the boot guard refuses to start dfs until this sentinel exists (exit
code 78).

Use --dry-run for a non-destructive preview (file count, estimated
dedup ratio, sampled bytes-per-second).
Use --share to scope the run to one share.
Use --json to emit one JSON object per second of progress on stdout.

See docs/CONFIGURATION.md §migration for the full operator runbook.

```
dfs migrate-to-cas [flags]
```

Flags:

```
      --dry-run               Walk + sample only; report file count, bytes, estimated dedup ratio, ETA. Writes nothing.
      --json                  Emit one JSON object per second of progress to stdout (machine-parseable)
      --max-disk int          Per-share max-disk budget for the destination FSStore (0 = unlimited)
      --max-memory int        Per-share max-memory budget for the destination FSStore (0 = 256 MiB default)
      --metadata-dir string   Path to the badger metadata database directory (REQUIRED; the directory passed to the metadata store's 'path' config)
      --share string          Scope migration to one share (default: all shares discovered under <storage-dir>/shares/)
      --storage-dir string    Storage root (REQUIRED; expects <root>/shares/<name>/blocks layout)
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs start`

Start the DittoFS server

Start the DittoFS server with the specified configuration.

By default, the server runs in the background (daemon mode). Use --foreground
to run in the foreground for debugging or when managed by a process supervisor.

Use --config to specify a custom configuration file, or it will use the
default location at $XDG_CONFIG_HOME/dittofs/config.yaml.

Examples:
  # Start in background (default)
  dfs start

  # Start in foreground
  dfs start --foreground

  # Start with custom config file
  dfs start --config /etc/dittofs/config.yaml

  # Start with environment variable overrides
  DITTOFS_LOGGING_LEVEL=DEBUG dfs start --foreground

```
dfs start [flags]
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

This command checks the server health by calling the health endpoint
and displays status, uptime, and control plane DB reachability.

When an API token is provided (--api-token or DFS_API_TOKEN), per-entity
status is fetched from the list endpoints and displayed as a color-coded table.

Examples:
  # Check status (uses default settings)
  dfs status

  # Check status with custom API port
  dfs status --api-port 9080

  # Check status with per-entity details
  dfs status --api-token <token>

  # Output as JSON
  dfs status --output json

```
dfs status [flags]
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

By default, sends a graceful shutdown signal. Use --force for immediate
termination.

Examples:
  # Stop server (uses default PID file)
  dfs stop

  # Stop server using custom PID file
  dfs stop --pid-file /var/run/dittofs.pid

  # Force stop
  dfs stop --force

```
dfs stop [flags]
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

Display the DittoFS version, build information, and system details.

```
dfs version [flags]
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

```
dfsctl
  adapter        Protocol adapter management
    disable        Disable an adapter
    edit           Edit an adapter
    enable         Enable an adapter
    list           List protocol adapters
    settings       Manage adapter settings
      nfs            Manage NFS adapter settings
        reset          Reset adapter settings to defaults
        show           Show current adapter settings
        update         Update adapter settings
      smb            Manage SMB adapter settings
        reset          Reset adapter settings to defaults
        show           Show current adapter settings
        update         Update adapter settings
  bench          Run filesystem benchmarks
    compare        Compare benchmark results from multiple systems
    run            Run filesystem benchmarks
    storage-tiers  Benchmark storage tier performance (cold/warm/local-only)
  client         Manage connected clients
    disconnect     Disconnect a client
    list           List connected clients
    sessions       Manage NFS client sessions
      destroy        Force-destroy a session
      list           List sessions for a client
  completion     Generate shell completion script
  context        Manage server contexts
    current        Show current context
    delete         Delete a context
    list           List all configured contexts
    rename         Rename a context
    use            Switch to a different context
  grace          Manage NFSv4 grace period
    end            Force-end the grace period
    status         Show grace period status
  group          Group management
    add-user       Add a user to a group
    create         Create a new group
    delete         Delete a group
    edit           Edit a group
    get            Get group details
    list           List all groups
    remove-user    Remove a user from a group
  idmap          Manage identity mappings
    add            Add an identity mapping
    list           List identity mappings
    remove         Remove an identity mapping
  login          Authenticate with DittoFS server
  logout         Clear stored credentials
  netgroup       Manage netgroups (IP access control)
    add-member     Add a member to a netgroup
    create         Create a new netgroup
    delete         Delete a netgroup
    list           List all netgroups
    remove-member  Remove a member from a netgroup
    show           Show netgroup details
  quota          Per-identity quota management
    list           List all quotas on a share
    rm             Remove a per-identity quota
    set            Create or update a per-identity quota
  settings       Server settings management
    get            Get a setting value
    list           List all settings
    set            Set a setting value
  share          Share management
    create         Create a new share
    delete         Delete a share
    disable        Disable a share (drain clients, block new connections)
    edit           Edit a share
    enable         Enable a share (accept new connections)
    list           List all shares
    list-mounts    List mounted DittoFS shares
    mount          Mount a share via NFS or SMB
    nfs-config     Manage per-share NFS adapter configuration
      set            Update a share's NFS adapter configuration
      show           Show a share's NFS adapter configuration
    permission     Manage share permissions
      grant          Grant permission on a share
      list           List permissions on a share
      revoke         Revoke permission from a share
    show           Show share details
    snapshot       Manage share snapshots (create, list, show, delete, restore)
      create         Create a snapshot of a share
      delete         Delete a snapshot
      list           List snapshots for a share
      restore        Restore a snapshot into a (disabled) share
      show           Show details of a snapshot
    snapshot-policy Manage scheduled snapshot policies (schedule + retention)
      delete         Delete a share's snapshot policy
      list           List all snapshot policies
      run            Trigger a share's snapshot policy now (manual override)
      set            Create or update a share's snapshot policy
      show           Show a share's snapshot policy
    unmount        Unmount a mounted share
  status         Show server status
  store          Store management
    block          Block store management
      audit-refcounts Verify FileBlock.RefCount matches FileAttr.Blocks references
      evict          Evict block store data
      gc             Run garbage collection for a block store share
      gc-status      Show the last block-store GC run summary for a share
      health         Check block store health
      local          Local block store management
        add            Add a local block store
        edit           Edit a local block store
        list           List local block stores
        remove         Remove a local block store
      remote         Remote block store management
        add            Add a remote block store
        edit           Edit a remote block store
        list           List remote block stores
        remove         Remove a remote block store
      stats          Show block store statistics
    metadata       Manage metadata stores
      add            Add a metadata store
      edit           Edit a metadata store
      health         Check metadata store health
      list           List metadata stores
      remove         Remove a metadata store
  switch-user    Switch to a different user on the current server
  system         System operations
    drain-uploads  Wait for all pending uploads to complete
  trash          Recycle-bin management
    empty          Empty a share's recycle bin
    list           List recycle-bin entries for a share
    restore        Restore a recycled file or directory
    status         Show recycle-bin status for a share
  user           User management
    change-password Change your own password
    create         Create a new user
    delete         Delete a user
    edit           Edit a user
    get            Get user details
    list           List all users
    password       Reset a user's password
  version        Show version information
```

### `dfsctl`

DittoFS Control - Remote management client

dfsctl is the command-line client for managing DittoFS servers remotely.

Use this tool to manage users, groups, shares, stores, and server settings
through the DittoFS REST API.

Use "dfsctl [command] --help" for more information about a command.

Flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter`

Protocol adapter management

Manage protocol adapters on the DittoFS server.

Adapter commands allow you to list, enable, disable, and edit protocol adapters.
These operations require admin privileges.

Examples:
  # List adapters
  dfsctl adapter list

  # Enable NFS adapter on port 12049
  dfsctl adapter enable nfs --port 12049

  # Disable SMB adapter
  dfsctl adapter disable smb

  # Edit adapter interactively
  dfsctl adapter edit nfs

  # Edit adapter settings with flags
  dfsctl adapter edit nfs --port 3049

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter disable`

Disable an adapter

Disable a protocol adapter on the DittoFS server.

Examples:
  # Disable NFS adapter
  dfsctl adapter disable nfs

  # Disable SMB adapter
  dfsctl adapter disable smb

```
dfsctl adapter disable <type>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter edit`

Edit an adapter

Edit an existing protocol adapter on the DittoFS server.

When run without flags, opens an interactive editor to modify adapter properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit adapter interactively
  dfsctl adapter edit nfs

  # Update port directly
  dfsctl adapter edit nfs --port 3049

  # Enable/disable adapter
  dfsctl adapter edit smb --enabled false

  # Update configuration
  dfsctl adapter edit nfs --config '{"read_size":65536}'

```
dfsctl adapter edit <type> [flags]
```

Flags:

```
      --config string    Adapter configuration as JSON
      --enabled string   Enable/disable adapter (true|false)
      --port int         Listen port
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter enable`

Enable an adapter

Enable a protocol adapter on the DittoFS server.

If the adapter doesn't exist, it will be created.

Examples:
  # Enable NFS adapter with default port
  dfsctl adapter enable nfs

  # Enable NFS adapter on specific port
  dfsctl adapter enable nfs --port 2049

  # Enable SMB adapter
  dfsctl adapter enable smb --port 445

```
dfsctl adapter enable <type> [flags]
```

Flags:

```
      --port int   Listen port (uses default if not specified)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter list`

List protocol adapters

List all protocol adapters on the DittoFS server.

Examples:
  # List as table
  dfsctl adapter list

  # List as JSON
  dfsctl adapter list -o json

```
dfsctl adapter list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter settings`

Manage adapter settings

Manage protocol adapter settings on the DittoFS server.

The adapter type (nfs or smb) must be specified as the first argument.

Examples:
  # Show NFS adapter settings
  dfsctl adapter settings nfs show

  # Update NFS lease time
  dfsctl adapter settings nfs update --lease-time 120

  # Reset all NFS settings to defaults
  dfsctl adapter settings nfs reset

  # Reset a specific NFS setting
  dfsctl adapter settings nfs reset --setting lease_time

```
dfsctl adapter settings <type>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter settings nfs`

Manage NFS adapter settings

Manage NFS protocol adapter settings.

```
dfsctl adapter settings nfs
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter settings nfs reset`

Reset adapter settings to defaults

Reset adapter settings to their default values.

If --setting is specified, only that setting is reset. Otherwise, all settings
are reset to defaults.

Examples:
  # Reset all NFS settings
  dfsctl adapter settings nfs reset

  # Reset only lease_time
  dfsctl adapter settings nfs reset --setting lease_time

```
dfsctl adapter settings nfs reset [flags]
```

Flags:

```
  -f, --force            Skip confirmation prompt
      --setting string   Reset a specific setting (omit to reset all)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter settings nfs show`

Show current adapter settings

Show current adapter settings with defaults comparison.

Non-default values are marked with '*'.

Examples:
  # Show NFS settings
  dfsctl adapter settings nfs show

  # Show SMB settings as JSON
  dfsctl adapter settings smb show -o json

```
dfsctl adapter settings nfs show
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter settings nfs update`

Update adapter settings

Update adapter settings with partial changes.

Only specified flags are included in the update. Unspecified settings are not changed.

Examples:
  # Update NFS lease time
  dfsctl adapter settings nfs update --lease-time 120

  # Validate without applying
  dfsctl adapter settings nfs update --lease-time 120 --dry-run

  # Bypass range validation
  dfsctl adapter settings nfs update --lease-time 999 --force

```
dfsctl adapter settings nfs update [flags]
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
      --preferred-transfer-size int          Preferred transfer size in bytes
      --v4-max-connections-per-session int   Maximum connections per NFSv4.1 session (0=unlimited)
      --v4-max-minor-version int             Maximum NFSv4 minor version (0=v4.0, 1=v4.1)
      --v4-min-minor-version int             Minimum NFSv4 minor version (0=v4.0, 1=v4.1)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter settings smb`

Manage SMB adapter settings

Manage SMB protocol adapter settings.

```
dfsctl adapter settings smb
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter settings smb reset`

Reset adapter settings to defaults

Reset adapter settings to their default values.

If --setting is specified, only that setting is reset. Otherwise, all settings
are reset to defaults.

Examples:
  # Reset all NFS settings
  dfsctl adapter settings nfs reset

  # Reset only lease_time
  dfsctl adapter settings nfs reset --setting lease_time

```
dfsctl adapter settings smb reset [flags]
```

Flags:

```
  -f, --force            Skip confirmation prompt
      --setting string   Reset a specific setting (omit to reset all)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter settings smb show`

Show current adapter settings

Show current adapter settings with defaults comparison.

Non-default values are marked with '*'.

Examples:
  # Show NFS settings
  dfsctl adapter settings nfs show

  # Show SMB settings as JSON
  dfsctl adapter settings smb show -o json

```
dfsctl adapter settings smb show
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl adapter settings smb update`

Update adapter settings

Update adapter settings with partial changes.

Only specified flags are included in the update. Unspecified settings are not changed.

Examples:
  # Update NFS lease time
  dfsctl adapter settings nfs update --lease-time 120

  # Validate without applying
  dfsctl adapter settings nfs update --lease-time 120 --dry-run

  # Bypass range validation
  dfsctl adapter settings nfs update --lease-time 999 --force

```
dfsctl adapter settings smb update [flags]
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
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl bench`

Run filesystem benchmarks

Run I/O and metadata benchmarks against any mounted filesystem.

Benchmarks operate directly on the filesystem — no API authentication required.
Use this to measure DittoFS performance or compare against other NFS/SMB servers.

Examples:
  # Run all benchmarks on a mounted NFS share
  dfsctl bench run /mnt/bench

  # Run with custom parameters
  dfsctl bench run /mnt/bench --threads 8 --file-size 512MiB --duration 30s

  # Run specific workloads and save results
  dfsctl bench run /mnt/bench --workload seq-write,seq-read --system dittofs --save results.json

  # Compare results from multiple systems
  dfsctl bench compare dittofs.json kernel-nfs.json ganesha.json

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl bench compare`

Compare benchmark results from multiple systems

Load two or more JSON result files and render a side-by-side comparison table.

Examples:
  # Compare DittoFS vs kernel NFS
  dfsctl bench compare results/dittofs.json results/kernel-nfs.json

  # Compare all results
  dfsctl bench compare results/*.json

```
dfsctl bench compare FILE [FILE...]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl bench run`

Run filesystem benchmarks

Run I/O and metadata benchmarks against the given directory.

No API authentication is required — this operates purely on the filesystem.

Examples:
  # Run all benchmarks with defaults
  dfsctl bench run /mnt/bench

  # Run specific workloads with custom parameters
  dfsctl bench run /mnt/bench --workload seq-write,seq-read --threads 8

  # Save results for later comparison
  dfsctl bench run /mnt/bench --system dittofs --save results/dittofs.json

```
dfsctl bench run PATH [flags]
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl bench storage-tiers`

Benchmark storage tier performance (cold/warm/local-only)

Run a 6-step storage tier benchmark that measures read performance at each
storage level by selectively evicting layers between reads.

This workload requires:
  - An authenticated session (block store eviction requires admin access)
  - A DittoFS server with a mounted share
  - The share must be configured with a remote store for cold read testing

The benchmark runs the following steps for each file size:
  1. Write: Create test file via NFS/SMB mount
  2. Evict all: Clear read buffer + local store via API
  3. Cold read: Read file (data from remote store)
  4. Warm read: Read file again (data in read buffer + local store)
  5. Evict read buffer: Clear memory read buffer only via API
  6. Local-only read: Read file (data from local FS store only)

Results show throughput and read buffer hit rate per step.

Examples:
  # Run with default sizes (10MB, 100MB, 1GB)
  dfsctl bench storage-tiers --share /export --mount /mnt/test

  # Custom file sizes
  dfsctl bench storage-tiers --share /export --mount /mnt/test --sizes 1MB,10MB,50MB

```
dfsctl bench storage-tiers [flags]
```

Flags:

```
      --mount string   Mount point for file I/O (default "/mnt/test")
      --share string   Share name for block store API operations (required)
      --sizes string   Comma-separated file sizes (default: 10MB,100MB,1GB)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl client`

Manage connected clients

Manage connected NFS and SMB clients on the DittoFS server.

Client commands allow you to list connected clients across all protocols
and disconnect misbehaving ones. These operations require admin privileges.

Examples:
  # List all connected clients
  dfsctl client list

  # List only NFS clients
  dfsctl client list --protocol nfs

  # List clients on a specific share
  dfsctl client list --share /export

  # Disconnect a client by ID
  dfsctl client disconnect nfs-42

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl client disconnect`

Disconnect a client

Disconnect a connected client by its ID.

This performs protocol-specific teardown: for NFS clients it closes the TCP
connection and triggers state revocation; for SMB clients it triggers session
cleanup. Use with caution.

Examples:
  # Disconnect a client (with confirmation prompt)
  dfsctl client disconnect nfs-42

  # Disconnect without confirmation
  dfsctl client disconnect nfs-42 --force

```
dfsctl client disconnect <client-id> [flags]
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl client list`

List connected clients

List all connected clients on the DittoFS server.

Displays NFS and SMB clients with their protocol, address,
user, shares, and connection duration.

Examples:
  # List as table
  dfsctl client list

  # Filter by protocol
  dfsctl client list --protocol nfs

  # Filter by share
  dfsctl client list --share /export

  # List as JSON
  dfsctl client list -o json

```
dfsctl client list [flags]
```

Flags:

```
      --protocol string   Filter by protocol (nfs, smb)
      --share string      Filter by share name
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl client sessions`

Manage NFS client sessions

Manage NFSv4.1 sessions for a connected NFS client.

Session commands allow you to list active sessions and force-destroy
misbehaving sessions. These operations require admin privileges.

Examples:
  # List sessions for a client
  dfsctl client sessions list 0000000100000001

  # Force-destroy a session
  dfsctl client sessions destroy 0000000100000001 a1b2c3d4...

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl client sessions destroy`

Force-destroy a session

Force-destroy an NFSv4.1 session by client ID and session ID.

This will forcefully tear down the session, bypassing in-flight request checks.
Use with caution -- the NFS client may experience errors.

Examples:
  # Destroy a session (with confirmation prompt)
  dfsctl client sessions destroy 0000000100000001 a1b2c3d4e5f6a7b8...

  # Destroy without confirmation
  dfsctl client sessions destroy 0000000100000001 a1b2c3d4e5f6a7b8... --force

```
dfsctl client sessions destroy <client-id> <session-id> [flags]
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl client sessions list`

List sessions for a client

List all NFSv4.1 sessions for a given client by its hex-encoded client ID.

Examples:
  # List sessions as table
  dfsctl client sessions list 0000000100000001

  # List sessions as JSON
  dfsctl client sessions list 0000000100000001 -o json

```
dfsctl client sessions list <client-id>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl completion`

Generate shell completion script

Generate shell completion script for dfsctl.

To load completions:

Bash:
  # Linux:
  $ dfsctl completion bash > /etc/bash_completion.d/dfsctl
  # macOS:
  $ dfsctl completion bash > $(brew --prefix)/etc/bash_completion.d/dfsctl

Zsh:
  # If shell completion is not already enabled in your environment,
  # you will need to enable it. You can execute the following once:
  $ echo "autoload -U compinit; compinit" >> ~/.zshrc

  # To load completions for each session, execute once:
  # Linux:
  $ dfsctl completion zsh > "${fpath[1]}/_dfsctl"
  # macOS:
  $ dfsctl completion zsh > $(brew --prefix)/share/zsh/site-functions/_dfsctl

  # You will need to start a new shell for this setup to take effect.

Fish:
  $ dfsctl completion fish > ~/.config/fish/completions/dfsctl.fish

PowerShell:
  PS> dfsctl completion powershell | Out-String | Invoke-Expression

  # To load completions for every new session, run:
  PS> dfsctl completion powershell > dfsctl.ps1
  # and source this file from your PowerShell profile.


```
dfsctl completion [bash|zsh|fish|powershell]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl context`

Manage server contexts

Manage connection contexts for multiple DittoFS servers.

Contexts allow you to save and switch between different server configurations,
similar to kubectl contexts.

Subcommands:
  list     List all configured contexts
  use      Switch to a different context
  current  Show current context
  rename   Rename a context
  delete   Delete a context

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl context current`

Show current context

Display information about the current active context.

Examples:
  # Show current context
  dfsctl context current

  # Show as JSON
  dfsctl context current --output json

```
dfsctl context current [flags]
```

Flags:

```
  -o, --output string   Output format (table|json|yaml) (default "table")
```

Global flags:

```
      --no-color        Disable colored output
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl context delete`

Delete a context

Delete a server context.

This removes the saved configuration and credentials for the context.

Examples:
  # Delete context named "staging"
  dfsctl context delete staging

  # Delete without confirmation
  dfsctl context delete staging --force

```
dfsctl context delete <name> [flags]
```

Flags:

```
  -f, --force   Skip confirmation
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl context list`

List all configured contexts

List all configured server contexts.

Shows the context name, server URL, and username for each saved context.
The current context is marked with an asterisk (*).

Examples:
  # List contexts as table
  dfsctl context list

  # List as JSON
  dfsctl context list -o json

```
dfsctl context list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl context rename`

Rename a context

Rename an existing server context.

Examples:
  # Rename context from "default" to "production"
  dfsctl context rename default production

```
dfsctl context rename <old-name> <new-name>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl context use`

Switch to a different context

Switch to a different server context.

This changes the active context used for subsequent commands.

Examples:
  # Switch to context named "production"
  dfsctl context use production

```
dfsctl context use <name>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl grace`

Manage NFSv4 grace period

Manage the NFSv4 grace period on the DittoFS server.

Grace period commands allow you to monitor and control the NFSv4 grace
period that occurs after server restart. During the grace period, clients
reclaim their previously-held state (open files, locks).

Examples:
  # Check grace period status
  dfsctl grace status

  # Check status in JSON format
  dfsctl grace status -o json

  # Force-end the grace period
  dfsctl grace end

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl grace end`

Force-end the grace period

Force-end the NFSv4 grace period immediately.

This is an admin-only operation that immediately ends the grace period,
allowing new state-creating operations to proceed. Use this for fast
recovery in development and testing environments.

Examples:
  # Force-end the grace period
  dfsctl grace end

```
dfsctl grace end
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl grace status`

Show grace period status

Display the current NFSv4 grace period status.

Shows whether a grace period is active, time remaining, and client
reclaim progress. The grace period occurs after server restart to
allow clients to reclaim their previously-held state.

Examples:
  # Show status as table
  dfsctl grace status

  # Show status as JSON
  dfsctl grace status -o json

  # Show status as YAML
  dfsctl grace status -o yaml

```
dfsctl grace status
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl group`

Group management

Manage groups on the DittoFS server.

Group commands allow you to create, list, get, edit, and delete groups,
as well as manage group membership.
These operations require admin privileges.

Examples:
  # List all groups
  dfsctl group list

  # Get group details
  dfsctl group get admins

  # Create a new group
  dfsctl group create --name editors

  # Edit a group interactively
  dfsctl group edit editors

  # Add a user to a group
  dfsctl group add-user editors alice

  # Remove a user from a group
  dfsctl group remove-user editors alice

  # Delete a group
  dfsctl group delete editors

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl group add-user`

Add a user to a group

Add a user to a group on the DittoFS server.

Examples:
  # Add user alice to group editors
  dfsctl group add-user editors alice

```
dfsctl group add-user <group> <username>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl group create`

Create a new group

Create a new group on the DittoFS server.

Examples:
  # Create a group
  dfsctl group create --name editors

  # Create a group with specific GID
  dfsctl group create --name editors --gid 1001

  # Create a group with description
  dfsctl group create --name editors --description "Content editors"

```
dfsctl group create [flags]
```

Flags:

```
      --description string   Group description
      --gid uint32           Group ID (auto-generated if not set)
      --name string          Group name (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl group delete`

Delete a group

Delete a group from the DittoFS server.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Delete group with confirmation
  dfsctl group delete editors

  # Delete group without confirmation
  dfsctl group delete editors --force

```
dfsctl group delete <name> [flags]
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl group edit`

Edit a group

Edit an existing group on the DittoFS server.

When run without flags, opens an interactive editor to modify group properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit group interactively
  dfsctl group edit editors

  # Update GID directly
  dfsctl group edit editors --gid 1002

  # Update description
  dfsctl group edit editors --description "New description"

```
dfsctl group edit <name> [flags]
```

Flags:

```
      --description string   Group description
      --gid uint32           Group ID
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl group get`

Get group details

Get detailed information about a group.

Examples:
  # Get group details as table
  dfsctl group get admins

  # Get as JSON
  dfsctl group get admins -o json

```
dfsctl group get <name>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl group list`

List all groups

List all groups on the DittoFS server.

Examples:
  # List groups as table
  dfsctl group list

  # List as JSON
  dfsctl group list -o json

  # List as YAML
  dfsctl group list -o yaml

```
dfsctl group list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl group remove-user`

Remove a user from a group

Remove a user from a group on the DittoFS server.

Examples:
  # Remove user alice from group editors
  dfsctl group remove-user editors alice

```
dfsctl group remove-user <group> <username>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl idmap`

Manage identity mappings

Manage identity mappings (authentication principal to control plane user).

Identity mappings allow you to associate authentication principals with
local DittoFS user accounts. This works across protocols:

  NFS/Kerberos:  alice@EXAMPLE.COM
  SMB/NTLM:      CORP\alice
  SMB/Kerberos:  alice@CORP.COM

Mappings are shared across NFS and SMB, ensuring consistent uid/gid
resolution in mixed-protocol deployments.

Examples:
  # List all identity mappings
  dfsctl idmap list

  # Map a Kerberos principal (works for both NFS and SMB)
  dfsctl idmap add --principal alice@EXAMPLE.COM --username alice

  # Map an NTLM domain user
  dfsctl idmap add --principal 'CORP\alice' --username alice

  # Remove a mapping
  dfsctl idmap remove --principal alice@EXAMPLE.COM

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl idmap add`

Add an identity mapping

Add a new identity mapping from an external identity to a DittoFS user.

Examples:
  # Map a Kerberos principal to a local user
  dfsctl idmap add --principal alice@EXAMPLE.COM --username alice

  # Map with explicit provider
  dfsctl idmap add --provider kerberos --principal admin@CORP.COM --username alice

  # Map a numeric UID principal
  dfsctl idmap add --principal 1000@localdomain --username bob

```
dfsctl idmap add [flags]
```

Flags:

```
      --principal string   External identity (e.g., alice@EXAMPLE.COM)
      --provider string    Identity provider (e.g., kerberos, oidc, ad) (default "kerberos")
      --username string    DittoFS username
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl idmap list`

List identity mappings

List identity mappings on the DittoFS server.

Examples:
  # List all mappings
  dfsctl idmap list

  # List only Kerberos mappings
  dfsctl idmap list --provider kerberos

  # List as JSON
  dfsctl idmap list -o json

```
dfsctl idmap list [flags]
```

Flags:

```
      --provider string   Filter by identity provider (e.g., kerberos, oidc, ad)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl idmap remove`

Remove an identity mapping

Remove an identity mapping by provider and principal.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Remove with confirmation
  dfsctl idmap remove --principal alice@EXAMPLE.COM

  # Remove with explicit provider
  dfsctl idmap remove --provider kerberos --principal alice@EXAMPLE.COM

  # Remove without confirmation
  dfsctl idmap remove --principal alice@EXAMPLE.COM --force

```
dfsctl idmap remove [flags]
```

Flags:

```
  -f, --force              Skip confirmation prompt
      --principal string   External identity to remove
      --provider string    Identity provider (e.g., kerberos, oidc, ad) (default "kerberos")
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl login`

Authenticate with DittoFS server

Authenticate with a DittoFS server and store credentials.

On first login, you must specify the server URL. Subsequent logins will
use the stored server URL unless overridden.

Examples:
  # First login to a server
  dfsctl login --server http://localhost:8080 --username admin

  # Login with password on command line (less secure)
  dfsctl login --server http://localhost:8080 -u admin -p secret

  # Re-login to stored server
  dfsctl login

```
dfsctl login [flags]
```

Flags:

```
  -c, --context string    Context name (defaults to current or auto-generated)
  -p, --password string   Password
      --server string     Server URL (default "http://localhost:8080")
  -u, --username string   Username
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

This removes the access and refresh tokens but keeps the server URL
and context configuration for easy re-login.

Examples:
  # Logout from current context
  dfsctl logout

```
dfsctl logout
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup`

Manage netgroups (IP access control)

Create and manage netgroups for IP-based share access control.

Netgroups define sets of IP addresses, CIDR ranges, or hostnames that can
be referenced from share security policies to control access.

Examples:
  # List all netgroups
  dfsctl netgroup list

  # Create a netgroup
  dfsctl netgroup create --name office-network

  # Show netgroup details
  dfsctl netgroup show office-network

  # Add a member
  dfsctl netgroup add-member office-network --type cidr --value 192.168.1.0/24

  # Remove a member
  dfsctl netgroup remove-member office-network --member-id <uuid>

  # Delete a netgroup
  dfsctl netgroup delete office-network

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup add-member`

Add a member to a netgroup

Add an IP address, CIDR range, or hostname to a netgroup.

Examples:
  # Add a single IP
  dfsctl netgroup add-member office-network --type ip --value 192.168.1.100

  # Add a CIDR range
  dfsctl netgroup add-member office-network --type cidr --value 10.0.0.0/8

  # Add a hostname
  dfsctl netgroup add-member office-network --type hostname --value server1.example.com

```
dfsctl netgroup add-member <name> [flags]
```

Flags:

```
      --type string    Member type: ip, cidr, or hostname (required)
      --value string   Member value (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup create`

Create a new netgroup

Create a new netgroup on the DittoFS server.

Examples:
  # Create a netgroup
  dfsctl netgroup create --name office-network

  # Create and output as JSON
  dfsctl netgroup create --name office-network -o json

```
dfsctl netgroup create [flags]
```

Flags:

```
      --name string   Netgroup name (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup delete`

Delete a netgroup

Delete a netgroup from the DittoFS server.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

If the netgroup is referenced by any shares, the deletion will fail
with a conflict error listing the affected shares.

Examples:
  # Delete netgroup with confirmation
  dfsctl netgroup delete office-network

  # Delete without confirmation
  dfsctl netgroup delete office-network --force

```
dfsctl netgroup delete <name> [flags]
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup list`

List all netgroups

List all netgroups on the DittoFS server.

Examples:
  # List netgroups as table
  dfsctl netgroup list

  # List as JSON
  dfsctl netgroup list -o json

```
dfsctl netgroup list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup remove-member`

Remove a member from a netgroup

Remove a member from a netgroup by its member ID.

Use 'dfsctl netgroup show <name>' to see member IDs.

Examples:
  # Remove a member by ID
  dfsctl netgroup remove-member office-network --member-id 550e8400-e29b-41d4-a716-446655440000

```
dfsctl netgroup remove-member <name> [flags]
```

Flags:

```
      --member-id string   Member ID to remove (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup show`

Show netgroup details

Show detailed information about a netgroup including all members.

Examples:
  # Show netgroup details
  dfsctl netgroup show office-network

  # Show as JSON
  dfsctl netgroup show office-network -o json

```
dfsctl netgroup show <name>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl quota`

Per-identity quota management

Manage per-identity (user/group/default-user) storage quotas on a share.

Quotas bound both bytes and inode (file) count, with optional soft thresholds
and a grace period before a soft threshold is enforced as hard. These operations
require admin privileges.

Examples:
  # List all quotas on a share
  dfsctl quota list /archive

  # Set a per-user quota (uid 1000)
  dfsctl quota set /archive --scope user --id 1000 --limit-bytes 10GiB --limit-files 100000

  # Set the default-user fallback quota (applies to users without an explicit quota)
  dfsctl quota set /archive --scope default-user --limit-bytes 1GiB

  # Set a per-group quota with soft thresholds and a grace period
  dfsctl quota set /archive --scope group --id 2000 --limit-bytes 50GiB --soft-bytes 45GiB --grace-seconds 604800

  # Remove a per-user quota
  dfsctl quota rm /archive --scope user --id 1000

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl quota list`

List all quotas on a share

List all per-identity quotas configured on a share.

Examples:
  # List quotas as a table
  dfsctl quota list /archive

  # List as JSON
  dfsctl quota list /archive -o json

```
dfsctl quota list <share>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl quota rm`

Remove a per-identity quota

Remove a per-identity quota from a share.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Remove a per-user quota (uid 1000)
  dfsctl quota rm /archive --scope user --id 1000

  # Remove the default-user fallback quota
  dfsctl quota rm /archive --scope default-user

  # Remove without confirmation
  dfsctl quota rm /archive --scope group --id 2000 --force

```
dfsctl quota rm <share> [flags]
```

Flags:

```
  -f, --force          Skip confirmation prompt
      --id int         Identity id (uid for user, gid for group). Required for user/group; omit for default-user. (default -1)
      --scope string   Quota scope (user|group|default-user) (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl quota set`

Create or update a per-identity quota

Create or update a per-identity quota on a share.

The --scope flag selects user, group, or default-user. For user/group scopes an
identity --id (uid or gid) is required. The default-user scope is a fallback
applied to any user without an explicit user quota and takes no --id.

A byte or file limit of 0 (the default) means "no limit on that dimension".

Examples:
  # Per-user quota (uid 1000): 10GiB / 100k files
  dfsctl quota set /archive --scope user --id 1000 --limit-bytes 10GiB --limit-files 100000

  # Default-user fallback quota
  dfsctl quota set /archive --scope default-user --limit-bytes 1GiB

  # Per-group quota with soft thresholds and a 7-day grace period
  dfsctl quota set /archive --scope group --id 2000 --limit-bytes 50GiB --soft-bytes 45GiB --grace-seconds 604800

```
dfsctl quota set <share> [flags]
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl settings`

Server settings management

Manage server settings on the DittoFS server.

Settings commands allow you to get, set, and list server configuration settings.
These operations require admin privileges.

Examples:
  # List all settings
  dfsctl settings list

  # Get a specific setting
  dfsctl settings get logging.level

  # Set a setting value
  dfsctl settings set logging.level DEBUG

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl settings get`

Get a setting value

Get the value of a server setting.

Examples:
  # Get a setting
  dfsctl settings get logging.level

  # Get as JSON
  dfsctl settings get logging.level -o json

```
dfsctl settings get <key>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl settings list`

List all settings

List all server settings.

Examples:
  # List as table
  dfsctl settings list

  # List as JSON
  dfsctl settings list -o json

```
dfsctl settings list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl settings set`

Set a setting value

Set the value of a server setting.

Examples:
  # Set logging level
  dfsctl settings set logging.level DEBUG

  # Set a numeric value
  dfsctl settings set server.port 8080

```
dfsctl settings set <key> <value>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share`

Share management

Manage shares on the DittoFS server.

The `share` tree uses `share <verb> <name>` — Cobra
resolves the verb as the subcommand and `<name>` as its positional
argument. These operations require admin privileges.

Examples:
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

  # Delete a share
  dfsctl share delete /archive

  # Grant permission
  dfsctl share permission grant /archive --user alice --level read-write

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share create`

Create a new share

Create a new share on the DittoFS server.

A share requires a metadata store and a local block store. A remote block store
is optional and enables tiered storage (local cache + remote durable storage).

Examples:
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
dfsctl share create [flags]
```

Flags:

```
      --access-based-enumeration        Enable Windows access-based enumeration (SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM). When true, SMB clients only see directory entries they can read.
      --acl-canonicalize-inherited      When false, preserves the SE_DACL_AUTO_INHERITED control bit verbatim on SET_INFO Security instead of applying MS-DTYP §2.5.3.4.2 canonicalization (Samba "acl flag inherited canonicalization = no"). Default true matches Windows. (default true)
      --allow-mfsymlink                 Convert 1067-byte XSym (Minshall+French) symlink files written by macOS/Windows SMB clients into real symlinks on CLOSE. Off by default (XSym files are stored as regular files).
      --change-notify-disabled          Reject SMB2 CHANGE_NOTIFY with STATUS_NOT_IMPLEMENTED on this share (mirrors Samba 'kernel change notify = no').
      --continuous-availability         Advertise SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY and allow SMB3 persistent durable handles on this share.
      --default-permission string       Default permission (none|read|read-write|admin) (default "read-write")
      --description string              Share description
      --enable-trash                    Enable the per-share recycle bin so deletes move to #recycle instead of being permanent.
      --encrypt-data                    Require SMB3 encryption for this share
      --local string                    Local block store name (required)
      --local-store-size string         Per-share disk cache size override (e.g., 10GiB, 500MiB)
      --metadata string                 Metadata store name (required)
      --name string                     Share name/path (required)
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share delete`

Delete a share

Delete a share from the DittoFS server.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Delete share with confirmation
  dfsctl share delete /archive

  # Delete share without confirmation
  dfsctl share delete /archive --force

```
dfsctl share delete <name> [flags]
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
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

Examples:
  # Disable a share before restoring its metadata store
  dfsctl share disable /archive

  # Emit the updated Share record as JSON
  dfsctl share disable /archive -o json

```
dfsctl share disable <name>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share edit`

Edit a share

Edit an existing share on the DittoFS server.

When run without flags, opens an interactive editor to modify share properties.
When flags are provided, only the specified fields are updated.

Examples:
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
dfsctl share edit <name> [flags]
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share enable`

Enable a share (accept new connections)

Enable a share on the DittoFS server.

Re-enabling a share allows new client connections and lifts the drain state
set by 'share disable'. Re-enabling is a deliberate operator act; no
mid-restore safety check is performed — the operator owns the timing.

Examples:
  # Enable a share after a completed metadata-store restore
  dfsctl share enable /archive

  # Emit the updated Share record as JSON
  dfsctl share enable /archive -o json

```
dfsctl share enable <name>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share list`

List all shares

List all shares on the DittoFS server.

Examples:
  # List shares as table
  dfsctl share list

  # List as JSON
  dfsctl share list -o json

  # List as YAML
  dfsctl share list -o yaml

```
dfsctl share list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share list-mounts`

List mounted DittoFS shares

List all currently mounted DittoFS shares.

This command shows NFS and SMB mounts from localhost that are likely DittoFS shares.
Optionally filter by share name.

Examples:
  # List all mounted DittoFS shares
  dfsctl share list-mounts

  # Filter by share name
  dfsctl share list-mounts /export

  # Short alias
  dfsctl share mounts

```
dfsctl share list-mounts [share]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share mount`

Mount a share via NFS or SMB

Mount a DittoFS share at a local mount point using NFS or SMB protocol.

For SMB mounts, credentials are resolved in order:
  1. --username/--password flags
  2. DITTOFS_PASSWORD environment variable (for password)
  3. Current login context username
  4. Interactive password prompt

Examples:
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

Note: Mount commands typically require sudo/root privileges on Unix systems.

Platform differences for SMB with sudo:
  - Linux: Mount owner set to your user via uid/gid options (default mode 0755)
  - macOS: Mount owned by root (uid/gid removed in Catalina), default mode 0777
  - macOS alternative: mount to ~/mnt without sudo for user-owned mount
  - Windows: Uses 'net use' to map network drives (e.g., dfsctl share mount /export --protocol smb Z:)

```
dfsctl share mount [share] [mountpoint] [flags]
```

Flags:

```
      --dir-mode string    Directory permissions for SMB mount (octal) (default "0777")
      --file-mode string   File permissions for SMB mount (octal, default 0777 on macOS since uid/gid not supported) (default "0777")
  -P, --password string    Password for SMB mount (will prompt if not provided)
  -p, --protocol string    Protocol to use (nfs or smb) (required)
  -u, --username string    Username for SMB mount (defaults to login username)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share nfs-config`

Manage per-share NFS adapter configuration

View and update the NFS adapter configuration for a share.

This controls protocol-specific NFS export settings such as the squash mode,
auth flavor, and the netgroup that restricts which clients may access the
export. Netgroup changes take effect immediately; other fields apply on the
next adapter restart.

Examples:
  # Show a share's NFS config
  dfsctl share nfs-config show /export

  # Associate a netgroup with the share's NFS export
  dfsctl share nfs-config set /export --netgroup office-network

  # Remove the netgroup association (allow all clients)
  dfsctl share nfs-config set /export --netgroup ""

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share nfs-config set`

Update a share's NFS adapter configuration

```
dfsctl share nfs-config set <name> [flags]
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share nfs-config show`

Show a share's NFS adapter configuration

```
dfsctl share nfs-config show <name>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share permission`

Manage share permissions

Manage permissions on shares.

Permission commands allow you to grant, revoke, and list permissions
for users and groups on shares.

Examples:
  # Grant read-write permission to a user
  dfsctl share permission grant /archive --user alice --level read-write

  # Grant read permission to a group
  dfsctl share permission grant /archive --group editors --level read

  # Revoke permission from a user
  dfsctl share permission revoke /archive --user alice

  # List permissions on a share
  dfsctl share permission list /archive

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share permission grant`

Grant permission on a share

Grant permission to a user or group on a share.

Permission levels:
  - none: No access
  - read: Read-only access
  - read-write: Read and write access
  - admin: Full administrative access

Examples:
  # Grant read-write to user
  dfsctl share permission grant /archive --user alice --level read-write

  # Grant read to group
  dfsctl share permission grant /archive --group editors --level read

```
dfsctl share permission grant <share> [flags]
```

Flags:

```
      --group string   Group name to grant permission to
      --level string   Permission level (none|read|read-write|admin)
      --user string    Username to grant permission to
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share permission list`

List permissions on a share

List all permissions configured on a share.

Examples:
  # List permissions as table
  dfsctl share permission list /archive

  # List as JSON
  dfsctl share permission list /archive -o json

```
dfsctl share permission list <share>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share permission revoke`

Revoke permission from a share

Revoke permission from a user or group on a share.

Examples:
  # Revoke permission from user
  dfsctl share permission revoke /archive --user alice

  # Revoke permission from group
  dfsctl share permission revoke /archive --group editors

```
dfsctl share permission revoke <share> [flags]
```

Flags:

```
      --group string   Group name to revoke permission from
      --user string    Username to revoke permission from
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share show`

Show share details

Show detailed information about a share including retention settings.

Examples:
  # Show share details
  dfsctl share show /edge-data

  # Show as JSON
  dfsctl share show /edge-data -o json

```
dfsctl share show <name>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot`

Manage share snapshots (create, list, show, delete, restore)

Manage share snapshots.

A snapshot captures the full state of a share at a point in time. It can
be inspected, listed, deleted, or restored back onto a (disabled) share.

Examples:
  # Create a snapshot and wait for it to be ready
  dfsctl share snapshot create /archive --name weekly

  # List snapshots for a share
  dfsctl share snapshot list /archive

  # Show details of a single snapshot
  dfsctl share snapshot show /archive snap-abc123

  # Delete a snapshot (prompts for confirmation)
  dfsctl share snapshot delete /archive snap-abc123

  # Restore a snapshot onto a disabled share
  dfsctl share disable /archive
  dfsctl share snapshot restore /archive snap-abc123

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot create`

Create a snapshot of a share

Create a snapshot of a share.

By default the command blocks until the snapshot reaches a terminal
state (ready or failed). Use --no-wait to return immediately after the
snapshot is enqueued.

Examples:
  # Block until snapshot is ready
  dfsctl share snapshot create /archive --name weekly

  # Return immediately with the new snapshot ID
  dfsctl share snapshot create /archive --no-wait

  # Skip the remote-durability verify step
  dfsctl share snapshot create /archive --no-verify

  # Retry a failed previous snapshot
  dfsctl share snapshot create /archive --retry snap-prev123

```
dfsctl share snapshot create <share> [flags]
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot delete`

Delete a snapshot

Delete a snapshot. This is irreversible.

Examples:
  # Delete with prompt
  dfsctl share snapshot delete /archive snap-abc123

  # Delete without prompt
  dfsctl share snapshot delete /archive snap-abc123 --yes

```
dfsctl share snapshot delete <share> <id> [flags]
```

Flags:

```
      --yes   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot list`

List snapshots for a share

List snapshots for a share, newest-first.

Examples:
  # List as table
  dfsctl share snapshot list /archive

  # Filter by state
  dfsctl share snapshot list /archive --state ready

  # Filter by name prefix
  dfsctl share snapshot list /archive --name-prefix weekly

  # JSON output
  dfsctl share snapshot list /archive -o json

```
dfsctl share snapshot list <share> [flags]
```

Flags:

```
      --name-prefix string   Filter by name prefix
      --no-relative          Print absolute timestamps instead of relative
      --state string         Filter by state (creating|ready|failed)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot restore`

Restore a snapshot into a (disabled) share

Restore a snapshot into a share.

The target share must be disabled first; the command refuses with a
hint when the share is still enabled. Restore is destructive: a safety
snapshot is taken before the reset and its ID is printed on success
(delete it once you have verified the restored share).

Examples:
  # Restore with prompt
  dfsctl share disable /archive
  dfsctl share snapshot restore /archive snap-abc123

  # Restore without prompt
  dfsctl share snapshot restore /archive snap-abc123 --yes

  # Restore a snapshot that is not remotely durable
  dfsctl share snapshot restore /archive snap-abc123 --yes --force

```
dfsctl share snapshot restore <share> <id> [flags]
```

Flags:

```
      --force   Allow restoring a snapshot that is not remotely durable
      --yes     Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot show`

Show details of a snapshot

```
dfsctl share snapshot show <share> <id>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot-policy`

Manage scheduled snapshot policies (schedule + retention)

Manage per-share snapshot policies.

A snapshot policy makes a share snapshot itself automatically on a fixed
interval and prunes old scheduler-created snapshots past the retention
bounds (keep-last and/or ttl). Manually-created snapshots are never pruned.

Examples:
  # Daily snapshots, keep the newest 7, drop anything older than 30 days
  dfsctl share snapshot-policy set /archive --interval @daily --keep-last 7 --ttl 720h

  # Show a share's policy
  dfsctl share snapshot-policy show /archive

  # List every policy
  dfsctl share snapshot-policy list

  # Trigger the policy immediately, ignoring its interval
  dfsctl share snapshot-policy run /archive

  # Remove a share's policy
  dfsctl share snapshot-policy delete /archive

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot-policy delete`

Delete a share's snapshot policy

Delete a share's snapshot policy. Existing snapshots are not removed;
only the schedule and automatic pruning stop.

```
dfsctl share snapshot-policy delete <share> [flags]
```

Flags:

```
      --yes   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot-policy list`

List all snapshot policies

```
dfsctl share snapshot-policy list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot-policy run`

Trigger a share's snapshot policy now (manual override)

Run a share's snapshot policy immediately, ignoring its interval.

This creates a scheduled snapshot now, advances the policy's run clock, and
prunes per the retention bounds. Useful to take an out-of-band snapshot
without changing the schedule.

```
dfsctl share snapshot-policy run <share>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
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

Examples:
  dfsctl share snapshot-policy set /archive --interval @daily --keep-last 7 --ttl 720h
  dfsctl share snapshot-policy set /archive --interval 6h --disabled

```
dfsctl share snapshot-policy set <share> [flags]
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share snapshot-policy show`

Show a share's snapshot policy

```
dfsctl share snapshot-policy show <share>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl share unmount`

Unmount a mounted share

Unmount a DittoFS share from a local mount point.

Examples:
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
dfsctl share unmount [mountpoint] [flags]
```

Flags:

```
  -f, --force   Force unmount even if busy
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl status`

Show server status

Display the status of the connected DittoFS server.

This command checks the server health endpoint and displays
status, uptime, and control plane DB reachability.

When authenticated, per-entity status is fetched from the list
endpoints and displayed as a color-coded table.

Examples:
  # Check status of connected server
  dfsctl status

  # Output as JSON
  dfsctl status -o json

```
dfsctl status
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store`

Store management

Manage metadata and block stores on the DittoFS server.

Store commands allow you to create, list, update, and delete stores.
These operations require admin privileges.

Examples:
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

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block`

Block store management

Manage local and remote block stores on the DittoFS server.

Block stores hold file content data as blocks. Local block stores provide
fast disk-backed storage, while remote block stores provide durable cloud
storage (e.g., S3).

Examples:
  # List local block stores
  dfsctl store block local list

  # Add a local filesystem block store
  dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

  # List remote block stores
  dfsctl store block remote list

  # Add an S3 remote block store
  dfsctl store block remote add --name s3-store --type s3 --config '{"bucket":"my-bucket","region":"us-east-1"}'

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block audit-refcounts`

Verify FileBlock.RefCount matches FileAttr.Blocks references

Run the INV-02 reconciliation audit for the named share.

Computes ∑ FileBlock.RefCount and compares against ∑ len(FileAttr.Blocks)
across all files in the share. A non-zero delta indicates a refcount drift
that may block GC reclamation (a leaked block survives the grace window)
or signal a bug in the dedup short-circuit.

Persists last-run summary at <localStore>/audit-state/last-inv02.json
analogously to GC's last-run.json. Operator-invokable; no periodic
schedule in v0.15.0.

Examples:
  dfsctl store block audit-refcounts myshare
  dfsctl store block audit-refcounts myshare -o json

```
dfsctl store block audit-refcounts <share>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
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

Examples:
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
dfsctl store block evict [flags]
```

Flags:

```
      --local-only         Evict only local disk data (preserves read buffer)
      --read-buffer-only   Evict only read buffer (in-memory)
      --share string       Evict data for a specific share only
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block gc`

Run garbage collection for a block store share

Trigger an on-demand GC run for the named share.

The mark phase enumerates every live ContentHash across all shares whose
remote-store config matches the named share (cross-share aggregation).
The sweep phase deletes any cas/.../ object absent from the live set
whose LastModified is older than the configured grace period (default
1h). The last-run.json summary is persisted under the share's gc-state
directory and can be inspected with:

  dfsctl store block gc-status <share>

Use --dry-run to skip deletes and print up to dry_run_sample_size
candidate keys (default 1000). Recommended for first-time deployment
confidence and for debugging suspected mark-phase bugs.

Examples:
  dfsctl store block gc myshare
  dfsctl store block gc myshare --dry-run
  dfsctl store block gc myshare -o json

```
dfsctl store block gc <share> [flags]
```

Flags:

```
      --dry-run   Run mark + sweep enumeration but skip deletes; print candidate keys
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block gc-status`

Show the last block-store GC run summary for a share

Print the most recent garbage collection run summary for the named share.

Reads <gcStateRoot>/last-run.json, which is overwritten by every
completed GC run. Returns exit 1 with a friendly message if no run has
been recorded yet (the share has never been GC'd, or its local store
has no persistent root).

Examples:
  dfsctl store block gc-status myshare
  dfsctl store block gc-status myshare -o json

```
dfsctl store block gc-status <share>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block health`

Check block store health

Perform a health check on a block store configuration.

For local filesystem stores, checks if the path exists and is writable.
For local memory stores, always reports healthy.
For remote S3 stores, performs a HeadBucket call to verify connectivity.
For remote memory stores, always reports healthy.

Examples:
  # Check health of a local block store
  dfsctl store block health --kind local --name fs-cache

  # Check health of a remote block store
  dfsctl store block health --kind remote --name s3-store

  # Output as JSON
  dfsctl store block health --kind remote --name s3-store -o json

```
dfsctl store block health [flags]
```

Flags:

```
      --kind string   Block store kind: local or remote (required)
      --name string   Block store name (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block local`

Local block store management

Manage local block stores on the DittoFS server.

Local block stores provide fast disk-backed storage for file content blocks.
Supported types: fs (filesystem), memory (testing)

Examples:
  # List local block stores
  dfsctl store block local list

  # Add a filesystem block store
  dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

  # Add a memory block store (for testing)
  dfsctl store block local add --name test-local --type memory

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block local add`

Add a local block store

Add a new local block store to the DittoFS server.

Supported types:
  - fs: Filesystem-backed block store (fast, persistent)
  - memory: In-memory block store (fast, ephemeral, for testing)

Type-specific options:
  fs:
    --path: Block directory path (or prompted interactively)

Examples:
  # Add a filesystem block store
  dfsctl store block local add --name fs-cache --type fs --path /data/blocks

  # Add with JSON config
  dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

  # Add a memory store (for testing)
  dfsctl store block local add --name test-local --type memory

  # Add interactively (prompts for path)
  dfsctl store block local add --name fs-cache --type fs

```
dfsctl store block local add [flags]
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block local edit`

Edit a local block store

Edit an existing local block store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit interactively
  dfsctl store block local edit default-local

  # Update config with JSON
  dfsctl store block local edit default-local --config '{"path":"/new/path"}'

  # Update path for fs store
  dfsctl store block local edit default-local --path /new/path

```
dfsctl store block local edit <name> [flags]
```

Flags:

```
      --config string   Store configuration as JSON
      --path string     Block directory path (for fs type)
      --type string     Store type: fs, memory
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block local list`

List local block stores

List all local block stores on the DittoFS server.

Examples:
  # List as table
  dfsctl store block local list

  # List as JSON
  dfsctl store block local list -o json

```
dfsctl store block local list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block local remove`

Remove a local block store

Remove a local block store from the DittoFS server.

Warning: This will fail if the store is in use by any shares.

Examples:
  # Remove with confirmation
  dfsctl store block local remove fs-cache

  # Remove without confirmation
  dfsctl store block local remove fs-cache --force

```
dfsctl store block local remove <name> [flags]
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block remote`

Remote block store management

Manage remote block stores on the DittoFS server.

Remote block stores provide durable cloud storage for file content blocks.
Supported types: s3 (AWS S3 or S3-compatible), memory (testing)

Examples:
  # List remote block stores
  dfsctl store block remote list

  # Add an S3 block store
  dfsctl store block remote add --name s3-store --type s3 --config '{"bucket":"my-bucket","region":"us-east-1"}'

  # Add a memory block store (for testing)
  dfsctl store block remote add --name test-remote --type memory

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block remote add`

Add a remote block store

Add a new remote block store to the DittoFS server.

Supported types:
  - s3: AWS S3 or S3-compatible store (durable, production)
  - memory: In-memory store (fast, ephemeral, for testing)

Type-specific options:
  s3:
    --bucket: S3 bucket name (or prompted interactively)
    --region: AWS region (default: us-east-1)
    --endpoint: Custom endpoint for S3-compatible stores
    --prefix: Key prefix within the bucket
    --access-key: AWS access key ID
    --secret-key: AWS secret access key

Examples:
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
dfsctl store block remote add [flags]
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
      --prefix string                     Key prefix within the bucket (for s3)
      --region string                     AWS region (for s3) (default "us-east-1")
      --secret-key string                 AWS secret access key (for s3)
      --type string                       Store type: s3, memory (default "s3")
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block remote edit`

Edit a remote block store

Edit an existing remote block store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit interactively
  dfsctl store block remote edit s3-store

  # Update config with JSON
  dfsctl store block remote edit s3-store --config '{"bucket":"new-bucket"}'

  # Update S3 settings
  dfsctl store block remote edit s3-store --bucket new-bucket --region us-west-2

```
dfsctl store block remote edit <name> [flags]
```

Flags:

```
      --access-key string   AWS access key ID (for s3)
      --bucket string       S3 bucket name (for s3)
      --config string       Store configuration as JSON
      --endpoint string     Custom S3 endpoint
      --region string       AWS region (for s3)
      --secret-key string   AWS secret access key (for s3)
      --type string         Store type: s3, memory
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block remote list`

List remote block stores

List all remote block stores on the DittoFS server.

Examples:
  # List as table
  dfsctl store block remote list

  # List as JSON
  dfsctl store block remote list -o json

```
dfsctl store block remote list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block remote remove`

Remove a remote block store

Remove a remote block store from the DittoFS server.

Warning: This will fail if the store is in use by any shares.

Examples:
  # Remove with confirmation
  dfsctl store block remote remove s3-store

  # Remove without confirmation
  dfsctl store block remote remove s3-store --force

```
dfsctl store block remote remove <name> [flags]
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store block stats`

Show block store statistics

Display block store statistics.

Without --share, shows aggregated totals across all shares with a per-share breakdown.
With --share, shows statistics for a single share only.

Examples:
  # Show aggregated block store stats
  dfsctl store block stats

  # Show stats for a specific share
  dfsctl store block stats --share /export

  # Output as JSON
  dfsctl store block stats -o json

```
dfsctl store block stats [flags]
```

Flags:

```
      --share string   Show stats for a specific share only
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store metadata`

Manage metadata stores

Manage metadata stores on the DittoFS server.

Metadata stores hold file system structure, attributes, and permissions.
Supported types: memory, badger, postgres

Examples:
  # List metadata stores
  dfsctl store metadata list

  # Add a memory store
  dfsctl store metadata add --name fast-meta --type memory

  # Add a BadgerDB store
  dfsctl store metadata add --name persistent-meta --type badger --config '{"path":"/data/meta"}'

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store metadata add`

Add a metadata store

Add a new metadata store to the DittoFS server.

Supported types:
  - memory: In-memory store (fast, ephemeral)
  - badger: BadgerDB store (persistent, embedded)
  - postgres: PostgreSQL store (persistent, distributed)

Type-specific options:
  badger:
    --db-path: Path to BadgerDB directory (or prompted interactively)

  postgres:
    --config: JSON with connection settings, or omit for interactive prompts

Examples:
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
dfsctl store metadata add [flags]
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store metadata edit`

Edit a metadata store

Edit an existing metadata store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit interactively (default)
  dfsctl store metadata edit default

  # Update config with JSON
  dfsctl store metadata edit default --config '{"path":"/new/path"}'

  # Update type
  dfsctl store metadata edit default --type badger

  # Update BadgerDB path
  dfsctl store metadata edit default --db-path /new/path

```
dfsctl store metadata edit <name> [flags]
```

Flags:

```
      --config string    Store configuration as JSON
      --db-path string   Database path (for badger)
      --type string      Store type: memory, badger, postgres
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store metadata health`

Check metadata store health

Perform a health check on a metadata store.

If the store is loaded in the runtime, calls its native health check method.
Otherwise, reports that the store is not loaded.

Examples:
  # Check health of a metadata store
  dfsctl store metadata health --name fast-meta

  # Output as JSON
  dfsctl store metadata health --name fast-meta -o json

```
dfsctl store metadata health [flags]
```

Flags:

```
      --name string   Metadata store name (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store metadata list`

List metadata stores

List all metadata stores on the DittoFS server.

Examples:
  # List as table
  dfsctl store metadata list

  # List as JSON
  dfsctl store metadata list -o json

```
dfsctl store metadata list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl store metadata remove`

Remove a metadata store

Remove a metadata store from the DittoFS server.

Warning: This may fail if the store is in use by any shares.

Examples:
  # Remove with confirmation
  dfsctl store metadata remove fast-meta

  # Remove without confirmation
  dfsctl store metadata remove fast-meta --force

```
dfsctl store metadata remove <name> [flags]
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl switch-user`

Switch to a different user on the current server

Switch to a different user on the current server.

This command authenticates as the specified user against the same server
configured in the current context, creating a new context if needed.

If a context already exists for this user on the same server and has a valid
(non-expired) token, it switches to that context without re-authenticating.

Examples:
  # Switch to user marmos91 (will prompt for password)
  dfsctl switch-user marmos91

  # Switch with password on command line
  dfsctl switch-user marmos91 -p secret

```
dfsctl switch-user <username> [flags]
```

Flags:

```
  -p, --password string   Password (will prompt if not provided)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl system`

System operations

System-level operations for managing the DittoFS server.

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl system drain-uploads`

Wait for all pending uploads to complete

Wait for all in-flight block store uploads to complete across all files.

This is useful for benchmarking and testing to ensure clean boundaries
between workloads. The command blocks until all uploads are drained or
the server-side timeout (5 minutes) is reached.

Examples:
  # Drain all pending uploads
  dfsctl system drain-uploads

  # Output as JSON
  dfsctl system drain-uploads -o json

```
dfsctl system drain-uploads
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl trash`

Recycle-bin management

Manage a share's recycle bin (#recycle).

When a share has trash enabled, deleted files and directories are moved to a
per-share recycle bin instead of being purged immediately. Use these commands
to inspect, restore, or empty that bin.

Examples:
  dfsctl trash list myshare
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt"
  dfsctl trash empty myshare --force
  dfsctl trash status myshare

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl trash empty`

Empty a share's recycle bin

Permanently remove every recycled root from a share's recycle bin.

This cannot be undone. Use --force to skip any server-side safety checks.

Examples:
  dfsctl trash empty myshare
  dfsctl trash empty myshare --force

```
dfsctl trash empty <share> [flags]
```

Flags:

```
      --force   Force empty, skipping server-side safety checks
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl trash list`

List recycle-bin entries for a share

List the recycled roots in a share's recycle bin.

Each entry shows where it now lives under #recycle, the path it occupied
before deletion, who deleted it, when, its size, and whether it is a
directory subtree.

Examples:
  dfsctl trash list myshare
  dfsctl trash list myshare -o json

```
dfsctl trash list <share>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl trash restore`

Restore a recycled file or directory

Restore the recycled root at <bin-path> back into the share.

Without --to the entry is restored to the path it occupied before deletion.
Use --to to restore it elsewhere — useful when the original location is now
occupied.

Examples:
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt"
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt" --to /restored/report.txt

```
dfsctl trash restore <share> <bin-path> [flags]
```

Flags:

```
      --to string   Restore to this share-relative path instead of the original location
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl trash status`

Show recycle-bin status for a share

Print the recycle-bin roll-up for a share: whether trash is enabled,
how many recycled roots it holds, their total size, and the oldest deletion.

Examples:
  dfsctl trash status myshare
  dfsctl trash status myshare -o json

```
dfsctl trash status <share>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl user`

User management

Manage users on the DittoFS server.

User commands allow you to create, list, edit, and delete users.
These operations require admin privileges.

Examples:
  # List all users
  dfsctl user list

  # Create a new user interactively
  dfsctl user create

  # Create a user with flags
  dfsctl user create --username alice --password secret --role user

  # Edit a user interactively
  dfsctl user edit alice

  # Delete a user
  dfsctl user delete alice

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl user change-password`

Change your own password

Change your own password.

This is used when you need to change your password, especially
when the server requires a password change after initial login.

Examples:
  # Change password interactively
  dfsctl user change-password

  # Change password with flags (less secure)
  dfsctl user change-password --current oldpass --new newpass

```
dfsctl user change-password [flags]
```

Flags:

```
  -c, --current string   Current password (prompts if not provided)
  -n, --new string       New password (prompts if not provided)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl user create`

Create a new user

Create a new user on the DittoFS server.

If username or password are not provided via flags, you will be prompted
to enter them interactively.

Examples:
  # Create user interactively
  dfsctl user create

  # Create user with flags
  dfsctl user create --username alice --password secret

  # Create admin user
  dfsctl user create --username admin2 --password secret --role admin

  # Create user with email and groups
  dfsctl user create --username bob --password secret --email bob@example.com --groups editors,viewers

  # Create user with specific UID
  dfsctl user create --username bob --password secret --uid 1001

  # Create user with your current host UID (for NFS access)
  dfsctl user create --username bob --password secret --host-uid

```
dfsctl user create [flags]
```

Flags:

```
      --email string      Email address
      --enabled           Enable account (default true)
      --groups string     Comma-separated list of groups
      --host-uid          Use current host user's UID (for NFS access)
  -p, --password string   Password (prompts if not provided)
      --role string       Role (user|admin) (default "user")
      --uid uint32        Unix user ID (auto-assigned if not specified)
  -u, --username string   Username (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl user delete`

Delete a user

Delete a user from the DittoFS server.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Delete user with confirmation
  dfsctl user delete alice

  # Delete user without confirmation
  dfsctl user delete alice --force

```
dfsctl user delete <username> [flags]
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl user edit`

Edit a user

Edit an existing user on the DittoFS server.

When run without flags, opens an interactive editor to modify user properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit user interactively
  dfsctl user edit alice

  # Update email directly
  dfsctl user edit alice --email alice@newdomain.com

  # Update role to admin
  dfsctl user edit alice --role admin

  # Disable user
  dfsctl user edit alice --enabled false

  # Update multiple fields
  dfsctl user edit alice --email alice@example.com --groups editors,admins

  # Update UID
  dfsctl user edit alice --uid 1001

```
dfsctl user edit <username> [flags]
```

Flags:

```
      --display-name string   Display name
      --email string          Email address
      --enabled string        Enable/disable account (true|false)
      --groups string         Comma-separated list of groups
      --role string           Role (user|admin)
      --uid uint32            Unix user ID
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl user get`

Get user details

Get detailed information about a user.

Examples:
  # Get user details as table
  dfsctl user get alice

  # Get as JSON
  dfsctl user get alice -o json

```
dfsctl user get <username>
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl user list`

List all users

List all users on the DittoFS server.

Examples:
  # List users as table
  dfsctl user list

  # List as JSON
  dfsctl user list -o json

  # List as YAML
  dfsctl user list -o yaml

```
dfsctl user list
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl user password`

Reset a user's password

Reset a user's password (admin operation).

This sets the user's password and marks them as needing to change it
on next login.

Examples:
  # Reset password interactively
  dfsctl user password alice

  # Reset password with flag (less secure)
  dfsctl user password alice --password newsecret

```
dfsctl user password <username> [flags]
```

Flags:

```
  -p, --password string   New password (prompts if not provided)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl version`

Show version information

Display the dfsctl version, build information, and system details.

```
dfsctl version [flags]
```

Flags:

```
      --short   Show only version number
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output
```

