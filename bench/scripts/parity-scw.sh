#!/usr/bin/env bash
# parity-scw.sh — run the rclone-parity benchmark (#1467) on a DISPOSABLE
# Scaleway VM against a real S3 target (Cubbit, SCW S3, ...). The VM is
# created, used, and DELETED by this script; pass --keep to retain it for
# debugging.
#
# Usage:
#   AWS_S3_BUCKET=... AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
#   AWS_ENDPOINT_URL=https://s3.cubbit.eu \
#   ./bench/scripts/parity-scw.sh [--keep] [extra dfsbench parity flags...]
#
# Credentials policy (post-incident): the S3 target comes from environment
# variables ONLY. They are forwarded to the VM through the ssh session's stdin
# — never written to local or remote disk, never on an argv.
#
# Requirements: scw CLI (configured), ssh, go.
# Scaleway knobs: SCW_ZONE (default fr-par-1), SCW_INSTANCE_TYPE (default
# POP2-8C-32G — the #1432 baseline shape), SCW_IMAGE (default ubuntu_noble).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ZONE="${SCW_ZONE:-fr-par-1}"
TYPE="${SCW_INSTANCE_TYPE:-POP2-8C-32G}"
IMAGE="${SCW_IMAGE:-ubuntu_noble}"
NAME="dfsbench-parity-$(date +%Y%m%d-%H%M%S)"

KEEP=0
EXTRA_ARGS=()
for arg in "$@"; do
    case "$arg" in
        --keep) KEEP=1 ;;
        *) EXTRA_ARGS+=("$arg") ;;
    esac
done

for tool in scw ssh go python3; do
    command -v "$tool" >/dev/null || { echo "parity-scw: $tool not found in PATH" >&2; exit 1; }
done
MISSING=()
for var in AWS_S3_BUCKET AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY; do
    [ -n "${!var:-}" ] || MISSING+=("$var")
done
if [ "${#MISSING[@]}" -gt 0 ]; then
    echo "parity-scw: missing required environment variables: ${MISSING[*]}" >&2
    echo "parity-scw: credentials are env-only by design — never put them in files." >&2
    exit 1
fi

SERVER_ID=""
cleanup() {
    if [ -n "$SERVER_ID" ]; then
        if [ "$KEEP" = 1 ]; then
            echo "parity-scw: --keep set; VM $SERVER_ID left running (delete with:" \
                "scw instance server terminate $SERVER_ID zone=$ZONE with-ip=true with-block=true)"
        else
            echo "parity-scw: deleting VM $SERVER_ID"
            scw instance server terminate "$SERVER_ID" zone="$ZONE" with-ip=true with-block=true >/dev/null || \
                echo "parity-scw: WARNING — failed to delete $SERVER_ID; delete it manually!" >&2
        fi
    fi
}
trap cleanup EXIT

echo "parity-scw: cross-building dfsbench for linux/amd64"
BIN="$(mktemp -d)/dfsbench.linux"
(cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BIN" ./cmd/bench)

echo "parity-scw: creating $TYPE in $ZONE"
SERVER_ID=$(scw instance server create type="$TYPE" zone="$ZONE" image="$IMAGE" \
    name="$NAME" ip=new -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
IP=$(scw instance server get "$SERVER_ID" zone="$ZONE" -o json |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["public_ip"]["address"])')
echo "parity-scw: VM $SERVER_ID at $IP"

SSH=(ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 "root@$IP")
echo "parity-scw: waiting for ssh"
for _ in $(seq 1 60); do
    if "${SSH[@]}" true 2>/dev/null; then break; fi
    sleep 5
done
"${SSH[@]}" true

echo "parity-scw: installing rclone on VM"
"${SSH[@]}" 'command -v rclone >/dev/null || (curl -fsSL https://rclone.org/install.sh | bash) >/dev/null'

echo "parity-scw: pushing dfsbench"
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$BIN" "root@$IP:/root/dfsbench"
"${SSH[@]}" chmod +x /root/dfsbench

# Forward the S3 target through stdin (sourced by the remote shell) so the
# credentials never touch a disk or an argv on either side.
LABEL="scw-$(date +%Y%m%d-%H%M%S)"
echo "parity-scw: running harness (label=$LABEL) — this can take a while"
# shellcheck disable=SC2016 # $PARITY_LABEL expands on the VM, not here
"${SSH[@]}" 'set -a; . /dev/stdin >/dev/null; set +a; cd /root && ./dfsbench parity --label "$PARITY_LABEL" --out-dir /root/parity-results '"$(printf '%q ' "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}")" <<EOF
AWS_S3_BUCKET='${AWS_S3_BUCKET}'
AWS_ACCESS_KEY_ID='${AWS_ACCESS_KEY_ID}'
AWS_SECRET_ACCESS_KEY='${AWS_SECRET_ACCESS_KEY}'
AWS_ENDPOINT_URL='${AWS_ENDPOINT_URL:-}'
AWS_S3_REGION='${AWS_S3_REGION:-}'
AWS_S3_PATH_STYLE='${AWS_S3_PATH_STYLE:-}'
AWS_S3_KEY_PREFIX='${AWS_S3_KEY_PREFIX:-}'
PARITY_LABEL='${LABEL}'
EOF

echo "parity-scw: pulling results"
mkdir -p "$REPO_ROOT/bench/results"
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    "root@$IP:/root/parity-results/parity-${LABEL}-*" "$REPO_ROOT/bench/results/"

echo "parity-scw: done — scorecard in bench/results/parity-${LABEL}-*"
