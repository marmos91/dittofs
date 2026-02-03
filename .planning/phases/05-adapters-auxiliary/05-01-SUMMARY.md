---
phase: 05-adapters-auxiliary
plan: 01
subsystem: e2e-testing
tags: [adapter, nfs, smb, cli, testing, e2e]

dependency_graph:
  requires:
    - 02-server-identity (CLI helpers, LoginAsAdmin)
    - 04-shares-permissions (CLIRunner pattern)
  provides:
    - Adapter CLI helpers (ListAdapters, GetAdapter, EnableAdapter, DisableAdapter, EditAdapter)
    - WaitForAdapterStatus polling helper
    - TestAdapterLifecycle E2E test suite
  affects:
    - 05-02 (uses same test patterns)
    - 05-03 (uses same test patterns)

tech_stack:
  added: []
  patterns:
    - AdapterOption functional options for adapter configuration
    - GetAdapter via list+filter (CLI lacks dedicated get command)
    - WaitForAdapterStatus polling for async adapter state changes

file_tracking:
  created:
    - test/e2e/adapters_test.go
  modified:
    - test/e2e/helpers/cli.go
    - test/e2e/helpers/server.go

decisions:
  - GetAdapter uses list+filter pattern (CLI lacks dedicated 'adapter get' command)
  - Adapter options prefixed with 'WithAdapter' to avoid collision with other options
  - Tests run sequentially (not parallel) to avoid port conflicts
  - WaitForAdapterStatus polls with 100ms interval for async operations

metrics:
  duration: ~5 min
  completed: 2026-02-02
---

# Phase 05 Plan 01: Adapter Lifecycle E2E Tests Summary

**One-liner:** Adapter CLI helpers and E2E tests for NFS/SMB enable/disable lifecycle with hot reload validation

## What Was Built

### Adapter CLI Helpers (cli.go)

Added comprehensive adapter management methods to CLIRunner:

**Types:**
- `Adapter` - struct matching API response (Type, Port, Enabled)
- `AdapterOption` - functional options pattern
- `adapterOptions` - internal options struct

**Methods:**
- `ListAdapters()` - lists all protocol adapters via `adapter list`
- `GetAdapter(type)` - retrieves adapter by type (list+filter pattern)
- `EnableAdapter(type, opts...)` - enables adapter via `adapter enable`
- `DisableAdapter(type)` - disables adapter via `adapter disable`
- `EditAdapter(type, opts...)` - edits adapter config via `adapter edit`

**Options:**
- `WithAdapterPort(port)` - sets adapter listen port
- `WithAdapterEnabled(enabled)` - sets adapter enabled status

**Helpers:**
- `WaitForAdapterStatus(t, runner, type, enabled, timeout)` - polls adapter status

### Server Helper Enhancement (server.go)

Added `PID()` method to `ServerProcess` for hot reload verification tests.

### Adapter Lifecycle E2E Tests (adapters_test.go)

Created comprehensive test suite covering requirements ADP-01 through ADP-08:

| Test | Requirements | Description |
|------|--------------|-------------|
| NFS enable/disable cycle | ADP-01, ADP-02 | Enable NFS, verify via list, disable, verify disabled |
| SMB enable/disable cycle | ADP-03, ADP-04 | Enable SMB, verify via list, disable, verify disabled |
| Port configuration change | ADP-05 | Enable on port A, edit to port B, verify change |
| Hot reload without restart | ADP-06 | Change adapter port, verify server PID unchanged |
| Adapter clean restart | ADP-07 | Disable, re-enable on same port, verify clean restart |
| Invalid config rejection | ADP-08 | Test invalid port (>65535, negative), unknown type |

## Key Implementation Details

1. **Sequential Test Execution**: Tests run sequentially (not parallel) to avoid port conflicts
2. **Dynamic Port Allocation**: Uses `FindFreePort()` for each adapter operation
3. **Status Polling**: `WaitForAdapterStatus` with 100ms poll interval, configurable timeout
4. **Hot Reload Verification**: PID comparison confirms adapter restarts without server restart

## Commits

| Hash | Type | Description |
|------|------|-------------|
| 54a476e | feat | Add adapter helper methods to CLIRunner |
| fd2052b | feat | Add adapter lifecycle E2E tests |

## Files Modified

| File | Changes |
|------|---------|
| `test/e2e/helpers/cli.go` | +182 lines (Adapter type, options, CRUD methods, WaitForAdapterStatus) |
| `test/e2e/helpers/server.go` | +9 lines (PID() method) |
| `test/e2e/adapters_test.go` | +316 lines (TestAdapterLifecycle suite) |

## Deviations from Plan

None - plan executed exactly as written.

## Next Phase Readiness

No blockers. Ready for 05-02 (Auxiliary Servers - Backup/Health) and 05-03 (Context Management).
