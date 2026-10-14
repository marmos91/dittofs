# nfstest known failures — NFSv4 overlay

Failures expected under every v4 option set (`default`, which runs NFSv4.2, and `v4.0`, `v4.1`, `v4.1-noac`, `v4.1-smallio`), graded together with `KNOWN_FAILURES.md`.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| delegation/recall*: CB_RECALL should be sent to the client after a conflicting * is received from a second client | bug | no recall when a second client chmods, renames or removes the delegated file; a conflicting OPEN is recalled | [#3098](https://github.com/marmos91/dittofs/issues/3098) |
