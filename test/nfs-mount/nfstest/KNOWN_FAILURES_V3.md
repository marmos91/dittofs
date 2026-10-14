# nfstest known failures — NFSv3 overlay

Failures expected under the `v3` option set only, graded together with `KNOWN_FAILURES.md`.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| cache/*: Hard link count should have * | bug | a directory's nlink reads one short right after a local MKDIR, so the test's baseline is stale; every file-cache check passes | [#3099](https://github.com/marmos91/dittofs/issues/3099) |
| cache/*: Directory listing should have * | suite | the client sees the second client's new subdirectory before the directory cache expires; knfsd fails 7 of 11 of these on the same kernel (`baseline-knfsd.md`, `6.17.0-1022-azure`) | - |
| cache/acdirmax_*: traceback: Exception: Hard link count of directory is N, should have been N | bug | as the hard link count row | [#3099](https://github.com/marmos91/dittofs/issues/3099) |
