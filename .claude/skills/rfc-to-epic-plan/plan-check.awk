# Sums a plan's epic estimate lines and reports every malformed, missing or misplaced one,
# and a plan line or §2 total row that disagrees; only a TOTAL line means the estimate is sound.
# Usage: awk -f plan-check.awk <plan>
/^## /                                   { in2 = ($0 ~ /^## 2\./); in5 = ($0 ~ /^## 5\./); cur = "" }
in2 && /^\|/ {
  nc2 = split($0, cell, "|")
  if (!sc) {
    for (i = 1; i <= nc2; i++) { t = cell[i]; gsub(/^ +| +$/, "", t)
      if (t == "#") ic = i; if (t == "Subtasks") sc = i; if (t == "Man-days") mc = i }
  } else if ($0 ~ /\*\*Total\*\*/) {
    if (trow++) print "a second total row in §2, line " NR
    else { tn = cell[sc]; td = cell[mc]; gsub(/[ *]/, "", tn); gsub(/[ *]/, "", td); tline = NR }
  } else if (ic) {
    id = cell[ic]; gsub(/[ *]/, "", id); sub(/^E/, "", id)
    if (match(id, /^[0-9]+/)) { id = substr(id, 1, RLENGTH)
      if (id in rown) print "a second §2 row for epic " id ", line " NR
      rown[id] = cell[sc]; rowd[id] = cell[mc]; gsub(/[ *]/, "", rown[id]); gsub(/[ *]/, "", rowd[id]); rowl[id] = NR
    }
  }
}
in5 && /^### /                           { cur = "" }
in5 && /^### Epic [0-9]+ /               { cur = $3; head[$3]++; order[++k] = $3
                                           if (head[$3] > 1) print "duplicate heading for epic " $3 }
/(Epic [0-9]+|Plan) estimate:/ {
  if ($0 ~ /^\*\*Epic [0-9]+ estimate: [0-9]+ subtasks, [0-9]+(\.(25|5|75))? man-days\.\*\*$/) {
    est[$2]++; n += $4; d += $6; en[$2] = $4; ed[$2] = $6
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
  if (!sc || !mc) print "no Subtasks and Man-days columns in the §2 table"
  else if (!trow) print "no **Total** row in the §2 table"
  else if (tn !~ /^[0-9]+$/ || td !~ /^[0-9]+(\.(25|5|75))?$/) print "§2 total row, line " tline ", is not plain numbers: " tn " subtasks, " td " man-days"
  else if (tn + 0 != n || td + 0 != d) printf "§2 total row says %d subtasks, %g man-days\n", tn, td
  if (ic && sc && mc) for (i = 1; i <= k; i++) {
    id = order[i]
    if (!(id in rown)) { print "epic " id " has no row in the §2 table"; continue }
    if (id in later) { if (rown[id] ~ /^[0-9]/ || rowd[id] ~ /^[0-9]/) print "§2 row for epic " id ", line " rowl[id] ", has a figure, but §5 says planned after"; continue }
    if (!(id in en)) continue
    if (rown[id] != en[id] || rowd[id] + 0 != ed[id] + 0 || rowd[id] !~ /^[0-9]+(\.(25|5|75))?$/)
      print "§2 row for epic " id ", line " rowl[id] ", says " rown[id] " subtasks, " rowd[id] " man-days; §5 says " en[id] ", " ed[id]
  }
  for (id in rown) if (!(id in head)) print "§2 row for epic " id " has no ### heading in §5"
  if (!seen) { print "no plan estimate line in §2"; exit }
  if (pn != n || pd != d) printf "plan line says %d subtasks, %g man-days\n", pn, pd
  for (id in later) if (!(id in listed)) print "plan line does not name epic " id " as not counted"
  for (id in listed) if (!(id in later)) print "plan line names epic " id ", which has a figure"
}
