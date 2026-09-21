#!/bin/bash
# Boundary cut-cost measurement for internal/adapter/smb/handlers.
#
# The figure in the roadmap goes stale every time a package is extracted out of
# handlers, and it has now been wrong twice. Re-run this rather than quoting it.
#
#   ./<this> <worktree> <pkgname> <file.go> [file.go ...]
#
# It moves the named files into handlers/<pkgname>/ as their own package, builds
# with -gcflags=-e so every error is reported rather than the first ten, and
# counts undefined symbols in both directions: what the new package needs back
# from handlers (the import cycle, and the real cost), and what handlers needs
# from the new package (an export plus an import, which is cheap).
#
# Run it against the pre-extraction commit too, or the delta means nothing.
set -u
WT=$1; shift
H=$WT/internal/adapter/smb/handlers
OUT=$(mktemp -d)
PKG=$1; shift

# reset: drop any previous subpackage dirs, restore originals from HEAD
rm -rf $H/info $H/create $H/state
cd $WT && git restore --source=HEAD --staged --worktree internal/adapter/smb/handlers

mkdir -p $H/$PKG
for f in "$@"; do
  mv "$H/$f" "$H/$PKG/$f" || exit 1
  sed -i.bak "s/^package handlers\$/package $PKG/" "$H/$PKG/$f" && rm -f "$H/$PKG/$f.bak"
done
grep -h '^package ' $H/$PKG/*.go | sort -u | sed "s/^/  pkg clause: /"

cd $WT && go build -gcflags=-e ./internal/adapter/smb/... > /dev/null 2> $OUT/err_$PKG.txt

grep -E "^internal/adapter/smb/handlers/$PKG/" $OUT/err_$PKG.txt \
  | grep -oE 'undefined: [A-Za-z_][A-Za-z0-9_]*' | sed 's/undefined: //' | sort > $OUT/need_$PKG.txt
grep -E "^internal/adapter/smb/handlers/[a-z_0-9]+\.go" $OUT/err_$PKG.txt \
  | grep -oE 'undefined: [A-Za-z_][A-Za-z0-9_]*' | sed 's/undefined: //' | sort > $OUT/prov_$PKG.txt

ALIASES="CreateContext OpenName OpenFile SMBResponseBase TreeConnection"
FREE=""; for s in $ALIASES; do grep -qx "$s" $OUT/need_$PKG.txt && FREE="$FREE $s"; done
NFREE=$(echo $FREE | wc -w | tr -d ' ')
NA=$(sort -u $OUT/need_$PKG.txt | wc -l | tr -d ' ')

echo "=== $PKG — $# files, $(cat $H/$PKG/*.go | wc -l | tr -d ' ') LOC ==="
echo "total compile errors:        $(grep -cE '^internal/adapter/smb' $OUT/err_$PKG.txt)"
echo "  in the new subpackage:     $(grep -cE "^internal/adapter/smb/handlers/$PKG/" $OUT/err_$PKG.txt)"
echo "  in remaining handlers:     $(grep -cE "^internal/adapter/smb/handlers/[a-z_0-9]+\.go" $OUT/err_$PKG.txt)"
echo "  elsewhere under smb/:      $(grep -E '^internal/adapter/smb' $OUT/err_$PKG.txt | grep -vcE '^internal/adapter/smb/handlers/')"
echo "A. needs BACK from handlers: $NA distinct / $(wc -l < $OUT/need_$PKG.txt | tr -d ' ') refs   [free aliases: $NFREE ->$FREE]"
echo "   REAL CYCLE SYMBOLS:       $((NA - NFREE))"
echo "B. handlers needs FROM it:   $(sort -u $OUT/prov_$PKG.txt | wc -l | tr -d ' ') distinct / $(wc -l < $OUT/prov_$PKG.txt | tr -d ' ') refs"
echo
