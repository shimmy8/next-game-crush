# Isolate this module from any enclosing Go workspace.
export GOWORK := off

SQLC_VERSION := v1.31.1
SQLC_IMAGE := sqlc/sqlc:$(SQLC_VERSION:v%=%)

.PHONY: dev test build down wait-test-db sqlc

dev:
	docker compose up --build

wait-test-db:
	docker compose up --quiet-pull --wait --wait-timeout 60 test-db

sqlc:
	docker run --rm -v "$(CURDIR):/src" -w /src $(SQLC_IMAGE) generate

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
