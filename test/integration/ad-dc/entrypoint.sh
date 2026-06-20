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
    samba-tool domain provision \
        --use-rfc2307 \
        --realm="$REALM" \
        --domain="$DOMAIN" \
        --server-role=dc \
        --dns-backend=SAMBA_INTERNAL \
        --adminpass="$ADMIN_PASSWORD" \
        --option="ad dc functional level = 2016"

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

# --- Service keytab -------------------------------------------------------
# Register the DittoFS SMB + NFS service principals on the Administrator account
# and export their keys so the server can decrypt client AP-REQs. The PAC is
# signed with the same key, so a correct keytab is required for PAC validation.
keytab="$KEYTAB_DIR/dittofs.keytab"
if [ ! -f "$keytab" ]; then
    log "Exporting service keytab to $keytab ($SMB_SPN, $NFS_SPN)"
    samba-tool spn add "$SMB_SPN" "$DOMAIN\\Administrator" >/dev/null 2>&1 || true
    samba-tool spn add "$NFS_SPN" "$DOMAIN\\Administrator" >/dev/null 2>&1 || true
    samba-tool domain exportkeytab "$keytab" \
        --principal="$SMB_SPN@$REALM" >/dev/null 2>&1 || true
    samba-tool domain exportkeytab "$keytab" \
        --principal="$NFS_SPN@$REALM" >/dev/null 2>&1 || true
    # Also export Administrator's key so tests that need a TGT via keytab can.
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
