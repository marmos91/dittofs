#!/usr/bin/env bash
# parity-scw.sh — run the rclone-parity benchmark (#1467) on a DISPOSABLE
# Scaleway VM against a real S3 target (Cubbit, SCW S3, ...). The VM is created,
# used, and DELETED by this script; pass --keep to retain it for debugging.
#
# Usage:
#   AWS_S3_BUCKET=... AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
#   AWS_ENDPOINT_URL=https://s3.fr-par.scw.cloud AWS_S3_REGION=fr-par \
#   ./bench/scripts/parity-scw.sh [--keep] [extra dfsbench parity flags...]
#
# Example — full concurrency sweep matching the committed baseline:
#   ... ./bench/scripts/parity-scw.sh --conc 1,8,24,64 \
#       --large-file-bytes 536870912 --large-file-count 4 --small-file-count 1024
# then compare against the baseline:
#   python3 bench/scripts/parity_check.py bench/results/parity-*.json \
#       --baseline bench/parity/baselines/wan.json
#
# Credentials policy (post-incident): the S3 target comes from environment
# variables ONLY, forwarded to the VM through the ssh session's stdin — never
# written to local or remote disk, never on an argv.
#
# The dfsbench run is launched DETACHED on the VM (nohup) and polled via a DONE
# sentinel, so a dropped ssh session no longer aborts the whole run before
# results are pulled. The VM gets a large root volume so rclone's local download
# copies + dittofs engine dirs don't exhaust disk mid-run.
#
# Requirements: scw CLI (configured), ssh, go, python3.
# Scaleway knobs: SCW_ZONE (default fr-par-1), SCW_INSTANCE_TYPE (default
# POP2-8C-32G — the #1432 baseline shape), SCW_IMAGE (default ubuntu_noble),
# SCW_ROOT_VOLUME (default sbs:100GB:5000 — ample scratch).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ZONE="${SCW_ZONE:-fr-par-1}"
TYPE="${SCW_INSTANCE_TYPE:-POP2-8C-32G}"
IMAGE="${SCW_IMAGE:-ubuntu_noble}"
ROOT_VOL="${SCW_ROOT_VOLUME:-sbs:100GB:5000}"
NAME="dfsbench-parity-$(date +%Y%m%d-%H%M%S)"

KEEP=0
EXTRA_ARGS=()
for arg in "$@"; do
    case "$arg" in
        --keep) KEEP=1 ;;
        *) EXTRA_ARGS+=("$arg") ;;
    esac
done

