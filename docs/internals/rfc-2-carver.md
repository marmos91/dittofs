# RFC 2 — the carver

**Status:** draft.
**Depends on:** RFC 0, for the terms, the data model and the invariants. RFC 1
supplies the bytes this component reads. Nothing here redefines them.
**Audience:** anyone changing `pkg/block/carver` or `pkg/block/chunker`.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are
to be interpreted as in RFC 2119.

Where the current implementation does not satisfy a requirement, that is recorded
as a **deviation** in its own subsection, with evidence. A deviation is a defect
to be fixed or migrated, never a rule for an implementer to build around.

---

## 1. Purpose

The carver cuts the bytes the journal is holding into the pieces that get uploaded.

> **Given a contiguous run of a file's bytes, say where the chunk boundaries fall
> and what each chunk's content hash is.**

That is all of it. Chunks are not the product — blocks are, and a block is a whole
number of chunks (RFC 0 §2.1). But assembling blocks is a fold over what the
carver returns (§5), not a thing the carver does.

The cutting earns its place for one reason: boundaries chosen by content, rather
than by position, survive an edit elsewhere in the file. A region that did not
change keeps its hash, so its bytes can be recognised as already stored and left
out of the upload. Everything downstream — dedup, refcounts, sweep safety — rests
on those cuts being reproducible, which is why most of this document is about
them.

### 1.1 Non-goals

The carver **MUST NOT**:

- open, read or write anything — it is handed a reader and returns descriptors;
- decide *when* to cut, or which bytes to offer — that is flush policy (RFC 0 §5.2);
- assemble blocks, upload anything, or learn whether an upload succeeded;
- know whether a chunk is already stored — it has no dedup oracle and requires no
  interface from any other component;
- know what a segment, a record, a block or a remote key is;
- hold state between calls.

Its dependency set is the standard library and a BLAKE3 implementation, and an
import test **MUST** enforce that.

### 1.2 It is one function

The carver is a function, not an object with a lifecycle. Two calls with the same
bytes and parameters produce the same result, in any order, concurrently, in any
process.

An indicative shape — the obligations, not the signature, are normative:

    Cut(r io.Reader, base int64, p Params, emit func(Chunk, []byte) error) error

    Chunk = { Offset int64; Length int32; Hash [32]byte }

`Cut` reads `r` to EOF, and for each chunk it cuts calls `emit` with the chunk's
descriptor and its bytes. `base` is the file offset `r`'s first byte sits at, so
`Offset` is a file offset and the carver never has to be told anything else about
the file.

Streaming happens inside. The carver holds one buffer and a partial chunk while it
looks for the next boundary; neither is visible to the caller, and neither
survives the call. A caller therefore has no cursor to advance and no state to
keep in step.

### 1.3 It is testable on its own, by construction

The carver declares no interfaces and requires no capability from any other
component (§1.1). RFC 0 §1.2 obliges every component to build and pass its tests
with each declared interface stubbed; here that holds vacuously, because there is
nothing to stub.

This is a requirement, not a happy accident, and an implementation **MUST**
preserve it. Concretely, a conformance check for this document **MUST** be
expressible as: construct parameters, hand `Cut` a byte slice and a closure,
assert on what the closure saw. No store, no fake backend, no temp directory, no
network, no clock, no goroutine.

Two properties follow that are worth naming because they decide how cheap the
checks are:

- **Determinism (B2) makes differential testing free.** Two implementations, or
  one before and after a change, can be asserted bit-identical over random input.
  The 11.6× result in §3.4 was established exactly this way, and that check is
  the template rather than a one-off.
- **Purity makes property testing free.** The invariants of §8 are properties of
  a return value, so they can be fuzzed over random inputs and random parameters
  without a fixture.

An implementation that acquires a dependency here — a metadata lookup, a store
handle, a logger with behaviour, a clock — forfeits both, and **MUST NOT**.

