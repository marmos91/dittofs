#!/bin/sh
# Provisions a DittoFS server over the REST API: one metadata store, one block
# store, the share /export, and that export's squash mode. The NFS and SMB
# adapters are seeded enabled, so nothing here touches them.
#
# Every step treats "already exists" as success, so a re-run finishes what a
# partial run left undone instead of wedging on the first conflict.
set -eu

API=http://dittofs:8080
PROFILE="${PROFILE:-default}"
SHARE="/export"
ADMIN_PASSWORD="${DITTOFS_ADMIN_INITIAL_PASSWORD:?must be set}"

log() { echo "[bootstrap] $*"; }

# json_escape VALUE — escape a value for use inside a JSON string literal.
# Backslash first, then quote, or the quote's own backslash gets doubled. A
# control character has no single-character escape here, so it is refused rather
# than sent as a body the server would reject with an opaque parse error.
json_escape() {
    if [ "$(printf %s "$1" | tr -d '[:cntrl:]')" != "$1" ]; then
        log "the admin password contains a control character and cannot be sent as JSON"
        exit 1
    fi
    printf %s "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# sh has no pipefail, so a failed login reaches the token check as empty output
# rather than as a non-zero status.
login=$(curl -sS -X POST "$API/api/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"admin\",\"password\":\"$(json_escape "$ADMIN_PASSWORD")\"}") || true
TOKEN=$(printf '%s' "$login" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
if [ -z "$TOKEN" ]; then
    log "admin login failed: $login"
    exit 1
fi

# exists PATH — true when a GET of the resource answers 2xx. Creating a store
# that is already there does not always fail as a conflict: a metadata store is
# instantiated before its name is checked, so a second `badger` create reports
# the data directory as locked instead. Asking first keeps a re-run quiet.
exists() {
    code=$(curl -sS -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $TOKEN" "$API$1")
    case "$code" in
    2??) return 0 ;;
    *) return 1 ;;
    esac
}

# send METHOD PATH BODY — 409 means the resource is already there, which is
# what provisioning wanted.
send() {
    out=$(curl -sS -w '\n%{http_code}' -X "$1" "$API$2" \
        -H "Authorization: Bearer $TOKEN" \
        -H 'Content-Type: application/json' \
        -d "$3")
    code=$(printf '%s' "$out" | tail -n 1)
    case "$code" in
    2?? | 409) ;;
    *)
        log "$1 $2 -> $code: $(printf '%s' "$out" | sed '$d')"
        exit 1
        ;;
    esac
}

log "provisioning profile: $PROFILE"

case "$PROFILE" in
default | s3-backend)
    metadata_store='{"name":"default","type":"badger","config":"{\"db_path\":\"/data/metadata\"}"}'
    ;;
postgres-backend)
    metadata_store='{"name":"default","type":"postgres","config":"{\"host\":\"postgres\",\"port\":5432,\"user\":\"dittofs\",\"password\":\"dittofs\",\"database\":\"dittofs\",\"sslmode\":\"disable\"}"}'
    ;;
*)
    log "unknown profile \"$PROFILE\" — expected default, s3-backend or postgres-backend"
    exit 1
    ;;
esac

if [ "$PROFILE" = s3-backend ]; then
    # Localstack holds its buckets for the life of its container, so the bucket
    # is (re)created on every run rather than only on the first. An unsigned
    # path-style PUT is enough — Localstack does not verify signatures — and an
    # existing bucket answers 409.
    bucket=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "http://localstack:4566/dittofs")
    case "$bucket" in
    2?? | 409) ;;
    *)
        log "creating the bucket returned $bucket"
        exit 1
        ;;
    esac
    # allow_private_endpoint opts Localstack past the S3 SSRF guard, which
    # otherwise rejects the private address the name resolves to.
    block_store='{"name":"default","type":"s3","config":"{\"bucket\":\"dittofs\",\"region\":\"us-east-1\",\"endpoint\":\"http://localstack:4566\",\"force_path_style\":true,\"access_key_id\":\"test\",\"secret_access_key\":\"test\",\"allow_private_endpoint\":true}"}'
else
    # A share's durable home is s3 or memory; there is no filesystem block store.
    block_store='{"name":"default","type":"memory"}'
fi

exists /api/v1/store/metadata/default || send POST /api/v1/store/metadata "$metadata_store"
exists /api/v1/store/block/default || send POST /api/v1/store/block "$block_store"

send POST /api/v1/shares "{\"name\":\"$SHARE\",\"metadata_store_id\":\"default\",\"block_store\":\"default\",\"default_permission\":\"read-write\"}"

# decision: the export keeps root as root rather than squashing it. The share
# root is owned by root, so under the default squash a freshly mounted client
# cannot write anything at the root and the stack looks broken. The exemption is
# worth only what this stack reaches — one host, fixed development credentials,
# every port bound to the loopback. Withdraw it the moment either stops holding.
send PATCH "/api/v1/shares/${SHARE#/}/adapters/nfs/config" '{"squash":"root_to_admin"}'

log "$SHARE is exported over NFS and SMB"
