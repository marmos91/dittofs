# Phase 2: Server & Identity - Context

**Gathered:** 2026-02-02
**Status:** Ready for planning

<domain>
## Phase Boundary

E2E tests validating server lifecycle management and user/group CRUD operations via CLI. Tests verify server starts/stops gracefully, users and groups can be managed, and invalid operations are rejected with clear errors.

</domain>

<decisions>
## Implementation Decisions

### Server Lifecycle Tests
- Start server as background process, poll readiness endpoint before tests
- **Readiness vs Health distinction:** Server is ready when all adapters started AND all store healthchecks pass
- Test both health and readiness endpoints
- Verify status endpoint response structure (version, uptime, adapters status)
- Test SIGINT/SIGTERM signal handling for graceful shutdown
- 5-second startup timeout before considering server failed
- Default config only (no config variations in this phase)
- No logging verification tests
- Multiple server instances are valid (different ports, separate databases)

### User CRUD Tests
- Verify all user fields (username, password, email, display name, metadata)
- Password verification: Test login flow AND verify password is stored hashed
- Comprehensive error scenarios: duplicate username, missing fields, invalid chars, delete user with permissions
- Full edit coverage: test updating all editable fields including password change
- Admin user protection: verify admin cannot be deleted
- Simple list only (skip pagination testing)
- Test password requirements (min length, complexity)
- Test self-service password change
- Test token invalidation after password change
- Test username format restrictions (special chars, length)

### Group Management Tests
- Verify group name and description fields
- Full bidirectional membership: group lists members, user shows group memberships
- Cascade delete: deleting group removes its permissions automatically
- Test multi-group membership (user in multiple groups)
- Idempotent membership operations: add/remove same user succeeds silently
- Auto-remove from groups when user deleted
- Test empty group deletion
- Test group name uniqueness
- Test group name format restrictions
- Full edit coverage for groups (rename, update description)
- System groups protection: verify cannot delete/modify system groups

### Test Isolation Strategy
- Shared server model (reuse Phase 1 infrastructure)
- Parallel where safe: independent tests run in parallel, shared state sequential
- Server lifecycle tests run first, CRUD tests assume running server
- Verify admin user intact after test suite
- Cleanup then fail: attempt cleanup even on failure, then report

### Claude's Discretion
- Graceful shutdown verification level (status check vs resource cleanup)
- Username case sensitivity handling
- Test naming convention (test prefix, UUID suffix)
- Fixture strategy (fresh each test vs shared fixtures)
- Cleanup approach (delete in teardown vs unique prefixes)

</decisions>

<specifics>
## Specific Ideas

- Readiness check = all adapters started + all store healthchecks pass (distinct from ongoing health)
- Multiple DittoFS instances can coexist on different ports with separate databases

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 02-server-identity*
*Context gathered: 2026-02-02*
