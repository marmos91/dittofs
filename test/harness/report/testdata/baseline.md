# smbtorture Baseline Results

> Written by `dt known baseline` from one cell's results in a run. Don't edit it by hand:
> write it again, and commit it through a pull request. It grades nothing: a run is graded
> against the suite's `known.yaml`.

**Date:** 2026-10-19
**DittoFS Commit:** d6223c90
**Cell:** smbtorture/memory
**Platform:** linux
**smbtorture:** smbtorture 4.22.6

## Overall Summary

| Metric | Count |
|--------|-------|
| Total Tests | 26 |
| Passed | 11 |
| Failed | 11 |
| Known failures | 8 |
| New failures | 3 |
| Skipped | 4 |
| Pass Rate | 42.3% |

## Per-Sub-Suite Breakdown

| Sub-Suite | Pass | Fail | Skip | Total | Pass Rate |
|-----------|------|------|------|-------|-----------|
| smb2.acls | 2 | 1 | 0 | 3 | 67% |
| smb2.charset | 3 | 1 | 0 | 4 | 75% |
| smb2.compound | 1 | 0 | 0 | 1 | 100% |
| smb2.compound_async | 1 | 0 | 0 | 1 | 100% |
| smb2.connect | 1 | 0 | 0 | 1 | 100% |
| smb2.lock | 1 | 2 | 1 | 4 | 33% |
| smb2.multichannel | 0 | 0 | 1 | 1 | N/A |
| smb2.oplock | 1 | 7 | 1 | 9 | 12% |
| smb2.rename | 1 | 0 | 0 | 1 | 100% |
| smb2.replay | 0 | 0 | 1 | 1 | N/A |

## Passing Tests (11)

<details><summary>Show 11 test(s)</summary>

### smb2.acls (2)
- smb2.acls.OWNER
- smb2.acls.inheritance

### smb2.charset (3)
- smb2.charset.Testing
- smb2.charset.Testing
- smb2.charset.Testing

### smb2.compound (1)
- smb2.compound.related1

### smb2.compound_async (1)
- smb2.compound_async.flush_close

### smb2.connect (1)
- smb2.connect

### smb2.lock (1)
- smb2.lock.lock2

### smb2.oplock (1)
- smb2.oplock.exclusive1

### smb2.rename (1)
- smb2.rename.simple

</details>

## Failing Tests (11)

<details><summary>Show 11 test(s)</summary>

### smb2.acls (1)
- smb2.acls.GENERIC **(NEW)**

### smb2.charset (1)
- smb2.charset.Testing _(known)_

### smb2.lock (2)
- smb2.lock.lock1 _(known)_
- smb2.lock.lock10 **(NEW)**

### smb2.oplock (7)
- smb2.oplock.batch2 _(known)_
- smb2.oplock.batch3 _(known)_
- smb2.oplock.batch4 _(known)_
- smb2.oplock.batch5 _(known)_
- smb2.oplock.batch6 _(known)_
- smb2.oplock.batch7 _(known)_
- smb2.oplock.stream1 **(NEW)**

</details>

## Skipped Tests (4)

<details><summary>Show 4 test(s)</summary>

### smb2.lock (1)
- smb2.lock.async

### smb2.multichannel (1)
- smb2.multichannel.interface_info

### smb2.oplock (1)
- smb2.oplock.batch1

### smb2.replay (1)
- smb2.replay.channel-sequence

</details>
