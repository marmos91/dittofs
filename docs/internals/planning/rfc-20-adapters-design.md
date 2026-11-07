# RFC 20 adapter model: design brief for the epic plan

The decisions the epic plan for [RFC 20](../rfcs/rfc-20-adapters.md) is built from. They were
settled before planning; the plan (`rfc-20-adapters-plan.md`, written by the `rfc-to-epic-plan`
skill) takes them as given. Where a line says **(builder's call)** it was not decided with the
owner and the plan may change it.

## Intent

Programme #3119, stage 5 ("adapters"). The filesystem service of RFC 17 (`pkg/vfs`,
`internal/vfs.Filesystem`, `vfstest.Record` and `vfstest.Null`) exists on `feat/storage-v2` and no
adapter calls it. Today's adapters reach the stores through `pkg/controlplane/runtime`, written
twice, with the drift Appendix B lists. Stage 5 replaces them with RFC 20's shape: one shared
`core`, thin per-protocol parts, a boundary the build proves.

This plan builds **RFC 20 only**: the generic adapter, its lifecycle, its dispatch, its tests and
benchmarks, and each protocol's scaffold on it. What each protocol serves is RFC 21's and RFC 22's,
planned separately with the same skill afterwards.

## Goals

1. `internal/adapter/core` — `Server` and its four states, listeners, accept, drain, `Reconfigure`
   with the settings snapshot, health, `Connections()`; `Conn` with one reader and one writer;
   §4.1's pipeline; `OpTable`, `Op`, `Invocation`, `Bind`; the compound runner; held requests;
   the middleware chain; the `ErrorTable` type and its totality check.
2. The public surface in `pkg/adapter`: the contract (`Adapter`, `Health`, `ConnInfo`,
   `Sidecar`, `Serving`), `Deps`, the settings shapes in `pkg/adapter/config`, and the
   constructors `pkg/adapter/nfs.New` and `pkg/adapter/smb.New`.
3. A minimal `pkg/resources` (RFC 24 §2.8, amended from `pkg/resources`), shared by every component, not only adapters:
   `Budget` and `Reservation` (reserve, release, reserved bytes; wait or refuse per pool) and
   `Buf` and its `BufPool` (RFC 24 §3.1's four classes, a `sync.Pool` per class, reference counted,
   reserved before taken). Decided by the owner, replacing an adapter-only `buf` package. The rest
   of RFC 24 — per-address turns, `Reclaim`, `ShareCap`, `Detect` and `Split` — is not built, and
   each gap carries a `ponytail:` marker naming RFC 24 as the upgrade.
4. `internal/adapter/adaptertest` — a **test-only protocol** that proves `core` alone, the import
   and call-site test of §7.1, golden-vector and capture helpers.
5. A thin SMB scaffold and a thin NFS scaffold on `core` (below).
6. Every test and benchmark the RFC states for these parts, registered as `dt` suites in
   `test/suites.yaml` at the tier RFC 29 gives it. These are the reference for v2.
7. Observability per §10, all in `core`.
8. The generic sidecar runner (§3.5), decided by the owner: `internal/sidecar`'s root package
   drives a set of `Sidecar`s from the adapters' states — `Sync` with the serving set after an
   adapter is serving and before it drains, a disabled sidecar synced with the empty set, a failing
   one raising `sidecar_failed` without touching the adapters or readiness (A19). Proven with fake
   sidecars and `testproto` adapters. The concrete sidecars (mDNS, WS-Discovery, port mapper,
   rpcbind) stay out; `pkg/discovery/mdns` and `wsd` move onto the runner at cutover.

## Non-goals

- Any file operation handler, and any protocol-only handler (`NEGOTIATE`, `SESSION_SETUP`,
  `SEQUENCE`, `NULL`, `ECHO`, MOUNT, the §4.10 entry points): RFC 21's and RFC 22's plans.
- Each protocol's error-table rows, and so the totality test passing per protocol: those plans.
- RFC 20's protocol-security and translation parts — the `gss` package of §4.4 (Kerberos,
  NTLM, SPNEGO, `Exchanges`, `Identity`), each protocol's `Security`, `Flow` and `Replay` stage
  implementations (signing and encryption, credits, message-ID window, GSS window, NFSv4.1 slots and
  reply cache), session registries, SMB `Caps`, and `translate`. Decided by the owner: nothing in
  this plan calls them; they go with the RFC 21/22 plans that first do, and §4.4 may become an RFC
  of its own. `core` proves the stage *interfaces* with toy stages in `testproto`.
- The concrete sidecars, `Callbacks`, `Conn.StartTLS`, the datagram listener, MOUNT/NLM/NSM tables, the
  `dcerpc` seam: their RFC 21/22 epics, or cutover for sidecars.
