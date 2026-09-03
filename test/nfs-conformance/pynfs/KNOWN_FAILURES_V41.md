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

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| COUR2 | bug | courtesy-client state reclaimed too aggressively | #2327 |
| COUR3 | bug | courtesy-client state reclaimed too aggressively | #2327 |
| COUR5 | bug | courtesy-client state reclaimed too aggressively | #2327 |
| COUR6 | bug | courtesy-client state reclaimed too aggressively | #2327 |
| CSID1 | bug | current stateid not tracked across a COMPOUND | #2328 |
| CSID10 | bug | current stateid not tracked across a COMPOUND | #2328 |
| CSID2 | bug | current stateid not tracked across a COMPOUND | #2328 |
| CSID3 | bug | current stateid not tracked across a COMPOUND | #2328 |
| CSID4 | bug | current stateid not tracked across a COMPOUND | #2328 |
| CSID8 | bug | current stateid not tracked across a COMPOUND | #2328 |
| CSID9 | bug | current stateid not tracked across a COMPOUND | #2328 |
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
| COMP5 | bug | COMPOUND op-count and request-size limits not enforced | #2330 |
| SEQ6 | bug | COMPOUND op-count and request-size limits not enforced | #2330 |
| SEQ7 | bug | COMPOUND op-count and request-size limits not enforced | #2330 |
| LKPP1a | bug | lookupp41 | #2335 |
| LKPP1b | bug | lookupp41 | #2335 |
| LKPP1c | bug | lookupp41 | #2335 |
| LKPP1d | bug | lookupp41 | #2335 |
| LKPP1f | bug | lookupp41 | #2335 |
| LKPP1r | bug | lookupp41 | #2335 |
| LKPP1s | bug | lookupp41 | #2335 |
| LKPP2 | bug | lookupp41 | #2335 |
| PUTFH2 | bug | lookupp41 | #2335 |
| RNM10 | bug | names41 | #2333 |
| RNM11 | bug | names41 | #2333 |
| RNM20 | bug | names41 | #2333 |
| RNM4 | bug | names41 | #2333 |
| CSESS15 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS16a | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS23 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS25 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS26 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS27 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS28 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS29 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| CSESS9 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| DESCID1 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| DESCID2 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| DESCID4 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| DESCID7 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
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
| EID8 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| EID9 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| RECC3 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| RECC4 | bug | EXCHANGE_ID/CREATE_SESSION/DESTROY_CLIENTID validation | #2340 |
| SECNN1 | feature | SECINFO_NO_NAME not implemented | #2338 |
| SECNN2 | feature | SECINFO_NO_NAME not implemented | #2338 |
| SECNN3 | feature | SECINFO_NO_NAME not implemented | #2338 |
| SECNN4 | feature | SECINFO_NO_NAME not implemented | #2338 |
| DELEG24 | suite | knfsd fails this too — needs FATTR4_OPEN_ARGUMENTS (v4.2) | - |
| DELEG25 | suite | knfsd fails this too — needs FATTR4_OPEN_ARGUMENTS (v4.2) | - |
