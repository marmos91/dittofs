# nfstest known failures — NFSv4.0 only

Failures expected under the `v4.0` option set and no other, graded together with `KNOWN_FAILURES.md` and `KNOWN_FAILURES_V4.md`. A row belongs here, not in the v4 overlay, when the same failure on a v4.1 set would be a regression.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| dio/vectored_io: traceback: Exception: Packet trace file is empty: use --trcdelay option to give tcpdump time to flush buffer to packet trace | unverified | deterministic: from vectored_io_057 on (the vectored WRITE cases) the capture holds no packets at all, not even the OPEN, though the writes succeed; the v4.1 sets capture these cases fine | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
