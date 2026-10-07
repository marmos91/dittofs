// smb-open URL ACCESS SHARE: opens an existing file over SMB with the given desired access and
// share access (numbers, as in MS-SMB2: 0x1 is FILE_READ_DATA, 0x7 is FILE_SHARE_READ, WRITE and
// DELETE), and prints the open's NT status. On success it keeps the file open until stdin ends;
// then the exit status is 0, otherwise 1. Logs in as tester. A stream is f:s.
//   smb-open smb://127.0.0.1/test/d/f 0x3 0x1 </tmp/hold &
#include <poll.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/time.h>
#include <smb2/smb2.h>
#include <smb2/libsmb2.h>
#include <smb2/libsmb2-raw.h>

static int done;
static uint32_t status;

static void opened(struct smb2_context *smb2, int st, void *reply, void *data) {
    status = st;
    done = 1;
}

int main(int argc, char **argv) {
    char name[1024], line[64];
    struct smb2_create_request req = {0};
    if (argc != 4) {
        fprintf(stderr, "usage: smb-open URL ACCESS SHARE\n");
        return 2;
    }
    struct smb2_context *smb2 = smb2_init_context();
    struct smb2_url *url = smb2_parse_url(smb2, argv[1]);
    smb2_set_security_mode(smb2, SMB2_NEGOTIATE_SIGNING_ENABLED);
    smb2_set_authentication(smb2, 1); // NTLMSSP; libsmb2 tries Kerberos first
    smb2_set_password(smb2, "tester-secret");
    if (url == NULL || smb2_connect_share(smb2, url->server, url->share, "tester") != 0) {
        fprintf(stderr, "smb-open: connect: %s\n", smb2_get_error(smb2));
        return 1;
    }
    // A CREATE names the file relative to the share, with backslashes.
    snprintf(name, sizeof name, "%s", url->path ? url->path : "");
    for (char *c = name; *c; c++)
        if (*c == '/')
            *c = '\\';
    req.requested_oplock_level = SMB2_OPLOCK_LEVEL_NONE;
    req.impersonation_level = SMB2_IMPERSONATION_IMPERSONATION;
    req.desired_access = strtoul(argv[2], NULL, 0);
    req.share_access = strtoul(argv[3], NULL, 0);
    req.create_disposition = SMB2_FILE_OPEN;
    req.name = name;
    smb2_queue_pdu(smb2, smb2_cmd_create_async(smb2, &req, opened, NULL));
    while (!done) {
        struct pollfd pfd = {.fd = smb2_get_fd(smb2), .events = smb2_which_events(smb2)};
        if (poll(&pfd, 1, 1000) < 0 || smb2_service(smb2, pfd.revents) < 0) {
            fprintf(stderr, "smb-open: %s\n", smb2_get_error(smb2));
            return 1;
        }
    }
    printf("%s\n", nterror_to_str(status));
    fflush(stdout);
    if (status != 0)
        return 1;
    while (fgets(line, sizeof line, stdin) != NULL) {
    }
    return 0;
}
