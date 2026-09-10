# Wave 3.5 lane prompts (fan-out 2026-09-10, base 2c9ea79e7)

Two lanes, one writer for the code consolidation, one walker for the pynfs bookkeeping.

## Lane A — consolidation writer (fix/error-universe-consolidation)

One branch, one PR. Zero behaviour change. Base origin/develop 2c9ea79e7.

### Part 1 — sentinel normalization at the payload choke point

`internal/adapter/common/read_payload.go` (ReadFromBlockStore, the `fmt.Errorf("ReadAt error: %w", readErr)` fallthrough), `write_payload.go` (WriteToBlockStore passthrough + CommitBlockStore hard-error return) classify block sentinels as real `*merrs.StoreError` (multi-%w, sentinel preserved in Cause):

- `engine.ErrStoreClosed` → `merrs.ErrStaleHandle` (Message mentions closed store)
- everything else (ErrChunkContentMismatch, ErrChunkRefMissing, ErrRemoteUnavailable, ErrNotDurableYet, unknown) → `merrs.ErrIOError` (Message preserves the sentinel text)
- context.Canceled / DeadlineExceeded keep their raw pass-through (already before the wrap)
- io.EOF / ErrUnexpectedEOF unchanged (nil-error path)

Convert all 17 `MapContentTo*` call sites to the uniform `MapToNFS3/4/SMB`:
- nfs/v4/handlers: read.go, read_plus.go, write.go, commit.go, deallocate.go (5 sites)
- smb/handlers: read.go, write.go, close.go, flush.go, ioctl_sparse.go, ioctl_copychunk.go (12 sites)
- nfs/v3/handlers/write.go (1 site)
Delete `content_errmap.go` + `content_errmap_test.go` (8 pin tests move/convert into a payload-test at read_payload_test.go/write_payload_test.go level or errmap_test.go).
Test surface: errmap_test.go references MapContentTo (2 refs) — convert those to table pins on the new wrapper shape.
Update `doc.go:25` comment (lock_errmap.go + content_errmap.go naming).

### Part 2 — StatusFor extraction into the adapter types packages

Move the per-protocol switches out of common/ into the packages whose wire codes they produce:

- `internal/adapter/nfs/types/statusfor.go`: `StatusFor(code merrs.ErrorCode) uint32` — from errorMap's NFS3 column
- `internal/adapter/nfs/v4/types/statusfor.go`: `StatusFor(code merrs.ErrorCode) uint32` — from errorMap's NFS4 column
- `internal/adapter/smb/types/statusfor.go`: `StatusFor(code merrs.ErrorCode) Status` + `StatusForLock(code merrs.ErrorCode) Status` — from errorMap's SMB column + lockErrorMap's SMB deltas

Naming convention IS the interface: same name, same signature, same contract in every adapter package; NO Go interface declared. StatusForLock is forced by MS-SMB2 3.3.5.14 (LOCK denial → STATUS_LOCK_NOT_GRANTED vs I/O sharing violation → STATUS_FILE_LOCK_CONFLICT), both contexts live (lock manager conflict + CheckLockForIO), NFS needs no split (NFS4ERR_DENIED both, NFSv3 no lock procedure).

Delete `errmap.go` + `lock_errmap.go` entirely. v4's state-machine mapper (MapStateError in v4/v41/handlers/deps.go) stays OUT — it unwraps NFS4StateError.Status, not merrs.ErrorCode.

Convert call sites (census at fan-out):
- `common.MapToNFS3` × 16 → `types.StatusFor` (nfs/types pkg import path: internal/adapter/nfs/types)
- `common.MapToNFS4` × 50 → `v4types.StatusFor` (internal/adapter/nfs/v4/types)
- `common.MapToSMB` × 47 → `smbtypes.StatusFor` (internal/adapter/smb/types)
- `common.MapLockToSMB` × 6 → `smbtypes.StatusForLock` (smb/handlers/lock.go, lock_async.go)
- `common.MapContentTo*` → gone (Part 1 already converted them to common.MapTo*, then this lane renames to StatusFor)

xdr/errors.go MapStoreErrorToNFSStatus (audit wrapper) delegates to types.StatusFor directly now.

Tests: port TestErrorMapCoverage + TestMapToNFS3/4/SMB + TestMapLockToSMB_* into one enum-walk test per types package (the #2499 pattern): one test per package asserting every merrs.ErrorCode has a switch arm (walk ErrNotFound..ErrConflict) + table pins for nil/non-code/LOCK-delta semantics. CI-fails on any code a switch forgot.

### Rules

- Lane-owned files: internal/adapter/common/{read_payload,write_payload,doc}.go, content_errmap*.go, errmap*.go, lock_errmap*.go; the 17+113 call-site files above; the three types packages (new statusfor.go + new statusfor_test.go each).
- Never hand-edit docs/guide/cli.md.
- Signed commits, no AI/Co-Authored-By mentions; comments must not reference issue/PR/phase IDs.
- Verification: go build ./..., go vet ./..., gofmt -l, go test -race -count=1 ./internal/adapter/... plus full go test -count=1.
- No pull/rebase/merge during run. Push via git push origin +HEAD:refs/heads/fix/error-universe-consolidation.
- Write pr-body-error-universe-consolidation.md at worktree root (suggested title: "consolidate every error mapping into the adapter types packages"; no Closes #N — no lane maps to an open issue).
- Stop condition: final message line BRANCH=<name> HEAD=<full sha> PRBODY=<path> STATUS=GREEN|BLOCKED.
- Premise failure means STOP and report BLOCKED, never improvise.

## Lane B — pynfs v4.1 walker (Wave 2 residual, #2340 rows)

Walk-only, no code changes. The nix pynfs binaries are confirmed at /nix/store/9nq1f3wyaf8g55cjx2axca79i50klrnf-pynfs-2026-03-27/bin/{pynfs-4.0,pynfs-4.1}.

- Server: cd test/posix && ./setup-posix.sh --no-mount (memory profile is the default; if it fails, STOP and report BLOCKED).
- Run: cd test/nfs-conformance/pynfs && PATH=/nix/store/9nq1f3wyaf8g55cjx2axca79i50klrnf-pynfs-2026-03-27/bin:$PATH ./run-pynfs.sh --minor-version 4.1 (memory profile default).
- Compare the output against test/nfs-conformance/pynfs/KNOWN_FAILURES_V41.md (row regex: ^\| *[A-Z]+[0-9]+[a-z]?*\|; the file's own tally is authoritative).
- Update KNOWN_FAILURES_V41.md: walk out any row that now passes (mark PASS + the fix that walked it out, per the file's existing conventions); never add rows without a demonstrated local pass; never re-count the tally by regex — read the file's own number and adjust it only if rows change.
- No source changes outside KNOWN_FAILURES_V41.md. If the run fails to start or produces fewer than the file's tally suggests, STOP and report BLOCKED.
- Signed commit (docs only): branch fix/wave2-pynfs-v41-walkout from origin/develop. Write pr-body-pynfs-v41-walkout.md at worktree root (suggested title: "pynfs v4.1: walk out the rows the Wave 2 fixes closed"; reference #2340 as related in the body, Closes #2340 only if every #2340 row in the file passes).
- Stop condition: BRANCH=<name> HEAD=<full sha> PRBODY=<path> STATUS=GREEN|BLOCKED.

## Sequencing

Both lanes in parallel managed worktrees based on refs/heads/develop (2c9ea79e7). No Closes #N for Lane A. Lane B may Closes #2340 only if every #2340 row walks out. After both return: serialized review fan-out (correctness + adversarial per branch), then PR open/merge pipeline.
