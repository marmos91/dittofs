# nfstest known failures — NFSv3

Failures expected when nfstest runs against DittoFS under the `v3` option set.
`run.sh` grades every run against this table through
`test/common/known-failures.sh`: a failure listed here is reported but does
not fail CI, and any failure not listed does. A listed test that passes is
reported as "consider removing".

Names are as the grader prints them, e.g. `posix/read: file st_atime should be updated`, `posix/seekdir: *`. Shell globs work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| posix/read: file st_atime should be updated | semantics | reading a file does not update its atime, on every option set; may be deliberate | [#2982](https://github.com/marmos91/dittofs/issues/2982) |
| posix/seekdir: traceback | semantics | readdir() after seekdir() to a saved cookie returns nothing (NULL), the same failure as cthon04 special/telldir | [#2979](https://github.com/marmos91/dittofs/issues/2979) |
| posix/telldir: traceback | semantics | as posix/seekdir: the cookie from telldir() does not seek back | [#2979](https://github.com/marmos91/dittofs/issues/2979) |
| dio/*: WRITE (*) should be sent | client | a WRITE the client is expected to send is not in the capture; which ones varies between runs. Client behaviour, unverified against knfsd | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
| dio/*: WRITEs should be cached for buffered I/O | client | buffered writes reach the server before close. Client behaviour, unverified against knfsd | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
| dio/vectored_io: WRITE should be sent to the server with size * | client | how the client splits vectored direct writes; the count varies between runs (29-55). Unverified against knfsd | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
| dio/rsize: traceback | env | the subtest remounts with its own options (hard,intr,rsize=4096,wsize=4096) and drops mountport; an NFSv3 client then cannot find MOUNT on a non-standard port | - |
| dio/wsize: traceback | env | as dio/rsize | - |
