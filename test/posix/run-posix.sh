#!/usr/bin/env bash
# Run POSIX compliance tests for DittoFS
#
# Usage (from nix shell):
#   cd /tmp/dittofs-test
#   sudo env PATH="$PATH" /path/to/run-posix.sh [--nfs-version 3|4|4.0] [test_pattern]
#
# Examples:
#   sudo env PATH="$PATH" ./run-posix.sh                        # Run all tests
#   sudo env PATH="$PATH" ./run-posix.sh chmod                  # Run chmod tests
#   sudo env PATH="$PATH" ./run-posix.sh 'chmod/*.t'            # Run specific test pattern
#   sudo env PATH="$PATH" ./run-posix.sh --nfs-version 4        # Run all tests (log NFSv4)
#   sudo env PATH="$PATH" ./run-posix.sh --nfs-version 4 chmod  # Run chmod tests (log NFSv4)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MOUNT_POINT="${DITTOFS_MOUNT:-/tmp/dittofs-test}"
NFS_VERSION=""

# Parse --nfs-version if provided (for informational logging)
POSITIONAL_ARGS=()
while [[ $# -gt 0 ]]; do
    case $1 in
        --nfs-version)
            NFS_VERSION="${2:-}"
            shift 2
            ;;
        --nfs-version=*)
            NFS_VERSION="${1#*=}"
            shift
            ;;
        *)
            POSITIONAL_ARGS+=("$1")
            shift
            ;;
    esac
done
# Restore positional arguments
set -- "${POSITIONAL_ARGS[@]+"${POSITIONAL_ARGS[@]}"}"

# Check if running as root
if [[ $EUID -ne 0 ]]; then
    echo "Error: This script must be run as root (sudo)"
    exit 1
fi

# Check if mount point exists and is mounted
if ! mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
    echo "Error: $MOUNT_POINT is not mounted"
    echo ""
    echo "Start DittoFS and mount with:"
    echo "  ./dfs start"
    echo "  sudo mkdir -p $MOUNT_POINT"
    echo "  sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,nolock localhost:/export $MOUNT_POINT"
    exit 1
fi

# Helper function to check if a pjdfstest binary has tests directory
has_tests_dir() {
    local bin="$1"
    local tests_dir
    tests_dir="$(dirname "$bin")/../share/pjdfstest/tests"
    [[ -d "$tests_dir" ]]
}

# Find pjdfstest binary that has tests directory
PJDFSTEST_BIN=""
TESTS_DIR=""

# First, try pjdfstest from PATH (works when run from nix develop shell)
if command -v pjdfstest &>/dev/null; then
    candidate="$(which pjdfstest)"
    if has_tests_dir "$candidate"; then
        PJDFSTEST_BIN="$candidate"
    fi
fi

# If not found or doesn't have tests, search nix store for one that does
if [[ -z "$PJDFSTEST_BIN" ]]; then
    for candidate in $(find /nix/store -path "*/bin/pjdfstest" -type f -executable 2>/dev/null); do
        if has_tests_dir "$candidate"; then
            PJDFSTEST_BIN="$candidate"
            break
        fi
    done
fi

if [[ -z "$PJDFSTEST_BIN" ]]; then
    echo "Error: pjdfstest not found (or no version with tests directory)"
    echo "Enter nix development shell: nix develop"
    exit 1
fi

TESTS_DIR="$(dirname "$PJDFSTEST_BIN")/../share/pjdfstest/tests"

# pjdfstest's misc.sh searches for the binary by traversing up the directory tree.
# Since nix store is read-only and the binary is in bin/ (not a parent of tests/),
# we need to create a working directory with a symlink to the binary.
WORK_DIR=$(mktemp -d)
trap "rm -rf '$WORK_DIR'" EXIT

# Create symlink to pjdfstest binary at the working directory root
ln -s "$PJDFSTEST_BIN" "$WORK_DIR/pjdfstest"

# Copy the tests directory structure (symlinks to actual test files)
# The conf file is inside tests/, so cp -rs includes it
cp -rs "$TESTS_DIR" "$WORK_DIR/tests"

