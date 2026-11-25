# Siliconics backend. `make help` lists targets.
SHELL := /bin/bash
COMPOSE := docker compose -f deploy/compose/docker-compose.yml
SQLC := docker run --rm -v "$(CURDIR)":/src -w /src sqlc/sqlc:1.27.0
TEST_DB ?= postgres://siliconia:siliconia@localhost:5432/siliconia_test?sslmode=disable

.PHONY: help
help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

.PHONY: up
up: ## Start the whole stack (Caddy on http://localhost:8080)
	@test -f deploy/compose/.env || cp deploy/compose/.env.example deploy/compose/.env
	$(COMPOSE) up --build -d

.PHONY: down
down: ## Stop the stack (keeps data)
	$(COMPOSE) down

.PHONY: logs
logs: ## Follow logs
	$(COMPOSE) logs -f --tail=100

.PHONY: fmt
fmt: ## Format Go code
	gofmt -w cmd internal db

.PHONY: lint
lint: ## gofmt check, go vet, staticcheck
	@test -z "$$(gofmt -l cmd internal db)" || (gofmt -l cmd internal db; echo "run make fmt"; exit 1)
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

.PHONY: test
test: ## Unit tests with the race detector
	go test -race -count=1 ./...

.PHONY: admin
admin: ## Create an admin (max 3): make admin PHONE=9876543210 NAME="Asha"
	@test -n "$(PHONE)" -a -n "$(NAME)" || (echo 'usage: make admin PHONE=<10 digits> NAME="<name>"'; exit 2)
	$(COMPOSE) run --rm --no-deps api /usr/local/bin/admin create --phone "$(PHONE)" --name "$(NAME)"

.PHONY: demo
demo: ## Add the demo store (about 24 products with photos); runs once
	$(COMPOSE) run --rm --no-deps api /usr/local/bin/demo

.PHONY: test-integration
test-integration: ## Integration tests against the Compose PostgreSQL (make up first)
	$(COMPOSE) exec -T postgres psql -U siliconia -tc "SELECT 1 FROM pg_database WHERE datname='siliconia_test'" | grep -q 1 || \
	  $(COMPOSE) exec -T postgres createdb -U siliconia siliconia_test
	# -p 1: integration packages share one test database.
	TEST_DATABASE_URL="$(TEST_DB)" go test -race -count=1 -p 1 ./...

.PHONY: generate
generate: ## Regenerate sqlc code from db/queries
	$(SQLC) generate

.PHONY: check-generated
check-generated: ## Fail if generated code is out of date
	$(SQLC) diff

.PHONY: vuln
vuln: ## Known-vulnerability scan
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: check
check: lint test check-generated ## Everything CI runs except integration and security jobs
