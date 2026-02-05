---
phase: 06-documentation
plan: 01
subsystem: operator-docs
tags: [helm, crd, documentation, kubernetes, operator]

dependency-graph:
  requires:
    - 05-03 (observability complete - all CRD fields finalized)
  provides:
    - Helm chart for operator installation
    - Complete CRD field reference documentation
    - Installation guide with kubectl and Helm methods
  affects:
    - 06-02 (can link to CRD_REFERENCE.md from README)
    - 06-03 (can reference INSTALL.md for setup)

tech-stack:
  added:
    - helmify (Helm chart generation from Kustomize)
  patterns:
    - Helm CRD in crds/ directory (no templating)
    - make helm target for regeneration

key-files:
  created:
    - k8s/dittofs-operator/chart/Chart.yaml
    - k8s/dittofs-operator/chart/values.yaml
    - k8s/dittofs-operator/chart/templates/_helpers.tpl
    - k8s/dittofs-operator/chart/templates/deployment.yaml
    - k8s/dittofs-operator/chart/templates/serviceaccount.yaml
    - k8s/dittofs-operator/chart/templates/*.yaml (11 files)
    - k8s/dittofs-operator/chart/crds/dittoservers.yaml
    - k8s/dittofs-operator/docs/CRD_REFERENCE.md
    - k8s/dittofs-operator/docs/INSTALL.md
  modified:
    - k8s/dittofs-operator/Makefile

decisions:
  - id: helm-crds-no-templating
    choice: "Use crds/ directory with raw CRD (no templating)"
    reason: "Helm crds/ directory doesn't support Go templating, copy raw CRD from config/crd/bases"

  - id: helmify-generation
    choice: "Generate chart from Kustomize using helmify"
    reason: "Maintains consistency between Kustomize and Helm deployments"

  - id: make-helm-regenerate
    choice: "Add make helm target that regenerates chart"
    reason: "Allows easy updates when Kustomize manifests change"

metrics:
  duration: "5 min"
  completed: "2026-02-05"
---

# Phase 6 Plan 1: Documentation and Helm Chart Summary

Helm chart generation from Kustomize manifests using helmify, with CRD reference and installation documentation.

## One-liner

Generated Helm chart from Kustomize using helmify with CRD reference (674 lines) and INSTALL.md covering kubectl/Helm methods.

## What Was Built

### Task 1: Helm Chart with helmify Generation

**Generated chart structure:**
```
k8s/dittofs-operator/chart/
  Chart.yaml          - Chart metadata (name, version, maintainers)
  values.yaml         - Configurable values (image, resources, replicas)
  .helmignore         - Helm ignore patterns
  templates/
    _helpers.tpl      - Template helper functions
    deployment.yaml   - Operator deployment
    serviceaccount.yaml
    manager-rbac.yaml
    leader-election-rbac.yaml
    metrics-service.yaml
    dittoserver-*-rbac.yaml (admin, editor, viewer)
  crds/
    dittoservers.yaml - Raw CRD (no templating)
```

**Makefile targets added:**
- `make helm` - Regenerate chart from Kustomize
- `make helm-lint` - Lint the chart
- `make helm-template` - Render templates for debugging
- `make helmify` - Download helmify tool

### Task 2: CRD_REFERENCE.md

Complete API reference documenting all DittoServer fields:

**Coverage:**
- 674 lines of documentation
- All spec fields with types, defaults, validation rules
- All status fields and 5 conditions (Ready, Available, Progressing, ConfigReady, DatabaseReady)
- Architecture diagram (Mermaid) showing CR to managed resources
- 4 complete CR examples:
  1. Minimal (memory stores)
  2. Production with S3
  3. Production with Percona PostgreSQL
  4. With SMB protocol enabled

**Field sections documented:**
- Core fields (image, replicas, resources, security)
- Storage configuration (metadataSize, cacheSize, contentSize, storageClassName)
- Database configuration (type, sqlite, postgresSecretRef)
- Cache configuration (path, size)
- Service configuration (type, annotations)
- NFS port
- SMB configuration (enabled, port, timeouts, credits)
- Metrics configuration
- Control plane configuration
- Identity configuration (jwt, admin)
- S3 configuration (credentialsSecretRef, region, bucket)
- Percona PostgreSQL (enabled, replicas, backup)

### Task 3: INSTALL.md

Installation guide covering:

**Prerequisites section:**
- Kubernetes 1.25+ requirement
- kubectl/helm version checks
- StorageClass verification
- Optional Percona Operator note

**Installation methods:**
1. kubectl (recommended) - CRD-first order
2. Helm - local chart with custom values examples

**Additional sections:**
- Verification commands
- Quick start (deploy first DittoServer)
- NFS client configuration examples
- Uninstallation (both methods)
- Upgrade notes (Helm CRD limitation)
- Troubleshooting guide

## Commits

| Hash | Message |
|------|---------|
| d7aea28 | feat(06-01): add Helm chart with helmify generation |
| a6b128a | docs(06-01): add comprehensive CRD_REFERENCE.md |
| 8af829a | docs(06-01): add comprehensive INSTALL.md guide |

## Verification Results

```bash
# Chart structure exists
ls chart/Chart.yaml chart/values.yaml chart/templates/ chart/crds/
# All present

# Helm lint passes
helm lint ./chart
# 1 chart(s) linted, 0 chart(s) failed

# Helm template renders
helm template test ./chart > /dev/null
# Success

# Makefile has helm target
make help | grep helm
# helm target present

# CRD_REFERENCE.md complete
wc -l docs/CRD_REFERENCE.md
# 674 lines

# INSTALL.md has both methods
grep -c "kubectl apply\|helm install" docs/INSTALL.md
# 9 occurrences
```

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] CRD in crds/ directory needs raw format**

- **Found during:** Task 1
- **Issue:** helmify generates templated CRD in templates/, but Helm crds/ directory doesn't support Go templating
- **Fix:** Copy raw CRD from config/crd/bases to crds/, remove templated version from templates/
- **Files modified:** chart/crds/dittoservers.yaml, Makefile (helm target includes copy step)

**2. [Rule 3 - Blocking] helmify not in PATH**

- **Found during:** Task 1
- **Issue:** helmify installed to GOBIN but not in shell PATH
- **Fix:** Use full path to helmify binary, add helmify target to Makefile for managed installation
- **Files modified:** Makefile

## Next Phase Readiness

Phase 6 Plan 1 complete. Ready for:
- Plan 06-02: PERCONA.md, TROUBLESHOOTING.md, README updates
- Plan 06-03: Architecture documentation

## Files Changed

| File | Change Type | Lines |
|------|-------------|-------|
| k8s/dittofs-operator/Makefile | Modified | +28 |
| k8s/dittofs-operator/chart/* | Created | +1682 |
| k8s/dittofs-operator/docs/CRD_REFERENCE.md | Created | +674 |
| k8s/dittofs-operator/docs/INSTALL.md | Created | +390 |
