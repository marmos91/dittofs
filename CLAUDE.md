# CLAUDE.md

Guidance for Claude Code working in this repository. Project details live in `README.md` and
`docs/` — read those for depth. This file captures what *won't* be found there: invariants,
conventions, and playbooks.

## Project

DittoFS is an experimental modular virtual filesystem in Go. NFSv3/v4.0/v4.1 + SMB2/3 servers
in userspace (no FUSE), with pluggable metadata and block stores. **Not production ready.**

Two binaries: `dfs` (server, `cmd/dfs/`) and `dfsctl` (REST client, `cmd/dfsctl/`). Both use Cobra.

## Where to look

- `README.md` — install, quick start, feature matrix
- `docs/internals/architecture.md` — full design, diagrams, directory map
- `docs/guide/configuration.md` — config file + env vars (`DITTOFS_*`)
- `docs/guide/cli.md` — full `dfs` / `dfsctl` reference (**generated** — see below)
- `docs/guide/nfs.md`, `docs/guide/smb.md` — user mounting guides (protocol internals in `docs/internals/`)
- `docs/guide/smb-acl-fidelity.md` — SMB Windows-ACL/SD interop matrix (Works/Partial/Unsupported)
- `docs/internals/implementing-stores.md` — metadata/block store contracts
- `docs/internals/debugging.md` — SMB/NFS pcap-diff interop playbook
- `docs/guide/faq.md` — known limitations (ETXTBSY, POSIX gaps, single-node)
- `docs/internals/contributing.md` — dev workflow

## Finding your way around the code

There is a graphify code graph at `graphify-out/`. It is one tool among several,
not a gate you must pass through — route by the *shape* of the question, because
the wrong tool costs either tokens or a round trip, and there is no prize for
using the graph on a question it cannot answer.

| The question | Use |
| --- | --- |
| Who calls `X`? What breaks if I change it? How do `A` and `B` connect? | `graphify query` / `path` / `explain` |
| Where is the identifier `X` — a symbol, error string, flag, `DITTOFS_*` key? | `rg` directly |
| Something spanning many files whose names you don't know yet | an `Explore` / `Agent` subagent |
| Anything in `test/`, `docs/`, `.planning/`, `.pi/`, `.claude/`, or non-Go files | `rg` / `Read` — not in the graph |

**The graph is AST-only.** `graphify update .` extracts symbols and edges; it
runs no model and infers no intent. So a question phrased the way you'd ask a
colleague — "why do cold reads return zeros after a restart" — is matched as the
bare keywords *cold*, *read*, *restart* and comes back as noise. Name a symbol,
a type, or a package, or don't use it. If a query returns junk, that is the
answer: switch to `rg`, don't re-word it three times.

Greps that stay cheap: scope to the package directory rather than the repo root,
`rg -l` first when you only need the file set, `-g '!*_test.go'` when tests are
drowning the signal, and `-B2 -A2` instead of reading the whole file afterwards.

Run `graphify update .` after merging, from the main checkout on the merged
code — see the `fix-issue` skill, step 10.

## Frequent commands

```bash
# Build
go build -o dfs    cmd/dfs/main.go
go build -o dfsctl cmd/dfsctl/main.go

# Unit/integration
go test ./...
go test -race ./...

# E2E (requires sudo + kernel NFS client)
cd test/e2e && sudo ./run-e2e.sh           # all configs
sudo ./run-e2e.sh --s3                      # include S3 via Localstack
sudo ./run-e2e.sh --test TestCreateFile_1MB

# Lint
go fmt ./...
go vet ./...

# Regenerate docs/guide/cli.md after changing any command/flag
go run ./cmd/gendocs

# Run server with debug logging
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
```

Default NFS port is `12049` (not 2049). Server config lives at `~/.config/dittofs/config.yaml`
(`dfsctl` credentials at `~/.config/dfsctl/config.json`).

`docs/guide/cli.md` is generated from the Cobra command trees by `cmd/gendocs`. Never hand-edit it —
change the command definitions and rerun `go run ./cmd/gendocs`.

## Architecture invariants

The Runtime (`pkg/controlplane/runtime/`) is the single entrypoint for all operations — a
composition layer over six sub-services: `adapters/`, `stores/`, `shares/`, `mounts/`,
`lifecycle/`, `identity/`. API handlers and protocol adapters both go through it.

**Key rules you must preserve when editing:**

1. **Protocol handlers handle only protocol concerns.** XDR/SMB wire encoding, framing,
   dispatch, type conversion. Permission checks, file ops, and directory traversal belong in
   `pkg/metadata/` and the store implementations. Don't inline business logic into handlers.

