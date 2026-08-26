#!/usr/bin/env python3
"""Extract a section-number -> title map from an Open Specifications PDF.

Manual refresh step; see README.md. Requires `pypdf`.

Two independent passes over the extracted text produce the map:

  TOC pass   lines carrying dot leaders, e.g. "2.4.14 FileEndOfFileInformation ..... 87"
  body pass  headings in the body, e.g. "2.4.14 FileEndOfFileInformation"

They are cross-checked against each other, and every number that neither pass
found but whose siblings exist is reported, so a section the extraction silently
dropped shows up as a warning rather than as a false citation failure later.
"""

import json
import re
import sys

from pypdf import PdfReader

TOC_LINE = re.compile(r"^\s*(\d+(?:\.\d+)+)\s+(.+?)[\s.]*\.{2,}\s*\d+\s*$")
# A table-of-contents entry whose title fills the line leaves no room for dot
# leaders, so the page number ends up glued to the last word.
TOC_FULL = re.compile(r"^\s*(\d+(?:\.\d+)+)\s+(.{55,}?[A-Za-z])(\d{1,4})\s*$")
NUMBERED = re.compile(r"^\s*(\d+(?:\.\d+)+)\s+\S")
BODY_LINE = re.compile(r"^\s*(\d+(?:\.\d+)+)\s+([A-Z][^.]{2,90})\s*$")
REVISION = re.compile(r"\[MS-[A-Z0-9]+\]\s*-\s*(v\d+)")


def parent(num):
    return num.rsplit(".", 1)[0]


def main(pdf_path, spec, out_path):
    pages = [p.extract_text() or "" for p in PdfReader(pdf_path).pages]
    text = "\n\f\n".join(pages)
    lines = text.split("\n")

    revision = max(REVISION.findall(text), default="unknown")

    toc, body = {}, {}
    i = 0
    while i < len(lines):
        line = lines[i]
        m = TOC_LINE.match(line)
        if m:
            toc.setdefault(m.group(1), m.group(2).strip())
            i += 1
            continue
        m = TOC_FULL.match(line)
        if m:
            toc.setdefault(m.group(1), m.group(2).strip())
            i += 1
            continue
        # A title too long for one line wraps, carrying the leaders and the
        # page number onto the next line.
        if NUMBERED.match(line) and i + 1 < len(lines) and not NUMBERED.match(lines[i + 1]):
            m = TOC_LINE.match(line.rstrip() + " " + lines[i + 1].strip())
            if m:
                toc.setdefault(m.group(1), m.group(2).strip())
                i += 2
                continue
        i += 1

    for line in lines:
        if "...." in line:
            continue
        m = BODY_LINE.match(line)
        if m:
            body.setdefault(m.group(1), m.group(2).strip())

    merged = {**body, **toc}  # the TOC pass wins where both found a title

    def norm(s):
        return re.sub(r"[^a-z0-9]", "", s.lower())

    disagree = [
        (k, toc[k], body[k])
        for k in sorted(toc)
        if k in body and norm(toc[k])[:25] != norm(body[k])[:25]
    ]

    # Every parent of a known section must itself be known.
    orphans = sorted({parent(k) for k in merged if "." in parent(k)} - set(merged))

    # A gap in a sibling run (".1 .2 .4") means .3 was dropped by the extraction.
    by_parent = {}
    for k in merged:
        by_parent.setdefault(parent(k), set()).add(int(k.rsplit(".", 1)[1]))
    gaps = [
        f"{p}.{i}"
        for p, kids in sorted(by_parent.items())
        for i in range(1, max(kids) + 1)
        if i not in kids
    ]

    # A number the passes missed is only a real miss if the flattened body text
    # mentions it as a heading somewhere.
    flat = re.sub(r"\s+", " ", text)
    really_missing = [n for n in orphans + gaps if re.search(rf"(?<![\d.]){re.escape(n)} [A-Z]", flat)]

    print(f"{spec}: revision={revision} pages={len(pages)} toc={len(toc)} body={len(body)} merged={len(merged)}")
    print(f"  toc/body disagreements: {len(disagree)}", disagree[:5])
    print(f"  orphan parents: {len(orphans)}", orphans[:10])
    print(f"  sibling-run gaps: {len(gaps)}", gaps[:10])
    print(f"  gaps present in body text (REAL MISSES): {len(really_missing)}", really_missing[:20])

    with open(out_path, "w") as fh:
        json.dump(
            {"spec": spec, "revision": revision, "sections": dict(sorted(merged.items()))},
            fh,
            indent=1,
        )
        fh.write("\n")
    return 1 if really_missing else 0


if __name__ == "__main__":
    if len(sys.argv) != 4:
        sys.exit("usage: extract_sections.py <spec.pdf> <MS-XXXX> <out.json>")
    sys.exit(main(*sys.argv[1:]))
