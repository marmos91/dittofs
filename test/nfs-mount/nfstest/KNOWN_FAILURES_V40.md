# nfstest known failures — NFSv4.0 only

Failures expected under the `v4.0` option set and no other, graded together with `KNOWN_FAILURES.md` and `KNOWN_FAILURES_V4.md`. A row belongs here, not in the v4 overlay, when the same failure on a v4.1 set would be a regression.

CI grades nfstest with [`known.yaml`](known.yaml). This table is the knfsd baseline's: [`baseline-knfsd.sh`](baseline-knfsd.sh) grades knfsd's runs against it through `test/common/known-failures.sh`, says which tables grade which option set, and reports which of knfsd's failures these rows cover.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
