# Active Directory / LDAP / Kerberos / NTLM Integration

Operator guide for joining DittoFS to an Active Directory (or compatible LDAP)
directory so that **one AD user resolves to the same Unix UID/GID over both SMB
and NFS**. This covers the two halves of AD integration:

- **LDAP idmap** — resolves a directory principal (a `user@REALM` form or an AD
  SID) to a Unix identity by reading RFC2307 `uidNumber`/`gidNumber` attributes
  (the `idmap_ad` model) or deriving them algorithmically from the RID
  (`idmap_rid`), and resolves nested group membership.
- **Kerberos** — accepts Kerberos service tickets for the server's SPN over both
  SMB (SPNEGO) and NFS (RPCSEC_GSS), from a single service keytab. NTLM remains
  available as a fallback for SMB.

> **Status.** AD/LDAP/Kerberos support is functional but the project is **not
> production ready**. Read [docs/FAQ.md](faq.md) for known limitations.

See also: [docs/CONFIGURATION.md](configuration.md) (every config key + env var),
[docs/SMB.md](smb.md), [docs/NFS.md](nfs.md), and [docs/ACLS.md](access-control.md) (the
cross-protocol ACL/SID model).

---

## Overview: cross-protocol identity unification

DittoFS resolves every authenticated principal through a single identity
resolver chain. The LDAP/AD provider is registered in that chain after Kerberos,
so a directory principal or SID with no local mapping is resolved against the
directory. Because **both** the NFS and SMB adapters consume the same resolver,
an AD user maps to the **same Unix UID/GID regardless of which protocol they
arrive on** — the property the `go test -tags=ad_dc` suite asserts (alice →
uid 10001, gid 10000 over LDAP; the same identity over an SMB Kerberos session).

The two providers play complementary roles:

| Provider | What it does | When it applies |
|---|---|---|
| **Kerberos** | Authenticates the ticket (SMB SPNEGO / NFS RPCSEC_GSS), yields a principal + PAC group SIDs | At session/RPC auth time |
| **LDAP idmap** | Maps the principal / SID to a Unix UID/GID + group GIDs | When resolving the authenticated identity to POSIX |

You can enable LDAP alone (idmap for NTLM/AUTH_UNIX-named principals), Kerberos
alone, or both together (the full AD-joined deployment).

---

## Prerequisites

- A reachable directory: a real Windows Server AD-DC, a Samba AD-DC, or any
  RFC2307-capable LDAP server. For a throwaway dev directory see **Part A**.
- LDAP idmap: a read-only **service account** (bind DN + password) and the
  directory's **base DN**. RFC2307 mode additionally requires `uidNumber` /
  `gidNumber` POSIX attributes to be stamped on the user/group objects.
- Kerberos: a **service keytab** containing the DittoFS SPN(s) and (optionally) a
  `krb5.conf`. See **Part C**.
- For verification: `kinit` (the `krb5-user` package) and `mount.cifs` /
  `smbclient` on the client.

---

## Part A: Stand up a dev Samba AD-DC

> **DEV / TEST ONLY.** The Samba AD-DC described here is an identity authority
> (Kerberos KDC on 88, LDAP on 389/636, AD DNS on 53, kpasswd on 464) used to
> exercise the LDAP-idmap and Kerberos paths against a *real* AD without a
> Windows Server license. **It is not a file server, is not a DittoFS component,
> and is not managed by the dittofs-operator.** In production you install **no**
> Samba — you point DittoFS at your existing enterprise AD. Port 445 (SMB) is
> deliberately not exposed by the fixture; DittoFS owns 445.

The provisioning logic below lives in `test/integration/ad-dc/entrypoint.sh`. It
provisions realm **`DITTOFS.AD`** / domain **`DITTOFS`** and creates:

- user `alice` — RFC2307 `uidNumber=10001`, `gidNumber=10000`, member of `devs`
  (which is nested under `engineering`).
- user `bob` — no RFC2307 attrs (the RID-fallback case), member of `engineering`.
- groups `engineering` and `devs` (with `devs` nested under `engineering`).
- a combined keytab at `/keytabs/dittofs.keytab` holding the `cifs/` and `nfs/`
  SPNs.

### What the provisioning does

The one-shot provision (run on first start) is:

```bash
samba-tool domain provision \
    --use-rfc2307 \
    --realm=DITTOFS.AD \
    --domain=DITTOFS \
    --server-role=dc \
    --dns-backend=SAMBA_INTERNAL \
    --adminpass="$ADMIN_PASSWORD"
```

