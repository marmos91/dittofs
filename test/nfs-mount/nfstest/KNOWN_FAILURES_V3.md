# nfstest known failures — NFSv3 overlay

Failures expected under the `v3` option set only, graded together with `KNOWN_FAILURES.md`.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| dio/[rw]size: traceback: *mount.nfs: requested NFS version or transport protocol is not supported for /tmp/dittofs-test | env | the subtest remounts with its own options (hard,intr,rsize=4096,wsize=4096) and drops mountport; an NFSv3 client then cannot find MOUNT on a non-standard port. The leading * also matches the first remount on a fresh host, where systemd starting rpc-statd adds a line to the error and the name loses its "Exception: " prefix | - |
| dio/[rw]size: traceback: Exception: Packet trace file is empty | env | follows from the failed remount above: the subtest's capture holds no traffic | - |
