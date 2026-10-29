# cthon04 known failures — every option set

Failures expected when cthon04 runs against DittoFS under any option set. The `v3` set is graded against this table plus `KNOWN_FAILURES_V3.md`.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

cthon04 is graded per program (`basic/test6`, `special/telldir`), not per assertion, so a row excuses any failure of that program, not only the one its reason documents. Shell globs work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| special/op_chmod | bug | a WRITE that reaches the server after the file's owner chmods it to 0 is refused; knfsd lets the owner through regardless of mode. Whether it shows depends on when the client flushes: it fails on v3, v4.0 and v4.1-noac, and passes where the client holds the writes until close | [#3097](https://github.com/marmos91/dittofs/issues/3097) |
