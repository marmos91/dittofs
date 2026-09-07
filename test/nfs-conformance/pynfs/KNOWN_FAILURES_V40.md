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

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| OPEN4 | bug | exclusive re-create with the same verifier answers EXIST on the SQL backends, which do not persist the create verifier | #2317 |
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
| WRT18 | bug | change attribute frozen across a write session | #2342 |
| RPLY8 | suite | knfsd fails this too — replay of a waiting LOCKU | - |
