# smbtorture Test Suite - Makefile
# Convenience targets for running smbtorture against DittoFS
# GPL compliance: smbtorture runs inside Docker container only

.DEFAULT_GOAL := help

PROFILE ?= memory

.PHONY: help test test-quick test-full clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

test: ## Run smbtorture with current profile (default: memory)
	./run.sh --profile $(PROFILE)

test-quick: ## Run smbtorture connect test only (fastest)
	./run.sh --profile memory --filter smb2.connect

test-full: ## Run smbtorture with all profiles
	@for profile in memory memory-fs badger-fs; do \
		echo "=== Profile: $$profile ==="; \
		./run.sh --profile $$profile || exit 1; \
	done

clean: ## Remove smbtorture results
	rm -rf ../results/smbtorture-*
