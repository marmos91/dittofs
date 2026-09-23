# RFC 2 — the carver

**Status:** draft.
**Depends on:** RFC 0, for the terms and the data model. RFC 1 supplies the bytes
this component reads.
**Audience:** anyone changing `pkg/block/carver` or `pkg/block/chunker`.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are
to be interpreted as in RFC 2119.

---

## In short

- The carver cuts bytes into chunks. Blocks are made of chunks, and blocks are
  what gets uploaded.
- Boundaries are chosen by content, not by position. That way an edit in one part
  of a file leaves the chunks everywhere else untouched. It is the only reason to
  cut this way.
- There are two pieces. The **chunker** says where a chunk ends. The **carver**
  runs it over a stretch of bytes and hashes each chunk it finds.
- Neither keeps anything between calls, and neither calls out to anything else.
  Both can be tested with a byte slice and a closure.
- One call covers one unbroken stretch of a file. That is what keeps a chunk from
  straddling a hole.
- A chunk's name is the hash of its bytes, and nothing else.
- Whoever builds blocks follows three rules (§5). Building them is not the
  carver's job.
- The settings are public, so chunk sizes are predictable. This layer does not
  hide which files you have (§6).
- The shipped code misses two requirements here. Appendix A shows what it does
  instead, and how that was measured.

---

## Background — the ideas this document assumes

*Skip this if content-defined chunking is familiar. Everything below is standard;
nothing in it is specific to DittoFS.*

**The problem.** We want to avoid storing or sending bytes we already have. So we
split each file into pieces, name each piece by the hash of its content, and skip
any piece whose name we have seen before.

**Why the pieces cannot be a fixed size.** Suppose we cut every 1 MB. Insert one
byte at the front of a file and every boundary after it shifts by one. Every piece
now has different content, so every piece gets a different name, and nothing
matches what we stored yesterday. One inserted byte costs a full re-upload.

**Content-defined chunking (CDC)** fixes that by letting the data choose the
boundaries. Slide a small window along the bytes, keep a cheap running fingerprint
of it, and declare a boundary whenever that fingerprint hits an agreed pattern.
Because the decision depends only on nearby bytes, inserting a byte moves only the
boundaries near the insertion. Everything further on is cut exactly where it was
before, keeps its name, and still matches. The idea comes from LBFS [3].

**The rolling fingerprint.** It has to be updatable one byte at a time, or the
scan costs too much. DittoFS uses a *gear* hash: shift the running value left one
bit and add a table entry for the new byte. Cheap, and — as §3.4 works out — it
forgets anything older than 64 bytes, which turns out to matter.

**FastCDC** [1] is the specific scheme used here. On top of plain CDC it adds
three things:

- a *gear* hash instead of the older, slower Rabin fingerprint;
- a **minimum** size, below which it does not even look for a boundary, to avoid
  emitting lots of tiny pieces;
- **normalisation** — a stricter test below the target size and a looser one above
  it, so sizes cluster near the target instead of spreading out. "Normalisation
  level 2" just names how much stricter and looser.

**BLAKE3** [2] is the hash that names a chunk: fast, and 256 bits wide. It needs
to be a cryptographic hash because a name collision — two different chunks with
the same name — would mean silently serving the wrong bytes.

**Three words used throughout.** A **chunk** is one cut piece, named by its hash.
A **block** is a group of whole chunks, and a block is what actually gets uploaded
as one object. A file's **manifest** is the ordered list of the chunks it is made
of. Chunks are shared between files; blocks and manifests are not the same thing
and §5 is careful about the difference.

---

## 1. Purpose

The carver cuts the bytes the journal is holding into the pieces that get
uploaded.

> **Given an unbroken stretch of a file's bytes, say where the chunk boundaries
> fall and what each chunk's content hash is.**

That is all of it. Chunks are not the product — blocks are, and a block is a whole
number of chunks (RFC 0 §2.1). But building blocks is a fold over what the carver
hands back (§5), not something the carver does.

