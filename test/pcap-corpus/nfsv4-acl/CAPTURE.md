# NFSv4 over Kerberos sample

`dittofs-krb5-sample.pcap` / `.skel` — a **DittoFS** NFSv4.1 `sec=krb5` session
(EXCHANGE_ID → mount → file create/chmod/mkdir/read) by the AD domain user
`alice`, captured on the wire. RPCSEC_GSS service was `none` (authentication
only), so the NFSv4 COMPOUND payload is in cleartext and fully decoded.

This is the **candidate** side. The matching **reference** (`reference.pcap`)
from a Linux `knfsd` export carrying an equivalent `fattr4_acl` get/set is not
yet captured — it needs a knfsd server with the same export (Phase 2). Once
present, diff with:

```bash
../pcap-diff.sh --reference reference.pcap --candidate dittofs-krb5-sample.pcap \
  --proto nfs --candidate-port 12049
```

## How the sample was produced

```bash
# Linux NFSv4.1 client, krb5 TGT for alice, rpc.gssd -n (use the user ccache):
tcpdump -i any -s0 -w nfsv4-krb5.pcap port 12049 &
mount -t nfs -o vers=4.1,sec=krb5,port=12049 dittofs.dittofs.ad:/export /mnt/krb
# create / chmod / mkdir / read as alice ...
tshark -r nfsv4-krb5.pcap -T pdml | normalize.py --proto nfs > dittofs-krb5-sample.skel
```

This sample also documents the live validation that DittoFS resolves a Kerberos
domain user to the RFC2307 UID/GID over the AD/LDAP idmap: files created in the
session are owned by alice's `uidNumber:gidNumber` (10001:10000), not nobody.
