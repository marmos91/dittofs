{
  description = "DittoFS - Modular virtual filesystem with pluggable storage backends";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    # Original C-based pjdfstest (used by JuiceFS, FreeBSD, and others)
    # This is the authoritative POSIX compliance test suite
    pjdfstest-src = {
      url = "github:pjd/pjdfstest";
      flake = false;
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      pjdfstest-src,
    }:
    let
      # Version configuration - update this for releases
      version = "0.5.1";
    in
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # Git revision for build info (use "dirty" if uncommitted changes)
        gitRev = self.shortRev or self.dirtyShortRev or "unknown";

        # Helper scripts for DittoFS development (works in any shell)
        dfs-mount = pkgs.writeShellScriptBin "dfs-mount" ''
          mount_point="''${1:-/tmp/dittofs-test}"
          sudo mkdir -p "$mount_point" 2>/dev/null || true
          sudo ${pkgs.nfs-utils}/bin/mount.nfs -o nfsvers=3,tcp,port=12049,mountport=12049,nolock,actimeo=0 \
            localhost:/export "$mount_point"
          echo "Mounted at $mount_point"
        '';

        dfs-umount = pkgs.writeShellScriptBin "dfs-umount" ''
          mount_point="''${1:-/tmp/dittofs-test}"
          sudo ${pkgs.util-linux}/bin/umount "$mount_point"
          echo "Unmounted $mount_point"
        '';

        # Original C-based pjdfstest (used by JuiceFS, FreeBSD, and others)
        # This is the authoritative POSIX compliance test suite with 8,789 tests
        pjdfstest = pkgs.stdenv.mkDerivation {
          pname = "pjdfstest";
          version = "2025-01-07";
          src = pjdfstest-src;

          nativeBuildInputs = with pkgs; [
            autoconf
            automake
            pkg-config
          ];

          buildInputs = with pkgs; [
            acl
          ];

          # Build the pjdfstest binary
          buildPhase = ''
            autoreconf -ifs
            ./configure --prefix=$out
            make pjdfstest
          '';

          # Install binary and tests
          # Note: binary must be in share/pjdfstest/ because misc.sh walks up
          # from tests/ looking for it in parent directories
          installPhase = ''
            mkdir -p $out/bin $out/share/pjdfstest
            cp pjdfstest $out/bin/
            cp pjdfstest $out/share/pjdfstest/
            cp -r tests $out/share/pjdfstest/
          '';

          meta = with pkgs.lib; {
            description = "POSIX filesystem test suite (C version, used by JuiceFS/FreeBSD)";
            homepage = "https://github.com/pjd/pjdfstest";
            license = licenses.bsd2;
            platforms = platforms.linux;
          };
        };

        # Helper script to start PostgreSQL for testing
        # Uses sudo for docker commands to avoid docker group requirement
        dfs-postgres-start = pkgs.writeShellScriptBin "dfs-postgres-start" ''
          container_name="dittofs-postgres-test"

          # Check if docker is available
          if ! command -v docker &>/dev/null; then
            echo "Error: docker not found in PATH."
            exit 1
          fi

          # Check if docker daemon is running (using sudo)
          if ! sudo docker info &>/dev/null; then
            echo "Error: Cannot connect to Docker daemon."
            echo "Make sure Docker daemon is running: sudo systemctl start docker"
            exit 1
          fi

          # Check if container already exists
          if sudo docker ps -a --format '{{.Names}}' | grep -q "^$container_name$"; then
            # Check if it's running
            if sudo docker ps --format '{{.Names}}' | grep -q "^$container_name$"; then
              echo "PostgreSQL container already running"
              exit 0
            else
              echo "Starting existing PostgreSQL container..."
              sudo docker start "$container_name"
            fi
          else
            echo "Creating PostgreSQL container for DittoFS testing..."
            sudo docker run -d \
              --name "$container_name" \
              -e POSTGRES_USER=dittofs \
              -e POSTGRES_PASSWORD=dittofs \
              -e POSTGRES_DB=dittofs_test \
              -p 5432:5432 \
              postgres:16-alpine
          fi

          echo "Waiting for PostgreSQL to be ready..."
          for i in $(seq 1 30); do
            if sudo docker exec "$container_name" pg_isready -U dittofs -d dittofs_test &>/dev/null; then
              echo "PostgreSQL is ready!"
              echo ""
              echo "Connection details:"
              echo "  Host:     localhost"
              echo "  Port:     5432"
              echo "  User:     dittofs"
              echo "  Password: dittofs"
              echo "  Database: dittofs_test"
              echo ""
              echo "Start DittoFS with: ./dfs start --config test/posix/configs/config-postgres.yaml"
              exit 0
            fi
            sleep 1
          done

          echo "Error: PostgreSQL failed to start within 30 seconds"
          exit 1
        '';

        # Helper script to stop PostgreSQL test container
        # Uses sudo for docker commands to avoid docker group requirement
        dfs-postgres-stop = pkgs.writeShellScriptBin "dfs-postgres-stop" ''
          container_name="dittofs-postgres-test"

          if sudo docker ps -a --format '{{.Names}}' | grep -q "^$container_name$"; then
            echo "Stopping PostgreSQL container..."
            sudo docker stop "$container_name" 2>/dev/null || true
            echo "Removing PostgreSQL container..."
            sudo docker rm "$container_name" 2>/dev/null || true
            echo "PostgreSQL container removed"
          else
            echo "PostgreSQL container not found"
          fi

          # Also clean up content store
          if [ -d "/tmp/dittofs-content-postgres" ]; then
            echo "Cleaning up content store..."
            sudo rm -rf /tmp/dittofs-content-postgres
          fi

          echo "Cleanup complete"
        '';

        # Helper script for running e2e tests with sudo
        dfs-e2e = pkgs.writeShellScriptBin "dfs-e2e" ''
          # E2E tests require sudo for NFS mounting
          # This script preserves the nix shell's PATH for go binary access

          # Get the directory of the dittofs project (assume we're in it)
          PROJECT_DIR="$(pwd)"

          # Verify we're in the right directory
          if [[ ! -f "$PROJECT_DIR/go.mod" ]] || ! grep -q "dittofs" "$PROJECT_DIR/go.mod"; then
            echo "Error: Must run from dittofs project root"
            exit 1
          fi

          echo "Running DittoFS E2E tests..."
          echo "Project: $PROJECT_DIR"
          echo ""

          # Build first (without sudo)
          echo "Building dfs..."
          go build -o "$PROJECT_DIR/dfs" "$PROJECT_DIR/cmd/dfs/main.go" || exit 1
          echo ""

          # Run e2e tests with sudo, preserving PATH for go binary
          # Also preserve GOPATH, GOMODCACHE, GOCACHE for go test to work
          if [[ $# -gt 0 ]]; then
            # Run specific test pattern
            sudo env \
              PATH="$PATH" \
              GOPATH="$GOPATH" \
              GOMODCACHE="$GOMODCACHE" \
              GOCACHE="$GOCACHE" \
              HOME="$HOME" \
              go test -tags=e2e -v -timeout 30m "$@" ./test/e2e/...
          else
            # Run all e2e tests
            sudo env \
              PATH="$PATH" \
              GOPATH="$GOPATH" \
              GOMODCACHE="$GOMODCACHE" \
              GOCACHE="$GOCACHE" \
              HOME="$HOME" \
              go test -tags=e2e -v -timeout 30m ./test/e2e/...
          fi
        '';

        # Helper script for running pjdfstest
        dfs-posix = pkgs.writeShellScriptBin "dfs-posix" ''
          mount_point="''${DITTOFS_MOUNT:-/tmp/dittofs-test}"

          # Check if mounted using /proc/mounts (more reliable than mountpoint for NFS)
          if ! grep -q " $mount_point " /proc/mounts 2>/dev/null; then
            echo "Error: $mount_point not mounted. Run: dfs-mount"
            exit 1
          fi

          cd "$mount_point"

          # Set up pjdfstest binary path for the tests
          export PATH="${pjdfstest}/bin:$PATH"

          tests_dir="${pjdfstest}/share/pjdfstest/tests"

          echo "Running pjdfstest POSIX compliance suite..."
          echo "Mount point: $mount_point"
          echo "Tests directory: $tests_dir"
          echo ""

          if [[ $# -gt 0 ]]; then
            # Run specific test pattern - use eval to expand globs
            # shellcheck disable=SC2086
            sudo env PATH="$PATH" ${pkgs.perl}/bin/prove -rv --timer --merge -o $tests_dir/$1
          else
            # Run all tests
            sudo env PATH="$PATH" ${pkgs.perl}/bin/prove -rv --timer --merge -o "$tests_dir"
          fi
        '';

        # Common build inputs for both development and CI
        commonBuildInputs = with pkgs; [
          # Shell
          zsh

          # Go development (matches go.mod)
          go_1_25
          gopls
          golangci-lint
          delve

          # Build tools
          gnumake
          git
        ];

        # Platform-specific inputs
        linuxInputs =
          with pkgs;
          lib.optionals stdenv.isLinux [
            # NFS testing tools (Linux only)
            nfs-utils
            # ACL support for POSIX compliance testing
            acl
            # Perl for pjdfstest (prove is included in base perl)
            perl
            # POSIX filesystem compliance testing (C version - authoritative)
            pjdfstest
            # Docker client for PostgreSQL testing (daemon must be running on host)
            docker-client
            # Helper scripts (work in any shell - bash, zsh, etc.)
            dfs-mount
            dfs-umount
            dfs-posix
            dfs-e2e
            dfs-postgres-start
            dfs-postgres-stop
          ];

        darwinInputs =
          with pkgs;
          lib.optionals stdenv.isDarwin [
            # macOS uses built-in NFS client
            # For pjdfstest on macOS, use Docker: see test/posix/README.md
          ];

      in
      {
        # Development shell
        devShells.default = pkgs.mkShell {
          buildInputs = commonBuildInputs ++ linuxInputs ++ darwinInputs;

          shellHook = ''
            # Ensure Go modules are cached in user's home directory
            export GOPATH="$HOME/go"
            export GOMODCACHE="$HOME/go/pkg/mod"
            export GOCACHE="$HOME/.cache/go-build"

            echo "╔═══════════════════════════════════════════╗"
            echo "║     DittoFS Development Environment       ║"
            echo "╚═══════════════════════════════════════════╝"
            echo ""
            echo "Go version: $(go version | cut -d' ' -f3)"
            echo ""
            echo "Available commands:"
            echo "  go build ./cmd/dfs          Build DittoFS binary"
            echo "  go test ./...               Run all tests"
            echo "  go test -race ./...         Run tests with race detection"
            echo "  golangci-lint run           Run linters"
            echo ""
            if [[ "$(uname)" == "Darwin" ]]; then
              echo "NFS mount (macOS):"
              echo "  sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,resvport,nolock localhost:/export /tmp/dittofs-test"
              echo ""
              echo "POSIX compliance testing (via Docker):"
              echo "  docker build -t dfs-pjdfstest -f test/posix/Dockerfile.pjdfstest ."
              echo "  docker run --rm -v /tmp/dittofs-test:/mnt/test dfs-pjdfstest"
            else
              echo "NFS helper commands:"
              echo "  dfs-mount [path]            Mount NFS (default: /tmp/dittofs-test)"
              echo "  dfs-umount [path]           Unmount NFS"
              echo ""
              echo "POSIX compliance testing (pjdfstest - 8,789 tests):"
              echo "  dfs-posix                   Run all tests"
              echo "  dfs-posix chmod             Run chmod tests only"
              echo "  dfs-posix chown             Run chown tests only"
              echo ""
              echo "PostgreSQL testing:"
              echo "  dfs-postgres-start          Start PostgreSQL container"
              echo "  dfs-postgres-stop           Stop and remove container"
              echo ""
              echo "E2E testing (requires sudo for NFS mounts):"
              echo "  dfs-e2e                     Run all E2E tests"
              echo "  dfs-e2e -run TestName       Run specific test"
            fi
            echo ""

            # Use zsh if available and not already in zsh
            if [ -n "$ZSH_VERSION" ]; then
              : # Already in zsh
            elif command -v zsh &> /dev/null; then
              exec zsh
            fi
          '';
        };

        # CI shell (minimal, for running tests in CI)
        devShells.ci = pkgs.mkShell {
          buildInputs = commonBuildInputs ++ linuxInputs;

          shellHook = ''
            # Ensure Go modules are cached in user's home directory
            export GOPATH="$HOME/go"
            export GOMODCACHE="$HOME/go/pkg/mod"
            export GOCACHE="$HOME/.cache/go-build"
          '';
        };

        # Packages for building DittoFS
        packages =
          let
            commonArgs = {
              inherit version;
              src = ./.;

              # To update: set to "", run `nix build`, copy hash from error
              vendorHash = "sha256-nEbpeEH1cDv0bLYdjI3ewMGQDokq7WF0C172PPZHSH8=";

              ldflags = [
                "-s"
                "-w"
                "-X main.version=${version}"
                "-X main.commit=${gitRev}"
              ];

              meta = with pkgs.lib; {
                description = "Modular virtual filesystem with pluggable storage backends";
                homepage = "https://github.com/marmos91/dittofs";
                license = licenses.mit;
                platforms = platforms.unix;
              };
            };
          in
          {
            # Build both dfs and dfsctl
            default = pkgs.buildGoModule (commonArgs // {
              pname = "dittofs";
              subPackages = [
                "cmd/dfs"
                "cmd/dfsctl"
              ];
              meta = commonArgs.meta // { mainProgram = "dfs"; };
            });

            # Build only the server daemon
            dfs = pkgs.buildGoModule (commonArgs // {
              pname = "dfs";
              subPackages = [ "cmd/dfs" ];
            });

            # Build only the client CLI
            dfsctl = pkgs.buildGoModule (commonArgs // {
              pname = "dfsctl";
              subPackages = [ "cmd/dfsctl" ];
            });
          }
        // pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
          inherit pjdfstest;
        };

        # Flake checks - run with `nix flake check`
        checks = {
          # Verify the default package builds and contains both binaries
          default-has-dfs = pkgs.runCommand "check-default-has-dfs" { } ''
            ${self.packages.${system}.default}/bin/dfs version > /dev/null 2>&1 || \
            ${self.packages.${system}.default}/bin/dfs --help > /dev/null 2>&1
            touch $out
          '';
          default-has-dfsctl = pkgs.runCommand "check-default-has-dfsctl" { } ''
            ${self.packages.${system}.default}/bin/dfsctl --help > /dev/null 2>&1
            touch $out
          '';

          # Verify individual packages produce the correct binary
          dfs-binary = pkgs.runCommand "check-dfs-binary" { } ''
            ${self.packages.${system}.dfs}/bin/dfs version > /dev/null 2>&1 || \
            ${self.packages.${system}.dfs}/bin/dfs --help > /dev/null 2>&1
            touch $out
          '';
          dfsctl-binary = pkgs.runCommand "check-dfsctl-binary" { } ''
            ${self.packages.${system}.dfsctl}/bin/dfsctl --help > /dev/null 2>&1
            touch $out
          '';
        };
      }
    );
}