Why cut this way at all? Because a boundary chosen by content stays put when the
file changes elsewhere. Edit the middle of a file and only the chunks around the
edit are re-cut; everything else keeps its hash, so those bytes can be recognised
as already stored and left out of the upload. Everything downstream — dedup,
refcounts, sweep safety — rests on that. It is why most of this document is about
getting the cuts right.

### 1.1 Non-goals

The carver **MUST NOT**:

- open, read or write anything — it is handed a reader and hands back descriptors;
- decide *when* to cut, or which bytes to offer; that is flush policy (RFC 0 §5.2);
- build blocks, upload anything, or find out whether an upload worked;
- know whether a chunk is already stored — it has no dedup oracle and asks nothing
  of any other component;
- know what a segment, a record, a block or a remote key is;
- keep state between calls.

Its dependencies are the standard library and a BLAKE3 implementation, and an
import test **MUST** enforce that.

### 1.2 Two layers: the chunker and the carver

Two things are specified here, and keeping them apart is what keeps both simple.

**The chunker answers *where*. The carver answers *what*.**

| | chunker | carver |
| --- | --- | --- |
| Answers | where does this buffer's next chunk end? | what chunks are in this stretch, and what are they called? |
| Given | settings, a byte slice, and whether it is the last | settings, a reader, the file offset it starts at |
| Returns | one position, or "not yet — give me more" | one callback per chunk: offset, length, hash, bytes |
| Knows about | bytes and settings, nothing else | a reader, a hash function, a file offset |
| Keeps between calls | nothing at all | nothing at all |
| Allocates | never | one buffer per call |


Indicative shapes — the obligations are normative, the signatures are not:

    // chunker: pure, stateless, allocation-free
    NextBoundary(p Params, buf []byte, final bool) (end int)

    // carver: runs the chunker across one stretch of bytes
    Cut(r io.Reader, base int64, p Params, emit func(Chunk, []byte) error) error

    Chunk = { Offset int64; Length int32; Hash [32]byte }

`NextBoundary` gets a buffer and returns where the first chunk in it ends. If it
has not seen enough bytes to be sure, it returns zero and asks for more; `final`
tells it no more are coming, so whatever is left is the last chunk. It looks at
nothing but its arguments.

`Cut` reads until the reader is empty. It refills a buffer, asks `NextBoundary`
where each chunk ends, hashes that chunk, and calls `emit`. `base` says where in
the file the reader's first byte sits, so offsets come out as file offsets and the
carver needs to know nothing else about the file.

**Neither does the other's job**, and an implementation **MUST NOT** let them
drift together:

- the chunker **MUST NOT** hash, hold a file offset, read from anything, or
  allocate;
- the carver **MUST NOT** work out a boundary itself, look at a fingerprint, or
  depend on how the chunker reached its answer.

That second rule is the one to enforce. The design this document replaced had a
carver reaching into the boundary search to manage its own buffer, and that is how
callers ended up having to track two positions in the file at once.

**Which layer owns what.** Section 3 is all chunker. Sections 2 and 4 are all
carver. Section 5 belongs to neither, and section 6 is about the pair.

The split is not about packaging. It is what lets the boundary rules be checked
against a byte slice with no reader and no hash anywhere in sight, and it is what
makes swapping the boundary function a change to one thing instead of two.

### 1.3 It is testable on its own, by construction

Neither layer declares an interface or needs a capability from anything else. RFC
0 §1.2 says every component must build and pass its tests with each declared
interface stubbed out; here there is nothing to stub. The chunker goes further —
with no reader and no hash, a byte slice is the whole fixture.

This is a requirement, not a lucky accident, and an implementation **MUST** keep
it. A conformance check for this document **MUST** be writable as: build the
settings, hand `Cut` a byte slice and a closure, check what the closure saw. No
store, no fake backend, no temp directory, no network, no clock, no goroutine.

Two things follow, and both make checks cheap:

