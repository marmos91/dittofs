# Kerberos reference capture

`reference.pcap` / `reference.skel` — a real Kerberos AS/TGS/AP exchange against
a **Samba Active Directory DC** (the official reference KDC), realm
`DITTOFS.AD`, user `alice`.

## Endpoint

Samba AD-DC from `test/integration/ad-dc/` (realm `DITTOFS.AD`, users
`alice`/`bob`), KDC on port 88.

## How it was produced

```bash
# Against the running AD-DC (KDC on :88):
tcpdump -i any -s0 -w kerberos.pcap port 88 &
echo "$ALICE_PW" | kinit alice@DITTOFS.AD        # AS-REQ / AS-REP
kvno nfs/dittofs.dittofs.ad@DITTOFS.AD           # TGS-REQ / TGS-REP for the DittoFS NFS SPN

tshark -r kerberos.pcap -T pdml | normalize.py --proto kerberos > reference.skel
```

## What it covers

The skeleton carries the full message set DittoFS's RPCSEC_GSS / SPNEGO path
must interoperate with: `krb-as-req` (10), `krb-as-rep` (11), `krb-tgs-req`
(12), `krb-tgs-rep` (13), `krb-ap-req` (14), and `krb-error` (30) — including
the pre-auth `PA-ETYPE-INFO2` padata and PA-DATA negotiation. Encrypted parts,
nonces, timestamps, and ticket bytes are stripped by `normalize.py`; what
remains is the wire *structure* a DittoFS-issued or DittoFS-consumed Kerberos
exchange is diffed against.

## Diffing

```bash
../pcap-diff.sh --reference reference.pcap --candidate <dittofs-krb>.pcap --proto kerberos
```
