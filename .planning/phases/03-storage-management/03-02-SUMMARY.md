# Phase 3 Plan 02: S3 Credentials Secret Reference Summary

S3 credentials from Kubernetes Secrets injected as AWS SDK environment variables

## Metadata

| Field | Value |
|-------|-------|
| Phase | 03-storage-management |
| Plan | 02 |
| Completed | 2026-02-05 |
| Duration | 2 min |

## What Was Built

### S3 Credentials Secret Reference Support

Added CRD types and controller logic to inject S3 credentials from Kubernetes Secrets into the DittoFS pod as environment variables. This enables users to configure S3-compatible payload stores (Cubbit DS3, AWS S3) without hardcoding credentials.

**Key Features:**
- S3CredentialsSecretRef type with customizable key names (accessKeyId, secretAccessKey, endpoint)
- S3StoreConfig type with region, bucket, and credentialsSecretRef fields
- S3 field in DittoServerSpec
- Environment variables: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_ENDPOINT_URL (optional), AWS_REGION
- S3 Secret included in config hash for automatic pod restart on credential change

## Tasks Completed

| # | Task | Commit | Key Changes |
|---|------|--------|-------------|
| 1 | Add S3 credential types to CRD | 70b7c76 | dittoserver_types.go, CRD manifest |
| 2 | Inject S3 credentials as env vars | aa21cc6 | buildS3EnvVars helper, container Env field |
| 3 | Include S3 Secret in config hash | b53c10d | collectSecretData updated |
| 4 | Create sample S3 Secret and CR | af0f3ad | dittofs_s3_secret.yaml, dittofs_v1alpha1_dittofs_s3.yaml |

## Key Files Modified

| File | Purpose |
|------|---------|
| k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go | S3CredentialsSecretRef, S3StoreConfig types, S3 field |
| k8s/dittofs-operator/api/v1alpha1/zz_generated.deepcopy.go | Auto-generated deepcopy |
| k8s/dittofs-operator/internal/controller/dittoserver_controller.go | buildS3EnvVars, Env wiring, collectSecretData |
| k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml | CRD with s3 section |
| k8s/dittofs-operator/config/samples/dittofs_s3_secret.yaml | Sample S3 credentials Secret |
| k8s/dittofs-operator/config/samples/dittofs_v1alpha1_dittofs_s3.yaml | Sample DittoServer CR using S3 |

## Decisions Made

| Decision | Rationale |
|----------|-----------|
| AWS_ENDPOINT_URL is optional | AWS S3 doesn't need endpoint; Cubbit DS3 does |
| Include all S3 secret keys in hash | Any credential change triggers pod restart |
| Customizable key names with defaults | Flexibility for different Secret layouts |

## Deviations from Plan

None - plan executed exactly as written.

## Verification Results

All verification checks passed:
- CRD schema has s3 section with credentialsSecretRef, region, bucket
- Build succeeds
- Tests pass
- All AWS env vars present (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_ENDPOINT_URL, AWS_REGION)
- buildS3EnvVars wired to container Env field

## Usage Example

```yaml
# Secret with S3 credentials
apiVersion: v1
kind: Secret
metadata:
  name: dittofs-s3-credentials
type: Opaque
stringData:
  accessKeyId: "AKIA..."
  secretAccessKey: "..."
  endpoint: "https://s3.cubbit.eu"  # Optional for AWS S3

---
# DittoServer using S3
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs-s3
spec:
  s3:
    credentialsSecretRef:
      secretName: dittofs-s3-credentials
    region: "eu-west-1"
    bucket: "dittofs-data"
```

## Next Phase Readiness

Ready to proceed with Plan 03-03 (Percona reference in CRD).

**Prerequisites satisfied:**
- S3 credentials can be injected from Secrets
- Config hash includes S3 credentials for pod restart
- Sample manifests available for testing
