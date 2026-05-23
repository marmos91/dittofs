#!/usr/bin/env bash
# bootstrap.sh - Configure DittoFS with WPTS-required stores, shares, and users
#
# This script provisions a running DittoFS instance for Microsoft
# WindowsProtocolTestSuites (WPTS) SMB conformance testing.
#
# Works in both Docker Compose mode (dfsctl inside container) and
# local mode (dfsctl on host).
#
# Usage:
#   PROFILE=memory ./bootstrap.sh
#   DFSCTL=/app/dfsctl API_URL=http://localhost:8080 ./bootstrap.sh

set -euo pipefail

# Configuration (overridable via environment)
DFSCTL="${DFSCTL:-dfsctl}"
API_URL="${API_URL:-http://localhost:8080}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-${DITTOFS_CONTROLPLANE_SECRET:-WptsConformanceTesting2026!Secret}}"
TEST_PASSWORD="${TEST_PASSWORD:-TestPassword01!}"
PROFILE="${PROFILE:-memory}"
SMB_PORT="${SMB_PORT:-12445}"

# Kerberos settings (auto-detected from profile name, or forced via KERBEROS=1).
# When enabled, an identity mapping wpts-admin@${KERBEROS_REALM} -> wpts-admin
# is created so Kerberos session setup resolves to the right control plane user.
KERBEROS_REALM="${KERBEROS_REALM:-DITTOFS.TEST}"
is_kerberos_profile() {
    [[ "$PROFILE" == *kerberos* ]] || [[ "${KERBEROS:-0}" == "1" ]]
}

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[BOOTSTRAP]${NC} $*"; }
log_error() { echo -e "${RED}[BOOTSTRAP]${NC} $*"; }

# Wait for DittoFS API to be ready
wait_for_ready() {
    local max=30
    local attempt=1

    log_info "Waiting for DittoFS API at ${API_URL}/health/ready ..."

    while [ "$attempt" -le "$max" ]; do
        if curl -sf "${API_URL}/health/ready" >/dev/null 2>&1; then
            log_info "DittoFS API is ready"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "DittoFS not ready after ${max}s"
    return 1
}

# Wait for SMB port to be accepting connections
wait_for_smb() {
    local max=15
    local attempt=1
    local host="${1:-localhost}"

    log_info "Waiting for SMB adapter on ${host}:${SMB_PORT}..."

    while [ "$attempt" -le "$max" ]; do
        # Try nc first, fall back to /dev/tcp for minimal containers without nc
        if nc -z "$host" "$SMB_PORT" 2>/dev/null || (echo >/dev/tcp/"$host"/"$SMB_PORT") 2>/dev/null; then
            log_info "SMB adapter is listening on port ${SMB_PORT}"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "SMB adapter not listening after ${max}s"
    return 1
}

# Create metadata store based on profile
create_metadata_store() {
    log_info "Creating metadata store for profile: ${PROFILE}"

    case "$PROFILE" in
        memory|memory-fs|memory-kerberos)
            $DFSCTL store metadata add --name default --type memory
            ;;
        badger*)
            $DFSCTL store metadata add --name default --type badger \
                --config '{"db_path":"/data/metadata"}'
            ;;
        postgres*)
            $DFSCTL store metadata add --name default --type postgres \
                --config '{"host":"postgres","port":5432,"user":"dittofs","password":"dittofs","database":"dittofs_test","sslmode":"disable"}'
            ;;
        *)
            log_error "Unknown profile: ${PROFILE}"
            return 1
            ;;
    esac
}

# Create block stores based on profile
create_block_stores() {
    log_info "Creating block stores for profile: ${PROFILE}"

    case "$PROFILE" in
        memory|memory-kerberos)
            $DFSCTL store block local add --name default --type memory
            ;;
        *-s3-legacy|*-fs)
            # Legacy profile names kept for CI compatibility.
            # Filesystem block store was removed in Phase 42; these use memory.
            $DFSCTL store block local add --name default --type memory
            ;;
        *-s3)
            $DFSCTL store block local add --name default --type memory
            $DFSCTL store block remote add --name default --type s3 \
                --config '{"bucket":"dittofs-test","region":"us-east-1","endpoint":"http://localstack:4566","force_path_style":true}'
            ;;
        *)
            log_error "Unknown profile payload pattern: ${PROFILE}"
            return 1
            ;;
    esac
}

