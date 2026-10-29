### dt run lint-repo, unit/windows, fio-verify, pynfs/badger-s3/1, smbtorture/memory

| Cell | Result | Time | Note |
|---|---|---|---|
| `lint-repo` | failed | 3s | `false` exited 1 |
| `unit/windows` | failed | 1m1s | 1 passed, 0 failed, 1 skipped; example.com/m/c: failed outside any test |
| `fio-verify` | passed | 1m4s |  |
| `pynfs/badger-s3/1` | not run | 0s | dt doesn't set up fixtures yet |
| `smbtorture/memory` | skipped | 0s | docker: not found |

#### New failures

| Cell | Test | Message |
|---|---|---|
| `lint-repo` | `false` | `` `false` exited 1 `` |
| `unit/windows` | `example.com/m/c` | `failed outside any test` |
