SHELL := /bin/sh

GO ?= go
GOLANGCI_LINT ?= golangci-lint
GO_PACKAGES ?= ./...
GO_TEST_FLAGS ?= -v -race

MODULES := \
	otel-sampler \
	otel-testkit \
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

SAMPLING_DIR := otel-mongo/v2/tests/integration
SAMPLING_PKG := ./sampling/
HTTP_DIRECT_DIR := otel-testkit/examples/httpdirect
HTTP_STDLIB_DIR := otel-testkit/examples/httpdirect-stdlib

.PHONY: help modules examples integration \
	build test lint verify \
	build-examples test-examples lint-examples verify-examples \
	test-integration test-integration-sampling test-integration-http-direct \
	test-integration-http-stdlib \
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
		'  make test-integration-sampling  Run consistent-sampling E2E flag matrix (Docker)' \
		'  make test-integration-http-direct  Run direct-mode (HTTP) consistent-sampling demo (Docker)' \
		'  make test-integration-http-stdlib  Run Core (stdlib TraceIDRatioBased) demo (Docker)' \
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

# Consistent-sampling end-to-end suite, run once per feature-flag combination
# (the gates cache per process, so each flag state needs its own go test run).
# Requires Docker/Podman (real MongoDB + OTel Collector).
test-integration-sampling:
	@set -e; cd $(SAMPLING_DIR); \
	run() { desc="$$1"; shift; printf '\n==> sampling matrix: %s\n' "$$desc"; \
		env "$$@" $(GO) test -race -timeout 600s -run TestMongo $(SAMPLING_PKG); }; \
	run "row1 all-on arg=1.0"        OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=1 OTEL_MONGO_TRACING_ENABLED=1 OTEL_MONGO_PROPAGATION_ENABLED=1 OTEL_TRACES_SAMPLER_ARG=1.0; \
	run "row2 all-on arg=0.5"        OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=1 OTEL_MONGO_TRACING_ENABLED=1 OTEL_MONGO_PROPAGATION_ENABLED=1 OTEL_TRACES_SAMPLER_ARG=0.5; \
	run "row3 propagation-off"       OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=1 OTEL_MONGO_TRACING_ENABLED=1 OTEL_MONGO_PROPAGATION_ENABLED=0 OTEL_TRACES_SAMPLER_ARG=1.0; \
	run "row4 mongo-tracing-off"     OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=1 OTEL_MONGO_TRACING_ENABLED=0 OTEL_MONGO_PROPAGATION_ENABLED=1 OTEL_TRACES_SAMPLER_ARG=1.0; \
	run "row5 global-off"            OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=0 OTEL_MONGO_TRACING_ENABLED=1 OTEL_MONGO_PROPAGATION_ENABLED=1 OTEL_TRACES_SAMPLER_ARG=1.0

# Black-box demonstration: the consistent-sampling checks over a synchronous
# HTTP transport. No broker container; requires Docker for the OTel Collector.
test-integration-http-direct:
	cd $(HTTP_DIRECT_DIR) && $(GO) test -race -timeout 600s -run TestHTTP ./...

# Core (sampler-agnostic) demonstration: the same harness over a stdlib
# TraceIDRatioBased sampler that never writes ot=rv:. Requires Docker (Collector).
test-integration-http-stdlib:
	cd $(HTTP_STDLIB_DIR) && $(GO) test -race -timeout 600s ./...

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