- **Same input, same output, always.** Two implementations — or one before and
  after a change — can be checked for identical results over random input. The
  11.6× speed-up in Appendix A.2 was established exactly this way.
- **No state means properties can be fuzzed.** The invariants in §8 are properties
  of a return value, so random inputs and random settings are enough to test them.

An implementation that picks up a dependency here — a metadata lookup, a store
handle, a logger that does something, a clock — loses both, and **MUST NOT**.

## 2. What one call covers

### 2.1 One unbroken stretch per call

The reader **MUST** supply one unbroken stretch of the file's bytes. Where the
file has holes, the caller makes a separate call for each stretch. RFC 1 §3.2
already tells the caller where the holes are, so this costs it nothing.

This is a guarantee built into the shape rather than a rule to be obeyed, which is
the point. A chunk cannot straddle a hole, because no single call ever sees two
stretches. There is no leftover state to clear between stretches, because there is
no state between calls at all.

The last chunk of a stretch comes out at whatever length is left, and **MAY** be
shorter than the minimum. Content **MUST NOT** be padded to reach a chunk size.

> *A real cost, worth stating:* a badly fragmented file gives one short chunk per
> stretch, and short chunks only dedup against identical fragments. Nothing can be
> done about it here — bytes that are not next to each other cannot share a chunk.
> Offering longer stretches is the caller's lever.

### 2.2 The bytes handed to `emit` are borrowed

The bytes passed to `emit` point into the carver's own buffer and are only good
**for the length of that call**. A caller that wants to keep them **MUST** copy
them.

The carver **MUST** say so plainly, and **MUST NOT** allocate a fresh slice per
chunk to sidestep it. Allocating per chunk is what makes buffer zeroing the
largest single cost of a carve pass. The whole point of handing bytes to a
callback is that the consumer is right there and can decide, once, whether these
particular bytes are worth keeping.

> *Note.* An earlier draft required the opposite — that the bytes become the
> caller's outright. That rule was written for an interface that returned finished
> *blocks*, used long after the call, where borrowing really is unsafe. For a
> callback that runs inside the call, borrowing is the normal contract, and
> `restic/chunker` [4] uses exactly it.

## 3. The boundary function

*This section is the chunker's contract: what a boundary function has to
guarantee, what its three settings mean, and what makes a setting invalid. The
shipped code does not meet two of these requirements — Appendix A says what it
does instead.*

### 3.1 What it must guarantee

These are properties, not an algorithm. Any function with them will do.

| # | Property | What it means |
| --- | --- | --- |
| B1 | **Content-defined** | Where a boundary falls depends on the bytes around it, never on a position or a running count. |
| B2 | **Deterministic** | Same bytes and same settings give the same boundaries — any process, any machine, any version. |
| B3 | **Bounded** | No chunk larger than `Max`. None smaller than `Min`, except the last one in a stretch. |
| B4 | **Shift-resistant** | Inserting or deleting bytes re-cuts only the chunks near the edit. Everything further on is untouched. |
| B5 | **Locally dependent** | A decision looks back only a fixed number of bytes, and the function **MUST** say how many (§3.4). |


Shift resistance (B4) is what the whole content-addressed model rests on. Without
it, an edit re-hashes every chunk after it and nothing downstream dedups at all.
Local dependence (B5) is what makes the function cheap to run.

FastCDC [1] with gear hashing is the chosen instantiation, and BLAKE3-256 [2] the
chunk hash (RFC 0 §2.1, §3). A replacement **MUST** have all five properties. Any
replacement re-cuts every file ever written, so it is a migration (§3.6).

### 3.2 The three settings

Each **MUST** mean what its name says:

| Setting | What it **MUST** mean |
| --- | --- |
| `Target` | the average chunk size actually produced, on data with no structure to exploit |
| `Min` | no chunk smaller than this, except the last one in a stretch |
| `Max` | no chunk larger than this, ever |


