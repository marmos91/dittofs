<#
.SYNOPSIS
  Capture the Windows-side proof for #1297: real Windows Explorer / icacls
  resolving DittoFS SMB security-descriptor SIDs to AD names (DITTOFS\alice),
  exercising the LSARPC LsarLookupSids2/3 path fixed in #1291 + #1341.

.DESCRIPTION
  Run this INSIDE an RDP session on the Windows test VM (Scaleway has no
  headless SSH on Windows — RDP only). It:
    1. maps the DittoFS SMB share,
    2. starts a pktmon packet capture (built-in; no Wireshark needed),
    3. reads a file's ACL via icacls + Get-Acl (same LSARPC path the Explorer
       Security tab uses) so resolved names land in a text log,
    4. stops the capture and converts the .etl to .pcapng via etl2pcapng.

  Then open the same file's Properties > Security tab in Explorer and screenshot
  it — that is the human-facing #1297 evidence; the icacls text + pcap are the
  machine-checkable backup.

.NOTES
  Auth: connecting AS the AD user `alice` over SMB needs NTLM passthrough
  (#1314 / PR #1344) deployed on the server, OR this VM domain-joined to
  DITTOFS.AD. SID->name *resolution* (the thing under test) is independent of
  who authenticates — any session that can read the SD triggers it. If alice
  auth fails with LOGON_FAILURE, #1344 is not yet on the server.
#>

param(
  [string]$Server   = "51.15.130.120",        # DittoFS SMB LB
  [string]$Share    = "demo",
  [string]$User     = "DITTOFS\alice",
  [string]$Password = "TestPassword01!",
  [string]$OutDir   = "C:\dittofs-acl-evidence"
)

$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$etl  = Join-Path $OutDir "lsarpc.etl"
$pcap = Join-Path $OutDir "lsarpc.pcapng"
$log  = Join-Path $OutDir "acl-evidence.txt"
function Log($m) { $m | Tee-Object -FilePath $log -Append }

Log "=== DittoFS #1297 Windows ACL capture  $(Get-Date -Format o) ==="
Log "server=$Server share=$Share user=$User"

# 1. start capture (filter to the SMB server IP)
& pktmon stop 2>$null | Out-Null
& pktmon filter remove 2>$null | Out-Null
& pktmon filter add -i $Server | Out-Null
& pktmon start --capture --pkt-size 0 -f $etl | Out-Null
Log "pktmon capturing -> $etl"

try {
  # 2. map the share
  & net use "\\$Server\$Share" /user:$User $Password 2>&1 | ForEach-Object { Log $_ }

  $unc = "\\$Server\$Share"
  $target = Get-ChildItem -Path $unc -File -ErrorAction SilentlyContinue | Select-Object -First 1
  if (-not $target) {
    # nothing to read yet — create one so the SD has an owner to resolve
    $target = New-Item -Path (Join-Path $unc "win-acl-probe.txt") -ItemType File -Force
  }
  Log "target file: $($target.FullName)"

  # 3. read the ACL — this is the LSARPC LookupSids path
  Log "`n--- icacls ---"
  & icacls "$($target.FullName)" 2>&1 | ForEach-Object { Log $_ }

  Log "`n--- Get-Acl (Owner + Access) ---"
  $acl = Get-Acl -Path $target.FullName
  Log ("Owner: " + $acl.Owner)
  foreach ($ace in $acl.Access) {
    Log ("ACE: {0,-30} {1,-8} {2}" -f $ace.IdentityReference, $ace.AccessControlType, $ace.FileSystemRights)
  }

  Log "`nPASS criteria: Owner/ACE identities show 'DITTOFS\\<name>', not a raw 'S-1-5-21-...' SID."
}
finally {
  # 4. stop + convert
  & pktmon stop | Out-Null
  Log "`npktmon stopped."
  $e2p = Get-Command etl2pcapng -ErrorAction SilentlyContinue
  if ($e2p) {
    & etl2pcapng $etl $pcap | ForEach-Object { Log $_ }
    Log "pcap -> $pcap"
  } else {
    Log "etl2pcapng not found; raw capture left at $etl"
    Log "  get it: https://github.com/microsoft/etl2pcapng/releases  (or: tshark can read .etl on recent builds)"
  }
  & net use "\\$Server\$Share" /delete 2>$null | Out-Null
}

Log "`nDone. Evidence in $OutDir :"
Get-ChildItem $OutDir | ForEach-Object { Log ("  " + $_.Name) }
Log "Now: open the file's Properties > Security tab in Explorer and screenshot it for #1297."
