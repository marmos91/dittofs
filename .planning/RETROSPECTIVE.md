# Project Retrospective

*A living document updated after each milestone. Lessons feed forward into future planning.*

## Milestone: v4.7 — Offline/Edge Resilience

**Shipped:** 2026-03-20
**Phases:** 4 | **Plans:** 10

### What Was Built
- Per-share cache retention policies (pin/ttl/lru) with full REST API and CLI integration
- S3 health monitor with circuit breaker, exponential backoff, and automatic recovery
- Offline read/write paths — local-first operation during S3 outages
- Health observability via REST API, `dfs status`, `dfsctl status`
- NTLM challenge flag cleanup (removed unimplemented encryption capabilities)
- Share hot-reload integration tests (OnShareChange callback lifecycle)
- Edge test infrastructure via Pulumi (PR #286, outside GSD)

### What Worked
- Clean phase dependencies: 63 (retention) → 64 (health) → 65 (offline) built naturally on each other
- Circuit breaker pattern at syncer level was clean — single integration point for all remote access
- Atomic types for health state avoided mutex contention on hot paths
- Phase 68 (protocol correctness) fit naturally into the milestone despite being roadmapped under v4.6

### What Was Inefficient
- Phase 66 (edge test infrastructure) was delivered outside GSD via PR #286, causing audit traceability gaps
- Nyquist validation was skipped for all 4 phases — should be incorporated into execution flow
- Phase 68 was planned under v4.6 but executed in v4.7 — milestone scoping could be tighter

### Patterns Established
- Health-gated remote access: check `IsRemoteHealthy()` before any remote store operation
- Degraded 200 (not 503) for edge nodes: health endpoint indicates degradation without triggering K8s restarts
- RetentionPolicy as string type for GORM/JSON compatibility with empty → LRU default

### Key Lessons
1. Circuit breaker + health monitor is the right pattern for remote store resilience — applies cleanly to syncer, eviction, and read paths
2. Per-file access tracking enables both LRU and TTL eviction — worth the metadata overhead
3. Work delivered outside GSD framework creates audit gaps — better to create minimal GSD artifacts even for PR-driven work
4. Phase 68 proved cross-milestone flexibility is valuable — protocol fixes fit naturally alongside edge resilience

### Cost Observations
- Sessions: ~6
- Notable: 10 plans in 6 days — efficient execution due to clean phase dependencies

---

## Milestone: v4.3 — Protocol Gap Fixes

**Shipped:** 2026-03-13
**Phases:** 3 | **Plans:** 1

### What Was Built
- NFSv4 READDIR mtime-based cookie verifier (fixes macOS Finder error -8062)
- Verified READDIRPLUS performance optimization already shipped in v4.0
- Verified LSA named pipe (lsarpc) already shipped in v3.6/v3.8

### What Worked
- Gap analysis before implementation saved time: 2 of 3 issues were already resolved
- Mtime-based verifier pattern reused from NFSv3 (consistent approach across protocol versions)
- Small, focused milestone kept scope tight and shipped in 2 days

### What Was Inefficient
- Issues #222 and #236 were tracked as open gaps despite being already implemented — earlier triage during v4.0 completion would have caught this
- No REQUIREMENTS.md existed for this milestone (was deleted after v4.0), making the milestone lightweight but less traceable

### Patterns Established
- "Verify pre-existing" phases: when gap analysis reveals work already done, mark phase complete with evidence links rather than creating unnecessary plans
- Advisory-only NFSv4 verifier validation: log mismatches at debug level but never reject (lenient approach for client compatibility)

### Key Lessons
1. Always check if a tracked issue was already resolved by earlier work before planning a phase
2. Small "gap fix" milestones are efficient for closing known issues between major feature milestones
3. Cookie verifier patterns should be consistent across NFS protocol versions (v3 and v4 now both use mtime)

### Cost Observations
- Sessions: 2
- Notable: Minimal cost — 2 of 3 phases required only verification, not implementation

---

## Cross-Milestone Trends

### Process Evolution

| Milestone | Phases | Plans | Key Change |
|-----------|--------|-------|------------|
| v4.3 | 3 | 1 | Pre-existing verification pattern for gap fixes |
| v4.7 | 4 | 10 | Circuit breaker pattern for remote store resilience; cross-milestone phase flexibility |

### Top Lessons (Verified Across Milestones)

1. Gap analysis before planning saves significant effort
2. Consistent patterns across protocol versions (NFSv3/v4) reduce implementation complexity
3. Clean phase dependencies enable efficient sequential execution (v4.7: retention → health → offline)
