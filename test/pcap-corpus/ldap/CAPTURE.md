# LDAP RFC2307 idmap query reference

`reference.pcap` / `reference.skel` — a real **Samba AD-DC** (realm `DITTOFS.AD`)
answering the exact RFC2307 identity-map lookup DittoFS performs: search for a
user by `sAMAccountName` and read back `uidNumber` / `gidNumber` / `objectSid`.
This is the AD-2 (LDAP) correctness oracle.

The AD-DC mandates an encrypted bind — a cleartext bind on `:389` is refused with
`strongAuthRequired (8)`. The reference is therefore captured over **LDAPS**
(`:636`) and decrypted with the client's TLS key log, so the inner LDAP PDUs are
fully decoded in the skeleton (TLS 1.3, GnuTLS via OpenLDAP honours
`SSLKEYLOGFILE`).

## What the skeleton proves

The `searchResEntry` carries a `PartialAttributeList` whose `AttributeValue`s
include the POSIX idmap attributes and the binary `objectSid` (decoded by tshark
into `nt.sid.domain` / `nt.sid.auth`). That is the precise shape DittoFS must
parse to map an AD principal → `uidNumber:gidNumber` and to reverse-resolve a
machine-domain SID. Live values from this capture:
`dn: CN=alice,CN=Users,DC=dittofs,DC=ad`, `uidNumber: 10001`, `gidNumber: 10000`.

## How it was produced

```bash
# AD-DC LDAPS endpoint (in-cluster Samba AD-DC pod), realm DITTOFS.AD
AD=10.42.0.22
tcpdump -i any -w reference.pcap "host $AD and (port 636 or port 389)" &

# OpenLDAP client; GnuTLS writes TLS secrets to $SSLKEYLOGFILE
export SSLKEYLOGFILE=/tmp/lk.log
LDAPTLS_REQCERT=never ldapsearch -x -H ldaps://$AD:636 \
  -D "Administrator@dittofs.ad" -w '****' \
  -b "DC=dittofs,DC=ad" "(sAMAccountName=alice)" \
  uidNumber gidNumber sAMAccountName objectSid

# decrypt + normalize
tshark -r reference.pcap -o tls.keylog_file:/tmp/lk.log -T pdml \
  | ../normalize.py --proto ldap > reference.skel
```

The TLS key log is **not** committed (it is per-session secret material and only
needed to regenerate `reference.skel` from `reference.pcap`). To re-decrypt,
re-capture with a fresh key log as above.

## Diffing a DittoFS LDAP client capture

```bash
../pcap-diff.sh --reference reference.pcap --candidate /tmp/dittofs-ldap.pcap \
  --proto ldap
```

A non-empty diff means DittoFS's RFC2307 query/parse diverges structurally from
what the AD-DC expects/returns.
