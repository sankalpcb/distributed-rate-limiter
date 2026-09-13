SHELL := /bin/bash
.DEFAULT_GOAL := help

GO      ?= go
REGION  ?= us-central1
SERVICE ?= limiterd

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | column -t -s ':'

## test: unit tests. No Docker needed -- the Lua scripts run against an in-process Redis
test:
	$(GO) test -race ./...

## test-integration: tests against a real Redis via docker compose
test-integration:
	docker compose up -d redis
	REDIS_ADDR=127.0.0.1:6379 $(GO) test -race -tags=integration ./... ; \
	  status=$$? ; docker compose down ; exit $$status

## lint: formatting and vet
lint:
	@test -z "$$(gofmt -l cmd internal)" || { echo "gofmt needed:"; gofmt -l cmd internal; exit 1; }
	$(GO) vet ./...

## build: compile both binaries into ./bin
build:
	mkdir -p bin
	$(GO) build -o bin/limiterd ./cmd/limiterd
	$(GO) build -o bin/loadgen  ./cmd/loadgen

## run-local: full local stack on port 8080
run-local:
	docker compose up --build

## infra-up: provision GCP infrastructure (needs terraform and infra/terraform.tfvars)
infra-up:
	cd infra && terraform init && terraform apply

## infra-down: destroy ALL GCP infrastructure. Run at the end of every session.
infra-down:
	cd infra && terraform destroy

## bench: run the full experiment suite against the deployed service
bench:
	./bench/sweep.sh all

## bench-validate: day-4 gate -- confirm the load generator is not the bottleneck
bench-validate:
	./bench/sweep.sh validate

.PHONY: help test test-integration lint build run-local infra-up infra-down bench bench-validate
