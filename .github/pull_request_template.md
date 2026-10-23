<!--
Title: conventional commit, `type(scope): what changes`, lowercase, under ~70 characters.
  Good: fix(smb): refuse TREE_CONNECT when the share permission cannot be resolved
  Bad:  SMB improvements

The description should fit on one screen without scrolling. Delete any section that
would be empty or obvious. If the PR cannot be explained in one screen, it is probably
too big: say so and suggest how to split it.

Writing rules for PRs, review comments and replies:
.github/instructions/pr-writing.instructions.md
-->

# Description

<!-- One to three sentences: what changes in behavior, and why. Link the issue / RFC
instead of re-explaining it. -->

## Decision needed

<!-- What you need from the reviewer: "approve as is", "pick A or B", "check the
locking in X". One line. -->

## Type of change

<!-- Please delete options that are not relevant. -->

- [ ] Bug fix (non-breaking change which fixes an issue)
- [ ] New feature (non-breaking change which adds functionality)
- [ ] Breaking change (fix or feature that would cause existing functionality to not work as expected)
- [ ] Documentation update
- [ ] Performance improvement
- [ ] Code refactoring
- [ ] Test coverage improvement

## Changes Made

<!-- The changes a reviewer needs to know about, not every file or function: the diff
already shows those. Say where to start, which files are mechanical (renames, moves,
generated code) and which part needs careful eyes. -->

-
-
-

## Testing

<!-- Please describe the tests that you ran to verify your changes. Mention new tests
by the behavior they protect. Do not paste test output or logs. -->

- [ ] Unit tests pass (`go test ./...`)
- [ ] Manual testing performed
- [ ] Integration testing performed

## Testing Instructions

<!-- Describe how reviewers can test your changes: -->

1.
2.
3.

## Checklist

- [ ] My code follows the style guidelines of this project
- [ ] I have performed a self-review of my own code
- [ ] I have made corresponding changes to the documentation
- [ ] New and existing unit tests pass locally with my changes
- [ ] Any dependent changes have been merged and published

## Screenshots (if applicable)

Add screenshots to help explain your changes.

## Additional Notes

<!-- Risks, known limitations, follow-ups, things deliberately left out. -->
