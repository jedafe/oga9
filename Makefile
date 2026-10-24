# Ref: oga9-engineering/sections/01-foundation-tech-stack.md
.PHONY: dev dev-full migrate-up migrate-down sqlc-gen test build lint

dev:
	docker compose up -d postgres valkey mailpit glitchtip
	go run ./cmd/api

dev-full:
	docker compose --profile mvp --profile full up -d
	go run ./cmd/api

migrate-up:
	migrate -path db/migrations -database "$(OGA9_DATABASE_URL)" up

migrate-down:
	migrate -path db/migrations -database "$(OGA9_DATABASE_URL)" down 1

sqlc-gen:
	sqlc generate

test:
	go test ./...

build:
	go build -o bin/oga9 ./cmd/api

lint:
	gofmt -l . | grep -v vendor || true
	go vet ./...
