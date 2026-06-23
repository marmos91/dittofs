#!/usr/bin/env bash
#
# pcap-diff.sh — structural wire-diff of a DittoFS capture against a reference
# capture from an official server, for the AD interop corpus (#1237).
#
# It decodes both pcaps with tshark, reduces each to a canonical structural
# skeleton (normalize.py — field shape, volatile values stripped), and diffs
# the skeletons. A non-empty diff means DittoFS emits a structurally different
# wire form than the reference (a missing/extra field, option, or flag) — the
# class of interop bug that "it compiles and a client connects" hides.
#
# Exit status: 0 = structurally identical, 1 = divergence (diff printed), 2 = usage/tooling error.
#
# Usage:
#   pcap-diff.sh --reference ref.pcap --candidate dittofs.pcap --proto kerberos \
#                [--candidate-port 12445] [--proto ldap ...]
#
# DittoFS runs SMB/NFS on non-standard ports; pass --candidate-port so tshark
# applies the right dissector to the candidate capture (the reference, from a
# real server, uses the well-known port and needs no hint).

set -euo pipefail

REF="" CAND="" CAND_PORT="" PROTOS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --reference) REF="$2"; shift 2 ;;
    --candidate) CAND="$2"; shift 2 ;;
    --candidate-port) CAND_PORT="$2"; shift 2 ;;
    --proto) PROTOS+=("--proto" "$2"); shift 2 ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

[[ -z "$REF" || -z "$CAND" ]] && { echo "need --reference and --candidate" >&2; exit 2; }
command -v tshark >/dev/null || { echo "tshark not found" >&2; exit 2; }
HERE="$(cd "$(dirname "$0")" && pwd)"

# Dissector hint maps a non-standard candidate port onto the SMB (nbss) or NFS
# dissector. Kerberos/LDAP autodetect on their well-known ports either way.
hint=()
if [[ -n "$CAND_PORT" ]]; then
  hint=(-d "tcp.port==${CAND_PORT},nbss")
fi

skel() { # <pcap> <extra tshark args...>
  local pcap="$1"; shift
  tshark -r "$pcap" -T pdml "$@" 2>/dev/null \
    | python3 "${HERE}/normalize.py" "${PROTOS[@]}"
}

ref_skel="$(mktemp)"; cand_skel="$(mktemp)"
trap 'rm -f "$ref_skel" "$cand_skel"' EXIT

skel "$REF"            > "$ref_skel"
skel "$CAND" "${hint[@]}" > "$cand_skel"

if diff -u --label "reference:$(basename "$REF")" --label "dittofs:$(basename "$CAND")" \
        "$ref_skel" "$cand_skel"; then
  echo "OK: structurally identical ($(wc -l < "$ref_skel" | tr -d ' ') fields)"
  exit 0
else
  echo "DIVERGENCE: DittoFS wire structure differs from reference (see -/+ above)" >&2
  exit 1
fi
