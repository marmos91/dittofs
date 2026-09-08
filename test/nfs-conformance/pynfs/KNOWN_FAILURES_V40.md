# pynfs Known Failures — NFSv4.0

Tests listed here are expected to fail and will NOT cause CI to report a
failure. Only NEW failures (not in this list) cause CI to fail. This is the same
blacklist model the POSIX and SMB conformance harnesses use, and all of them
share the table format and the `test/common/known-failures.sh` parser.

The `Test Name` column is the pynfs test **code** — the identifier you pass
back to re-run one test:

```bash
./run-pynfs.sh --no-setup --minor-version 4.0 --tests LOOK1 --verbose
```

`pynfs-4.0 --showcodes` lists them all. Shell-glob wildcards are supported
(`LAYOUT*`).

Categories:

- **proto** — NFSv4 protocol behaviour, not a server bug.
- **feature** — something DittoFS deliberately does not implement.
- **suite** — the assertion is not one a conformant server is expected to
  satisfy. **Only valid with knfsd evidence**: the test must appear in
  [`baseline-knfsd.md`](baseline-knfsd.md) as one the Linux kernel server also
  does not pass. Without that, this category is just a bug in disguise.
- **bug** — a real DittoFS defect, tracked by the linked issue. These are meant
  to leave the list; walk the row out when the fix lands.

## Removing a row

A row leaves this table on one piece of evidence: the literal verdict line

```
WRT2     st_write.testStateidOne                                  : PASS
```

read from the per-backend `pynfs.log` artifact, for every backend the suite
runs on rather than just the one you reproduced against.

A green grader is not that evidence. The grader exits with the count of
failures that are *not* listed here, and a pynfs test whose `DEPEND:`
prerequisites failed is skipped — it emits no `: FAILURE` line at all. Skip and
pass are therefore indistinguishable to anything counting failures, so a
prerequisite collapsing elsewhere in the suite reads exactly like the fix you
were hoping for. Remove the row on that reading and the test stops being graded
while it is still broken, which is the one outcome this table exists to prevent.

Check each backend separately, because a row can be backend-dependent: `OPEN4`
passed on `memory` and failed on the SQL backends for as long as those dropped
the create verifier. Memory-passes/SQL-fails is a diagnosis worth keeping, not a
flake worth re-running.

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| CLOSE4 | bug | bad/old/stale stateid accepted | #2341 |
| CLOSE5 | bug | bad/old/stale stateid accepted | #2341 |
| CLOSE6 | bug | bad/old/stale stateid accepted | #2341 |
| LKT9 | bug | bad/old/stale stateid accepted | #2341 |
| LKU6b | bug | bad/old/stale stateid accepted | #2341 |
| LOCK10 | bug | bad/old/stale stateid accepted | #2341 |
| LOCK9b | bug | bad/old/stale stateid accepted | #2341 |
| LOCK9c | bug | bad/old/stale stateid accepted | #2341 |
| OPCF1 | bug | bad/old/stale stateid accepted | #2341 |
| OPCF6 | bug | bad/old/stale stateid accepted | #2341 |
| OPDG2 | bug | bad/old/stale stateid accepted | #2341 |
| OPDG3 | bug | bad/old/stale stateid accepted | #2341 |
| OPDG6 | bug | bad/old/stale stateid accepted | #2341 |
| OPDG7 | bug | bad/old/stale stateid accepted | #2341 |
| WRT18 | feature | write-session freeze deliberately holds ctime, so change cannot advance | - |
| RPLY8 | suite | knfsd fails this too — replay of a waiting LOCKU | - |
