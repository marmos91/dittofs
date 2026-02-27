#!/usr/bin/env bash
set -euo pipefail

# Benchmark suite cleanup script
# Safely tears down containers, volumes, and NFS/SMB mounts.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

FORCE=false

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
for arg in "$@"; do
    case "$arg" in
        --force) FORCE=true ;;
        -h|--help)
            echo "Usage: $(basename "$0") [--force]"
            echo
            echo "Cleans up benchmark infrastructure: unmounts NFS/SMB, stops containers, removes volumes."
            echo
            echo "Options:"
            echo "  --force    Skip confirmation prompt and prune Docker build cache"
            exit 0
            ;;
        *)
            die "Unknown argument: $arg. Use --help for usage."
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Confirmation (unless --force)
# ---------------------------------------------------------------------------
if [ "$FORCE" = false ]; then
    log_warn "This will stop all benchmark containers, remove volumes, and unmount NFS/SMB shares."
    read -rp "Continue? [y/N] " confirm
    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        log_info "Aborted."
        exit 0
    fi
fi

OS=$(detect_os)

# ---------------------------------------------------------------------------
# Unmount helper -- tries graceful, then forced, then lazy (Linux only)
# ---------------------------------------------------------------------------
try_unmount() {
    local mp="$1"
    if sudo umount "$mp" 2>/dev/null; then return 0; fi
    if sudo umount -f "$mp" 2>/dev/null; then return 0; fi
    if [ "$OS" = "linux" ] && sudo umount -l "$mp" 2>/dev/null; then return 0; fi
    log_warn "  Could not unmount $mp (may not be mounted)"
}

# ---------------------------------------------------------------------------
# Step 1: Unmount NFS/SMB mounts
# ---------------------------------------------------------------------------
log_info "Step 1: Unmounting NFS/SMB benchmark mounts..."

JUICEFS_MOUNT="${JUICEFS_MOUNT:-/tmp/bench-juicefs}"

for mount_prefix in /mnt/bench /tmp/bench "${JUICEFS_MOUNT}"; do
    while IFS= read -r mount_point; do
        [ -z "$mount_point" ] && continue
        log_info "  Unmounting: $mount_point"
        try_unmount "$mount_point"
    done < <(mount | awk -v prefix="$mount_prefix" '$3 ~ "^"prefix {print $3}')
done

log_info "  Mount cleanup complete."

# ---------------------------------------------------------------------------
# Step 2: Stop Docker Compose services
# ---------------------------------------------------------------------------
log_info "Step 2: Stopping benchmark Docker Compose services..."

BENCH_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

if [ -f "${BENCH_DIR}/docker-compose.yml" ]; then
    (cd "$BENCH_DIR" && docker compose down -v --remove-orphans 2>/dev/null) || log_warn "  No running Compose services found."
else
    log_warn "  No docker-compose.yml found in ${BENCH_DIR}, skipping."
fi

log_info "  Docker Compose cleanup complete."

# ---------------------------------------------------------------------------
# Step 3: Remove dangling benchmark volumes
# ---------------------------------------------------------------------------
log_info "Step 3: Removing dangling benchmark volumes..."

BENCH_VOLUMES=$(docker volume ls -q --filter name=bench 2>/dev/null || true)
if [ -n "$BENCH_VOLUMES" ]; then
    echo "$BENCH_VOLUMES" | xargs docker volume rm 2>/dev/null || log_warn "  Some volumes could not be removed (may be in use)."
    log_info "  Removed benchmark volumes."
else
    log_info "  No dangling benchmark volumes found."
fi

# ---------------------------------------------------------------------------
# Step 4: Prune Docker build cache (only with --force)
# ---------------------------------------------------------------------------
if [ "$FORCE" = true ]; then
    log_info "Step 4: Pruning Docker build cache..."
    docker builder prune -f 2>/dev/null || log_warn "  Docker builder prune failed."
    log_info "  Docker build cache pruned."
else
    log_info "Step 4: Skipping Docker build cache prune (use --force to enable)."
fi

log_info "Cleanup complete."
