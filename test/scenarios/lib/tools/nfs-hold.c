// nfs-hold URL: opens a file over NFS and keeps it open. Each line on stdin prints the whole file
// to stdout; the end of stdin closes it. With a FIFO, a scenario holds the file across commands:
//   mkfifo /tmp/hold; nfs-hold "$URL" </tmp/hold >/tmp/held & exec 3>/tmp/hold
//   ... echo >&3 (read it now) ... exec 3>&- (close it)
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <sys/time.h>
#include <nfsc/libnfs.h>

int main(int argc, char **argv) {
    static char buf[1 << 20];
    char line[64];
    struct nfsfh *fh;
    if (argc != 2) {
        fprintf(stderr, "usage: nfs-hold URL\n");
        return 2;
    }
    struct nfs_context *nfs = nfs_init_context();
    struct nfs_url *url = nfs_parse_url_full(nfs, argv[1]);
    if (url == NULL || nfs_mount(nfs, url->server, url->path) != 0 ||
        nfs_open(nfs, url->file, O_RDONLY, &fh) != 0) {
        fprintf(stderr, "nfs-hold: %s\n", nfs_get_error(nfs));
        return 1;
    }
    while (fgets(line, sizeof line, stdin) != NULL) {
        uint64_t at = 0;
        int n;
        while ((n = nfs_pread(nfs, fh, at, sizeof buf, buf)) > 0) {
            fwrite(buf, 1, n, stdout);
            at += n;
        }
        if (n < 0) {
            fprintf(stderr, "nfs-hold: read: %s\n", nfs_get_error(nfs));
            return 1;
        }
        fflush(stdout);
    }
    nfs_close(nfs, fh);
    return 0;
}
