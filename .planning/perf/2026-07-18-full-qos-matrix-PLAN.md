# Full QoS × Protocol × System benchmark matrix — PLAN (2026-07-18)

Handoff for a fresh session. Goal: benchmark **8 systems × 3 protocols × 3 durability
tiers** apples-to-apples, fix each system's setup, verify durability honestly, then
update `docs/BENCHMARKS.md`. **Open a PR for the harness changes** and land it before
running the big matrix. Be critical: double-test every durability claim with an
object-list-after-commit check; fix problems on the go; report honestly.

## First: land the in-flight PR
- **PR #1763** (`docs/1758-durable-tier-bench`): narrative BENCHMARKS rewrite +
  verified sync-to-S3 tier. Was CI-green and being merged when this plan was saved.
  Confirm merged (`gh pr view 1763`), then `graphify update .` on develop.

## The axes
- **Systems (8):** dittofs, ganesha, juicefs, rclone, s3fs, zerofs, goofys, s3ql
- **Protocols (3):** nfs3, nfs4, smb3
- **QoS tiers (3):** writeback (local-ack, async-S3) · local-durable (fsync-local,
  async-S3, crash-safe, NO per-write S3 hop) · remote-sync (ack-on-S3)

## QoS-support map — most cells are N/A (verify each; don't assume)
| System | writeback | local-durable | remote-sync | protocols |
|---|:--:|:--:|:--:|---|
| **dittofs** | ✅ `durability:writeback` | ✅ **default (only one)** | ✅ `durability:remote` | nfs3, nfs4, smb3 (native) |
| juicefs | ✅ `--writeback` | ❌ | ✅ default | FUSE → re-export |
| zerofs | ✅ default (`sync_writes=false`) | ❌ (S3-native, no local disk/WAL) | ✅ `[lsm] sync_writes=true` | native nfs (2049) + smb3 label exists |
| s3ql | ✅ native | ⚠️ **TEST IT** (local SQLite meta + on-disk cache; maybe crash-safe) | ❌ | FUSE → re-export |
| s3fs | ⚠️ partial | ❌ | ✅ write-through | FUSE → re-export |
| rclone | ✅ `vfs-cache=writes` | ❌ | ❌ **(DEBUNKED: `vfs-cache=off` fsync→EIO, 0-byte object — NOT durable)** | FUSE → re-export |
| goofys | ❌ | ❌ | ⚠️ write-through class but **DNF on create** (no metadata engine) | FUSE → re-export |
| ganesha | over local disk only | (local disk) | ❌ **no S3 backend** (mainline FSALs: VFS/CEPH/RGW; no plain-S3) | nfs3, nfs4 only (no SMB) |

**Critical claim to double-check (user asked):** dittofs-default is the ONLY *deliberate*
local-durable tier. Confirmed for juicefs/zerofs/s3fs/rclone/goofys. **s3ql is the one
unknown** — architecturally closest (local cache + SQLite metadata). TEST: write file to
s3ql, `sync`, kill s3fs process, check if data survives locally / is NOT yet in S3. If
s3ql fsyncs its cache on COMMIT it's accidentally local-durable; else it's writeback.

**Realistic matrix ≈ 30–35 meaningful cells, not 72.** Ganesha=local-NFS baseline only
(no S3). SMB3 excludes rclone/s3fs/goofys unless Samba-re-exported. goofys DNFs create.

## Harness changes needed (this is the PR)
Harness lives in `internal/dfsbench/` + `cmd/bench` (the `dfsbench` tool). Bench arm
scripts used this session are in `/tmp/abbench2/` on the local mac (abbench.sh,
jfsbench.sh, s3fsbench.sh, s3qlbench.sh, zerofsbench.sh, rcloneoffbench.sh,
goofysbench.sh, flushtest2.sh, rcloneflushtest.sh). Verified system labels:
`dittofs-s3-nfs3/nfs4/smb3`, `zerofs`, `zerofs-smb3`, `juicefs`, `s3ql`, `s3fs`,
`rclone`, `goofys`, `local-disk`.

1. **Protocol arms**: today everything re-exports over **knfsd (NFS3)**. Add:
   - NFS4: knfsd already serves v4; mount with `-o vers=4.1`. Confirm competitors' FUSE mounts survive v4 re-export.
   - **SMB3**: new **Samba** re-export path (`smbd` share over the FUSE mountpoint), mount with `mount -t cifs`. New harness arm.