- Feature parity with develop, and re-enabling today's suites: those plans, then cutover (#3119
  stage 7).
- Wiring adapters into `cmd/dfs` and `node.New`: cutover.

## Anti-goals

- A port of today's code. Mechanism code Appendix B marks **keep** (SPNEGO and NTLM message
  handling, GSS per-message tokens, NFS-over-TLS through `tlsconfig`, accept backoff) is lifted
  from develop's history into its new home; nothing else is carried over.
- A `core` proven only through a protocol: a `core` bug must never be confusable with a protocol
  bug. `core` and its tests import no protocol package, `smb` or `nfs`, and the import test
  checks it; block 1 is complete and green with neither scaffold present.
- Any import into an adapter package but what §2.3 lists, from the first commit.

## Transition from v1

Clean slate, decided by the owner:

1. The first epic deletes `internal/adapter/` and `pkg/adapter/` on `feat/storage-v2` (about 930
   Go files) in one change, with everything that imports them, decided by the owner as "delete
   everything": `pkg/controlplane/runtime/adapters`, the adapter start path in `cmd/dfs`, the API
   endpoints that read adapter state (`grace.go`, `nfs_clients.go`) and their routes, the identity
   mapping config in `pkg/config` that imports `pkg/adapter/nfs/identity`, the API tests on
   `testruntime`. `internal/vfs/arch_test.go` keeps its adapter clause: its fixtures are never
   built, and it is RFC 17's check that the service imports no adapter. The branch then builds
   and serves no protocol, and loses that API and config surface until RFC 21/22 bring it back.
   develop keeps the whole implementation and stays the reference for parity.
2. In the same epic, the suites that need a served protocol stay skipped on the branch by
   `tests.yml`'s V2 filter, which already skips `e2e`, `pjdfstest`, `pynfs`, `wpts`,
   `smbtorture`, the conformance and client suites and `integration/ad-dc`. The filter grows by
   `integration/kerberos`, whose package is deleted, and the required job's fixed
   `dt run integration/kerberos` step goes on the branch. `test/suites.yaml` gains one cell,
   `unit/adapter-allocs`, run per change from a fixed step as `unit/journal-index` is; otherwise
   it stays as develop has it, so each suite keeps one switch. `unit`, `integration`, journal, engine and lint keep
   running.
3. An adapter holds no durable state (A2), and RFC 14's `Adapter` bytes are new with v2. What
   the control plane persists for adapters (the `NFSAdapterSettings` and `SMBAdapterSettings`
   rows, the share adapter config) is mapped onto `pkg/adapter/config` at cutover; the plan's
   ledger carries it.

## Order

Three blocks, in sequence; the plan's epics fall inside them.

1. **The generic adapter and its lifecycle.** Goals 1–4, 6, 7, 8 for `resources`, `core`, `adaptertest`, the sidecar runner.
2. **SMB scaffold.**
3. **NFS scaffold.**

### Proving `core`: a test protocol, not NFSv3

RFC 20 §7 runs the `core` layer on the real NFSv3 adapter. With NFS last, block 1 would have no
protocol. Decided: `adaptertest` carries a test-only protocol — length-prefixed frames, an echo
operation, a scripted delay, an operation that panics, one that holds — and every `core` test
(lifecycle, drain, reconfigure, connection limit, flow control, writer order, slow-client
isolation, panic recovery, cancellation, no buffer leaked) and §6.10's pipeline micro-benchmark run
on it over loopback against `vfstest.Null`. The plan includes the amendment to RFC 20 §7 (and the
§3.6 and §6.10 sentences that name NFSv3 for this) saying so.

### What a protocol scaffold is

The least that shows a protocol plugs into `core` as RFC 20 says, and that the boundary checks hold
on it:

| Part | SMB | NFS |
| --- | --- | --- |
| public `New(adapter.Deps, Settings) (adapter.Adapter, error)` and the internal one it calls | `pkg/adapter/smb`, `internal/adapter/smb` | `pkg/adapter/nfs`, `internal/adapter/nfs` |
| `wire/`: framer and header codec, golden vectors, fuzz targets, header 0-alloc gate | NetBIOS, SMB2 header (transform header with `Security`, in RFC 22) | record mark, RPC call and reply header, AUTH_SYS credential |
| `Protocol` and `Router` | `protocol/`: by dialect | `protocol/`: by (program, version) |
| one empty `OpTable` and `ErrorTable` | in `smb.go` | in `nfs.go` |

