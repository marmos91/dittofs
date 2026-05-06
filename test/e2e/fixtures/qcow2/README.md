# qcow2 Test Fixtures

Phase 13 DEDUP-03 (VER-03 gate) uses a pinned qcow2 base image plus
deterministic synthetic clones to verify >=40% storage reduction on a
representative VM-fleet workload.

## Pinned Base Image

- URL: `https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/cloud/nocloud_alpine-3.20.3-x86_64-bios-cloudinit-r0.qcow2`
- SHA256: `<freeze-at-first-run>` — first nightly run of `TestDEDUP03_VMFleet40Pct`
  logs the actual digest; commit it back into `qcow2BaseSHA256` in
  `test/e2e/framework/qcow2_fixture.go` and re-run to verify.
- Cached at: `test/e2e/fixtures/qcow2/base.qcow2` (gitignored — see
  the root `.gitignore` rule for `test/e2e/fixtures/qcow2/*.qcow2`).

The Alpine cloud image is ~50 MiB compressed and downloads once per CI
host; subsequent runs use the cached copy after a SHA256 check. If the
upstream image rotates, the SHA mismatch fails the test loudly (T-13-20
mitigation) — refresh the pinned `URL` + `SHA256` pair via an explicit
plan + commit.

## Clone Synthesis

Clones are synthesized at runtime from the base by overlaying small
random byte patches at seeded offsets. Each clone diverges from the
base by ~7.5% of base length (50-200 patches of 4-32 KiB each).

The seed for clone `i` is `int64(i) ^ 0xDEDD0F` so clones are reproducible
across runs and CI nodes but distinct from one another. See
`framework.SynthesizeClones` for the generator.

## Why This Fixture

- **Real qcow2 content**: file-system journal headers, BIOS images,
  cloud-init artifacts — representative of what FastCDC chunks in
  production VM workloads.
- **Deterministic**: seeded RNG produces identical clones across CI
  runs, so the dedup ratio is reproducible.
- **Cheap**: ~50 MiB base + ~7.5% divergence per clone keeps the run
  under one minute on local hardware. Nightly tier accepts the cost
  per Phase 13 D-15.

## Tier

This fixture runs only when `DITTOFS_E2E_NIGHTLY=1` is set, alongside
the existing nightly gates that require sudo + kernel NFS client +
Localstack + Postgres. It is NOT exercised in PR CI.
