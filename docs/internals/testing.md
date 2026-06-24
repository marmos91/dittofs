# Testing DittoFS

This document is the protocol-conformance and interop test runbook for DittoFS contributors. It
covers Windows VM setup, manual SMB and NFS validation checklists, and the WPTS and smbtorture
conformance suites.

For Go unit/integration tests and the e2e test runner, see
[`./contributing.md`](./contributing.md).

For the end-user guide to connecting a Windows client, see
[`../guide/windows.md`](../guide/windows.md).

## Table of Contents

- [Windows VM setup](#windows-vm-setup)
  - [Option A: UTM (Apple Silicon, recommended)](#option-a-utm-apple-silicon-recommended)
  - [Option B: VirtualBox / Hyper-V (x86)](#option-b-virtualbox--hyper-v-x86)
  - [Network configuration](#network-configuration)
  - [Required Windows features](#required-windows-features)
  - [Guest auth GPO configuration](#guest-auth-gpo-configuration)
  - [DittoFS server setup for testing](#dittofs-server-setup-for-testing)
- [SMB manual validation checklist](#smb-manual-validation-checklist)
  - [Connection tests](#connection-tests)
  - [Explorer operations](#explorer-operations)
  - [cmd.exe operations](#cmdexe-operations)
  - [PowerShell operations](#powershell-operations)
  - [Office testing](#office-testing)
  - [VS Code testing](#vs-code-testing)
  - [File size testing](#file-size-testing)
- [NFS client validation checklist](#nfs-client-validation-checklist)
  - [NFS connection](#nfs-connection)
  - [NFS file operations](#nfs-file-operations)
- [Conformance test suites](#conformance-test-suites)
  - [WPTS (Windows Protocol Test Suites)](#wpts-windows-protocol-test-suites)
  - [smbtorture (Samba Test Suite)](#smbtorture-samba-test-suite)
  - [Running both suites](#running-both-suites)

---

## Windows VM setup

**Last validated:** 2026-02-28 (Windows 11 24H2)

### Option A: UTM (Apple Silicon, recommended)

UTM is the recommended VM solution for Apple Silicon Macs running DittoFS.

1. Download and install UTM from [mac.getutm.app](https://mac.getutm.app/).
2. Download the Windows 11 ARM ISO from [Microsoft UUP dump](https://uupdump.net/) or the
   official Microsoft site.
3. Create a new VM:
   - Click "Create a New Virtual Machine" > "Virtualize".
   - Select "Windows".
   - Allocate at least 4 GB RAM and 2 CPU cores.
   - Allocate 64 GB disk (minimum).
   - Enable "Install drivers and SPICE tools".
4. Attach the Windows 11 ARM ISO and boot the VM.
5. Complete the Windows 11 installation (skip network during OOBE if needed; use a local
   account).

### Option B: VirtualBox / Hyper-V (x86)

- **VirtualBox:** download from [virtualbox.org](https://www.virtualbox.org/), create a Windows
  11 x64 VM with at least 4 GB RAM, enable EFI and TPM 2.0 emulation.
- **Hyper-V:** available on Windows 10/11 Pro/Enterprise, enable via "Turn Windows features on
  or off".

### Network configuration

The Windows VM must be able to reach the DittoFS host on ports **12445** (SMB) and **12049**
(NFS).

| Network mode | When to use | Configuration |
|-------------|-------------|---------------|
| **Bridged** | VM and host on same LAN | VM gets its own IP from DHCP; use host's LAN IP to connect |
| **Host-only** | Isolated testing | Configure a host-only network adapter; use host-only gateway IP |
| **Shared (NAT)** | Default in UTM | Forward ports 12445 (SMB) and 12049 (NFS) from host to guest |

Verify connectivity from the Windows VM:

```powershell
Test-NetConnection -ComputerName <host-ip> -Port 12445
```

### Required Windows features

**Client for NFS** (for NFS client testing):

1. Open **Settings** > **Apps** > **Optional Features** > **More Windows features**.
2. Expand **Services for NFS**, check **Client for NFS**, click OK and restart if prompted.

**SMB client:** installed and enabled by default on Windows 11; no additional setup needed.

### Guest auth GPO configuration

Windows 11 24H2 blocks insecure guest logons by default. For testing DittoFS with
guest/anonymous sessions:

**Option 1 — Group Policy Editor (`gpedit.msc`):**

1. Press `Win+R`, type `gpedit.msc`, press Enter.
2. Navigate to: **Computer Configuration** > **Administrative Templates** > **Network** >
   **Lanman Workstation**.
3. Double-click **"Enable insecure guest logons"**, set to **Enabled**, click OK.
4. Restart the Lanman Workstation service or reboot.

**Option 2 — Registry (Windows 11 Home or scripted):**

```powershell
# Run as Administrator
Set-ItemProperty `
  -Path "HKLM:\SYSTEM\CurrentControlSet\Services\LanmanWorkstation\Parameters" `
  -Name "AllowInsecureGuestAuth" -Value 1 -Type DWord
Restart-Service LanmanWorkstation
```

Registry path: `HKLM\SYSTEM\CurrentControlSet\Services\LanmanWorkstation\Parameters\AllowInsecureGuestAuth` = 1 (DWORD)

### DittoFS server setup for testing

On the macOS/Linux host:

```bash
# 1. Build DittoFS
go build -o dfs cmd/dfs/main.go
go build -o dfsctl cmd/dfsctl/main.go

# 2. Initialize config (first time only)
./dfs config init

# 3. Start DittoFS
./dfs start

# 4. Create test stores
./dfsctl store metadata add --name test-meta --type memory
./dfsctl store block local add --name test-blocks --type memory

# 5. Create a test share
./dfsctl share create --name /smbbasic --metadata test-meta --local test-blocks

# 6. Create a test user
./dfsctl user create --username testuser    # enter password when prompted

# 7. Grant permissions
./dfsctl share permission grant /smbbasic --user testuser --level read-write

# 8. Verify the SMB adapter is listening on port 12445
./dfsctl adapter list
```

Connect from the Windows VM:

```cmd
rem Mapped drive
net use Z: \\<host-ip>\smbbasic /user:testuser <password>

rem UNC path
explorer \\<host-ip>\smbbasic
```

Optional software useful for testing:

| Software | Purpose |
|----------|---------|
| VS Code | Test opening projects from SMB share, file editing, integrated terminal |
| Microsoft Office | Test Word/Excel save/open cycles with file locking |
| 7-Zip or WinRAR | Test archive extraction to/from share |

---

## SMB manual validation checklist

Test both **mapped drives** (e.g., `Z:`) and **UNC paths** (e.g., `\\host\smbbasic`) for each
category.

### Connection tests

| # | Test | Mapped drive | UNC path | Notes |
|---|------|:---:|:---:|-------|
| C-01 | Connect with `net use Z: \\host\smbbasic /user:testuser <pass>` | [ ] Pass / [ ] Fail / [ ] Skip | N/A | |
| C-02 | Browse via Explorer `\\host\smbbasic` | N/A | [ ] Pass / [ ] Fail / [ ] Skip | |
| C-03 | Disconnect with `net use Z: /delete` | [ ] Pass / [ ] Fail / [ ] Skip | N/A | |
| C-04 | Reconnect after disconnect | [ ] Pass / [ ] Fail / [ ] Skip | [ ] Pass / [ ] Fail / [ ] Skip | |

### Explorer operations

| # | Test | Mapped drive | UNC path | Notes |
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

### cmd.exe operations

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

### PowerShell operations

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

### Office testing

| # | Test | Steps | Result | Notes |
|---|------|-------|:---:|-------|
| OFF-01 | Word: Create new document | Open Word > Save As to `Z:\` as .docx | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-02 | Word: Reopen document | Double-click the .docx on `Z:\` | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-03 | Word: Edit and save | Modify text, Ctrl+S | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-04 | Excel: Create workbook | Open Excel > Add data + formulas > Save As .xlsx to `Z:\` | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-05 | Excel: Reopen workbook | Double-click the .xlsx on `Z:\` | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-06 | Excel: Verify formulas | Check that formulas compute correctly after reopen | [ ] Pass / [ ] Fail / [ ] Skip | |
| OFF-07 | Large file save (10 MB+) | Create a large document with images, save to `Z:\` | [ ] Pass / [ ] Fail / [ ] Skip | |

### VS Code testing

| # | Test | Steps | Result | Notes |
|---|------|-------|:---:|-------|
| VS-01 | Open folder from share | File > Open Folder > select `Z:\` or `\\host\smbbasic` | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-02 | Create new file | File > New File > save to share | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-03 | Edit and save | Modify file content, Ctrl+S | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-04 | Search across files | Ctrl+Shift+F, search for a string | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-05 | Integrated terminal | Open terminal, run `dir` on share path | [ ] Pass / [ ] Fail / [ ] Skip | |
| VS-06 | Git operations (if .git exists) | `git status`, `git log` on share | [ ] Pass / [ ] Fail / [ ] Skip | Best-effort |

### File size testing

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

## NFS client validation checklist

Windows NFS client support is best-effort. The Windows Services for NFS client has known
limitations compared to Linux/macOS NFS clients.

### NFS connection

| # | Test | Command | Result | Notes |
|---|------|---------|:---:|-------|
| NFS-01 | Mount NFS share | `mount -o anon \\<host-ip>\export Z:` | [ ] Pass / [ ] Fail / [ ] Skip | Port 12049 |
| NFS-02 | List share contents | `dir Z:\` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-03 | Unmount | `umount Z:` | [ ] Pass / [ ] Fail / [ ] Skip | |

### NFS file operations

| # | Test | Command | Result | Notes |
|---|------|---------|:---:|-------|
| NFS-04 | Create file | `echo test > Z:\nfstest.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-05 | Read file | `type Z:\nfstest.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-06 | Create directory | `mkdir Z:\nfsdir` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-07 | Delete file | `del Z:\nfstest.txt` | [ ] Pass / [ ] Fail / [ ] Skip | |
| NFS-08 | Delete directory | `rmdir Z:\nfsdir` | [ ] Pass / [ ] Fail / [ ] Skip | |

---

## Conformance test suites

DittoFS is validated against two industry-standard conformance test suites.

### WPTS (Windows Protocol Test Suites)

- **Suite:** MS-SMB2 BVT (Build Verification Tests)
- **Known failures:** see
  [`../../test/smb-conformance/KNOWN_FAILURES.md`](../../test/smb-conformance/KNOWN_FAILURES.md)
- **Run locally:**
  ```bash
  cd test/smb-conformance
  make test        # Run WPTS BVT suite
  make test-quick  # Quick run (memory profile only)
  ```

### smbtorture (Samba Test Suite)

- **Suite:** Full SMB2 test suite (`smb2.*`)
- **Image:** `quay.io/samba.org/samba-toolbox:v0.8`
- **Known failures:** see
  [`../../test/smb-conformance/smbtorture/KNOWN_FAILURES.md`](../../test/smb-conformance/smbtorture/KNOWN_FAILURES.md)
- **Run locally:**
  ```bash
  cd test/smb-conformance
  make smbtorture        # Run full smbtorture suite
  make smbtorture-quick  # Quick run (memory profile only)
  ```

### Running both suites

```bash
cd test/smb-conformance
make test smbtorture     # Run WPTS + smbtorture in sequence
```

Both test suites run in CI via `.github/workflows/smb-conformance.yml` on every PR touching
SMB-related code.

> Do not run two instances of the e2e or conformance suites concurrently — they share a Docker
> container name and will collide. Run them serially and `docker rm -f` between runs if needed.
