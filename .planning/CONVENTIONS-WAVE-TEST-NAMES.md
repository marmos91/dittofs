# General rule: source files are wave-agnostic

Wave/program-tracking names (`wave4_*`, `wave_4_*`, lane names, PR numbers, audit
batch labels) never go into source files — Go sources and tests included. Source
code is agnostic to which wave, lane, or fan-out produced it: a file dropped into
`internal/` must read as if it always belonged there.

## The rule

When a lane/child produces a new source or test file, its name describes the
**domain** it tests, not the batch that shipped it:

| Bad (wave-tracking) | Good (domain) |
| --- | --- |
| `wave4_highs_test.go` | `session_durable_test.go` |
| `wave4_ct_tree_test.go` | `tree_connect_acl_test.go` |
| `wave4_ct_compound_test.go` | `compound_integrity_test.go` |
| `wave4_rw_gates_test.go` | `set_info_gates_test.go` |

Reasoning: wave names rot into permanent noise. Six months later nobody knows
what "wave 4" was, the name outlives the roadmap, and `rg wave4` results in
source grep noise for every future fan-out. `.planning/` is where wave-tracking
context lives.

## Prompt boilerplate for every future fan-out

Every subagent/worker prompt that can create source files must carry this line:

> **File naming:** source and test files are wave/lane/PR-number agnostic. Name
> new files after the domain they cover (e.g. `compound_integrity_test.go`, not
> `wave4_ct_compound_test.go`). Wave-tracking names live only in `.planning/`.

## If a wave-named file lands anyway

Fix forward in the branch before merge: `git mv` to the domain name, re-verify
(same package, so only build + the moved tests need re-running), force-push
(`git push origin +HEAD:refs/heads/<branch>`). Renaming after merge costs a
follow-up PR — catch it at review time: reviewers should flag any `wave\d|_w\d`
match under `internal/`, `pkg/`, `cmd/` as a blocking finding.
