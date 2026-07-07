# Connecting a Windows Client to DittoFS

This guide walks through mounting a DittoFS share on a Windows machine over SMB, and using the
Windows NFS client if needed. It assumes DittoFS is already running on a Linux or macOS host.

For the test VM setup and protocol conformance runbook used by contributors, see
[`../internals/testing.md`](../internals/testing.md).

## Table of Contents

- [Network reachability](#network-reachability)
- [Mounting an SMB share](#mounting-an-smb-share)
  - [Enabling insecure guest logons (if needed)](#enabling-insecure-guest-logons-if-needed)
  - [Map a drive with net use](#map-a-drive-with-net-use)
  - [Browse with Explorer](#browse-with-explorer)
- [Mounting an NFS share](#mounting-an-nfs-share)
  - [Enable the Windows NFS client](#enable-the-windows-nfs-client)
  - [Mount an NFS share](#mount-an-nfs-share)
- [Known limitations](#known-limitations)
- [Troubleshooting](#troubleshooting)
  - [Connection issues](#connection-issues)
  - [Permission issues](#permission-issues)
  - [Performance issues](#performance-issues)
  - [NFS-specific issues](#nfs-specific-issues)

---

## Network reachability

Before mounting, confirm the Windows machine can reach the DittoFS host on the required ports:

| Protocol | Default port |
|----------|-------------|
| SMB | **12445** |
| NFS | **12049** |

From a PowerShell prompt on Windows:

```powershell
Test-NetConnection -ComputerName <host-ip> -Port 12445
```

If `TcpTestSucceeded` is `False`, check:

- DittoFS is running (`dfs start` on the host).
- The firewall on the host allows inbound connections on those ports from the Windows machine's
  IP address.
- The VM network mode routes traffic to the host (bridged and host-only modes work; NAT requires
  port forwarding).

---

## Mounting an SMB share

### Enabling insecure guest logons (if needed)

Windows 11 24H2 and later block insecure guest logons by default. If you are connecting without
Kerberos or with a local DittoFS user account, you may need to enable this policy.

> If your DittoFS deployment uses Kerberos authentication, skip this section.

**Option 1 — Group Policy Editor (`gpedit.msc`):**

1. Press `Win+R`, type `gpedit.msc`, press Enter.
2. Navigate to: **Computer Configuration** > **Administrative Templates** > **Network** >
   **Lanman Workstation**.
3. Double-click **"Enable insecure guest logons"**, set to **Enabled**, click OK.
4. Restart the Lanman Workstation service or reboot.

**Option 2 — Registry (Windows 11 Home or scripted setup):**

```powershell
# Run as Administrator
Set-ItemProperty `
  -Path "HKLM:\SYSTEM\CurrentControlSet\Services\LanmanWorkstation\Parameters" `
  -Name "AllowInsecureGuestAuth" -Value 1 -Type DWord
Restart-Service LanmanWorkstation
```

### Map a drive with net use

```cmd
net use Z: \\<host-ip>\<share-name> /user:<username> <password>
```

To reconnect at logon:

```cmd
net use Z: \\<host-ip>\<share-name> /user:<username> <password> /persistent:yes
```

To disconnect:

```cmd
net use Z: /delete
```

### Browse with Explorer

Open Explorer and type the UNC path in the address bar:

```
\\<host-ip>\<share-name>
```

Windows will prompt for credentials if the server requires them.

### Per-user access on a shared workstation (Active Directory)

When DittoFS is joined to AD (Kerberos), each interactive Windows logon mounts
the share with **that user's own permissions** — no per-user configuration on the
DittoFS side. Because each logon holds its own Kerberos ticket, mounting
`\\<host>\<share>` presents that user's identity, and DittoFS authorizes them by
their AD user/group SIDs. On one shared machine: alice logs in and gets her
level, bob logs in and gets his, an administrator gets theirs — each independent.

To mount automatically at logon for every user, use **Group Policy Drive Maps**
(User Configuration → Preferences → Windows Settings → Drive Maps) or a per-user
logon script running `net use Z: \\<host>\<share> /persistent:yes` — no
credentials are passed, Windows uses the logged-in user's ticket (SSO).

Grant AD users/groups directly, with no local DittoFS accounts — see the
[Windows AD setup runbook](windows-ad-setup.md).

---

## Mounting an NFS share

Windows NFS client support is best-effort. SMB is the recommended protocol for Windows clients.
The Windows Services for NFS client has known limitations compared to Linux or macOS NFS clients.

### Enable the Windows NFS client

1. Open **Settings** > **Apps** > **Optional Features** > **More Windows features**.
2. Expand **Services for NFS**.
3. Check **Client for NFS**, click OK, and restart if prompted.

### Mount an NFS share

```cmd
mount -o anon \\<host-ip>\<export-path> Z:
```

The NFS adapter listens on port **12049**. To unmount:

```cmd
umount Z:
```

If you encounter a "Network Error" at mount time, try:

```cmd
mount -o anon,nolock \\<host-ip>\<export-path> Z:
```

---

## Known limitations

| Limitation | Impact | Workaround |
|-----------|--------|------------|
| **No multi-channel SMB** | Single TCP connection per session | Performance is limited to single-connection throughput |
| **NFS from Windows is best-effort** | Windows NFS client has limited functionality | Use SMB for primary Windows file access |
| **No NTFS object IDs** | `FSCTL_CREATE_OR_GET_OBJECT_ID` not supported | No impact for typical workflows |
| **No DFS referrals** | Distributed File System namespace not supported | Access shares directly by server IP or hostname |

SMB3 encryption and signing, change notifications, durable handles, and server-side copy
(`FSCTL_SRV_COPYCHUNK`) **are** supported — see [`./smb.md`](./smb.md).

---

## Troubleshooting

### Connection issues

| Symptom | Cause | Solution |
|---------|-------|----------|
| "The network path was not found" | DittoFS not running or firewall blocking the port | Verify `dfs start` is running; check port 12445 is accessible with `Test-NetConnection` |
| "Access denied" | Invalid credentials or missing share permissions | Check share permissions (`dfsctl share permission list /<share>`). For a local user, verify it exists (`dfsctl user list`). For an AD user, no local account is needed — confirm a grant matches their SID or one of their AD group SIDs (see [windows-ad-setup.md](windows-ad-setup.md)) |
| "The specified network name is no longer available" | Connection dropped during operation | Retry `net use`; check DittoFS logs for errors |
| "Insecure guest logon" error | Windows 11 24H2 blocks guest logons by default | Follow [Enabling insecure guest logons](#enabling-insecure-guest-logons-if-needed) above |

### Permission issues

| Symptom | Cause | Solution |
|---------|-------|----------|
| Explorer shows blank Security tab | SD query failed or returned malformed data | Check DittoFS logs for SD synthesis errors |
| Explorer shows "Everyone: Full Control" | SD synthesis not returning proper owner/DACL | Verify machine SID is initialized (`dfsctl settings list`); ensure share has an assigned user |
| `icacls` shows unexpected permissions | POSIX-to-DACL translation differs from NTFS semantics | Expected; DittoFS synthesizes DACLs from Unix mode bits |
| `Set-Acl` returns an error | Best-effort ACL mapping | DittoFS translates SMB SET_INFO ACL changes to Unix mode bits; complex ACLs may not map cleanly |

### Performance issues

| Symptom | Cause | Solution |
|---------|-------|----------|
| Slow large file copies | No multi-channel (single TCP connection) | Ensure sufficient network bandwidth |
| Explorer feels sluggish | Many small SMB round-trips | Reduce auto-refresh extensions |
| Office save takes a long time | Office uses multiple SMB operations (create temp, write, rename) | Expected; ensure sufficient network bandwidth |

### NFS-specific issues

| Symptom | Cause | Solution |
|---------|-------|----------|
| `mount` command not found | Services for NFS not installed | Install via Settings > Apps > Optional Features > More Windows features > Services for NFS > Client for NFS |
| Mount fails with "Network Error" | Wrong port or protocol | Ensure the DittoFS NFS adapter is on port 12049; try `mount -o anon,nolock \\host\export Z:` |
| Permission denied on NFS mount | UID/GID mapping issues | Windows NFS client uses anonymous UID/GID by default; configure in NFS client settings or use `mount -o anon` |
