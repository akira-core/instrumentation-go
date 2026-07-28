# consistent-probability-sampling Delta: use-span-link-sampler-arg-like-parent-span

## ADDED Requirements

### Requirement: Threshold probability sampler

The module SHALL export `ProbabilitySampler(probability float64) sdktrace.Sampler` that implements consistent threshold-based probabilistic sampling aligned with OpenTelemetry Go PR #8123.

Randomness used for the decision SHALL be resolved in this order:

1. If the parent SpanContext's `tracestate` contains a valid `ot` member with an `rv:` sub-key of exactly 14 hexadecimal digits, use that value as a 56-bit integer.
2. Otherwise, use the least-significant 56 bits of `SamplingParameters.TraceID` (`TraceID[8:16]` masked with `2^56 - 1`).
3. A malformed `rv` value SHALL be ignored (treated as absent) so TraceID randomness is used instead.

The sampler SHALL record (`RecordAndSample`) when `threshold <= randomness`, and SHALL drop otherwise. When recording, the result's `tracestate` SHALL contain an `ot` member with a `th:` sub-key encoding the configured threshold; other existing `ot` sub-keys (including `rv`) SHALL be preserved. When dropping, the sampler SHALL NOT introduce a new `th:` value.

Edge configuration:

- `probability >= 1.0` SHALL always record (threshold equivalent to always-sample, `th:0`).
- `probability` that is `NaN` or strictly less than `1/2^56` SHALL behave as never-sample (`sdktrace.NeverSample()`).

For a fixed randomness value, if rate A is less than rate B, a sample decision at A SHALL imply a sample decision at B (deterministic subset property).

#### Scenario: Explicit parent rv overrides TraceID

- **WHEN** the parent context carries `tracestate` `ot=rv:f0000000000000` and the current TraceID's low 56 bits would otherwise drop at rate `0.5`
- **THEN** `ProbabilitySampler(0.5)` SHALL return `RecordAndSample`
- **AND** the result `tracestate` SHALL still contain `rv:f0000000000000` and a `th:` sub-key

#### Scenario: TraceID randomness used when rv absent

- **WHEN** there is no parent `ot=rv` and the TraceID low 56 bits equal `0xf0000000000000`
- **THEN** `ProbabilitySampler(0.5)` SHALL return `RecordAndSample` with `ot` containing `th:` and not `rv:`
- **AND WHEN** the TraceID low 56 bits equal `0x00000000000000`
- **THEN** the same sampler SHALL return `Drop` without introducing `ot=th`

#### Scenario: Invalid rv falls back to TraceID

- **WHEN** the parent `tracestate` contains a non-hex or wrong-length `rv` value and the TraceID low 56 bits are below the rate-`0.5` threshold
- **THEN** `ProbabilitySampler(0.5)` SHALL return `Drop`

#### Scenario: Higher rates include lower-rate samples

- **WHEN** the same randomness is evaluated at rates `0.1`, `0.2`, and `0.5`
- **THEN** every randomness sampled at `0.1` SHALL also be sampled at `0.2` and `0.5`
- **AND** every randomness sampled at `0.2` SHALL also be sampled at `0.5`

#### Scenario: Same TraceID is stable

- **WHEN** `ProbabilitySampler(0.1)` is invoked twice with identical sampling parameters
- **THEN** both calls SHALL return the same decision and the same `tracestate` string

### Requirement: Environment-configured probability

The module SHALL export `ProbabilitySamplerFromEnv(defaultProbability float64) sdktrace.Sampler` that constructs a `ProbabilitySampler` using the float value of environment variable `OTEL_TRACES_SAMPLER_ARG` when that value parses successfully after trimming whitespace. When the variable is unset or not a valid float, the sampler SHALL use `defaultProbability`.

#### Scenario: Env arg selects never-sample

- **WHEN** `OTEL_TRACES_SAMPLER_ARG` is set to `0` and `ProbabilitySamplerFromEnv(1)` is used
- **THEN** sampling a TraceID that would be kept at rate `1.0` SHALL return `Drop`
