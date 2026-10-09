# nfstest known failures — NFSv4.0 only

Failures expected under the `v4.0` option set and no other. `run.sh` grades
`v4.0` against this table *together with* `KNOWN_FAILURES_V4.md`. A row
belongs here, not there, when the same failure on a v4.1 set would be a
regression: a row in the shared table would excuse it on every v4 set.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| dio/vectored_io: traceback | unknown | NFSv4.0 only, deterministic: from vectored_io_057 on (the vectored WRITE cases) the capture holds no packets at all, not even the OPEN, though the writes succeed; passes on the v4.1 sets | [#2981](https://github.com/marmos91/dittofs/issues/2981) |
