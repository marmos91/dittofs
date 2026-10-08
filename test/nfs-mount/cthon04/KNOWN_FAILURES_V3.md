# cthon04 known failures — NFSv3

Failures expected when cthon04 runs against DittoFS under the `v3` option set.
`run.sh` grades every run against this table through
`test/common/known-failures.sh`: a failure listed here is reported but does
not fail CI, and any failure not listed does. A listed test that passes is
reported as "consider removing".

Names are as the grader prints them, e.g. `basic/test6`, `special/*`. Shell globs work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| basic/test6 | semantics | NFSv3 READDIR returns no "." and ".." entries; knfsd returns both and the Linux client passes the server's list through as-is | [#2978](https://github.com/marmos91/dittofs/issues/2978) |
| special/telldir | semantics | seekdir() to a saved cookie can return no entries: cookies are a full 64-bit hash (pkg/metadata/cookies.go), and one with the top bit set is likely read as a negative directory offset by the Linux client; intermittent, the hash moves with the share's handle | [#2979](https://github.com/marmos91/dittofs/issues/2979) |
