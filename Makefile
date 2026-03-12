.PHONY: setup-hooks fmt lint vet build

# Configure git to use the project's hooks directory
setup-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks configured (using .githooks/)"

# Format all Go source files
fmt:
	gofmt -s -w .

# Run golangci-lint (must be installed separately)
lint:
	golangci-lint run

# Run go vet
vet:
	go vet ./...

# Build both CLI binaries
build:
	go build -o dfs cmd/dfs/main.go
	go build -o dfsctl cmd/dfsctl/main.go
