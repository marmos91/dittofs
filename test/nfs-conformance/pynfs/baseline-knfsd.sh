#!/usr/bin/env bash
# Run the pynfs suite against the Linux kernel NFS server, for comparison.
#
# This is what stops KNOWN_FAILURES from becoming a place to park bugs. A pynfs
# failure means one of three different things, and only a second server can tell
# them apart:
#
#   knfsd passes, DittoFS fails  -> ours: a real protocol bug, or a feature we
#                                   deliberately do not implement
#   knfsd fails too              -> the assertion is not something a conformant
#                                   server is expected to satisfy; blacklisting
#                                   it says nothing about DittoFS
#
# The output is a Markdown table of per-test knfsd verdicts, written to
# baseline-knfsd.md and committed, so triage can cite it and later readers can
# see the evidence behind every `suite` row without re-running anything.
#
# Linux only: it needs the in-kernel nfsd. Not a CI gate — run it by hand or
# from the nightly job.
#
# Usage:
#   sudo ./baseline-knfsd.sh [--export-dir DIR] [--keep]

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

EXPORT_DIR="/srv/pynfs-baseline"
KEEP=false

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
log()       { echo -e "${GREEN}[BASELINE]${NC} $*"; }
log_error() { echo -e "${RED}[BASELINE]${NC} $*" >&2; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --export-dir) EXPORT_DIR="$2"; shift 2 ;;
        --keep)       KEEP=true; shift ;;
        --help|-h)
            echo "Usage: sudo $0 [--export-dir DIR] [--keep]"
            echo ""
            echo "Runs pynfs 4.0 and 4.1 against the Linux kernel NFS server and"
            echo "writes per-test verdicts to baseline-knfsd.md."
            exit 0
            ;;
        *) log_error "Unknown option: $1"; exit 1 ;;
    esac
done

if [[ "$(uname -s)" != "Linux" ]]; then
    log_error "The baseline needs the in-kernel nfsd, so it only runs on Linux."
    log_error "On macOS, run it from CI: gh workflow run nfs-pynfs.yml"
    exit 1
fi

if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root (sudo): it exports a filesystem."
    exit 1
fi

# --------------------------------------------------------------------------
# Locate pynfs.
# --------------------------------------------------------------------------
PYNFS_PREFIX=""
if ! command -v pynfs-4.0 >/dev/null 2>&1; then
    log "Building pynfs via nix..."
    PYNFS_OUT="$(nix build "${REPO_ROOT}#pynfs" --no-link --print-out-paths)" || {
        log_error "nix build .#pynfs failed"
        exit 1
    }
    PYNFS_PREFIX="${PYNFS_OUT}/bin/"
fi

# --------------------------------------------------------------------------
# Export a scratch directory over NFSv4.
# --------------------------------------------------------------------------
# `insecure` is required: pynfs connects from an unprivileged source port,
# which knfsd rejects by default. `fsid=0` makes this the NFSv4 pseudo-root, so
# the export path pynfs asks for is "/".
EXPORT_OPTS="rw,insecure,no_root_squash,no_subtree_check,fsid=0"

cleanup() {
    if [[ "$KEEP" == true ]]; then
        log "Leaving the export in place (--keep)."
        return
    fi
    log "Removing the export..."
    exportfs -u "*:${EXPORT_DIR}" 2>/dev/null || true
    rm -rf "$EXPORT_DIR"
}
trap cleanup EXIT

if ! command -v exportfs >/dev/null 2>&1; then
    log "Installing nfs-kernel-server..."
    apt-get update -qq && apt-get install -y -qq nfs-kernel-server || {
        log_error "Could not install nfs-kernel-server"
        exit 1
    }
fi

log "Exporting ${EXPORT_DIR} (${EXPORT_OPTS})"
mkdir -p "$EXPORT_DIR"
chmod 777 "$EXPORT_DIR"
systemctl start nfs-server 2>/dev/null || service nfs-kernel-server start || true
exportfs -o "$EXPORT_OPTS" "*:${EXPORT_DIR}" || {
    log_error "exportfs failed — is nfsd running? (modprobe nfsd)"
    exit 1
}
exportfs -v

RESULTS_DIR="${SCRIPT_DIR}/results/baseline-knfsd-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"

# --------------------------------------------------------------------------
# Run both trees. Flags must match run-pynfs.sh exactly, or the comparison is
# between two different experiments.
# --------------------------------------------------------------------------
for v in 4.0 4.1; do
    case "$v" in
        4.0) minor=0 ;;
        4.1) minor=1 ;;
    esac
    log "Running pynfs ${v} against knfsd..."
    "${PYNFS_PREFIX}pynfs-${v}" "localhost:2049/" \
        --minorversion "$minor" \
        --security sys \
        --uid 0 --gid 0 \
        --maketree \
        all > "${RESULTS_DIR}/knfsd-v${v}.log" 2>&1
    tail -2 "${RESULTS_DIR}/knfsd-v${v}.log"
done

# --------------------------------------------------------------------------
# Emit the evidence table.
# --------------------------------------------------------------------------
OUT="${SCRIPT_DIR}/baseline-knfsd.md"
KERNEL="$(uname -r)"
{
    echo "# pynfs against Linux knfsd — baseline"
    echo ""
    echo "Generated by \`baseline-knfsd.sh\`. Do not hand-edit."
    echo ""
    echo "Every pynfs failure is one of: a DittoFS bug, a feature DittoFS does not"
    echo "implement, or an assertion no server satisfies. This table separates the"
    echo "third case from the first two by measuring rather than assuming: a test"
    echo "that knfsd also fails is evidence about the suite, not about DittoFS."
    echo ""
    echo "- Kernel: \`${KERNEL}\`"
    echo "- Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo ""
    for v in 4.0 4.1; do
        log_file="${RESULTS_DIR}/knfsd-v${v}.log"
        echo "## NFSv${v}"
        echo ""
        grep -E '^Of those: ' "$log_file" | tail -1
        echo ""
        echo "| Test | knfsd |"
        echo "|------|-------|"
        awk '/ : (PASS|FAILURE|WARNING|UNSUPPORTED|OMIT)[[:space:]]*$/ {
                 code = $1; verdict = $NF;
                 printf "| %s | %s |\n", code, verdict
             }' "$log_file" | sort -u
        echo ""
    done
} > "$OUT"

log "Wrote $OUT"
log "Logs: $RESULTS_DIR"