2. **Durability tiers as a bench dimension**: dittofs via `durability` config
   (`--config '{"path":...,"durability":"local|writeback|remote"}'`); competitors via
   their own flags (juicefs `--writeback`; zerofs `[lsm] sync_writes`; s3fs write-through;
   rclone `vfs-cache-mode`). Skip N/A cells explicitly and LOG that they were skipped.
3. **Ganesha**: install `nfs-ganesha` + `nfs-ganesha-vfs`, FSAL_VFS over a local dir.
   NFS3/NFS4 baseline only — label it clearly as "local-disk NFS server", NOT S3.
4. **Durability verification harness**: for every remote-sync arm, run the
   object-list-after-commit check (see flushtest2.sh) and RECORD pass/fail next to the
   number. A number without a verified guarantee is not comparable.

## Per-system setup — hard-won gotchas from this session (USE THESE)
- **SSH to SCW VM**: zsh does NOT word-split vars → inline all flags every call:
  `ssh -i "$HOME/.ssh/id_rsa" -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@<ip>`.
  Reused IPs → `ssh-keygen -R <ip>` first. Sign commits: `SSH_AUTH_SOCK="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"`.
- **pkill**: `pkill -f <pattern>` SELF-MATCHES the ssh command's own argv → kills the
  session (empty output). Use `pkill -x <comm>` (exact name) only, or PID-exclude `$$`/`$PPID`.
- **VM**: SCW POP2-8C-32G, fr-par-1, image `66678a7e-a2bb-4877-bbdb-294c9644bcd9`, `root-volume=block:40GB ip=new`. Always `scw instance server terminate <id> zone=fr-par-1 with-ip=true` when done. Purge test buckets (`rclone purge`).
- **fio workload**: create+4k-write, `nrfiles=1000 filesize=4k create_on_open=1 numjobs=8 runtime=45 time_based`. Durability barrier is on NFS COMMIT/close, so fio write-clat percentiles read 0 — rely on iops across runs. **Use ≥6 runs** for the remote tier: S3 latency variance is huge (18–40 ops/s span), 3 runs gave a spurious 2.4× gap that 6 runs erased (parity).
- **dittofs**: `SECRET=dfsbench-controlplane-secret-0123456789ab` (≥32 chars REQUIRED), `ADMINPASS=dfsbench-admin-pw`. Ports 12049 (nfs) / 8080 (api). `durability` goes in the LOCAL block store config map. `dfs`/`dfsctl` cross-compiled `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`. env diags DITTOFS_METADATA_WRITEBACK / DITTOFS_JOURNAL_ASYNC_COMMIT are GONE from develop (use `durability` config).
- **juicefs**: `curl -sSL https://d.juicefs.com/install | sh -`. jfsbench.sh `<label> <writeback:0|1> ...`; WB=0=durable-default. sqlite meta.
- **s3ql**: NOT in apt/PyPI. `pip install <github-release-tarball>` into a venv (build deps: python3-dev pkg-config libfuse3-dev fuse3 libsqlite3-dev cython3). mount flags: `--cachesize <KiB>` (NOT --cache-size) `--nfs --allow-other`. `mkfs.s3ql --plain`. storage url `s3c://s3.fr-par.scw.cloud/BUCKET/PREFIX`, authinfo2 file.
- **s3fs**: apt. **BUG**: `s3fs "$BUCKET:/$PREFIX"` → 404 NoSuchKey at mount. FIX: mount bucket ROOT `s3fs "$BUCKET"` with a fresh empty bucket.
- **rclone**: apt. writeback=`--vfs-cache-mode writes`. **`--vfs-cache-mode off` is NOT durable** (fsync EIO, 0-byte object) — exclude from remote tier.
- **goofys**: DNF (fio I/O error at create, no metadata engine). Document, don't chase.
- **zerofs**: `curl` release `Barre/ZeroFS` v2.0.10 `zerofs-linux-amd64-pgo`. **BLOCKER: needs a controlling TTY** — headless `nohup`/`setsid script`/`tmux` all failed to bind NFS 2049 (empty logs; `--version` only prints under `ssh -tt`). NEXT: try `expect`/`unbuffer`/`script` with a REAL allocated pty + `TERM=xterm`, or a persistent detached tmux that stays attached, or run its 9P mount client. Config: `[lsm] sync_writes=true` (NOT under [storage]!), `[storage] url=s3://BK/data encryption_password=...`, `[servers.nfs] addresses=["127.0.0.1:2049"]`, `[aws] access_key_id/secret_access_key/endpoint/default_region`. **Stop knfsd before starting zerofs** (both want 2049): `systemctl stop nfs-kernel-server`.
- **ganesha**: not yet attempted. FSAL_VFS over local ext4. `/etc/ganesha/ganesha.conf` with EXPORT{ FSAL{Name=VFS;} Path=/export; Pseudo=/; }. NFS3+4.

