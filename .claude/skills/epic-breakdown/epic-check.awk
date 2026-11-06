# Sums an epic document's estimate lines and reports every malformed, missing or misplaced
# one, and every subtask that depends on one listed after it in §6; only a TOTAL line means
# the document is sound. Usage: awk -f epic-check.awk <epic-doc>
# An epic id is a number with an optional letter (7, 7a): the parts of a split epic keep the
# epic's subtask ids, so the subtasks of epic 7a are T7.<n>.
/^## /                                   { in6 = ($0 ~ /^## 6\./); cur = "" }
in6 && /^### /                           { cur = "" }
in6 && /^### T[0-9]+\.[0-9]+[a-z]? /     { cur = $2; head[$2]++; order[++k] = $2; pos[$2] = k
                                           if (head[$2] > 1) print "duplicate heading " $2 }
in6 && cur != "" && /\*\*Depends on:\*\*/ {
  s = $0; sub(/^.*\*\*Depends on:\*\*/, "", s)
  while (match(s, /T[0-9]+\.[0-9]+[a-z]?/)) {
    deps[cur] = deps[cur] " " substr(s, RSTART, RLENGTH); s = substr(s, RSTART + RLENGTH)
  }
}
/(Epic [0-9]+[a-z]?|Subtask T?[0-9]+\.[0-9]+[a-z]?) estimate:/ {
  if ($0 ~ /^\*\*Subtask T[0-9]+\.[0-9]+[a-z]? estimate: [0-9]+(\.(25|5|75))? man-days\.\*\*$/) {
    est[$2]++; n++; d += $4
    if ($2 != cur) print "estimate line for " $2 " (line " NR ") sits under " (cur == "" ? "no subtask heading" : "heading " cur)
  } else if ($0 ~ /^\*\*Epic [0-9]+[a-z]? estimate: [0-9]+ subtasks, [0-9]+(\.(25|5|75))? man-days\.\*\*$/) {
    if (seen++) print "a second epic estimate line, line " NR
    else { epic = $2; hn = $4; hd = $6 }
  } else print "malformed, line " NR ": " $0
}
END {
  for (i = 1; i <= k; i++) if (est[order[i]] != 1) print "subtask " order[i] ": " est[order[i]] + 0 " estimate lines"
  for (id in est) if (!(id in head)) print "estimate line for " id " has no ### heading in §6"
  num = epic; sub(/[a-z]$/, "", num)
  if (seen) for (id in head) if (id !~ "^T" num "\\.") print "subtask " id " is not an id of epic " epic " (its subtasks are T" num ".<n>)"
  # §6 lists the subtasks in order of work, so a subtask depends only on subtasks above it.
  # An id with no heading here (another epic's, or the other part's of a split epic) is
  # not checked.
  for (i = 1; i <= k; i++) {
    m = split(deps[order[i]], dl, " ")
    for (j = 1; j <= m; j++) if ((dl[j] in pos) && pos[dl[j]] >= i)
      print "subtask " order[i] " depends on " dl[j] ", which §6 lists after it"
  }
  printf "TOTAL %d subtasks, %g man-days\n", n, d
  if (!seen) print "no epic estimate line in the header"
  else if (hn != n || hd != d) printf "header says %d subtasks, %g man-days\n", hn, hd
}