An implementation **MUST** hit `Target` as the average chunk size, within a stated
tolerance, on data with no structure to exploit. It **MUST NOT** ship a boundary
function whose average lands somewhere else, and **MUST NOT** settle such a
mismatch by writing the real number into the specification.

`Min` and `Max` are the edges of a distribution centred on `Target`, so they
**MUST** sit either side of it: `Min ≤ Target ≤ Max`. Setting `Min` at or above
`Target` is not cautious, it is invalid — the minimum then smothers the search and
`Target` stops describing anything (§3.3).

**Whatever decides a boundary MUST be derived from `Target`.** In a gear-hash
design that decision is a bit mask, and the number of bits set in it fixes the
average chunk size. A hard-coded mask therefore pins the average whatever `Target`
is set to. An implementation **MUST** compute the mask from `Target`, and
**SHOULD** assert the two agree when it starts up.

*The shipped masks are hard-coded and encode a different target — Appendix A.1.*

### 3.3 Why a large minimum smothers the search

A minimum is enforced by simply not looking for a boundary until `Min` bytes have
gone by. If `Min` is much larger than `Target`, then the moment looking starts, a
boundary turns up almost immediately — because the test was tuned to fire about
every `Target` bytes.

Every chunk then comes out just over `Min`, with only a small random tail, and
`Max` is never reached. In other words you no longer have content-defined
chunking; you have fixed-size chunking with a bit of jitter, which throws away
shift resistance (B4). An implementation **MUST** therefore refuse `Min ≥ Target`
outright (§3.7).

### 3.4 How far back a decision looks

A boundary decision only depends on a short run of recent bytes, and the function
**MUST** say how long that run is. Before testing the first position, an
implementation **MUST** warm its rolling state over exactly that many bytes, and
**MUST NOT** warm it from the start of the chunk.

For gear hashing the answer is 64 bytes, and it is exact rather than approximate.
The fingerprint is built by shifting left one bit per byte, so after 64 bytes an
old byte's contribution has been shifted clean off the end of a 64-bit number. It
cannot affect anything. Written out:

fp_i = Σ_{j ≤ i} g[b_j] << (i − j) (mod 2⁶⁴)

Every term where `i − j` is 64 or more is a 64-bit value shifted left by at least
64 bits, which is exactly zero.

![One candidate chunk: the Min-byte warmed region that affects nothing, the 64-byte window that does, and the tested positions beyond it](img/rfc2-dependency-window.svg)

Warming from the start of the chunk therefore does `Min` bytes of work to reach a
search that finishes after roughly `Target` bytes. At the shipped settings that is
about 97% wasted effort, and fixing it is 11.6× faster for byte-identical output.
*Measurements in Appendix A.2.*

### 3.5 What "deterministic" covers

Same bytes, same settings, same boundaries — in any process, on any machine, at
any version.

The one thing it does **not** cover is where a stretch begins. The first `Min`
bytes of any stretch are never tested, so the same bytes starting at a different
point cut differently. Section 2.1 is what stops that mattering: a stretch is
always the same stretch.

The boundary function **MUST NOT** depend on anything beyond the bytes and the
settings — not a file identity, an offset, a clock, or a random seed. A
per-deployment secret is the one exception anyone should consider, and it is not
free (§6).

### 3.6 Changing any of this is a migration

Settings are chosen per share, at write time. Reading never re-cuts anything: a
file's chunk list freezes its boundaries (RFC 0 §2.2), so content written under
one set of settings reads fine under any other.

Changing a share's settings — or the boundary function, or anything derived from
it — is therefore **MUST NOT** be treated as a configuration change. New writes
cut in different places, hash to different chunks, and dedup against nothing
already stored. An implementation **MUST** record which settings produced a
share's existing content, and **MUST** report a change as a migration instead of
quietly applying it.

### 3.7 Bad settings must be refused, not replaced

An implementation **MUST** reject invalid settings when it starts up, with an
error, and **MUST NOT** quietly fall back to a default profile.

