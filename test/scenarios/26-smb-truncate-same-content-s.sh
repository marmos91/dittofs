#!/usr/bin/env bash
# The SMB side of B-01, as 25-smb-nfs-truncate-same-content-s.sh shows it over NFS. Every truncate here is
# an SMB SET_INFO FileEndOfFileInformation, what a Windows program calling SetEndOfFile sends.
# a.bin and b.bin are the same 8 MiB, so dedup gives them one chunk list. Truncated to 3 MB, both keep
# the same chunks and get the same ObjectID, which every metadata store requires to be unique: the
# first truncate succeeds, the second must too.
# Fails today: the second truncate answers STATUS_INSUFFICIENT_RESOURCES (the store's conflict,
# mapped as if it were a passing race) on every try, and b.bin stays 8 MiB. Control: c.bin, the same
# content cut to 6 MB, keeps more chunks, so its ObjectID differs and its truncate succeeds.

# smb-truncate PATH SIZE: smbclient cannot set a file's size; libsmb2's smb2_truncate sends the
# SET_INFO. libsmb2 tries Kerberos first, so NTLM is forced.
apt-get install -y -qq --no-install-recommends libsmb2-dev >/dev/null
cat >/tmp/smb-truncate.c <<'EOF'
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <smb2/smb2.h>
#include <smb2/libsmb2.h>
int main(int argc, char **argv) {
    struct smb2_context *smb2 = smb2_init_context();
    smb2_set_security_mode(smb2, SMB2_NEGOTIATE_SIGNING_ENABLED);
    smb2_set_authentication(smb2, 1); /* SMB2_SEC_NTLMSSP, not in the installed headers */
    smb2_set_password(smb2, "tester-secret");
    if (smb2_connect_share(smb2, "127.0.0.1", "test", "tester") != 0) {
        fprintf(stderr, "smb-truncate: connect: %s\n", smb2_get_error(smb2));
        return 1;
    }
    int ret = smb2_truncate(smb2, argv[1], strtoull(argv[2], NULL, 10));
    if (ret != 0) {
        fprintf(stderr, "smb-truncate: %s: %s (%d)\n", argv[1], strerror(-ret), ret);
        return 1;
    }
    return 0;
}
EOF
tcc -o /tmp/smb-truncate /tmp/smb-truncate.c -lsmb2

head -c 8M /dev/urandom >/tmp/a.bin
cp /tmp/a.bin /tmp/b.bin
cp /tmp/a.bin /tmp/c.bin
head -c 3000000 /tmp/a.bin >/tmp/want.bin
head -c 6000000 /tmp/a.bin >/tmp/want-c.bin
smbclient //127.0.0.1/test -c 'lcd /tmp; put a.bin'
dfsctl system drain-uploads
smbclient //127.0.0.1/test -c 'lcd /tmp; put b.bin; put c.bin'
dfsctl system drain-uploads

/tmp/smb-truncate a.bin 3000000
for _ in $(seq 1 10); do /tmp/smb-truncate b.bin 3000000 && break; sleep 1; done
/tmp/smb-truncate c.bin 6000000
mkdir /tmp/back
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get a.bin; get b.bin; get c.bin'

# Outcomes first, then the checks
stat -c '%n %s' /tmp/back/a.bin /tmp/back/b.bin /tmp/back/c.bin
cmp /tmp/want-c.bin /tmp/back/c.bin
cmp /tmp/want.bin /tmp/back/a.bin
cmp /tmp/want.bin /tmp/back/b.bin
