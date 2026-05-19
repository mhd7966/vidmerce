SHELL := /bin/bash
.DEFAULT_GOAL := help

# ---- Variables ----
APP_NAME      := vidmerce
API_BIN       := bin/api
WORKER_BIN    := bin/worker
PG_DSN        ?= postgres://vidmerce:vidmerce@localhost:5432/vidmerce?sslmode=disable
MIGRATE_PG    := migrations/postgres
MIGRATE_CH    := migrations/clickhouse

# Go version pinning. The project requires Go 1.25+. If GO is unset and a
# gvm-managed go1.25.0 exists we use it automatically; otherwise we trust
# whatever `go` is on PATH. Override with `make GO=/path/to/go ...`.
GVM_GO_125    := $(HOME)/.gvm/gos/go1.25.0/bin/go
GO            ?= $(shell [ -x "$(GVM_GO_125)" ] && echo "$(GVM_GO_125)" || echo "go")
export GOTOOLCHAIN := local

# ---- Helpers ----
.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ---- Infra ----
.PHONY: up down logs ps
up: ## Start postgres + redis + clickhouse + prometheus + grafana + exporters
	docker-compose up -d
	@echo "Waiting for services..."
	@sleep 3
	@docker-compose ps
	@echo ""
	@echo "Grafana:    http://localhost:3000  (admin / admin)"
	@echo "Prometheus: http://localhost:9090"

down: ## Stop and remove containers
	docker-compose down

logs: ## Tail infra logs
	docker-compose logs -f --tail=100

ps: ## Show infra status
	docker-compose ps

# ---- Build / Run ----
.PHONY: build run-api run-worker
build: ## Build api + worker binaries
	$(GO) build -o $(API_BIN) ./cmd/api
	$(GO) build -o $(WORKER_BIN) ./cmd/worker

run-api: ## Run the API
	$(GO) run ./cmd/api

run-worker: ## Run the background worker
	$(GO) run ./cmd/worker

# ---- Tooling ----
.PHONY: tools tidy fmt lint
tools: ## Install dev tools for this repo (migrate + golangci-lint)
	$(GO) install -tags 'postgres clickhouse' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@echo "Installed to $$(dirname $$($(GO) env GOPATH))/bin — ensure it is on your PATH"

tidy: ## go mod tidy
	$(GO) mod tidy

fmt: ## go fmt
	$(shell dirname $(GO))/gofmt -s -w .

lint: ## golangci-lint
	golangci-lint run ./...

# ---- Migrations ----
# Uses local `migrate` if on PATH; otherwise hack/migrate.sh runs the official
# Docker image (no install required for `make demo`).
MIGRATE := bash hack/migrate.sh
CH_HTTP_URL ?= clickhouse://default:@localhost:9000/vidmerce?x-multi-statement=true

.PHONY: migrate-up migrate-down migrate-status migrate-version migrate-force
migrate-up: ## Run Postgres migrations up
	$(MIGRATE) -path $(MIGRATE_PG) -database "$(PG_DSN)" up

migrate-down: ## Roll back last Postgres migration
	$(MIGRATE) -path $(MIGRATE_PG) -database "$(PG_DSN)" down 1

migrate-status: ## Show Postgres migration status (version + dirty flag)
	@$(MIGRATE) -path $(MIGRATE_PG) -database "$(PG_DSN)" version 2>&1 \
		| awk 'NF{print "postgres:", $$0; next} {print}'

migrate-version: migrate-status ## Alias for migrate-status

migrate-force: ## Force Postgres migration to VERSION=N (use to clear dirty state)
	@if [ -z "$(VERSION)" ]; then echo "usage: make migrate-force VERSION=N"; exit 1; fi
	$(MIGRATE) -path $(MIGRATE_PG) -database "$(PG_DSN)" force $(VERSION)

.PHONY: migrate-create-pg
migrate-create-pg: ## Create new Postgres migration (NAME=add_foo; requires local migrate)
	@if [ -z "$(NAME)" ]; then echo "usage: make migrate-create-pg NAME=add_foo"; exit 1; fi
	@command -v migrate >/dev/null || (echo "install migrate: make tools"; exit 1)
	migrate create -ext sql -dir $(MIGRATE_PG) -seq $(NAME)

# ClickHouse
.PHONY: migrate-up-ch migrate-down-ch migrate-status-ch migrate-create-ch
migrate-up-ch: ## Run ClickHouse migrations up
	$(MIGRATE) -path $(MIGRATE_CH) -database "$(CH_HTTP_URL)" up

migrate-down-ch: ## Roll back last ClickHouse migration
	$(MIGRATE) -path $(MIGRATE_CH) -database "$(CH_HTTP_URL)" down 1

migrate-status-ch: ## Show ClickHouse migration status
	@$(MIGRATE) -path $(MIGRATE_CH) -database "$(CH_HTTP_URL)" version 2>&1 \
		| awk 'NF{print "clickhouse:", $$0; next} {print}'

