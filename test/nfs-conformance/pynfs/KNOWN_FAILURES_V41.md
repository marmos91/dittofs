# pynfs Known Failures — NFSv4.1

Tests listed here are expected to fail and will NOT cause CI to report a
failure. Only NEW failures (not in this list) cause CI to fail. This is the same
blacklist model the POSIX and SMB conformance harnesses use, and all of them
share the table format and the `test/common/known-failures.sh` parser.

The `Test Name` column is the pynfs test **code** — the identifier you pass
back to re-run one test:

```bash
./run-pynfs.sh --no-setup --minor-version 4.1 --tests LOOK1 --verbose
```

`pynfs-4.1 --showcodes` lists them all. Shell-glob wildcards are supported
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
| DELEG1 | bug | no delegation is granted | #2329 |
| DELEG2 | bug | no delegation is granted | #2329 |
| DELEG23 | bug | no delegation is granted | #2329 |
| DELEG26 | bug | no delegation is granted | #2329 |
| DELEG3 | bug | no delegation is granted | #2329 |
| DELEG4 | bug | no delegation is granted | #2329 |
| DELEG5 | bug | no delegation is granted | #2329 |
| DELEG6 | bug | no delegation is granted | #2329 |
| DELEG7 | bug | no delegation is granted | #2329 |
| DELEG8 | bug | no delegation is granted | #2329 |
| CSESS15 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS16a | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS25 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS26 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS27 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS28 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS29 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS9 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| DESCID1 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| DESCID2 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| DESCID4 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| DESCID8 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID4 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID5c | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID5d | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID5f | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID5g | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID6 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID6a | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID6b | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID6c | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID6d | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID6e | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID6f | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID6g | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID7 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID9 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| RECC3 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| RECC4 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| DELEG24 | suite | knfsd fails this too — needs FATTR4_OPEN_ARGUMENTS (v4.2) | - |
| DELEG25 | suite | knfsd fails this too — needs FATTR4_OPEN_ARGUMENTS (v4.2) | - |
