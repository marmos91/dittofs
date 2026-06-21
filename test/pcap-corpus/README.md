# AD interop pcap corpus (#1237)

Reference packet captures from **official** protocol endpoints, plus a
structural diff harness, so each AD feature is validated against real-server
wire behavior — not against our own assumptions. This is the correctness oracle
referenced by AD-1 (Kerberos/PAC), AD-2 (LDAP), and AD-4 (security descriptors,
NFSv4 ACLs). It extends the `docs/DEBUGGING.md` pcap-diff playbook (the method
that root-caused the macOS NFSv4.1 kernel panic).

## Why structural, not byte-level

Kerberos/SPNEGO/PAC, LDAP, security-descriptors-on-wire, and NFSv4 ACLs are
interop-hell. Two correct implementations differ in plenty of bytes (timestamps,
nonces, session IDs, encrypted blobs, addresses) yet must agree on *structure* —
which fields, options, and flags appear, and how they nest. `pcap-diff.sh`
reduces each capture to a canonical **skeleton** (`normalize.py`: field shape
with volatile values stripped) and diffs those. A non-empty diff is a structural
divergence — the interop-bug class that "it compiles and a client connects"
hides.

## Layout

```
pcap-corpus/
  pcap-diff.sh        # structural diff harness (tshark + normalize.py + diff)
  normalize.py        # tshark PDML -> canonical structural skeleton
  kerberos/           # AS-REQ/AP-REQ / SPNEGO / PAC reference
  ldap/               # RFC2307 query reference
  nfsv4-acl/          # fattr4_acl get/set reference
  smb-sd/             # SMB2 SECURITY_DESCRIPTOR query/set reference
```

Each feature dir holds `reference.pcap` (from the official server), a
`reference.skel` snapshot (the normalized skeleton, human-diffable in review),
and a `CAPTURE.md` recording exactly how it was produced (endpoint, client
command, tshark filter) so it is reproducible.

## Usage

```bash
# Compare a fresh DittoFS capture against the stored reference.
# DittoFS uses non-standard ports, so pass --candidate-port for SMB/NFS.
test/pcap-corpus/pcap-diff.sh \
  --reference test/pcap-corpus/kerberos/reference.pcap \
  --candidate /tmp/dittofs-krb.pcap \
  --proto kerberos --proto spnego
```

Exit `0` = structurally identical, `1` = divergence (unified diff printed).

## Regenerating the reference captures

All endpoints come from the existing AD infra: the dockerized Samba AD-DC
(`test/integration/ad-dc/`, realm `DITTOFS.AD`, users `alice`/`bob`) for
Kerberos + LDAP, a Linux `knfsd` export for NFSv4 ACLs, and a Samba member
server (or Windows) for SMB security descriptors. Capture with `tcpdump -i any
-s 0 -w ref.pcap`, then run the same operation against DittoFS and diff.

| Feature | Reference endpoint | Capture recipe | tshark `--proto` |
|---------|--------------------|----------------|------------------|
| Kerberos / SPNEGO / PAC | `kinit` / SMB session-setup ↔ Samba AD-DC | `tcpdump port 88 or port 445` while `kinit alice` + an SMB mount | `kerberos`, `spnego` |
| LDAP RFC2307 | `ldapsearch` ↔ AD-DC | `tcpdump port 389` while `ldapsearch -b … '(objectClass=posixAccount)'` | `ldap` |
| NFSv4 `fattr4_acl` | Linux client ↔ `knfsd` | `tcpdump port 2049` while `nfs4_getfacl` / `nfs4_setfacl` on a krb5 mount | `nfs` |
| SMB SECURITY_DESCRIPTOR | `smbcacls` / Windows `icacls` ↔ Samba member / Windows share | `tcpdump port 445` while `smbcacls //srv/share file` | `smb2` |

See each feature's `CAPTURE.md` for the exact commands used.