for tool in scw ssh scp go python3; do
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
state_of() { scw instance server get "$1" zone="$ZONE" -o json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"])' 2>/dev/null || echo GONE; }
ip_of()    { scw instance server get "$1" zone="$ZONE" -o json 2>/dev/null | python3 -c 'import json,sys
d=json.load(sys.stdin); pi=d.get("public_ip") or {}; a=pi.get("address")
if not a:
    for p in (d.get("public_ips") or []):
        if p.get("address"): a=p["address"]; break
print(a or "")' 2>/dev/null; }
cleanup() {
    [ -z "$SERVER_ID" ] && return
    if [ "$KEEP" = 1 ]; then
        echo "parity-scw: --keep set; VM $SERVER_ID left running (delete with:" \
            "scw instance server terminate $SERVER_ID zone=$ZONE with-ip=true with-block=true)"
        return
    fi
    # Wait out any transient (starting/stopping) state — terminate is refused otherwise.
    for _ in $(seq 1 30); do
        st=$(state_of "$SERVER_ID")
        [ "$st" = GONE ] && return
        case "$st" in running|stopped)
            scw instance server terminate "$SERVER_ID" zone="$ZONE" with-ip=true with-block=true >/dev/null 2>&1 \
                && { echo "parity-scw: terminated $SERVER_ID"; return; } ;;
        esac
        sleep 10
    done
    echo "parity-scw: WARNING — failed to terminate $SERVER_ID (last state=$st); delete manually!" >&2
}
trap cleanup EXIT

echo "parity-scw: cross-building dfsbench for linux/amd64"
BIN="$(mktemp -d)/dfsbench.linux"
(cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BIN" ./cmd/bench)

echo "parity-scw: creating $TYPE ($ROOT_VOL) in $ZONE"
SERVER_ID=$(scw instance server create type="$TYPE" zone="$ZONE" image="$IMAGE" name="$NAME" ip=new root-volume="$ROOT_VOL" -o json \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
echo "parity-scw: VM $SERVER_ID — waiting for public IP"
IP=""
for _ in $(seq 1 30); do IP=$(ip_of "$SERVER_ID"); [ -n "$IP" ] && break; sleep 4; done
[ -n "$IP" ] || { echo "parity-scw: no public IP after wait" >&2; exit 1; }
echo "parity-scw: VM $SERVER_ID at $IP"

SSH=(ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o ServerAliveInterval=30 -o ServerAliveCountMax=6 "root@$IP")
echo "parity-scw: waiting for ssh"
for _ in $(seq 1 60); do "${SSH[@]}" true 2>/dev/null && break; sleep 5; done
"${SSH[@]}" true
echo "parity-scw: disk on VM:"; "${SSH[@]}" 'df -h / | tail -1'

echo "parity-scw: installing rclone on VM"
"${SSH[@]}" 'command -v rclone >/dev/null || (curl -fsSL https://rclone.org/install.sh | bash) >/dev/null'

echo "parity-scw: pushing dfsbench"
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$BIN" "root@$IP:/root/dfsbench"
"${SSH[@]}" chmod +x /root/dfsbench

# Forward the S3 target through stdin so credentials never touch a disk or an
# argv. printf %q quotes each value safely for the remote shell. Optional vars
# are emitted only when non-empty (the harness ParseBool's PATH_STYLE).
LABEL="scw-$(date +%Y%m%d-%H%M%S)"
env_block() {
    printf 'AWS_S3_BUCKET=%q\n' "$AWS_S3_BUCKET"
    printf 'AWS_ACCESS_KEY_ID=%q\n' "$AWS_ACCESS_KEY_ID"
    printf 'AWS_SECRET_ACCESS_KEY=%q\n' "$AWS_SECRET_ACCESS_KEY"
    [ -n "${AWS_ENDPOINT_URL:-}" ]  && printf 'AWS_ENDPOINT_URL=%q\n' "$AWS_ENDPOINT_URL"
    [ -n "${AWS_S3_REGION:-}" ]     && printf 'AWS_S3_REGION=%q\n' "$AWS_S3_REGION"
    [ -n "${AWS_S3_PATH_STYLE:-}" ] && printf 'AWS_S3_PATH_STYLE=%q\n' "$AWS_S3_PATH_STYLE"
    return 0
}

# Build the remote driver (no credentials in it — those arrive via env at launch)
# and write it to the VM, then launch it DETACHED with creds in-env and poll a
# DONE sentinel over short ssh calls that survive individual drops.
EXTRA_QUOTED=""
[ "${#EXTRA_ARGS[@]}" -gt 0 ] && EXTRA_QUOTED="$(printf '%q ' "${EXTRA_ARGS[@]}")"
DRIVER="set -uo pipefail
rm -f /root/DONE; mkdir -p /root/parity-results
cd /root && ./dfsbench parity --label ${LABEL} --out-dir /root/parity-results ${EXTRA_QUOTED}; echo \"rc=\$?\" > /root/DONE"
printf '%s' "$DRIVER" | "${SSH[@]}" 'cat > /root/driver.sh'
echo "parity-scw: launching detached run (label=$LABEL)"
# shellcheck disable=SC2016
env_block | "${SSH[@]}" 'set -a; . /dev/stdin >/dev/null; set +a; cd /root && nohup bash driver.sh > /root/run.log 2>&1 & echo "parity-scw: driver pid $!"'

echo "parity-scw: polling for completion (max ~60 min)"
DONE=0
for i in $(seq 1 180); do
    "${SSH[@]}" 'test -f /root/DONE' 2>/dev/null && { DONE=1; break; }
    "${SSH[@]}" 'grep -a "^parity:" /root/run.log 2>/dev/null | tail -1' 2>/dev/null | sed "s/^/  [$i] /"
    sleep 20
done

echo "parity-scw: pulling results"
mkdir -p "$REPO_ROOT/bench/results"
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=30 \
    "root@$IP:/root/parity-results/parity-${LABEL}-*" "$REPO_ROOT/bench/results/" 2>/dev/null \
    || echo "parity-scw: WARNING — no scorecard pulled"
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "root@$IP:/root/run.log" "$REPO_ROOT/bench/results/parity-${LABEL}.log" 2>/dev/null || true
"${SSH[@]}" 'cat /root/DONE 2>/dev/null' 2>/dev/null | sed 's/^/parity-scw: DONE /' || true
[ "$DONE" = 1 ] || echo "parity-scw: WARNING — polling timed out before DONE (partial results)"
echo "parity-scw: done — scorecard in bench/results/parity-${LABEL}-*"
