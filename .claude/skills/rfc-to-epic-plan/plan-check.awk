# Sums a plan's epic estimate lines and reports every malformed, missing or misplaced one,
# and a plan line that disagrees; only a TOTAL line means the estimate is sound.
# Usage: awk -f plan-check.awk <plan>
/^## /                                   { in5 = ($0 ~ /^## 5\./); cur = "" }
in5 && /^### /                           { cur = "" }
in5 && /^### Epic [0-9]+ /               { cur = $3; head[$3]++; order[++k] = $3
                                           if (head[$3] > 1) print "duplicate heading for epic " $3 }
/(Epic [0-9]+|Plan) estimate:/ {
  if ($0 ~ /^\*\*Epic [0-9]+ estimate: [0-9]+ subtasks, [0-9]+(\.(25|5|75))? man-days\.\*\*$/) {
    est[$2]++; n += $4; d += $6
    if ($2 != cur) print "estimate line for epic " $2 " (line " NR ") sits under " (cur == "" ? "no epic heading" : "epic " cur)
  } else if ($0 ~ /^\*\*Epic [0-9]+ estimate: planned after S[0-9]+[a-z]?\.\*\*$/) {
    est[$2]++; later[$2] = 1; nl++
    if ($2 != cur) print "estimate line for epic " $2 " (line " NR ") sits under " (cur == "" ? "no epic heading" : "epic " cur)
  } else if ($0 ~ /^\*\*Plan estimate: [0-9]+ subtasks, [0-9]+(\.(25|5|75))? man-days(, not counting epics? [0-9]+((, | and )[0-9]+)*)?\.\*\*$/) {
    if (seen++) { print "a second plan estimate line, line " NR; next }
    pn = $3; pd = $5
    clause = $0; sub(/^.*man-days/, "", clause); gsub(/[^0-9]+/, " ", clause)
    nc = split(clause, c, " "); for (i = 1; i <= nc; i++) if (c[i] != "") listed[c[i]] = 1
  } else print "malformed, line " NR ": " $0
}
END {
  for (i = 1; i <= k; i++) if (est[order[i]] != 1) print "epic " order[i] ": " est[order[i]] + 0 " estimate lines"
  for (id in est) if (!(id in head)) print "estimate line for epic " id " has no ### heading in §5"
  printf "TOTAL %d subtasks, %g man-days", n, d
  sep = ", not counting epic"; for (i = 1; i <= k; i++) if (order[i] in later) { printf "%s %s", sep, order[i]; sep = "," }
  print ""
  if (!seen) { print "no plan estimate line in §2"; exit }
  if (pn != n || pd != d) printf "plan line says %d subtasks, %g man-days\n", pn, pd
  for (id in later) if (!(id in listed)) print "plan line does not name epic " id " as not counted"
  for (id in listed) if (!(id in later)) print "plan line names epic " id ", which has a figure"
}