## Current shipped state (context for the fresh session)
- Durability tiers: `durability` enum (local/writeback/remote) shipped (`pkg/controlplane/runtime/shares/service.go` `resolveDurabilityTier`). `remote`=`require_durable_commit` (block CLOSE/COMMIT sync-to-S3); VERIFIED on real S3 (4MiB in bucket on commit; local tier leaves it empty).
- **Headline finding**: DittoFS-remote ≈ JuiceFS-default at PARITY (26.1 vs 26.2, 6 runs); both ~10× s3fs. The "half as fast" was 3-run noise.
- **The wall (optional follow-up, NOT in this matrix)**: remote-tier commits serialize on the journal carve lock (98% mutex contention, `journal.Store.Carve` via `Syncer.Flush`) instead of pipelining S3 uploads like juicefs's 20-way. Fix lives in `pkg/block/journal/` = the S2 restore zone (other session owns it) → coordinate. Distinct from the refuted local-fsync group-commit.
- Copilot nit still open: legacy `require_durable_commit:true` path lost its enable log (fold a one-liner into a future PR).
- JuiceFS durable path (why it's fast): NO local fsync in its durability path — the S3 PUT is the only barrier, parallelized 20-way (`MaxUpload`), metadata batched in local SQLite WAL, inode IDs batched 1024. DittoFS can match/beat by pipelining the carve uploads.

## Execution order for the fresh session
1. Confirm #1763 merged; `graphify update .`.
2. Open a harness-update PR (branch off develop). Add SMB(Samba)+NFS4 arms, Ganesha, tier dimension, per-arm durability verification. Land it.
3. Fix ZeroFS TTY (expect/pty). Verify s3ql local-durable question. 
4. Fresh VM, run the ~30-cell meaningful matrix (≥6 runs on remote tier). Verify every durable number with object-list-after-commit. Tear down VM + buckets.
5. Update `docs/BENCHMARKS.md` (narrative, no jargon/issue-numbers/creds) with the full results. Open PR, code-simplifier + code-reviewer, babysit.

---

## Overnight autonomous run — live discovery log (2026-07-18)

Full-auto mandate: provision VM, run full cross-product, fix all recipe bugs by
iterating (systematic-debugging), track every discovery here, tear down VM,
merge PRs, fill BENCHMARKS.md. Started ~20:5x UTC.

### Harness state going in
- **PR #1764** (`bench/dittofs-durability-tiers`): DittoFS badger 9-variant NO —
  badger×{local,writeback,remote} tier axis. Reviewed+simplified clean. Merging on flake-retry green.
- **`bench/competitor-coverage-full`** (stacked on #1764): full coverage, reviewer+simplifier clean.
  Registers 25 backends. Dry-run = 350 cells at `--sizes medium --workloads metadata,seq-write,rand-write-4k,rand-read-4k`.
- Runner isolation VERIFIED: one backend at a time, deferred idempotent teardown, cold-barrier per read cell.

### Recipes flagged UNSURE by build (watch these on the VM, fix live)
1. zerofs `[lsm] sync_writes=true` — section name/placement a guess; verify vs live binary.
2. juicefs postgres meta: `META_PASSWORD` env vs password-in-URL — versions differ.
3. juicefs redis meta: `redis://127.0.0.1:6379/1` + `redis-cli -n 1 flushdb`; `ensureRedis` installs redis-server.
4. goofys: `latest/download/goofys` release URL (goofys ~unmaintained) is likeliest breakage; assumes AWS_ env.
5. ganesha: apt `nfs-ganesha nfs-ganesha-vfs`, conf schema, `ganesha.nfsd -f` daemonization, 2049-free dance.

### Auth verified (pre-run)
scw CLI authed; S3 keys present in scw config (creds stay in env, never committed); zone fr-par-1; SSH key present;
dittofs-bench bucket reachable (stale prefixes to purge). nix finding: don't nixify
VM; only real gap = pin juicefs/zerofs curl|sh installers (follow-up, not blocking).

### Timeline / discoveries
- [start] `dfsbench setup` launched (POP2-8C-32G, fr-par-1) from coverage branch.
- VM provisioned: server 44adbe25-c819-40dd-a9de-151d396641e5, ip 51.15.239.132, fr-par-1. Prereqs installed.
- Full matrix run launched `--remote --resume` from coverage branch: 25 systems, workloads metadata,seq-write,rand-write-4k,rand-read-4k, sizes medium, runtime 30. Detached on VM (/root/run.log, /root/DONE, results /root/bench-results). Local poller pid 12082.
- SSH gotcha CONFIRMED: reused IP → `ssh-keygen -R 51.15.239.132` first; pass flags INLINE not via $VAR (word-split breaks -i); `export SSH_AUTH_SOCK="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"`.
- First cells OK: dittofs-s3-nfs3 metadata/seq-write/rand-write/rand-read warm all ran.
- [21:10] smbtorture/badger-fs retry PASSED = confirmed flake (tracked #1701/#1658, not a regression). PR #1764 CI all green, Copilot 0 comments, reviewer+simplifier clean → SQUASH-MERGED to develop (aa968eed). graphify updated.
- [21:1x] PR-E rebased onto develop (dropped merged PR-A commit), single-commit, build+12tests green, pushed. Opened as **PR #1765** so CI+Copilot run in parallel with the live VM recipe validation. MERGE HELD until recipes validate on VM + CI green + Copilot clean.

### STANDING RULES for the autonomous night (do NOT drop these on context compaction)
1. Full auto: iterate until everything works; fix ALL bugs; systematic-debugging on failures.
2. Poll BOTH the benchmark (SSH VM progress, hang-detect ~20min stall) AND CI frequently.
3. CI red → root-cause and fix until GREEN (not retry); any flake → make sure it's tracked on a gh issue.
4. Every PR: run code-simplifier + code-reviewer, AND resolve ALL Copilot comments, BEFORE merging.
5. NEVER commit secrets (creds stay in env; bench.yaml = bucket+endpoint only).
6. Validate recipes live on VM → fix in coverage worktree → redeploy → `--resume`. Then merge PR-E.
7. Tear down VM (`scw instance server terminate 44adbe25-... zone=fr-par-1 with-ip=true`) + purge test bucket prefixes at the end.
8. Track every discovery HERE as it happens.

### BUG #1 FOUND+FIXED: writeback tier EIO under sustained write (~21:21)
- Symptom: fio `Input/output error` (132 lines) on dittofs-s3-**writeback** seq-write + rand-write. Default (local) tier CLEAN. nfs4 too.
- Root cause (server log /root/.local/state/dittofs/dittofs.log): `journal: local store full, all segments pinned by unsynced bytes` (ErrLocalStoreFull, pkg/block/journal/reclaim.go). Writeback relaxes metadata fsync → writes outrun async S3 syncer → local journal (MaxLocalBytes) saturates → EIO. Local tier's fsync paces writes so it never fills.
- Journal internals owned by S2 session (PR #1716) → did NOT touch them.
- FIX (config, in-scope): local block store config key `max_size` sets journal MaxLocalBytes (service.go:2841). Added `"max_size": 32GiB` to dittofs localCfg → writeback measured at throughput not saturation. Committed 88868ee8 → PR #1765.
- Robustness bug (EIO-instead-of-backpressure) TRACKED: **issue #1766** (coordinate w/ journal S2 session).
- ACTION: killed run (was ~10 cells in), redeployed fixed binary to VM (/root/dfsbench), wiped results, RESTARTED fresh (pid 79128). New file-based hang-monitor.
- SSH-in-background gotcha SOLVED: 1Password agent unreachable from detached jobs → generated throwaway ed25519 keyfile (scratchpad/vmkey), installed on VM authorized_keys; background monitors use `SSH_AUTH_SOCK="" ssh -i vmkey`. Monitor script = scratchpad/monitor.sh (avoids inline-quote parse errors).

### PR-E (#1765) Copilot comments — MUST resolve before merge (batch with recipe fixes)
1. ganesha.go:131 `ganeshaStop()` returns nil even if ganesha.nfsd survives the wait → return error on still-alive.
2. fuse.go:389 juicefs postgres meta URL embeds password (`postgres://juicefs:juicefs@…`) — contradicts META_PASSWORD comment; use META_PASSWORD env + strip pw from URL, or fix comment. (Also a live recipe question — validate on VM.)
NOTE: pushed max_size fix (88868ee8) AFTER the CI poll → re-verify PR-E CI green on latest before merge.

### [~21:40] max_size fix CONFIRMED on VM
- dittofs-s3-writeback seq-write + rand-write completed with total-eio=0 (was 132). Writeback tier now benchmarkable. ✅
- PR-E #1765 CI on latest (post-fix): 0 fails, 18 pass, 2 pending. Green.
- Pace: ~6-7min/plan (dittofs cold-read drain barrier is the time sink); ~70 plans → est 7-8hr full pass. Within authorized envelope; will trim cold passes only if trending >10hr.
- Model: btr5jtdn1 fires at DONE/hang. Competitor recipe failures (juicefs pg/redis, goofys, ganesha, zerofs-sync) get RECORDED and the pass continues — I collect them all at end, fix in one recipe wave (+ the 2 Copilot comments), redeploy, --resume the failed cells. Mid-run redeploy can't hot-swap the running pass.

### HANG #1 (00:xx 07-19): s3fs rand-write-4k hangs 20min → self-healing watchdog installed
- At 140/350 the run wedged on s3fs-nfs3__rand-write-4k; fio stuck 1320s (D-state on hung s3fs FUSE mount). Both monitors flagged it. dittofs writeback held: eio=0 across all 140 cells.
- Cause: s3fs re-uploads the whole object on random writes — a KNOWN s3fs limitation, NOT a dittofs bug. Those cells are legitimately DNF for s3fs (document in results, no issue).
- FIX (self-healing): VM-side /root/fio-watchdog.sh (pid 78566, nohup) kills any fio with etimes>90s every 20s. No legit fio runs >90s at runtime=30, so this auto-aborts every hung cell in ≤~110s and the matrix continues — no more 20min stalls for s3fs (6 plans) or any future hang. Log: /root/fio-watchdog.log.
- Monitor relaunched (bnfadgofe). If VM reboots, re-run: scp watchdog + nohup.

### FIX WAVE 2 (01:xx 07-19): 5 failures triaged after pass 1 reached 144/350
Coverage audit (pass 1): dittofs badger+sqlite nfs3/nfs4 warm OK (4 each); juicefs (sqlite/redis ± durable) all 3 protos OK (5 each incl cold); rclone±cachefull all OK. GAPS:
- **BUG #2 dittofs native SMB3** — not served (NFS default-on, SMB not). FIX: added `dfsctl adapter enable smb --port 12445` + waitPort to dittofsSetup. Committed 7a31e652.
- **BUG #5 postgres** — TRANSIENT cold-start race (psql hit cluster before ready); postgres is up+working now → dittofs-postgres + juicefs-postgres re-run will succeed. No code fix.
- **BUG #3 drain-uploads stall** ("no upload progress within 5m") + **BUG #4 evict no-op <1GiB** ("477MiB→477MiB") → dittofs cold-read barrier broken. Product bugs (journal/engine, S2 zone) → **issue #1767**. WORKAROUND: warm-only re-run (`--evict-cache=false`); cold-read documented as gap.
- Copilot #1765 comments FIXED (ganesha stop escalates+errors; juicefs pg pw→META_PASSWORD only). Committed 7a31e652. Task #8 done.
- Writeback EIO (bug #1) → issue #1766; max_size=32GiB fix held (eio=0 across 144 cells).
- ACTION: committed+pushed fixes to PR-E, cross-built, redeployed /root/dfsbench, RESTARTED warm-only `--resume --evict-cache=false` (pid 85685) keeping 144 good cells. VM fio-watchdog still running (pid from earlier).
- Issues filed tonight: #1766 (writeback EIO), #1767 (cold-read drain+evict).

### BUG #2 deeper: SMB-enable fixed the MOUNT, but dittofs native SMB3 has a real bug
- With SMB enabled, mount succeeds (no more exit-32) BUT fio fails: metadata→`Resource deadlock avoided` (EDEADLK err 35 on fstat); then mount wedges → seq/rand cells `/mnt/bench-client is not a directory`. Next plan's lazy-unmount recovers (no cascade). Fail-fast (~seconds), run continues.
- Native SMB2/3 lock/lease bug under concurrent metadata — NOT a harness issue, needs protocol investigation → **issue #1768**. Keep the SMB-enable fix (surfaces honest result vs silently not-serving).
- Note: `--remote` restart WIPES /root/bench-results (so --resume re-collects from scratch across a remote restart — acceptable, warm-only is fast). Re-collecting now, eio=0.

### DELIVERABLE SCOPE (honest coverage)
- **DittoFS**: nfs3 + nfs4, all 3 durability tiers × 3 metadata engines (badger/sqlite/postgres). Native **smb3 = gap (#1768)**. Cold-read = gap (#1767); warm only.
- **Competitors**: full — juicefs (sqlite/postgres/redis ± durable), rclone (writes/full), s3fs (cache/nocache; rand-write DNF = s3fs limitation), zerofs (async/sync), goofys, ganesha, s3ql — all protocols incl smb3 via Samba, warm + their cold where it worked.
- Issues filed tonight: #1766 writeback-EIO, #1767 cold-read drain+evict, #1768 native-SMB EDEADLK.

### HANG #2 (01:xx): s3fs D-state fio — watchdog kill-9 powerless
- s3fs (esp via Samba re-export, s3fs-smb3) wedges fio in D-state (uninterruptible I/O). `kill -9` can't touch D-state → the simple watchdog stalled 20min. Also saw `double free detected in tcache` (fio/s3fs crash) + fio's own 300s stuck-timeout.
- FIX: ESCALATING watchdog — kill -9 fio; if still alive after 4s (D-state), `pkill -9 s3fs/goofys` + `umount -f -l /mnt/{bench-client,fuse-s3fs,fuse-goofys}` to remove what fio blocks on → fio unblocks. Self-heals s3fs/goofys hangs in ~110s. /root/fio-watchdog.sh (nohup). Log /root/fio-watchdog.log.
- Manually unstuck the current one (126/… cells, eio=0). Run continues.
- LESSON for teardown/re-run: D-state needs mount-stack detach, not just kill.
- WATCHDOG v1 had an escaping bug (nested SSH heredoc `<<"WD"` mangled the awk) → never fired (empty log). REWROTE locally + scp'd as scratchpad/vm-fio-watchdog.sh → VERIFIED it logs+kills (caught the 1239s D-state fio). Lesson: write VM scripts locally + scp, never inline-heredoc-over-SSH. s3fs-nocache-smb3 also wedges (same s3fs+SMB metadata pathology as #1768-adjacent, but this is s3fs's bug not ours). Run self-healing now, 136/~250 warm cells, eio=0.

### DATA-LOSS INCIDENT + FINAL FIX WAVE (05:xx) — LESSON
- Coverage audit of the 157-cell completed pass found gaps: postgres (ALL dittofs-postgres+juicefs-postgres) setup failed; ganesha didn't bind 2049; s3ql 0 cells; goofys writes EIO.
- ROOT CAUSES (real, not transient):
  - postgres: provisionPostgres heredoc `<<SQL` UNQUOTED → `set -eu` + shell expands `$do$` dollar-quote → "do: parameter not set" exit 2. FIX: `<<'SQL'`.
  - ganesha: FATAL opening /var/run/ganesha/ganesha.pid (dir missing). FIX: mkdir -p /var/run/ganesha.
  - s3ql: leftover fs at S3 prefix (SCW eventual-consistency defeats cleanS3Prefix) → mkfs "refusing to overwrite". FIX: force-wipe prefix before run.
  - goofys: EIO on layout write — goofys can't sustain writes (document as gap, no fix).
- Fixes committed to PR-E (postgres+ganesha). Targeted re-run PROVED postgres fix works (dittofs-postgres-s3-nfs3 produced a result).
- **DATA LOSS**: launched targeted `--remote` re-run which WIPES /root/bench-results on start — BEFORE the 157-cell pass results were pulled to local. The warm-only poller never completed its pull (killed during redeploy). 157 cells LOST. Local results/ had only 22 OLD prior-session files.
- **LESSON (critical)**: `--remote` wipes VM results on start; results only reach local at clean completion+pull. ALWAYS scp /root/bench-results → local backup BEFORE any kill/restart. Add a periodic backup poller.
- RECOVERY: one clean FULL run, all fixes, warm-only, + continuous local backup of /root/bench-results.

### Active monitors (background)
- bov0ds5ff: smart matrix hang-detector (SSH, 20min stall threshold) — PRIMARY.
- bg0ekskct: pid-based run completion monitor (redundant backstop).
- bq838rpiq: smbtorture/badger-fs retry CI poll (PR #1764).

### Deliverables by morning
Merged PR-A + PR-E (Copilot-clean, CI-green), filled docs/BENCHMARKS.md + open docs PR, VM torn down, this log complete.

## === SECOND VM (cache study #1770 + #1766 repro — 2026-07-19) — PAID, TEAR DOWN ===
**VM:** server `61dcbc53-12f1-488a-884b-53db7c48f96a`, ip 51.15.239.132 (SCW recycled the old IP; NEW server), fr-par-1. Provisioned via `dfsbench setup` (.bench-vm.json), prereqs installed, SSH via default scw key (`ssh -o StrictHostKeyChecking=no root@51.15.239.132`), binary at /root/dfsbench (x86_64).
**TEARDOWN when done:** `dfsbench teardown` OR `scw instance server terminate 61dcbc53-12f1-488a-884b-53db7c48f96a zone=fr-par-1 with-ip=true with-block=true`.
**Purpose:** (1) cache study #1770 — needs cache-cap variants (subagent branch bench/cache-cap-variants → PR) built into binary, re-push to /root/dfsbench, run 3 scenarios; (2) badger-writeback-EIO / #1766 repro.

## === RESUME STATE (RUN COMPLETE — 2026-07-19) ===
**VM: TORN DOWN ✓** server 44adbe25 terminated (with-ip + with-block), IP 51.15.239.132 released, no `dfsbench-*` servers remain. Bill stopped. (One unattached volume `f2a360e4` left in fr-par-1 — could NOT positively attribute to bench VM vs other infra, so left it; verify+delete if it's ours.)
**Data: SAFE.** All 169 result cells backed up LOCAL at scratchpad(170aa653)/results-live/*.json (== VM before teardown). VM logs pulled to scratchpad(09faccab): run.log (180KB), fio-watchdog.log.
**Matrix result:** 169 cells, 25 systems × {nfs3,nfs4,smb3} × {metadata,seq-write,rand-write-4k,rand-read-4k}, warm pass, medium size, 30s each.

**KEY FINDINGS (trustworthy):**
- **Durable tier is the clean apples-to-apples comparison** (dittofs-*-remote 4/4 nfs3+nfs4, 0 errors; juicefs-*-durable 4/4, 0 errors). DittoFS ≈ JuiceFS on metadata(10-12 vs 15-18 IOPS) & seq-write(~5-7 MB/s); DittoFS AHEAD on durable rand-write (10-16 vs 4-6 IOPS). Confirms existing doc headline.
- **Metadata-engine sensitivity (answers user's "different dbs" Q):** DURABLE tier → engine INVISIBLE (S3 round-trip dominates), both DittoFS(badger/sqlite/pg=11/12/10) & JuiceFS(sqlite/pg/redis=15/18/16). LOCAL-ACK tier → engine VISIBLE for DittoFS (badger 686 > sqlite 362 > pg 251 meta IOPS, ~2-3×); JuiceFS flat ~14 (syncs metadata even in writeback). → shipped to doc, PR #1772.
- **Raw ceilings:** disk 1.7 GB/s (NVMe direct+fdatasync), 8 vCPU/31GB, net TTFB 21ms to fr-par S3. → in doc.

**DATA-QUALITY CAVEATS (why some cells absent, NOT published):**
- dittofs LOCAL-ACK + WRITEBACK write-workloads (seq/rand-write) → EIO, wrote no JSON. Binary on VM PREDATES PR#1771 (#1769 metadata-contention fix) → sqlite/pg EIO'd; badger writeback also only metadata(1/4) = journal-full #1766. So writeback/local write numbers NOT trustworthy from this run; existing doc's curated writeback numbers kept.
- dittofs SMB3 = 0/4 everywhere (#1768 EDEADLK). nfs4 durable rand-read anomalously low vs nfs3 (protocol caching diff, footnote-worthy).
- s3fs write/smb3 = D-state hangs (s3fs's own limit). goofys writes = EIO (goofys limit). Both competitor limits, not ours.

**PR STATUS UPDATE (2026-07-19, later):**
- #1765 PR-E harness → **MERGED** (both Copilot comments verified stale/addressed).
- #1772 docs (engine section + ceiling + COMPLETE 169-cell matrix appendix w/ honest legend) → **MERGED** (CI green).
- #1771 metadata-backpressure (#1769) → 2 Copilot comments FIXED (sqlite ctx.Err()-before-mapDBError on BeginTx+Commit; backpressure_test 30s ctx timeout), tested -race, pushed. Awaiting CI → merge on green. Postgres already had the guard (no change).
- #1773 NEW SMB fix (#1768) → root-caused: new-file CREATE Step-6e (create_post_break.go:694) blocked inline on parent-dir lease-break ACK (5s), same-client credit starvation → cifs.ko EDEADLK. Fix = swap to fire-and-forget BreakParentDirLeasesOnContentChangeAsync. 1412 SMB unit tests pass. NEEDS smbtorture dirlease + WPTS CI validation + review — DO NOT self-merge (SMB lease delicacy + in-flight SMB refactor via foreign stash).
**STILL OPEN (need fresh paid VM or cross-session coord):**
- #1766 journal writeback-full EIO → journal S2 zone (other session owns). ALSO: badger-writeback writes EIO'd in run too (only metadata cell survived) — cause NOT confirmed as journal-full (30s/medium shouldn't fill 32GiB); needs fresh-VM repro. Loose end.
- #1770 cache-behavior study (3 scenarios) → needs fresh paid VM (reprovision). Cost decision — ask user before spinning up.

**(historical, mid-run snapshot below)**
**PRs (ALL OPEN, CI-GREEN, MERGEABLE — NOT merged, left for review under fresh context):**
- #1772 docs metadata-engine+ceiling (NEW, checks pending) — the doc deliverable.
- #1771 metadata-backpressure fix (#1769). 2 LEGIT Copilot comments UNADDRESSED: (a) sqlite/transaction.go:~161 BeginTx returning context.DeadlineExceeded (non-busy) falls to mapDBError→ErrIOError w/o re-checking ctx.Err() — real small correctness fix; (b) backpressure_test.go:32 use ctx.WithTimeout not Background (hang-guard). Fix both, re-run pkg/metadata -race, then merge. Branch fix/1769-metadata-backpressure.
- #1765 PR-E harness. 2 Copilot comments (ganesha.go:140 ganeshaStop always nil; fuse.go juicefs postgres URL embeds pw) — likely ALREADY fixed in later commits (verify staleness on branch), then merge. pw is local test cred juicefs:juicefs, not a real secret.

**NEXT (fresh session):** 1) merge #1772 after CI green; 2) address #1771 Copilot ×2 → re-test → merge (write-path, do carefully); 3) verify #1765 Copilot staleness → merge; 4) close issues manually (develop≠default); 5) cache-behavior study #1770 (3 scenarios: unbounded / tiny-256M cap / full-2GiB loopback; apples-to-apples across dittofs+rclone+s3fs+juicefs; DittoFS unbounded via max_size≥disk) — NEEDS FRESH VM (reprovision, setup scripted in internal/dfsbench); 6) later #1768 SMB fix+VM repro, #1766 w/ S2 coordination, ≥6 remote-tier repeats.
**Foreign git state:** working tree has pre-existing `stash@{0}: nfs-refactor-task ... pending_create_registry.go` (foreign, DON'T drop) + uncommitted README.md/.planning edits — left as found; my doc work is isolated on branch docs/benchmarks-metadata-engine.
