# SMB SECURITY_DESCRIPTOR reference + validation

`reference.pcap` / `reference.skel` — an SMB2 `QUERY_INFO` (info class
`SMB2_0_INFO_SECURITY`) exchange against a **Samba 4.19 file server** (the
reference SMB implementation), read with `smbcacls`. `dittofs-sample.pcap` is
the same `smbcacls` query against DittoFS's SMB server.

## Endpoints

Both servers ran on port 445 (sequentially) so `smbcacls` and `tshark` need no
special flags. Reference = `smbd` (standalone, share `smbref`, local user
`testuser`); candidate = DittoFS SMB on the same port/share/user.

## How it was produced

```bash
# Reference (Samba on :445):
tcpdump -i any -s0 -w smb-sd-ref.pcap port 445 &
smbcacls //127.0.0.1/smbref file.txt -U testuser%PW          # SMB2 QUERY_INFO security

# Candidate (DittoFS on :445, after stopping smbd):
tcpdump -i any -s0 -w smb-sd-dfs.pcap port 445 &
smbcacls //127.0.0.1/smbref file.txt -U testuser%PW

tshark -r smb-sd-ref.pcap -T pdml | normalize.py --proto smb2 > reference.skel
../pcap-diff.sh --reference reference.pcap --candidate dittofs-sample.pcap --proto smb2
```

## Result — SD structure matches, two divergences found

Comparing only the security-descriptor fields (`nt.*`: owner/group SID, ACL
revision, ACE type/flags/access-mask, etc.), DittoFS and Samba are **structurally
identical — 78/78 fields present in both**. DittoFS serves a faithful
SECURITY_DESCRIPTOR on the wire. (The whole-`smb2` skeleton still differs on
session/negotiate/GUID/op-count noise — that is expected; the SD comparison is
the signal.)

The harness surfaced two concrete divergences in the SD *values*:

1. **`SE_DACL_PROTECTED` (0x1000) not set.** Samba returns control `0x9004`
   (Self-Relative + DACL-Protected + DACL-Present); DittoFS returns `0x8004`
   (Self-Relative + DACL-Present). DittoFS omits the DACL-Protected bit.
2. **No LSARPC SID→name resolution.** `smbcacls` resolves owner/ACE SIDs to
   names against Samba (`DITTOFS-SMBSD\testuser`); against DittoFS it gets
   `DCERPC_NCA_S_UNKNOWN_IF` and falls back to raw SIDs
   (`S-1-5-21-…`). DittoFS does not implement the LSARPC pipe `smbcacls` calls
   to translate SIDs.

Both are tracked as a follow-up (see the issue referenced from #1237). Neither
blocks the SD wire contract — they are fidelity gaps the corpus is designed to
catch.

## Update — both divergences fixed (#1291)

1. **`SE_DACL_PROTECTED`** is now set on DittoFS's synthesized default DACL
   (`acl.SynthesizeWindowsDefault`), so a file with no explicit/inherited ACL
   reports control `0x9004` to match standalone Samba.
2. **LSARPC SID→name** now works: the root cause was *not* a missing pipe (the
   LSA interface and `LsarLookupSids2/3` were already implemented). `smbcacls`
   calls the legacy `LsarOpenPolicy` (opnum 6) *before* the lookup, which
   DittoFS did not handle — it returned a fault that the client read as
   `DCERPC_NCA_S_UNKNOWN_IF` and gave up. Opnum 6 is now routed to the same
   policy-handle handler as `LsarOpenPolicy2` (opnum 44).

`dittofs-sample.pcap` above predates the fix; regenerate it on the next live
capture against a patched DittoFS to refresh the corpus.

## Windows GUI evidence (#1297)

The `smbcacls`/`rpcclient` captures above prove SID→name on Linux. `#1297` asks
for the same against a **real Windows client** — Explorer's Properties > Security
tab (and `icacls`) resolving DittoFS SIDs to `DITTOFS\<name>` via
`LsarLookupSids2/3` (opnum 57/76). Run `windows-gui-capture.ps1` inside an RDP
session on the Windows test VM: it maps the share, captures the LSARPC exchange
with built-in `pktmon`, and dumps the resolved owner/ACE names via `icacls` +
`Get-Acl`. Open the same file's Security tab and screenshot it for the
human-facing proof.

**Dependency:** authenticating *as* the AD user `alice` over SMB from a
non-domain-joined Windows box needs NTLM passthrough (#1314 / PR #1344) deployed
on the server, or the VM domain-joined to `DITTOFS.AD`. The SID→name resolution
under test is independent of who authenticates, but a session must first be
established — so this capture is gated on #1344 landing on the demo (or a
domain-join). Until then, the Linux `smbcacls`/`rpcclient` evidence stands.
