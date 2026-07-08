## ADDED Requirements

### Requirement: Reported instrumentation scope version matches module version
Every wrapper package (`otelmongo` v1, `otelmongo` v2, `otelnats`, `otelgorillaws`) SHALL report its module's semantic version as the instrumentation scope version on every `Tracer` it creates, via `trace.WithInstrumentationVersion(Version())` passed to `TracerProvider.Tracer(ScopeName, ...)`. This applies uniformly whether the tracer comes from a caller-supplied `TracerProvider`, the process-global `otel.GetTracerProvider()`, or the noop provider used when tracing is disabled.

#### Scenario: Real tracer reports current module version
- **WHEN** any of the four packages creates a `Tracer` from a configured (non-noop) `TracerProvider`
- **THEN** the tracer is created with `trace.WithInstrumentationVersion(Version())`, so every span it emits carries the package's current module version (e.g. `0.6.0`) as its instrumentation scope version

#### Scenario: Noop tracer still reports the version
- **WHEN** tracing is disabled for a package (its feature-flag gate evaluates to off) and it falls back to `noop.NewTracerProvider()`
- **THEN** the noop tracer is still created with `trace.WithInstrumentationVersion(Version())`, keeping the reported scope version consistent regardless of whether spans are real or no-ops

#### Scenario: Version bump changes the reported value
- **WHEN** a module's `instrumentationVersion` constant is bumped (e.g. from `0.5.0` to `0.6.0`)
- **THEN** every subsequently created `Tracer` in that module reports the new version string as its instrumentation scope version, with no other code change required
