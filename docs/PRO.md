# DittoFS Pro

**DittoFS Pro** is the premium edition of DittoFS. It wraps the open-source
DittoFS server with a modern web dashboard, so administrators and users can
manage their entire deployment — stores, shares, adapters, users, and access
control — visually, without touching the CLI.

Everything `dfsctl` does today is available through the dashboard, plus
license management and white-label branding. It ships as a single Go binary
with the UI embedded, and as a Docker image, and works fully air-gapped with an
offline license.

Learn more at [dittofs.io](https://dittofs.io).

## Dashboard

Manage your entire DittoFS deployment from the browser.

![DittoFS Pro dashboard](assets/pro/dashboard.png)

<table>
  <tr>
    <td width="50%"><img src="assets/pro/shares.png" alt="Shares" /><br /><sub><b>Shares</b> — connect a block store and a metadata store into a virtual filesystem.</sub></td>
    <td width="50%"><img src="assets/pro/block-stores.png" alt="Block stores" /><br /><sub><b>Block stores</b> — local (filesystem/memory) and remote (S3) backends.</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="assets/pro/metadata-stores.png" alt="Metadata stores" /><br /><sub><b>Metadata stores</b> — Badger, PostgreSQL, or Memory.</sub></td>
    <td width="50%"><img src="assets/pro/share-mount.png" alt="Mount instructions" /><br /><sub><b>Mount instructions</b> — per-share CLI and native NFS/SMB commands.</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="assets/pro/nfs-adapter.png" alt="NFS adapter settings" /><br /><sub><b>Adapters</b> — tune NFS/SMB protocol and performance knobs.</sub></td>
    <td width="50%"><img src="assets/pro/users.png" alt="Users" /><br /><sub><b>Access control</b> — users, roles, and groups.</sub></td>
  </tr>
</table>

## How it relates to open-source DittoFS

DittoFS Pro builds **on top of** this open-source server — it imports DittoFS as
a Go module and talks to the same control-plane and auth endpoints. The core
filesystem, protocols, stores, and adapters documented in this repository are
identical; Pro adds the dashboard, licensing, and branding layer around them.

New control-plane APIs introduced for Pro are additive and remain backward
compatible with existing DittoFS deployments.

## Roadmap

DittoFS Pro is actively expanding toward enterprise deployments. Planned
additions include:

- **Monitoring dashboard** — live metrics, throughput, and health across stores,
  shares, and adapters.
- **Enterprise-grade features** — capabilities aimed at large, multi-tenant, and
  regulated environments.
- **Enterprise support** — SLAs and direct support for production deployments.

See [dittofs.io](https://dittofs.io) for the latest.
