# nfstest known failures — every option set

Failures expected when nfstest runs against DittoFS under any option set. Each option set is graded against this table plus its version's overlay (`KNOWN_FAILURES_V3.md`, `_V4.md`, and `_V40.md` for `v4.0`), so a row every set shares is written once.

CI grades nfstest with [`known.yaml`](known.yaml). This table is the knfsd baseline's: [`baseline-knfsd.sh`](baseline-knfsd.sh) grades knfsd's runs against it through `test/common/known-failures.sh`, says which tables grade which option set, and reports which of knfsd's failures these rows cover.

Names are as the grader prints them: `<module>/<subtest>: <message>`, and for a Python traceback `<module>/<subtest>: traceback: <exception line>`, with addresses as `<ip>` and numbers as `N`. Shell globs work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| posix/read: file st_atime should be updated | semantics | reading a file does not update its atime; may be deliberate | [#2982](https://github.com/marmos91/dittofs/issues/2982) |
| dio/*: WRITEs should be cached for buffered I/O | suite | buffered writes reach the server before close under `v4.1-noac`, where the client writes through; knfsd fails 16 of 20 there on the same kernel and DittoFS passes them on every other set (`baseline-knfsd.md`, `6.17.0-1022-azure`) | - |