Invalid means: `Min` below the floor where chunking stops being worthwhile;
`Min ≤ Target ≤ Max` broken, including `Min ≥ Target` (§3.3); or `Max` larger than
the buffer contract allows.

Falling back to a default looks like robustness and is really a data-shape bug. A
share asked for 64 KiB chunks and quietly given 1 MiB ones produces content that
is valid, readable and correctly hashed — and that dedups against nothing the
operator expected, at sixteen times the read amplification they planned for, with
nothing anywhere reporting a problem. This is RFC 0 §1.2's silent-fallback hazard
wearing configuration as a disguise: the missing thing is the requested chunk
size, and its absence **MUST** be loud.

## 4. Chunk identity

A chunk's identity is the BLAKE3-256 [2] hash of its bytes, and nothing else. An
implementation **MUST NOT** feed an offset, a file identity, a length, a settings
profile or a version number into the hash.

The hash covers exactly the bytes handed to `emit` — the same bytes a later read
has to reproduce. It **MUST** be computed over the chunk as cut, never over a
buffer that merely contains it.

## 5. Packing: three rules, not a component

*Blocks are built by whoever owns the dedup query, which is not the carver (RFC 6).
The rules live here because they are properties of chunks.*

| # | Rule |
| --- | --- |
| P1 | A block holds whole chunks only. A chunk is never split to make a block come out an exact size. |
| P2 | A block reaches at least the block target, and overshoots it by at most one chunk. |
| P3 | A block holds only the chunks whose bytes it actually carries. |


P1 is RFC 0 §2.2. A chunk split across two blocks would have one hash naming
content in two places, so the hash would stop being a locator and a refcount would
stop having a single answer. P2 follows from P1: if a block has to end on a chunk
boundary, the most it can overshoot by is the chunk that crossed the line.

P3 is worth spelling out, because the tempting mistake is to let already-stored
chunks ride along so that a block's chunk list covers an unbroken range. It must
not. A chunk that is already stored lives in another block under its own key;
putting it in this one either duplicates its bytes or produces a block whose list
disagrees with its contents. The thing that needs *every* chunk, stored or not, is
the **file's manifest** — and the manifest is a different reader of the same
sequence `Cut` produced.

> *Note.* Keeping those two readers apart is also what lets block-assignment
> policy change without touching chunking or breaking dedup. `restic` [4] relies
> on exactly that separation, and used it to swap sequential assembly for a
> randomised one with no migration.

**Where the dangerous rule went.** Block assembly asks a dedup oracle whether a
chunk is already stored. If that oracle can see the block currently being built,
it will call a chunk stored before it has been uploaded — so an identical chunk
later in the same pass carries no bytes, and if the upload then fails the content
exists nowhere while metadata records two references to it. That hazard is real
and it belongs to **RFC 6**, because that is where the oracle gets asked. It is
named here so that taking it out of the carver does not look like dropping it.

## 6. Boundaries are public

The boundary function and its settings are fixed constants in the source. Anyone
can therefore work out where a given file's chunks will fall. **This layer does
not hide which files a deployment holds.**

The consequence is a known-file channel. The run of chunk sizes a file produces is
a fingerprint, so an observer who can see object sizes and counts in the remote
tier can test whether a particular file is stored. The same channel sits under
cross-tenant dedup: the fact that two tenants share an object is itself an answer
about their content.

An implementation **MUST NOT** claim otherwise, and a deployment that needs this
hidden **MUST** get it from another layer. Two mitigations exist, neither free:

- **Put a per-deployment secret in the boundary function.** Boundaries become
  unpredictable. Chunks also stop being comparable between deployments, so
  cross-deployment dedup ends, and adopting it re-cuts everything (§3.6). Keyed
  chunking is an active research target and recent work [5] has broken deployed
  schemes, so it **MUST NOT** be adopted on the assumption that a key settles the
  matter.
- **Randomise how blocks are assembled.** This blurs the link between chunk sizes
  and stored object sizes. It costs nothing in dedup and is not a migration,
  because assembly is policy (§5). It is RFC 6's call.