cd "$MOUNT_POINT"

# Determine effective NFS version (from flag or mount detection)
EFFECTIVE_NFS_VERSION="$NFS_VERSION"
if [[ -z "$EFFECTIVE_NFS_VERSION" ]]; then
    EFFECTIVE_NFS_VERSION=$(mount | grep "$MOUNT_POINT" | sed -n 's/.*vers=\([0-9.]*\).*/\1/p' 2>/dev/null || echo "")
fi

# Select known failures file based on NFS version
case "$EFFECTIVE_NFS_VERSION" in
    4|4.0|4.1|4.2)
        KNOWN_FAILURES_FILE="$SCRIPT_DIR/known_failures_v4.txt"
        ;;
    *)
        KNOWN_FAILURES_FILE="$SCRIPT_DIR/known_failures.txt"
        ;;
esac

# Parse file-level exclusion patterns from known failures file
EXCLUDE_PATTERNS=()
if [[ -f "$KNOWN_FAILURES_FILE" ]]; then
    while IFS='|' read -r pattern _; do
        # Trim leading/trailing whitespace
        pattern="${pattern#"${pattern%%[![:space:]]*}"}"
        pattern="${pattern%"${pattern##*[![:space:]]}"}"
        # Skip comments and empty lines
        [[ -z "$pattern" || "$pattern" == \#* ]] && continue
        # Skip non-file patterns (:: separator = test name, not file path)
        [[ "$pattern" == *::* ]] && continue
        # Strip subtest marker (:testN) to get file-level path
        pattern="${pattern%%:test[0-9]*}"
        EXCLUDE_PATTERNS+=("$pattern")
    done < "$KNOWN_FAILURES_FILE"
fi

# Check if a test file matches any exclusion pattern
is_excluded() {
    local test_path="$1"  # relative to tests dir, e.g. "unlink/14.t"
    for pat in "${EXCLUDE_PATTERNS[@]}"; do
        # shellcheck disable=SC2254
        case "$test_path" in
            $pat) return 0 ;;
        esac
        # Try with .t suffix for patterns without extension (e.g. "open/etxtbsy")
        if [[ "$pat" != *.t && "$pat" != *\* ]]; then
            # shellcheck disable=SC2254
            case "$test_path" in
                ${pat}.t) return 0 ;;
                ${pat}/*) return 0 ;;
            esac
        fi
    done
    return 1
}

echo "Running POSIX compliance tests..."
echo "Mount point: $MOUNT_POINT"
if [[ -n "$NFS_VERSION" ]]; then
    echo "NFS version: NFSv${NFS_VERSION}"
elif [[ -n "$EFFECTIVE_NFS_VERSION" ]]; then
    echo "NFS version: NFSv${EFFECTIVE_NFS_VERSION} (detected from mount)"
else
    echo "NFS version: unknown (use --nfs-version to specify)"
fi
echo "Tests directory: $WORK_DIR/tests"
echo "pjdfstest binary: $PJDFSTEST_BIN"
if [[ ${#EXCLUDE_PATTERNS[@]} -gt 0 ]]; then
    echo "Known failures: ${#EXCLUDE_PATTERNS[@]} patterns from $(basename "$KNOWN_FAILURES_FILE")"
fi
echo ""

# Collect test files, filtering out known failures
collect_tests() {
    local search_dir="$1"
    find -L "$search_dir" -name '*.t' -type f 2>/dev/null | sort | while IFS= read -r test_file; do
        local rel_path="${test_file#$WORK_DIR/tests/}"
        if ! is_excluded "$rel_path"; then
            echo "$test_file"
        fi
    done
}

if [[ $# -gt 0 ]]; then
    TEST_FILES=$(collect_tests "$WORK_DIR/tests/$1")
else
    TEST_FILES=$(collect_tests "$WORK_DIR/tests")
fi

if [[ -z "$TEST_FILES" ]]; then
    echo "No test files found (all excluded or missing)"
    exit 1
fi

# shellcheck disable=SC2086
exec prove -rv $TEST_FILES
