#!/bin/bash
# Samba Active Directory Domain Controller bootstrap for AD-1 PAC testing.
#
# Provisions a fresh AD domain on first start, then creates test users and
# groups that exercise the Kerberos PAC group-SID path:
#
#   Users:
#     alice  -- member of devs (nested under engineering) + RFC2307 attrs set
#     bob    -- member of engineering only, NO RFC2307 attrs (RID-fallback case)
#
#   Groups:
#     engineering -- top-level group (group-level RFC2307 gidNumber is left to
#                    AD-3, which is the consumer; AD-1 only needs membership)
#     devs        -- nested member of engineering (so alice's PAC carries BOTH
#                    devs and engineering SIDs: AD resolves nesting at the DC)
#
# alice's ticket therefore carries a PAC whose GroupMembershipSIDs include the
# devs and engineering domain group SIDs. That is the property AD-1's test
# asserts: PAC group SIDs flow into the Kerberos AuthResult.
#
# A service keytab for the DittoFS SMB/NFS principals is exported to the shared
# /keytabs volume so the dittofs server (or a Go test) can authenticate tickets.
#
# RFC2307 attrs vs RID fallback: alice gets uidNumber/gidNumber (the idmap_ad
# path AD-3 will read over LDAP); bob gets none (the idmap_rid algorithmic-
# fallback path). AD-1 does not consume these user attrs — they are provisioned
# now so the AD-2/AD-3 PRs gate against the same fixture. Group-level gidNumbers
# are AD-3's concern and are stamped there when that path is built.

set -euo pipefail

REALM="${AD_REALM:-DITTOFS.AD}"
DOMAIN="${AD_DOMAIN:-DITTOFS}"
ADMIN_PASSWORD="${AD_ADMIN_PASSWORD:-Passw0rd!2024}"
KEYTAB_DIR="${KEYTAB_DIR:-/keytabs}"

# Service principals the DittoFS server presents to clients.
SMB_SPN="${SMB_SPN:-cifs/dittofs.${REALM,,}}"
NFS_SPN="${NFS_SPN:-nfs/dittofs.${REALM,,}}"

# Test users + groups.
USER_ALICE="${USER_ALICE:-alice}"
USER_BOB="${USER_BOB:-bob}"
USER_PASSWORD="${USER_PASSWORD:-TestPassword01!}"

# UID/GID the dittofs container runs as; the exported keytab must be readable
# there. Defaults match the smb-conformance harness.
DITTOFS_UID="${DITTOFS_UID:-65532}"
DITTOFS_GID="${DITTOFS_GID:-65532}"

log() { echo "[AD-DC] $*"; }

mkdir -p "$KEYTAB_DIR"

# samba-tool domain provision is destructive and one-shot. Use the presence of
# the provisioned config as the "already initialised" sentinel so a container
# restart re-uses the existing domain database rather than re-provisioning.
if [ ! -f /var/lib/samba/.provisioned ]; then
    log "Provisioning AD domain $DOMAIN / realm $REALM"

    # Remove any stub smb.conf so provision writes a clean AD-DC config.
    rm -f /etc/samba/smb.conf

    # Provision the domain controller. SAMBA_INTERNAL DNS keeps the fixture
    # self-contained (no bind9 dependency). --use-rfc2307 enables the RFC2307
    # POSIX attribute schema so we can stamp uidNumber/gidNumber on some objects.
    #
    # The DC functional level is intentionally left at samba's default: the
    # "ad dc functional level" smb.conf parameter (and a fixed 2016 level) is not
    # accepted across all samba versions in debian:bookworm-slim, and the fixture
    # needs none of it — PAC group SIDs, RFC2307 attrs, and Kerberos all work at
    # the default level. Pinning it broke provisioning on a freshly rebuilt image
    # (issue #1252).
    samba-tool domain provision \
        --use-rfc2307 \
        --realm="$REALM" \
        --domain="$DOMAIN" \
        --server-role=dc \
        --dns-backend=SAMBA_INTERNAL \
        --adminpass="$ADMIN_PASSWORD"

    # Samba writes its own krb5.conf during provision; expose it to other
    # containers / the Go test on the shared volume.
    cp /var/lib/samba/private/krb5.conf "$KEYTAB_DIR/krb5.conf"
    chmod 644 "$KEYTAB_DIR/krb5.conf"

    touch /var/lib/samba/.provisioned
    log "Provision complete"