How much this actually gives away, at DittoFS's block sizes, has not been
measured (§10).

## 7. Errors

`Cut` fails without throwing away the work it finished.

- An error from `emit` or from the reader **MUST** stop the call and be returned.
  Chunks already handed to `emit` have been delivered; the carver **MUST NOT** try
  to take them back.
- A part-built chunk held when the error arrives **MUST** be dropped, never handed
  over short. A short chunk is indistinguishable from a legitimate last chunk, and
  it would hash to something no later read can reproduce.
- The carver **MUST NOT** report anything as stored, or keep state that would let
  a retry cut differently. What happens next is RFC 0 §5.2: the extents stay
  **Dirty** and the pass is retried.

Because cutting is deterministic and keeps no state, a retry over the same bytes
produces the same chunks. A failed pass costs work and never correctness.

## 8. Invariants

| # | Invariant |
| --- | --- |
| C1 | A chunk's hash depends on its bytes and nothing else. |
| C2 | The same bytes and settings give the same boundaries, in any process or version. |
| C3 | The average chunk size is the configured `Target`. |
| C4 | Every chunk is between `Min` and `Max`, except the last one in a stretch. |
| C5 | A block holds whole chunks and overshoots its target by at most one. |
| C6 | A block holds only the chunks whose bytes it carries. |


C3 is the one the shipped code fails (Appendix A.1). C1 and C2 are what make a
hash usable as an address; the rest cost wrong sizing or an unreadable file.

Two invariants from an earlier draft are gone because the shape made them
impossible to break: a chunk cannot straddle a hole (§2.1), and there is no
leftover state to clear. Two more moved to RFC 6 along with the dedup oracle.

## 9. Conformance

Conformance is every **MUST** holding. The checks below are evidence for the ones
that fail *quietly* — where nothing errors and nothing goes red by accident. A
check is only validated by reverting the code and watching it fail on its own
assertion.

Every check here is a pure function of a byte slice and a set of settings (§1.3).
If a check needs a fixture, the implementation has picked up a dependency it is
not allowed to have, and that is the finding.

Each check names its layer, because that is where the defect is. Five of the eight
need no reader and no hash at all.

| Layer | Requirement | Check |
| --- | --- | --- |
| chunker | §3.2 target is real | Cut incompressible data; assert the average is within tolerance of `Target`. **This fails against the shipped code** (Appendix A.1) and is the regression gate for fixing it. |
| chunker | §3.2 mask comes from target | Build at several targets; assert the mask's bit count tracks the target, and that one hard-coded mask cannot satisfy two of them. |
| chunker | §3.7 bad settings refused | Build with `Min` below the floor, with `Min ≥ Target`, and with `Max` above the ceiling; assert each errors and nothing usable comes back. |
| chunker | §3.4 warm-up equivalence | Assert boundaries are identical whether the fingerprint is warmed from the chunk start or over the last 64 bytes, across several profiles. |
| chunker | B4 shift resistance | Insert one byte early in a large input; assert every boundary past the edited chunk is unchanged. |
| carver | §4 hash covers the chunk | Cut identical content at two different `base` offsets; assert one hash. |
| carver | §2.2 borrowed bytes | Hold on to the slice passed to `emit` and assert it is seen to change. The check exists to prove the contract is real, so a caller that copies is not doing it out of superstition. |
| carver | §7 no short chunk on error | Fail `emit` mid-stretch, and fail the reader mid-chunk; assert no delivered chunk is a truncated prefix of one the clean path would produce. |


A check that needs both layers to be wrong at once is in the wrong place. If a
boundary defect only shows up through `Cut`, the chunker's own checks are too
weak, and strengthening those is the fix.

Two things **MUST NOT** stand in:

- **Compressible or repetitive data MUST NOT be the only input for the size
  checks.** A gear hash degenerates on repetitive input, and that is the one case
  where `Max` is ever reached — so a rig built on it measures the opposite of the
  normal case. It **MUST** be covered separately, since it is what `Max` is for.
