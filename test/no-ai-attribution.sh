#!/usr/bin/env bash
#
# No commit message credits an AI tool.
#
# The project rule is that commit messages and pull request bodies never name
# AI tooling or carry co-author trailers for it. Enforcement belongs in CI
# rather than in review, because the trailer arrives by default: the agent
# harness instructs sessions to append one, the project instructs them not to,
# and which of the two a given session followed is not visible in the diff.
# `06a6f2654` reached develop carrying six of them, from a branch whose author
# had already removed them — a squash body concatenates the individual commit
# messages, so a cleanup that lands after the squash cleans nothing.
#
# Matched on the trailer shape, not on a bare address. The angle brackets are
# what git writes into a real co-author trailer, so requiring them catches every
# genuine one while leaving prose free to name the pattern — this file's own
# commit message discusses the address, and a check that cannot describe itself
# gets disabled the first time someone edits it.
#
# Usage: no-ai-attribution.sh <base>
#   <base> is the commit the range starts after — a merge base on a pull
#   request, or the previously pushed head on a push.
set -euo pipefail

BASE="${1:-}"
if [ -z "$BASE" ]; then
    echo "usage: $0 <base-commit>" >&2
    exit 2
fi

# Matched case-insensitively, one pattern per line.
PATTERNS='<[^>]*@anthropic\.com>
co-authored-by:.*\(claude\|anthropic\)
generated with .*claude
🤖'

fail=0
while IFS= read -r sha; do
    [ -n "$sha" ] || continue
    message=$(git log -1 --format='%B' "$sha")
    hits=$(echo "$message" | grep -in "$PATTERNS" || true)
    if [ -n "$hits" ]; then
        fail=1
        echo "::error::$sha credits an AI tool in its commit message"
        git log -1 --format='  %h %s' "$sha"
        echo "$hits" | sed 's/^/    /'
    fi
done < <(git rev-list "${BASE}..HEAD")

if [ "$fail" -ne 0 ]; then
    cat >&2 <<'MSG'

Commit messages must not credit AI tooling. Remove the offending lines and
force-push the branch, keeping the commits signed:

  - a single commit: `git commit --amend -S` and delete the line.
  - a range: rewrite each message, then check with
    `git range-diff <base>..<old-head> <base>..<new-head>` that the only
    difference is the removed trailer.

No one-liner is offered here on purpose: a message rewrite that silently
does nothing looks identical to one that worked, and this check is the only
thing that would tell you apart.
MSG
    exit 1
fi

echo "no AI attribution in $(git rev-list --count "${BASE}..HEAD") commit(s)"
