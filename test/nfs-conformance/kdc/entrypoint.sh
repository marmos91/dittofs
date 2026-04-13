#!/bin/bash
# KDC bootstrap for NFS Kerberos conformance testing.
#
# Creates a self-contained MIT Kerberos realm with:
#   - Service principal  nfs/dittofs@$REALM (random key, exported to keytab)
#   - User principal     nfs-test@$REALM    (password TestPassword01!)
#
# The keytab is written to /keytabs/dittofs.keytab on a shared volume so the
# DittoFS container can pick it up for NFS Kerberos authentication.

set -euo pipefail

REALM="${KRB5_REALM:-DITTOFS.TEST}"
KDC_HOST="${KDC_HOST:-kdc}"
KEYTAB_DIR="${KEYTAB_DIR:-/keytabs}"
NFS_SPN="${NFS_SPN:-nfs/dittofs}"
USER_PRINCIPAL="${USER_PRINCIPAL:-nfs-test}"
USER_PASSWORD="${USER_PASSWORD:-TestPassword01!}"

DITTOFS_UID="${DITTOFS_UID:-65532}"
DITTOFS_GID="${DITTOFS_GID:-65532}"

REALM_LOWER="$(echo "$REALM" | tr '[:upper:]' '[:lower:]')"

log() { echo "[KDC] $*"; }

mkdir -p "$KEYTAB_DIR"

log "Configuring realm $REALM (KDC host: $KDC_HOST)"

write_krb5_conf() {
    local target="$1"
    cat > "$target" <<EOF
[libdefaults]
    default_realm = $REALM
    dns_lookup_realm = false
    dns_lookup_kdc = false
    rdns = false
    ticket_lifetime = 24h
    forwardable = true
    udp_preference_limit = 1

[realms]
    $REALM = {
        kdc = $KDC_HOST:88
        admin_server = $KDC_HOST
    }

[domain_realm]
    .$REALM_LOWER = $REALM
    $REALM_LOWER = $REALM
EOF
}

write_krb5_conf /etc/krb5.conf
write_krb5_conf "$KEYTAB_DIR/krb5.conf"
chmod 644 "$KEYTAB_DIR/krb5.conf"

mkdir -p /etc/krb5kdc
cat > /etc/krb5kdc/kdc.conf <<EOF
[kdcdefaults]
    kdc_ports = 88
    kdc_tcp_ports = 88

[realms]
    $REALM = {
        database_name = /var/lib/krb5kdc/principal
        admin_keytab = FILE:/etc/krb5kdc/kadm5.keytab
        acl_file = /etc/krb5kdc/kadm5.acl
        key_stash_file = /etc/krb5kdc/stash
        max_life = 10h 0m 0s
        max_renewable_life = 7d 0h 0m 0s
        supported_enctypes = aes256-cts-hmac-sha1-96:normal aes128-cts-hmac-sha1-96:normal
    }
EOF

echo "*/admin@$REALM *" > /etc/krb5kdc/kadm5.acl

if [ ! -f /var/lib/krb5kdc/principal ]; then
    keytab="$KEYTAB_DIR/dittofs.keytab"

    log "Creating realm database..."
    printf 'masterpassword\nmasterpassword\n' | kdb5_util create -s -r "$REALM"

    log "Adding NFS service principal $NFS_SPN@$REALM"
    kadmin.local -q "addprinc -randkey $NFS_SPN@$REALM"

    log "Exporting server keytab to $keytab"
    kadmin.local -q "ktadd -k $keytab $NFS_SPN@$REALM"
    chown "$DITTOFS_UID:$DITTOFS_GID" "$keytab"
    chmod 0400 "$keytab"

    log "Adding user principal $USER_PRINCIPAL@$REALM (password-based)"
    printf 'addprinc -pw %s %s@%s\n' \
        "$USER_PASSWORD" "$USER_PRINCIPAL" "$REALM" \
        | kadmin.local > /dev/null
fi

log "Starting krb5kdc on port 88..."
exec krb5kdc -n
