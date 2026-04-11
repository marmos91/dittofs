#!/bin/bash
# KDC bootstrap for SMB conformance testing.
#
# Creates a self-contained MIT Kerberos realm with:
#   - Service principal  cifs/dittofs@$REALM (random key, exported to keytab)
#   - User principal     wpts-admin@$REALM   (password TestPassword01!)
#
# The keytab is written to /keytabs/dittofs.keytab on a shared volume so the
# DittoFS container can pick it up for SMB Kerberos authentication.
#
# The KDC listens on port 88 (TCP+UDP) and is reachable from other containers
# via the docker-compose service name "kdc".

set -euo pipefail

REALM="${KRB5_REALM:-DITTOFS.TEST}"
KDC_HOST="${KDC_HOST:-kdc}"
KEYTAB_DIR="${KEYTAB_DIR:-/keytabs}"
DITTOFS_SPN="${DITTOFS_SPN:-cifs/dittofs}"
USER_PRINCIPAL="${USER_PRINCIPAL:-wpts-admin}"
USER_PASSWORD="${USER_PASSWORD:-TestPassword01!}"

# UID/GID the dittofs container runs as; the keytab must be readable there.
DITTOFS_UID="${DITTOFS_UID:-65532}"
DITTOFS_GID="${DITTOFS_GID:-65532}"

# Lowercase realm for the [domain_realm] mapping (e.g. DITTOFS.TEST -> dittofs.test).
REALM_LOWER="$(echo "$REALM" | tr '[:upper:]' '[:lower:]')"

log() { echo "[KDC] $*"; }

mkdir -p "$KEYTAB_DIR"

log "Configuring realm $REALM (KDC host: $KDC_HOST)"

# krb5.conf is written twice: to /etc/krb5.conf for tools running inside this
# container (kadmin.local, krb5kdc) and to $KEYTAB_DIR/krb5.conf on the shared
# volume so other containers (dittofs, smbtorture) can mount it read-only.
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

# kdc.conf — server-side KDC database configuration.
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

# Allow the default admin user to modify principals.
echo "*/admin@$REALM *" > /etc/krb5kdc/kadm5.acl

# Initialize realm database (only once per container).
if [ ! -f /var/lib/krb5kdc/principal ]; then
    keytab="$KEYTAB_DIR/dittofs.keytab"

    log "Creating realm database..."
    # Pipe the master password via stdin so it does not appear in /proc/cmdline
    # while kdb5_util runs. The password goes twice (kdb5_util prompts for
    # confirmation in non-password-file mode).
    printf 'masterpassword\nmasterpassword\n' | kdb5_util create -s -r "$REALM"

    log "Adding service principal $DITTOFS_SPN@$REALM"
    kadmin.local -q "addprinc -randkey $DITTOFS_SPN@$REALM"

    log "Exporting keytab to $keytab"
    kadmin.local -q "ktadd -k $keytab $DITTOFS_SPN@$REALM"
    # The keytab contains long-lived secret keys; restrict to the dittofs
    # container's non-root UID and owner-read only.
    chown "$DITTOFS_UID:$DITTOFS_GID" "$keytab"
    chmod 0400 "$keytab"

    log "Adding user principal $USER_PRINCIPAL@$REALM"
    # Pipe the password via stdin rather than embedding it in -q. This
    # avoids kadmin argument parsing issues if the password contains
    # whitespace or shell metacharacters, and keeps the plaintext off
    # kadmin's command history / trace logs.
    printf 'addprinc -pw %s %s@%s\n' \
        "$USER_PASSWORD" "$USER_PRINCIPAL" "$REALM" \
        | kadmin.local > /dev/null
fi

log "Starting krb5kdc on port 88..."
exec krb5kdc -n
