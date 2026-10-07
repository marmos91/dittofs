#!/usr/bin/env bash
# Issue #2922: the group owner of a new file over SMB. bob is created with --uid 2002 --gid 2002 and no group
# memberships. The SMB session takes its GID from the first group that has one and falls back to 1000
# (tester's group here) without reading the user's own GID, so bob's SMB files belong to group 1000.
# Control: the same user over NFS, whose AUTH_SYS credential carries gid 2002.

dfsctl user create --username bob --uid 2002 --gid 2002 --password first-bob-secret --role user
XDG_CONFIG_HOME=/tmp/bob dfsctl login --server http://127.0.0.1:8080 --username bob --password first-bob-secret
XDG_CONFIG_HOME=/tmp/bob dfsctl user change-password --current first-bob-secret --new bob-secret
dfsctl user list -o json | jq -c '.[] | {username, uid, gid, groups}'
dfsctl share create --name /own --metadata md --block-store s3 --default-permission none
dfsctl share permission grant /own --user bob --level read-write

smbclient //127.0.0.1/own -U bob%bob-secret -c 'put /etc/hostname smb.txt'
nfs-cp /etc/hostname 'nfs://127.0.0.1/own/nfs.txt?version=4&uid=2002&gid=2002'
nfs-ls 'nfs://127.0.0.1/own?version=4&uid=2002&gid=2002' | tee /tmp/own.ls

test "$(awk '$6 == "nfs.txt" {print $3 ":" $4}' /tmp/own.ls)" = 2002:2002
test "$(awk '$6 == "smb.txt" {print $3 ":" $4}' /tmp/own.ls)" = 2002:2002
