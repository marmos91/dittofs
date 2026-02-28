# Windows Testing Guide

**Version:** 1.0 (v3.6 milestone)
**Last validated:** 2026-02-28
**Windows version tested:** Windows 11 24H2
**DittoFS version:** v0.7.1-42-ga6c59f39

This document provides a comprehensive guide for setting up a Windows 11 VM to test DittoFS, along with a formal validation checklist covering SMB file operations, NFS client testing, and application compatibility.

---

## Table of Contents

1. [Windows VM Setup Guide](#windows-vm-setup-guide)
2. [SMB Validation Checklist](#smb-validation-checklist)
3. [NFS Client Validation Checklist](#nfs-client-validation-checklist)
4. [Known Limitations](#known-limitations)
5. [Troubleshooting](#troubleshooting)
6. [Conformance Test Results](#conformance-test-results)

---

## Windows VM Setup Guide

### Option A: UTM (macOS ARM Host) -- Recommended for Apple Silicon

UTM is the recommended VM solution for Apple Silicon Macs running DittoFS.

1. **Download and install UTM** from [mac.getutm.app](https://mac.getutm.app/)
2. **Download Windows 11 ARM ISO** from [Microsoft UUP dump](https://uupdump.net/) or the official Microsoft site
3. **Create a new VM:**
   - Click "Create a New Virtual Machine" > "Virtualize"
   - Select "Windows"
   - Allocate at least 4 GB RAM and 2 CPU cores
   - Allocate 64 GB disk (minimum)
   - Enable "Install drivers and SPICE tools"
4. **Attach the Windows 11 ARM ISO** and boot the VM
5. **Complete Windows 11 installation** (skip network during OOBE if needed, use a local account)

### Option B: VirtualBox / Hyper-V (x86 Host)

For x86 hosts or non-macOS environments:

- **VirtualBox:** Download from [virtualbox.org](https://www.virtualbox.org/), create a Windows 11 x64 VM with at least 4 GB RAM, enable EFI and TPM 2.0 emulation
- **Hyper-V:** Available on Windows 10/11 Pro/Enterprise, enable via "Turn Windows features on or off"

### Network Configuration

The Windows VM must be able to reach the DittoFS host machine over the network.

| Network Mode | When to Use | Configuration |
|-------------|-------------|---------------|
| **Bridged** | VM and host on same LAN | VM gets its own IP from DHCP; use host's LAN IP to connect |
| **Host-only** | Isolated testing | Configure a host-only network adapter; use host-only gateway IP |
| **Shared (NAT)** | Default in UTM | Forward ports 12445 (SMB) and 12049 (NFS) from host to guest |

**Verify connectivity:** From the Windows VM, open PowerShell and run:
```powershell
Test-NetConnection -ComputerName <host-ip> -Port 12445
```

### Required Windows Features

#### Services for NFS (for NFS client testing)

1. Open **Settings** > **Apps** > **Optional Features** > **More Windows features**
2. Expand **Services for NFS**
3. Check **Client for NFS**
4. Click OK and restart if prompted

#### SMB Client

The SMB client is installed and enabled by default on Windows 11. No additional setup is needed.

### Guest Auth GPO Configuration

**Windows 11 24H2 blocks insecure guest logons by default.** Since DittoFS currently uses guest/anonymous SMB sessions for initial negotiation, you must enable insecure guest logons.

**Option 1: Group Policy Editor (gpedit.msc)**

1. Press `Win+R`, type `gpedit.msc`, press Enter
2. Navigate to: **Computer Configuration** > **Administrative Templates** > **Network** > **Lanman Workstation**
3. Double-click **"Enable insecure guest logons"**
4. Set to **Enabled**, click OK
5. Restart the Lanman Workstation service or reboot

**Option 2: Registry (for Windows 11 Home or scripted setup)**

```powershell
# Run PowerShell as Administrator
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\LanmanWorkstation\Parameters" -Name "AllowInsecureGuestAuth" -Value 1 -Type DWord
Restart-Service LanmanWorkstation
```

**Registry path:** `HKLM\SYSTEM\CurrentControlSet\Services\LanmanWorkstation\Parameters\AllowInsecureGuestAuth` = 1 (DWORD)

### Optional Software

| Software | Purpose |
|----------|---------|
| **VS Code** | Test opening projects from SMB share, file editing, integrated terminal |
| **Microsoft Office** | Test Word/Excel save/open cycles with Office file locking |
| **7-Zip or WinRAR** | Test archive extraction to/from share |

### DittoFS Server Setup

On the macOS/Linux host machine:

```bash
# 1. Build DittoFS
go build -o dfs cmd/dfs/main.go
go build -o dfsctl cmd/dfsctl/main.go

# 2. Initialize config (if first time)
./dfs config init

# 3. Start DittoFS
./dfs start

# 4. Create test stores
./dfsctl store metadata add --name test-meta --type memory
./dfsctl store payload add --name test-payload --type memory

# 5. Create test shares
./dfsctl share create --name /smbbasic --metadata test-meta --payload test-payload

# 6. Create test users
./dfsctl user create --username testuser    # Enter password when prompted

# 7. Grant permissions
./dfsctl share permission grant /smbbasic --user testuser --level read-write

# 8. Verify SMB adapter is listening
./dfsctl adapter list
# Should show SMB adapter on port 12445
```

**Connect from Windows:**

```cmd
# Mapped drive (persistent)
net use Z: \\<host-ip>\smbbasic /user:testuser <password>

# UNC path (direct access)
explorer \\<host-ip>\smbbasic
```

---

## SMB Validation Checklist

Test both **mapped drives** (e.g., `Z:`) and **UNC paths** (e.g., `\\host\smbbasic`) for each category.

### Connection Tests

| # | Test | Mapped Drive | UNC Path | Notes |
|---|------|:---:|:---:|-------|
| C-01 | Connect with `net use Z: \\host\smbbasic /user:testuser <pass>` | [ ] Pass / [ ] Fail / [ ] Skip | N/A | |
| C-02 | Browse via Explorer `\\host\smbbasic` | N/A | [ ] Pass / [ ] Fail / [ ] Skip | |
| C-03 | Disconnect with `net use Z: /delete` | [ ] Pass / [ ] Fail / [ ] Skip | N/A | |
| C-04 | Reconnect after disconnect | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |

### Explorer Operations

| # | Test | Mapped Drive | UNC Path | Notes |
|---|------|:---:|:---:|-------|
| E-01 | Create new text file (right-click > New > Text Document) | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-02 | Create new folder | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-03 | Rename file | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-04 | Rename folder | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-05 | Delete file (Delete key or right-click > Delete) | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-06 | Delete folder (empty) | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-07 | Delete folder (non-empty) | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-08 | Copy file (Ctrl+C / Ctrl+V) | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-09 | Move file (Ctrl+X / Ctrl+V) | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-10 | Drag-and-drop file within share | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-11 | Copy file from local disk to share | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-12 | Copy file from share to local disk | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-13 | View file Properties (General tab) | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |
| E-14 | View file Properties (Security tab) | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | Should show Owner + DACL |

### cmd.exe Operations

| # | Test | Command | Result | Notes |
|---|------|---------|:---:|-------|
| CMD-01 | List directory | `dir Z:\` | [ ] Pass / [ ] Fail / [ ] Skip | |
| CMD-02 | Display file contents | `type Z:\testfile.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| CMD-03 | Copy file | `copy Z:\file1.txt Z:\file2.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| CMD-04 | Move file | `move Z:\file2.txt Z:\subfolder\` | [ ] Pass / [ ] Fail / [ ] Skip | |
| CMD-05 | Rename file | `ren Z:\file1.txt newname.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| CMD-06 | Delete file | `del Z:\newname.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| CMD-07 | Create directory | `mkdir Z:\testdir` | [ ] Pass / [ ] Fail / [ ] Skip | |
| CMD-08 | Remove directory | `rmdir Z:\testdir` | [ ] Pass / [ ] Fail / [ ] Skip | |
| CMD-09 | View ACLs | `icacls Z:\` | [ ] Pass / [ ] Fail / [ ] Skip | Should show Owner + DACL entries |
| CMD-10 | View attributes | `attrib Z:\testfile.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| CMD-11 | File system info | `fsutil fsinfo volumeinfo Z:\` | [ ] Pass / [ ] Fail / [ ] Skip | |

### PowerShell Operations

| # | Test | Command | Result | Notes |
|---|------|---------|:---:|-------|
| PS-01 | Get file info | `Get-Item Z:\testfile.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| PS-02 | List directory | `Get-ChildItem Z:\ -Recurse` | [ ] Pass / [ ] Fail / [ ] Skip | |
| PS-03 | Create file | `New-Item Z:\pstest.txt -ItemType File -Value "hello"` | [ ] Pass / [ ] Fail / [ ] Skip | |
| PS-04 | Create directory | `New-Item Z:\psdir -ItemType Directory` | [ ] Pass / [ ] Fail / [ ] Skip | |
| PS-05 | Remove file | `Remove-Item Z:\pstest.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| PS-06 | Remove directory | `Remove-Item Z:\psdir -Recurse` | [ ] Pass / [ ] Fail / [ ] Skip | |
| PS-07 | Copy file | `Copy-Item Z:\file1.txt Z:\file_copy.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| PS-08 | Move file | `Move-Item Z:\file_copy.txt Z:\moved.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| PS-09 | Get ACL | `Get-Acl Z:\testfile.txt` | [ ] Pass / [ ] Fail / [ ] Skip | Should show Owner + Access rules |
| PS-10 | Set ACL | `$acl = Get-Acl Z:\testfile.txt; Set-Acl Z:\testfile.txt $acl` | [ ] Pass / [ ] Fail / [ ] Skip | Round-trip test |
| PS-11 | Write content | `Set-Content Z:\pstest.txt "new content"` | [ ] Pass / [ ] Fail / [ ] Skip | |

### Office Testing

| # | Test | Steps | Result | Notes |
|---|------|-------|:---:|-------|
| OFF-01 | Word: Create new document | Open Word > Save As to Z:\ as .docx | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-02 | Word: Reopen document | Double-click the .docx on Z:\ | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-03 | Word: Edit and save | Modify text, Ctrl+S | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-04 | Excel: Create workbook | Open Excel > Add data + formulas > Save As .xlsx to Z:\ | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-05 | Excel: Reopen workbook | Double-click the .xlsx on Z:\ | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-06 | Excel: Verify formulas | Check that formulas compute correctly after reopen | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-07 | Large file save (10MB+) | Create a large document with images, save to Z:\ | [ ] Pass / [ ] Fail / [ ] Skip | |

### VS Code Testing

| # | Test | Steps | Result | Notes |
|---|------|-------|:---:|-------|
| VS-01 | Open folder from share | File > Open Folder > select Z:\ or \\host\smbbasic | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-02 | Create new file | File > New File > save to share | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-03 | Edit and save | Modify file content, Ctrl+S | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-04 | Search across files | Ctrl+Shift+F, search for a string | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-05 | Integrated terminal | Open terminal, run `dir` on share path | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-06 | Git operations (if .git exists) | `git status`, `git log` on share | [ ] Pass / [ ] Fail / [ ] Skip | Best-effort |

### File Size Testing

| # | Test | Command | Result | Notes |
|---|------|---------|:---:|-------|
| FS-01 | 1 MB file | `fsutil file createnew Z:\test_1mb.bin 1048576` | [ ] Pass / [ ] Fail / [ ] Skip | |
| FS-02 | 10 MB file | `fsutil file createnew Z:\test_10mb.bin 10485760` | [ ] Pass / [ ] Fail / [ ] Skip | |
| FS-03 | 50 MB file | `fsutil file createnew Z:\test_50mb.bin 52428800` | [ ] Pass / [ ] Fail / [ ] Skip | |
| FS-04 | 100 MB file | `fsutil file createnew Z:\test_100mb.bin 104857600` | [ ] Pass / [ ] Fail / [ ] Skip | |
| FS-05 | Read back 1 MB | `copy Z:\test_1mb.bin NUL` | [ ] Pass / [ ] Fail / [ ] Skip | |
| FS-06 | Read back 10 MB | `copy Z:\test_10mb.bin NUL` | [ ] Pass / [ ] Fail / [ ] Skip | |
| FS-07 | Read back 50 MB | `copy Z:\test_50mb.bin NUL` | [ ] Pass / [ ] Fail / [ ] Skip | |
| FS-08 | Read back 100 MB | `copy Z:\test_100mb.bin NUL` | [ ] Pass / [ ] Fail / [ ] Skip | |
| FS-09 | Verify integrity | Write known content, read back, compare | [ ] Pass / [ ] Fail / [ ] Skip | Use `certutil -hashfile` for SHA256 |

---

## NFS Client Validation Checklist

Windows NFS client support is best-effort. The Windows Services for NFS client has known limitations compared to Linux/macOS NFS clients.

### NFS Connection

| # | Test | Command | Result | Notes |
|---|------|---------|:---:|-------|
| NFS-01 | Mount NFS share | `mount -o anon \\<host-ip>\export Z:` | [ ] Pass / [ ] Fail / [ ] Skip | Port 12049 |
| NFS-02 | List share contents | `dir Z:\` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-03 | Unmount | `umount Z:` | [ ] Pass / [ ] Fail / [ ] Skip | |

### NFS File Operations

| # | Test | Command | Result | Notes |
|---|------|---------|:---:|-------|
| NFS-04 | Create file | `echo test > Z:\nfstest.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-05 | Read file | `type Z:\nfstest.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-06 | Create directory | `mkdir Z:\nfsdir` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-07 | Delete file | `del Z:\nfstest.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-08 | Delete directory | `rmdir Z:\nfsdir` | [ ] Pass / [ ] Fail / [ ] Skip | |

---

## Known Limitations

The following features are **not supported** in DittoFS v3.6. These are architectural decisions, not bugs.

| Limitation | Impact | Workaround | Future Phase |
|-----------|--------|------------|-------------|
| **No Alternate Data Streams (ADS)** | NTFS named streams (`:Zone.Identifier`, etc.) not available | None -- files from Internet may lack "Unblock" option | v3.8 Phase 43 |
| **No Change Notify** | Windows Explorer does not auto-refresh when files change externally | Press **F5** to manually refresh | v3.8 Phase 40.5 |
| **No SMB3 encryption/signing upgrade** | SMB 2.0.2/2.1 only -- no AES-CCM/GCM encryption | Traffic is unencrypted; use VPN for untrusted networks | v3.8 Phase 39 |
| **No durable handles** | Reconnection after network interruption not supported; open files lost on disconnect | Save frequently; expect re-authentication after disconnects | v3.8 Phase 42 |
| **No server-side copy** | `FSCTL_SRV_COPYCHUNK` not implemented; copies go through client | Large file copies may be slower than native SMB servers | v3.8 Phase 43 |
| **No multi-channel** | Single TCP connection per session | Performance limited to single connection throughput | v3.8 |
| **NFS from Windows** | Windows NFS client (Services for NFS) has limited functionality | Use SMB for primary Windows file access; NFS is best-effort | -- |
| **No NTFS object IDs** | `FSCTL_CREATE_OR_GET_OBJECT_ID` not supported | No impact for typical workflows | Not planned |
| **No DFS referrals** | Distributed File System namespace not supported | Access shares directly by server IP/hostname | Not planned |

---

## Troubleshooting

### Connection Issues

| Symptom | Cause | Solution |
|---------|-------|----------|
| "The network path was not found" | DittoFS not running or firewall blocking port | Verify `dfs start` is running; check port 12445 is accessible with `Test-NetConnection` |
| "Access denied" | Invalid credentials or missing share permissions | Verify user exists (`dfsctl user list`), check share permissions (`dfsctl share permission list /smbbasic`) |
| "The specified network name is no longer available" | Connection dropped during operation | Retry `net use`; check DittoFS logs for errors |
| "Insecure guest logon" error | Windows 11 24H2 blocks guest logons by default | Follow [Guest Auth GPO Configuration](#guest-auth-gpo-configuration) above |

### Permission Issues

| Symptom | Cause | Solution |
|---------|-------|----------|
| Explorer shows blank Security tab | SD query failed or returned malformed data | Check DittoFS logs for SD synthesis errors; ensure Phase 31 ACL support is active |
| Explorer shows "Everyone: Full Control" | SD synthesis not returning proper owner/DACL | Verify machine SID is initialized (`dfsctl settings list`); ensure share has assigned user |
| `icacls` shows unexpected permissions | POSIX-to-DACL translation differs from NTFS semantics | This is expected behavior; DittoFS synthesizes DACLs from Unix mode bits |
| `Set-Acl` returns error | Best-effort ACL mapping failed | DittoFS translates SMB SET_INFO ACL changes to Unix mode bits; complex ACLs may not map cleanly |

### Performance Issues

| Symptom | Cause | Solution |
|---------|-------|----------|
| Slow large file copies | No server-side copy; data traverses client | Expected for v3.6; server-side copy planned for v3.8 |
| Explorer feels sluggish | No Change Notify; Explorer polls for changes | Press F5 to refresh; reduce auto-refresh extensions |
| Office save takes long | Office uses multiple SMB operations (create temp, write, rename) | Expected behavior; ensure sufficient network bandwidth |

### NFS-Specific Issues

| Symptom | Cause | Solution |
|---------|-------|----------|
| `mount` command not found | Services for NFS not installed | Install via Settings > Apps > Optional Features > More Windows features > Services for NFS > Client for NFS |
| Mount fails with "Network Error" | Wrong port or protocol | Ensure DittoFS NFS adapter is on port 12049; try `mount -o anon,nolock \\host\export Z:` |
| Permission denied on NFS mount | UID/GID mapping issues | Windows NFS client uses anonymous UID/GID by default; configure in NFS client settings or use `mount -o anon` |

---

## Conformance Test Results

DittoFS is validated against two industry-standard conformance test suites.

### WPTS (Microsoft WindowsProtocolTestSuites)

- **Suite:** MS-SMB2 BVT (Build Verification Tests)
- **Baseline:** 133/240 tests passing (Phase 29.8 initial baseline)
- **Expected improvements:** Phases 30-32 fixes (sparse READ, directory listing, ACL support, MxAc, QFid) should increase pass count
- **Known failures:** See [test/smb-conformance/KNOWN_FAILURES.md](../test/smb-conformance/KNOWN_FAILURES.md)
- **Run locally:**
  ```bash
  cd test/smb-conformance
  make test        # Run WPTS BVT suite
  make test-quick  # Quick run (memory profile only)
  ```

### smbtorture (Samba Test Suite)

- **Suite:** Full SMB2 test suite (`smb2.*`)
- **Image:** quay.io/samba.org/samba-toolbox:v0.8
- **Known failures:** See [test/smb-conformance/smbtorture/KNOWN_FAILURES.md](../test/smb-conformance/smbtorture/KNOWN_FAILURES.md)
- **Run locally:**
  ```bash
  cd test/smb-conformance
  make smbtorture        # Run full smbtorture suite
  make smbtorture-quick  # Quick run (memory profile only)
  ```

### Running Both Suites

```bash
cd test/smb-conformance
make test smbtorture     # Run WPTS + smbtorture in sequence
```

Both test suites run in CI via `.github/workflows/smb-conformance.yml` on every PR touching SMB-related code.

---

## Checklist Version History

| Version | Date | Milestone | Changes |
|---------|------|-----------|---------|
| 1.0 | 2026-02-28 | v3.6 | Initial checklist with Explorer, cmd.exe, PowerShell, Office, VS Code, NFS, file size tests |

---
*Document: docs/WINDOWS_TESTING.md*
*Phase: 32-windows-integration-testing*
*Created: 2026-02-28*
