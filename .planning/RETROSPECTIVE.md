# Project Retrospective

*A living document updated after each milestone. Lessons feed forward into future planning.*

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

### Top Lessons (Verified Across Milestones)

1. Gap analysis before planning saves significant effort
2. Consistent patterns across protocol versions (NFSv3/v4) reduce implementation complexity
