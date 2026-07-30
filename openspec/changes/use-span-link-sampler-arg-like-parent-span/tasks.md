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

## 5. Code-review follow-ups

Findings from the branch review in `reviews/code-review-use-span-link-sampler-arg-like-parent-span.zh-TW.html`.

- [x] 5.1 P1/S4: rename the remaining `DecodeWithContext` call sites and README references to `DecodeAndTrace` (the API was renamed on `main`; the sampling package no longer compiled)
- [x] 5.2 N4/N9/S6: `WithSingleLinkSeed` seeds with the link's `ot=rv:` only (no vendor members, no upstream `th:`) and preserves the caller's `ParentContext`; godoc + design D3 document what crosses a link and warn against `ParentBased`
- [x] 5.3 N3: `jsBatchDeliver` scopes each batch to a closure with `defer batch.Stop()` and fetches 1 message, per the CLAUDE.md early-return rule
- [x] 5.4 N5: `harness.envTruthy` matches `flags.EnvEnabled` on set-but-empty values, pinned by a golden table
- [x] 5.5 N6/N7/N8: `WaitForAppSpans(want=0)` settles before asserting; `AssertAllSpansCarryRV` catches a lost rv; rate checks use deterministic `UniformRVs` and a 4σ tolerance floor
- [x] 5.6 N1/N2: `SpansOfRun` expands span links in both directions; the manual span-link test is named honestly and a real `Watch`/`DecodeAndTrace` change-stream E2E was added
- [x] 5.7 N10/N11/F1: `serviceName` uses `Sprintf`; the `th:`/`rv:` tracestate writers merged into one helper; dead `Delete("ot")` branch removed; harness predicts decisions via exported `otelsampler.Threshold`/`Sampled`
- [x] 5.8 S1/S3/F3: generic integration jobs exclude `./sampling`; `otel-sampler`/`otel-testkit` raised to OTel v1.44 + testcontainers v0.43; the stdlib baseline example gained a CI job and both examples are linted
- [x] 5.9 S2/S5: `otel-sampler` gained a version constant, CHANGELOG and release-guard pattern (starting at `0.2.0`, since the published `v0.1.0` is superseded); root docs describe six modules, Go 1.25 and the current CI shape

## 6. Spec conformance check

- [x] 6.1 Re-run `cd otel-sampler && go test -v -race ./...` and `golangci-lint run ./...` against the two capability specs
- [ ] 6.2 Confirm every scenario in `consistent-probability-sampling` and `span-link-sampling-seed` maps to an existing test name (gap list or done)
- [ ] 6.3 Archive this change into `openspec/specs/` when the branch is merged (`/opsx:archive`)