`--use-rfc2307` enables the POSIX (`uidNumber`/`gidNumber`) schema so those
attributes can be stamped on objects. Users and groups are then created:

```bash
samba-tool group add engineering
samba-tool group add devs
# Nest devs under engineering so a member of devs carries BOTH group SIDs
# in their Kerberos PAC (AD resolves the nesting at the DC):
samba-tool group addmembers engineering devs

# alice: RFC2307 attrs (idmap_ad path), member of devs (=> nested in engineering)
samba-tool user create alice "$USER_PASSWORD" \
    --uid-number=10001 --gid-number=10000 \
    --unix-home=/home/alice --login-shell=/bin/bash
samba-tool group addmembers devs alice

# bob: no RFC2307 attrs (idmap_rid algorithmic-fallback path)
samba-tool user create bob "$USER_PASSWORD"
samba-tool group addmembers engineering bob
```

### The AES-keytab requirement (issue #1318)

The DittoFS SPNs are registered on the Administrator account and exported to a
combined keytab:

```bash
samba-tool spn add cifs/dittofs.dittofs.ad 'DITTOFS\Administrator'
samba-tool spn add nfs/dittofs.dittofs.ad  'DITTOFS\Administrator'
```

By default the account may carry only an **RC4 (arcfour-hmac)** Kerberos key, so
`exportkeytab` would emit an RC4-only keytab. **Windows 11 refuses RC4
(arcfour-hmac) service tickets**, so the keytab must carry AES256/AES128 keys.
Two steps are required, in order:

1. **Advertise AES** on the SPN-holding account by setting
   `msDS-SupportedEncryptionTypes` to `31` (`0x1F` = DES-CBC-CRC + DES-CBC-MD5 +
   RC4-HMAC + AES128-CTS + AES256-CTS; the legacy bits are kept so RC4/DES
   clients still work):

   ```bash
   ldbmodify -H /var/lib/samba/private/sam.ldb <<EOF
   dn: <Administrator-DN>
   changetype: modify
   replace: msDS-SupportedEncryptionTypes
   msDS-SupportedEncryptionTypes: 31
   EOF
   ```

2. **Regenerate the Kerberos keys.** AES keys derive from the account password +
   salt, so they only materialise on a password change. Reset the password to
   itself to force regeneration without changing the credential:

   ```bash
   samba-tool user setpassword Administrator --newpassword="$ADMIN_PASSWORD"
   ```

Then export both principals into one keytab (a second `exportkeytab` into the
same path **appends**):

```bash
samba-tool domain exportkeytab /keytabs/dittofs.keytab --principal=cifs/dittofs.dittofs.ad@DITTOFS.AD
samba-tool domain exportkeytab /keytabs/dittofs.keytab --principal=nfs/dittofs.dittofs.ad@DITTOFS.AD
```

Verify the keytab carries AES keys for both SPNs (the entrypoint fast-fails if
`aes256-cts` / `aes128-cts` are missing):

```bash
klist -ke /keytabs/dittofs.keytab
```

### Route 1 — Docker (the test fixture image)

The image is built from `test/integration/ad-dc/`. The provisioning realm,
domain, and passwords are controlled by environment variables (defaults shown):

| Env var | Default |
|---|---|
| `AD_REALM` | `DITTOFS.AD` |
| `AD_DOMAIN` | `DITTOFS` |
| `AD_ADMIN_PASSWORD` | `Passw0rd!2024` |
| `USER_PASSWORD` | `TestPassword01!` |
| `KEYTAB_DIR` | `/keytabs` |
| `DITTOFS_UID` / `DITTOFS_GID` | `65532` / `65532` (keytab ownership) |

The container needs to bind privileged ports (88/389/636/464/53) and set NT
ACLs, so it runs privileged. The combined keytab lands in the `KEYTAB_DIR`
volume; mount that volume (or copy the keytab out) so the `dfs` server can read
it.

### Route 2 — In-cluster (k8s/samba-ad-dc manifests)

For a Kubernetes dev cluster, apply the manifests in `k8s/samba-ad-dc/`:

