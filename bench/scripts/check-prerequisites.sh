#!/usr/bin/env bash
set -euo pipefail

# Benchmark suite prerequisites checker
# Validates that all required tools are installed and functional.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

MISSING=0

# ---------------------------------------------------------------------------
# Check helpers
# ---------------------------------------------------------------------------

check_required() {
    local cmd="${1:?}"
    local hint="${2:-}"

    if command -v "$cmd" >/dev/null 2>&1; then
        log_info "Found: $cmd ($(command -v "$cmd"))"
    else
        log_error "Missing required tool: $cmd"
        if [ -n "$hint" ]; then
            log_error "  Install: $hint"
        fi
        MISSING=$(( MISSING + 1 ))
    fi
}

check_optional() {
    local cmd="${1:?}"
    local purpose="${2:-}"

    if command -v "$cmd" >/dev/null 2>&1; then
        log_info "Found (optional): $cmd ($(command -v "$cmd"))"
    else
        log_warn "Optional tool not found: $cmd"
        if [ -n "$purpose" ]; then
            log_warn "  Purpose: $purpose"
        fi
    fi
}

# ---------------------------------------------------------------------------
# Required tools
# ---------------------------------------------------------------------------
log_info "Checking required tools..."
echo

check_required "docker"  "https://docs.docker.com/get-docker/"
check_required "fio"     "apt install fio / brew install fio"
check_required "jq"      "apt install jq / brew install jq"
check_required "bc"      "apt install bc / brew install bc"
check_required "make"    "apt install make / xcode-select --install"
check_required "curl"    "apt install curl / brew install curl"

# ---------------------------------------------------------------------------
# Docker Compose v2 (special check -- it's a plugin, not a standalone binary)
# ---------------------------------------------------------------------------
echo
log_info "Checking Docker Compose v2..."

if docker compose version >/dev/null 2>&1; then
    log_info "Found: docker compose ($(docker compose version --short 2>/dev/null || echo 'unknown version'))"
else
    log_error "Missing: Docker Compose v2 plugin"
    log_error "  Install: https://docs.docker.com/compose/install/"
    MISSING=$(( MISSING + 1 ))
fi

# ---------------------------------------------------------------------------
# Docker daemon running
# ---------------------------------------------------------------------------
log_info "Checking Docker daemon..."

if docker info >/dev/null 2>&1; then
    log_info "Docker daemon is running"
else
    log_error "Docker daemon is not running"
    log_error "  Start Docker Desktop or run: sudo systemctl start docker"
    MISSING=$(( MISSING + 1 ))
fi

# ---------------------------------------------------------------------------
# Optional tools
# ---------------------------------------------------------------------------
echo
log_info "Checking optional tools..."

check_optional "python3"   "Analysis pipeline (Phase 37)"
check_optional "smbclient" "SMB benchmark verification"
check_optional "showmount" "NFS mount verification"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo

if [ "$MISSING" -eq 0 ]; then
    log_info "All required prerequisites found. Ready to benchmark."
else
    log_error "${MISSING} required prerequisite(s) missing."
fi

exit "$MISSING"
