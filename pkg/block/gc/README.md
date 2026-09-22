# `pkg/block/gc`

Garbage collection and the offline storage tooling that shares its inputs:
mark-sweep (`CollectGarbage`), block compaction, orphan reclamation, the
read-only orphan report, the manifest check/repair pass and the refcount audit.

The package is a **leaf with respect to the engine** — it reads a share's
metadata store and a remote block store and holds no `*engine.Store`. The one
edge back is `AdoptDedup`, which the engine's carve dedup oracle calls so an
adoption is recorded under the same stripe lock the sweep's claim takes; see
`dedup_sweep_guard.go` for why the two decisions have to be ordered.

## The live set must span every lifecycle state

The mark phase streams every live `FileChunk` hash into a disk-backed live set,
and the sweep frees whatever the live set does not name. So the enumeration the
mark phase runs decides what survives: **a chunk in a lifecycle state the
enumeration skips is indistinguishable from an orphan and gets freed.**

That is not hypothetical. A mark phase that scanned only the sealed set left
every live active unaccounted for, and the sweep reclaimed their bytes out from
under files that still pointed at them. Any change to the enumeration — a new
state, a filter added for speed, a "rows in this state can't matter" shortcut —
has to be checked against this: if the state can hold bytes a file still reads,
the live set has to name it.

The sweep's other two guards do not cover this. The grace TTL only spares
*freshly* synced hashes, and the dedup guard only spares hashes a carve is
adopting right now; neither notices a whole state missing from the scan.

## Failure directions

- **Mark is fail-closed.** Any enumeration error aborts the sweep. An orphan
  left behind costs storage; a live chunk freed costs the file.
- **Sweep continues and captures.** A per-object reclaim error is recorded in
  `GCStats` and the run carries on, so one unreachable object cannot strand the
  rest of the pass.
