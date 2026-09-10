# Wave 4 fix-wave 1 lane prompts (base 41a64c8d4 → prompts commit)

Four parallel worker lanes, file-disjoint. Workers implement, verify, push branches, write
pr-body files outside the repo, do NOT open PRs. Orchestrator runs review fan-out + Copilot
babysitting + serialized merges after.

Design decisions settled by the orchestrator (2026-09-10):

- **Session-id squat (#2518)**: MS-SMB2 3.3.5.5 + SessionTable rule — nonzero SessionId matching
  no session → STATUS_USER_SESSION_DELETED. RFC-mandated; Windows/Isilon behave the same.
- **Encryption bypass (#2518)**: fail closed — anonymous/guest + tree.EncryptData →
  STATUS_ACCESS_DENIED (server cannot derive keys without a session key; 3.3.5.3). Disclose the
  wire-behavior change.
- **AppInstanceId (#2518)**: scope the force-close filter to same share + same file identity per
  MS-SMB2 3.3.5.9.13; preserve today's no-match fallback.

## Lane A — highs (#2518)

Branch fix/wave4-highs-2518. Files: handlers/session_setup.go, response.go, handlers/durable_context.go, handlers/create.go + new wave4_highs_*_test.go. Fixes 1-3 above + the TreeID==0 rows (response.go:514,:728) only if a real hole remains after checking the server-wide gate; otherwise disclose covered. PR body /tmp/pr-body-wave4-highs-2518.md, Closes #2518.

## Lane B — setinfo-rw (#2512)

Branch fix/wave4-setinfo-rw-2512. Files: handlers/set_info.go, read.go, write.go, query_info.go + new wave4_rw_*_test.go. Ten LIVE rows: short-buffer INFO_LENGTH_MISMATCH (set_info.go:1692), FILE_WRITE_EA gate (:226-227,:1776), FILE_WRITE_DATA gate on EOF (:1545), Rename non-zero RootDirectory → STATUS_INVALID_PARAMETER (:868-873), RequestedAllocSize/CreateOptions unlocked writes (:1695,:1766), PositionInfo lock everywhere (read.go:145, set_info.go:1654, query_info.go:755,945), WRITE advances PositionInfo, DecodeWriteRequest speculative fallback removed (write.go:128-131), RDMA Channel rejected (write.go:104-106, read.go:104), atime SetFileAttributes errors logged at Debug (read.go:448, write.go:529/532). Structure rows (dup gate, ResolveForWrite clone) left → disclose. PR body /tmp/pr-body-wave4-setinfo-rw-2512.md, Closes #2512.

## Lane C — compound-tree (#2514)

Branch fix/wave4-compound-tree-2514. Files: handlers/compound.go, tree_connect.go, tree_disconnect.go + new wave4_ct_*_test.go. Rows: signature failure classes (compound.go:654-709 — invalid signature → ACCESS_DENIED + connection teardown per MS-SMB2), CreditCharge for sub-commands 2..N (:98,:130-152), CHANGE_NOTIFY first-command gate (:167,:245 vs :953), fileIDOffset non-FileId frames (:714-722 — oplock/lease break ACKs carry no FileId), ReplaceCallback bool + MarkStarted (:196,:228), TreeConnect EncryptData dialect >= 3.0 gate (tree_connect.go:316-321), parked-LOCK callback synchronous (tree_disconnect.go:73). response.go row owned by lane A → disclose. PR body /tmp/pr-body-wave4-compound-tree-2514.md, Closes #2514.

## Lane D — security-hygiene (#2516)

Branch fix/wave4-security-hygiene-2516. Files: handlers/security.go, ioctl_dispatch.go, ioctl_validate_negotiate.go, doc.go, conn_types.go, framing.go, hooks.go, lease_context.go + new wave4_sec_*_test.go. Rows: malformed-ACE panic guard aceSize < 8 → STATUS_INVALID_ACL (security.go:929, red-without-fix test), IS_FSCTL flag validation (ioctl_dispatch.go:64), VALIDATE_NEGOTIATE MaxOutputResponse check (ioctl_validate_negotiate.go:43), stale doc.go (:10,:16), issue-number comment citations in the lane's own files only. PR body /tmp/pr-body-wave4-security-hygiene-2516.md, Closes #2516.

## Shared boilerplate (all lanes)

- Worktree-isolated, base = current HEAD. No pull/rebase/merge, no gh, no PR opening.
- Smallest correct diff; no refactors beyond the named rows (structure = Wave 5).
- New code comments describe behavior, never cite issues/PRs (CLAUDE.md rule).
- Signed commits (-S), subject ≤ 72 chars, no AI mentions. Check git diff --stat before committing.
- Verify: go build ./... ; go vet ./... ; gofmt -l empty; go test -race -count=1 ./internal/adapter/smb/... ; full go test -count=1 ./... ; red-without-fix per behavior fix (temporary local revert).
- Windows CI gotchas: renewed.Equal(before) tolerance; QF1006 poll-loop condition in for header.
- Push: git push origin +HEAD:refs/heads/<branch>.
- Stop condition (final message): BRANCH= HEAD=<full sha> PRBODY= STATUS=GREEN|BLOCKED. Premise mismatch → BLOCKED with the reason.