2. **Every operation carries an `*metadata.AuthContext`.** Created in
   `dispatch.go:ExtractAuthContext` for NFS and in the SMB session layer. It threads through
   RPC → handler → store. Export-level squashing (`AllSquash`, `RootSquash`) is applied via
   `ResolveSharePermission` (`internal/adapter/nfs/auth/share_permission.go`) and the SMB
   Tree Connect check (`internal/adapter/smb/handlers/tree_connect.go`).

   The export auth-flavor policy (`AllowAuthSys`, `RequireKerberos`, `MinKerberosLevel`) lives in
   `CheckExportAccess` (`internal/adapter/nfs/auth/export_access.go`), which also applies the
   netgroup client allowlist — but only when its caller supplies a lookup. It has three callers:
   MOUNT supplies one, the NFSv3 per-operation dispatch does not, and the NFSv4 auth-context
   builder does not either, because it runs its own netgroup check first with its own status. The
   flavor gates run per operation, not only at mount.

   **NFSv4 needs a second gate, and it is not that one.** LOCK, LOCKT, LOCKU and
   GET_DIR_DELEGATION act on the current filehandle without ever building an auth context, so
   nothing in `buildV4AuthContext` reaches them. They are gated where a share handle *enters* the
   compound, and there are exactly two such places: PUTFH, and a LOOKUP that crosses an export
   junction out of the pseudo-fs. Both route through `shareEntryStatus`
   (`internal/adapter/nfs/v4/handlers/helpers.go`) — keep it that way rather than growing a second
   copy. Its two checks previously existed at one of the two sites only, which is how both a
   quiesced share and a netgroup-restricted one stayed reachable by walking the pseudo-fs.

   **That gate covers the share's enabled flag and its netgroup allowlist, not the flavor
   policy.** `CheckExportAccess` still runs only where an auth context is built, so those four
   operations reach a Kerberos-only share over AUTH_SYS on both entry paths. Do not write a
   comment or a rule asserting the handle-entry path is fully gated: two comments claimed exactly
   that, in opposite directions, and both were wrong.

3. **File handles are opaque.** Generated by the metadata store; they encode share identity so
   the Runtime can route. Handlers never parse or interpret them. They must stay stable across
   restarts for persistent backends.

4. **Block stores are per-share, and every share has one.** Each share owns a
   `*engine.Store` (journal + block store + syncer). No global block store exists.
   Resolve via `rt.GetBlockStoreForHandle(ctx, handle)`. The `*engine.Store` itself is never
   shared, but the S3/memory store behind it is ref-counted when shares reference the same
   config. Each share's journal hangs off the server-level `blockstore.journal.path` in its
   own subdirectory, so two shares never share one. Lifecycle is tied to `AddShare` /
   `RemoveShare`.

5. **WRITE coordinates metadata + block store in this order:** `metadataStore.WriteFile`
   (permission check, size/mtime update, returns pre-op attrs for WCC) →
   `GetBlockStoreForHandle` → `blockStore.WriteAt` → return updated attrs.

6. **Error codes:** return `metadata.ExportError` values (`ErrNotDirectory`, `ErrNoEntity`,
   `ErrAccess`, `ErrExist`, `ErrNotEmpty`, …). Log expected errors at `Debug`, unexpected at `Error`.

7. **Store contracts** live in `pkg/metadata/storetest/` — any new metadata backend must pass
   that conformance suite. Block stores have exactly one suite,
   `blockstoretest.RemoteBlockStoreConformance`, and it covers only the block-keyed
   `remote.RemoteBlockStore` surface. There is **no hash-keyed suite**: the CAS `block.Store`
   interface and its `BlockStoreConformance` were deleted, leaving orphan godoc for the
   interface in `pkg/block/blockstore.go` — that file's `Meta`, `DurabilityReporter` and
   `IsDurable` are still live. The local tier is payload-keyed and is covered by its own
   package tests.

## Verifying a change

Two failure modes this codebase has actually hit. Both are invisible to a passing test suite,
so they are checks to run deliberately rather than rules a test will catch for you.

**Test the layer that consumes the data, not the layer that produces it.** A regression test
placed next to the function you changed can pass green while the bug is untouched, because the
data may be dropped again downstream. When a fix carries a field from A to B, the test belongs
at B — the consumer. A fix that set `Identity.SID` where the identity was *built* proved nothing:
the RPC layer copied only UID/GID/GIDs into the handler contexts, and both auth-context builders
then built a fresh identity from those three fields, so the SID was read by nobody. Verify a
regression test by reverting the fix and watching it fail **on the assertion** — a build error
from an unused import is not proof.