migrate-create-ch: ## Create new ClickHouse migration (NAME=add_foo; requires local migrate)
	@if [ -z "$(NAME)" ]; then echo "usage: make migrate-create-ch NAME=add_foo"; exit 1; fi
	@command -v migrate >/dev/null || (echo "install migrate: make tools"; exit 1)
	migrate create -ext sql -dir $(MIGRATE_CH) -seq $(NAME)

.PHONY: migrate-all migrate-status-all
migrate-all: migrate-up migrate-up-ch ## Run all migrations (Postgres + ClickHouse)
migrate-status-all: migrate-status migrate-status-ch ## Show status across both stores

# ---- Tests ----
.PHONY: test test-unit test-integration
test: test-unit ## Alias for test-unit

test-unit: ## Run unit tests
	$(GO) test -race -count=1 ./...

test-integration: ## Run integration tests (uses testcontainers; needs Docker)
	$(GO) test -race -count=1 -tags=integration -timeout 10m ./tests/integration/...

# ---- Load tests (k6) ----
.PHONY: load-bootstrap load-feed load-like load-view load-stats load-all
load-bootstrap: ## Seed videos in the feed (run once before like/view/stats)
	k6 run loadtest/bootstrap.js

load-feed: ## GET /feed under ramping load
	k6 run loadtest/feed.js

load-like: ## POST /videos/:id/like ↔ /unlike burst
	k6 run loadtest/like.js

load-view: ## POST /videos/:id/view mixed traffic
	k6 run loadtest/view.js

load-stats: ## GET /videos/:id/stats stampede stress
	k6 run loadtest/stats.js

load-all: load-bootstrap load-feed load-like load-view load-stats ## Run every scenario in sequence

load-run: ## Full k6 suite + loadtest/RESULTS.md (API + worker must be running)
	bash hack/run-loadtest.sh

# ---- Interview demo (one command) ----
DEMO_DIR := .demo

.PHONY: demo demo-stop demo-seed
demo: ## Infra + migrate + API + worker + seed data + HTML report (open .demo/report.html)
	@$(MAKE) demo-stop
	@[ -f .env ] || cp .env.example .env
	@bash -c 'grep -q "^LIKE_RECONCILER_INTERVAL=" .env 2>/dev/null || echo "LIKE_RECONCILER_INTERVAL=30s" >> .env'
	$(MAKE) up
	@echo "Waiting for Postgres, Redis, ClickHouse (healthy)..."
	@bash -c 'for i in $$(seq 1 90); do \
	  pg=$$(docker inspect -f "{{.State.Health.Status}}" vidmerce-postgres 2>/dev/null || echo starting); \
	  rd=$$(docker inspect -f "{{.State.Health.Status}}" vidmerce-redis 2>/dev/null || echo starting); \
	  ch=$$(docker inspect -f "{{.State.Health.Status}}" vidmerce-clickhouse 2>/dev/null || echo starting); \
	  if [ "$$pg" = healthy ] && [ "$$rd" = healthy ] && [ "$$ch" = healthy ]; then exit 0; fi; \
	  sleep 2; done; echo "timeout waiting for infra (check: docker-compose ps)"; exit 1'
	$(MAKE) migrate-all
	@bash hack/repair-clickhouse-db.sh
	@mkdir -p $(DEMO_DIR)
	@bash hack/free-demo-port.sh 8080
	@bash hack/free-demo-metrics-port.sh 9091
	@echo "Starting API → $(DEMO_DIR)/api.log"
	@nohup $(GO) run ./cmd/api >$(DEMO_DIR)/api.log 2>&1 & echo $$! >$(DEMO_DIR)/api.pid
	@echo "Starting worker → $(DEMO_DIR)/worker.log"
	@nohup $(GO) run ./cmd/worker >$(DEMO_DIR)/worker.log 2>&1 & echo $$! >$(DEMO_DIR)/worker.pid
	@echo "Waiting for /ready..."
	@bash -c 'for i in $$(seq 1 60); do curl -sf http://localhost:8080/ready >/dev/null 2>&1 && exit 0; sleep 1; done; echo "API not ready — tail $(DEMO_DIR)/api.log"; tail -30 $(DEMO_DIR)/api.log; exit 1'
	$(GO) run ./cmd/demo -out $(DEMO_DIR)/report.html

demo-seed: ## Re-run HTTP seed only (API + worker must already be running)
	$(GO) run ./cmd/demo -out $(DEMO_DIR)/report.html

demo-stop: ## Stop API + worker started by make demo
	@bash hack/free-demo-port.sh 8080
	@bash hack/free-demo-metrics-port.sh 9091
	@echo "Stopped demo API/worker (infra still running — use make down to stop Docker)"

# ---- House-keeping ----
.PHONY: clean
clean: ## Remove built binaries
	rm -rf bin/
