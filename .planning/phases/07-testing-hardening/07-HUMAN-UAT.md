---
status: partial
phase: 07-testing-hardening
source: [07-VERIFICATION.md]
started: 2026-04-18T22:00:00Z
updated: 2026-04-18T22:00:00Z
---

## Current Test

[awaiting CI validation]

## Tests

### 1. E2E Matrix Live Run
expected: `go test -tags=e2e -run '^TestBackupMatrix' ./test/e2e/...` passes for all 6 sub-tests (3 engines × 2 destinations) with live dfs binary and test containers
result: [pending — requires compiled dfs binary + Docker]

### 2. Chaos Tests Live Run
expected: `go test -tags=e2e -run '^TestBackupChaos' ./test/e2e/...` passes — kill-mid-backup/restore transitions job to `interrupted`, no ghost MPUs remain after restart
result: [pending — requires Localstack + SIGKILL timing validation]

## Summary

total: 2
passed: 0
issues: 0
pending: 2
skipped: 0
blocked: 0

## Gaps
