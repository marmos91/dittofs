#!/bin/sh
# Publish APT and YUM repositories to S3.
# Called from CI after GoReleaser produces .deb and .rpm artifacts in dist/.
#
# Required env vars:
#   AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (S3 credentials)
#
# Optional env vars:
#   GPG_PRIVATE_KEY — if set, APT Release file is GPG-signed (InRelease)
#   S3_BUCKET       — defaults to dittofs-binaries
#   S3_ENDPOINT     — defaults to https://s3.cubbit.eu
set -e

S3_BUCKET="${S3_BUCKET:-dittofs-binaries}"
S3_ENDPOINT="${S3_ENDPOINT:-https://s3.cubbit.eu}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

s3_sync() {
  aws s3 sync "$1" "s3://${S3_BUCKET}/$2" \
    --endpoint-url "$S3_ENDPOINT" \
    --acl public-read
}

# ── APT repository ──────────────────────────────────────────────────────────

APT_DIR="${WORK_DIR}/apt"
mkdir -p "${APT_DIR}/pool" "${APT_DIR}/dists/stable/main/binary-amd64" "${APT_DIR}/dists/stable/main/binary-arm64"

# Copy .deb artifacts into pool
cp dist/*.deb "${APT_DIR}/pool/" 2>/dev/null || true

DEB_COUNT=$(find "${APT_DIR}/pool" -name '*.deb' | wc -l)
if [ "$DEB_COUNT" -eq 0 ]; then
  echo "No .deb files found in dist/, skipping APT repo"
else
  echo "Building APT repo with ${DEB_COUNT} packages..."

  # Generate Packages files per architecture
  cd "${APT_DIR}"
  for arch in amd64 arm64; do
    dpkg-scanpackages --arch "$arch" pool > "dists/stable/main/binary-${arch}/Packages"
    gzip -k "dists/stable/main/binary-${arch}/Packages"
  done

  # Generate Release file
  cd "${APT_DIR}/dists/stable"
  cat > Release <<RELEASE
Origin: DittoFS
Label: DittoFS
Suite: stable
Codename: stable
Date: $(LC_ALL=C date -Ru)
Architectures: amd64 arm64
Components: main
Description: DittoFS APT repository
RELEASE

  # Append checksums
  {
    echo "SHA256:"
    find main -type f \( -name 'Packages' -o -name 'Packages.gz' \) | sort | while read -r f; do
      size=$(wc -c < "$f" | tr -d ' ')
      hash=$(sha256sum "$f" | awk '{print $1}')
      printf ' %s %s %s\n' "$hash" "$size" "$f"
    done
  } >> Release

  # GPG sign if key is available (isolated keyring)
  if [ -n "${GPG_PRIVATE_KEY:-}" ]; then
    if ! (
      set -e
      GNUPGHOME="${WORK_DIR}/gnupg"
      export GNUPGHOME
      mkdir -p "${GNUPGHOME}"
      chmod 700 "${GNUPGHOME}"
      printf '%s\n' "$GPG_PRIVATE_KEY" | gpg --batch --import
      KEY_FP=$(gpg --batch --with-colons --list-keys 2>/dev/null | awk -F: '/^fpr/{print $10; exit}')
      gpg --batch --yes --armor --detach-sign -o Release.gpg Release
      gpg --batch --yes --armor --clearsign -o InRelease Release
      gpg --batch --yes --armor --export "$KEY_FP" > "${APT_DIR}/dittofs.gpg.key"
    ); then
      echo "ERROR: GPG signing failed" >&2
      exit 1
    fi
  fi

  cd "${WORK_DIR}"
  echo "Uploading APT repo to s3://${S3_BUCKET}/apt/ ..."
  s3_sync "${APT_DIR}/" "apt/"
  echo "APT repo published."
fi

# ── YUM repository ──────────────────────────────────────────────────────────

RPM_DIR="${WORK_DIR}/rpm"
mkdir -p "${RPM_DIR}/packages"

# Copy .rpm artifacts
cp dist/*.rpm "${RPM_DIR}/packages/" 2>/dev/null || true

RPM_COUNT=$(find "${RPM_DIR}/packages" -name '*.rpm' | wc -l)
if [ "$RPM_COUNT" -eq 0 ]; then
  echo "No .rpm files found in dist/, skipping YUM repo"
else
  echo "Building YUM repo with ${RPM_COUNT} packages..."

  createrepo_c "${RPM_DIR}" 2>/dev/null || createrepo "${RPM_DIR}"

  # Generate .repo file for easy user setup
  cat > "${RPM_DIR}/dfs.repo" <<REPO
[dfs]
name=DittoFS
baseurl=${S3_ENDPOINT}/${S3_BUCKET}/rpm
enabled=1
gpgcheck=0
REPO

  echo "Uploading YUM repo to s3://${S3_BUCKET}/rpm/ ..."
  s3_sync "${RPM_DIR}/" "rpm/"
  echo "YUM repo published."
fi

echo "Done."
