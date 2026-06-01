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
#
#   # Reset a single share to clean state (delete + recreate). Used by the
#   # smbtorture runner between sub-suites to clear cross-test state leak
#   # (leftover restrictive DACLs, non-empty dirs, dangling opens + leases).
#   # Refs #568.
#   DFSCTL=/app/dfsctl API_URL=http://localhost:8080 \
#     ADMIN_PASSWORD=... ./bootstrap.sh reset-share /smbbasic

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
                --config '{"bucket":"dittofs-test","region":"us-east-1","endpoint":"http://localstack:4566","force_path_style":true,"access_key_id":"test","secret_access_key":"test"}'
            ;;
        *)
            log_error "Unknown profile payload pattern: ${PROFILE}"
            return 1
            ;;
    esac
}

# Canonical list of WPTS/smbtorture shares. The smbtorture runner (run.sh)
# resets these between sub-suites via `reset-share`, so the create flags must
# live in exactly one place. Keep this in sync with create_shares() below.
SHARE_NAMES=(
    /smbbasic
    /smbencrypted
    /fileshare
    /hideunread
    /change_notify_disabled
    /smbnoncanon
    /create_no_streams
)

# share_create_args NAME — echo the per-share `dfsctl share create` flags
# (without the common --metadata/--local/--remote flags, which the caller
# appends). This is the single source of truth for share configuration,
# shared by initial bootstrap and by reset-share.
share_create_args() {
    case "$1" in
        # /smbbasic uses Windows-default ACL canonicalization (MS-DTYP
        # §2.5.3.4.2): AUTO_INHERITED is persisted only when SET_INFO Security
        # also carries AUTO_INHERIT_REQ. Required by smb2.acls.INHERITFLAGS and
        # smb2.acls.SDFLAGSVSCHOWN.
        /smbbasic) echo "--name /smbbasic" ;;
        /smbencrypted) echo "--name /smbencrypted --encrypt-data" ;;
        /fileshare) echo "--name /fileshare" ;;
        # Refs #532: smbtorture smb2.acls.ACCESSBASED connects to a share with
        # the MS-SRVS SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM flag set so
        # QUERY_DIRECTORY hides entries the caller cannot read.
        /hideunread) echo "--name /hideunread --access-based-enumeration" ;;
        # /change_notify_disabled rejects SMB2 CHANGE_NOTIFY with
        # STATUS_NOT_IMPLEMENTED. Target of smb2.change_notify_disabled.
        /change_notify_disabled) echo "--name /change_notify_disabled --change-notify-disabled" ;;
        # /smbnoncanon disables MS-DTYP §2.5.3.4.2 canonicalization (Samba
        # `acl flag inherited canonicalization = no` extension). Target of
        # smb2.acls_non_canonical.flags.
        /smbnoncanon) echo "--name /smbnoncanon --acl-canonicalize-inherited=false" ;;
        # /create_no_streams rejects SMB2 Alternate Data Stream opens with
        # STATUS_OBJECT_NAME_INVALID (Samba `smbd:streams = no` semantics).
        /create_no_streams) echo "--name /create_no_streams --streams-disabled" ;;
        *)
            log_error "Unknown share: $1"
            return 1
            ;;
    esac
}

# common_share_flags — the --metadata/--local[/--remote] flags shared by every
# share, derived from the active profile.
common_share_flags() {
    local flags="--metadata default --local default"
    if [[ "$PROFILE" == *-s3 ]]; then
        flags="$flags --remote default"
    fi
    echo "$flags"
}

# create_one_share NAME — create a single share from the canonical map,
# appending the profile-derived common flags.
create_one_share() {
    # shellcheck disable=SC2046,SC2086  # word-splitting of flag strings is intended
    $DFSCTL share create $(share_create_args "$1") $(common_share_flags)
}

# create_shares — create every WPTS/smbtorture share from the canonical map.
create_shares() {
    log_info "Creating WPTS shares..."
    local name
    for name in "${SHARE_NAMES[@]}"; do
        create_one_share "$name"
    done
}

# reset_share NAME — delete and recreate a single share, returning it to clean
# state (no files, no dirs, no leftover DACLs, no dangling opens or leases).
# Used by run.sh between smbtorture sub-suites. Refs #568.
#
# Delete+recreate is the most thorough reset available: it runs as an admin
# REST op (bypassing any restrictive SMB DACLs a failed ACL test left behind)
# and tears down the share's metadata root + block store, which drops every
# server-side open and lease record bound to it. It does NOT touch users,
# identity mappings, or the SMB adapter config.
reset_share() {
    local name="$1"
    wait_for_ready
    # main() rotates the admin password to TEST_PASSWORD on first login, so by
    # the time the runner calls reset-share the original ADMIN_PASSWORD is dead.
    # Log in with TEST_PASSWORD here (the post-bootstrap admin password).
    log_info "Logging in as admin..."
    $DFSCTL login --server "$API_URL" --username admin --password "$TEST_PASSWORD"

    log_info "Resetting share ${name} (delete + recreate)..."
    $DFSCTL share delete "$name" --force
    create_one_share "$name"
    log_info "Share ${name} reset to clean state"
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

    # Create WPTS-required shares from the canonical share map (single source
    # of truth in share_create_args, reused by reset-share). FileShare is the
    # default share name WPTS tests use for TREE_CONNECT.
    create_shares

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

    log_info "Bootstrap complete: shares=smbbasic,smbencrypted,fileshare,hideunread,change_notify_disabled,smbnoncanon,create_no_streams users=wpts-admin,nonadmin adapter=smb:${SMB_PORT}"
}

# Subcommand dispatch. Default (no args) runs the full bootstrap; `reset-share
# <name>` resets a single share between smbtorture sub-suites (refs #568).
case "${1:-}" in
    reset-share)
        if [[ -z "${2:-}" ]]; then
            log_error "reset-share requires a share name"
            exit 1
        fi
        reset_share "$2"
        ;;
    "")
        main
        ;;
    *)
        log_error "Unknown command: $1"
        exit 1
        ;;
esac