- `samba-ad-dc.yaml` — a `StatefulSet` + headless `Service` exposing only the
  directory roles (88, 389, 636, 464, 53). The pod runs privileged, uses Samba's
  internal DNS (`dnsPolicy: None`, nameserver `127.0.0.1`, search `dittofs.ad`),
  and persists `/var/lib/samba` and `/keytabs` on PVCs. The image
  (`docker.io/library/dittofs-ad-dc:dev`, `imagePullPolicy: Never`) is the same
  one built from `test/integration/ad-dc/`, imported locally (e.g. via
  `k3s ctr images import`).
- `krb5-test-client.yaml` — a throwaway pod that reaches the in-cluster AD-DC and
  the DittoFS adapters, mounting the AD-generated `krb5.conf` from a Secret
  (`demo-krb5-conf`) at `/etc/krb5.conf` (`KRB5_CONFIG=/etc/krb5.conf`). Use it
  to `kinit` and then exercise SMB/NFS from inside the cluster.

Both manifests are headed **DEV / TEST only** — the same caveat as above.

---

## Part B: Configure the LDAP idmap

LDAP is configured over the control-plane API (hot-reloads the live resolver, no
restart) or seeded from the config file / environment on first boot.

### Over the API (recommended; hot-reloads)

```bash
dfsctl identity-provider set ldap --config '{
  "enabled": true,
  "url": "ldaps://dc.example.com:636",
  "base_dn": "DC=example,DC=com",
  "bind_dn": "CN=svc-dittofs,CN=Users,DC=example,DC=com",
  "bind_password": "s3cret",
  "idmap": "rfc2307",
  "nested_groups": true,
  "user_attr": "sAMAccountName"
}'
```

Validate reachability without persisting:

```bash
dfsctl identity-provider test ldap --config '{ ... same JSON ... }'
```

Inspect and list (the bind password is redacted to `********`):

```bash
dfsctl identity-provider get ldap
dfsctl identity-provider list
```

**Config field names** (the JSON shape matches the API schema, source
`pkg/apiclient/identity_providers.go` / the handler DTO in
`internal/controlplane/api/handlers/identity_providers.go`):

| JSON field | Meaning |
|---|---|
| `enabled` | turn the provider on |
| `url` | `ldaps://host:636` (preferred) or `ldap://host:389` |
| `start_tls` | upgrade an `ldap://` connection to TLS |
| `allow_plaintext` | explicit opt-in to an unencrypted bind (off) |
| `base_dn` | search base, e.g. `DC=example,DC=com` |
| `bind_dn` | service-account DN |
| `bind_password` | service-account password (write-only; send `********` or omit to keep stored) |
| `user_attr` | attribute matched against the bare username (default `sAMAccountName`) |
| `realm` | matches `user@REALM` credentials |
| `idmap` | `rfc2307` (uidNumber/gidNumber) or `rid` |
| `nested_groups` | resolve transitive AD group membership |
| `max_group_results` | cap on (nested) groups resolved per user |
| `timeout` | Go duration string, e.g. `10s` |
| `tls` | object: `ca_cert_file`, `client_cert_file`, `client_key_file`, `insecure_skip_verify`, `min_version` |

> **Encryption is on by default.** A plaintext `ldap://` bind is refused unless
> you set `start_tls: true` or explicitly set `allow_plaintext: true`. Prefer
> `ldaps://`.

> **Samba AD-DC self-signed certs.** A default Samba AD-DC serves a cert with a
> negative serial number, which Go's `crypto/x509` rejects at parse time. The
> `dfs` binary is built with `x509negativeserial=1` so `ldaps://` against a
> default Samba AD-DC works out of the box. For production, use a properly
> issued DC certificate. (See [docs/CONFIGURATION.md §15](configuration.md).)

### Config-file / environment equivalents (first-boot seed)

The `ldap.*` block seeds the database on first boot; thereafter the DB row wins
(see **Precedence** below).

```yaml
ldap:
  enabled: true
  url: ldaps://dc.example.com:636
  start_tls: false
  allow_plaintext: false
  base_dn: "DC=example,DC=com"
  bind_dn: "CN=svc-dittofs,CN=Users,DC=example,DC=com"
  bind_password: "********"
  user_attr: sAMAccountName
  realm: EXAMPLE.COM
  idmap: rfc2307
  nested_groups: true
  max_group_results: 200
  timeout: 10s
  tls:
    ca_cert_file: /etc/dittofs/ad-ca.pem
    insecure_skip_verify: false
    min_version: "1.2"
```

