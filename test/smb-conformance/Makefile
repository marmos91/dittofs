# SMB Conformance Test Suite - Makefile
# Convenience targets for running WPTS BVT tests against DittoFS

.DEFAULT_GOAL := help

PROFILE ?= memory

.PHONY: help test test-quick test-full build clean dry-run local keep

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

test: ## Run BVT tests with current profile (default: memory)
	./run.sh --profile $(PROFILE)

test-quick: ## Run BVT tests with memory profile (fastest)
	./run.sh --profile memory

test-full: ## Run BVT tests across all profiles
	@for p in memory memory-fs badger-fs badger-s3 postgres-s3; do \
		echo "=== Profile: $$p ==="; \
		./run.sh --profile $$p || exit 1; \
	done

build: ## Build DittoFS Docker image only
	docker compose build dittofs

dry-run: ## Show configuration without running tests
	./run.sh --profile $(PROFILE) --dry-run

clean: ## Remove results and generated ptfconfig
	rm -rf results/ ptfconfig-generated/
	docker compose down -v 2>/dev/null || true

local: ## Run in local mode (DittoFS native, WPTS in Docker)
	./run.sh --profile $(PROFILE) --mode local

keep: ## Run tests and keep containers for debugging
	./run.sh --profile $(PROFILE) --keep

smbtorture: ## Run smbtorture tests
	cd smbtorture && $(MAKE) test

smbtorture-quick: ## Run smbtorture connect test only
	cd smbtorture && $(MAKE) test-quick
