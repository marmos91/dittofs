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

## Delete a Tag

```bash
git tag -d v0.1.0
git push --delete origin v0.1.0
# Delete GitHub release manually from web UI
```
