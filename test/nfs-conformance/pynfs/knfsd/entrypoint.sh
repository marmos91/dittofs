#!/bin/bash
# Export /export over NFSv4 only.
#
# `insecure` is required: pynfs connects from an unprivileged source port, which
# knfsd rejects by default. `fsid=0` makes this the NFSv4 pseudo-root, so the
# path pynfs asks for is "/". UDP is off because NFSv4 is TCP-only.
set -e

chmod 777 /export
echo "/export *(rw,insecure,no_root_squash,no_subtree_check,fsid=0)" > /etc/exports

exportfs -ra
exportfs -v

# Match run-pynfs.sh's lease time. Lease-expiry tests sleep for a full lease, so
# leaving this at the 90s default would compare a slow knfsd run against a fast
# DittoFS one, and any test whose outcome depends on expiry timing would be
# measured under different conditions on the two servers.
echo "${NFSD_LEASE_TIME:-30}" > /proc/fs/nfsd/nfsv4leasetime

rpc.nfsd --no-udp 2049
exec rpc.mountd --no-udp --foreground