Each key has a `DITTOFS_LDAP_*` env equivalent (e.g. `DITTOFS_LDAP_ENABLED`,
`DITTOFS_LDAP_URL`, `DITTOFS_LDAP_BASE_DN`, `DITTOFS_LDAP_BIND_DN`,
`DITTOFS_LDAP_BIND_PASSWORD`, `DITTOFS_LDAP_IDMAP`, `DITTOFS_LDAP_NESTED_GROUPS`,
`DITTOFS_LDAP_TLS_CA_CERT_FILE`, …). The full table is in
[docs/CONFIGURATION.md §15](configuration.md).

### Precedence and secret handling

- A **persisted DB row** (set over the API) **wins** over the file/env config on
  subsequent boots.
- The file/env config **seeds the DB on first boot** only.
- **LDAP changes hot-reload** the live resolver — no restart.
- The **bind password is write-only**: `get` redacts it to `********`; submitting
  `********` (or omitting `bind_password`) on a `set`/`PUT` preserves the stored
  secret.

(Verified against `cmd/dfs/commands/start.go` `resolveIdentityProviders` and the
handler in `internal/controlplane/api/handlers/identity_providers.go`.)

---

## Part C: Configure Kerberos

Kerberos backs both NFS RPCSEC_GSS and SMB SPNEGO from a single service keytab.
**Kerberos changes are restart-required** — the NFS/SMB adapters bind the
config at startup.

### Over the API

```bash
dfsctl identity-provider set kerberos --config '{
  "enabled": true,
  "keytab_path": "/etc/dittofs/dittofs.keytab",
  "service_principal": "nfs/server.example.com@EXAMPLE.COM",
  "realm": "EXAMPLE.COM",
  "netbios_domain": "EXAMPLE",
  "dns_domain": "example.com",
  "krb5_conf": "/etc/krb5.conf"
}'
```

The `--config` argument also accepts `@/path/to/krb.json` to read from a file.
On success, `dfsctl` reminds you the change applies on the next restart. Validate
the keytab/krb5.conf without persisting:

```bash
dfsctl identity-provider test kerberos --config '{ ... }'
dfsctl identity-provider get kerberos
```

**Config field names** (source `pkg/apiclient/identity_providers.go`
`KerberosProviderConfig` / the handler `KerberosConfigDTO`):

| JSON field | Meaning |
|---|---|
| `enabled` | turn the provider on (requires `keytab_path` + `service_principal`) |
| `keytab_path` | in-server path to the service keytab |
| `service_principal` | the SPN, e.g. `nfs/server.example.com@EXAMPLE.COM` |
| `realm` | Kerberos realm; defaults to the `@REALM` of `service_principal` |
| `netbios_domain` | short NetBIOS name; single label (no `.`/`@`/`/`/spaces) |
| `dns_domain` | DNS domain; defaults to the lowercased realm |
| `krb5_conf` | path to `krb5.conf` |
| `max_clock_skew` | Go duration string (e.g. `5m`) |
| `context_ttl` | Go duration string (e.g. `8h`) |
| `max_contexts` | integer (`>= 0`) |

### Config-file / environment equivalents

`kerberos` is a **top-level** config block (not nested under `adapters`):

```yaml
kerberos:
  enabled: true
  keytab_path: /etc/dittofs/dittofs.keytab
  service_principal: nfs/server.example.com@EXAMPLE.COM
  krb5_conf: /etc/krb5.conf
  realm: EXAMPLE.COM
  netbios_domain: EXAMPLE
  dns_domain: example.com
```

Env equivalents include `DITTOFS_KERBEROS_REALM`, `DITTOFS_KERBEROS_NETBIOS_DOMAIN`,
and `DITTOFS_KERBEROS_DNS_DOMAIN` (see [docs/CONFIGURATION.md §12](configuration.md)).

### One keytab, both protocols

A keytab can hold multiple service principals, so a single file serves SMB and
NFS. Export a keytab containing **both** the `cifs/` (SMB) and `nfs/` (NFS) SPNs:

```bash
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=cifs/server.example.com@EXAMPLE.COM
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=nfs/server.example.com@EXAMPLE.COM
```

Point `kerberos.keytab_path` at the combined keytab. The SMB handler selects the
`cifs/` principal; NFS RPCSEC_GSS uses the `nfs/` principal.

### Domain-aware SMB

When `netbios_domain` is set, the SMB server advertises the AD domain in the NTLM
challenge (`MsvAvNbDomainName` / `MsvAvDnsDomainName`) so domain users
authenticate against the correct domain. Unset → it advertises `WORKGROUP` /
`local` (standalone behavior). See [docs/SMB.md](smb.md).

