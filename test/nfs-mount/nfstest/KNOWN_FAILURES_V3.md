# nfstest known failures — NFSv3

Failures expected when nfstest runs against DittoFS under the `v3`, `v3-noac` option sets.
`run.sh` grades every run against this table through
`test/common/known-failures.sh`: a failure listed here is reported but does
not fail CI, and any failure not listed does. A listed test that passes is
reported as "consider removing".

Names are as the grader prints them, e.g. `posix/read: file st_atime should be updated`, `posix/seekdir: *`. Shell globs work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