## 2. What a call covers

### 2.1 One contiguous run per call

`r` **MUST** yield one contiguous run of the file's bytes. The caller splits at
the gaps — RFC 1 §3.2 already hands it the gaps, so this costs it nothing.

This is a structural guarantee rather than a rule, and that is the point. A chunk
cannot span a gap because no call ever sees two runs, so a chunk can never hash
bytes that are not adjacent in the file. There is no rolling state to reset at a
run's end because there is no state between calls at all.

A run's last chunk is emitted whole at whatever length remains and **MAY** be
below the minimum. Content **MUST NOT** be padded to reach a chunk size.

> *Consequence, stated because it is a real cost:* a heavily fragmented file
> produces one short chunk per run, and those chunks dedup only against other
> copies of the same fragment. This is inherent — non-adjacent bytes cannot share
> a chunk — and it is the caller's to avoid by offering larger runs.

### 2.2 The emitted bytes are borrowed

The bytes passed to `emit` alias the carver's buffer and are valid **only for the
duration of that call**. A caller that keeps them **MUST** copy them.

The carver **MUST** document this and **MUST NOT** allocate per chunk to avoid it.
Per-chunk allocation is what makes large-buffer zeroing the dominant cost of a
carve pass, and the whole point of a synchronous callback is that the consumer is
right there and can decide, once, whether these bytes are worth keeping.

> *Note.* An earlier draft of this document required the opposite — that emitted
> bytes belong to the caller outright. That rule was written for an interface
> which returned assembled *blocks*, consumed long after the call, where
> borrowing is genuinely unsafe. For a synchronous per-chunk callback it is the
> standard contract, and `restic/chunker` uses exactly it.

## 3. The boundary function

### 3.1 What it must guarantee

Properties, not an algorithm:

| # | Requirement |
| --- | --- |
| B1 | **Content-defined.** A boundary's position depends on the bytes around it, not on an absolute offset or a count. |
| B2 | **Deterministic.** The same bytes and parameters yield the same boundaries in any process, on any platform, at any version. |
| B3 | **Bounded.** No chunk exceeds `Max`. No chunk is below `Min`, except a run's final chunk. |
| B4 | **Shift-resistant.** Inserting or removing bytes re-cuts only the chunks near the edit; boundaries beyond it are unchanged. |
| B5 | **Locally dependent.** A boundary decision depends on a bounded window of preceding bytes, and the function **MUST** declare that window's size (§3.4). |

B4 is what the whole content-addressed model rests on — without it an edit
re-hashes every chunk after it and nothing downstream dedups (RFC 0 §2.2). B5 is
what makes the function cheap to evaluate.

FastCDC with gear hashing and normalisation level 2 is the chosen instantiation,
and BLAKE3-256 the chunk hash (RFC 0 §2.1, §3). A replacement **MUST** satisfy
B1–B5, and any replacement re-cuts all content and is a migration (§3.6).

### 3.2 Parameters

| Parameter | **MUST** mean |
| --- | --- |
| `Target` | the expected chunk size the function realises on data with no exploitable structure |
| `Min` | no chunk below this, except a run's final chunk |
| `Max` | no chunk above this, unconditionally |

An implementation **MUST** realise `Target` as the expected chunk size, within a
stated tolerance, on incompressible input. It **MUST NOT** ship a boundary
function whose expected chunk size is some other value, and **MUST NOT** resolve
such a discrepancy by documenting the actual value as the specification.

`Min` and `Max` bound a distribution whose centre is `Target`, so they **MUST**
bracket it: `Min ≤ Target ≤ Max`. `Min` at or above `Target` is invalid, not
merely unusual — the minimum gate then suppresses the search entirely and `Target`
describes nothing (§3.3).

**Any breakpoint threshold MUST be derived from `Target`, not fixed.** In a
gear-hash design the thresholds are bit masks whose population counts encode the
expected size, so a hardcoded pair silently pins that size whatever `Target` says.
An implementation **MUST** compute them from `Target` and **SHOULD** assert the
relationship at construction.