Every other stage is nil and takes `core`'s default. A scaffold is done when a connection is
answered "not supported" for every operation through the real pipeline over loopback, and the
import, file-size and `wire`-purity checks of §7.1 pass on it.

## `pkg/` and `internal/`

Decided by the owner: **the public surface is in `pkg/`, the implementation in `internal/`**, as
`pkg/vfs` is the service's surface and `internal/vfs` its implementation.

| Package | Where | Why |
| --- | --- | --- |
| `vfs`, `vfstest` | `pkg/vfs`, `pkg/vfs/vfstest` (exist) | the service's contract and its fakes |
| `metrics` | `pkg/metrics` (exists) | shared by every component |
| `Budget`, `Reservation`, `Pools`, `Buf`, `BufPool` | `pkg/resources` | RFC 24's package, public because `adapter.Deps` carries `Pools`; the adapters, open state and the service charge it, and any component wanting pooled buffers takes it; imports only the standard library and `pkg/metrics` |
| `Adapter`, `Health`, `ConnInfo`, `Sidecar`, `Serving`, `Deps` | `pkg/adapter` | the contract the node drives and `core.Server` implements; the control plane's client list names `ConnInfo` |
| `Adapter`, `NFS`, `SMB` settings shapes | `pkg/adapter/config` | a leaf the config loader, the management API and `dfsctl` name, with the live vs restart-only marks |
| `New` | `pkg/adapter/nfs`, `pkg/adapter/smb` | one public constructor per protocol, `New(adapter.Deps, config.NFS) (adapter.Adapter, error)`, a one-line call into the implementation; returns the contract, never `*core.Server` |
| `core`, `adaptertest` (later `gss`, `translate`) | `internal/adapter/…` | implementation; `core`'s API is not public |
| each protocol's implementation | `internal/adapter/nfs`, `internal/adapter/smb` | imports `pkg/adapter` and `pkg/adapter/config`, never its constructor package, so there is no cycle |
| the sidecar runner; the concrete sidecars at cutover | `internal/sidecar` (root), `internal/sidecar/…` | built and driven by the node only |
| TLS loading | `internal/tlsconfig` (exists) | shared with the control plane's listener |
| keytab, PAC, NETLOGON, LDAP | `internal/identity` (RFC 18; today `internal/auth`, `pkg/auth`, `pkg/identity/ldap`) | the authenticator behind `SignIn`, not adapter code; adapter packages never import it |

**Why the contract is not declared by the node.** `core.Server` returns `Health` and `ConnInfo`, so
`core` names them. Declared in `internal/node`, they close a cycle: `node.New` → `nfs` → `core` →
`node`. In `pkg/adapter`, both import it and neither imports the other.

**One shape for both protocols.** `internal/adapter/nfs` and `internal/adapter/smb` have the same
directories, so a reader who knows one finds their way in the other:

| Directory | Holds | NFS | SMB |
| --- | --- | --- | --- |
| `nfs.go` / `smb.go` | `New`: builds stages, handler sets, tables; returns `*core.Server` | | |
| `wire/` | pure codecs, golden vectors, fuzz targets | XDR, RPC header, AUTH_SYS; generated | NetBIOS, SMB2 header; hand-written |
| `protocol/` | the `Protocol` and its stages, the session registry | record-mark framer, router by (program, version); later the GSS window, slots, reply cache | NetBIOS framer, router by dialect; later signing, encryption, credits, `Caps` |
| `ops/` | the operation tables, one subpackage per table (§4.8); empty in this plan | later `v3/`, `v4/`, `mount/`, `nlm/`, `nsm/` | later one table for every dialect, `ioctl/` |
| `callbacks.go` | `Callbacks` | later | later |

The one asymmetry is the protocol's own: NFS's versions and auxiliary programs are several tables on
one RPC protocol, SMB's dialects one table, so `ops/` holds five subpackages in one and one in the
other. SMB's `dcerpc/` sits beside them and imports no SMB package. RFC 20 §2.5's layout is
amended to this shape (it named the protocol packages `rpc/` and `session/`).

### Target layout

What exists after this plan, `(exists)` marking what is on `feat/storage-v2` already. Files are
indicative; directories are not.

