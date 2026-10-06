// smb-pwrite URL LOCAL <PLAN: writes in place over SMB through one open handle, the way Windows keeps
// a mounted VHDX open for a whole desktop session. Each PLAN line "OFFSET LENGTH" copies LENGTH bytes
// of LOCAL at OFFSET to the remote file at the same OFFSET; an offset past the end grows the file.
// Logs in as tester.
//   smb-pwrite smb://127.0.0.1/test/f /tmp/f </tmp/plan
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <smb2/smb2.h>
#include <smb2/libsmb2.h>

int main(int argc, char **argv) {
    if (argc != 3) {
        fprintf(stderr, "usage: smb-pwrite URL LOCAL <PLAN\n");
        return 2;
    }
    int local = open(argv[2], O_RDONLY);
    if (local < 0) {
        perror("smb-pwrite: local file");
        return 1;
    }
    struct smb2_context *smb2 = smb2_init_context();
    struct smb2_url *url = smb2_parse_url(smb2, argv[1]);
    smb2_set_security_mode(smb2, SMB2_NEGOTIATE_SIGNING_ENABLED);
    smb2_set_authentication(smb2, 1); // NTLMSSP; libsmb2 tries Kerberos first
    smb2_set_password(smb2, "tester-secret");
    if (url == NULL || smb2_connect_share(smb2, url->server, url->share, "tester") != 0) {
        fprintf(stderr, "smb-pwrite: connect: %s\n", smb2_get_error(smb2));
        return 1;
    }
    struct smb2fh *fh = smb2_open(smb2, url->path, O_RDWR);
    if (fh == NULL) {
        fprintf(stderr, "smb-pwrite: open %s: %s\n", url->path, smb2_get_error(smb2));
        return 1;
    }
    uint32_t most = smb2_get_max_write_size(smb2);
    unsigned long long off, len, writes = 0;
    uint8_t *buf = NULL;
    while (scanf("%llu %llu", &off, &len) == 2) {
        buf = realloc(buf, len);
        if (buf == NULL || pread(local, buf, len, off) != (ssize_t)len) {
            fprintf(stderr, "smb-pwrite: read %llu bytes of the local file at %llu\n", len, off);
            return 1;
        }
        for (unsigned long long done = 0; done < len;) {
            uint32_t n = len - done < most ? len - done : most;
            int ret = smb2_pwrite(smb2, fh, buf + done, n, off + done);
            if (ret <= 0) {
                fprintf(stderr, "smb-pwrite: write at %llu: %s (%d)\n", off + done, smb2_get_error(smb2), ret);
                return 1;
            }
            done += ret;
        }
        writes++;
    }
    if (smb2_close(smb2, fh) != 0) {
        fprintf(stderr, "smb-pwrite: close: %s\n", smb2_get_error(smb2));
        return 1;
    }
    printf("smb-pwrite: %llu writes through one handle\n", writes);
    return 0;
}
