# cmd/dittofs

Server daemon CLI for DittoFS. Manages the local DittoFS server instance.

## Commands

```
dittofs
├── start           # Start server (foreground or daemon)
├── stop            # Stop running server
├── status          # Show server status
├── logs            # Tail server logs
├── config/
│   ├── init        # Create default config
│   ├── show        # Display current config
│   ├── validate    # Validate config file
│   ├── edit        # Edit config in editor
│   └── schema      # Generate JSON schema
└── backup/
    └── controlplane # Backup control plane data
```

## Key Files

- `main.go` - Entry point, sets up root command
- `commands/root.go` - Root command with global flags
- `commands/start.go` - Server startup logic
- `commands/config/*.go` - Configuration management

## Global Flags

```
--config, -c    Config file path (default: ~/.config/dittofs/config.yaml)
--verbose, -v   Verbose output
```

## Start Command

The start command:
1. Loads and validates configuration
2. Initializes control plane store (SQLite/PostgreSQL)
3. Creates admin user if not exists
4. Loads shares and metadata stores
5. Starts protocol adapters (NFS, SMB)
6. Starts REST API server
7. Starts metrics server (optional)

Supports:
- Foreground mode (`-f`) for development
- Daemon mode for production
- Graceful shutdown on SIGINT/SIGTERM

## Configuration

Default location: `~/.config/dittofs/config.yaml`

Can override with:
- `--config` flag
- `DITTOFS_CONFIG` environment variable
- Individual env vars like `DITTOFS_LOGGING_LEVEL`

## Conventions

### Exit Codes
- 0: Success
- 1: General error
- 2: Configuration error

### Logging
- Use structured logging via `internal/logger`
- Respect `--verbose` flag for debug output
