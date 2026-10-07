#!/usr/bin/env bash
# Permission is checked on every call, not only at connect (RFC 7 §7.4; PE-01, C-06). An SMB session
# (smbclient reading commands from a FIFO) and an NFSv4 open (nfs-hold) are held across a revoke of
# tester's grant: a read and a write on the open SMB session, a read on the open NFS file, and new
# connections must all be refused. With a read grant, reads succeed and writes are refused on both.
# nfs-hold only reads and exits on an error, so NFS writes and the read after the grant use new
# connections. smbclient runs under script, for a terminal: from a pipe it reads all of stdin before it
# runs a command. Its "!touch" marks when it has worked through the commands before it. A revoke
# disconnects the open SMB tree (NT_STATUS_NETWORK_NAME_DELETED), so that session cannot read after the
# grant either; the read after the grant is checked on new connections.

N='?version=4&uid=1000&gid=1000'
mkdir /tmp/w /tmp/r
head -c 1M /dev/urandom >/tmp/f
head -c 1M /dev/urandom >/tmp/w/w
smbclient //127.0.0.1/test -c 'lcd /tmp; put f'
mkfifo /tmp/smb /tmp/hold
script -qfec 'smbclient //127.0.0.1/test' /dev/null </tmp/smb >/tmp/smb.out 2>&1 &
S=$!
exec 3>/tmp/smb
nfs-hold "nfs://127.0.0.1/test/f$N" </tmp/hold >/tmp/held &
H=$!
exec 4>/tmp/hold
echo 'lcd /tmp/r' >&3
echo 'get f before' >&3
echo '!touch /tmp/m1' >&3
for _ in $(seq 1 100); do test -e /tmp/m1 && break; sleep 0.1; done
cmp /tmp/r/before /tmp/f

dfsctl share permission revoke /test --user tester
echo 'get f revoked' >&3
echo 'put /tmp/w/w w-revoked' >&3
echo '!touch /tmp/m2' >&3
echo >&4
exec 4>&-
for _ in $(seq 1 100); do test -e /tmp/m2 && break; sleep 0.1; done
wait "$H" && NH='read' || NH=refused
smbclient //127.0.0.1/test -c 'lcd /tmp/r; get f revoked-new' && SN='read' || SN=refused
nfs-cp "nfs://127.0.0.1/test/f$N" /tmp/r/nfs-revoked && NN='read' || NN=refused
nfs-cp /tmp/w/w "nfs://127.0.0.1/test/n-revoked$N" && NW=written || NW=refused

dfsctl share permission grant /test --user tester --level read
echo 'get f granted' >&3
echo 'put /tmp/w/w w-granted' >&3
echo '!touch /tmp/m3' >&3
echo quit >&3
exec 3>&-
wait "$S" || true
smbclient //127.0.0.1/test -c 'lcd /tmp/r; get f granted-new' && SGN='read' || SGN=refused
smbclient //127.0.0.1/test -c 'lcd /tmp/w; put w w-granted-new' && SGW=written || SGW=refused
nfs-cp "nfs://127.0.0.1/test/f$N" /tmp/r/nfs-granted && NGR='read' || NGR=refused
nfs-cp /tmp/w/w "nfs://127.0.0.1/test/n-granted$N" && NGW=written || NGW=refused
dfsctl share permission grant /test --user tester --level read-write
cat /tmp/smb.out
smbclient //127.0.0.1/test -c 'ls' | tee /tmp/ls
command ls -l /tmp/r
echo "revoked: smb open read $(test -s /tmp/r/revoked && echo read || echo refused), smb open write $(grep -q w-revoked /tmp/ls && echo written || echo refused), nfs open read $NH, smb new read $SN, nfs new read $NN, nfs new write $NW"
echo "read: smb open read $(test -s /tmp/r/granted && echo read || echo refused), smb open write $(grep -q 'w-granted ' /tmp/ls && echo written || echo refused), smb new read $SGN, smb new write $SGW, nfs new read $NGR, nfs new write $NGW"

test ! -s /tmp/r/revoked
! grep -q w-revoked /tmp/ls
test "$NH/$SN/$NN/$NW" = refused/refused/refused/refused
test ! -s /tmp/r/granted
! grep -q 'w-granted ' /tmp/ls
test "$SGN/$SGW/$NGR/$NGW" = read/refused/read/refused
