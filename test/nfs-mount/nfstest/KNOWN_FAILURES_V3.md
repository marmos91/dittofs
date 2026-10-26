# nfstest known failures — NFSv3 overlay

Failures expected under the `v3` option set only, graded together with `KNOWN_FAILURES.md`.

CI grades nfstest with [`known.yaml`](known.yaml). This table is the knfsd baseline's: [`baseline-knfsd.sh`](baseline-knfsd.sh) grades knfsd's runs against it through `test/common/known-failures.sh`, says which tables grade which option set, and reports which of knfsd's failures these rows cover.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| cache/*: Hard link count should have * | bug | a directory's nlink reads one short right after a local MKDIR, so the test's baseline is stale; every file-cache check passes | [#3099](https://github.com/marmos91/dittofs/issues/3099) |
| cache/*: Directory listing should have * | suite | the client sees the second client's new subdirectory before the directory cache expires; knfsd fails 7 of 11 of these on the same kernel (`baseline-knfsd.md`, `6.17.0-1022-azure`) | - |
| cache/acdirmax_*: traceback: Exception: Hard link count of directory is N, should have been N | bug | as the hard link count row | [#3099](https://github.com/marmos91/dittofs/issues/3099) |
