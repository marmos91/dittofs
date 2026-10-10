---
rfc: 5
title: "RFC 5 — transforms"
component: transforms
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-4-remote-tier]]"
aliases:
  - RFC 5
tags:
  - rfc
---
# RFC 5 — transforms

**Status:** draft.
**Audience:** anyone writing a transform, configuring a store's transforms, or
deciding what a deployment that enables one is protected against. Conventions
and test tiers are in [the RFC index](rfc-index.md).

This document specifies behaviour, not the current code. Where the code differs,
[Appendix D](#Appendix%20D%20%E2%80%94%20where%20the%20current%20code%20differs) lists it for the refactor.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

Before a chunk of file content leaves the machine for the remote object store,
it may be compressed, encrypted, or both. A **transform** is one such step,
applied to one chunk at a time; a store's **chain** is the transforms its
operator turned on. This RFC specifies what any transform must guarantee, how a
chain is configured and changed over time, and what encryption does and does not
protect against.

In outline: clients write over NFS (Network File System) or SMB (Server Message
Block) into a local journal; in the background the bytes are cut into chunks of
about 256 KiB, each named by a keyed hash of its content, packed into blocks of
about 4 MiB and uploaded to an S3 (Simple Storage Service) bucket
([RFC 0](rfc-0-data-lifecycle.md)). Transforms run in the middle of that, per
chunk, as the block is encoded ([RFC 4](rfc-4-remote-tier.md)).

### The problem, in one example

The `profiles` share stores its blocks in the bucket `dfs-data`, with a chain of
compression (zstd) then encryption (AES-256-GCM-SIV, under a key belonging to
the share's namespace in the bucket). The namespace was created encrypting, and
the operator turned compression on with it explicitly, accepting the size leak
([Appendix A](#Appendix%20A%20%E2%80%94%20compression)); a chain that encrypts leaves compression off unless told.

1. **February.** alice-pc writes part of her mailbox into
   `profiles/alice/ODFC_alice.vhdx`. One 256 KiB chunk of it, `X`, is hashed
   first, over its plain bytes and under the namespace's chunk-ID key: that
   keyed hash, `X`'s **chunk ID**, names it and never changes.
   Compression shrinks it to 150 KiB; encryption seals that. The stored body
   begins with a 6-byte **envelope** listing what was applied: compression,
   then encryption. A chunk of an already-compressed photo in the same profile
   would not shrink by 1/16, so compression declines it and its envelope lists
   only encryption.
2. **March.** The operator turns compression off for this store, because
   anyone who can put mail into a profile disk can learn from compressed sizes
   what else that chunk holds ([Appendix A](#Appendix%20A%20%E2%80%94%20compression)). This
   governs new writes only. Encryption could not be turned off the same way: a
   namespace encrypts, or not, from its creation.
3. **April.** alice's laptop asks for the range holding `X`, which is no longer
   local. If the reader followed the current configuration, it would only
   decrypt, hand back compressed bytes, fail the hash check, and every chunk
   written before March would be unreadable: a configuration change would have
   destroyed data. Instead the reader follows `X`'s own envelope: it decrypts,
   decompresses, and checks the result against `X`'s hash. That check, not any
   transform's own, decides whether the bytes are correct.
4. **If the key service is unreachable** in April, the read fails as the remote
   tier being unavailable and is retried; it never returns zeros. If the key had
   been destroyed for good, `X` would be reported **Lost**.

```text
 chunk X of alice's VHDX, 256 KiB plain
   │
   ├──► keyed hash of the plain bytes: X's chunk ID, checked again on read
   ▼
 stage 1  compress (zstd)        declines if it saves less than 1/16
 stage 2  encrypt (namespace key)
 stage 3  redundancy             not configured here
   ▼
 body = envelope [compress, encrypt] + encrypted bytes  ──►  block in dfs-data
   │
 read: undo what the envelope lists, in reverse ──► check hash ──► alice
```

### The words you need

- **Chunk ID**: a chunk's identity, a keyed hash of its plaintext under the
  namespace's chunk-ID key ([Appendix B.2](#B.2%20Keys)); "plaintext hash" in this
  document means it.
- **Transform**: an invertible step over one chunk's bytes, which may decline a
  chunk it cannot help ([§2.1](#2.1%20A%20transform%20acts%20on%20one%20chunk)).
- **Chain** and **stage**: the configured transforms, at most one per stage, in
  the fixed order compress, encrypt, redundancy ([§2.3](#2.3%20The%20chain%20order%20is%20fixed)).
- **Envelope**: the few bytes at the head of each stored body listing which
  transforms were applied ([§2.4](#2.4%20Every%20body%20records%20what%20was%20applied)).
- **Material**: what a transform needs from outside the body, such as a key,
  held by a provider by ID with a fingerprint of its content
  ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)).
- **Chain ID**: a hash of everything in the chain that decides a body's bytes,
  part of every [block](rfc-0-data-lifecycle.md#Glossary) name ([§2.8](#2.8%20The%20chain%20ID)).
- **Census**: the record, kept with each block, of which transforms and which
  material its bodies used; it says when a key can be retired
  ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)).

### What this RFC promises

- A chunk's ID is always a keyed hash of its plain bytes, so turning compression
  on or off, or rotating a data key, never changes which chunk is which. In an
  encrypting namespace no one without its keys can compute an ID; in one that
  does not encrypt, the chunk-ID key sits in the clear beside the blocks, so the
  ID claims nothing there ([Appendix B.2](#B.2%20Keys)).
- Every key there is, and what losing each one costs, is one table
  ([Appendix B.2](#B.2%20Keys)); every other RFC cites it.
- Changing the chain changes only what is written next; every stored body stays
  readable as long as its key is held. Whether a namespace encrypts is fixed when
  it is created: encryption can change algorithm or key, never be removed or added.
- No byte is returned unless the fully decoded chunk matches its hash.
- A store whose chain cannot be built does not open: it never writes plain bytes
  because encryption failed to load.
- A missing key is an outage, retried, never zeros; a key lost for good makes
  exactly the chunks it covered **Lost**, and says so. A key the provider was
  never given is a configuration fault, reported as such, never corruption.

### How the rest is organised

[§2](#2.%20The%20model) is the model: what a transform is, the fixed order, the
envelope, material, failures and the chain ID. [§3](#3.%20API%20surface) is the
interface and configuration; [§4](#4.%20Writing%20a%20custom%20transform) is the
checklist for writing a new transform; [§5](#5.%20Changing%20a%20chain%20over%20time)
covers change over time, including retiring a key. [§7](#7.%20Invariants) and
[§8](#8.%20Test%20plan%20and%20benchmarks) are invariants and tests. The
shipped transforms are [Appendix A](#Appendix%20A%20%E2%80%94%20compression)
and [Appendix B](#Appendix%20B%20%E2%80%94%20encryption);
[Appendix B.5](#B.5%20What%20a%20bucket%20reader%20still%20learns) says what
someone who can read the bucket still learns. On a first read, skip §3, §4 and
[Appendix C](#Appendix%20C%20%E2%80%94%20example%2C%20a%20parity%20transform).

## In short

- A **transform** is an invertible function over one chunk's bytes: compression
  and encryption are the two that ship, and anyone can add another.
- A store's **chain** is the list of transforms its operator configures. The
  order is fixed in code — compress, then encrypt, then redundancy — and a
  configuration in any other order is refused.
- Every chunk body records which transforms were applied to it, so reading needs
  no configuration, and a transform may skip a chunk it cannot help.
- The chain runs inside the block codec ([RFC 4 §3](rfc-4-remote-tier.md#3.%20The%20block%20format)), per chunk, after the chunk's
  hash is taken and before the body is written. Nothing else sees it.
- After the chain is undone, the codec checks the plaintext hash. That check, not
  any transform's own, decides whether a chunk is correct.
- Encoding is deterministic within one put attempt, and the chain's identity, its
  **chain ID**, is part of every block name, so one name is always one byte
  sequence.

## 1. Purpose

Data leaving the machine may need to be smaller, secret, or both, and what "both"
means differs per deployment. This document specifies the generic mechanism: what
a transform is, how transforms are stacked and configured, where they run, and
what any transform must guarantee. Compression ([Appendix A](#Appendix%20A%20%E2%80%94%20compression)) and encryption
([Appendix B](#Appendix%20B%20%E2%80%94%20encryption)) are the two transforms that ship, and serve as worked
examples.

### 1.1 Non-goals

This document **MUST NOT** be read as specifying:

- the block format, the chunk index or where a body sits: [RFC 4 §3](rfc-4-remote-tier.md#3.%20The%20block%20format);
- a chunk's hash or a block's name: [RFC 2 §4](rfc-2-carver.md#4.%20Identity). A transform never changes either;
- when a chunk is encoded, or whether a block is relocated: [RFC 8](rfc-8-engine.md) and [RFC 9](rfc-9-gc.md);
- protection of data in the journal, in metadata or on the wire to the remote
  store: those are disk, database and transport concerns.

## 2. The model

### 2.1 A transform acts on one chunk

A transform takes one chunk's bytes and returns one body, and can turn that body
back into the same bytes. It never sees a block, a file, an offset or another
chunk. Within that, it may do anything: make the body smaller (compression),
unreadable (encryption), larger (padding, parity for error correction), or
anything else invertible.

Per-chunk scope is what keeps ranged reads working: a get for one chunk's body
([RFC 4 §4.4](rfc-4-remote-tier.md#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)) returns something the chain can undo on its own, without the rest of
the block. It also means one corrupt body loses one chunk.

A transform **MUST** be:

- **invertible**: `Decode(Encode(p)) == p`, byte for byte, for every input,
  including inputs chosen to look like the transform's own output. A lossy
  transform is not a transform;
- **self-contained**: everything `Decode` needs is in the body it wrote, apart
  from **material** held outside it: a key, a dictionary, any versioned input the
  transform names by ID ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material));
- **size-bounded**: it declares `MaxEncodedLen(n)`, the largest body it can
  produce from `n` bytes, and never exceeds it. `Decode` **MUST NOT** allocate or
  produce more than the `max` it is given, however large a body claims its output
  to be;
- **stateless per call**: safe to call concurrently, with no memory between calls
  other than read-only configuration and material;
- **deterministic**: `Encode` of the same input, with the same settings,
  material and format version, returns the same body every time. Anything that
  would otherwise be random, such as an encryption salt, is derived from the
  chunk's hash ([Appendix B.1](#B.1%20How%20a%20chunk%20is%20encrypted)). What correctness needs is narrower: a block's
  name is minted fresh for each put attempt ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), and within that
  attempt the measuring pass, the put and any retry of the put **MUST** produce
  the same bytes, so the positions planned by the first pass are the positions
  stored ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)). Two attempts, or two builds, need not agree for any name
  to stay right; the transform's fixtures ([§4](#4.%20Writing%20a%20custom%20transform), item 7) still hold its
  output fixed across builds within one format version;
- **honest about length**: `LengthOf(n)` returns the exact body length for an
  `n`-byte input and whether that is known without encoding. Encryption, parity
  and the identity know it; compression does not. When every transform in the
  chain knows it, a put is encoded once; otherwise it takes a measuring pass
  ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)).

A transform **MAY** decline a chunk: compression declines one that would not
shrink. A declined chunk passes to the next transform unchanged, and the body
records that the transform was not applied ([§2.4](#2.4%20Every%20body%20records%20what%20was%20applied)).

What does not fit, by design: a transform that loses information, one that needs
another chunk (delta encoding against a neighbour), and one that turns a chunk
into several bodies stored in different places (erasure coding across failure
domains). The last belongs to the store layer, as a store that spreads fragments
over several backends, not to the chain.

Nor does encryption meant to be computed on, such as homomorphic encryption. Its
point is to let a party without the key compute on ciphertext, and no such party
exists here: the remote store only keeps bytes, and every computation on content
happens in the process that holds the keys. Its schemes are also randomised by
construction and expand data by orders of magnitude, against determinism and a
bounded `MaxEncodedLen`. A design in which the store computes on content is a
different architecture, not a transform.

### 2.2 Where it runs

The chain runs inside the block codec, which the engine owns ([RFC 4 §3.1](rfc-4-remote-tier.md#3.1%20Who%20writes%20it)).

![The chain in its fixed order: the chunk is hashed first, then compress, encrypt and redundancy run in that order, each able to decline; the envelope records which ran. Reading undoes the listed stages in reverse and the plaintext hash decides](img/rfc5t-chain.svg)

- The hash is taken over plaintext **before** the chain, so identity never
  depends on a transform's settings, material or library version ([RFC 4 §3.3](rfc-4-remote-tier.md#3.3%20Transforms)).
- The hash is checked **after** the whole chain is undone ([§2.6](#2.6%20The%20plaintext%20hash%20is%20the%20final%20check)).
- Every consumer of block bytes goes through the codec, so every consumer gets
  the chain: the engine's reads and writes, and GC's relocation, which re-encodes
  under the current chain ([§5.2](#5.2%20Relocation%20re-encodes)).

The syncer and the remote store never see a transform. Apart from the byte
offsets a transform changes, nothing above the codec behaves differently with a
chain on or off.

### 2.3 The chain order is fixed

Every transform declares one **stage**, and the stages run in one order, fixed in
code:

| Stage | Transforms | Why there |
| --- | --- | --- |
| 1. compress | compression | needs the plaintext's redundancy |
| 2. encrypt | encryption | covers everything before it |
| 3. redundancy | parity | protects the stored bytes, so it must see them last |

A chain holds at most one transform per stage. `Encode` runs them in stage order
and `Decode` in reverse. A configuration that lists transforms out of stage
order, two in one stage, or one transform twice **MUST** be refused when the
chain is built ([§2.7](#2.7%20Failures)); the system never reorders it silently. A custom
transform ([§4](#4.%20Writing%20a%20custom%20transform)) declares one of these stages.

Every other order is at best ineffective — compressing after encrypting saves
nothing, parity before encryption cannot repair what a flipped ciphertext bit
breaks — and none is worth a configuration surface or the tests to cover it.

The envelope ([§2.4](#2.4%20Every%20body%20records%20what%20was%20applied)) and the registry ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)) stay: they are what
lets a transform be skipped per chunk, and what lets an algorithm be replaced by
another in the same stage ([§5.4](#5.4%20Changing%20an%20algorithm%20is%20adding%20a%20transform)).

### 2.4 Every body records what was applied

Each body starts with a short **envelope**: its own version, then, in order, the
IDs of the transforms that were applied to it:

```
body = envelope version (1 byte) ‖ count (1 byte) ‖ transform ID (2 bytes) × count ‖ output of the last applied transform
```

This is what makes the chain self-describing, per chunk, independently of
configuration:

- a declined transform is simply absent from the list;
- the list, not each transform, says whether a transform ran, so no transform
  needs a marker of its own and no input can be mistaken for a transform's output;
- a reader undoes exactly the listed transforms, whatever the chain is configured
  to do now.

A transform **MAY** still write a header of its own inside its output: its
format version, and the IDs of the material it used. The envelope costs 2 + 2n
bytes per chunk.

The envelope carries its own version because a ranged read of one chunk's body
([RFC 4 §4.4](rfc-4-remote-tier.md#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)) never sees the block header, and so cannot learn the block format
version from it. A new envelope layout is a new envelope version; a reader keeps
decoding every version it may still meet, and rejects a version it does not know
with `ErrMalformed`. Version 1 is the layout above.

![A body: the versioned envelope listing the applied transforms, then encryption's output with its header wrapping compression's output, with the chunk's bytes innermost](img/rfc5t-body.svg)

A reader **MUST** reject with `ErrMalformed`, before decoding anything, an
envelope with an unknown version, or one that lists an unregistered ID, the same
ID twice, two IDs of one stage, or IDs out of stage order ([§2.3](#2.3%20The%20chain%20order%20is%20fixed)). An
envelope therefore lists at most one transform per stage. Otherwise a bucket
writer could wrap a body in hundreds of layers that each decode correctly and
cost a full decode on every read.

The envelope is outside every transform, so a redundancy transform cannot repair
it: a damaged envelope fails the chunk loudly, as it would without one. Giving
the envelope its own protection is deferred until a redundancy transform ships.

### 2.5 Reading needs no configuration, only material

Every transform compiled into the binary is registered by its ID at start, and
`Decode` looks transforms up by the IDs in the envelope, not in the configured
chain. For each store, the registry builds every transform once for decoding,
with its default settings and the store's material.

**Material is configured on the store configuration, apart from the chain, and
belongs to one namespace** ([§3.2](#3.2%20Configuration)). A material provider holds it by namespace,
kind (`data-key`, `dictionary`, whatever a transform declares) and ID. Every
call that names material names its namespace too: two namespaces on one store
configuration never share a data key, a header key or a chunk-ID key, so a
lookup without the namespace could return another tenant's key. Removing a transform from the chain therefore stops it for new
writes but keeps its material, and bodies written under it stay readable.
Removing material is a separate act, allowed only once the census shows nothing
uses it and no encode in flight is using it ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)).

**Material carries a fingerprint.** Each piece of material is stored with its ID
and a **fingerprint** of its content, `HMAC-SHA256(material, "dittofs key check
v1")`. The provider **MUST** refuse to serve an ID whose material no longer
matches its recorded fingerprint — a key file replaced under the same ID, a key
service returning a different key — and reports it as `ErrMaterialUnavailable`
with a health condition: the right material may come back, and decoding with the
wrong one would only turn every body under it into a verification failure.
Everything that names material outside the provider names it by (ID,
fingerprint): the chain ID ([§2.8](#2.8%20The%20chain%20ID)), the census ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)) and a namespace
export.

**A read asks only for material its block recorded.** The codec hands the chain
the block record's census ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)), and `Decode` **MUST** check that every
material ID a body names appears there before asking the provider for it. An ID
that does not is `ErrMalformed`, without a provider call. Otherwise a bucket
writer could plant an ID in a body and send every read of it to a key service,
or turn a corrupt body into a remote that looks unavailable.

**Not every kind is consumed by a transform.** The provider also holds the
namespace's **chunk-ID key** (kind `chunk-id-key`), under which every chunk ID
is computed; in an encrypting namespace its **chunking key** (kind
`chunking-key`), which the carver consumes for keyed boundaries
([RFC 2 §6](rfc-2-carver.md#6.%20Boundaries%20are%20public)), and its **header key** (kind `header-key`), which seals the
chunk IDs and the chunk index in block headers ([Appendix B.5](#B.5%20What%20a%20bucket%20reader%20still%20learns)); and its
**export key** (kind `export-key`), which seals and authenticates the
namespace's exports and backup state objects ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)). Which kinds
exist in which namespace, and how each is held, is the key table of
[Appendix B.2](#B.2%20Keys). Each carries a fingerprint like any material; an export carries
each one as the table says, with its fingerprint, and the importer checks them.

**Material says what it is for.** A kind is specific to what consumes it — a key
for one cipher is not a key for another, whatever their lengths — so a transform
declares the kinds it accepts and **MUST** refuse, at construction, material of
any other kind. Handing one cipher's key to another is then a failure to start,
not a body written under a key used for two purposes.

Because reading does not consult the chain, a store accepts a body without a
transform its chain now applies: an old plaintext body after encryption was
turned on, for example. An operator who wants to refuse such bodies lists the
transform under `require`; a body missing a required transform is then
`ErrMalformed`. Turn it on only once the census shows no body lacks it.

An ID is assigned once and never reused ([§4](#4.%20Writing%20a%20custom%20transform)). A format change inside a
transform is a new version in that transform's own header, not a new ID, and the
transform **MUST** keep decoding every version it may still meet.

### 2.6 The plaintext hash is the final check

After the chain is undone, the codec compares the result with the chunk's
plaintext hash from block metadata ([RFC 4 §3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec)). That check decides; no
transform's own check stands in for it. An encryption tag proves a body was
sealed for that hash, not that decompression then reproduced it; a parity
transform that "repairs" a body can still repair it wrongly.

It also makes a tampered envelope harmless to integrity. Someone who can write
the bucket can strip a transform from an envelope or swap a body, but the result
must still hash to what local metadata expects, and producing that requires
knowing the plaintext already.

### 2.7 Failures

- **A chain that cannot be built is a construction failure.** If a store is
  configured with a transform that cannot start (an unknown name, bad settings,
  material it needs and cannot get), the store **MUST NOT** open, and **MUST
  NOT** fall back to writing without it. Writing plaintext because encryption
  failed to load is the failure this rule exists for.
- **`Decode` errors are one of five.**

  | Error | Meaning | Reported by the codec as |
  | --- | --- | --- |
  | `ErrMalformed` | the body is not something this transform wrote, fails its own integrity check, or names material its block record's census does not list | `ErrCorrupt`: a verification failure of the chunk |
  | `ErrTooLarge` | the output would exceed `max` | `ErrCorrupt`: a verification failure of the chunk |
  | `ErrMaterialUnavailable` | material the store holds cannot be reached now | the remote being unavailable: retryable, never zeros, never an absent chunk ([RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model)) |
  | `ErrUnknownMaterial` | material the census lists but the provider was never given: a key not loaded on this installation, a provider pointed at the wrong key set | as `ErrMaterialUnavailable`, with a configuration health condition naming the ID: the material exists somewhere, and loading it ends the outage |
  | `ErrMaterialDestroyed` | material the store once held is gone for good | itself, never `ErrCorrupt`: the chunk is **Lost** ([RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function)), not retried, and not counted as a verification failure, since its body may be intact |

  The distinctions matter. A material ID planted by a bucket writer must not
  turn a corrupt body into a remote that looks unavailable forever — which is
  why the census check of [§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material) runs before the provider is asked — and a key
  that is gone for good must not be retried forever as if it might return. An ID
  that passed the census check came from metadata, not from the bucket, so a
  provider that does not know it is misconfigured: reporting that as corruption
  would mark every chunk under the key damaged, and a restored installation
  that had not yet loaded its keys would condemn its own data.
  `ErrCorrupt` is the remote store's ([RFC 4 §4.8](rfc-4-remote-tier.md#4.8%20Errors%20are%20a%20closed%20set)); `ErrMaterialUnavailable` and
  `ErrMaterialDestroyed` are the two errors the syncer adds to that set
  ([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)). Folding a destroyed key into `ErrCorrupt` would raise a
  corruption alert for every chunk under a key the operator retired on purpose,
  and hide which loss was a key and which was the bucket.
- **`Encode` errors** fail the put. A declined chunk is not an error.

A transform's own error types **MUST NOT** cross the codec. Callers see only the
codec's errors, so nothing above it can tell which transforms are configured.

### 2.8 The chain ID

A block's name includes a **chain ID** ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)): a hash of everything in
the chain that decides a body's bytes or length, taken under its own versioned
domain, `transform chain id v1`, as the block name is ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)). For each
configured transform, in order, it covers:

- the transform's ID and the format version it writes;
- its **length-affecting settings**: a compression level is one; a setting that
  changes only speed, such as a thread count, is not. Each transform declares
  which of its settings count;
- the (ID, fingerprint) of the current material it uses for the namespace being
  written ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)), so a key rotation changes the ID. A chain ID is therefore per
  namespace: one chain configuration yields one chain ID for each namespace it
  writes.

The stage order is fixed ([§2.3](#2.3%20The%20chain%20order%20is%20fixed)), so order is not an input of its own.

**Its bytes.** The chain ID is 32 bytes:

    chain ID = BLAKE3-derive-key("transform chain id v1", encoded chain), 32 bytes of output

where `context` is that ASCII string exactly, and `encoded chain` is:

| Field | Bytes | Encoding |
| --- | --- | --- |
| transform count | 1 | number of configured transforms, 0 to 3; an empty chain is count 0 and still has a chain ID |
| then, per transform, in stage order: | | |
| transform ID | 2 | big-endian |
| format version | 1 | the version its `Encode` writes |
| length key | 4 + *k* | *k* as 4 bytes big-endian, then `LengthKey()`'s *k* bytes ([§3.1](#3.1%20Interfaces)) |
| material count | 1 | number of current materials the transform uses for the namespace |
| then, per material, ordered by kind then ID, bytewise ascending: | | |
| kind | 4 + *k* | length as 4 bytes big-endian, then the kind's ASCII bytes |
| ID | 4 + *k* | length as 4 bytes big-endian, then the ID's bytes |
| fingerprint | 32 | `HMAC-SHA256(material, "dittofs key check v1")` ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)) |

Integers are unsigned and big-endian. A block name always carries a 32-byte
chain ID ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)). Each transform's `LengthKey` is
its own canonical encoding and its own golden vector; the format version and the
material are encoded here, not in it.

A block's name is not derived from its content alone: it also carries a
nonce minted fresh for each put attempt ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), so two writers of the
same chunks produce two names and two objects. The chain ID stays in the name
for what is left: an attempt fixes its chain when it starts, and the name it
mints records that chain, so one name is one layout even if the configuration
changes between the measuring pass and the put, and a recorded position never
goes stale ([RFC 4 §3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec)). The block header carries the chain ID with the nonce, so
a whole-block read can recompute and check the name. The chain ID is not needed
to decode: a body's envelope says how ([§2.4](#2.4%20Every%20body%20records%20what%20was%20applied)).

**Every layer carries its own version, so each can change alone.** A change is
made at one of five points, and each is read from the stored bytes, not from
configuration:

| What changes | Where its version is | What a change costs |
| --- | --- | --- |
| the block layout | the block header's format marker and version ([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)) | readers keep every stored version ([RFC 4 §3.5](rfc-4-remote-tier.md#3.5%20Format%20changes%20are%20migrations)); relocation rewrites old blocks when they are to go |
| the envelope layout | the envelope's own version byte ([§2.4](#2.4%20Every%20body%20records%20what%20was%20applied)), readable from a ranged get | readers keep every envelope version they may meet |
| one transform's output format | that transform's header ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)) | the transform decodes every version it wrote |
| the algorithm | a new transform, with a new ID ([§5.4](#5.4%20Changing%20an%20algorithm%20is%20adding%20a%20transform)) | configuration, then retirement by census |
| how the chain ID, the block name or a key is derived | the versioned labels: `transform chain id v1`, the block name's domain ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), and each derivation label of [Appendix B.1](#B.1%20How%20a%20chunk%20is%20encrypted) | new names or keys for new blocks only; a stored block keeps its name, which is recorded, never recomputed to find it, and a body keeps the derivation its transform version names |

A golden test vector pins each derivation, as for the block name: a changed
vector fails the build.

## 3. API surface

### 3.1 Interfaces

Signatures are indicative; the obligations of [§2](#2.%20The%20model) are normative.

```go
// ID names a transform in every body it was applied to. Assigned once, never reused.
type ID uint16

// Stage is a transform's fixed place in every chain (§2.3).
type Stage uint8

const (
	StageCompress Stage = iota + 1
	StageEncrypt
	StageRedundancy
)

// Transform is one invertible, per-chunk step.
type Transform interface {
	ID() ID
	Stage() Stage

	// MaxEncodedLen is the largest body Encode can produce from n bytes.
	MaxEncodedLen(n int) int

	// LengthOf is the exact body length for n bytes, and whether it is known
	// without encoding. A declined chunk is never known in advance.
	LengthOf(n int) (int, bool)

	// LengthKey encodes, canonically, the settings that decide the body's
	// length: this transform's part of the chain ID. The chain ID adds the
	// transform ID, format version and material itself (§2.8).
	LengthKey() []byte

	// Encode transforms plain, deterministically (§2.1), for namespace ns.
	// Applied=false declines the chunk: plain passes on unchanged. hash is the
	// chunk ID, for transforms that bind to it or derive from it.
	Encode(ctx context.Context, ns NamespaceID, dst []byte, hash [32]byte, plain []byte) (Encoded, error)

	// Decode inverts Encode. It never produces more than max bytes, and asks m
	// only for material the block's census lists (§2.5).
	Decode(ctx context.Context, ns NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m Materials) (plain []byte, err error)
}

// Encoded is one transform's output, and what the census records about it.
type Encoded struct {
	Body     []byte
	Applied  bool
	Version  uint8        // the transform's own format version
	Material []MaterialID // the material used, if any
}

// MaterialID names one piece of material of one kind, of one namespace.
type MaterialID struct {
	Namespace   NamespaceID
	Kind        string
	ID          string
	Fingerprint [32]byte // HMAC-SHA256(material, "dittofs key check v1") (§2.5)
}

// Materials holds a store configuration's material, per namespace. A
// transform asks it for what it needs.
type Materials interface {
	// Current is the material new bodies of this kind use in namespace ns.
	Current(ctx context.Context, ns NamespaceID, kind string) (MaterialID, error)
	// Get returns material by ID, whose Namespace it is looked up in:
	// ErrUnknownMaterial for an ID the provider was never given,
	// ErrMaterialUnavailable for one it cannot reach now or whose content no
	// longer matches its fingerprint, ErrMaterialDestroyed for one it once had
	// and has lost or removed for good.
	Get(ctx context.Context, id MaterialID) ([]byte, error)
	// Remove destroys material: it refuses while the material is current in
	// any open chain, waits for encodes in flight that hold it, then checks
	// the census, all under one fence, and erases every copy (§5.3). A master
	// key or export key that a retained export names is ErrMaterialInUse.
	Remove(ctx context.Context, id MaterialID) error
}

// Factory builds a transform from its settings and the store's material. It
// fails rather than returning a transform that cannot work.
type Factory func(ctx context.Context, settings map[string]any, m Materials) (Transform, error)

// Register makes a transform available for configuration and for decoding.
// Called at init; a duplicate ID panics.
func Register(id ID, name string, f Factory)

// Chain is a store's configured list of transforms.
type Chain struct{ /* ... */ }

// NewChain builds the configured transforms. It refuses a duplicate, two
// transforms of one stage, or a list out of stage order (§2.3).
func NewChain(ctx context.Context, cfg []Config, m Materials) (*Chain, error)

// MaxEncodedLen composes the transforms' bounds, in stage order, plus the
// envelope. It sizes writes.
func (c *Chain) MaxEncodedLen(n int) int

// MaxDecodeLen is the largest body any registered transforms could have
// written from n bytes, one per stage, plus the envelope. It sizes reads.
func MaxDecodeLen(n int) int

// LengthOf composes the transforms' LengthOf; false if any one is false.
func (c *Chain) LengthOf(n int) (int, bool)

// ID is the chain ID (§2.8) for namespace ns, from each transform's ID,
// LengthKey and ns's current material.
func (c *Chain) ID(ns NamespaceID) [32]byte

// Encode writes the envelope and returns the body with one Encoded per applied
// transform, which the codec collects into the block's census (§5.3).
func (c *Chain) Encode(ctx context.Context, ns NamespaceID, dst []byte, hash [32]byte, plain []byte) ([]byte, []Encoded, error)

// Decode undoes the envelope's transforms. census is the block record's
// (§5.3): a body naming material outside it is ErrMalformed.
func (c *Chain) Decode(ctx context.Context, ns NamespaceID, dst []byte, hash [32]byte, body []byte, max int, census []Encoded) ([]byte, error)

type Config struct {
	Name     string         // a registered transform's name
	Settings map[string]any // passed to its Factory
}

var (
	ErrMalformed           = errors.New("transform: malformed body")
	ErrUnknownMaterial     = errors.New("transform: unknown material")
	ErrMaterialUnavailable = errors.New("transform: material unavailable")
	ErrMaterialDestroyed   = errors.New("transform: material destroyed")
	ErrTooLarge            = errors.New("transform: output exceeds its bound")
	ErrMaterialInUse       = errors.New("transform: material named by a retained export")
	ErrNotEscrowed         = errors.New("transform: master key escrow not verified") // Appendix B.2
)
```

`Chain.Decode` uses the registry, not the configured list ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)), so a chain
decodes what an older configuration wrote. It gives each step the chunk maximum
([RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it)) passed through the `MaxEncodedLen` of the steps applied before it, so
a step that legitimately enlarges the body is not refused by the next one. `dst`
**MUST NOT** overlap the input; the result may be `dst` resliced or a new slice.
The `Materials` a transform's `Decode` receives is a view that answers only for
the material IDs in `census`, of the namespace being read, and returns
`ErrMalformed` for any other without asking the provider. `ErrUnknownMaterial`
from the provider, for an ID the census does list, is reported as
`ErrMaterialUnavailable` with a configuration health condition
([§2.7](#2.7%20Failures)), never as `ErrMalformed`.

Two bounds size everything downstream. `Chain.MaxEncodedLen` of the chunk
maximum bounds what this chain writes: the largest encoded block on a put. A
read may meet a body any earlier chain wrote, so the read side — the syncer's
memory bound ([RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound)), the block header's bound ([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)) and the
per-body length check before allocating — uses `MaxDecodeLen`, the maximum over
every registered transform, never the current chain's bound. Removing parity
from a chain must not make the parity bodies still stored unreadable.

### 3.2 Configuration

A chain is part of a **store configuration** ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)), which the control
plane holds ([RFC 13](rfc-13-configuration.md)): every namespace created on it, and every share in
those namespaces, inherits it. The engine reads the record and builds the chain
from it when it opens a namespace's store ([RFC 8 §2.4](rfc-8-engine.md#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced)). No file on the host
describes it. The record holds:

| Field | Meaning |
| --- | --- |
| `transforms` | the transforms, each a registered name and its settings, at most one per stage and listed in stage order ([§2.3](#2.3%20The%20chain%20order%20is%20fixed)) |
| `materials` | the material provider and how to reach it; a reference to secret material, never the material itself ([RFC 13](rfc-13-configuration.md)). Required on every store configuration, since every namespace has an export key wrapped under a master key; where the master key must live is the key table's ([Appendix B.2](#B.2%20Keys)) |
| `require` | transforms every body must carry ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)); optional |

A change to the record governs the next write only ([§5.1](#5.1%20Configuration%20governs%20the%20next%20write)), with one exception.

**Whether a namespace encrypts is fixed at its creation.** A namespace records,
when it is created, whether its store configuration's chain had an encrypt stage
then, and that never changes ([RFC 13 §5.1](rfc-13-configuration.md#5.1%20A%20bound%20setting%20refuses%20change)). A change to the chain that would
add or remove the encrypt stage **MUST** be refused while any namespace of the
configuration holds content; one that replaces the encryption transform by
another of the same stage ([§5.4](#5.4%20Changing%20an%20algorithm%20is%20adding%20a%20transform)) is allowed. In an encrypting namespace the
encrypt stage is implicitly in `require`, so a body without it is `ErrMalformed`
and no plaintext body can be planted or survive there. A namespace that must
start or stop encrypting is re-homed into a new one ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)).

**Compression and encryption together are an explicit choice.** A chain with an
encrypt stage **MUST** be refused if it also has a compress stage whose settings
do not carry `with_encryption: accept`. Encryption does not hide sizes, and a
writer who can place content of their own beside a victim's in one chunk learns
the victim's content from compressed sizes ([Appendix A](#Appendix%20A%20%E2%80%94%20compression)). The default chain
for an encrypting namespace therefore compresses nothing.

> decision: compression is off by default where the namespace encrypts, rather
> than padded. Padding to fixed sizes costs space on every chunk and still leaks
> the size class; turning compression off costs space only where data
> compresses. Specify a padding transform, in the compress stage's place, if a
> deployment needs compression and encryption together against writers it does
> not trust ([§9](#9.%20Open%20questions), item 3).

A share that needs a different chain, or different master keys, uses a different
store configuration. Within one, each namespace has its own keys
([Appendix B.2](#B.2%20Keys)), and deduplication never spans namespaces ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)), so it never
spans two data keys either: a chunk encrypted under one is readable only where
that key is held.

## 4. Writing a custom transform

A transform is compiled in and registers itself at start. It **MUST**:

1. take an ID from the range reserved for custom transforms (`0x8000`–`0xFFFF`;
   `0x0000`–`0x7FFF` is reserved for shipped ones) and never reuse it;
2. declare its stage ([§2.3](#2.3%20The%20chain%20order%20is%20fixed)), an honest `MaxEncodedLen` and `LengthOf`,
   encode deterministically, and put in `LengthKey` every setting that changes
   its output: [§8.1](#8.1%20Transform%20conformance) checks all of them;
3. give every value it derives from material its own versioned label, never
   shared with another derivation ([Appendix B.1](#B.1%20How%20a%20chunk%20is%20encrypted));
4. put a format version in its own header if its format can ever change, and
   decode every version it has written;
5. bound `Decode` by `max` before allocating;
6. return only the errors of [§2.7](#2.7%20Failures);
7. pass the transform conformance suite ([§8.1](#8.1%20Transform%20conformance)), and ship a fixture: inputs,
   settings, material and the bodies its first version wrote from them. Every
   later build **MUST** decode every fixture body, and **MUST** encode each
   fixture input to exactly its recorded body; an encoder whose output differs
   is a new format version of the transform, never a silent change.

Custom transforms are compiled in. Loading them at run time would put foreign
code in the data path with nothing to check it before it writes; revisit if an
operator needs one that is not built in.

## 5. Changing a chain over time

### 5.1 Configuration governs the next write

Adding, removing or re-configuring transforms changes only what is
written from then on, except that the encrypt stage cannot be added to or
removed from a configuration whose namespaces hold content ([§3.2](#3.2%20Configuration)). Every stored body keeps the envelope it was written with,
and [§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material) keeps it readable as long as its material is held.

### 5.2 Relocation re-encodes

GC relocation reads chunks through the codec and writes them into a new block
under the current chain ([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)). This is how old bodies migrate to a new chain:
each relocated chunk leaves its old transforms, and its old material, behind. A
relocation that copied bodies unchanged would keep every retired key in use for
as long as its chunks live.

### 5.3 Retiring material or a transform needs a census

To stop holding material, or to stop compiling a transform in, no stored body may
still need it. That takes a **census**: which blocks use which transform, version
and material.

- **It is written with the block.** The codec collects the `Encoded` descriptors
  of every body ([§3.1](#3.1%20Interfaces)). The engine's `Store` returns the distinct ones with the
  put, beside the chunk ranges ([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)), and the block's commit records
  them in the block record ([RFC 6 §2.3](rfc-6-block-metadata.md#2.3%20Block)). A block's census never changes, since
  a block is never re-encoded in place.
- **It counts every block record until it is pruned**, `retired` and `deleted`
  ones included. A retired block can be resurrected by an adoption and read
  again ([RFC 9 §3.3](rfc-9-gc.md#3.3%20Adoption%20resurrects%20a%20retired%20block)), so its material is still needed; a `deleted` one cannot,
  but is counted until its prune so the census never depends on timing. To empty
  a census, compaction moves the live blocks, and the retention of their retired
  sources is waited out or those sources are recounted and moved too.

- **Retirement is ordinary relocation.** GC lists the blocks whose census names
  what is being retired and relocates each ([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)): its chunks are
  re-encoded under the current chain into a block under a freshly minted name
  ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)); the chunks move and the move retires the source. Retirement
  relocates a block even when it is fully live, the one exception to
  [RFC 9 §4.4](rfc-9-gc.md#4.4%20When%20to%20compact%20is%20policy).
- **Removal waits for an empty census, and for encodes in flight.** A census
  says only what committed blocks use; a put already encoding under the
  material commits a block the census does not yet show. So removal, under one
  fence against new encodes, **MUST** first make sure the material is current in
  no open chain, then wait for every encode in flight that took it, and only then
  check that the census is empty. An encode that starts after the fence finds the
  material not current and cannot take it.
- **Copying backups hold material too.** A copying backup copies sealed blocks,
  never re-sealed, into a block folder at its location, and each retained
  backup's manifest keeps the census of every folder block it lists
  ([RFC 26 §2.4.2](rfc-26-catalog-backups.md#2.4.2%20What%20is%20copied)). The namespace's folder record keeps their union
  ([RFC 26 §2.4.1](rfc-26-catalog-backups.md#2.4.1%20Layout%20at%20the%20location)). So removal **MUST** also find the material in no folder
  record's census of the namespace. Retirement cannot relocate a folder block;
  a copy stops reusing one that carries retiring material, and the material
  leaves the folder as the backups that list such blocks expire.
- **Exports hold master keys and export keys.** No census lists them: a master
  key is named by the clear part of every export carrying its wrapping, and an
  export key by every export and state object it sealed. Removal of either
  **MUST** find it named by no retained export ([Appendix B.2](#B.2%20Keys)), and is
  refused with `ErrMaterialInUse` otherwise.
- **Removed material is erased, not only marked.** `Remove` returns only once
  the material, every key derived from it (salt and chunk keys, sealing keys)
  and its wrapped record are gone from every place the installation keeps them:
  the provider's memory and storage, every node's cache of unwrapped keys, and
  the namespace's key record, which keeps only the ID, the fingerprint and the
  mark `destroyed`. A node that cannot be reached is fenced first
  ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) (cluster), so no copy survives in a process that still runs. The
  provider then reports the ID `ErrMaterialDestroyed` ([§2.7](#2.7%20Failures)). Material
  merely marked destroyed while a cache still held it would stay usable until
  that cache evicted it, which is no removal at all for a key thought exposed.

### 5.4 Changing an algorithm is adding a transform

A transform ID names an algorithm and its body format, not a library. So:

- **A new algorithm is a new transform.** Moving from AES-256-GCM-SIV to another
  deterministic AEAD — AES-SIV, or one standardised later — registers a
  transform with a new ID, in the same stage, and its own material kind. The move is then [§5.1](#5.1%20Configuration%20governs%20the%20next%20write)
  and [§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census): put it in the chain, and old bodies stay readable through their
  envelopes until relocation retires the old transform.
- **A new library for the same algorithm is not a change.** Replacing the
  library that implements AES-256-GCM-SIV keeps the ID, provided it produces
  identical bodies. The transform's fixtures ([§4](#4.%20Writing%20a%20custom%20transform), item 7) decide, and a library
  that fails them is a new format version or a new transform, never a silent
  swap.
- **Post-quantum.** The chain's symmetric primitives — a 256-bit cipher key, and
  HMAC and HKDF over SHA-256 — keep a security margin against quantum search
  that is the recommended posture, and need no change. The quantum exposure is
  in how master keys reach the process: a key service's transport and key
  wrapping, which are the material provider's ([Appendix B.2](#B.2%20Keys)). A provider that
  moves to a post-quantum key encapsulation changes nothing in stored bodies.

## 6. Observability

Metric names are shown without the deployment's prefix.

Each transform is labelled by its name. A custom transform gets these metrics
without writing any: the chain exports them around each call.

| Answers | Metric | Type |
| --- | --- | --- |
| chunks encoded, labelled `applied` = `true` or `false` | `transform_chunks_total` | counter |
| bytes in and out, labelled `direction` = `encode` or `decode`; their ratio is what the transform costs or saves | `transform_bytes_total` | counter |
| time per call, by direction | `transform_seconds` | histogram |
| decode failures, labelled `error` = `malformed`, `material_unavailable`, `unknown_material`, `material_destroyed` or `too_large`. `malformed`, `unknown_material` and `material_destroyed` are alerts | `transform_decode_failures_total` | counter |
| bodies written per transform ID, version and material ID, from block metadata ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)) | `transform_census_blocks` | gauge |

A chain that fails to build logs the transform and the reason at `Error` and
refuses the store ([§2.7](#2.7%20Failures)). A transform logs nothing per chunk: its outcomes
are metrics. A transform with outcomes of its own exports them under its name, as
encryption does ([Appendix B.6](#B.6%20What%20it%20adds%20to%20observability)) and a parity transform would with a count of
repaired bodies ([Appendix C](#Appendix%20C%20%E2%80%94%20example%2C%20a%20parity%20transform)). A transform that earns nothing shows here: a
compressor whose `applied=false` share is near 100%, or a repair count that
never moves.

## 7. Invariants

| # | Invariant |
| --- | --- |
| T1 | A transform never changes a chunk's hash or a block's name, and never sees more than one chunk. |
| T2 | `Decode(Encode(p)) == p` for every input. |
| T3 | Every body lists the transforms applied to it; reading needs the registry and material, never the configured chain. The one exception is `require` ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)), which can only refuse a body, never change how one decodes. |
| T4 | No body exceeds its transform's `MaxEncodedLen`, and no step of `Decode` produces or allocates more than the bound the chain gives it. |
| T5 | A store whose chain cannot be built does not open, and never writes without its chain. |
| T6 | The plaintext hash is checked after the whole chain is undone, before any byte is returned. |
| T7 | A material failure is never zeros and never an absent chunk: unavailable or unknown material is the remote being unavailable, destroyed material makes the chunk Lost, and only material outside the census is malformed. |
| T8 | No transform error type crosses the codec. |
| T9 | Within one put attempt, `Encode` returns the same bytes on every call; within one format version, it returns each fixture's recorded body. `LengthOf`, when it answers, is exact. |
| T10 | The chain ID changes whenever a body's bytes or length could; nothing else changes it. |
| T11 | A chain and an envelope list at most one transform per stage, in stage order; anything else is refused. |
| T12 | A read asks the material provider only for material its block record's census lists, and the provider never serves material whose fingerprint no longer matches. |
| T13 | Material is destroyed only after it is current in no open chain, no encode in flight holds it, and the census is empty, checked under one fence; once destroyed, no copy of it or of any key derived from it remains anywhere the installation runs. |
| T14 | Material belongs to one namespace, and every call that names it names that namespace. |
| T15 | Whether a namespace encrypts is fixed at its creation; an encrypting namespace holds no body without the encrypt stage. |
| T16 | A chain that encrypts compresses only where its compress stage explicitly accepts the size leak. |
| T17 | Every chunk ID is a keyed hash of the chunk's plaintext under its namespace's chunk-ID key. |
| T18 | Every key is one row of [Appendix B.2](#B.2%20Keys)'s table: a non-encrypting namespace's chunk-ID key is held in the clear beside its blocks and it has no chunking key; every other namespace key is wrapped only under a master key, with AAD naming its namespace, kind and ID, never under a host-held wrapping key; and the master key of a configuration that encrypts or takes backups lives off the host. |
| T19 | No master key that a retained export names is destroyed, and no export key that a retained export or state object names. |
| T20 | Destroyed material is reported as itself, never as corruption, and a chunk it covered whose bytes the journal still holds stays unevictable until a block under current material holds it. |
| T21 | An encrypting namespace's block header holds no chunk ID unsealed. |
| T22 | An export and its state objects are sealed and authenticated as [Appendix B.1](#B.1%20How%20a%20chunk%20is%20encrypted) states, with a nonce unique per frame within the export. |

## 8. Test plan and benchmarks

The set-wide test rules and tiers are in [the RFC index](rfc-index.md#Test%20tiers).

### 8.1 Transform conformance

One suite that every registered transform passes, shipped ones and custom ones
alike. A new transform gets it by registering.

| Invariant | Check |
| --- | --- |
| T2 | Round-trip 10,000 inputs: random, all zeros, all one byte, 1 byte to the chunk maximum, and inputs that begin with this transform's own header and with any envelope. |
| T2 | Every body in the transform's fixture decodes to the recorded plaintext. |
| T4 | Fuzz `Decode`: no panic, and no allocation above `max` before the header is validated. A body declaring an output of `max + 1` fails with `ErrTooLarge`. |
| [§2.1](#2.1%20A%20transform%20acts%20on%20one%20chunk) | For every round-trip input, the body is no longer than `MaxEncodedLen(len(input))`, and an input built to hit the worst case reaches it. |
| concurrency | Encode and decode from 64 goroutines under the race detector: same results as sequential. |
| [§2.7](#2.7%20Failures) | Flip every byte of a body in turn: `Decode` returns an error of [§2.7](#2.7%20Failures) or bytes the codec's hash check rejects, and never panics. A transform that authenticates (encryption) **MUST** return `ErrMalformed` itself for every flip. |
| [§2.7](#2.7%20Failures) | For an authenticating transform: decode chunk A's body with chunk B's hash: `ErrMalformed`. A body naming destroyed material: `ErrMaterialDestroyed`. |
| T9 | Encode every round-trip input twice, and from two chains built from the same configuration: identical bodies. Where `LengthOf` answers, the body's length equals it. |
| T9 | Encode every fixture input with its recorded settings and material: exactly the recorded body. |
| [Appendix B.1](#B.1%20How%20a%20chunk%20is%20encrypted) | Two different chunks under one key get different salts; one chunk under two data keys gets two different bodies. Two different inputs under one plaintext hash (the same chunk compressed at two levels, or declined then compressed) encrypt to bodies that both decode and share no keystream: XOR of the two ciphertexts is not XOR of the two inputs. |
| [Appendix B.1](#B.1%20How%20a%20chunk%20is%20encrypted) | Every derivation label of Appendix B.1 is distinct, and each is pinned by a golden vector. |
| [Appendix B.1](#B.1%20How%20a%20chunk%20is%20encrypted) | Golden vectors, from an independent implementation working from Appendix B.1's length and field tables alone: the salt key, salt and chunk key for a fixed data key and plaintext hash; one chunk body with its AAD; the header hash and index key for a fixed header key; one fingerprint; the keyed gear table's first and last 8 bytes. Each derived key is 32 bytes, and an implementation deriving with `salt` as an empty string instead of 32 zero bytes gets the same key — RFC 5869 treats both alike — while one using a 16-byte `L` or prefixing `info` gets a different one. |
| T22 | Golden vectors, from an independent implementation, for the frame key, MAC key and state key derivations, one sealed frame, each of the four export MACs, one state MAC and one key wrap. Write an export of two sections of three frames each: all six nonces differ. Move a frame to the other section at the same frame index, or swap two sections: the read fails. A design whose nonce is the frame index alone gives two equal nonces here. |
| T18 | Unwrap a wrapped key record after changing its namespace ID, its kind or its key ID in the AAD: each fails. Copy one namespace's wrapped data key into another namespace's key records: it does not unwrap there. |
| [§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material) | Replace a key under its ID with different bytes: the provider refuses it (`ErrMaterialUnavailable`, availability gauge 0), and no body is decoded or written with it. |
| [§3.1](#3.1%20Interfaces) | A transform that enlarges its input (a test transform adding 50%) before another: a chunk of exactly the maximum round-trips. |
| [Appendix B.2](#B.2%20Keys) | In the daily tier, against a real key-management server, not a stand-in: the provider unwraps each namespace key through the server at start, the server's audit shows no master key exported, and a body round-trips. A missing key, an unreachable server and an invalid certificate each return the provider's error — the store does not open, or the decode returns `ErrMaterialUnavailable` — and none falls back to another provider or to writing without encryption. A stand-in server cannot fail the certificate case. |

### 8.2 Chain tests

| Invariant | Check |
| --- | --- |
| T11 | A chain listing transforms out of stage order, two of one stage, or one twice fails to build, and nothing is written. |
| T3 | Write under chain A, reconfigure to chain B (a transform removed, another added), read everything back. |
| T3 | A declined chunk's envelope omits the transform, and decodes. |
| T15 | With a namespace holding content, remove the encryption transform from its configuration's chain, and add one to a non-encrypting configuration: both refused, nothing written. Replace AES-256-GCM-SIV by a second encryption transform: allowed, and every older body still decodes. Plant a body without the encrypt stage in an encrypting namespace: `ErrMalformed`. |
| T16 | Build a chain of compression and encryption without `with_encryption: accept`: refused. With it: built. The default chain of an encrypting namespace has no compress stage. |
| T17 | The same file written into two namespaces yields disjoint chunk IDs; the IDs in a block header, chunk record and export match no unkeyed hash of the plaintext; a golden vector pins the keyed derivation. |
| T14 | Two namespaces on one store configuration: a read in one asking for a material ID of the other gets `ErrMalformed` from the census view, and the provider records no call; `Current` of one never returns the other's key. |
| T7 | A body whose census lists a key the provider was never given: `ErrMaterialUnavailable` with a configuration condition naming the ID, retried, never `ErrCorrupt`; load the key and the read succeeds. |
| T13 | Remove a key while two nodes hold it unwrapped in cache: after `Remove` returns, neither node can encode or decode with it, the key record keeps only ID, fingerprint and `destroyed`, and a decode naming it returns `ErrMaterialDestroyed`. |
| [§2.4](#2.4%20Every%20body%20records%20what%20was%20applied) | Envelopes with an unknown version, an unregistered ID, a duplicate ID, two IDs of one stage, or IDs out of stage order: `ErrMalformed` before any decode runs. A ranged get of one body decodes without the block header. |
| [§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material) | With `require: [aes-gcm-siv]`, a body without it is `ErrMalformed`; without `require`, it decodes. |
| T12 | A body naming a material ID the store holds but its block record's census does not list: `ErrMalformed`, and the provider records no call. |
| T13 | Start a put under key K and hold it mid-encode; retire K concurrently: removal waits for the put, then finds K in the census and refuses. Make K not current first: an encode starting after the fence uses the new key. |
| [§3.1](#3.1%20Interfaces) | Write parity-protected bodies, remove parity from the chain, restart: the read-side bound still admits them and they decode. |
| [§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census) | Write blocks under two keys and two chains: the census each put returns, and the block records it reaches, list exactly the transform IDs, versions and material IDs used. Retire one key: every block naming it is relocated to a new name, fully live ones included, and the census then shows none. |
| T10 | Build chains that differ in a transform's version, in a length-affecting setting and in current material: three different IDs. Change only a setting that is not length-affecting: the same ID. |
| [§2.8](#2.8%20The%20chain%20ID) | Golden vectors, from an independent implementation working from [§2.8](#2.8%20The%20chain%20ID)'s byte table alone: the chain ID of the empty chain, of the default encrypting chain with one data key, and of a compress-then-encrypt chain with two materials. Swap the order of two materials, change one byte of a fingerprint, or encode a length prefix as 2 bytes or little-endian: each gives a different ID than the vector. |
| T5 | Make a transform's factory fail: the store does not open, and no body is written. |
| T6 | A fake transform that decodes to wrong bytes: every read through the codec fails. |
| T7 | A decode that returns `ErrMaterialUnavailable` reaches the engine as the remote being unavailable, is retried, and never yields zeros or an absent chunk. One that returns `ErrMaterialDestroyed` is not retried and reports the chunk Lost. |
| T8 | Force each decode error: the codec returns only its own errors. |
| [§5.2](#5.2%20Relocation%20re-encodes) | Relocate a block written under an old chain: every body in the new block carries the current envelope. |
| [Appendix B.5](#B.5%20What%20a%20bucket%20reader%20still%20learns) | With encryption on, write a known file: no chunk ID appears unsealed anywhere in the stored block, and every read still verifies. With encryption off, the header lists chunk IDs, none of which equals an unkeyed hash of the file's chunks. |
| T19 | Rotate a master key while a retained export carries the old wrapping: destroying the old key is refused with `ErrMaterialInUse` naming the backup; expire the backup and the destroy succeeds. A design that checks only the key records destroys it and leaves the backup unopenable. Make one backup location unreachable: the destroy is refused naming it. Move the namespace to a second installation sharing the key service: the importer re-wraps every key record under its own master key before publish, and the exporter refuses to destroy the old key until an operator releases the move's pin. |
| [Appendix B.3](#B.3%20Rotation) | Rotate the export key: the next export is sealed under the new key, names both IDs in its clear part and carries the old key wrapped; importing it alone opens an export sealed under the old key. Destroying the old export key while a retained export names it is refused. |
| T18 | Create a non-encrypting namespace: its chunk-ID key is in its key control object in the clear, it has no chunking key, and its boundaries equal the unkeyed table's. Remove the master key and the metadata store: every chunk still verifies from the bucket alone. On a key-file provider without verified escrow, creating an encrypting namespace or a backup policy is refused with `ErrNotEscrowed`; after the escrow check passes, both succeed. A namespace key offered for wrapping under the `storage` role key is refused. |
| T20 | Destroy a data key while the journal still holds some chunks its blocks cover: a decode of a remote-only chunk returns `ErrMaterialDestroyed`, the codec's verification-failure counter does not move, the engine reports those chunks Lost; the chunks still held locally are not evicted, are offloaded again under the current key, and read back. |
| T21 | Encode a block of an encrypting namespace: no plaintext chunk ID appears in its bytes; the sealed index decodes under the header key its seal names, and ranged reads at block metadata's ranges still verify. Scan the block for the clear envelope: every body's offset and length is found, as [B.5](#B.5%20What%20a%20bucket%20reader%20still%20learns) states, so no test or text claims lengths hidden. |
| [Appendix B.2](#B.2%20Keys) | Rotate the data and header keys after the last catalog backup, relocate that backup's blocks under them, then lose the metadata store: the recovery import loads the new key records from the bucket's `keys` object and every file reads back. Fail the put to one backup location during a rotation: the new key does not become current. Rotate a key-file master key without its escrow check: refused with `ErrNotEscrowed`. |
| [Appendix B.1](#B.1%20How%20a%20chunk%20is%20encrypted) | Write a backup's state as `writing`, `complete`, then `damaged`; re-put the authentic `complete` version on top: the reader takes `damaged`, by sequence. Two field lists that join to the same bytes without length prefixes give different MACs. The golden vectors cover the sealed header and the progress MAC. |
| [Appendix B.2](#B.2%20Keys) | Hand the put intent step a plan whose chunk IDs were computed under another namespace's chunk-ID key: refused, and nothing is put. |
| [Appendix B.3](#B.3%20Rotation) | Rotate the header key: new blocks carry the new key's ID in their seal; old blocks still verify under the key their seal names; after relocation retires the old key by census, no block names it and its removal succeeds. |

### 8.3 Benchmarks and targets

Run after merge ([the RFC index](rfc-index.md#Test%20tiers)), over three
corpora: random bytes, a text and source tree, and a mixed VM image. Record the
corpus, chunk size distribution and CPU with each result.

| Benchmark | Measures | Target |
| --- | --- | --- |
| Each transform, encode and decode | MB/s per core | report, per transform, against the previous run |
| The chain around its transforms | overhead | within 2% of the sum of its transforms' own times, median of 10 runs |
| Envelope size | bytes per chunk | 2 + 2n |
| `Chain.MaxEncodedLen` against the largest body seen | ratio | at most 1: the declared bound is never exceeded on any corpus |
| Compression ratio per corpus | bytes out / in | report; tracked against the previous run |
| A put and a whole-block get with the default chain | MB/s | within 10% of the same without a chain, on the reference link, or the chain is what the link waits on and pool sizing must say so ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)) |

## 9. Open questions

1. **Hiding chunk hashes from the service** — closed. When encryption is on,
   header hashes are sealed under the namespace's header key ([Appendix B.5](#B.5%20What%20a%20bucket%20reader%20still%20learns)).

2. **External key services that never release a key** — closed. A provider
   backed by a key service unwraps each namespace key through it, once per key
   when the namespace opens, and keeps only the unwrapped namespace keys in
   memory ([Appendix B.2](#B.2%20Keys)). Data keys are per namespace, not per chunk, so the
   cost is one round trip per namespace key, not per chunk, and the master key
   never leaves the service.
3. **Padding.** A transform that pads bodies to fixed sizes would hide compressed
   sizes ([Appendix A](#Appendix%20A%20%E2%80%94%20compression)) at a cost in space. Compression is off by default where a
   namespace encrypts ([§3.2](#3.2%20Configuration)), so none is specified until a deployment needs
   both against writers it does not trust.

---

## Appendix A — compression

A shipped transform, and an example of one that makes bodies smaller.

**What it does.** Compresses a chunk with zstd (ID `0x0001`) or LZ4 (ID `0x0002`).
The algorithm is the transform, so the envelope records it; the level is not
recorded, because decoding does not need it.

**It declines what does not shrink.** If the output, header included, would save
less than 1/16 of the input, `Encode` returns `applied=false` and the chunk passes on unchanged.
Already-compressed data (media, archives, encrypted files) then costs one attempt
and no stored overhead.

**Its header.** A format version (1 byte) and the decompressed length (varint).
`Decode` refuses a declared length above `max` before allocating, and runs the
decoder with its window capped at the chunk maximum, so a body cannot make it
allocate more than one chunk however it was crafted.

**Bound.** `MaxEncodedLen(n) = n`: a chunk that would not shrink, header included, is declined.

**Length.** Not known without compressing: `LengthOf` returns false, so a chain
with compression takes a measuring pass on every put ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)), and
compresses each chunk twice.

> decision: a chain with compression encodes every block twice, once to measure
> and once to send, so that no encoded block is held in memory. The default
> chains — none, or encryption alone — declare their lengths and encode once
> ([§3.2](#3.2%20Configuration)). Keep the measuring pass's bodies for the send, within the syncer's
> memory bound, when the second compression shows in a put profile.

**Settings.** `level` (default 3 for zstd). It changes the output, so it is in the
chain ID ([§2.8](#2.8%20The%20chain%20ID)). Changing it affects the next write only.

**Leaks.** Compression makes a body's size depend on its content, and
encryption does not hide sizes. On an encrypted share, a chunk's stored size
says how compressible it was, and anyone who can place data of their own next
to a secret inside one chunk, then watch the stored size, can recover the secret
a guess at a time — the attack family known from compressed, encrypted web
traffic. Placing data needs no write access to the share: mail delivered into a
profile container, or a document a server fetches, lands in the victim's chunks.
So a chain that encrypts compresses only where its compress stage carries
`with_encryption: accept` ([§3.2](#3.2%20Configuration)), and encryption **MUST NOT** be described
as hiding content from a bucket reader on such a chain.

## Appendix B — encryption

A shipped transform (ID `0x0010`, stage encrypt), and an example of one that
needs material: a namespace's data keys, of kind `data-key`.

### B.1 How a chunk is encrypted

Keys are layered ([Appendix B.2](#B.2%20Keys)): master keys, held by the provider, wrap one or
more **data keys** per namespace. A chunk is sealed under a key derived from the
namespace's current data key and a salt derived from the chunk's hash, with
AES-256-GCM-SIV (IETF RFC 8452), a deterministic, nonce-misuse-resistant AEAD:

```
salt key  = HKDF-SHA256(data key, salt = none, info = "dittofs chunk salt v2"), 32 bytes
salt      = HMAC-SHA256(salt key, plaintext hash), 32 bytes
chunk key = HKDF-SHA256(data key, salt = salt, info = "dittofs chunk key v2"), 32 bytes
body      = AES-256-GCM-SIV(chunk key, nonce = 12 zero bytes, input,
                            AAD = transform ID ‖ version ‖ data key ID ‖ plaintext hash)
```

`input` is what reaches the encrypt stage: the compressed chunk, or the chunk
itself when compression declined or is off.

![Encrypting one chunk: the master key unwraps per-namespace data, header and chunking keys; the data key seals the chunk with AES-256-GCM-SIV, the header key seals the chunk hashes in block headers, the chunking key keys boundaries](img/rfc5t-key-derivation.svg)

**Every derivation has its own versioned label.** Nothing derived from a key is
derived under a label another derivation uses:

| Label | Derived from | Used for |
| --- | --- | --- |
| `dittofs chunk salt v2` | data key | the salt key |
| `dittofs chunk key v2` | data key and salt | the chunk key |
| `dittofs header hash v1` | header key | sealing chunk hashes in block headers ([Appendix B.5](#B.5%20What%20a%20bucket%20reader%20still%20learns)) |
| `dittofs header index v1` | header key | the key sealing a block's chunk offsets and lengths ([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)) |
| `dittofs chunking key v1` | chunking key | keyed boundaries, encrypting namespaces only ([RFC 2 §6](rfc-2-carver.md#6.%20Boundaries%20are%20public)) |
| `dittofs export frame v1` | export key and the export's salt | the key sealing each frame of an export ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)) |
| `dittofs export mac v1` | export key and the export's salt | the key authenticating an export's clear part, header, sections and trailer |
| `dittofs export state v1` | export key and the backup's ID | the key authenticating each version of a backup's state object ([RFC 4 §4.15](rfc-4-remote-tier.md#4.15%20Versioned%20objects%20at%20a%20backup%20location)) |
| `dittofs export progress v1` | export key and the backup's ID | the key authenticating each version of a running copy's progress objects ([RFC 4 §4.15](rfc-4-remote-tier.md#4.15%20Versioned%20objects%20at%20a%20backup%20location)) |
| `dittofs key check v1` | any material | its fingerprint ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)) |

**Export cryptography.** An export ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)) is sealed with the same AEAD
as a chunk, and every value below has a golden vector ([§8.1](#8.1%20Transform%20conformance)):

```
export salt = 32 bytes from a cryptographic random source, per export, in its clear part
frame key   = HKDF-SHA256(export key, salt = export salt, info = "dittofs export frame v1"), 32 bytes
mac key     = HKDF-SHA256(export key, salt = export salt, info = "dittofs export mac v1"), 32 bytes
frame       = AES-256-GCM-SIV(frame key,
                nonce = section index (4 bytes, big-endian) ‖ frame index (8 bytes, big-endian),
                records,
                AAD = export salt ‖ section index ‖ section type ‖ frame index)
clear MAC   = HMAC-SHA256(mac key, "clear" ‖ every byte of the clear part before it)
sealed header = AES-256-GCM-SIV(frame key,
                nonce = 0xFFFFFFFF (4 bytes) ‖ 0 (8 bytes),
                header, AAD = export salt ‖ "header")
header MAC  = HMAC-SHA256(mac key, "header" ‖ clear MAC ‖ sealed header)
section MAC = HMAC-SHA256(mac key, "section" ‖ section index ‖ section type ‖ record count ‖ each frame's tag)
trailer MAC = HMAC-SHA256(mac key, "trailer" ‖ clear MAC ‖ header MAC ‖ every section MAC, in order)
state key   = HKDF-SHA256(export key, salt = backup ID, info = "dittofs export state v1"), 32 bytes
state MAC   = HMAC-SHA256(state key, backup ID ‖ state ‖ sequence ‖ export digest ‖ time)
progress key = HKDF-SHA256(export key, salt = backup ID, info = "dittofs export progress v1"), 32 bytes
progress MAC = HMAC-SHA256(progress key, backup ID ‖ batch ‖ sequence ‖ SHA-256 of the progress body)
```

**Every joined field is length-prefixed.** In every AAD and MAC input of this
appendix, every field that is not an integer — a label, a backup ID, a state
name, a key ID, a namespace ID, a hash or digest, a salt, a sealed header, the
clear part's bytes — is preceded by its length as 4 bytes, big-endian, even
where that length is fixed; an integer is big-endian at the
width stated (section index 4 bytes; frame index, batch, sequence and record
count 8 bytes; a time as 8 bytes of Unix nanoseconds). Without the prefix, two
different field lists can join to one byte string and share one tag. The golden
vectors check the encoding this text defines; they do not define it.

**Lengths and encodings, exactly.** Every key and every derived value of this
appendix is fixed here, so two implementations agree before either runs a
vector:

| Value | Bytes | How |
| --- | --- | --- |
| master key, data key, header key, chunking key, chunk-ID key, export key | 32 | drawn from a cryptographic random source when created or rotated; never derived |
| every HKDF-SHA256 output: salt key, chunk key, index key, frame key, mac key, state key, progress key | 32 | IETF RFC 5869 with `L = 32`; `salt = none` means RFC 5869's default, 32 zero bytes; `info` is the label's ASCII bytes alone, with no length prefix and no terminator |
| salt, header hash, every MAC, fingerprint | 32 | HMAC-SHA256, full output, never truncated |
| plaintext hash, chunk ID | 32 | the keyed BLAKE3 chunk ID ([RFC 2 §4.1](rfc-2-carver.md#4.1%20A%20chunk)) |
| keyed gear table | 2,048 | BLAKE3 keyed mode under the chunking key, over the ASCII label `dittofs chunking key v1` ([RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it)) |
| AES-256-GCM-SIV nonce | 12 | the chunk's is 12 zero bytes; a frame's and the header's as below; a key wrap's random |
| AES-256-GCM-SIV tag | 16 | appended to the ciphertext |

A value computed over **one** field — the salt over the plaintext hash, a
fingerprint over its label — takes that field's bytes alone, with no prefix;
the prefix rule above applies wherever two or more fields are joined. The
joined inputs are, field by field:

| Input | Fields, in order |
| --- | --- |
| chunk AAD | transform ID (2 bytes, big-endian) ‖ version (1 byte) ‖ data key ID (4-byte length, bytes) ‖ plaintext hash (4-byte length, 32 bytes) |
| header hash ([Appendix B.5](#B.5%20What%20a%20bucket%20reader%20still%20learns)) | label (4-byte length, ASCII) ‖ chunk ID (4-byte length, 32 bytes) |
| frame AAD | export salt (4-byte length, 32 bytes) ‖ section index (4 bytes) ‖ section type (1 byte) ‖ frame index (8 bytes) |
| header AAD | export salt (4-byte length, 32 bytes) ‖ `header` (4-byte length, ASCII) |
| each MAC | its literal label (`clear`, `header`, `section`, `trailer`; 4-byte length, ASCII) first, then its fields as listed, each integer at its stated width and each other field length-prefixed |
| key wrap AAD | namespace ID ‖ kind (ASCII) ‖ key ID, each with a 4-byte length |

Integers are big-endian at the width stated.

**The header is sealed like a frame.** Section index 2³² − 1 is reserved for the
header and names no section, so the header's nonce never meets a frame's.

**State and progress versions are ordered by their authenticated sequence.** A
writer raises `sequence` by one at every write of a backup's state object, and
at every write of one batch's progress object. A reader takes, of every version
that authenticates, the one with the **highest sequence** — never the newest by
the store's order or time, which anyone holding the location's put credential
can change by re-putting an older authentic version
([RFC 4 §4.15](rfc-4-remote-tier.md#4.15%20Versioned%20objects%20at%20a%20backup%20location)). A state's transitions are monotone — `writing`, then
`complete`, then `damaged` or `expired` — and a write that would go back is
refused, so a replayed `complete` cannot hide a later `damaged`, nor a replayed
`writing` unmake a `complete`.

The nonce is unique per frame within one export, since a section index and a
frame index never repeat together, and every export draws its own salt, so its
frame key is its own; GCM-SIV is chosen anyway, so a repeat would leak only that
two frames were equal, never the key stream or the authentication key. A frame
index that restarted per section under one key and one nonce field would repeat
across sections, which under a nonce-based AEAD reveals the key stream and lets a
reader forge frames. Every export's clear part names the ID of the export key
that seals it and the IDs of every master key whose wrapping it carries
([Appendix B.2](#B.2%20Keys)).

**Key wrap.** A namespace key is stored only wrapped under a master key:

```
wrapped = AES-256-GCM-SIV(master key, nonce = 12 bytes from a cryptographic random source,
                          key bytes, AAD = namespace ID ‖ kind ‖ key ID)
```

stored as master-key ID ‖ nonce ‖ ciphertext ‖ tag. A key-service provider sends
the same AAD as the service's additional authenticated data, and the service
does the wrap. The AAD binds a wrapped key to its namespace, kind and ID, so a
wrapped record copied into another namespace's key records, or relabelled as
another kind, fails to unwrap instead of serving as that key.

A change to any derivation is a new label and a new transform format version; a
golden vector pins each. The chunk-ID key is the one key used with no derivation:
it is the key of BLAKE3's keyed mode, which computes every chunk ID
([RFC 2 §4.1](rfc-2-carver.md#4.1%20A%20chunk)), and it serves nothing else.

In plain terms:

- **one chunk hash does not mean one input.** The chunk key depends only on the
  data key and the plaintext hash, but what it encrypts is the compression
  stage's output, which can differ for the same chunk: a new compression level,
  a chunk declined once and compressed later, a compression library upgrade, or
  a relocation, which re-encodes by design ([§5.2](#5.2%20Relocation%20re-encodes)). Two different inputs under
  one key and one fixed nonce are exactly what a nonce-based AEAD must never see.
  AES-GCM-SIV derives its internal IV from the key, the nonce and the exact bytes
  encrypted, so two different inputs get two unrelated keystreams, and the most
  a repeat leaks is that two inputs were identical.
- **deterministic, as [§2.1](#2.1%20A%20transform%20acts%20on%20one%20chunk) requires.** The same input under one data key
  encrypts identically. The salt is an HMAC under a key only the data key
  derives, so no one without it can compute the salt.
- **everything is authenticated.** The header fields and the plaintext hash are
  all in the AAD, so changing any byte of the body, or presenting it as a
  different chunk, fails decryption.
- **master keys never leave the key provider.** A provider backed by a key
  service never fetches them: it asks the service to unwrap each namespace key
  and holds only the unwrapped namespace keys in memory. The key-file provider
  holds its master keys in its own process memory. A namespace key is stored
  only wrapped. Bodies store the data key's ID; the salt is not
  stored: `Decode` derives it again from the hash it is given.

**Its header:** version (1 byte), data key ID length (1 byte), data key ID. Then
the ciphertext and its 16-byte tag. `MaxEncodedLen(n) = n + 273`, the worst case
with a 255-byte key ID: 1 + 1 + 255 + 16. `LengthOf(n)` is exact: `n + 18` plus
the current data key ID's length. The data key's (ID, fingerprint) is in the
chain ID ([§2.8](#2.8%20The%20chain%20ID)).

### B.2 Keys

Keys are layered, as envelope encryption. **This table is the one authoritative
statement of which keys exist, what holds each, what rotates and what must live
off the host.** Every other RFC that names a key cites it and restates none of
it: [RFC 0](rfc-0-data-lifecycle.md), [RFC 2 §4.1](rfc-2-carver.md#4.1%20A%20chunk) and [§6](rfc-2-carver.md#6.%20Boundaries%20are%20public),
[RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material) and [RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout), and [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets).

| Kind | Exists in | Held as | An export carries it | Rotates | Must live off the host | Lost for good means |
| --- | --- | --- | --- | --- | --- | --- |
| `master-key` | a store configuration, one or more | in a key service, which never releases it; or in the key-file provider's memory, read from a key file | its ID only | yes, by re-wrapping; destroyed only once no key record and no retained export names it | **yes**, wherever any namespace of the configuration encrypts or takes catalog backups: a key service, or a key file whose off-host escrow was verified at setup (below) | every key it wraps is lost, and with it the rows below |
| `data-key` | an encrypting namespace, one current at a time | wrapped under a master key | wrapped | yes, by relocation ([B.3](#B.3%20Rotation)) | through its master key | every chunk it sealed is **Lost** |
| `header-key` | an encrypting namespace, one current at a time | wrapped under a master key | wrapped | yes, by relocation ([B.3](#B.3%20Rotation)) | through its master key | every block whose seal names it is unverifiable as a block; its chunks still read through their recorded ranges |
| `chunking-key` | an encrypting namespace, for its life | wrapped under a master key | wrapped | never: changing it re-cuts every file | through its master key | the namespace can no longer cut new content as it cut the old; it is re-homed ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| `chunk-id-key`, encrypting namespace | the namespace, for its life | wrapped under a master key | wrapped | never: changing it renames every chunk | through its master key | no chunk of the namespace can be checked against its ID: every remote-only chunk is **Lost** |
| `chunk-id-key`, non-encrypting namespace | the namespace, for its life | **in the clear**: in its key record, and in the namespace's key control object beside its blocks ([RFC 4 §4.13](rfc-4-remote-tier.md#4.13%20Control%20objects)) | in the clear | never | no: the bucket holds it | cannot be lost while the bucket survives |
| `export-key` | every namespace, one current at a time | wrapped under a master key | the current and every retired one not yet destroyed, wrapped | yes, by issuing a new current key; a retired one is destroyed once no retained export or state object names it | through its master key | every export and state object it sealed is unreadable: those backups are lost |

**Every namespace has a chunk-ID key and an export key from its creation**; an
encrypting one also gets its first data key, header key and chunking key then. A
chunk ID is a keyed hash from the first write, so a namespace never migrates its
identities when encryption or deduplication arrive later
([RFC 2 §4.1](rfc-2-carver.md#4.1%20A%20chunk)). A non-encrypting namespace has no chunking key: its
boundaries use the unkeyed table ([RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it)), and nothing derives a gear
table from a key there.

> decision: a non-encrypting namespace keeps its chunk-ID key in the clear beside
> its blocks. Its bodies are plaintext, so a keyed ID there hides nothing from a
> bucket reader, and a wrapped one would only add a way to lose the data: with
> the master key on the same disk as the embedded metadata store, losing that
> disk would leave every chunk unverifiable. Keyed IDs there are kept so that the
> namespace's identities never change, and so that an index built from them
> later holds no unkeyed hash. Wrap it only if non-encrypting namespaces are ever
> required to hide content from a bucket reader, which needs encryption anyway.

**What a keyed ID claims, and to whom.** A keyed chunk ID, a sealed header ID and
keyed boundaries stop a bucket reader confirming that a namespace holds a known
file only **in an encrypting namespace, against a reader who can cause no write
into it**. In a non-encrypting namespace the bodies and the chunk-ID key are both
readable, and nothing is hidden. In an encrypting one, a reader who can also
cause a write — a co-tenant share of the namespace, a sender whose mail lands in
a profile container — writes a candidate and compares: chunk IDs and sealed IDs
are deterministic per namespace, so a match confirms the content
([B.5](#B.5%20What%20a%20bucket%20reader%20still%20learns)). No text in this set
**MUST** claim more.

**Where the master key lives.** A master key on the host it protects protects
nothing against losing that host. So wherever a store configuration has a
namespace that encrypts or takes catalog backups, its master key **MUST** live
off the host:

- in a key service, which never releases it ([B.1](#B.1%20How%20a%20chunk%20is%20encrypted)); or
- in a key file whose **off-host escrow was verified** as a required setup step:
  the operator stores a copy off the host and hands it back once, and the
  provider checks that the copy unwraps a test record and matches the
  fingerprint. Until that check has passed, creating an encrypting namespace, or
  a backup policy for any namespace of the configuration, is refused
  (`ErrNotEscrowed`), naming the key.

The check is per key, not per provider: a key-file master key becomes
`current`, at creation or by rotation ([B.3](#B.3%20Rotation)), only once its own escrow check
has passed, and is refused with `ErrNotEscrowed` otherwise. A rotated key with
no escrow would leave every key record re-wrapped under it held only on the host.

**No storage-key fallback.** A namespace key is wrapped only under a master key
of its store configuration. The `storage` role's wrapping key
([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)), which seals remote-tier credentials in the metadata store,
**MUST NOT** wrap a namespace key: it lives on the host, so an export wrapped
under it would restore only on the host that made it, which is the one place a
backup is not needed.

**Where wrapped keys live.** Each namespace key is a **key record** in the
metadata store, under the namespace's prefix: its kind, ID, fingerprint, the ID
of the master key that wraps it, the wrapped bytes (the key itself for a
non-encrypting namespace's chunk-ID key), and its state — `current`, `retired` or
`destroyed` ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)). A key record is not a secret record: the wrapped
bytes are useless without the master key, and the master key is never in the
metadata store ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)). Every export carries the namespace's key records as the
table says ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)), so an installation that holds the master key can
restore a namespace whose metadata store is gone.

**A key record reaches the bucket before it is used.** Every new or rewritten
key record — at the namespace's creation, at every rotation and every re-wrap —
is written, as the metadata store holds it, to the namespace's `keys` control
object ([RFC 4 §4.13](rfc-4-remote-tier.md#4.13%20Control%20objects)) and to every backup location a policy of the namespace
writes to ([RFC 4 §4.15](rfc-4-remote-tier.md#4.15%20Versioned%20objects%20at%20a%20backup%20location)), and becomes `current` only once every one of those
puts has succeeded; a put that fails leaves the old key current and is retried.
A recovery import loads key records from there as well as from the export
([RFC 26 §2.3](rfc-26-catalog-backups.md#2.3%20Restore)), taking the union of every version it finds, each checked by its
unwrap and its fingerprint. Without this, a key rotated after the last backup —
under which relocation may since have re-encoded that backup's blocks — would
be held only by the metadata store that was lost, and the whole namespace with
it, not only the day's writes.

**A retained export pins its master keys.** Each export's clear part lists the
IDs of every master key whose wrapping it carries, and so does its backup's
state object ([RFC 26 §2.1](rfc-26-catalog-backups.md#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)). Destroying a master key that a retained export
names **MUST** be refused (`ErrMaterialInUse`), naming the backups. The check
reads the state objects at every backup location a policy of the installation
names, and the catalog backups beside the namespaces' blocks; a location that
cannot be read **fails the check closed**: the destroy is refused, naming the
location, since a backup there may name the key. Re-wrapping the key records in
the metadata store does not rewrap an export already written, so a destroy that
checked only the key records would leave every earlier backup unopenable,
immutable ones included.

**The check is per installation, and a move pins as well.** An installation that
imports a namespace re-wraps every imported key record under its own master key
before it publishes ([RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material)), so from then on the two installations
share no master key in use. The exporting installation keeps a durable pin per
move naming every master key the moved export carried, and refuses to destroy
one (`ErrMaterialInUse`, naming the move) until an operator releases the pin.
Without it, an installation that rotated away from a shared key and saw its own
backups expire would find nothing naming the key, and destroy every key record
and export of the namespace it moved away.

**Keys belong to one namespace, and so do the IDs computed under them.** A chunk
ID means something only in the namespace whose chunk-ID key computed it. A block
plan, and the put intent recorded for it, name the namespace and the chunk-ID key
ID its chunk IDs were computed under, and the intent step **MUST** refuse a plan
keyed under another namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)). A re-home captures the source
namespace and its key IDs with each offer it reads, so a plan formed before the
switch cannot commit IDs of the old namespace into the new one
([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)).

Encryption asks the store configuration's `Materials` for the namespace's
current data key when it encodes and for the data key a body names when it
decodes; the provider unwraps it with the master key that wrapped it. Every piece
of material carries its fingerprint ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)), checked after unwrapping.

The key records keep every key ID the namespace has ever issued, marking one
destroyed once it is retired ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)) or declared lost by the operator, so the
provider can tell a key it never had, one it cannot reach now, and one that is
gone for good. Two providers ship: a key file, and a key service that unwraps on
request and never releases a master key. Encryption **MUST** fail its
construction if it cannot get the current data key ([§2.7](#2.7%20Failures)).

### B.3 Rotation

There are four rotations, and they cost different things:

- **Rotating a master key re-wraps.** A new master key becomes current; every
  namespace key wrapped under the old one is unwrapped and wrapped again under
  the new one, rewriting its key record, and every export written from then on
  names the new one. No stored body changes and no block is relocated. The old
  master key can be destroyed only once no key record names it, no retained
  export does and no move pins it ([B.2](#B.2%20Keys)): it stays held, retired, until the
  last backup that carries its wrapping expires. The new key becomes current only
  after its escrow check, and each re-wrapped record only once it has reached the
  bucket and the backup locations ([B.2](#B.2%20Keys)). This is the rotation a
  compliance schedule usually asks for.
- **Rotating a data key re-encrypts.** A new data key becomes current for its
  namespace: it is new material with a new ID, so the chain ID changes and new
  chunks use it. Existing chunks name their data key ID and stay readable as long
  as the provider holds that key. To stop holding the old one, relocate the
  blocks whose census names it ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)); this is the rotation for a data key
  thought exposed.
- **Rotating a header key re-seals.** A new header key becomes current; new
  blocks name it in their seal ([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)) and in their census, and old blocks
  stay checkable under the key their seal names. Relocation re-seals them under
  the current key and mints new names, so the old key retires by census like a
  data key. Configuration **MUST** accept it like a data-key rotation.
- **Rotating an export key re-keys the next export.** A new export key becomes
  current and seals every export and state-object version written from then on;
  the old one is `retired`, still held, and carried, wrapped, in every later
  export, so the newest export alone opens the older ones. A retired export key
  is destroyed once no retained export or state object names it. Rotation
  narrows a leaked export key's reach only for reading: it cannot read an export
  sealed after the rotation. Until it is destroyed it is still accepted, so its
  holder can still forge an export or a state object under it, and nothing ties
  an export to the time its key was current. It **SHOULD** rotate with the master
  key, and **MUST** be rotatable on request when thought exposed.

The chunk-ID key and the chunking key never rotate. Changing the chunk-ID key
renames every chunk, and changing the chunking key re-cuts every file: either is
a re-home into a new namespace ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), not a rotation.

### B.4 Losing a key loses the data

Every chunk encrypted under a data key is unreadable without it, every chunk of an
encrypting namespace is unverifiable without its chunk-ID key, and every
namespace key is unreadable without the master key that wraps it. There is no
recovery path, by design: a recovery path is a second key. That is why the
master key of an encrypting or backed-up namespace must live off the host
([B.2](#B.2%20Keys)): it is the one dependency the system enforces rather than
only reports. **No backup survives losing the master key that wraps its export
key** ([RFC 26 §2](rfc-26-catalog-backups.md#2.%20Catalog%20backups)).

| What happens | Result |
| --- | --- |
| The provider is unreachable at start | the store does not open ([§2.7](#2.7%20Failures)) |
| The provider becomes unreachable, or a key is missing, when reading | the read fails as the remote being unavailable, and recovers when the key returns. Never zeros |
| A tag fails | a verification failure of the chunk |
| A key's material no longer matches its fingerprint ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)) | refused, and handled as a missing key |
| A master or data key is lost for good, and the provider records it destroyed ([Appendix B.2](#B.2%20Keys)) | `ErrMaterialDestroyed`, never `ErrCorrupt`: every remote-only chunk it covered is **Lost** ([RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function)) |
| An encrypting namespace's chunk-ID key is lost for good | no chunk of the namespace can be checked against its ID, so every remote-only chunk is **Lost**, and the namespace takes no new write |
| A non-encrypting namespace's chunk-ID key | cannot be lost while its bucket survives: it is in the clear beside the blocks ([B.2](#B.2%20Keys)) |
| An export key is lost for good | every export and state object it sealed is unreadable; the data is untouched, and the next backup is sealed under a new export key |
| The provider was never given a key the census lists | `ErrUnknownMaterial`: the read fails as the remote being unavailable, with a configuration condition naming the key, until it is loaded |

**A chunk still held locally is not lost with its key.** When material is
destroyed or declared lost, a chunk whose remote body depends on it but whose
bytes the journal still holds is no longer offloaded: its remote copy cannot be
read, so the local one is the only copy. The engine **MUST** treat it as dirty —
not evictable, and offered again under current material — until a new block
holding it commits ([RFC 8 §10.4](rfc-8-engine.md#10.4%20Nothing%20but%20dirty%20content%20makes%20an%20extent%20unevictable)). Only a chunk with no local copy is Lost.
Without this, destroying a key would also let eviction drop the one readable copy
of every chunk it covered.

Each of these is a health condition of the store, not only a log line.

### B.5 What a bucket reader still learns

Encryption keeps chunk contents secret from anyone who can read the bucket but
not the key. It does not hide everything, and what it hides depends on whether
the reader can also cause a write into the namespace.

| Who | Can | What encryption protects |
| --- | --- | --- |
| Someone who can read the bucket (a leaked credential, the provider's staff) and can cause no write into the namespace | read every stored byte | the content of every chunk whose content they cannot already guess, and which known files the namespace holds |
| Someone who can read the bucket and can cause a write into the namespace (a co-tenant share, a sender whose mail lands in a profile container) | write a candidate, then compare what is stored | the content of chunks they cannot guess; **not** whether the namespace holds a candidate: the same chunk under one namespace's keys gives the same chunk ID, sealed ID and body, so a match confirms it |
| Someone who can also write the bucket | alter, delete or replace blocks | nothing is silently corrupted: any change fails the tag or the plaintext hash. Deletion is still loss |
| Someone on the host running the system | read memory and keys | nothing; they hold the keys |

**Chunk IDs are keyed, and header IDs are sealed when encryption is on.** Each block's header lists its chunks' IDs and where each body sits
([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)). An unkeyed hash would let anyone holding a file chunk it the same
way, hash the chunks and look for them: a match proves the file is stored, and
the same works for a chunk with few possible contents, such as a form with one
field. A chunk ID is keyed under the namespace's chunk-ID key ([Appendix B.2](#B.2%20Keys)).
When a namespace encrypts, the header **MUST** also list
`HMAC-SHA256(header key, "dittofs header hash v1" ‖ chunk ID)` for each chunk,
under the header key the block's seal names, instead of the chunk ID, and
**MUST** seal each body's offset and length:

```
index key    = HKDF-SHA256(header key, info = "dittofs header index v1"), 32 bytes
sealed index = AES-256-GCM-SIV(index key, nonce = the block's nonce, first 12 bytes,
                               every (offset, length) in block order, AAD = block name)
```

Block metadata keys chunks by chunk ID and records each chunk's range
([RFC 6 §2.2](rfc-6-block-metadata.md#2.2%20Chunk)), so a read never needs the header's index; only the stored
header is sealed. A reader that checks the header seals the ID it expects under
the block's own header key and compares — never under the namespace's current
key, which may have rotated since. With boundaries also keyed
([RFC 2 §6](rfc-2-carver.md#6.%20Boundaries%20are%20public)), a bucket reader who can cause no write cannot confirm a known
file by its chunk IDs.

**Chunk lengths are visible to a bucket reader.** Every body starts with the
same clear envelope ([§2.4](#2.4%20Every%20body%20records%20what%20was%20applied)) and the same encryption header carrying the data
key's ID, back to back, so one get of a block and a scan for that marker yields
every body's offset and length. Sealing the index hides nothing the bodies do
not already show, and this RFC claims no more for it.

> ponytail: the sealed index stays only because the header format already has
> it. Drop it with the sealed IDs at the next block format version; mask the
> envelope and header with a pad derived from the index key then, if hiding
> lengths is ever required.

> ponytail: with chunk IDs keyed, sealing them again in an encrypting
> namespace's headers protects only against a holder of the chunk-ID key who
> lacks the header key. It stays because block names derive from the sealed
> form ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)). Drop the seal at the next block format version, when a
> format change is made anyway.

> decision: a reader who can cause a write into an encrypting namespace still
> confirms content by comparison, because chunk IDs, sealed IDs and bodies are
> deterministic per namespace. Breaking that would take a per-block salt in the
> chunk key, which every ranged read would then need from somewhere other than
> the body. Shares that must not learn about each other's content belong in
> separate namespaces ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)), where nothing is comparable. Add the salt if
> a deployment must put mutually untrusting writers in one namespace.

What a bucket reader can still see:

- **sizes**: of blocks, and of every chunk in them, found by scanning a block
  for the clear envelope that starts each body. With
  compression, a chunk's size says how compressible it was
  ([Appendix A](#Appendix%20A%20%E2%80%94%20compression)), which is why an encrypting chain compresses only by
  explicit choice ([§3.2](#3.2%20Configuration));
- **repetition**: which content is written more than once;
- **timing**: what is written and read, and when;
- with encryption off, **everything**: bodies are plaintext, every chunk
  length is in the header, and the chunk-ID key is in the clear beside the
  blocks ([Appendix B.2](#B.2%20Keys)), so keyed IDs there confirm nothing and hide nothing.

### B.6 What it adds to observability

| Answers | Metric | Type |
| --- | --- | --- |
| chunks encrypted, labelled by data key ID | `encryption_chunks_total` | counter |
| the data key ID new chunks use, per namespace | `encryption_current_key` | gauge (1 on the current key's label) |
| whether the provider can currently return each configured key with a matching fingerprint | `encryption_key_available` | gauge (0/1) |

## Appendix C — example, a parity transform

Not shipped. An example of a transform that makes bodies **larger**, written the
way a custom transform would be ([§4](#4.%20Writing%20a%20custom%20transform)).

**What it does.** Splits the body into `k` data fragments, adds `m` Reed-Solomon
parity fragments, and stores all of them in the one body. `Decode` rebuilds the
body from any `k` intact fragments, so it can repair damage to up to `m` fragments
without fetching anything else. A per-fragment CRC32C tells it which fragments are
damaged.

**Its header.** Version, `k`, `m`, the original length, and one CRC per fragment.

**Bound.** `MaxEncodedLen(n) = ceil(n / k) × (k + m) + header`: with `k = 4`,
`m = 2`, a 16 MiB chunk encodes to about 24 MiB. The chain's bound grows with it,
and so do the block size and the syncer's memory bound ([§3.1](#3.1%20Interfaces)). `LengthOf`
is exact, so parity needs no measuring pass.

**Stage.** Redundancy, last in every chain ([§2.3](#2.3%20The%20chain%20order%20is%20fixed)), so it protects the bytes
actually stored. Placed before encryption, a flipped ciphertext bit would fail
decryption before parity could repair it, which is why no other place is
allowed.

**What it cannot protect.** The envelope and the block header, which are outside
every transform ([§2.4](#2.4%20Every%20body%20records%20what%20was%20applied)). A repaired body is still checked against the
plaintext hash ([§2.6](#2.6%20The%20plaintext%20hash%20is%20the%20final%20check)).

**Observability.** `transform_parity_repaired_total`, bodies repaired
on decode. A value that never moves on a store means the parity costs space for
nothing.

**Whether to use it.** Only for a store with no redundancy of its own, such as a
single disk. Object stores already store data redundantly, and corruption in
transit is caught by the put check ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)); there it costs `m / k` more
storage and bandwidth on every chunk for nothing.

## Appendix D — where the current code differs

Descriptive, for the refactor.

| # | This document says | The code today |
| --- | --- | --- |
| D1 | The envelope records what was applied ([§2.4](#2.4%20Every%20body%20records%20what%20was%20applied)) | each layer marks its own output; compression stores declined chunks unmarked, so an incompressible chunk that begins with the compression marker is read back as a frame and is unreadable. Data loss |
| D2 | Decoding is bounded by the chunk maximum (T4) | the declared-size ceiling is 64 MiB against a 16 MiB chunk maximum, the buffer is allocated before decoding, and the zstd window is unbounded |
| D3 | The stage order is fixed, and a chain or envelope out of it is refused ([§2.3](#2.3%20The%20chain%20order%20is%20fixed)) | compression then encryption is fixed in code, as specified, but nothing refuses a stored body layered otherwise, since there is no envelope (D1) |
| D4 | Reading needs only the registry and keys (T3) | decoders exist only for configured layers: disabling compression fails every compressed body, enabling encryption rejects existing plaintext bodies, and disabling encryption is refused |
| D5 | Chunks are sealed with AES-256-GCM-SIV under a key derived from a per-namespace data key wrapped by a master key, and every header field is authenticated ([Appendix B.1](#B.1%20How%20a%20chunk%20is%20encrypted), [Appendix B.2](#B.2%20Keys)) | a random data key per chunk is wrapped under the master key with AES-GCM; frame fields are outside the AAD, and nothing counts the master key's nonces |
| D6 | Relocation re-encodes ([§5.2](#5.2%20Relocation%20re-encodes)) | relocation copies encrypted bodies unchanged |
| D7 | No transform error crosses the codec (T8) | encryption and compression errors reach callers as their own types |
| D8 | The hash is checked once, in the codec (T6) | each consumer re-hashes; relocation does not |
| D9 | A chain that cannot be built stops the store (T5) | the offload finds its encryptor by type assertion, and a missing one writes unencrypted bodies |
| D10 | A key failure is the remote being unavailable, and recovers ([Appendix B.4](#B.4%20Losing%20a%20key%20loses%20the%20data)) | a provider failure at start fails the share's whole block store, journal included; a retired key that fails to load is logged and skipped, with no health condition |
| D11 | Material lost for good is told apart and makes a chunk Lost ([§2.7](#2.7%20Failures)) | no such outcome exists |
| D12 | Retirement relocates through a census ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)) | no census is recorded |
| D13 | Encoding is deterministic within a put attempt and pinned by encoder fixtures, and the chain ID is in the block name ([§2.1](#2.1%20A%20transform%20acts%20on%20one%20chunk), [§2.8](#2.8%20The%20chain%20ID)) | a random data key per chunk (D5), so re-encoding one block for a retry writes different bytes, and no fixture pins an encoder |
| D14 | Material carries a fingerprint, and a read asks only for material its block record lists ([§2.5](#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)) | not yet checked against the code; to verify during the refactor |
| D15 | Header hashes are sealed when encryption is on ([Appendix B.5](#B.5%20What%20a%20bucket%20reader%20still%20learns)) | not yet checked against the code; to verify during the refactor |
| D16 | Read-side bounds come from every registered transform ([§3.1](#3.1%20Interfaces)) | not yet checked against the code; to verify during the refactor |
| D17 | Material removal is fenced against in-flight encodes ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)) | not yet checked against the code; to verify during the refactor |
| D18 | Chunk IDs are keyed per namespace, and every namespace has keys held by a provider ([Appendix B.2](#B.2%20Keys)) | not yet checked against the code; to verify during the refactor |
| D19 | Whether a namespace encrypts is fixed at creation, and compression is off by default when it does ([§3.2](#3.2%20Configuration)) | not yet checked against the code; to verify during the refactor |
| D20 | Material is named per namespace, and removal erases every copy ([§5.3](#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)) | not yet checked against the code; to verify during the refactor |
