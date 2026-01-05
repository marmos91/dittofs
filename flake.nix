{
  description = "DittoFS - Modular virtual filesystem with pluggable storage backends";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    let
      # Version configuration - update this for releases
      version = "0.1.0";
    in
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # Git revision for build info (use "dirty" if uncommitted changes)
        gitRev = self.shortRev or self.dirtyShortRev or "unknown";

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
          ];

        darwinInputs =
          with pkgs;
          lib.optionals stdenv.isDarwin [
            # macOS uses built-in NFS client, no extra packages needed
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
            echo "  go build ./cmd/dittofs      Build DittoFS binary"
            echo "  go test ./...               Run all tests"
            echo "  go test -race ./...         Run tests with race detection"
            echo "  golangci-lint run           Run linters"
            echo ""
            echo "NFS testing (Linux only, requires root):"
            echo "  sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test"
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

        # Package for building DittoFS
        packages.default = pkgs.buildGoModule {
          pname = "dittofs";
          inherit version;
          src = ./.;

          # To update: set to "", run `nix build`, copy hash from error
          vendorHash = "sha256-vY9q02votXhfLN7KlkxQphWE+z7jhOhFtj5we9jOQ00=";

          subPackages = [ "cmd/dittofs" ];

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
      }
    );
}
