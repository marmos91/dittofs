// smb-truncate URL SIZE: sets a file's size with an SMB SET_INFO (FileEndOfFileInformation), what
// a Windows program calling SetEndOfFile sends; smbclient cannot. Logs in as tester.
//   smb-truncate smb://127.0.0.1/test/f 3000000
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <smb2/smb2.h>
#include <smb2/libsmb2.h>

int main(int argc, char **argv) {
    if (argc != 3) {
        fprintf(stderr, "usage: smb-truncate URL SIZE\n");
        return 2;
    }
    struct smb2_context *smb2 = smb2_init_context();
    struct smb2_url *url = smb2_parse_url(smb2, argv[1]);
    smb2_set_security_mode(smb2, SMB2_NEGOTIATE_SIGNING_ENABLED);
    smb2_set_authentication(smb2, 1); // NTLMSSP; libsmb2 tries Kerberos first
    smb2_set_password(smb2, "tester-secret");
    if (url == NULL || smb2_connect_share(smb2, url->server, url->share, "tester") != 0) {
        fprintf(stderr, "smb-truncate: connect: %s\n", smb2_get_error(smb2));
        return 1;
    }
    int ret = smb2_truncate(smb2, url->path, strtoull(argv[2], NULL, 10));
    if (ret != 0) {
        fprintf(stderr, "smb-truncate: %s: %s (%d)\n", url->path, strerror(-ret), ret);
        return 1;
    }
    return 0;
}
