.PHONY: help up down logs build test test-race vet fmt tidy demo

ENV ?= .env

help:
	@echo "Targets:"
	@echo "  up         docker compose up (build + start stack)"
	@echo "  down       docker compose down -v"
	@echo "  logs       tail api + worker logs"
	@echo "  build      go build ./cmd/notifd"
	@echo "  test       go test ./..."
	@echo "  test-race  go test ./... -race"
	@echo "  tidy       go mod tidy"
	@echo "  vet        go vet ./..."
	@echo "  fmt        gofmt -w"
	@echo "  demo       run scripts/demo.sh against the live stack"

up:
	docker compose --env-file $(ENV) up --build -d

down:
	docker compose --env-file $(ENV) down -v

logs:
	docker compose logs -f api worker

build:
	go build -o bin/notifd ./cmd/notifd

test:
	go test ./... -count=1

test-race:
	CGO_ENABLED=1 go test ./... -race -count=1

tidy:
	go mod tidy

vet:
	go vet ./...

fmt:
	gofmt -w .

demo:
	./scripts/demo.sh
