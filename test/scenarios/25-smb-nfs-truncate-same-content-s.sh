#!/usr/bin/env bash
# Truncating two files that hold the same content (B-01). a.bin and b.bin are the same 8 MiB, so
# dedup gives them one chunk list. A truncate that drops chunks records an ObjectID (the hash over the
# remaining chunk list), and every metadata store requires ObjectIDs to be unique. Truncated to the
# same size, both files would get the same ObjectID: the first truncate succeeds, the second must too.
# The ObjectID follows the chunks kept, not the exact size, so any size cutting inside the same chunk
# collides as well. Fails today: the second truncate answers NFS4ERR_DELAY and keeps doing so, and
# b.bin stays 8 MiB. Control: c.bin, the same content cut to 6 MB, keeps more chunks, so its
# ObjectID differs and its truncate succeeds.
NFS='nfs://127.0.0.1/test'
OPT='?version=4&uid=1000&gid=1000'

# nfs-truncate URL SIZE: libnfs-utils has no truncate to a non-zero size (nfs-io trunc goes to 0)
cat >/tmp/nfs-truncate.c <<'EOF'
#include <sys/time.h>
#include <nfsc/libnfs.h>
#include <stdio.h>
#include <stdlib.h>
int main(int argc, char **argv) {
    struct nfs_context *nfs = nfs_init_context();
    struct nfs_url *url = nfs_parse_url_full(nfs, argv[1]);
    if (url == NULL || nfs_mount(nfs, url->server, url->path) != 0 ||
        nfs_truncate(nfs, url->file, strtoull(argv[2], NULL, 10)) != 0) {
        fprintf(stderr, "nfs-truncate: %s\n", nfs_get_error(nfs));
        return 1;
    }
    return 0;
}
EOF
tcc -o /tmp/nfs-truncate /tmp/nfs-truncate.c -lnfs

head -c 8M /dev/urandom >/tmp/a.bin
cp /tmp/a.bin /tmp/b.bin
cp /tmp/a.bin /tmp/c.bin
head -c 3000000 /tmp/a.bin >/tmp/want.bin
head -c 6000000 /tmp/a.bin >/tmp/want-c.bin
smbclient //127.0.0.1/test -c 'lcd /tmp; put a.bin'
dfsctl system drain-uploads
smbclient //127.0.0.1/test -c 'lcd /tmp; put b.bin; put c.bin'
dfsctl system drain-uploads

/tmp/nfs-truncate "$NFS/a.bin$OPT" 3000000
for _ in $(seq 1 10); do /tmp/nfs-truncate "$NFS/b.bin$OPT" 3000000 && break; sleep 1; done
/tmp/nfs-truncate "$NFS/c.bin$OPT" 6000000
mkdir /tmp/back
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get a.bin; get b.bin; get c.bin'

# Outcomes first, then the checks
stat -c '%n %s' /tmp/back/a.bin /tmp/back/b.bin /tmp/back/c.bin
cmp /tmp/want-c.bin /tmp/back/c.bin
cmp /tmp/want.bin /tmp/back/a.bin
cmp /tmp/want.bin /tmp/back/b.bin
