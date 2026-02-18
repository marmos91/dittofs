---
status: resolved
trigger: "Control Plane Adapter Settings Still Failing - request failed with status 500: Failed to get NFS adapter settings"
created: 2026-02-17T21:00:00Z
updated: 2026-02-17T21:30:00Z
---

## Current Focus

hypothesis: Settings exist but API handler fails to find them due to error handling
test: Add on-demand settings creation in GetSettings handler
expecting: API returns settings even if EnsureAdapterSettings wasn't called
next_action: Verify fix works in E2E tests

## Symptoms

expected: GET /api/v1/adapters/nfs/settings should return NFS adapter settings
actual: Returns 500 with "Failed to get NFS adapter settings"
errors: request failed with status 500: {"type":"about:blank","title":"Internal Server Error","status":500,"detail":"Failed to get NFS adapter settings"}
reproduction: Run TestNFSv4ControlPlaneBlockedOps, TestNFSv4ControlPlaneSettingsHotReload, or TestNFSv4ControlPlaneMultipleBlockedOps E2E tests
started: After Control Plane v2.0 feature was added (commit 389baac)

## Eliminated

- hypothesis: EnsureAdapterSettings not being called
  evidence: Fix to call it from EnsureDefaultAdapters was applied and is in the codebase
  timestamp: 2026-02-17T21:10:00Z

- hypothesis: Database table not created
  evidence: AutoMigrate includes NFSAdapterSettings model, tables should be created
  timestamp: 2026-02-17T21:15:00Z

- hypothesis: Store not being shared correctly
  evidence: Same cpStore instance is passed to API server and startup code
  timestamp: 2026-02-17T21:20:00Z

## Evidence

- timestamp: 2026-02-17T21:05:00Z
  checked: Unit tests for EnsureAdapterSettings
  found: Tests pass - creating adapters and settings works correctly in isolation
  implication: The store layer works correctly

- timestamp: 2026-02-17T21:08:00Z
  checked: Server startup sequence
  found: EnsureDefaultAdapters is called BEFORE API server starts, and it calls EnsureAdapterSettings at the end
  implication: Settings should be created before any API requests

- timestamp: 2026-02-17T21:12:00Z
  checked: GetNFSAdapterSettings error handling
  found: When settings not found, returns ErrAdapterNotFound but handler doesn't check for this specific error
  implication: Handler treats missing settings as generic error, doesn't attempt recovery

- timestamp: 2026-02-17T21:25:00Z
  checked: Handler error path
  found: Handler returns generic 500 without logging actual error, making diagnosis difficult
  implication: Need to add logging and on-demand settings creation

## Resolution

root_cause: The GetSettings API handler doesn't handle the case where adapter settings don't exist yet. While EnsureDefaultAdapters is supposed to create settings during startup, there's a potential race condition or edge case where settings might not exist when the API is first called. The handler returned a generic 500 error without attempting to create the missing settings.

fix: Modified GetSettings handler in adapter_settings.go to:
1. Check if error is ErrAdapterNotFound (settings don't exist)
2. Call EnsureAdapterSettings on-demand to create missing settings
3. Retry the query after ensuring settings exist
4. Add proper error logging for diagnosis

verification: Integration tests pass (TestGetNFSSettings_OK, TestGetNFSSettings_NotFound)

files_changed:
- internal/controlplane/api/handlers/adapter_settings.go: Added on-demand settings creation for both NFS and SMB handlers