fi

# Start samba in the background so samba-tool (which talks to the running DC for
# user/group management) can operate. Foreground samba is re-exec'd at the end.
log "Starting samba for provisioning..."
samba -D
# Wait for the LDAP/KDC stack to accept connections.
for _ in $(seq 1 30); do
    if samba-tool processes >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

create_user_if_absent() {
    local user="$1"; shift
    if ! samba-tool user show "$user" >/dev/null 2>&1; then
        log "Creating user $user"
        samba-tool user create "$user" "$USER_PASSWORD" "$@"
    fi
}

create_group_if_absent() {
    local group="$1"
    if ! samba-tool group show "$group" >/dev/null 2>&1; then
        log "Creating group $group"
        samba-tool group add "$group"
    fi
}

# --- Groups ---------------------------------------------------------------
create_group_if_absent engineering
create_group_if_absent devs

# Nest devs under engineering: AD resolves the nesting at the DC, so a member of
# devs gets BOTH the devs and engineering SIDs in their ticket's PAC. This is
# the "no LDAP group-walk needed" property AD-1 demonstrates.
samba-tool group addmembers engineering devs >/dev/null 2>&1 || true

# --- Users ----------------------------------------------------------------
# alice: RFC2307 attrs set (idmap_ad path), member of devs (=> nested in engineering).
create_user_if_absent "$USER_ALICE" \
    --uid-number=10001 --gid-number=10000 \
    --unix-home="/home/$USER_ALICE" --login-shell=/bin/bash
samba-tool group addmembers devs "$USER_ALICE" >/dev/null 2>&1 || true

# bob: NO RFC2307 attrs (idmap_rid algorithmic-fallback path), member of engineering.
create_user_if_absent "$USER_BOB"
samba-tool group addmembers engineering "$USER_BOB" >/dev/null 2>&1 || true

# --- Combined service keytab ----------------------------------------------
# Register BOTH the DittoFS SMB (cifs/) AND NFS (nfs/) service principals on the
# Administrator account and export their keys into ONE keytab. A single keytab
# serving both protocols is what AD-4 exercises: the dittofs server loads one
# keytab and decrypts client AP-REQs for either the SMB or the NFS SPN with it.
#
# `samba-tool domain exportkeytab <file> --principal=...` APPENDS the principal's
# keys to <file> when it already exists, so two successive invocations into the
# same path produce a combined keytab containing both SPNs (each with all the
# enctypes the DC holds). AD-1's PAC test consumes the cifs/ entry of this same
# keytab, so keeping both in one file keeps AD-1 working unchanged.
keytab="$KEYTAB_DIR/dittofs.keytab"
if [ ! -f "$keytab" ]; then
    log "Registering SPNs and exporting COMBINED keytab to $keytab ($SMB_SPN + $NFS_SPN)"

    # Register both SPNs on Administrator. `spn add` is idempotent-ish: it errors
    # if the SPN already exists, which is fine on a re-provisioned fixture.
    samba-tool spn add "$SMB_SPN" "$DOMAIN\\Administrator" >/dev/null 2>&1 || true
    samba-tool spn add "$NFS_SPN" "$DOMAIN\\Administrator" >/dev/null 2>&1 || true

    # Force AES kerberos keys on the SPN-holding account (issue #1318).
    #
    # By default the Administrator account that holds the cifs/ + nfs/ SPNs may
    # carry only the arcfour-hmac (RC4) kerberos key, so `exportkeytab` emits an
    # RC4-only keytab. Windows 11 refuses RC4 (arcfour-hmac) service tickets, so
    # the exported keytab must carry AES256/AES128 keys for the SPNs.
    #
    # Two steps are required, in order:
    #   1. Advertise AES support on the account by setting
    #      msDS-SupportedEncryptionTypes. 0x1F (31) = DES-CBC-CRC + DES-CBC-MD5 +
    #      RC4-HMAC + AES128-CTS + AES256-CTS; keeping the legacy bits set avoids
    #      breaking any RC4/DES client while adding AES. (0x18 / 24 would be
    #      AES-only.)
    #   2. Regenerate the kerberos keys. AES keys are derived from the account
    #      password + salt, so they only materialise on a password change. Reset
    #      the Administrator password to itself to force key regeneration without
    #      changing the credential the rest of the fixture relies on.
    #
    # exportkeytab (below) then emits the full key set the DC now holds — AES256,
    # AES128, and RC4 — because we never restrict it to a single --enctype.
    admin_dn="$(samba-tool user show Administrator --attributes=distinguishedName 2>/dev/null \
        | sed -n 's/^distinguishedName: //p' | head -n1)"
    if [ -n "$admin_dn" ]; then
        log "Enabling AES enctypes on $admin_dn (msDS-SupportedEncryptionTypes=31)"
        ldbmodify -H /var/lib/samba/private/sam.ldb <<EOF || \
            log "WARN: failed to set msDS-SupportedEncryptionTypes (continuing)"
