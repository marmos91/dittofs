# nfstest known failures — NFSv4 overlay

Failures expected under every v4 option set (`default`, which runs NFSv4.2, and `v4.0`, `v4.1`, `v4.1-noac`, `v4.1-smallio`), graded together with `KNOWN_FAILURES.md`.

CI grades nfstest with [`known.yaml`](known.yaml). This table is the knfsd baseline's: [`baseline-knfsd.sh`](baseline-knfsd.sh) grades knfsd's runs against it through `test/common/known-failures.sh`, says which tables grade which option set, and reports which of knfsd's failures these rows cover.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| delegation/recall*: CB_RECALL should be sent to the client after a conflicting * is received from a second client | bug | no recall when a second client chmods, renames or removes the delegated file; a conflicting OPEN is recalled | [#3098](https://github.com/marmos91/dittofs/issues/3098) |
| delegation/recall*: CB_RECALL should recall * delegation granted to client | env | a Linux 7.0 client (`7.0.0-1017-azure`) holds a delegation on the test's directory under `v4.1-noac`, and DittoFS recalls it when the second client removes or renames a file there; nfstest takes that CB_RECALL for the file's, whose own recall is the row above; 6.17 runners hold none and pass | - |
| delegation/recall*: DELEGRETURN should be sent with the stateid of * delegation being recalled | env | as the CB_RECALL should recall row | - |
| delegation/recall*: LOCK should be sent before returning the * delegation | env | as the CB_RECALL should recall row | - |
| delegation/recall*: READs should be sent to the server with the OPEN stateid after returning the delegation | env | as the CB_RECALL should recall row | - |
| posix/link: parent directory st_ctime should be updated | env | a Linux 7.0 or 7.1 client holding a directory delegation skips GETATTR after its own change; DittoFS keeps the holder's delegation, as RFC 8881 §10.9.2 says; fixed in Linux 7.2 (84800a8d1d22), and 6.17 runners pass | - |
| posix/link: parent directory st_mtime should be updated | env | as the posix/link st_ctime row | - |
| posix/rmdir: parent directory st_ctime should be updated | env | as the posix/link st_ctime row | - |
| posix/rmdir: parent directory st_mtime should be updated | env | as the posix/link st_ctime row | - |
| posix/unlink: parent directory st_ctime should be updated | env | as the posix/link st_ctime row | - |
| posix/unlink: parent directory st_mtime should be updated | env | as the posix/link st_ctime row | - |
