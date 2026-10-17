# Isolate this module from any enclosing Go workspace.
export GOWORK := off

SQLC_VERSION := v1.31.1
SQLC_IMAGE := sqlc/sqlc:$(SQLC_VERSION:v%=%)
PLAYWRIGHT_BROWSERS_PATH := $(CURDIR)/.cache/ms-playwright
export PLAYWRIGHT_BROWSERS_PATH

.PHONY: dev test build down wait-test-db sqlc verify prepare-e2e-db playwright-browser

dev:
	docker compose up --build

wait-test-db:
	docker compose up --quiet-pull --wait --wait-timeout 60 test-db

sqlc:
	docker run --rm -v "$(CURDIR):/src" -w /src $(SQLC_IMAGE) generate

prepare-e2e-db: wait-test-db
	docker compose exec -T test-db psql -v ON_ERROR_STOP=1 -U next_game_crush_test -d postgres -c "DROP DATABASE IF EXISTS next_game_crush_e2e WITH (FORCE)"
	docker compose exec -T test-db psql -v ON_ERROR_STOP=1 -U next_game_crush_test -d postgres -c "CREATE DATABASE next_game_crush_e2e"

playwright-browser:
	@if [ -n "$${PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH:-}" ] && [ -x "$$PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH" ]; then \
		echo "Using explicit Playwright Chromium executable: $$PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH"; \
	else \
		pnpm --dir web exec playwright install chromium; \
	fi

verify: wait-test-db
	docker compose up -d mailpit
	$(MAKE) prepare-e2e-db
	@set -eu; sqlc_snapshot=$$(mktemp -d); trap 'rm -rf "$$sqlc_snapshot"' EXIT; \
		cp -R internal/store/sqlc "$$sqlc_snapshot/"; $(MAKE) sqlc; \
		diff -ru "$$sqlc_snapshot/sqlc" internal/store/sqlc
	TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' go test -race ./...
	pnpm --dir web run test
	pnpm --dir web run typecheck
	pnpm --dir web run lint
	pnpm --dir web run build
	docker compose config --quiet
	docker compose build app
	$(MAKE) playwright-browser
	pnpm --dir web exec playwright test tests/e2e/smoke.spec.ts

test:
	go test ./...
	pnpm --dir web run test
	pnpm --dir web run typecheck
	pnpm --dir web run lint

build:
	go build -o bin/next-game-crush ./cmd/next-game-crush
	pnpm --dir web run build

down:
	docker compose down
