# nfstest known failures — every option set

Failures expected when nfstest runs against DittoFS under any option set. Each option set is graded against this table plus its version's overlay (`KNOWN_FAILURES_V3.md`, `_V4.md`, and `_V40.md` for `v4.0`), so a row every set shares is written once.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

Names are as the grader prints them: `<module>/<subtest>: <message>`, and for a Python traceback `<module>/<subtest>: traceback: <exception line>`, with addresses as `<ip>` and numbers as `N`. Shell globs work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| posix/read: file st_atime should be updated | semantics | reading a file does not update its atime; may be deliberate | [#2982](https://github.com/marmos91/dittofs/issues/2982) |
| posix/seekdir: traceback: ValueError: NULL pointer access | semantics | readdir() after seekdir() to a saved cookie returns nothing (NULL), the same failure as cthon04 special/telldir | [#2979](https://github.com/marmos91/dittofs/issues/2979) |
| posix/telldir: traceback: ValueError: NULL pointer access | semantics | as posix/seekdir: the cookie from telldir() does not seek back | [#2979](https://github.com/marmos91/dittofs/issues/2979) |
| dio/*: WRITE (*) should be sent | unverified | a WRITE the client is expected to send is not in the capture; which ones varies between runs. Not yet checked against knfsd | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
| dio/*: WRITEs should be cached for buffered I/O | unverified | buffered writes reach the server before close. Not yet checked against knfsd | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
| dio/vectored_io: WRITE should be sent to the server with size * | unverified | how the client splits vectored direct writes; the count varies between runs. Not yet checked against knfsd | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
