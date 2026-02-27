# bench/scripts/lib/common.sh — Shared utility library for benchmark scripts
# Source this file; do NOT execute directly.
# The sourcing script must set: set -euo pipefail
# shellcheck shell=bash

# ---------------------------------------------------------------------------
# Color codes (safe for non-interactive terminals via printf)
# ---------------------------------------------------------------------------
RED='\033[0;31m'
# shellcheck disable=SC2034  # GREEN used by sourcing scripts
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# ---------------------------------------------------------------------------
# Logging helpers (output goes to stderr so stdout stays clean for data)
# ---------------------------------------------------------------------------
log_info() {
    printf "${BLUE}[INFO]${NC}  %s\n" "$*" >&2
}

log_warn() {
    printf "${YELLOW}[WARN]${NC}  %s\n" "$*" >&2
}

log_error() {
    printf "${RED}[ERROR]${NC} %s\n" "$*" >&2
}

die() {
    log_error "$@"
    exit 1
}

# ---------------------------------------------------------------------------
# Timer helpers
# ---------------------------------------------------------------------------

# Prints the current epoch seconds; capture with: start=$(timer_start)
timer_start() {
    date +%s
}

# Takes a start timestamp as $1, prints elapsed seconds.
# Usage: elapsed=$(timer_stop "$start")
timer_stop() {
    local start="${1:?usage: timer_stop <start_epoch>}"
    local end
    end=$(date +%s)
    echo "$(( end - start ))"
}

# ---------------------------------------------------------------------------
# OS detection
# ---------------------------------------------------------------------------
detect_os() {
    case "$(uname -s)" in
        Linux)  echo "linux"  ;;
        Darwin) echo "macos"  ;;
        *)      die "Unsupported OS: $(uname -s). Only Linux and macOS are supported." ;;
    esac
}

# ---------------------------------------------------------------------------
# Docker Compose v2 validation
# ---------------------------------------------------------------------------
require_docker_compose_v2() {
    if ! docker compose version >/dev/null 2>&1; then
        die "Docker Compose v2 is required (docker compose plugin). Install: https://docs.docker.com/compose/install/"
    fi
}

# ---------------------------------------------------------------------------
# Wait for a Docker Compose service to become healthy
# Usage: wait_healthy <service_name> [timeout_seconds]
# ---------------------------------------------------------------------------
wait_healthy() {
    local service="${1:?usage: wait_healthy <service> [timeout]}"
    local timeout="${2:-120}"
    local elapsed=0
    local status

    log_info "Waiting for ${service} to become healthy (timeout: ${timeout}s)..."

    while [ "$elapsed" -lt "$timeout" ]; do
        status=$(docker compose ps --format json "${service}" 2>/dev/null \
            | grep -o '"Health":"[^"]*"' \
            | head -1 \
            | cut -d'"' -f4) || true

        if [ "$status" = "healthy" ]; then
            log_info "${service} is healthy (took ${elapsed}s)"
            return 0
        fi

        sleep 2
        elapsed=$(( elapsed + 2 ))
    done

    log_error "${service} did not become healthy within ${timeout}s"
    return 1
}
