# Release Process

DittoFS uses [Semantic Versioning](https://semver.org/) and automated releases via GoReleaser.

## Branching Strategy

- **`develop`** - Active development (all CI runs here)
- **`main`** - Release-ready only (merge from develop when ready to release)

## Creating a Release

1. **Ensure CI is green** on `develop` branch

2. **Merge develop into main**:
   ```bash
   git checkout main
   git pull origin main
   git merge develop
   git push origin main
   ```

3. **Create and push a tag**:
   ```bash
   git tag -a v0.1.0 -m "Release v0.1.0"
   git push origin v0.1.0
   ```

4. **GitHub Actions automatically**:
   - Runs tests
   - Builds binaries for Linux, macOS, Windows (amd64, arm64, arm)
   - Generates checksums
   - Creates GitHub Release with artifacts

5. **Verify** at https://github.com/marmos91/dittofs/releases

## Versioning

- `v0.x.y` - Pre-1.0 experimental (x = minor features, y = patches, breaking changes allowed)
- `v1.0.0` - First stable release
- `v1.x.0` - Minor version (new features, backward compatible)
- `v1.x.y` - Patch version (bug fixes only)
- `v2.0.0` - Major version (breaking changes)
- `v1.2.3-beta.1` - Pre-release (auto-marked on GitHub)

## Testing Locally

```bash
goreleaser release --snapshot --clean
ls -la dist/
```

## Hotfix

```bash
git checkout -b hotfix/v1.2.4 v1.2.3
# Make fixes
git commit -am "fix: critical issue"
git tag -a v1.2.4 -m "Hotfix v1.2.4"
git push origin v1.2.4
git checkout main
git merge hotfix/v1.2.4
git push origin main
```

## Homebrew Tap

Releases automatically publish Homebrew formulae to [`marmos91/homebrew-tap`](https://github.com/marmos91/homebrew-tap). Users can install via:

```bash
brew tap marmos91/tap
brew install marmos91/tap/dfs      # Server daemon
brew install marmos91/tap/dfsctl   # Client CLI
```

### How It Works

GoReleaser's `brews` section generates Ruby formula files and pushes them to the tap repository on each non-prerelease tag push. The `skip_upload: auto` setting prevents prerelease versions (e.g., `v1.0.0-beta.1`) from being published to the tap.

Each formula:
- Downloads the correct archive for the user's OS/architecture
- Installs the binary to `$(brew --prefix)/bin`
- Generates shell completions (bash, zsh, fish) via `generate_completions_from_executable`

### Prerequisites

1. **Tap repository**: `marmos91/homebrew-tap` must exist on GitHub with a `Formula/` directory
2. **Personal Access Token**: A fine-grained token scoped to `marmos91/homebrew-tap` with Contents read+write permission, stored as `HOMEBREW_TAP_TOKEN` in the `marmos91/dittofs` repository secrets

### Local Testing

Test the full release pipeline locally with `--snapshot` (does not publish anything):

```bash
# Set a dummy token for local testing
export HOMEBREW_TAP_TOKEN=dummy

goreleaser release --snapshot --clean

# Verify archives
ls dist/dfs_*
ls dist/dfsctl_*

# Verify generated formulae
cat dist/homebrew/Formula/dfs.rb
cat dist/homebrew/Formula/dfsctl.rb
```

### Token Rotation

If the `HOMEBREW_TAP_TOKEN` needs to be rotated:

1. Create a new fine-grained PAT at https://github.com/settings/tokens scoped to `marmos91/homebrew-tap` (Contents: read+write)
2. Update the `HOMEBREW_TAP_TOKEN` secret in `marmos91/dittofs` repository settings
3. The old token is invalidated automatically when the new PAT is created with the same name

## Scoop Bucket (Windows)

Releases automatically publish Scoop manifests to [`marmos91/scoop-bucket`](https://github.com/marmos91/scoop-bucket). Users can install via:

```powershell
scoop bucket add dittofs https://github.com/marmos91/scoop-bucket
scoop install dfs       # Server daemon
scoop install dfsctl    # Client CLI
```

### How It Works

GoReleaser's `scoops` section generates JSON manifest files and pushes them to the bucket repository on each non-prerelease tag push. The `skip_upload: auto` setting prevents prerelease versions from being published.

### Prerequisites

1. **Bucket repository**: `marmos91/scoop-bucket` must exist on GitHub
2. **Personal Access Token**: A fine-grained token scoped to `marmos91/scoop-bucket` with Contents read+write permission, stored as `SCOOP_BUCKET_TOKEN` in the `marmos91/dittofs` repository secrets

### Local Testing

```bash
# Set a dummy token for local testing
export SCOOP_BUCKET_TOKEN=dummy

goreleaser release --snapshot --clean

# Verify generated manifests
cat dist/scoop/dfs.json
cat dist/scoop/dfsctl.json
```

### Token Rotation

If the `SCOOP_BUCKET_TOKEN` needs to be rotated:

1. Create a new fine-grained PAT at https://github.com/settings/tokens scoped to `marmos91/scoop-bucket` (Contents: read+write)
2. Update the `SCOOP_BUCKET_TOKEN` secret in `marmos91/dittofs` repository settings
3. The old token is invalidated automatically when the new PAT is created with the same name

## Delete a Tag

```bash
git tag -d v0.1.0
git push --delete origin v0.1.0
# Delete GitHub release manually from web UI
```
