# DittoFS CLI Architecture

This document describes the architecture of the DittoFS CLI tools following the Phase 1 implementation.

## Overview

The CLI is split into two separate binaries following Go best practices:

1. **`dfs`** - Server daemon management (local operations)
2. **`dfsctl`** - REST API client for remote control plane operations

## Binary Structure

### dfs (Server CLI)

Located in `cmd/dfs/`, this binary handles local server management:

```
dfs
├── start         Start the DittoFS server
├── stop          Stop the DittoFS server
├── status        Show server status
├── init          Initialize configuration file
├── migrate       Run database migrations
├── version       Show version information
├── config        Configuration management
│   ├── init      Initialize config file
│   ├── edit      Open config in editor
│   ├── validate  Validate configuration
│   └── show      Display current config
└── backup        Backup operations
    └── controlplane  Backup control plane database
```

### dfsctl (Client CLI)

Located in `cmd/dfsctl/`, this binary handles remote server management via REST API:

```
dfsctl
├── login         Authenticate with DittoFS server
├── logout        Clear stored credentials
├── version       Show version information
└── context       Manage server contexts (multi-server)
    ├── list      List all contexts
    ├── use       Switch to a different context
    ├── current   Show current context
    ├── rename    Rename a context
    └── delete    Delete a context
├── grace         Grace period management
│   ├── end       End the current grace period
│   └── status    Show grace period status
├── client        Client management
│   ├── evict     Evict a client
│   ├── list      List connected clients
│   └── sessions  List client sessions
├── idmap         Identity mapping management
│   ├── add       Add identity mapping
│   ├── list      List identity mappings
│   └── remove    Remove identity mapping
└── netgroup      Netgroup management
    ├── create    Create a netgroup
    ├── delete    Delete a netgroup
    ├── list      List netgroups
    ├── show      Show netgroup details
    └── members   Manage netgroup members
```

## Package Structure

### Internal Packages

Located in `internal/cli/`:

#### output/

Output formatting utilities:

- `format.go` - Format types and Printer for colored output
- `table.go` - Table rendering using tablewriter
- `json.go` - JSON output formatting
- `yaml.go` - YAML output formatting

Usage:
```go
printer := output.NewPrinter(os.Stdout, output.FormatTable, true)
printer.Print(data)
printer.Success("Operation completed")
printer.Error("Something went wrong")
```

#### prompt/

Interactive terminal prompts using promptui:

- `confirm.go` - Yes/no confirmation prompts
- `password.go` - Password input with masking
- `select.go` - Selection menus
- `input.go` - Text input prompts

Usage:
```go
confirmed, err := prompt.Confirm("Delete this item?", false)
password, err := prompt.NewPassword()
selection, err := prompt.SelectString("Choose option", []string{"a", "b", "c"})
```

#### credentials/

Credential and context management for dfsctl:

- `store.go` - Context storage and management

Credentials are stored in `~/.config/dfsctl/config.json` with mode 0600.

### Public Packages

Located in `pkg/`:

#### apiclient/

REST API client for dfsctl:

- `client.go` - HTTP client wrapper
- `auth.go` - Authentication (login, token refresh)
- `errors.go` - API error types

Usage:
```go
client := apiclient.New("http://localhost:8080")
tokens, err := client.Login(username, password)
client = client.WithToken(tokens.AccessToken)
```

## Dependencies

New dependencies added:

- `github.com/spf13/cobra` - CLI framework (industry standard)
- `github.com/manifoldco/promptui` - Interactive prompts
- `github.com/olekukonko/tablewriter` - Table output formatting

## Configuration

### dfs

Uses the same configuration as before, located at `$XDG_CONFIG_HOME/dittofs/config.yaml`.

### dfsctl

Stores credentials and preferences in `$XDG_CONFIG_HOME/dfsctl/config.json`:

```json
{
  "current_context": "default",
  "contexts": {
    "default": {
      "server_url": "http://localhost:8080",
      "username": "admin",
      "access_token": "eyJ...",
      "refresh_token": "eyJ...",
      "expires_at": "2025-01-21T12:00:00Z"
    }
  },
  "preferences": {
    "default_output": "table",
    "color": "auto"
  }
}
```

## Building

Build both binaries:

```bash
# Build dfs
go build -o dfs ./cmd/dfs

# Build dfsctl
go build -o dfsctl ./cmd/dfsctl

# Build with version info
go build -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o dfs ./cmd/dfs
```

## Testing

Run CLI package tests:

```bash
# Run all CLI tests
go test ./internal/cli/... ./pkg/apiclient/...

# Run with verbose output
go test -v ./internal/cli/...

# Run specific package tests
go test ./internal/cli/credentials/
```

## Usage Examples

### Server Management (dfs)

```bash
# Initialize configuration
dfs init

# Validate configuration
dfs config validate

# Start server
dfs start

# Start with custom config
dfs start --config /etc/dittofs/config.yaml

# Check status
dfs status --pid-file /var/run/dittofs.pid

# Stop server
dfs stop --pid-file /var/run/dittofs.pid
```

### Remote Management (dfsctl)

```bash
# Login to server
dfsctl login --server http://localhost:8080 --username admin

# List contexts
dfsctl context list

# Switch context
dfsctl context use production

# Get current context
dfsctl context current

# Logout
dfsctl logout
```

## Global Flags

### dfs

- `--config` - Path to configuration file

### dfsctl

- `--server` - Override server URL
- `--token` - Override authentication token
- `--output, -o` - Output format (table|json|yaml)
- `--no-color` - Disable colored output
- `--verbose, -v` - Enable verbose output

