# Ref: oga9-engineering/sections/01-foundation-tech-stack.md
.PHONY: dev dev-full migrate-up migrate-down sqlc-gen test build lint bundle

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
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...

# §09/§14.01 release bundle — versioned image + self-host package with
# pinned manifest and the migration set it upgrades through. Usage:
#   make bundle VERSION=1.0.0
VERSION ?= 0.0.0-dev
bundle:
	docker build -t oga9:$(VERSION) .
	rm -rf dist/oga9-selfhost-$(VERSION)
	mkdir -p dist/oga9-selfhost-$(VERSION)
	cp -r deploy/selfhost/. dist/oga9-selfhost-$(VERSION)/
	cp -r db/migrations dist/oga9-selfhost-$(VERSION)/migrations
	sed -i 's/^version:.*/version: "$(VERSION)"/' dist/oga9-selfhost-$(VERSION)/manifest.yaml
	sed -i -E 's|oga9:[[:space:]]+oga9:[^ ]*|oga9:     oga9:$(VERSION)|' dist/oga9-selfhost-$(VERSION)/manifest.yaml
	grep -n "oga9:" dist/oga9-selfhost-$(VERSION)/manifest.yaml
	tar -czf dist/oga9-selfhost-$(VERSION).tar.gz -C dist oga9-selfhost-$(VERSION)
	@echo "bundle: dist/oga9-selfhost-$(VERSION).tar.gz"
