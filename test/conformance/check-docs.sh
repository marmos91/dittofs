#!/usr/bin/env bash
# Keeps the documentation's conformance tables generated from suites.json.
#
# The tables in docs/internals/ used to be maintained by hand and had already
# drifted from the workflows they described. They are now rendered between
# markers from the manifest, so the manifest is the only place a suite, a
# profile or a tier is written down.
#
# Usage:
#   ./check-docs.sh            # fail if a table is stale (what CI runs)
#   ./check-docs.sh --write    # rewrite the tables in place

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
MANIFEST="${SCRIPT_DIR}/suites.json"

BEGIN='<!-- conformance-suites:begin -->'
END='<!-- conformance-suites:end -->'

DOCS=(
    "docs/internals/testing.md"
    "docs/internals/contributing.md"
)

WRITE=false
[[ "${1:-}" == "--write" ]] && WRITE=true

render() {
    echo "$BEGIN"
    echo "<!-- Generated from test/conformance/suites.json by test/conformance/check-docs.sh. Do not edit by hand. -->"
    echo ""
    echo "| Suite | Protocol | Profiles | Variants | Presubmit | Known failures |"
    echo "|---|---|---|---|---|---|"
    jq -r '
        def cell($x): if ($x | length) == 0 then "—" else ($x | join(", ")) end;
        .defaults as $d
        | .suites
        | to_entries[]
        | .key as $name
        | .value as $s
        | ($s.tiers.pull_request // $d.tiers.pull_request // "all") as $pr
        | (if $pr == "all" then $s.profiles else $pr end) as $prlist
        | ($s.known_failures
           | if . == null then []
             elif type == "string" then [.]
             else ([.[]] | unique)
             end) as $kf
        | "| `\($name)` | \($s.protocol) | \(cell($s.profiles | map("`\(.)`")))"
          + " | \(cell($s.variant.values // [] | map("`\(.)`")))"
          + " | \(cell($prlist | map("`\(.)`")))"
          + " | \(cell($kf | map("[`test/\(.)`](../../test/\(.))")))"
          + " |"
    ' "$MANIFEST"
    echo ""
    echo "Tiering, profiles and blacklists come from"
    echo "[\`test/conformance/suites.json\`](../../test/conformance/suites.json); every suite runs through"
    echo "[\`test/conformance/run.sh\`](../../test/conformance/run.sh)."
    echo "$END"
}

TABLE="$(render)"

status=0
for doc in "${DOCS[@]}"; do
    path="${REPO_ROOT}/${doc}"
    if ! grep -qF "$BEGIN" "$path" || ! grep -qF "$END" "$path"; then
        echo "ERROR: ${doc} has no conformance-suites markers" >&2
        status=1
        continue
    fi

    updated="$(awk -v begin="$BEGIN" -v end="$END" -v table="$TABLE" '
        $0 == begin { print table; skip = 1; next }
        $0 == end   { skip = 0; next }
        !skip       { print }
    ' "$path")"

    if [[ "$updated" == "$(cat "$path")" ]]; then
        echo "ok: ${doc}"
        continue
    fi

    if [[ "$WRITE" == true ]]; then
        printf '%s\n' "$updated" >"$path"
        echo "wrote: ${doc}"
    else
        echo "STALE: ${doc} — run test/conformance/check-docs.sh --write" >&2
        diff <(cat "$path") <(printf '%s\n' "$updated") || true
        status=1
    fi
done

exit "$status"
