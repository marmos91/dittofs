#!/usr/bin/env bash
# Issues #2920 and #2921: the recycle bin with two users. The first delete creates #recycle owned by the
# deleting user, mode 0700, and a delete that cannot be recycled is refused, so every other user's delete
# then fails. The share also sets --trash-restrict-empty-to-admin, which nothing reads: dfsctl trash
# empty is admin-only on every share anyway, but over SMB a non-admin can still delete from #recycle.

dfsctl user create --username bob --uid 2002 --gid 2002 --password first-bob-secret --role user
XDG_CONFIG_HOME=/tmp/bob dfsctl login --server http://127.0.0.1:8080 --username bob --password first-bob-secret
XDG_CONFIG_HOME=/tmp/bob dfsctl user change-password --current first-bob-secret --new bob-secret
dfsctl share create --name /shared --metadata md --block-store s3 --default-permission none --enable-trash --trash-restrict-empty-to-admin
dfsctl share permission grant /shared --user tester --level read-write
dfsctl share permission grant /shared --user bob --level read-write

smbclient //127.0.0.1/shared -c 'put /etc/hostname t1.txt'
smbclient //127.0.0.1/shared -U bob%bob-secret -c 'put /etc/hostname b1.txt; put /etc/hostname b2.txt'
smbclient //127.0.0.1/shared -c 'del t1.txt'
nfs-ls 'nfs://127.0.0.1/shared?version=4&uid=1000&gid=1000' | grep recycle
smbclient //127.0.0.1/shared -U bob%bob-secret -c 'del b1.txt' || true
nfs-io unlink 'nfs://127.0.0.1/shared/b2.txt?version=4&uid=2002&gid=2002' || true
smbclient //127.0.0.1/shared -c 'ls "#recycle/*"'
smbclient //127.0.0.1/shared -c 'del "#recycle/t1.txt"' || true
smbclient //127.0.0.1/shared -c 'ls "#recycle/*"' || true
XDG_CONFIG_HOME=/tmp/bob dfsctl trash empty /shared --force && echo emptied >/tmp/empty || echo refused >/tmp/empty
smbclient //127.0.0.1/shared -U bob%bob-secret -c 'ls' | tee /tmp/root.ls

test -z "$(grep 'b1\.txt' /tmp/root.ls)"
test -z "$(grep 'b2\.txt' /tmp/root.ls)"
smbclient //127.0.0.1/shared -c 'ls "#recycle/t1.txt"'
test "$(cat /tmp/empty)" = refused
