# nfstest known failures — NFSv4 overlay

Failures expected under every v4 option set (`v4.0`, `v4.1`, `v4.1-noac`, `v4.1-smallio`), graded together with `KNOWN_FAILURES.md`.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| delegation/basic*: DELEGRETURN should be sent after the close | unverified | the delegation is granted but the client does not return it after close; DittoFS or this kernel, not yet checked against knfsd | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
| delegation/basic*: CLOSE should be sent to the server | unverified | as the DELEGRETURN row; which subtests vary between runs | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