- **One fixed profile MUST NOT be the only settings under test.** The defect in
  Appendix A.1 is invisible at a single profile and obvious across two.

## 10. Open questions

1. **Which way out of Appendix A.1.** Both exits re-cut everything already
   written, so the choice gets more expensive the longer shares run on the current
   profile. What is unmeasured is the right `Target` for the SMB large-file
   workload: smaller chunks cut read amplification and raise index and refcount
   counts. The trade is at least legible now that the distribution is known.
2. **The warm-up fix** (§3.4, Appendix A.2). 11.6× on the boundary decision for
   identical output, verified. What is unmeasured is the effect on a whole pass,
   where cutting was 13% (amd64) and 29% (arm64) of CPU behind buffer allocation.
   Independent of everything else here, and the cheapest thing on this list.
3. **Whether `Min` survives at all** (§3.2). Once the mask comes from `Target`,
   `Max` still guards against repetitive input and read amplification, but `Min`
   would only be bounding per-chunk overhead. `Target` plus `Max` is simpler and
   not obviously worse. Deciding needs the repetitive-input case measured.
4. **How much §6 gives away.** That boundaries are public is certain. What an
   observer can actually recover from DittoFS's object sizes is not. Randomised
   block assembly is cheap enough that it may be worth doing without waiting for
   the measurement.
5. **Where the buffer cost lands now.** Per-chunk allocation is forbidden (§2.2)
   and the block-sized buffer moved to RFC 6. Two profiles put large-buffer
   allocation and zeroing at 53% (amd64) and 9% (arm64) of carve cost — but on a
   workload whose dedup oracle was stubbed out, and whose absolute numbers did not
   reproduce an earlier baseline. The ordering is solid; the magnitude is not, and
   the split moves where the cost falls.

---

## Appendix A — what the shipped code actually does

Two requirements in §3 are not met. Both are recorded here with their evidence, so
that §3 reads as the specification and this reads as the report against it.

A deviation is a defect to fix or migrate. It is never a rule for an implementer
to build around, and the mismatch **MUST NOT** be closed by amending §3.

### A.1 The masks encode a different target than the profile declares

The mask pair is hard-coded, and it encodes a target of **8 KiB** while the
default profile declares **4 MiB**.

FastCDC [1] ties the mask to the target: for a target of `2^k` at normalisation level
*nl*, the small-region mask carries `k + nl` bits and the large-region mask
`k − nl`. The shipped pair has 15 and 11 bits set, and the only `k` and `nl` that
fit are 13 and 2 — a target of `2^13`, which is 8 KiB.

| | The masks imply | The profile declares |
| --- | --- | --- |
| Target average | 8 KiB (`k` = 13) | 4 MiB (`k` = 22) |
| Minimum | — | 1 MiB |


`Min` is 128× the masks' natural target, so the minimum smothers the search (§3.3)
and every chunk lands just above `Min`. Predicted average:
`1,048,576 + 32,768 = 1,081,344` bytes. Measured over 256 MiB of incompressible
data:

| Configured Min / Target / Max | Chunks cut | Average chunk | Smallest | Largest | Any chunk reached Target? |
| --- | --- | --- | --- | --- | --- |
| 1 MiB / 4 MiB / 16 MiB | 248 | **1.029 MiB** | 1.000 MiB | 1.154 MiB | none |
| 64 KiB / 256 KiB / 1 MiB | 2,765 | **94.8 KiB** | 64.0 KiB | 258.6 KiB | none |

**How to read this.** In both rows the average sits about 30 KB above `Min`, and
nowhere near `Target`. That 30 KB is the masks' own scale showing through — the
small-region mask has 15 bits set, so a boundary turns up on average every
2¹⁵ = 32,768 bytes once looking starts. Change `Min` and that gap stays the same
size, which is what points at the masks rather than the profile. `Max` is not
approached in either row.

