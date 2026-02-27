# DittoFS Benchmark Suite - Makefile
# Convenience targets wrapping benchmark shell scripts.

.DEFAULT_GOAL := help

PROFILE ?= dittofs-badger-s3
COMPOSE := docker compose -f docker-compose.yml --profile $(PROFILE)

.PHONY: help check build up down logs ps clean bootstrap

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

check: ## Check prerequisites (docker, fio, jq, etc.)
	./scripts/check-prerequisites.sh

build: ## Build DittoFS Docker image for current profile
	$(COMPOSE) build

up: ## Start benchmark profile (default: dittofs-badger-s3)
	$(COMPOSE) up -d

down: ## Stop benchmark profile and remove volumes
	$(COMPOSE) down -v

logs: ## Follow logs for current profile
	$(COMPOSE) logs -f

ps: ## Show container status for current profile
	$(COMPOSE) ps

clean: ## Full cleanup: unmount, stop containers, remove volumes, prune
	./scripts/clean-all.sh --force

bootstrap: ## Bootstrap DittoFS stores/shares for current profile
	@BACKEND=$$(echo "$(PROFILE)" | sed 's/^dittofs-//'); \
	echo "Run: docker compose exec $(PROFILE) /app/bootstrap.sh $$BACKEND"
