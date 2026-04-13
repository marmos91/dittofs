#!/bin/sh
# bootstrap.sh - Configure DittoFS for NFS Kerberos conformance testing
#
# Provisions stores, shares, users, and NFS adapter on a running DittoFS instance.

set -eu

DFSCTL="${DFSCTL:-dfsctl}"
API_URL="${API_URL:-http://localhost:8080}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-${DITTOFS_CONTROLPLANE_SECRET:-NfsConformanceTesting2026!Secret}}"
TEST_PASSWORD="${TEST_PASSWORD:-TestPassword01!}"
NFS_PORT="${NFS_PORT:-12049}"

KERBEROS_REALM="${KERBEROS_REALM:-DITTOFS.TEST}"

log_info() { echo "[BOOTSTRAP] $*"; }
log_error() { echo "[BOOTSTRAP] ERROR: $*" >&2; }

wait_for_ready() {
    max=30
    attempt=1
    log_info "Waiting for DittoFS API at ${API_URL}/health/ready ..."
    while [ "$attempt" -le "$max" ]; do
        if curl -sf "${API_URL}/health/ready" >/dev/null 2>&1; then
            log_info "DittoFS API is ready"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_error "DittoFS API not ready after ${max}s"
    return 1
}

run_dfsctl() {
    "$DFSCTL" --server "$API_URL" --no-color "$@"
}

wait_for_ready

log_info "Logging in..."
run_dfsctl login --username admin --password "$ADMIN_PASSWORD"

log_info "Creating metadata store..."
run_dfsctl store metadata add --name meta-mem --type memory 2>/dev/null || true

log_info "Creating block store..."
run_dfsctl store block add --kind local --name local-mem --type memory 2>/dev/null || true

log_info "Creating NFS share /export..."
run_dfsctl share create --name /export --metadata meta-mem --local local-mem 2>/dev/null || true

log_info "Creating NFS adapter..."
run_dfsctl adapter create --type nfs --port "$NFS_PORT" 2>/dev/null || true

log_info "Creating test user nfs-test..."
run_dfsctl user create --username nfs-test --password "$TEST_PASSWORD" 2>/dev/null || true

log_info "Granting read-write on /export to nfs-test..."
run_dfsctl share permission grant /export --user nfs-test --level read-write 2>/dev/null || true

log_info "Creating Kerberos identity mapping..."
run_dfsctl adapter identity-map add --type nfs \
    --principal "nfs-test@${KERBEROS_REALM}" \
    --username nfs-test 2>/dev/null || true

log_info "Bootstrap complete"
