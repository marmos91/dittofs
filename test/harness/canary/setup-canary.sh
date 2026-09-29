#!/usr/bin/env bash
# Provision the DittoFS canary on a running server, idempotently. Run it as the user
# whose dfsctl login is admin on that server (and who runs the server, so it can own
# the canary's metadata directory), with sudo for the root-only files. It creates:
#   - metadata store  canary-md   (badger, CANARY_METADATA_DIR)
#   - block store     s3-canary   (S3, CANARY_BUCKET under CANARY_PREFIX, so its keys
#                                  never mix with other stores' objects in the bucket)
#   - share           /canary     (on those two, so GC on it touches nothing else)
#   - users           canary (SMB, read-write on the share) and canary-ops (admin, for
#                                  GC and evict), with fresh random passwords
#   - root-only       /etc/dittofs-canary/canary.env (CANARY_* for canary.sh and
#                     DITTOFS_E2E_LIVE_* for test/e2e/live) and CANARY_STATE/bin/dfsctl
# Credentials are read from an rclone remote (CANARY_RCLONE_REMOTE in rclone.conf) or
# from S3_ENDPOINT/S3_ACCESS_KEY/S3_SECRET_KEY, and are never printed.
#
#   DFSCTL=/path/to/dfsctl CANARY_BUCKET=my-bucket CANARY_RCLONE_REMOTE=myremote \
#     CANARY_METADATA_DIR=/srv/data/canary-metadata test/harness/canary/setup-canary.sh
set -euo pipefail
for t in jq sudo install awk; do command -v "$t" >/dev/null || { echo "setup-canary needs $t (e.g. sudo apt-get install -y $t)" >&2; exit 2; }; done
DFSCTL="${DFSCTL:-$(command -v dfsctl || true)}"
[[ -x "$DFSCTL" ]] || { echo "set DFSCTL to the server's dfsctl" >&2; exit 2; }
BUCKET="${CANARY_BUCKET:?set CANARY_BUCKET}"
PREFIX="${CANARY_PREFIX:-dittofs-canary/}"
SHARE="${CANARY_SHARE:-/canary}"
API="${CANARY_API:-http://127.0.0.1:8080}"
MDDIR="${CANARY_METADATA_DIR:?set CANARY_METADATA_DIR (writable by the dfs process)}"
ENV_FILE="${CANARY_ENV:-/etc/dittofs-canary/canary.env}"
STATE="${CANARY_STATE:-/srv/dittofs-canary}"
MD=canary-md BS=s3-canary SMB_USER=canary OPS_USER=canary-ops

say() { echo "[canary-setup] $*"; }
has() { "$DFSCTL" "$@" -o json 2>/dev/null; }
# A fixed read, no early-closing reader: `tr </dev/urandom | head` dies of SIGPIPE,
# which pipefail + set -e turn into a silent exit.
pw() { local p; p="$(head -c 512 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9')"; echo "${p:0:28}"; }

# S3 credentials, from rclone.conf or the environment.
if [[ -n "${CANARY_RCLONE_REMOTE:-}" ]]; then
    conf="${CANARY_RCLONE_CONF:-$HOME/.config/rclone/rclone.conf}"
    get() { awk -v s="[$CANARY_RCLONE_REMOTE]" -v k="$1" '$0==s {f=1; next} /^\[/ {f=0} f && $1==k {sub(/^[^=]*=[ \t]*/, ""); print; exit}' "$conf"; }
    S3_ENDPOINT="$(get endpoint)"; S3_ACCESS_KEY="$(get access_key_id)"; S3_SECRET_KEY="$(get secret_access_key)"
fi
[[ -n "${S3_ENDPOINT:-}" && -n "${S3_ACCESS_KEY:-}" && -n "${S3_SECRET_KEY:-}" ]] ||
    { echo "no S3 endpoint/credentials (CANARY_RCLONE_REMOTE or S3_ENDPOINT/S3_ACCESS_KEY/S3_SECRET_KEY)" >&2; exit 2; }

"$DFSCTL" share list >/dev/null || { echo "dfsctl is not logged in to the server as an admin" >&2; exit 2; }

if has store metadata list | jq -e --arg n "$MD" 'any(.[]; .name == $n)' >/dev/null; then say "metadata store $MD exists"
else
    mkdir -p "$MDDIR"
    "$DFSCTL" store metadata add --name "$MD" --type badger --db-path "$MDDIR" >/dev/null
    say "created metadata store $MD (badger, $MDDIR)"
fi

if has store block list | jq -e --arg n "$BS" 'any(.[]; .name == $n)' >/dev/null; then say "block store $BS exists"
else
    cfg="$(jq -cn --arg e "$S3_ENDPOINT" --arg b "$BUCKET" --arg p "$PREFIX" --arg a "$S3_ACCESS_KEY" --arg s "$S3_SECRET_KEY" \
        '{endpoint: $e, bucket: $b, prefix: $p, access_key_id: $a, secret_access_key: $s}')"
    "$DFSCTL" store block add --name "$BS" --type s3 --config "$cfg" >/dev/null
    say "created block store $BS ($S3_ENDPOINT, bucket $BUCKET, prefix $PREFIX)"
fi

