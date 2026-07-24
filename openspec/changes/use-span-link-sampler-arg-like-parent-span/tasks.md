## 1. Module scaffold

- [x] 1.1 Create `otel-sampler/` module with `go.mod` and package `otelsampler`
- [x] 1.2 Wire module into Makefile / CI `test-and-lint` matrix

## 2. ProbabilitySampler

- [x] 2.1 Implement `ProbabilitySampler` (threshold, TraceID/rv randomness precedence, `ot=th` write-on-record)
- [x] 2.2 Implement `ProbabilitySamplerFromEnv` reading `OTEL_TRACES_SAMPLER_ARG`
- [x] 2.3 Add unit tests for subset property, service threshold matrix, rv precedence, invalid rv fallback, env parsing

## 3. WithSingleLinkSeed

- [x] 3.1 Implement `WithSingleLinkSeed` (parent precedence, exactly-one-valid-link seeding, root `ot=rv` emission)
- [x] 3.2 Add unit tests for link rv / link TraceID seed, parent-over-link, multi-link fallback, linked-chain + A/B/C/D/E matrix
- [x] 3.3 Add SDK integration tests: new TraceID preserved, rv on linked/dropped spans, TraceContext inject/extract

## 4. Verification helpers (supporting)

- [x] 4.1 Expose `harness.ConsistentSampler` / `ConsistentSamplerFromEnv` as the recommended composition
- [x] 4.2 Add/adjust E2E and example suites that assert consistent rv across span-link topologies

## 5. Spec conformance check

- [ ] 5.1 Re-run `cd otel-sampler && go test -v -race ./...` and `golangci-lint run ./...` against the two capability specs
- [ ] 5.2 Confirm every scenario in `consistent-probability-sampling` and `span-link-sampling-seed` maps to an existing test name (gap list or done)
- [ ] 5.3 Archive this change into `openspec/specs/` when the branch is merged (`/opsx:archive`)
