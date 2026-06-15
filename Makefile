SHELL := /bin/sh

GO ?= go
GOLANGCI_LINT ?= golangci-lint
GO_PACKAGES ?= ./...
GO_TEST_FLAGS ?= -v -race

MODULES := \
	otel-sampler \
	otel-gorilla-ws \
	otel-mongo \
	otel-mongo/v2 \
	otel-nats

EXAMPLE_MODULES := \
	otel-gorilla-ws/examples \
	otel-mongo/examples \
	otel-nats/examples

INTEGRATION_MODULES := \
	otel-gorilla-ws/tests/integration \
	otel-mongo/tests/integration \
	otel-mongo/v2/tests/integration \
	otel-nats/tests/integration

.PHONY: help modules examples integration \
	build test lint verify \
	build-examples test-examples lint-examples verify-examples \
	test-integration \
	test-sampler test-gorilla-ws test-mongo test-mongo-v2 test-nats

help:
	@printf '%s\n' \
		'Targets:' \
		'  make test              Run go test -v -race ./... in main modules' \
		'  make build             Run go build ./... in main modules' \
		'  make lint              Run golangci-lint run ./... in main modules' \
		'  make verify            Run build, test, and lint in main modules' \
		'  make test-sampler      Test otel-sampler' \
		'  make test-gorilla-ws   Test otel-gorilla-ws' \
		'  make test-mongo        Test otel-mongo' \
		'  make test-mongo-v2     Test otel-mongo/v2' \
		'  make test-nats         Test otel-nats' \
		'  make test-examples     Run tests in example modules' \
		'  make test-integration  Run integration tests (requires Docker/Podman)' \
		'' \
		'Variables:' \
		'  GO_TEST_FLAGS="-v -race"   Override go test flags' \
		'  GO_PACKAGES="./..."        Override package pattern'

modules:
	@printf '%s\n' $(MODULES)

examples:
	@printf '%s\n' $(EXAMPLE_MODULES)

integration:
	@printf '%s\n' $(INTEGRATION_MODULES)

define run_in_modules
	@set -e; \
	for module in $(1); do \
		printf '\n==> %s: %s\n' "$$module" "$(2)"; \
		(cd "$$module" && $(3)); \
	done
endef

build:
	$(call run_in_modules,$(MODULES),go build,$(GO) build $(GO_PACKAGES))

test:
	$(call run_in_modules,$(MODULES),go test,$(GO) test $(GO_TEST_FLAGS) $(GO_PACKAGES))

lint:
	$(call run_in_modules,$(MODULES),golangci-lint,$(GOLANGCI_LINT) run $(GO_PACKAGES))

verify: build test lint

build-examples:
	$(call run_in_modules,$(EXAMPLE_MODULES),go build,$(GO) build $(GO_PACKAGES))

test-examples:
	$(call run_in_modules,$(EXAMPLE_MODULES),go test,$(GO) test $(GO_TEST_FLAGS) $(GO_PACKAGES))

lint-examples:
	$(call run_in_modules,$(EXAMPLE_MODULES),golangci-lint,$(GOLANGCI_LINT) run $(GO_PACKAGES))

verify-examples: build-examples test-examples lint-examples

test-integration:
	$(call run_in_modules,$(INTEGRATION_MODULES),go test,$(GO) test $(GO_TEST_FLAGS) $(GO_PACKAGES))

test-sampler:
	$(call run_in_modules,otel-sampler,go test,$(GO) test $(GO_TEST_FLAGS) $(GO_PACKAGES))

test-gorilla-ws:
	$(call run_in_modules,otel-gorilla-ws,go test,$(GO) test $(GO_TEST_FLAGS) $(GO_PACKAGES))

test-mongo:
	$(call run_in_modules,otel-mongo,go test,$(GO) test $(GO_TEST_FLAGS) $(GO_PACKAGES))

test-mongo-v2:
	$(call run_in_modules,otel-mongo/v2,go test,$(GO) test $(GO_TEST_FLAGS) $(GO_PACKAGES))

test-nats:
	$(call run_in_modules,otel-nats,go test,$(GO) test $(GO_TEST_FLAGS) $(GO_PACKAGES))