**When a change starts populating a field that was always empty, audit the cache keys.** Keys,
dedup keys and equality checks written against the old field set are invisible to `rg` for the
field name, because the bug is in the key rather than the field. Carrying SID/GroupSIDs into
`AuthContext.Identity` silently made `authCacheKey`
(`internal/adapter/nfs/v3/handlers/doc.go`) incomplete: it keyed on share + flavor + uid + gid +
gids, and two Kerberos principals can share that triple because a user record with no UID
defaults to 1000 (`pkg/adapter/identity.go`). The second principal would have been served the
first's cached identity and inherited its SID-keyed ACE grants — failing **open**, unlike the
missing-SID bug the change set out to fix.

When a gate or check is deliberately narrower than its name suggests, say so at the code site
with a `decision:` marker (below) rather than leaving the next reader to infer the ceiling.

## Code comments

- Comments describe the code's **behaviour** — what it does and why, in terms of
  the code itself. Never reference things external to the code: no issue/PR
  numbers, no CI/runner/OS names, no phase/plan/decision IDs. Those belong in
  commit messages, PR descriptions, or `.planning/`, not in source.

### `ponytail:` markers

A `ponytail:` comment marks a **knowingly-simple implementation**, naming its ceiling and
the upgrade path that would justify replacing it:

```go
// ponytail: O(n) keys-only scan per candidate, and only an overlap yields more
// than one candidate; upgrade to a big-endian fb-off index for a true O(log n)
// reverse-seek only if profiling at real N still shows it.
func (s *BadgerMetadataStore) GetFileChunkAtOffset(...)
```

They are a **debt ledger, not TODOs**: each one records a decision that was correct at the
time along with the evidence that would overturn it. They appear throughout the codebase,
mostly in `pkg/block/engine`, `pkg/block/journal` and `pkg/metadata/store`.

This is the one sanctioned exception to the "no external references" rule above — the
prefix is what makes the set harvestable as a group. Keep the marker when editing nearby
code; drop it only when the shortcut is actually replaced, not when the comment is merely
reworded.

### `decision:` markers

The same ledger, one axis over. A `ponytail:` marker says the *implementation* is simpler
than it could be; a `decision:` marker says the *behaviour* is deliberate — most often that
a gate is knowingly not applied:

```go
// decision: a null or guest session is exempt from encryption enforcement
// because it holds no session key and cannot encrypt at all (MS-SMB2 3.3.5.2.9).
// The exemption is worth only what such a session can reach — withdraw it if one
// can ever reach a share holding data.
if sess.IsNull || sess.IsGuest {
```

Write one whenever a check is deliberately skipped, narrower than its name suggests, or
fails open. Name the rule, why it holds, and what evidence would overturn it — an exemption
with no stated ceiling is indistinguishable from a bug, both to a reviewer and to whoever
audits the gate later.

The decision is recorded **at the code site**, not in the issue that asked for it. #2518
asked for exactly the exemption above; #2523 shipped it without stating the reasoning
anywhere, and nobody noticed, because an issue thread is not a thing anyone reads while
changing the line.

## Commits & PRs

- Never mention Claude Code, AI tools, or add `Co-Authored-By` lines for AI.
- Keep commit messages concise.
- Sign commits (`git commit -S`) when possible.

## Releases

**Tags live on `main`.** `main` is a linear, strict ancestor of `develop`; releases flow
develop → main. v0.18.0 / v0.19.0 broke this by tagging develop-only and never advancing
`main` (it stuck at v0.17.0 for 152 commits) — don't repeat that.

Steps (X.Y.Z):

1. Confirm `develop` is clean and synced with `origin/develop`.
2. Bump `flake.nix` `version = "X.Y.Z"` — the single source of truth, only line that changes.
3. Commit **flake only**: `chore(release): vX.Y.Z` on develop (signed). Push develop.
4. **Fast-forward `main` to that commit** (the easily-forgotten step):
   `git branch -f main <commit> && git push origin main:main`.
5. Signed annotated tag on that same commit: `git tag -s vX.Y.Z <commit> -m "vX.Y.Z"`,
   then `git push origin vX.Y.Z`. The tag push triggers `release.yml`; non-rc tags push
   docker `:latest`.

Notes:
- SSH signing failing with `Couldn't get agent socket` → use the key explicitly:
  `git -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S` (same for `tag -s`).
- `develop` and `main` are protected; admin bypass lets release pushes through (old unsigned
  commits in the catch-up range show as harmless "violations").
- A flaky SMB-conformance run does not block cutting a tag.