# Main bootstrap flow
main() {
    log_info "Starting DittoFS bootstrap (profile: ${PROFILE})"

    # Wait for API
    wait_for_ready

    # Login as admin
    log_info "Logging in as admin..."
    $DFSCTL login --server "$API_URL" --username admin --password "$ADMIN_PASSWORD"

    # Change password (required for new admin user on first login)
    log_info "Changing admin password (first login requirement)..."
    $DFSCTL user change-password --current "$ADMIN_PASSWORD" --new "$TEST_PASSWORD" 2>/dev/null || true

    # Re-login with new password
    $DFSCTL login --server "$API_URL" --username admin --password "$TEST_PASSWORD"

    # Create stores
    create_metadata_store
    create_block_stores

    # Create WPTS-required shares
    # FileShare is the default share name WPTS tests use for TREE_CONNECT
    log_info "Creating WPTS shares..."
    local share_flags="--metadata default --local default"
    if [[ "$PROFILE" == *-s3 ]]; then
        share_flags="$share_flags --remote default"
    fi
    # /smbbasic uses Windows-default ACL canonicalization (MS-DTYP §2.5.3.4.2):
    # AUTO_INHERITED is persisted only when SET_INFO Security also carries
    # AUTO_INHERIT_REQ. Required by smb2.acls.INHERITFLAGS and
    # smb2.acls.SDFLAGSVSCHOWN. The Samba extension
    # `acl flag inherited canonicalization = no` is exercised by
    # smb2.acls_non_canonical against /smbnoncanon below.
    #
    # --access-based-enumeration: smbtorture smb2.acls.ACCESSBASED runs against
    # the default share (/smbbasic) and asserts QUERY_DIRECTORY hides files the
    # caller cannot read. Other tests in the smb2.acls suite enumerate as the
    # file's owner with full rights, so ABE is a no-op for them — enabling the
    # flag here is safe and avoids needing a separate ABE-only share or
    # excluding ACCESSBASED from the suite filter. (Refs #532 / MS-SRVS
    # SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM / MS-SMB2 §2.2.10
    # SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM.)
    $DFSCTL share create --name /smbbasic --access-based-enumeration $share_flags
    $DFSCTL share create --name /smbencrypted --encrypt-data $share_flags
    $DFSCTL share create --name /fileshare $share_flags
    # Kept as the previous home of acls.ACCESSBASED (refs #532). Other test
    # paths may still expect a share named /hideunread; harmless duplicate.
    $DFSCTL share create --name /hideunread --access-based-enumeration $share_flags
    # /smbnoncanon disables MS-DTYP §2.5.3.4.2 canonicalization (Samba
    # `acl flag inherited canonicalization = no` extension). Target of
    # smb2.acls_non_canonical.flags; verifies AUTO_INHERITED round-trips
    # verbatim through SET_INFO Security without the AUTO_INHERIT_REQ gate.
    $DFSCTL share create --name /smbnoncanon --acl-canonicalize-inherited=false $share_flags

    # Create test users with DISTINCT UIDs. Without --uid each user falls back
    # to defaultUID=1000 in internal/adapter/smb/v2/handlers/auth_helper.go,
    # which collapses both identities onto the same POSIX UID. That collision
    # makes the second user match OWNER@ on files created by the first via
    # acl.Evaluate's UID == FileOwnerUID arm, leaking access to a "non-owner"
    # that is actually the same UID at the POSIX layer. smbtorture
    # smb2.acls.ACCESSBASED depends on these two principals being distinct.
    log_info "Creating test users..."
    $DFSCTL user create --username wpts-admin --password "$TEST_PASSWORD" --uid 1000
    $DFSCTL user create --username nonadmin   --password "$TEST_PASSWORD" --uid 1001

    # Identity mapping for Kerberos: the principal "wpts-admin@${KERBEROS_REALM}"
    # must resolve to the "wpts-admin" control plane user. Strip-realm would
    # already work implicitly, but we add an explicit mapping to exercise the
    # SMB identity mapping lookup path end-to-end.
    if is_kerberos_profile; then
        log_info "Creating Kerberos identity mapping (wpts-admin@${KERBEROS_REALM} -> wpts-admin)..."
        $DFSCTL idmap add --principal "wpts-admin@${KERBEROS_REALM}" --username wpts-admin
    fi

    # Enable SMB adapter
    log_info "Enabling SMB adapter on port ${SMB_PORT}..."
    $DFSCTL adapter enable smb --port "$SMB_PORT"

    # Enable encryption capability (preferred mode: advertise but don't enforce globally)
    log_info "Enabling SMB encryption (preferred mode)..."
    $DFSCTL adapter settings smb update --enable-encryption true

    # Restart adapter to apply encryption settings immediately.
    # The settings watcher polls every 10s, but tests start within seconds,
    # so we force a restart to pick up the settings synchronously.
    log_info "Restarting SMB adapter to apply encryption settings..."
    $DFSCTL adapter disable smb
    sleep 1
    $DFSCTL adapter enable smb --port "$SMB_PORT"

    # Wait for SMB adapter to start
    wait_for_smb localhost

    log_info "Bootstrap complete: shares=smbbasic,smbencrypted,fileshare,hideunread,smbnoncanon users=wpts-admin,nonadmin adapter=smb:${SMB_PORT}"
}

main "$@"
