#!/bin/bash
# Run the AD-1 PAC group-SID integration test against a real Samba AD-DC in
# Docker. Builds + provisions a fresh domain (slow under linux/amd64 emulation
# on Apple Silicon), so the timeout is generous.
#
# Usage:
#   ./run.sh          # Run the test
#   ./run.sh -v       # Verbose output
set -euo pipefail

cd "$(dirname "$0")/../../.."

echo "=== AD-DC PAC Group-SID Integration Test (AD-1) ==="
echo "Requires: Docker"
echo ""

exec go test -tags=ad_dc -timeout 20m ./test/integration/ad-dc/ "$@"
