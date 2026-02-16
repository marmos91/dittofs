#!/bin/bash
set -e

REALM="${KRB5_REALM:-DITTOFS.TEST}"

# Write krb5.conf
cat > /etc/krb5.conf <<EOF
[libdefaults]
    default_realm = $REALM
    dns_lookup_realm = false
    dns_lookup_kdc = false

[realms]
    $REALM = {
        kdc = localhost
        admin_server = localhost
    }
EOF

# Write kdc.conf
mkdir -p /etc/krb5kdc
cat > /etc/krb5kdc/kdc.conf <<EOF
[kdcdefaults]
    kdc_ports = 88

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

# Create realm database
kdb5_util create -s -r "$REALM" -P masterpassword

# Start KDC in foreground
exec krb5kdc -n