```text
pkg/
  vfs/                      (exists) the service interface and value types (RFC 17)
    vfstest/                (exists) Record, Null, the service suite
  metrics/                  (exists)
  resources/                RFC 24's package, minimal, shared by every component
    budget.go               Budget, Reservation, Pools
    buf.go                  Buf, BufPool: §3.1's classes, refcount, sync.Pool per class; wire may import it
  adapter/
    adapter.go              Adapter, Health, ConnInfo, Sidecar, Serving
    deps.go                 Deps
    config/                 Adapter, NFS, SMB settings, live vs restart-only marks (leaf)
    nfs/  smb/              New: the public constructors
  identity/                 (exists) RFC 18's contract: Evidence, SignInStep, SID…; adapters see it only as pkg/vfs aliases
internal/
  identity/                 RFC 18's authenticator, behind vfs.Service.SignIn; built by RFC 18's plan, not this one
    (kerberos)              AP-REQ acceptance, keytab and its rotation, PAC verification   ← today internal/auth/kerberos, pkg/auth/kerberos
    (ntlm, netlogon)        NTLMv2 verification, pass-through to the domain controller     ← today internal/auth/netlogon
    directory/              the LDAP / Active Directory client, the only LDAP importer       ← today pkg/identity/ldap
  tlsconfig/                (exists)
  adapter/
    core/                   one package, a file per concern; imports no protocol
      server.go             Server: states, listeners, accept, drain, health, connection map
      settings.go           the settings snapshot, Reconfigure, restart-only check
      conn.go               Conn: reader, writer, deadlines, TCP options
      protocol.go           Protocol, Framer, Stages and the stage interfaces, their nil defaults
      pipeline.go           §4.1's stages in order
      dispatch.go           OpTable, Op, Invocation, Bind, Needs, Range
      compound.go           the compound runner
      hold.go               held requests, interim timer, re-drive
      middleware.go         recovery, trace, metrics, deadline, in that order
      errors.go             ErrorTable and its totality check
      metrics.go            the series core produces, resolved at table build
    adaptertest/
      testproto/            the test-only protocol, with toy Security/Flow/Replay stages; core's tests are package core_test
      golden/               golden-vector helpers: a leaf, so wire tests need no core
      imports_test.go       §7.1: imports, call sites, file sizes, wire purity, test-only packages unimported
      testdata/violation/   the deliberate violation the import test must report
    nfs/                    same shape as smb/
      nfs.go                New; the empty OpTable and ErrorTable
      wire/                 record mark, RPC header, AUTH_SYS credential (pure)
      protocol/             Protocol, Router by (program, version)
    smb/
      smb.go                New; the empty OpTable and ErrorTable
      wire/                 NetBIOS framer, SMB2 header (pure)
      protocol/             Protocol, Router by dialect
    gss/                    later (RFC 21/22, or its own RFC): the wire side of authentication, no secret
                              Kerberos, NTLM and SPNEGO mechanisms, Exchanges, Identity, RFC 4121 MIC and wrap;
                              driven by nfs/protocol (RPCSEC_GSS) and smb/protocol (SESSION_SETUP)
      gsstest/              in-process initiators: Kerberos without a KDC, NTLM, SPNEGO
  sidecar/
    runner.go               drives Sidecars from the serving set: order, disabled, sidecar_failed
```

`ops/` and `callbacks.go` appear with the RFC 21/22 epics that fill them.

**Kerberos, NTLM and Active Directory, split in two (RFC 20 §4.4, RFC 18 §2.7).** The adapter
speaks the security messages; the authenticator holds every secret and talks to every directory.

| Piece | Lives in | Imported by |
| --- | --- | --- |
| Kerberos, NTLM, SPNEGO message handling; `Exchanges`; per-request `Identity`; RFC 4121 tokens | `internal/adapter/gss` | `nfs/protocol`, `smb/protocol` |
| SMB signing and encryption keys derived from the session key | `internal/adapter/smb/protocol` | — |
| RPCSEC_GSS context table and sequence window | `internal/adapter/nfs/protocol` | — |
| keytab, AP-REQ acceptance, PAC; NTLMv2 verification; NETLOGON pass-through | `internal/identity` (RFC 18) | the node only; reached by adapters through `vfs.Service.SignIn` |
| LDAP / AD lookups, ID mapping, SIDs | `internal/identity/directory` (RFC 18) | `internal/identity` only |
| lsarpc name and SID lookups from Windows | `internal/adapter/smb/dcerpc` (RFC 22) | one call each to the service |

No adapter package imports `pkg/identity` or `internal/identity` (§7.1). Today's
`internal/auth/{kerberos,netlogon}`, `pkg/auth/kerberos` and `pkg/identity/{kerberos,ldap}` are not
adapter code: this plan leaves them where they are, and RFC 18's plan moves them into
`internal/identity`. Their message-handling halves — SPNEGO, NTLM messages, GSS per-message code —
are lifted from develop's adapters into `gss` when it is built.

Gone: today's `pkg/adapter/` (replaced by the public surface above), today's
`internal/adapter/{common,nfs,smb,pool,testruntime}`, and `pkg/controlplane/runtime/adapters`.

