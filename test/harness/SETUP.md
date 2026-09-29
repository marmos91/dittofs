# Setting up the test harness

There are two ways to run the harness ([README.md](README.md)):
- **Container mode (`dtc`).** Needs only Docker Desktop and git, and is the
  recommended way on macOS.
- **Native macOS (`dt`).** Needs the full toolchain below.

Both were set up and verified on Apple Silicon (macOS 27, Docker Desktop with 8 CPUs and
16 GB) at `d562e665`. All paths below are relative to the repo root.

## Container mode: Docker Desktop + git

| Needed | Why |
|---|---|
| Docker Desktop, with about 16 GB RAM and 8 CPUs in Settings → Resources | runs the dev container, the services and the suites' containers |
| git (Xcode Command Line Tools) | the checkout; the hooks call into the container |
| this checkout | `dtc` bind-mounts it at its host path |

Nothing else is needed. `dtc` runs under macOS's stock `/bin/bash` 3.2. The image has:
- Go 1.26.8 and golangci-lint 2.12.2;
- bash 5.2, GNU coreutils and gawk;
- pynfs and pjdfstest;
- `nfs-common`, `cifs-utils`, `smbclient`, `krb5-user`, `rpcbind`, `nfs4-acl-tools`,
  `iptables` and `iproute2`;
- `fio`, `jq`, `xmlstarlet` and shellcheck.

All of it is pinned to the repo's versions.

```bash
test/harness/bin/dtc build      # first time: builds the image (a few minutes)
test/harness/bin/dtc doctor     # every check should say ok
test/harness/bin/dtc unit       # or any other dt command
test/harness/bin/dt hooks install --container   # optional: the hooks, through the container
```

What it leaves on the host. `test/harness/bin/dtc clean` plus `dt cleanup` remove all of
it except the logs:

| Artifact | Where |
|---|---|
| image `dittofs-harness-dev:go1.26` (about 1.3 GB) | Docker |
| volumes `dtc-gomod` (about 0.8 GB), `dtc-gobuild` (about 1.7 GB after a full unit run), `dtc-lint` | Docker |
| `/tmp/dtc` (the e2e `TMPDIR`, emptied after each run) | the host's `/tmp` |
| logs and state | `test/harness/logs`, `test/harness/state` (gitignored) |

The services (Localstack, Postgres, PyKMIP) and their images are pulled on first use.

## Native macOS

### Homebrew packages

```bash
brew install go@1.26 xmlstarlet shellcheck fio gettext bash coreutils samba python@3.12
brew link --force go@1.26        # go@1.26 is keg-only; this puts `go` on PATH
```

| Package | Why |
|---|---|
| `go@1.26` | `go.mod` says `go 1.26.0`, and CI uses 1.26.x |
| `xmlstarlet` | SMB conformance result parsing (TRX) |
| `shellcheck` | the pre-commit hook and CI's shellcheck job |
| `fio` | load generator for `dfsbench` |
| `gettext` | `envsubst` for the SMB conformance templates |
| `bash` | macOS `/bin/bash` is 3.2, and the repo's graders need bash 4 or later (`declare -A`, `mapfile`). With Homebrew first on PATH, `#!/usr/bin/env bash` picks 5.x. When started from 3.2, `dt` re-executes itself with `/opt/homebrew/bin/bash` (the Apple Silicon Homebrew path). |
| `coreutils` | GNU `timeout` for `test/posix/setup-posix.sh`. Exposed only through `test/harness/bin/timeout`; the other GNU tools stay `g`-prefixed. |
| `python@3.12` | the pynfs venv (pynfs breaks on Python 3.13+) |
| `samba` | `smbclient`, which the four `TestSMB3_SmbClient_*` e2e tests need (they skip without it). Create an empty `$(brew --prefix)/etc/smb.conf` containing `[global]`, or smbclient warns on every call. |

**Do not** `brew install go`, `gopls` or `delve`. Each one pulls a newer Go and takes over
`go` from 1.26. Install the Go tools with `go install` instead:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2   # CI's pinned version
go install golang.org/x/tools/gopls@latest                                 # optional
go install github.com/go-delve/delve/cmd/dlv@latest                        # optional
```

PATH (in `~/.bash_profile` or `~/.zshrc`, after the `brew shellenv` line):

```bash
export PATH="$HOME/go/bin:$PATH"
export PATH="<repo>/test/harness/bin:$PATH"   # dt, dtc, pynfs-4.0/4.1, GNU timeout
```

### Docker Desktop

Start the engine (`open -a Docker`) and set about 16 GB RAM and 8 CPUs in Settings →
Resources; the default is about 8 GB. `dt doctor`
reports the resources and whether the images the suites use are present:
- `localstack/localstack:4.13.1` (Compose and CI) and `localstack/localstack:3.0` (the
  e2e fallback);
- `postgres:16-alpine` and `postgres:16`;
- `python:3.11-slim` (PyKMIP);
- `curlimages/curl:8.11.1` (the stack's bootstrap).

`dt posix` and `dt e2e-linux` build their own images on first use.

### pynfs, pinned

pynfs mirrors the Nix flake's pin, in `test/harness/tools/` (gitignored):

```bash
cd test/harness
git clone https://github.com/kofemann/pynfs.git tools/pynfs
git -C tools/pynfs checkout cd4701827a8261fedbfb4c6e39029fb9671321a6   # flake.lock pin
python3.12 -m venv tools/pynfs-venv                                    # pynfs breaks on 3.13+
tools/pynfs-venv/bin/pip install ply setuptools
(cd tools/pynfs && PATH="../pynfs-venv/bin:$PATH" python3 setup.py build)
ls tools/pynfs/nfs4.1/xdrdef/nfs4_const.py tools/pynfs/nfs4.1/xdrdef/nfs4_pack.py   # must exist
```

**Trap:** pynfs's `setup.py` runs each sub-build as `os.system("python3 ...")` and
ignores the result. If the venv is not first on PATH, another Python without setuptools
runs it. The build then prints tracebacks, still exits 0, and produces no generated XDR
modules. Check for the files above.

### Build and hooks

```bash
go mod download && go build ./...
test/harness/bin/dt hooks install       # core.hooksPath -> test/harness/githooks
test/harness/bin/dt doctor              # every check should say ok
```

## Undo

```bash
test/harness/bin/dt cleanup --volumes          # services, stack, SMB stack, e2e-linux, temp dirs
test/harness/bin/dtc clean                     # the dev image and its cache volumes
test/harness/bin/dt hooks uninstall            # back to the repo's .githooks
rm -rf test/harness/logs test/harness/state test/harness/tools
# native macOS only:
brew uninstall go@1.26 xmlstarlet shellcheck fio bash coreutils samba python@3.12
rm -f ~/go/bin/{golangci-lint,gopls,dlv}
# and remove the PATH lines added above
```
