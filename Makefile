SHELL := /bin/bash
.DEFAULT_GOAL := help

ENV_FILE  := deploy/.env
STACK     := docker compose -f deploy/compose/stack.yml --env-file $(ENV_FILE)
REPO      := $(shell sed -n 's/^APP_IMAGE_REPO=//p' $(ENV_FILE) 2>/dev/null)
EDGE_PORT := $(shell sed -n 's/^EDGE_PORT=//p' $(ENV_FILE) 2>/dev/null)
V         ?= $(shell date +v%H%M%S)

.PHONY: help
help: ## Show the available commands
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  \033[32m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: test
test: ## Run all unit tests with the race detector
	cd app && go vet ./... && go vet -tags chaos ./... && go test -race ./...
	cd agent && go vet ./... && go test -race ./...
	cd tools && go vet ./...

.PHONY: e2e
e2e: ## Run the full end-to-end scenario suite against local Docker
	tests/e2e.sh

.PHONY: up
up: ## Start the local stack and the agent
	scripts/local-init.sh
	$(MAKE) --no-print-directory start

.PHONY: start
start:
	$(STACK) up --detach --wait --wait-timeout 180
	scripts/local-agent.sh start
	@echo "Edge http://127.0.0.1:$(EDGE_PORT)  Grafana http://127.0.0.1:3000  Prometheus http://127.0.0.1:9090"

.PHONY: down
down: ## Stop everything, keeping the data
	-scripts/local-agent.sh stop
	-docker rm --force app-blue app-green
	-$(STACK) down

.PHONY: clean
clean: down ## Stop everything and delete all local data
	-$(STACK) down --volumes
	rm -rf .run deploy/.env

.PHONY: release
release: ## Build and deploy a healthy release: make release V=v2
	docker build --quiet --build-arg VERSION=$(V) -t $(REPO):$(V) app
	SKIP_PULL=1 deploy/scripts/deploy.sh $(V)

.PHONY: release-bad-errors
release-bad-errors: ## Deploy a release that fails 40% of requests
	docker build --quiet --build-arg VERSION=$(V) --build-arg GO_TAGS=chaos --build-arg CHAOS_MODE=errors -t $(REPO):$(V) app
	SKIP_PULL=1 deploy/scripts/deploy.sh $(V)

.PHONY: release-bad-leak
release-bad-leak: ## Deploy a release that leaks memory until it is OOM-killed
	docker build --quiet --build-arg VERSION=$(V) --build-arg GO_TAGS=chaos --build-arg CHAOS_MODE=leak -t $(REPO):$(V) app
	SKIP_PULL=1 deploy/scripts/deploy.sh $(V)

.PHONY: load
load: ## Send traffic through the edge until interrupted
	cd tools && go run ./loadgen -url http://127.0.0.1:$(EDGE_PORT)

.PHONY: status
status: ## Show the live release and the agent's journal
	@jq '{current, previous, quarantined, history: .history[-5:]}' .run/state.json

.PHONY: provision
provision: ## Provision the servers listed in ansible/inventory.ini
	cd ansible && ansible-playbook site.yml
