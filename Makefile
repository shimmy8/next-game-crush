# Isolate this module from any enclosing Go workspace.
export GOWORK := off

.PHONY: dev test build down

dev:
	docker compose up --build

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