The import test of §7.1 checks: no adapter implementation imports a core package other than
`pkg/vfs`, `pkg/metrics`, `pkg/resources`, `pkg/adapter` and `pkg/adapter/config`; `pkg/adapter`,
`pkg/adapter/config` and `pkg/resources` import nothing under `internal/`; only
`pkg/adapter/nfs` and `pkg/adapter/smb` import `internal/adapter/`, each its own protocol; and no
implementation imports a constructor package.

## Tests are the v2 reference

The skill derives the test set from RFC 20's obligations, and each is a `dt` suite in
`test/suites.yaml`:

- `pr`/`unit`: codec golden vectors, round trips, fuzz seeds; stage tests; `core` tests on the test
  protocol; the import
  and call-site test (§7.1), seeded with one violation it must report; the `TCP_NODELAY` and
  keepalive assertion (§6.7); the middleware-order pin; `vfstest.Record`'s one-mutating-call
  enforcement.
- `unit/adapter-allocs`, per change and non-race: §6.10's codec 0-alloc gates and the pipeline
  allocation ceiling. `unit/nonrace` is nightly, and the nightly never runs `feat/storage-v2`.
- Registered but not yet run (they need handlers): error-table totality per protocol (§7.3), the
  handler layer (§7.2), cross-protocol (§7.4), end to end (§7.5), wire diff (§8), the end-to-end
  benchmark. The plan names which RFC 21/22 epic turns each on.

## Constraints from other RFCs

| RFC | Scope here |
| --- | --- |
| 17 (service) | constraints only: `pkg/vfs` exists; a missing service method is a finding against RFC 17, not adapter code |
| 13 (configuration) | only the `pkg/adapter/config` shapes and the live vs restart-only marking `Reconfigure` needs |
| 14 (open state) | constraints only: `Callbacks`, `Adapter` bytes format rules |
| 18 (identity) | constraints only: `gss` is deferred |
| 21, 22 (protocols) | constraints only: the scaffold's stage shapes |
| 23 (node) | only the `pkg/adapter` contract it drives; `internal/node` and `node.New` are cutover's |
| 24 (resources) | in scope for its minimal slice: `Budget`, `Reservation`, `Pools`, `Buf`, `BufPool` in `pkg/resources`; the plan amends RFC 24 §2.8 to put `Buf` there |
| 25 (observability) | §10's metrics, spans and logs, through `pkg/metrics` |
| 29 (harness) | in scope: every suite registered in `test/suites.yaml` |

## Calls made after review

Settled by the builder from the architect and over-engineering reviews; the owner may overturn any.

- **No empty scaffolding.** No empty `ops/` (or its `v3/`, `v4/`, `mount/`, `nlm/`, `nsm/`),
  `dcerpc/` directories and no `Callbacks` types without message kinds: each arrives with the
  RFC 21/22 epic that fills it. A protocol's empty `OpTable` and `ErrorTable` live in `nfs.go` /
  `smb.go`.
- **Suites are registered with their tests**, in the PR that adds them; no `test/suites.yaml`
  entry with nothing behind it.
- **0-alloc codec gates cover the header codecs here**; the operation codecs §6.10 names
  (`READ`, `WRITE`, `GETATTR`, `QUERY_INFO`) and their gates come with RFC 21/22.
- **One import test for the adapters.** §7.1's test checks what the adapters import;
  `internal/vfs/arch_test.go` keeps checking that the service imports no adapter.
- **Deferred to RFC 21:** `Conn.StartTLS` (NFS-over-TLS starts at the `NULL` handler) and the
  datagram listener (open question 3).
- **Concrete sidecars at cutover.** The runner and the contract are built here; `pkg/discovery/mdns`
  and `wsd` already exist on the branch and are moved onto the runner then, not rewritten.
- **Metrics:** only the series `core` produces in this plan, through `pkg/metrics`; the replay,
  translation, status-mapped and record series arrive with their producers.
- **No separate `registry.go` or `capture.go`:** the connection registry is a map in `server.go`;
  capture helpers come with §8's wire-diff epic.

## Left to the builder

- Epic boundaries and estimates inside the three blocks, tracks A/B/C, synchronisation points (the
  skill's).
- Open question 1 (generated SMB codecs): **(builder's call)** hand-written for the scaffold's
  headers; revisit in RFC 22's plan.
- Open questions 2 and 3: the scaffold leaves the `Replay` slot and the datagram listener in place;
  RFC 21's plan decides.
- **(builder's call)** The plan document lands on develop as a `docs(planning)` commit and is
  cherry-picked to `feat/storage-v2`, as other planning docs; the code PRs target
  `feat/storage-v2`.