### Precedence (same model as LDAP)

A persisted Kerberos DB row wins over file/env on subsequent boots; file/env
seeds the DB on first boot. Unlike LDAP, Kerberos does **not** hot-reload —
a change applies on the next `dfs` restart.

---

## Part D: Kubernetes operator (DittoServer CR)

The dittofs-operator renders the `ldap:` and `kerberos:` config blocks and mounts
the keytab/krb5.conf Secrets into the pod. Configure them under
`spec.identity`. (Source: `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go`,
`k8s/dittofs-operator/docs/CRD_REFERENCE.md`.)

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs
spec:
  identity:
    ldap:
      enabled: true
      url: ldaps://dc.example.com:636
      baseDN: "DC=example,DC=com"
      bindDN: "CN=svc-dittofs,CN=Users,DC=example,DC=com"
      bindPasswordSecretRef:        # injected as DITTOFS_LDAP_BIND_PASSWORD; never in the ConfigMap
        name: dittofs-ldap-secret
        key: bind-password
      idmap: rfc2307                 # rfc2307 | rid
      nestedGroups: true
      userAttr: sAMAccountName
      # caCertFile: /path/in/pod     # mount your own Secret/ConfigMap; operator does not manage this volume
    kerberos:
      enabled: true
      servicePrincipal: nfs/server.example.com@EXAMPLE.COM
      realm: EXAMPLE.COM
      netbiosDomain: EXAMPLE         # set explicitly for domain-aware SMB; empty => WORKGROUP
      dnsDomain: example.com
      keytabSecretRef:               # mounted read-only at /kerberos/dittofs.keytab
        name: dittofs-keytab
        key: dittofs.keytab
      krb5ConfSecretRef:             # optional; mounted at /kerberos-krb5/krb5.conf
        name: dittofs-krb5-conf
        key: krb5.conf
```

Operator behavior to know:

- The **keytab is supplied via a Secret** (`keytabSecretRef`), mounted **read-only**
  at `/kerberos/dittofs.keytab`; the operator points `kerberos.keytab_path` at the
  mount. **Never put a keytab in a ConfigMap** — it is secret key material.
  Rotating the Secret rolls the pod so the new keys take effect.
- The optional `krb5ConfSecretRef` is mounted at `/kerberos-krb5/krb5.conf` and
  wins over `krb5Conf` (an in-pod path). When neither is set, the server default
  `/etc/krb5.conf` (typically baked into the image) is used.
- The LDAP **bind password is injected as `DITTOFS_LDAP_BIND_PASSWORD`** from
  `bindPasswordSecretRef` and is never written to the ConfigMap.
- `kerberos.enabled: false` (or omitting the block) renders no `kerberos:` block —
  the server stays AUTH_UNIX/standalone-SMB only.

---

## Part E: Verify

The proven end-to-end fixture is the `ad_dc` integration suite, which provisions
the realm + alice/bob + nested groups and asserts identity unification:

```bash
go test -tags=ad_dc -v -timeout 20m ./test/integration/ad-dc/
```

It verifies, among other things:

- **RFC2307 idmap:** `alice@DITTOFS.AD` resolves to `uid 10001`, `gid 10000`
  (the `uidNumber`/`gidNumber` stamped by the fixture).
- **Nested groups:** alice ∈ `devs`, and `devs` ⊂ `engineering`, so both groups'
  GIDs resolve for alice.
- **RID fallback:** bob has no RFC2307 attrs (the `idmap_rid` path).
- **Kerberos accept:** alice's `cifs/` service ticket is accepted via SMB SPNEGO
  using the exported combined keytab.

### Manual verification (Linux client)

Obtain a ticket, then exercise both protocols. The Kerberos client config must
point at the AD realm/KDC (the AD-DC's generated `krb5.conf`, e.g. via
`KRB5_CONFIG`).

The exact mount forms below are the ones the e2e suites use (`test/e2e/`).
`KRB5_CONFIG` / `KRB5CCNAME` must point at the AD realm config and the ticket
cache. Substitute your server host, share name, and ports (SMB defaults to 445;
the DittoFS NFS default is 12049, not 2049).

```bash
# 1. Get a TGT for the domain user
kinit alice@DITTOFS.AD
klist                       # should show a TGT for DITTOFS.AD

