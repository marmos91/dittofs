# Sums an epic document's estimate lines and reports every malformed, missing or misplaced
# one; only a TOTAL line means the estimate is sound. Usage: awk -f epic-check.awk <epic-doc>
/^## /                                   { in6 = ($0 ~ /^## 6\./); cur = "" }
in6 && /^### /                           { cur = "" }
in6 && /^### T[0-9]+\.[0-9]+[a-z]? /     { cur = $2; head[$2]++; order[++k] = $2
                                           if (head[$2] > 1) print "duplicate heading " $2 }
/(Epic [0-9]+|Subtask T?[0-9]+\.[0-9]+[a-z]?) estimate:/ {
  if ($0 ~ /^\*\*Subtask T[0-9]+\.[0-9]+[a-z]? estimate: [0-9]+(\.(25|5|75))? man-days\.\*\*$/) {
    est[$2]++; n++; d += $4
    if ($2 != cur) print "estimate line for " $2 " (line " NR ") sits under " (cur == "" ? "no subtask heading" : "heading " cur)
  } else if ($0 ~ /^\*\*Epic [0-9]+ estimate: [0-9]+ subtasks, [0-9]+(\.(25|5|75))? man-days\.\*\*$/) {
    if (seen++) print "a second epic estimate line, line " NR
    else { epic = $2; hn = $4; hd = $6 }
  } else print "malformed, line " NR ": " $0
}
END {
  for (i = 1; i <= k; i++) if (est[order[i]] != 1) print "subtask " order[i] ": " est[order[i]] + 0 " estimate lines"
  for (id in est) if (!(id in head)) print "estimate line for " id " has no ### heading in §6"
  if (seen) for (id in head) if (id !~ "^T" epic "\\.") print "subtask " id " is not an id of epic " epic
  printf "TOTAL %d subtasks, %g man-days\n", n, d
  if (!seen) print "no epic estimate line in the header"
  else if (hn != n || hd != d) printf "header says %d subtasks, %g man-days\n", hn, hd
}
