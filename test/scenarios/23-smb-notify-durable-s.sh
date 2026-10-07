#!/usr/bin/env bash
# Change notify and durable handles over SMB (SMB-08, SMB-07; RFC 14 §2.5, §8). A watch on d (smbclient
# notify) must report what happens in d and below: an add, a modify, a rename (old and new name), a
# subdirectory's file and a remove, made over SMB, then a create and a remove made over NFS, since a
# watch hears about changes under its directory whatever protocol makes them (RFC 14 §2.5).
# smbtorture's smb2.durable-open and smb2.durable-v2-open suites (samba-testsuite, installed here) must
# pass, except durable-open.delete_on_close2, which Samba's own test run skips.
# Fails today at the NFS events: SMB watches do not see NFS-side changes, a gap the comment on
# deletePendingDirs in internal/adapter/smb/changenotify/registry.go names. The SMB events and both
# durable suites pass. It takes about 1 min.

N='?version=4&uid=1000&gid=1000'
T=(//127.0.0.1/test -U tester%tester-secret --option='client min protocol=SMB2_02' --option='client max protocol=SMB3' --option='torture:smbd=false')
apt-get install -y -qq --no-install-recommends samba-testsuite >/dev/null

smbclient //127.0.0.1/test -c 'mkdir d'
stdbuf -oL smbclient //127.0.0.1/test -c 'notify d' >/tmp/notify.txt 2>&1 &
W=$!
sleep 2
smbclient //127.0.0.1/test -c 'cd d; put /etc/hostname a.txt; rename a.txt b.txt; mkdir sub; put /etc/hostname sub/c.txt; del b.txt'
nfs-cp /etc/hostname "nfs://127.0.0.1/test/d/n.txt$N"
nfs-io unlink "nfs://127.0.0.1/test/d/n.txt$N"
sleep 2
kill "$W"
cat /tmp/notify.txt

smbtorture "${T[@]}" smb2.durable-open >/tmp/durable.txt 2>&1 || true
smbtorture "${T[@]}" smb2.durable-v2-open >>/tmp/durable.txt 2>&1 || true
grep -E '^(failure|error|skip):' /tmp/durable.txt | cut -c1-120 || true
grep -c '^success:' /tmp/durable.txt || true

grep -qx '0001 a.txt' /tmp/notify.txt
grep -qx '0003 a.txt' /tmp/notify.txt
grep -qx '0004 a.txt' /tmp/notify.txt
grep -qx '0005 b.txt' /tmp/notify.txt
grep -qx '0001 sub/c.txt' /tmp/notify.txt
grep -qx '0002 b.txt' /tmp/notify.txt
test -z "$(grep -E '^(failure|error):' /tmp/durable.txt | grep -v ' delete_on_close2 ')"
test "$(grep -c '^success:' /tmp/durable.txt)" -ge 50
grep -qx '0001 n.txt' /tmp/notify.txt
grep -qx '0002 n.txt' /tmp/notify.txt