dn: $admin_dn
changetype: modify
replace: msDS-SupportedEncryptionTypes
msDS-SupportedEncryptionTypes: 31
EOF
    else
        log "WARN: could not resolve Administrator DN; skipping enctype attribute set"
    fi

    # Reset the password to itself to regenerate kerberos keys (incl. AES) now
    # that AES enctypes are advertised on the account.
    log "Regenerating kerberos keys for Administrator (password reset to force AES)"
    samba-tool user setpassword Administrator --newpassword="$ADMIN_PASSWORD" \
        >/dev/null 2>&1 || log "WARN: setpassword failed (continuing)"

    # Export cifs/ then APPEND nfs/ into the same keytab file.
    samba-tool domain exportkeytab "$keytab" \
        --principal="$SMB_SPN@$REALM"
    samba-tool domain exportkeytab "$keytab" \
        --principal="$NFS_SPN@$REALM"

    # Verify the combined keytab actually carries BOTH principals before we hand
    # it off. klist -k lists keytab entries; fail loudly here rather than letting
    # the Go test discover a half-exported keytab much later.
    if klist -k "$keytab" >/dev/null 2>&1; then
        if ! klist -k "$keytab" 2>/dev/null | grep -qi "cifs/"; then
            log "ERROR: combined keytab missing cifs/ principal"; klist -k "$keytab" || true; exit 1
        fi
        if ! klist -k "$keytab" 2>/dev/null | grep -qi "nfs/"; then
            log "ERROR: combined keytab missing nfs/ principal"; klist -k "$keytab" || true; exit 1
        fi
        # Fast-fail if AES keys are missing (issue #1318). `klist -ke` prints the
        # enctype per entry; require at least one aes256-cts and one aes128-cts so
        # the Windows-11-incompatible RC4-only regression cannot slip through.
        if klist -ke "$keytab" >/dev/null 2>&1; then
            if ! klist -ke "$keytab" 2>/dev/null | grep -qi "aes256-cts"; then
                log "ERROR: combined keytab missing aes256-cts key (Win11 rejects RC4-only)"
                klist -ke "$keytab" || true; exit 1
            fi
            if ! klist -ke "$keytab" 2>/dev/null | grep -qi "aes128-cts"; then
                log "ERROR: combined keytab missing aes128-cts key (Win11 rejects RC4-only)"
                klist -ke "$keytab" || true; exit 1
            fi
            log "Combined keytab verified: carries cifs/ + nfs/ with aes256-cts and aes128-cts keys"
        else
            log "Combined keytab verified: carries both cifs/ and nfs/ principals (enctype check skipped)"
        fi
    else
        log "klist unavailable; skipping in-container keytab verification (Go test re-checks)"
    fi

    # The dittofs container runs as a non-root uid; make the keytab readable there.
    chown "$DITTOFS_UID:$DITTOFS_GID" "$keytab" 2>/dev/null || true
    chmod 0400 "$keytab" 2>/dev/null || true
fi

log "AD-DC ready: realm=$REALM users=[$USER_ALICE,$USER_BOB] groups=[engineering,devs(nested)]"

# Stop the backgrounded samba and re-exec it in the foreground as PID 1's child
# so the container stays alive and signals propagate.
samba-tool processes >/dev/null 2>&1 || true
pkill -TERM samba 2>/dev/null || true
sleep 2

log "Starting samba in foreground..."
exec samba --foreground --no-process-group