if has share list | jq -e --arg n "$SHARE" 'any(.[]; .name == $n)' >/dev/null; then say "share $SHARE exists"
else
    "$DFSCTL" share create --name "$SHARE" --metadata "$MD" --block-store "$BS" --default-permission none \
        --description "DittoFS canary: write/read/upload/delete/GC round trips" >/dev/null
    say "created share $SHARE"
fi

# An account an admin creates or resets must set its own password before it may do
# anything else, so each user gets a temporary password from the admin, then sets
# its final one itself, in a throwaway dfsctl session (the admin's is untouched).
as_user() { # as_user NAME PASSWORD DFSCTL-ARGS...
    local d rc=0; d="$(mktemp -d)"
    XDG_CONFIG_HOME="$d" "$DFSCTL" login --server "$API" --username "$1" --password "$2" >/dev/null &&
        XDG_CONFIG_HOME="$d" "$DFSCTL" "${@:3}" >/dev/null || rc=$?
    rm -rf "$d"; return "$rc"
}
SMB_PASS="$(pw)" OPS_PASS="$(pw)"
for u in "$SMB_USER:user:$SMB_PASS" "$OPS_USER:admin:$OPS_PASS"; do
    name="${u%%:*}" rest="${u#*:}"; role="${rest%%:*}" pass="${rest#*:}" tmp="$(pw)"
    if has user list | jq -e --arg n "$name" 'any(.[]; .username == $n)' >/dev/null; then
        "$DFSCTL" user password "$name" --password "$tmp" >/dev/null
        say "user $name exists; password reset"
    else
        "$DFSCTL" user create --username "$name" --password "$tmp" --role "$role" >/dev/null
        say "created user $name ($role)"
    fi
    as_user "$name" "$tmp" user change-password --current "$tmp" --new "$pass"
    say "user $name set its own password"
done
"$DFSCTL" share permission grant "$SHARE" --user "$SMB_USER" --level read-write >/dev/null
say "granted $SMB_USER read-write on $SHARE"

smb_port="$("$DFSCTL" adapter list -o json | jq -r '.[] | select(.type == "smb") | .port')"
sudo install -d -m 0700 "$(dirname "$ENV_FILE")"
tmp="$(mktemp)"; chmod 600 "$tmp"
cat >"$tmp" <<EOF
CANARY_API=$API
CANARY_SMB_HOST=127.0.0.1
CANARY_SMB_PORT=${smb_port:-12445}
CANARY_SHARE=$SHARE
CANARY_SMB_USER=$SMB_USER
CANARY_SMB_PASS=$SMB_PASS
CANARY_OPS_USER=$OPS_USER
CANARY_OPS_PASS=$OPS_PASS
CANARY_S3_BUCKET=$BUCKET
CANARY_S3_PREFIX=$PREFIX
RCLONE_CONFIG_CANARYS3_TYPE=s3
RCLONE_CONFIG_CANARYS3_PROVIDER=Other
RCLONE_CONFIG_CANARYS3_ENDPOINT=$S3_ENDPOINT
RCLONE_CONFIG_CANARYS3_ACCESS_KEY_ID=$S3_ACCESS_KEY
RCLONE_CONFIG_CANARYS3_SECRET_ACCESS_KEY=$S3_SECRET_KEY
RCLONE_CONFIG_CANARYS3_FORCE_PATH_STYLE=true
DITTOFS_E2E_LIVE_API=$API
DITTOFS_E2E_LIVE_ADMIN_USER=$OPS_USER
DITTOFS_E2E_LIVE_ADMIN_PASSWORD=$OPS_PASS
DITTOFS_E2E_LIVE_SHARE=$SHARE
DITTOFS_E2E_LIVE_SMB_HOST=127.0.0.1
DITTOFS_E2E_LIVE_SMB_PORT=${smb_port:-12445}
DITTOFS_E2E_LIVE_SMB_USER=$SMB_USER
DITTOFS_E2E_LIVE_SMB_PASSWORD=$SMB_PASS
DITTOFS_E2E_LIVE_S3_ENDPOINT=$S3_ENDPOINT
DITTOFS_E2E_LIVE_S3_BUCKET=$BUCKET
DITTOFS_E2E_LIVE_S3_PREFIX=$PREFIX
DITTOFS_E2E_LIVE_S3_ACCESS_KEY=$S3_ACCESS_KEY
DITTOFS_E2E_LIVE_S3_SECRET_KEY=$S3_SECRET_KEY
EOF
sudo install -m 0600 -o root -g root "$tmp" "$ENV_FILE"; rm -f "$tmp"
sudo install -d -m 0755 "$STATE" "$STATE/bin"
sudo install -m 0755 "$DFSCTL" "$STATE/bin/dfsctl"
say "wrote $ENV_FILE (root, 0600) and $STATE/bin/dfsctl (a copy of $DFSCTL)"
# The rclone the canary measures the bucket with: CANARY_RCLONE_BIN (the operators'
# own binary, so the numbers match theirs), else the one on PATH, else the image's.
RCLONE_BIN="${CANARY_RCLONE_BIN:-$(command -v rclone || true)}"
if [[ -x "$RCLONE_BIN" ]]; then
    sudo install -m 0755 "$RCLONE_BIN" "$STATE/bin/rclone"
    say "copied $RCLONE_BIN ($("$RCLONE_BIN" version 2>/dev/null | head -1)) to $STATE/bin/rclone"
fi
say "run one pass: sudo $(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/run-canary.sh"
