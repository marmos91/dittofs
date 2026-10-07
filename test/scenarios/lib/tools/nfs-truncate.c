// nfs-truncate URL SIZE: sets a file's size over NFS; libnfs-utils has none to a non-zero size.
//   nfs-truncate 'nfs://127.0.0.1/test/f?version=4&uid=1000&gid=1000' 3000000
#include <stdio.h>
#include <stdlib.h>
#include <sys/time.h>
#include <nfsc/libnfs.h>

int main(int argc, char **argv) {
    if (argc != 3) {
        fprintf(stderr, "usage: nfs-truncate URL SIZE\n");
        return 2;
    }
    struct nfs_context *nfs = nfs_init_context();
    struct nfs_url *url = nfs_parse_url_full(nfs, argv[1]);
    if (url == NULL || nfs_mount(nfs, url->server, url->path) != 0 ||
        nfs_truncate(nfs, url->file, strtoull(argv[2], NULL, 10)) != 0) {
        fprintf(stderr, "nfs-truncate: %s\n", nfs_get_error(nfs));
        return 1;
    }
    return 0;
}
