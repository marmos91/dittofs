#!/usr/bin/env python3
"""Insert a version's CHANGELOG.md section into the GitHub release notes.

GoReleaser writes the release body from commit subjects only. This reads that
body on stdin and prints it with the matching CHANGELOG.md section placed
before the installation instructions, so a release carries both the curated
entries and the commit list.

Usage: release-notes.py vX.Y.Z < body.md > new-body.md
"""
import re
import sys

tag = sys.argv[1]
version = tag.removeprefix("v")
body = sys.stdin.read()

section, inside = [], False
for line in open("CHANGELOG.md", encoding="utf-8"):
    if line.startswith("## ["):
        if inside:
            break
        inside = line.startswith(f"## [{version}]")
        continue
    if inside:
        section.append(line.rstrip("\n"))

if not section:
    # No curated entry for this version: leave the commit-generated body alone.
    sys.stdout.write(body)
    sys.exit(0)

# GitHub keeps single line breaks in release notes, so join the hard-wrapped
# continuation lines of each paragraph and list item.
lines = []
for line in section:
    text = line.strip()
    prev = lines[-1].strip() if lines else ""
    if text and prev and not text.startswith(("#", "- ", "|", "```")) and not prev.startswith(("#", "|", "```")):
        lines[-1] += " " + text
    else:
        lines.append(line)

notes = "\n".join(lines).strip()
notes = re.sub(r"(?m)^### ", "#### ", notes)
# Repo-relative links do not resolve on the release page.
notes = re.sub(r"\]\(((?:docs|cmd|pkg|internal)/[^)]+)\)",
               rf"](https://github.com/marmos91/dittofs/blob/{tag}/\1)", notes)

notes = f"## What's new\n\n{notes}\n\n"
marker = "### Installation"
i = body.find(marker)
sys.stdout.write(body[:i] + notes + body[i:] if i >= 0 else notes + body)
