# Spec-citation lint

Checks every `MS-FSCC` / `MS-FSA` / `MS-SMB2` / `MS-DTYP` / `MS-ERREF` citation in the Go
tree against a vendored section-number → title map for each spec.

```bash
go run ./test/spec-citations          # whole tree, under a second, no network
go test ./test/spec-citations         # the rules themselves
```

## Why

Two audits corrected ~300 wrong citations in the SMB adapter. In the larger of the two, a
citation naming a section that **exists but says something else** outnumbered a citation naming
a section that **does not exist** by more than two to one — and the first kind survives every
check a reader is likely to run, because it looks right. Nothing noticed either kind, so the
tree drifted back the moment someone wrote `[MS-FSCC] 2.4.x` from memory.

## What it checks

1. **The cited section exists.** Absent from the map → fail.
2. **A quoted parenthetical is the cited section's title.** `MS-FSA §2.1.5.15.2
   ("FileRenameInformation")` fails, because §2.1.5.15.2 is `FileBasicInformation`. Only
   double-quoted parentheticals whose characters all appear in real section titles are read as
   titles, which is what keeps `(28 bytes)`, `(Samba smb2srv_open_lookup_replay_cache)` and a
   quotation of spec prose such as `("NewEpoch = Epoch + 1 ...")` out of it.
3. **A named structure is defined by the section cited beside it.** If a line names exactly one
   of `File*Information`, `FILE_*_INFORMATION` or `FSCTL_*`, carries exactly one MS-FSCC/MS-FSA
   citation, and the spec defines that structure in a different section, that is decidable
   without reading a word of spec prose.

Comments wrap at column 80, which regularly puts the spec name at the end of one line and the
section number at the start of the next. A line ending in a bare spec name is joined with the
one below before matching, so a wrapped citation is checked rather than skipped.

Rule 3 stays quiet when the cited section's title is prose rather than a structure name:
`// FileRenameInformation share-delete check per MS-FSA §2.1.5.1.2.1` attributes behaviour to
an algorithm section and is correct. Family references (`§2.4`), lines naming two structures,
and lines carrying two structural citations are ambiguous and are skipped by construction.

**Not checked:** whether the cited section's prose supports the claim next to it. That was
roughly 40% of what the audits found, and it needs a human with the spec open. A citation can
pass every rule here and still assert something the section does not say.

## `known_wrong.txt`

Citations that are wrong and not yet corrected. Correcting one means deciding what the code was
supposed to do — a wrong citation and a wrong implementation look identical from inside a source
file — so they are listed here and resolved deliberately, not by editing the comment until the
check goes quiet.

The key is `<path>: <SPEC> <section>`; line numbers and message text are not part of it, so
moving code or bumping a spec revision does not rot an entry. An entry that stops matching fails
the run, so the list cannot silently become a graveyard.

## Refreshing a section map

Manual, and only when a spec revision is bumped. The maps are a few kB each; the PDFs are not
vendored and CI never fetches them.

```bash
python3 -m venv .venv && .venv/bin/pip install pypdf
curl -L -o 'MS-FSCC.pdf' 'https://winprotocoldoc.z19.web.core.windows.net/MS-FSCC/%5BMS-FSCC%5D.pdf'
.venv/bin/python test/spec-citations/extract_sections.py \
    MS-FSCC.pdf MS-FSCC test/spec-citations/sections/MS-FSCC.json
```

The extractor derives the map twice — once from the table of contents, once from the body
headings — and reports disagreements and orphaned parents, then probes for numbers no pass
produced: holes between extracted siblings, a few numbers past the highest sibling of every
family (a section dropped at the *end* of a family leaves no hole), and a first child for every
section that came out childless. A probe counts as a real miss only when some line of the
document opens with it the way a heading does, and the script exits non-zero if any does. Read
the output: **a map that silently drops a real section turns a correct citation into a false
failure**, which is the failure mode that gets a check like this disabled.

Current revisions are recorded in each JSON file.
