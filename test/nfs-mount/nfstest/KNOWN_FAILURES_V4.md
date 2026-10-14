# nfstest known failures — NFSv4 overlay

Failures expected under every v4 option set (`default`, which runs NFSv4.2, and `v4.0`, `v4.1`, `v4.1-noac`, `v4.1-smallio`), graded together with `KNOWN_FAILURES.md`.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| delegation/basic04: DELEGRETURN should be sent after the close | suite | write delegation + stat: the client keeps the delegation past close; knfsd fails it too under `v4.1` and `v4.1-smallio` on the same kernel (`baseline-knfsd.md`, `6.17.0-1022-azure`); knfsd never reaches it under `default` (no write delegation nfstest recognises at 4.2) or `v4.0`, so those sets rest on the `v4.1` evidence | - |
| delegation/basic10: DELEGRETURN should be sent after the close | unverified | as basic04, on a read-write open; knfsd fails it under `v4.1` and `v4.1-smallio` but passes it under `v4.1-noac`, where DittoFS failed it in one of two nightlies (`baseline-knfsd.md`, `6.17.0-1022-azure`) | [#3096](https://github.com/marmos91/dittofs/issues/3096) |
| delegation/basic0[369]: DELEGRETURN should be sent after the close | bug | the client does not return the delegation after close; knfsd passes these on `v4.1` and `v4.1-smallio` on the same kernel (under `v4.1-noac` it fails basic06 too); it never reaches them under `default` or `v4.0`, so those sets rest on the `v4.1` evidence | [#3096](https://github.com/marmos91/dittofs/issues/3096) |
| delegation/basic02: DELEGRETURN should be sent after the close | unverified | seen on `v4.1-noac` and `v4.0` only, but listed for every v4 set as there is no noac-only table; knfsd fails it too under noac and grants no write delegation on v4.0, so not yet sorted | [#3096](https://github.com/marmos91/dittofs/issues/3096) |
| delegation/basic*: CLOSE should be sent to the server | suite | the CLOSE is not in the capture (seen on DittoFS under `v4.1-noac`; on knfsd under `default`, `v4.0` and `v4.1-noac`); knfsd fails it too on the same kernel (`baseline-knfsd.md`, `6.17.0-1022-azure`) | [#3019](https://github.com/marmos91/dittofs/issues/3019) |
