# cmd/dittofsctl

Remote management CLI for DittoFS. Communicates with the server via REST API.

## Commands

```
dittofsctl
├── login           # Authenticate with server
├── logout          # Clear credentials
├── status          # Show server status
├── context/
│   ├── list        # List saved contexts
│   ├── use         # Switch context
│   └── delete      # Remove context
├── user/
│   ├── list        # List users
│   ├── get         # Get user details
│   ├── create      # Create user
│   ├── edit        # Edit user
│   ├── delete      # Delete user
│   ├── password    # Reset user password (admin)
│   └── change-password  # Change own password
├── group/
│   ├── list, get, create, edit, delete
│   ├── add-user    # Add user to group
│   └── remove-user # Remove user from group
├── share/
│   ├── list, get, create, edit, delete
│   └── permission/ # Manage share permissions
├── store/
│   ├── metadata/   # Metadata store management
│   └── payload/    # Payload store management
├── adapter/
│   ├── list        # List adapters
│   ├── enable      # Enable adapter
│   ├── disable     # Disable adapter
│   └── edit        # Edit adapter config
└── settings/
    ├── get         # Get settings
    └── set         # Update settings
```

## Key Files

- `main.go` - Entry point
- `cmdutil/util.go` - Shared utilities (auth client, output helpers)
- `commands/root.go` - Root command with global flags
- `commands/<resource>/*.go` - Resource-specific commands

## Global Flags

```
--server, -s    Server URL (default: http://localhost:8080)
--output, -o    Output format: table, json, yaml (default: table)
--no-color      Disable colored output
--context       Use specific context
```

## Authentication Flow

1. User runs `dittofsctl login --server URL --username USER`
2. Password prompted interactively
3. Server returns JWT tokens
4. Tokens saved to credentials file
5. Subsequent commands use saved token
6. Token auto-refreshed when expired

## Shared Utilities (`cmdutil/util.go`)

```go
// Get authenticated client
client, err := cmdutil.GetAuthClient(cmd)

// Output formatted data
cmdutil.Output(cmd, data)

// Handle API errors
cmdutil.HandleError(cmd, err)
```

## Conventions

### Output Formats
- `table`: Human-readable tables (default for TTY)
- `json`: Machine-readable JSON
- `yaml`: YAML format

### Interactive Prompts
- Use `internal/cli/prompt` for user input
- Detect non-TTY and fail with helpful message

### Error Messages
- Show user-friendly errors
- Include hints for common issues
- Use `--verbose` for stack traces
