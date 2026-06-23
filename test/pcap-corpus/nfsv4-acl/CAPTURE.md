# NFSv4 ACL (`fattr4_acl`) reference + DittoFS sample

Two captures live here:

- `reference.pcap` / `reference.skel` — the **reference**: a Linux **knfsd**
  export answering an NFSv4.0 `fattr4_acl` GETATTR/SETATTR exchange
  (`nfs4_getfacl` / `nfs4_setfacl`). This is the AD-4 (NFSv4 ACL) oracle.
- `dittofs-krb5-sample.pcap` / `.skel` — the **candidate**: a DittoFS NFSv4.1
  `sec=krb5` session by the AD domain user `alice` (EXCHANGE_ID → mount →
  create/chmod/mkdir/read). RPCSEC_GSS service was `none` (auth only) so the
  COMPOUND payload is cleartext and fully decoded.

## Diffing candidate vs reference

```bash
../pcap-diff.sh --reference reference.pcap --candidate dittofs-krb5-sample.pcap \
  --proto nfs --candidate-port 12049
```

Note the captures exercise different op sets (the reference is a focused
`getfacl`/`setfacl`; the sample is a full mount+create flow), so a raw diff
reports the union. Filter to the ACL ops (`nfs.opcode` GETATTR/SETATTR carrying
`nfs.ace.*`) when comparing the ACL encoding specifically — those are the lines
that must match.

## How the reference was produced

```bash
# Linux knfsd export, sec=sys for a focused fattr4_acl exchange
echo "/srv/k 127.0.0.1(rw,sync,fsid=0,no_subtree_check,no_root_squash,sec=sys)" >> /etc/exports
exportfs -ra
tcpdump -i lo -w reference.pcap port 2049 &
mount -t nfs4 -o vers=4.0,sec=sys 127.0.0.1:/ /mnt/k
nfs4_getfacl /mnt/k/ref.txt
nfs4_setfacl -a "A::OWNER@:rwatTnNcCy" /mnt/k/ref.txt
nfs4_getfacl /mnt/k/ref.txt
tshark -r reference.pcap -T pdml | ../normalize.py --proto nfs > reference.skel
```

## How the sample was produced

```bash
# Linux NFSv4.1 client, krb5 TGT for alice, rpc.gssd -n (use the user ccache):
tcpdump -i any -s0 -w nfsv4-krb5.pcap port 12049 &
mount -t nfs -o vers=4.1,sec=krb5,port=12049 dittofs.dittofs.ad:/export /mnt/krb
# create / chmod / mkdir / read as alice ...
tshark -r nfsv4-krb5.pcap -T pdml | ../normalize.py --proto nfs > dittofs-krb5-sample.skel
```

This sample also documents the live validation that DittoFS resolves a Kerberos
domain user to the RFC2307 UID/GID over the AD/LDAP idmap: files created in the
session are owned by alice's `uidNumber:gidNumber` (10001:10000), not nobody.
