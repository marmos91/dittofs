# SMB Conformance Test Suite - Makefile
# Convenience targets for the Docker Compose stack smbtorture's scripts run on

.DEFAULT_GOAL := help

.PHONY: help build clean test-admission smbtorture smbtorture-quick

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

test-admission: ## Test the host-wide admission lease (no Docker needed)
	./compose-env_test.sh

build: ## Build DittoFS Docker image only
	bash -c '. ./compose-env.sh && docker compose build dittofs'

clean: ## Remove results
	rm -rf results/
	bash -c '. ./compose-env.sh && docker compose down -v' 2>/dev/null || true

smbtorture: ## Run smbtorture tests
	cd smbtorture && $(MAKE) test

smbtorture-quick: ## Run smbtorture connect test only
	cd smbtorture && $(MAKE) test-quick