#### 3.2.1 Deviation — the shipped masks encode a different target

The mask pair is hardcoded and encodes a target of 8 KiB, while the default
profile declares 4 MiB.

For FastCDC at normalisation level *nl* and target `2^k`, the small-region mask
carries `k + nl` set bits and the large-region mask `k − nl`. The shipped pair has
population counts 15 and 11, which solves to `k = 13`, `nl = 2`:

| | Implied by the mask pair | Declared by the profile |
| --- | --- | --- |
| Target average | 8 KiB (`k = 13`) | 4 MiB (`k = 22`) |
| Minimum | — | 1 MiB |

`Min` is 128× the masks' natural target, so the minimum gate dominates and every
chunk is `Min` plus a geometric draw with mean `2^15`. Predicted mean
`1,048,576 + 32,768 = 1,081,344`. Measured over 256 MiB of incompressible data:

| Profile — Min / Avg / Max | Chunks | Mean | Smallest | Largest | Reached Avg | Mean − Min |
| --- | --- | --- | --- | --- | --- | --- |
| 1 MiB / 4 MiB / 16 MiB | 248 | 1,078,800 | 1,048,896 | 1,210,151 | 0 | 30,224 |
| 64 KiB / 256 KiB / 1 MiB | 2,765 | 97,047 | 65,553 | 264,776 | 0 | 31,511 |

0.24% from prediction. The second profile confirms the mechanism is the mask pair
rather than the profile: moving `Min` and `Avg` together moves the mean by exactly
the change in `Min`. No chunk in either run reached `Avg`; for the default profile
that needs 3 MiB of consecutive positions to fail a 2⁻¹⁵ test, probability about
e⁻⁹⁶.

