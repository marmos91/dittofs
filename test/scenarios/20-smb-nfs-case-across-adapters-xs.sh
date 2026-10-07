#!/usr/bin/env bash
# One case rule per share, whichever protocol asks: case sensitivity "MUST NOT vary between adapters
# reaching the same share" (RFC 7 §3.3). NFS creates README and then readme, with different
# contents, and SMB then creates Readme. SMB must list what NFS lists, and each name must give the
# same bytes over both. The rule must be one of two: case-sensitive (two names from NFS, then a third
# from SMB) or case-insensitive (one name, then no new one).
# Fails today. NFS keeps README and readme apart, and both protocols list and read them alike. But
# SMB's create of Readme silently overwrites README's contents: the share is case-sensitive over NFS
# and case-insensitive over SMB.
NFS='nfs://127.0.0.1/test'
OPT='?version=4&uid=1000&gid=1000'

echo upper >/tmp/upper.txt
echo lower >/tmp/lower.txt
echo mixed >/tmp/mixed.txt
nfs-cp /tmp/upper.txt "$NFS/README$OPT"
nfs-cp /tmp/lower.txt "$NFS/readme$OPT" || echo "exit $?" >/tmp/lower.rc

# The names each protocol lists, and what each name reads as
nfs-ls "$NFS$OPT" | awk '{print $NF}' | sort | tee /tmp/nfs1.names
smbclient //127.0.0.1/test -c ls | awk '/^  / && $1 != "." && $1 != ".." {print $1}' | sort | tee /tmp/smb1.names
for n in README readme; do nfs-cat "$NFS/$n$OPT" >"/tmp/nfs-$n.txt" || true; smbclient //127.0.0.1/test -c "get $n /tmp/smb-$n.txt" || true; done

# An SMB create of a third spelling
smbclient //127.0.0.1/test -c 'lcd /tmp; put mixed.txt Readme' || echo "exit $?" >/tmp/mixed.rc
nfs-ls "$NFS$OPT" | awk '{print $NF}' | sort | tee /tmp/nfs2.names
smbclient //127.0.0.1/test -c ls | awk '/^  / && $1 != "." && $1 != ".." {print $1}' | sort | tee /tmp/smb2.names
for n in README readme Readme; do echo "$n: $(nfs-cat "$NFS/$n$OPT" 2>/dev/null)"; done

# Outcomes first, then the checks
cat /tmp/*.rc 2>/dev/null || true
for n in README readme; do echo "$n: NFS $(cat "/tmp/nfs-$n.txt" 2>/dev/null), SMB $(cat "/tmp/smb-$n.txt" 2>/dev/null)"; done
cmp /tmp/nfs1.names /tmp/smb1.names
cmp /tmp/nfs-README.txt /tmp/smb-README.txt
cmp /tmp/nfs-readme.txt /tmp/smb-readme.txt
cmp /tmp/nfs2.names /tmp/smb2.names
test "$(cat /tmp/nfs1.names /tmp/nfs2.names | wc -l)" -eq 5 || test "$(cat /tmp/nfs1.names /tmp/nfs2.names | wc -l)" -eq 2
