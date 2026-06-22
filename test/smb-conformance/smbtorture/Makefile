# smbtorture Test Suite - Makefile
# Convenience targets for running smbtorture against DittoFS
# GPL compliance: smbtorture runs inside Docker container only

.DEFAULT_GOAL := help

PROFILE ?= memory

.PHONY: help test test-quick test-full emit-baseline clean

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

emit-baseline: ## Regenerate baseline-results.md from the latest results dir
	@RESULTS_DIR=$$(ls -td ../results/smbtorture-*/ 2>/dev/null | head -1); \
	if [ -z "$$RESULTS_DIR" ] || [ ! -f "$$RESULTS_DIR/smbtorture-output.txt" ]; then \
		echo "No smbtorture results found. Run 'make test' first."; exit 1; \
	fi; \
	echo "Regenerating baseline-results.md from $$RESULTS_DIR"; \
	BASELINE_PROFILE=$(PROFILE) ./parse-results.sh --emit-baseline \
		"$$RESULTS_DIR/smbtorture-output.txt" KNOWN_FAILURES.md > baseline-results.md

clean: ## Remove smbtorture results
	rm -rf ../results/smbtorture-*
