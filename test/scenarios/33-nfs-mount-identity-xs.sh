#!/usr/bin/env bash
# Who may mount a share with default permission none, over NFSv3 and NFSv4. RFC 7 §7.4 (draft):
# Authorize evaluates the share grant on every call, "not only at mount or tree connect", and "Root
# takes the identity, and is authorised against the grant like every other call". An identity with
# no grant is therefore refused at mount, on every protocol. On setup's /test only tester has a
# grant; a mount at boot runs as root, which root squash makes nobody, and neither has one.
# NFSv4 follows the RFC: LOOKUP into the share applies the share-permission gate
# (internal/adapter/nfs/v4/handlers/lookup.go). NFSv4.1, which kernel mounts use, is out of
# libnfs's reach (5.0.2 sends minor version 0 only); a v4.1 compound reaches the same LOOKUP
# through the v4.0 table (handlers/compound.go). Neither version logs its refusal.
# Fails today: NFSv3's MNT hands the share's root handle to root and nobody, and refuses only their
# later calls (NFS3ERR_ACCES on READDIRPLUS).

smbclient //127.0.0.1/test -c 'put /etc/hostname f.txt'
for v in 3 4; do for u in 0 65534 1000; do nfs-ls "nfs://127.0.0.1/test?version=$v&uid=$u&gid=$u&nfsport=2049&mountport=2049" >/tmp/v$v-$u.out 2>&1 || true; done; done

# Outcomes first, then the checks: tester reads over both versions; root and nobody read nothing
# and are refused at mount, over both
for f in /tmp/v*.out; do echo "== $f: $(tr '\n' ' ' <"$f")"; done
grep -q 'f\.txt' /tmp/v3-1000.out
grep -q 'f\.txt' /tmp/v4-1000.out
test -z "$(grep 'f\.txt' /tmp/v3-0.out)"
test -z "$(grep 'f\.txt' /tmp/v4-0.out)"
test -z "$(grep 'f\.txt' /tmp/v3-65534.out)"
test -z "$(grep 'f\.txt' /tmp/v4-65534.out)"
grep -q 'Failed to mount' /tmp/v4-0.out
grep -q 'Failed to mount' /tmp/v4-65534.out
grep -q 'Failed to mount' /tmp/v3-0.out
grep -q 'Failed to mount' /tmp/v3-65534.out
