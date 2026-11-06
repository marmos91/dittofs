# Sums an epic document's estimate lines and reports every malformed, missing or misplaced
# one, and every subtask that depends on one listed after it in §6; only a TOTAL line means
# the document is sound. Usage: awk -f epic-check.awk <epic-doc>
# An epic id is a number with an optional letter (7, 7a): the parts of a split epic keep the
# epic's subtask ids, so the subtasks of epic 7a are T7.<n>.
#
# Dependencies are read from a subtask's **Depends on:** field, up to the next bullet, blank
# line or heading, so the field may wrap. Ids are written out (T7.2, T7.3) or as a range of
# one epic's subtasks (T7.2–T7.4); a dependency on T4.3 when §6 holds only T4.3a and T4.3b
# stands for both parts.
function dep_scan(text,   a, b, e, i) {
  while (match(text, /T[0-9]+\.[0-9]+[a-z]?((–|-)T?[0-9]+\.[0-9]+)?/)) {
    tok = substr(text, RSTART, RLENGTH); text = substr(text, RSTART + RLENGTH)
    if (tok ~ /(–|-)/) {
      split(tok, rg, /(–|-)T?/)
      split(rg[1], a, "."); split(rg[2], b, ".")
      sub(/^T/, "", a[1])
      if (a[1] != b[1]) { print "dependency range " tok " of " cur " spans two epics"; continue }
      for (i = a[2] + 0; i <= b[2] + 0; i++) deps[cur] = deps[cur] " T" a[1] "." i
    } else deps[cur] = deps[cur] " " tok
  }
}
/^# Epic [0-9]+[a-z]?[: ]/               { t = $3; sub(/:$/, "", t); title = t }
/^## /                                   { in6 = ($0 ~ /^## 6\./); cur = ""; indep = 0 }
in6 && /^### /                           { cur = ""; indep = 0 }
in6 && /^### T/ && !/^### T[0-9]+\.[0-9]+[a-z]? / { print "malformed subtask heading, line " NR ": " $0 }
in6 && /^### T[0-9]+\.[0-9]+[a-z]? /     { cur = $2; head[$2]++; order[++k] = $2; pos[$2] = k
                                           if (head[$2] > 1) print "duplicate heading " $2 }
indep && (/^ *- / || /^$/ || /^#/)       { indep = 0 }
indep                                    { dep_scan($0) }
in6 && cur != "" && /\*\*Depends on:\*\*/ {
  s = $0; sub(/^.*\*\*Depends on:\*\*/, "", s); dep_scan(s); indep = 1
}
/estimate:/ && !/(Epic [0-9]+[a-z]?|Subtask T?[0-9]+\.[0-9]+[a-z]?) estimate:/ { print "malformed, line " NR ": " $0 }
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
  if (seen && title != "" && title != epic) print "the title names epic " title ", the estimate line epic " epic
  num = epic; sub(/[a-z]$/, "", num)
  if (seen) for (id in head) if (id !~ "^T" num "\\.") print "subtask " id " is not an id of epic " epic " (its subtasks are T" num ".<n>)"
  # §6 lists the subtasks in order of work, so a subtask depends only on subtasks above it.
  # An id with no heading here (another epic's, or the other part's of a split epic) is
  # not checked.
  for (i = 1; i <= k; i++) {
    m = split(deps[order[i]], dl, " ")
    for (j = 1; j <= m; j++) {
      if (dl[j] in pos) { chk(order[i], i, dl[j]); continue }
      for (c = 97; c <= 122; c++) { part = dl[j] sprintf("%c", c); if (part in pos) chk(order[i], i, part) }
    }
  }
  printf "TOTAL %d subtasks, %g man-days\n", n, d
  if (!seen) print "no epic estimate line in the header"
  else if (hn != n || hd != d) printf "header says %d subtasks, %g man-days\n", hn, hd
}
function chk(who, at, dep) {
  if (pos[dep] >= at) print "subtask " who " depends on " dep ", which §6 lists " (dep == who ? "as itself" : "after it")
}
