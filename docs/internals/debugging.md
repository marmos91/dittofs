# Debugging Protocol Interop (SMB / NFS)

When a conformance test fails against DittoFS but passes against Samba, Windows, or a Linux
NFS server, **go straight to a byte-level pcap diff.** Reading source alone misses the things
that actually break interop: SPNEGO token layout, NTLMSSP flag ordering, SMB2 error body
format (MS-SMB2 §2.2.2), `mechListMIC` presence, nonce derivation, and similar wire-level
details.

The reliable method is: run the same test against a reference server and against DittoFS,
capture both with `tcpdump`, and diff the decoded packets with `tshark`.

## Reference servers

| Image | Use |
|-------|-----|
| `dperson/samba:latest` | quick SMB reference |
| `quay.io/samba/samba:latest` | full SMB reference (DC-capable) |
| `erichough/nfs-server` | NFSv3 / NFSv4 reference |

On Apple Silicon, force `--platform linux/amd64` on every container.

## Workflow

```bash
# 1. Reference SMB server on port 11445 (DittoFS owns 12445 — keep them distinct)
docker run --rm -d --name ref-server --platform linux/amd64 -p 11445:445 \
  -v /tmp/ref-share:/share \
  dperson/samba:latest \
  -u "testuser;TestPassword01!" \
  -s "share;/share;yes;no;no;testuser"

# 2. Capture on the reference server (capture DittoFS traffic similarly on its port)
docker exec -u root -d ref-server tcpdump -i any -w /tmp/ref.pcap -s 0 port 445

# 3. Run the same test against each server

# 4. Diff the decoded packets with tshark.
#    DittoFS runs SMB on a non-standard port, so tshark needs the NBSS dissector hint.
docker run --rm -v /tmp:/tmp --platform linux/amd64 nicolaka/netshoot \
  tshark -r /tmp/ref.pcap -V \
  -Y "smb2.cmd==1 and smb2.nt_status==0 and smb2.flags.response==1" \
  -c 1 2>/dev/null | grep -iE "spnego|negtoken|mechListMIC|supportedMech"

docker run --rm -v /tmp:/tmp --platform linux/amd64 nicolaka/netshoot \
  tshark -r /tmp/dittofs.pcap -d tcp.port==12445,nbss -V \
  -Y "smb2.cmd==1 and smb2.nt_status==0 and smb2.flags.response==1" \
  -c 1 2>/dev/null | grep -iE "spnego|negtoken|mechListMIC|supportedMech"
```

## Pitfalls

- Force `--platform linux/amd64` on Apple Silicon.
- `tshark` needs `-d tcp.port==N,nbss` for SMB on any port other than 445.
- Always `docker compose down -v` between runs so stale state doesn't leak.
- Keep both pcaps for post-mortem comparison.

## Reference implementations

When in doubt about protocol correctness, compare against the canonical implementations — our
behavior sometimes drifts in subtle ways:

- NFS: <https://github.com/torvalds/linux/tree/master/fs/nfs>
- SMB: <https://github.com/samba-team/samba>
