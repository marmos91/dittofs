# Windows Active Directory Setup Runbook

End-to-end guide for a Windows administrator to join **DittoFS** to an existing
**Windows Server Active Directory** domain and give domain users SMB (and NFS)
access to a share — **without creating any local DittoFS user for them**
(issue #1528). Domain users authenticate with their own Kerberos credentials and
are authorized directly by their AD user/group **SID**.

This is the production Windows-Server companion to
[identity.md](identity.md) (which uses a Samba AD-DC dev fixture). The commands
are shown two ways: the **`dfsctl`** CLI (authoritative) and the **DittoFS Pro
dashboard** UI where applicable; Windows-side steps use the native tools
(`ktpass`, ADUC/ADSI Edit, Group Policy Management Console).

> **Status.** AD/LDAP/Kerberos support is functional but the project is **not
> production ready**. See [faq.md](faq.md).

The worked example uses realm **`CUBBIT.LOCAL`** / NetBIOS **`CUBBIT`**, a
member server **`vm2.cubbit.local`**, a service account **`svc-dittofs`**, and
domain users **alice** / **bob** in an AD group **Cubbit**. Substitute your own.

---

## 0. Accounts: what you create and what you don't

There are **two kinds of account**, and #1528 only removes one of them:

| | Control-plane (management) | Data-plane (mounting) |
|---|---|---|
| Who | The **one native DittoFS admin** | AD users (alice, bob, Administrator) |
| Auth | Local password → `dfsctl login` / dashboard | Kerberos ticket from the domain |
| Created how | Auto-provisioned at first server start (`DITTOFS_ADMIN_INITIAL_PASSWORD`) | **Never created in DittoFS** |
| Used for | Configure LDAP + create grants (once) | Mount shares |

So the workflow is: **sign in once as the native admin to wire up LDAP and
grants; from then on, domain users just mount.** You do **not** create AD-backed
admin logins, and you do **not** create a DittoFS user for each AD user.

---

## 1. Create the service account, SPN, and AES keytab (in AD)

DittoFS authenticates SMB/NFS clients as a Kerberos service. It needs a domain
**service account** that holds the service principal name (SPN) and an exported
**keytab**.

1. **Create the service account** (Active Directory Users and Computers, or
   PowerShell). Give it a strong password; it needs no special privileges.
   ```powershell
   New-ADUser -Name svc-dittofs -SamAccountName svc-dittofs `
     -AccountPassword (Read-Host -AsSecureString "svc-dittofs password") `
     -Enabled $true -PasswordNeverExpires $true
   ```

2. **Map the SPN and export an AES keytab** with `ktpass` (run on a DC or any
   RSAT-equipped admin box). This binds `cifs/<host>` (SMB) to the account and
   writes the keytab. **Use AES** — Windows 11 rejects RC4-only service tickets
   (#1318):
   ```cmd
   ktpass -princ cifs/vm2.cubbit.local@CUBBIT.LOCAL ^
     -mapuser CUBBIT\svc-dittofs -pass * ^
     -crypto AES256-SHA1 -ptype KRB5_NT_PRINCIPAL ^
     -out C:\dittofs\dittofs.keytab
   ```
   For NFS as well, repeat with `-princ nfs/vm2.cubbit.local@CUBBIT.LOCAL` and
   `-in`/`-out` the same file to append (or export a second keytab and merge).

3. **Advertise AES on the account** so the KDC issues AES tickets. In ADUC enable
   *"This account supports Kerberos AES 256 bit encryption"* (Account tab), or
   set `msDS-SupportedEncryptionTypes` via ADSI Edit / Attribute Editor. `ktpass`
   usually sets this; verify it.

4. Copy `dittofs.keytab` to the DittoFS host (e.g. `C:\ProgramData\dittofs\`).
   Treat it as a **secret** — it is the service's Kerberos key. Restrict its ACL
   to the DittoFS service account.

> If SMB port 445 on the host is held by the Windows *Server* service, stop and
> disable `LanmanServer` so DittoFS can bind 445 (or run DittoFS on a host that
> is not itself an SMB server).

---

## 2. Kerberos config and start the server

Provide a `krb5.conf` pointing at the realm/KDC (e.g. `C:\etc\krb5.conf`):

```ini
[libdefaults]
  default_realm = CUBBIT.LOCAL
[realms]
  CUBBIT.LOCAL = { kdc = vm1.cubbit.local }
[domain_realm]
  .cubbit.local = CUBBIT.LOCAL
  cubbit.local  = CUBBIT.LOCAL
```

Add the `kerberos:` block to the DittoFS config (or set it via
`dfsctl identity-provider set kerberos …` after start — note Kerberos changes are
restart-required):

```yaml
kerberos:
  enabled: true
  keytab_path: "C:/ProgramData/dittofs/dittofs.keytab"
  service_principal: "cifs/vm2.cubbit.local@CUBBIT.LOCAL"
  krb5_conf: "C:/etc/krb5.conf"
  realm: "CUBBIT.LOCAL"
  netbios_domain: "CUBBIT"
  dns_domain: "cubbit.local"
```

Start the server (the native admin password is provisioned on first start):

```powershell
$env:DITTOFS_ADMIN_INITIAL_PASSWORD = "<choose-a-strong-admin-password>"
dfs start --config C:\dittofs\config.yaml
```

Sign in with the native admin — this is the one management account from step 0:

```powershell
dfsctl login --server https://127.0.0.1:8080 --username admin
```

---

## 3. Connect the LDAP directory (read-only)

DittoFS reads the directory to (a) resolve an AD **name → SID** when you type a
grant, and (b) resolve a domain user's UID/GID at login. Use a **read-only**
service account (the same `svc-dittofs` is fine — it only needs to read
directory objects) and **LDAPS**.

Use `idmap: rid` (Samba `idmap_rid`-style): UIDs/GIDs are derived
deterministically from the SID's RID, so you do **not** need to stamp
`uidNumber`/`gidNumber` POSIX attributes on your AD objects.

**CLI** (LDAP is hot-reloaded — no restart):

```powershell
dfsctl identity-provider test ldap --config '{ ...json below... }'   # dry-run first
dfsctl identity-provider set  ldap --config '{
  "enabled": true,
  "url": "ldaps://vm1.cubbit.local:636",
  "base_dn": "DC=cubbit,DC=local",
  "bind_dn": "CN=svc-dittofs,CN=Users,DC=cubbit,DC=local",
  "bind_password": "<svc-dittofs password>",
  "idmap": "rid",
  "realm": "CUBBIT.LOCAL",
  "nested_groups": true
}'
dfsctl identity-provider list        # confirm LDAP shows configured + enabled
```

**Pro dashboard:** go to **Identity Providers → LDAP**, fill in the URL, base DN,
bind DN/password, and set idmap to `rid`; click **Test**, then **Save**.

**Security notes:**
- Use `ldaps://` (or `start_tls: true`) — a plaintext bind is refused unless you
  explicitly opt in. Set `tls.ca_cert_file` to validate the DC certificate.
- The bind account needs **read only**. Never use a domain-admin bind.
- The bind password is write-only over the API (redacted on read).

See [identity.md §Part B](identity.md) for every LDAP config field.

---

## 4. Grant AD users and groups directly (no local users)

Now grant share access straight to AD principals. `--user` / `--group` accept a
**local name, an AD name (`user@REALM` or `DOMAIN\group`), or a raw SID**. A bare
name resolves to a local object if one exists, otherwise to the directory; the
resolved **SID** is stored. No DittoFS user/group object is created.

Grant **distinct levels per principal** — this is what makes the same share
behave differently for each person:

```powershell
# AD user alice → read-only
dfsctl share permission grant ditto --user alice@cubbit.local --level read

# AD user bob → read-write
dfsctl share permission grant ditto --user bob@cubbit.local --level read-write

# AD group (everyone in Cubbit) → read-write, resolved to the group SID via LDAP
dfsctl share permission grant ditto --group "CUBBIT\Cubbit" --level read-write

# Domain Admins → admin (full control)
dfsctl share permission grant ditto --group "Domain Admins" --level admin

# By raw SID (no directory lookup needed)
dfsctl share permission grant ditto --sid S-1-5-21-1111-2222-3333-1104 --level read

dfsctl share permission list ditto     # shows each grant + resolved SID
```

**Pro dashboard:** **Access Control → Shares → <share> → Grant**, type the AD
user/group (or SID), pick a level, save.

Levels, lowest to highest: `none < read < read-write < admin`. Grants are
**additive** — a user gets the highest level of any grant that matches their own
SID or one of their group SIDs.

Revoke symmetrically: `dfsctl share permission revoke ditto --group "CUBBIT\Cubbit"`
(or `--sid …`).

---

## 5. Per-user "mount on login" (the target experience)

The goal: on a shared workstation, **each user who logs in gets the share mounted
with their own permissions.** This works because each interactive Windows logon
holds its own Kerberos ticket, so mounting `\\vm2.cubbit.local\ditto` under that
logon presents *that user's* identity; DittoFS reads their PAC user + group SIDs
and applies the grants keyed to those SIDs. No configuration ties a mount to a
specific person — DittoFS simply authorizes whoever connects.

- **alice logs in →** the share mounts **read-only** (her grant).
- **bob logs in on the same box →** his own logon, his own ticket → **read-write**.
- **Administrator logs in →** matched via *Domain Admins* → **admin**.

Mount on login is a **client-side** Windows step (nothing DittoFS-specific):

**Option A — Group Policy Drive Maps (recommended).** In the Group Policy
Management Console, edit a GPO applied to the workstations:
*User Configuration → Preferences → Windows Settings → Drive Maps → New → Mapped
Drive.* Set Location `\\vm2.cubbit.local\ditto`, Action *Update*, a drive letter,
and **Reconnect**. Use **item-level targeting** if you want different drives for
different groups. Because it is under *User Configuration*, the drive maps under
each user's own logon and ticket.

**Option B — logon script.** A per-user logon script running:
```cmd
net use Z: \\vm2.cubbit.local\ditto /persistent:yes
```
No credentials are passed — Windows uses the logged-in user's Kerberos ticket
(single sign-on).

**Verify the resolved identity** in Explorer: right-click a file →
*Properties → Security*. Owner/ACE SIDs resolve to `CUBBIT\<name>` when the LDAP
directory is configured (step 3); without it they show as raw `S-1-5-21-…`.

---

## 6. NTLM pass-through: Explorer double-click for AD users (optional)

Everything above authenticates over **Kerberos**, which needs a service ticket
for the SPN — so it works when you mount by the FQDN (`\\vm2.cubbit.local\ditto`).
But when a user **double-clicks the server in Explorer → Network** (LAN
discovery), or connects by **IP**, Windows connects by a name with **no SPN** and
falls back to **NTLM** — which the KDC never sees. Without extra setup, an AD
domain user fails that path with `STATUS_LOGON_FAILURE` (only local DittoFS users
with a stored password work over NTLM).

To make double-click work for **domain** users, enable **NETLOGON pass-through**:
DittoFS forwards the client's NTLM response to a DC (`NetrLogonSamLogon`) over a
secure channel, gets back the user + group SIDs, and authorizes them with the
**same SID grants** from step 4 (so `Domain Admins` etc. apply unchanged).

This needs a dedicated **machine (computer) account** — *not* the `svc-dittofs`
service account, because NETLOGON requires a workstation-trust secure channel
that only a machine account can establish.

**Step 1 — create the machine account (offline).** On a DC, elevated PowerShell:

```powershell
New-ADComputer -Name DITTOFS `
  -AccountPassword (ConvertTo-SecureString 'M4chine!Pass2026' -AsPlainText -Force) `
  -Enabled $true `
  -OtherAttributes @{'msDS-SupportedEncryptionTypes'=31}   # 31 = advertise AES
```

Don't set a `dNSHostName`/SPN on it — the host already owns `HOST/vm2…` via its
own computer account, and this account is only a NETLOGON client identity.
(Alternatively, skip this step and let DittoFS create + rotate the account itself
with `online_join` — see [configuration.md](configuration.md#12-kerberos-configuration).)

**Step 2 — add the machine account to the server config** (under `kerberos:`):

```yaml
kerberos:
  enabled: true
  realm: "CUBBIT.LOCAL"
  netbios_domain: "CUBBIT"
  # ... keytab_path etc. as in step 2 ...
  machine_account:
    enabled: true
    account_name: "DITTOFS$"
    secret: "M4chine!Pass2026"
    dc_address: ["192.168.100.70"]   # optional; empty => DNS SRV discovery
```

Restart the server. The log should show
`NETLOGON machine account: offline provider active account=DITTOFS$`.

Verify the machine account can reach the DC before testing a real logon:

```bash
dfs netlogon test --config /etc/dittofs/config.yaml
# OK — NETLOGON secure channel established and torn down successfully.
```

`dfs netlogon test` authenticates the machine account and brings up the NETLOGON
secure channel (no user logon), so it isolates a machine-account/DC/Kerberos
problem from an NTLM-logon problem. It probes the **offline** machine account;
online join provisions on the first logon (check the server log).

**Step 3 — test the NTLM path.** From a domain member, connect by **IP** (forces
NTLM) or double-click the server in Explorer → Network:

```powershell
net use * /delete /y
net use \\192.168.100.50\ditto /user:CUBBIT\alice P4ssword!
```

The debug log shows `NETLOGON pass-through: authenticated directory-resolved
domain user`, then the usual SID-grant match at TREE_CONNECT. For the
**double-click** experience specifically, LAN discovery (§10 of
[configuration.md](configuration.md)) must be enabled and, on a Windows host, the
discovery ports must be allowed **for the dfs.exe program** (Windows' built-in
Network-Discovery rules are scoped to `System`/`svchost`, so a third-party binary
is dropped by default — add inbound allow rules for 5357/TCP, 3702/UDP, 5353/UDP
pointing at your exact `dfs.exe` path).

> **Known limitation:** SMB multichannel **session-bind** for pass-through domain
> users is not yet supported — a client that opens extra channels logs a benign
> `LOGON_FAILURE` on the spare connection and keeps working on the primary. It
> does not affect browsing or mounting.

---

## 7. Verify and troubleshoot

**Prove no local users are needed:** you never ran `dfsctl user create` for alice
or bob — they mount purely as AD principals.

Check each logon gets the right level: as alice, reading works but writing is
denied; as bob, both work; as Administrator, full control. In the server debug
log (`DITTOFS_LOGGING_LEVEL=DEBUG`) each TREE_CONNECT logs the per-session SID
match — the decision is derived from that session's own PAC, so two users on one
workstation never bleed permissions.

| Symptom | Cause / fix |
|---|---|
| `System error 1219` on `net use` | A pre-existing/"Disconnected" session to the same server under different creds. `net use * /delete /y` and `cmdkey /delete:vm2.cubbit.local`, then retry. Not a DittoFS bug. |
| Mount falls back to NTLM or fails with a signing error | Keytab lacks AES keys (RC4-only) — Windows 11 rejects RC4 (#1318). Re-export with `-crypto AES256-SHA1` and set `msDS-SupportedEncryptionTypes`. |
| Grant by name returns "not found" | LDAP not configured/reachable, or the name isn't a directory object. Check `dfsctl identity-provider list` and `… test ldap`. Grant by `--sid` to bypass LDAP. |
| Access denied despite a grant | Confirm the grant with `dfsctl share permission list <share>`; confirm the user's ticket carries the granted group (their PAC group SIDs). |
| `ldaps://` cert error | Set `tls.ca_cert_file` to the DC's issuing CA; do not disable verification in production. |

---

## How authorization works (reference)

- At SMB **TREE_CONNECT**, DittoFS resolves the session's permission from (a) any
  local user/group grant and (b) the **SID grants** matched against the login's
  Kerberos PAC user + group SIDs — the higher wins. An AD principal with **no
  local account** is authorized entirely by (b).
- At the **file** layer, the share-root ACL carries a `sid:<SID>` ACE per grant,
  matched against the same PAC SIDs, so file operations agree with the share gate.
- **NFS** carries GIDs, not SIDs, so each SID grant also records the Unix id the
  principal resolves to (the RID in `idmap: rid`), and the NFS gate + root ACL
  match on that GID/UID. Grant by **name** (or a raw SID in `idmap: rid`) so the
  numeric id is captured; see [identity.md](identity.md) for NFS client setup.
  Because NFS matches by the *mapped* id (no SID is on the NFS wire), NFS
  authorization inherits the idmap's range contract: ensure AD ids do not overlap
  your local UID/GID space (the same requirement Samba meets with dedicated idmap
  ranges), or a local principal could match an AD grant over NFS. The SMB path is
  always SID-exact and not subject to this. A raw `--sid` grant only carries an
  NFS-matchable id under `idmap: rid`; under `idmap: rfc2307` grant by **name** so
  the correct `gidNumber` is captured.
- A **user-explicit** local grant (including `--level none`) is authoritative and
  is never overridden by a group or AD/SID grant — an explicit block stays a block.

See [access-control.md](access-control.md) for the full SID/ACL model.
