# Rows

Each line that starts with "|" is a row, split at every "|": the second field
is the name and the fourth the reason, both trimmed. Separator rows (a "|" and
then dashes, spaces allowed between) and the header are skipped, and so is a
row with no name. An alignment row such as "|:---|" is not a separator row, so
it loads as a row named ":---".

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| plain | bug | the second field is the name, the fourth the reason | - |
|	tabbed	|	bug	|	tabs around a field are trimmed too	|	-	|
| no-reason | bug |
| empty-reason | bug |  | - |
| extra-fields | bug | the reason | - | more | and more |
| no-closing-pipe | bug | the reason
| escaped\|pipe | bug | an escaped pipe splits the row too | - |
  | indented | bug | a row with anything before its first pipe is prose | - |
|# hash | bug | a row may start with a hash | - |
| | bug | a row with no name is skipped | - |
| Test Name | bug | the header is skipped wherever it is | - |
| test name | bug | only the exact header is skipped | - |
|:---|:---|:---|:---|
| -dash | bug | a name starting with a dash makes a separator row | - |
| **Expected** | | | |
| override | bug | the first reason | - |
| override | bug | a later row replaces the reason | - |
| blanked | bug | a reason | - |
| blanked | bug | | - |