# 2. Mount over SMB with Kerberos
mount -t cifs //server/share /mnt/smb -o sec=krb5,port=12445,vers=2.1,cache=none

# 3. Mount over NFSv4 with Kerberos.
#    Use vers=4.0 explicitly (the e2e suite mounts with vers=4.0, NOT vers=4):
mount -t nfs //server/share /mnt/nfs -o vers=4.0,port=12049,sec=krb5,actimeo=0
```

For an ad-hoc SMB check once a ticket is present, `smbclient` can force the
Kerberos path:

```bash
smbclient //server/share -I <server-ip> --use-kerberos=required
```

Confirm that files created over one protocol show the **same owner UID/GID**
when read over the other — that is the cross-protocol identity unification goal.
Expected: alice resolves to `uid 10001` on both.

### NFS `sec=krb5` from a Linux client — verified procedure

An NFSv4.0 `sec=krb5` mount has client-side prerequisites beyond `kinit` that are
easy to miss. The following sequence is **verified end-to-end** against the demo
(an external Linux host mounting the DittoFS NFS adapter as the AD user `alice`,
files resolving to `uid 10001 / gid 10000`):

1. **Kernel modules** (the client node, not a container): `rpcsec_gss_krb5` and
   `auth_rpcgss` must load (`modprobe rpcsec_gss_krb5`). A pod whose node lacks
   them — and which cannot `modprobe` — cannot do `sec=krb5` even though the
   server is correct.
2. **`krb5.conf`** mapping the realm to the KDC (use the AD-DC's generated
   `krb5.conf`, or point `kdc =` straight at the DC):
   ```ini
   [libdefaults]
     default_realm = DITTOFS.AD
   [realms]
   DITTOFS.AD = { kdc = <dc-host-or-ip> }
   [domain_realm]
     .dittofs.ad = DITTOFS.AD
   ```
3. **The server SPN must resolve to the NFS endpoint.** The client requests a
   ticket for `nfs/<the-name-you-mount>@REALM`; that name must match the keytab
   SPN (`nfs/dittofs.dittofs.ad`). Add a hosts entry if needed:
   `echo "<nfs-endpoint-ip> dittofs.dittofs.ad" >> /etc/hosts`, then mount
   `dittofs.dittofs.ad:/<share>`.
4. **A machine credential at `/etc/krb5.keytab`.** NFSv4 establishes its client
   lease with a *machine* principal, so `rpc.gssd` needs a host keytab — and the
   stock `rpc-gssd.service` is **gated on `/etc/krb5.keytab` existing**
   (`ConditionPathExists`), so without it the service silently never starts.
   Create a machine account in AD and export its `host/` key:
   ```bash
   samba-tool computer create <client-shortname>
   samba-tool domain exportkeytab /etc/krb5.keytab \
       --principal=host/<client-shortname>@DITTOFS.AD
   ```
5. **A running `rpc.gssd`.** Start it cleanly (`systemctl restart rpc-gssd`, now
   that the keytab exists). If `rpc.gssd` is not actually serving the upcall, the
   client silently falls back to **AUTH_SYS `uid=0`**, which a root-squashing
   export refuses — surfacing as the misleading `mount.nfs4: access denied by
   server`. If you see that, check `pgrep -a rpc.gssd` and the daemon's `-vvv`
   log for `do_downcall ... acceptor=nfs@<server>` (success).
6. **`kinit` + mount:**
   ```bash
   kinit alice@DITTOFS.AD
   mount -t nfs4 -o vers=4.0,sec=krb5,port=12049 dittofs.dittofs.ad:/<share> /mnt/nfs
   ls -lan /mnt/nfs          # alice's files show uid 10001 / gid 10000
   ```

Server-side confirmation (`dfs` debug log): `NFSv4 using GSS identity uid=10001
gid=10000 principal=alice realm=DITTOFS.AD`.

### NTLM fallback (SMB)

When a client cannot use Kerberos, SMB falls back to NTLM. With
`netbios_domain` set, the server advertises the AD domain in the NTLM challenge
so domain users authenticate against the correct domain (see Part C / docs/SMB.md).

## Part F: Windows client — domain-join (Kerberos) [dev]

A real Windows client gets the cleanest experience by **joining the domain** and
authenticating to DittoFS over **Kerberos** — the server side is fully proven
(SMB SPNEGO/PAC + AES keytab, Part C). A non-domain-joined Windows box would need
NTLM passthrough (NETLOGON), which is a separate track (see #1314 / #1345). For
acceptance testing, domain-join is the path with no extra server dependency.

The in-cluster AD-DC is not publicly reachable by default. To let an external
test VM join, expose the directory roles via the **scoped** dev LoadBalancer
`k8s/samba-ad-dc/samba-ad-dc-lb.yaml` (Kerberos 88, kpasswd 464, LDAP 389/636,
DNS 53):

The manifest ships with a closed TEST-NET placeholder in
`loadBalancerSourceRanges`, so applying it never exposes the AD-DC to the
internet — the LB routes to nobody until you patch in the client `/32`:

```bash
# 1. Apply (still closed — placeholder range routes nowhere), then open ONLY
#    to the Windows VM's public IP.
kubectl apply -f k8s/samba-ad-dc/samba-ad-dc-lb.yaml
kubectl -n dittofs patch svc samba-ad-lb --type=merge \
  -p '{"spec":{"loadBalancerSourceRanges":["<WINDOWS_VM_IP>/32"]}}'
