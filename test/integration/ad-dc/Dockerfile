# Samba Active Directory Domain Controller fixture.
#
# Unlike the MIT KDC in test/integration/kerberos (auth only, no PAC), a Samba
# AD-DC issues Kerberos tickets carrying a Microsoft PAC (MS-PAC) with the
# user's transitive group SIDs — exactly what AD-1 needs to validate PAC
# decoding (DecodePAC) end to end against a real domain controller.
#
# The container provisions a fresh domain on first start via entrypoint.sh,
# creates test users + groups (some with RFC2307 uidNumber/gidNumber, some
# without, to exercise the AD-3 idmap fallback later), and exports a service
# keytab. It runs linux/amd64 so it works on Apple Silicon under emulation,
# mirroring the other docker harnesses.
FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive

# Retry apt-get to handle transient network/mirror failures (exit code 100).
# See: https://forums.docker.com/t/docker-build-returns-exit-code-100/119303
#
# samba-ad-dc pulls in the AD-DC role (provisioning + KDC + LDAP). winbind and
# krb5-user give us kadmin-style tooling and samba-tool dependencies. ldb-tools
# (ldbadd/ldbmodify) lets entrypoint.sh stamp RFC2307 attributes directly.
RUN set -eu; \
    for i in 1 2 3; do \
        apt-get update \
          && apt-get install -y --no-install-recommends \
               samba samba-ad-dc samba-common-bin winbind \
               krb5-user ldb-tools ldap-utils procps \
          && rm -rf /var/lib/apt/lists/* \
          && exit 0; \
        echo "apt-get attempt $i failed, retrying in 5s..." >&2; \
        sleep 5; \
    done; \
    echo "ERROR: failed to install samba packages after 3 attempts" >&2; \
    exit 1

COPY --chmod=0755 entrypoint.sh /entrypoint.sh

# 88 = Kerberos KDC, 389 = LDAP, 464 = kpasswd. The test maps 88 to a random
# host port for the Go Kerberos client to reach.
EXPOSE 88/tcp 88/udp 389/tcp 464/tcp 464/udp

ENTRYPOINT ["/entrypoint.sh"]
