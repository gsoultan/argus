# Argus — optional shortcuts.
#
# Every target is a one-line delegation to a script in scripts/. The scripts are
# the real interface and work standalone; this exists only for `make` muscle
# memory and tab-completion. Nothing here has logic of its own — if you find
# yourself adding some, it belongs in the script instead.

SHELL := /bin/bash
.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo ""
	@echo "  Argus — make targets (thin wrappers over scripts/)"
	@echo ""
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo ""

.PHONY: setup
setup: ## First-time setup: verify toolchain, install dependencies
	@./scripts/setup.sh

.PHONY: dev
dev: ## Run every service present
	@./scripts/dev.sh

.PHONY: dev-web
dev-web: ## Run only the web UI
	@./scripts/dev.sh web

.PHONY: dev-force
dev-force: ## Run, killing whatever holds a needed port
	@./scripts/dev.sh --force

.PHONY: deps-up
deps-up: ## Start Postgres + MinIO
	@./scripts/deps.sh up

.PHONY: deps-down
deps-down: ## Stop Postgres + MinIO (keeps data)
	@./scripts/deps.sh down

.PHONY: deps-status
deps-status: ## Show dependency state
	@./scripts/deps.sh status

.PHONY: deps-logs
deps-logs: ## Tail dependency logs (SVC=postgres|minio)
	@./scripts/deps.sh logs $(or $(SVC),postgres)

.PHONY: deps-reset
deps-reset: ## Stop and DELETE all local data
	@./scripts/deps.sh reset

.PHONY: check
check: ## Typecheck, vet and test everything
	@./scripts/check.sh

.PHONY: build
build: ## Production build
	@./scripts/build.sh

.PHONY: clean
clean: ## Remove build output and installed dependencies
	@rm -rf web/dist web/node_modules bin
	@echo "Cleaned."