![Chunk size on a log scale: the masks' implied 8 KiB target, the declared Min of 1 MiB, Avg of 4 MiB and Max of 16 MiB, and the entire measured distribution as a narrow spike sitting on Min](img/rfc2-size-distribution.svg)

For context, FastCDC's own recommended regime is normalisation level 2 with a
minimum of 4–8 KB — that is, `Min` near `Target`, not 128× it.

Two ways out, both migrations (§3.6):

1. **Derive the masks from `Target`** and demote `Min`/`Max` to guard rails.
   Correct per §3.2; re-cuts all existing content.
2. **Redeclare the profile** to the target the masks implement. Cheaper to reason
   about, still re-cuts content unless `Min` is lowered to match, and leaves a
   design where the masks cannot be retuned.

This document does not choose. It records that the current state satisfies
neither, and that the discrepancy **MUST NOT** be closed by amending §3.2.

### 3.3 Why a minimum above the target suppresses the search

The general rule, independent of the deviation above. A minimum is enforced by not
testing for a boundary until `Min` bytes have accumulated. If `Min` is much larger
than `Target`, a boundary is available almost immediately once testing begins,
because the threshold was chosen to fire about every `Target` bytes.

The distribution is then `Min + Geom(1/Target)`: concentrated just above `Min`,
with `Target` visible only as the spread and `Max` unreachable. An implementation
**MUST** treat `Min ≥ Target` as invalid (§3.7), because it is not a conservative
setting — it is a silent replacement of content-defined chunking with fixed-size
chunking plus jitter, which forfeits B4.

### 3.4 The dependency window must be declared, and warm-up must match it

By B5 the function declares how many preceding bytes a decision depends on. An
implementation **MUST** warm its rolling state over exactly that window before the
first tested position, and **MUST NOT** warm it from the start of the candidate
chunk.

For gear hashing with a one-bit shift per byte over 64-bit arithmetic the window
is exactly 64 bytes. Expanding `fp = (fp << 1) + g[b]`:

fp_i = Σ_{j ≤ i} g[b_j] << (i − j) (mod 2⁶⁴)

every term with `i − j ≥ 64` is a 64-bit value shifted left by at least 64 bits,
so it is exactly zero. Nothing earlier than 64 bytes back can reach the decision.

![One candidate chunk: the Min-byte warmed region that affects nothing, the 64-byte dependency window, and the tested positions, with the measured cost of warming the whole region anyway](img/rfc2-dependency-window.svg)

Warming from the chunk start is therefore `Min` bytes of work to reach a scan that
ends after about `Target` bytes — at the shipped profile, roughly 97% of the work
decides nothing. Measured per boundary decision, default profile:

| Warm-up | Per boundary | Effective rate |
| --- | --- | --- |
| from the chunk start (`Min` bytes) | 758.7 µs | 1.38 GB/s |
| the declared window (64 bytes) | 65.8 µs | 15.9 GB/s |

11.6×, with bit-identical boundaries — verified on three profiles (`Min` of 4 KiB,
64 KiB and 1 MiB), 200 random inputs each, for both end-of-stream values.

The current implementation warms from the chunk start. Unlike §3.2.1 this is not a
migration: the output is unchanged, so it can be fixed at any time.

### 3.5 Determinism and its scope

Determinism is per call. Boundaries depend on where a run starts, because its
first `Min` bytes are never tested — so the same bytes at a different run start
cut differently. §2.1 is what keeps that from mattering: a run is always the same
run.

The boundary function **MUST NOT** depend on anything outside `(bytes,
parameters)` — not a file identity, an offset, a clock or a random seed. A
deployment secret is the one contemplated exception, and it is not free (§6).

### 3.6 Changing parameters is a migration event

Parameters are a **write-time, per-share** property. Reads never re-cut: a file's
chunk-ref list freezes its boundaries (RFC 0 §2.2), so content written under any
parameters is readable under any other.

Changing a share's parameters — or the boundary function, or a threshold derived
from it — therefore **MUST NOT** be treated as a configuration change. New writes
cut different boundaries, hash to different chunks, and dedup against nothing
already stored. An implementation **MUST** record the parameters that produced a
share's existing content, and **MUST** report a change as a migration rather than
applying it silently.

### 3.7 Invalid parameters MUST be rejected, not replaced

An implementation **MUST** reject invalid parameters at construction, with an
error, and **MUST NOT** substitute a default profile.

Invalid means: `Min` below the floor below which chunking is pointless;
`Min ≤ Target ≤ Max` violated, including `Min ≥ Target` (§3.3); or `Max` above the
largest chunk the buffer contract admits.

Silently substituting defaults is a data-shape failure disguised as robustness. A
share configured for 64 KiB chunks and quietly cut at 1 MiB produces valid,
readable, correctly hashed content that dedups against nothing the operator
expected and carries 16× the read amplification they sized for — and nothing
reports a problem. This is RFC 0 §1.2's silent-fallback hazard in its
configuration form: the absent capability is the requested chunk size, and its
absence **MUST** be loud.

## 4. Chunk identity

A chunk's identity is the BLAKE3-256 hash of its bytes, and nothing else. An
implementation **MUST NOT** include an offset, a file identity, a length, a
parameter set or a version in the hashed input.

The hash covers exactly the bytes emitted — the same bytes a later read must
reproduce — and **MUST** be computed over the chunk as cut, never over a buffer
that merely contains it.

## 5. Packing is a rule, not a component

Blocks are assembled by whoever owns the dedup query, which is not the carver
(RFC 6). The rules that assembly **MUST** follow belong here, because they are
properties of chunks:

| # | Rule |
| --- | --- |
| P1 | A block is a whole number of chunks. A chunk is never split to make a block an exact size. |
| P2 | A block reaches at least the block target and exceeds it by at most one chunk. |
| P3 | A block contains only chunks whose bytes it carries. |

P1 is RFC 0 §2.2: a split chunk would have one hash naming content in two blocks,
so the hash would stop being a locator, and a refcount would stop having a single
answer. P2 follows from P1 — ending at a chunk boundary means overshooting by at
most the crossing chunk.

P3 is worth stating because the obvious mistake is to let already-stored chunks
ride along in the block so the block's chunk list tiles a contiguous range. It
must not. A chunk that is already durable lives in some other block under its own
key; putting it in this one either duplicates its bytes or produces a block whose
chunk list disagrees with its contents. What needs every chunk — stored or not —
is the **file's manifest**, and the manifest is a different consumer of the same
sequence `Cut` returns.

> *Note.* Keeping these two consumers apart is also what lets pack-assignment
> policy change without touching chunking or breaking dedup. `restic` relies on
> exactly that separation, and used it to swap sequential pack assembly for a
> randomised one without a migration.

**Where the dangerous rule went.** Assembly consults a dedup oracle, and an oracle
that can see the block currently being assembled will report a chunk durable
before it has been uploaded — so an identical later chunk carries no bytes, and if
the block then fails to upload the content exists nowhere while metadata records
two refs to it. That hazard is real and it is **RFC 6's**, because that is where
the oracle is consulted. It is named here only so that removing it from the carver
does not look like deleting it.

## 6. Boundaries are public

The boundary function and its parameters are fixed constants in the source. Chunk
boundaries for any given content are therefore computable by anyone, and this
document does **not** provide membership privacy.

The consequence is a known-file channel: the sequence of chunk sizes a file
produces is a fingerprint, and an observer who can see object sizes and counts in
the remote tier can use it to test whether a particular file is stored. The same
channel underlies cross-tenant deduplication disclosure — that two tenants share
an object is itself an answer about content.

An implementation **MUST NOT** claim otherwise, and a deployment that needs
membership privacy **MUST** obtain it at another layer. Two mitigations exist and
neither is free:

- **A per-deployment secret in the boundary function** makes boundaries
  unpredictable. It also makes chunks incomparable across deployments, so
  cross-deployment dedup stops, and adopting it is a migration (§3.6). Keyed
  chunking is itself an active research target and recent work has broken
  deployed schemes, so it **MUST NOT** be adopted on the assumption that a key
  settles the question.
- **Randomising block assembly** blurs the relationship between chunk sizes and
  stored object sizes. It costs nothing in dedup and is not a migration, because
  by §5 assembly is policy. It is RFC 6's to adopt.

How much the channel actually reveals against DittoFS's block sizes is unmeasured
(§10).

## 7. Errors

`Cut` fails without losing the work it completed.

- An error from `emit` or from `r` **MUST** stop the call and be returned. Chunks
  already emitted have already been delivered; the carver **MUST NOT** try to
  retract them.
- A partial chunk held when an error occurs **MUST** be discarded, never emitted
  short. A short chunk is indistinguishable from a legitimate final chunk and
  would hash to something no later read reproduces.
- The carver **MUST NOT** report anything durable, or retain state that would let
  a retry cut differently. Failure handling is RFC 0 §5.2: extents stay **Dirty**
  and the pass is retried.

Because cutting is deterministic (B2) and stateless (§1.2), a retry over the same
run converges on the same chunks, so a failed pass costs work and never
correctness.

## 8. Invariants

| # | Invariant |
| --- | --- |
| C1 | A chunk's hash is a function of its bytes alone. |
| C2 | The same bytes and parameters yield the same boundaries, in any process or version. |
| C3 | The expected chunk size is the configured target. |
| C4 | Every chunk is within `[Min, Max]`, except a run's final chunk. |
| C5 | A block is a whole number of chunks and exceeds its target by at most one. |
| C6 | A block contains only chunks whose bytes it carries. |

C3 is the one the current implementation fails (§3.2.1). C1 and C2 are what make
a hash an address; the rest cost wrong sizing or an unreadable file.

Two invariants that appeared in an earlier draft are gone because they became
structural: a chunk cannot span a gap (§2.1), and there is no rolling state to
reset. Two more moved to RFC 6 with the dedup oracle.

## 9. Conformance

RFC 1 §11 applies unchanged: conformance is every **MUST** holding, and a check is
validated by reverting the code and watching it fail on its own assertion.

Every check below is a pure function of a byte slice and a set of parameters, per
§1.3. If a check here needs a fixture, the implementation has acquired a
dependency it is not allowed to have, and that is itself the finding.

| Requirement | Check |
| --- | --- |
| §3.2 target is realised | Cut incompressible data; assert the mean is within tolerance of `Target`. **Fails against the current implementation** (§3.2.1) and is the regression gate for fixing it. |
| §3.2 thresholds derive from target | Construct at several targets; assert the derived threshold's population count tracks the target, and that one hardcoded pair cannot satisfy two of them. |
| §3.7 invalid params rejected | Construct with `Min` below the floor, with `Min ≥ Target`, and with `Max` above the ceiling; assert each errors and no carver is produced. |
| §3.4 warm-up equivalence | Assert boundaries are identical whether rolling state is warmed from the chunk start or over the declared window, across several profiles. |
| B4 shift resistance | Insert one byte early in a large input; assert every boundary past the edited chunk is unchanged. |
| §4 hash covers the chunk | Cut identical content at two different `base` offsets; assert one hash. |
| §2.2 borrowed bytes | Retain the slice passed to `emit` and assert it is observed to change — the check exists to prove the contract is real, so that a caller that copies is not doing so out of superstition. |
| §7 no short chunk on error | Fail `emit` mid-run and fail `r` mid-chunk; assert no emitted chunk is a truncated prefix of a chunk the un-errored path would produce. |

Two things **MUST NOT** stand in:

- **Compressible or patterned data MUST NOT be the only input for the
  distribution checks.** A gear hash degenerates on repetitive input, which is the
  one regime where `Max` is reachable, so such a rig measures the opposite of the
  common case. It **MUST** be covered separately, since it is what `Max` is for.
- **A single fixed profile MUST NOT be the only parameters under test.** The
  defect in §3.2.1 is invisible at one profile and obvious across two.

## 10. Open questions

1. **Which way out of §3.2.1.** Both exits re-cut existing content, so the choice
   gets more expensive the longer shares write under the current profile. What is
   unmeasured is the right `Target` for the SMB large-file workload — the trade is
   read amplification against index and refcount cardinality, and it is now
   legible because the distribution is characterised.
2. **Warm-up fix** (§3.4). 11.6× on the boundary decision with identical output,
   verified. Unmeasured is the end-to-end effect on a pass, where cutting was 13%
   (amd64) and 29% (arm64) of CPU behind buffer allocation. Independent of
   everything else here and the cheapest to land.
3. **Whether `Min` survives** (§3.2). Once thresholds derive from `Target`, `Max`
   still bounds degenerate repetitive input and read amplification, but `Min`
   would exist only to bound per-chunk overhead. `Target` plus `Max` is simpler
   and not obviously worse; deciding needs the repetitive-input case measured.
4. **How much §6 leaks.** That boundaries are public is certain; what an observer
   can actually recover from DittoFS's object sizes is not. Whether randomised
   block assembly is worth adopting pre-emptively depends on it, and it is cheap
   enough that it may be worth doing without the measurement.
5. **Buffer cost after the split.** Per-chunk allocation is now forbidden (§2.2)
   and the block-sized buffer moved to RFC 6. Two profiles put large-buffer
   allocation and zeroing at 53% (amd64) and 9% (arm64) of carve cost, but on a
   workload whose dedup oracle was stubbed and whose absolute figures did not
   reproduce an earlier baseline. The ordering is solid; the magnitude is not, and
   the split changes where the cost lands.
