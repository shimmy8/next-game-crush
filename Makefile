# Isolate this module from any enclosing Go workspace.
export GOWORK := off

SQLC_VERSION := v1.31.1
SQLC_IMAGE := sqlc/sqlc:$(SQLC_VERSION:v%=%)

.PHONY: dev test build down wait-test-db sqlc verify

dev:
	docker compose up --build

wait-test-db:
	docker compose up --quiet-pull --wait --wait-timeout 60 test-db

sqlc:
	docker run --rm -v "$(CURDIR):/src" -w /src $(SQLC_IMAGE) generate

verify: wait-test-db
	docker compose up -d mailpit
	@set -eu; sqlc_snapshot=$$(mktemp -d); trap 'rm -rf "$$sqlc_snapshot"' EXIT; \
		cp -R internal/store/sqlc "$$sqlc_snapshot/"; $(MAKE) sqlc; \
		diff -ru "$$sqlc_snapshot/sqlc" internal/store/sqlc
	TEST_DATABASE_URL='postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_test?sslmode=disable' go test -p 1 -race ./...
	pnpm --dir web run test
	pnpm --dir web run typecheck
	pnpm --dir web run lint
	pnpm --dir web run build
	docker compose config --quiet
	docker compose build app

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
