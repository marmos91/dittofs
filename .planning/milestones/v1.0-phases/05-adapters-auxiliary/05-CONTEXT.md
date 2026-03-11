# Phase 5: Adapters & Auxiliary - Context

**Gathered:** 2026-02-02
**Status:** Ready for planning

<domain>
## Phase Boundary

Protocol adapter lifecycle management (NFS/SMB enable/disable with hot reload), backup and restore of control plane state, and multi-context server management. This phase covers operational CLI commands and configuration management — actual file operations over adapters are Phase 6.

</domain>

<decisions>
## Implementation Decisions

### Adapter Lifecycle

- **Disable behavior**: Graceful drain — wait for existing connections to complete (up to timeout), then stop
- **Hot reload scope**: All adapter settings can change without full server restart
- **User feedback**: Progress steps shown during operations ("Stopping listener...", "Draining connections (3 active)...", "Stopped")
- **Start failure**: Retry with exponential backoff before failing, then show clear error
- **Status detail**: Rich status including running/stopped, port, active connections count, uptime, last error
- **Batch operations**: Individual adapter control only (no --all flag)
- **Reload sync**: CLI waits for reload to complete, shows result

### Backup/Restore

- **Backup scope**: Config only — users, groups, shares, permissions, store configs (no file data or metadata store contents)
- **Format**: SQL dump (native to underlying database — SQLite or PostgreSQL)
- **Password handling**: Include bcrypt hashes — users can log in immediately after restore
- **Conflict handling**: Require empty state — restore fails if any data exists
- **Online/offline**: Online backup supported, restore requires server stopped
- **Output**: File only (`--output /path/to/backup.sql`), no stdout support
- **Restore preview**: Show summary of what will be restored, require `--confirm` to apply

### Multi-Context UX

- **Switching model**: Both explicit switch (`context use prod`) and per-command override (`--context prod`)
- **Credential storage**: Config file at `~/.config/dittofsctl/contexts.yaml`
- **Context indication**: Only shown on `context list` (asterisk next to current), other commands silent
- **Context data**: Full profile — server URL, token, plus preferred output format, timeout settings
- **Aliases**: Name only, no short aliases
- **Context list**: Fast list only — no network health checks by default
- **Context delete**: Immediate deletion, no confirmation prompt

### Claude's Discretion

- DB-specific backup flags (whether to pass through pg_dump options)
- Token expiry handling (error + hint vs auto re-login prompt)
- Partial failure handling (list all results vs stop on first error)
- JSON output progress behavior (final only vs streaming NDJSON)
- Exit code granularity (simple 0/1 vs specific codes)
- Network timeout configuration

</decisions>

<specifics>
## Specific Ideas

- Adapter tests should verify basic protocol check after enable (not just lifecycle start/stop)
- Error messages should include troubleshooting hints ("Error: port 12049 already in use. Check if another instance is running.")
- Verbose mode (-v) shows timing for each step ("Draining connections... done (2.3s)")
- CLI should offer retry prompt for retryable operations ("Failed. Retry? [y/N]")
- Connection errors vs server errors should be visually distinguishable in output
- Spinner with status text for long-running operations ("Draining connections (2 remaining)...")

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 05-adapters-auxiliary*
*Context gathered: 2026-02-02*