kubectl -n dittofs get svc samba-ad-lb -o wide      # confirm range + note EXTERNAL-IP
```

On the Windows VM (PowerShell, Administrator):

```powershell
# Point DNS at the AD-DC LB so DITTOFS.AD SRV records resolve.
# Discover your NIC first — the alias is not always "Ethernet".
$adlb = "<samba-ad-lb EXTERNAL-IP>"
$nic  = (Get-NetAdapter -Physical | Where-Object Status -eq 'Up')[0].ifIndex
Set-DnsClientServerAddress -InterfaceIndex $nic -ServerAddresses $adlb

# Join the realm and reboot.
$cred = Get-Credential DITTOFS\Administrator        # Passw0rd!2024 in the dev fixture
Add-Computer -DomainName dittofs.ad -Credential $cred -Restart
```

After reboot, log in as `DITTOFS\alice` (dev password `TestPassword01!`). Map the
DittoFS share and open a file's **Properties > Security** tab. **Prerequisite:**
the LDAP/AD idmap resolver must be configured (Part B) — with it, owner/ACE SIDs
resolve to `DITTOFS\<name>` via LSARPC (LsarLookupSids2/3; #1291 + #1341). If the
directory resolver is unconfigured or unreachable, the server falls back to raw
`S-1-5-21-…` SIDs (or `unix_user:*` / `unix_group:*`), so confirm the idmap is up
before reading the GUI as an acceptance signal:

```powershell
net use \\<dittofs-smb-ip>\<share>           # Kerberos SSO as the logged-in domain user
icacls \\<dittofs-smb-ip>\<share>\<file>     # CLI equivalent of the Security tab
```

> **Tear down after capture:** `kubectl -n dittofs delete svc samba-ad-lb` — the
> LB publishes a KDC + LDAP directory and must not be left exposed. The AD-DC is
> dev-only (see Known limitations).

---

## Known limitations

- **AD-DC is dev-only.** The Samba AD-DC in `test/integration/ad-dc/` and
  `k8s/samba-ad-dc/` is an identity authority for testing — not a file server and
  not operator-managed. In production, point DittoFS at your existing enterprise
  AD; install no Samba.
- **NFS krb5 over the wire needs kernel support.** A Linux NFS client mounting
  with `sec=krb5` requires the node kernel modules `rpcsec_gss_krb5` /
  `auth_rpcgss`. An in-cluster k3s node that lacks these modules cannot complete
  an `sec=krb5` NFS mount even though the server side is correct.
- **SID → name resolution** is implemented over the LSARPC pipe: `LsarOpenPolicy`
  (opnum 6/44), `LsarLookupSids` (15), and `LsarLookupSids2/3` (57/76, the EX
  forms Windows Explorer/`rpcclient` use) translate machine-domain and AD
  foreign-domain SIDs to `DITTOFS\<name>` (#1291, #1341, #1342). This depends on
  the directory-backed idmap resolver (Part B) being configured and reachable;
  when it is not, the server falls back to raw `S-1-5-21-…` SIDs (or
  `unix_user:*` / `unix_group:*`). See [docs/ACLS.md](access-control.md).
- **RC4-only keytabs are rejected by Windows 11 (#1318).** Ensure the keytab
  carries AES256/AES128 keys for the SPNs (Part A).
- Online `net ads join` + machine-password rotation is out of scope; supply the
  keytab offline.
