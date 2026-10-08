# cthon04 known failures — NFSv4

Failures expected when cthon04 runs against DittoFS under the `v4.0`, `v4.1`, `v4.1-smallio` option sets.
`run.sh` grades every run against this table through
`test/common/known-failures.sh`: a failure listed here is reported but does
not fail CI, and any failure not listed does. A listed test that passes is
reported as "consider removing".

Names are as the grader prints them, e.g. `basic/test6`, `special/*`. Shell globs work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| special/telldir | semantics | seekdir() to a saved cookie can return no entries: cookies are a full 64-bit hash (pkg/metadata/cookies.go), and one with the top bit set is likely read as a negative directory offset by the Linux client; intermittent, the hash moves with the share's handle | DIT-32 |
