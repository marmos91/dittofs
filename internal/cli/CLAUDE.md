# internal/cli

Shared CLI utilities for both `dittofs` (server) and `dittofsctl` (client) commands.

## Structure

```
internal/cli/
├── output/         # Output formatting (table, JSON, YAML)
├── prompt/         # Interactive prompts (confirm, password, select, input)
├── credentials/    # Multi-context credential storage
├── health/         # Health check utilities
└── timeutil/       # Time formatting helpers
```

## Output Package

Format data for CLI display:

```go
// Table output
output.Table(headers []string, rows [][]string)

// JSON output
output.JSON(data interface{})

// YAML output
output.YAML(data interface{})
```

## Prompt Package

Interactive user prompts using bubbletea:

```go
// Confirmation prompt
confirmed, err := prompt.Confirm("Are you sure?")

// Password input (hidden)
password, err := prompt.Password("Enter password:")

// Text input
value, err := prompt.Input("Enter value:", "default")

// Selection from options
choice, err := prompt.Select("Choose option:", []string{"a", "b", "c"})
```

## Credentials Package

Multi-context credential storage for dittofsctl:

```go
// Store credentials
creds.Save(context, server, token)

// Load credentials
token, err := creds.Load(context)

// List contexts
contexts := creds.List()

// Delete context
creds.Delete(context)
```

Credentials stored in `~/.config/dittofs/credentials.json` (or `$XDG_CONFIG_HOME`).

## Health Package

Health check utilities for status commands:

```go
// Check server health
status, err := health.Check(serverURL)

// Format uptime
uptime := health.FormatUptime(startTime)
```

## Conventions

### Error Handling
- Return errors to caller, don't exit directly
- Use `fmt.Errorf("context: %w", err)` for wrapping
- Commands handle error display and exit codes

### Interactive Mode Detection
- Check `os.Stdin` for TTY before prompting
- Fall back to flags/env vars in non-interactive mode
