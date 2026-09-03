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
| GATT11i | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT11j | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT3a | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT3b | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT3c | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT3d | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT3f | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT3r | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT3s | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT4a | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT4b | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT4c | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT4d | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT4f | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT4r | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT4s | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT6a | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT6b | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT6c | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT6d | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT6f | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT6r | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| GATT6s | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF5a | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF5b | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF5c | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF5d | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF5f | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF5r | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF5s | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF7a | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF7b | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF7c | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF7d | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF7f | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF7r | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| NVF7s | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT10 | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT12a | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT12b | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT12c | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT12d | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT12f | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT12s | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT12x | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT16 | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT17 | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT6d | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT6r | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| SATT8 | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF5a | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF5b | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF5c | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF5d | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF5f | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF5r | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF5s | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF7a | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF7b | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF7c | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF7d | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF7f | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF7r | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| VF7s | bug | attribute validation: VERIFY/NVERIFY/GETATTR/SETATTR accept attrs they must reject | #2325 |
| CID1 | bug | SETCLIENTID/SETCLIENTID_CONFIRM and RENEW validation | #2326 |
| CID2 | bug | SETCLIENTID/SETCLIENTID_CONFIRM and RENEW validation | #2326 |
| CID2a | bug | SETCLIENTID/SETCLIENTID_CONFIRM and RENEW validation | #2326 |
| RENEW3 | bug | SETCLIENTID/SETCLIENTID_CONFIRM and RENEW validation | #2326 |
| LOCK20 | bug | lock range merging and RENEW behaviour | #2331 |
| LOCKMRG | bug | lock range merging and RENEW behaviour | #2331 |
| CLOSE8 | bug | LOOKUP fails opening the test tree, so the test cannot run | #2332 |
| CLOSE9 | bug | LOOKUP fails opening the test tree, so the test cannot run | #2332 |
| LKU10 | bug | LOOKUP fails opening the test tree, so the test cannot run | #2332 |
| LOCK13 | bug | LOOKUP fails opening the test tree, so the test cannot run | #2332 |
| LOCK15 | bug | LOOKUP fails opening the test tree, so the test cannot run | #2332 |
| LOCK17 | bug | LOOKUP fails opening the test tree, so the test cannot run | #2332 |
| LINK2 | bug | '.' and '..' and malformed names not rejected | #2333 |
| LINK9 | bug | '.' and '..' and malformed names not rejected | #2333 |
| LOOK8 | bug | '.' and '..' and malformed names not rejected | #2333 |
| RNM10 | bug | '.' and '..' and malformed names not rejected | #2333 |
| RNM11 | bug | '.' and '..' and malformed names not rejected | #2333 |
| RNM20 | bug | '.' and '..' and malformed names not rejected | #2333 |
| CR11 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| CR13 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| OPEN14 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| OPEN2 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| OPEN21 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| OPEN23 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| OPEN25 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| OPEN27 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| OPEN30 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| OPEN4 | bug | OPEN claim, share-reservation and create-mode validation | #2334 |
| CMT2a | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| CMT2b | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| CMT2c | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| CMT2d | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| CMT2f | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| CMT2s | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LKT2a | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LKT2b | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LKT2c | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LKT2d | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LKT2f | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LKT2s | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LOOK5a | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LOOKP2a | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LOOKP2b | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LOOKP2c | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LOOKP2f | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LOOKP2r | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LOOKP2s | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| LOOKP3 | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| OPEN7a | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| OPEN7b | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| OPEN7c | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| OPEN7d | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| OPEN7f | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| OPEN7s | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| PUTFH2 | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| RM7 | bug | operation on the wrong object type returns NFS4_OK | #2335 |
| RDDR10 | bug | READDIR cookie and attribute-request validation | #2336 |
| RDDR7 | bug | READDIR cookie and attribute-request validation | #2336 |
| RDDR8 | bug | READDIR cookie and attribute-request validation | #2336 |
| RDDR9 | bug | READDIR cookie and attribute-request validation | #2336 |
| RPLY10 | bug | duplicate request cache replays the wrong status | #2337 |
| RPLY14 | bug | duplicate request cache replays the wrong status | #2337 |
| RPLY3 | bug | duplicate request cache replays the wrong status | #2337 |
| SEC2 | bug | SECINFO validation | #2339 |
| SEC3 | bug | SECINFO validation | #2339 |
| SEC5 | bug | SECINFO validation | #2339 |
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
| WRT1 | bug | WRITE ignores requested stability and special stateids | #2342 |
| WRT18 | bug | WRITE ignores requested stability and special stateids | #2342 |
| WRT2 | bug | WRITE ignores requested stability and special stateids | #2342 |
| WRT3 | bug | WRITE ignores requested stability and special stateids | #2342 |
| WRT4 | bug | WRITE ignores requested stability and special stateids | #2342 |
| WRT9 | bug | WRITE ignores requested stability and special stateids | #2342 |
| RPLY8 | suite | knfsd fails this too — replay of a waiting LOCKU | - |