![Chunk size on a log scale: the 8 KiB target the masks imply, the declared Min of 1 MiB, Target of 4 MiB and Max of 16 MiB, and the whole measured distribution as a narrow spike sitting on Min](img/rfc2-size-distribution.svg)


That is 0.24% from prediction. The second profile confirms the masks are the cause
rather than the profile: moving `Min` and `Avg` together shifts the average by
exactly the change in `Min`. Neither run produced a single chunk that reached
`Avg`; for the default profile that would need 3 MiB of consecutive positions to
all fail a 1-in-32,768 test.


For comparison, the FastCDC paper's own recommended setup [1] is normalisation
level 2 with a minimum of 4–8 KB — `Min` close to `Target`, not 128 times it.

**Two ways out, both migrations (§3.6):**

1. **Compute the masks from `Target`**, and demote `Min` and `Max` to guard rails.
   This is what §3.2 requires. It re-cuts all existing content.
2. **Redeclare the profile** to the 8 KiB target the masks actually implement.
   Easier to reason about, still re-cuts content unless `Min` comes down to match,
   and leaves a design where the masks can never be retuned.

This document does not choose between them. See §10, question 1.

### A.2 Warm-up runs over the whole chunk instead of the last 64 bytes

Only the previous 64 bytes can affect a boundary decision (§3.4). The shipped code
warms its rolling state from the start of the candidate chunk, which at the
default profile means about 1 MiB of work to reach a search that ends after about
32 KiB — roughly 97% of it deciding nothing.

Measured per boundary decision, default profile:

| Fingerprint warmed over | Time to find one boundary | Equivalent throughput |
| --- | --- | --- |
| the whole chunk (1 MiB) | 759 µs | 1.4 GB/s |
| the last 64 bytes | **66 µs** | **15.9 GB/s** |

**How to read this.** Both rows do the same job — find one chunk boundary at the
default settings — and both produce the *identical* boundary. The only difference
is how many bytes of fingerprint get computed before the search can begin. So this
is not a trade between speed and quality: the faster row is the same answer for a
eleventh of the work.


11.6× faster, with bit-identical boundaries. Verified across three profiles (`Min`
of 4 KiB, 64 KiB and 1 MiB), 200 random inputs each, for both end-of-stream cases.

Unlike A.1 this is **not** a migration. The output does not change, so it can be
fixed whenever convenient.

---

## References

1. Wen Xia, Yukun Zhou, Hong Jiang, Dan Feng, Yu Hua, Yuchong Hu, Qing Liu,
   Yucheng Zhang. **FastCDC: a Fast and Efficient Content-Defined Chunking
   Approach for Data Deduplication.** USENIX ATC '16.
   <https://www.usenix.org/system/files/conference/atc16/atc16-paper-xia.pdf>
2. Jack O'Connor, Jean-Philippe Aumasson, Samuel Neves, Zooko Wilcox-O'Hearn.
   **BLAKE3: one function, fast everywhere.** 2020.
   <https://github.com/BLAKE3-team/BLAKE3-specs>
3. Athicha Muthitacharoen, Benjie Chen, David Mazières. **A Low-Bandwidth Network
   File System.** SOSP '01. The paper that introduced content-defined chunking.
   <https://pdos.csail.mit.edu/papers/lbfs:sosp01/lbfs.pdf>
4. **restic/chunker** — a production CDC implementation with the same
   reader-plus-callback shape and the same borrowed-bytes contract; `restic`
   keeps block assembly in a separate component for the reason §5 gives.
   <https://pkg.go.dev/github.com/restic/chunker> ·
   <https://github.com/restic/restic/blob/master/doc/design.rst>
5. Kien Tuong Truong, Simon-Philipp Merz, Matteo Scarlata, Felix Günther, Kenneth
   G. Paterson. **Breaking and Fixing Content-Defined Chunking.** IACR ePrint
   2025/558. Relevant to §6: keyed chunking is not a settled mitigation.
   <https://eprint.iacr.org/2025/558.pdf>
