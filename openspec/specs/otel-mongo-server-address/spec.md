# otel-mongo-server-address Specification

## Purpose
Accurate per-command server.address/server.port attribution for otel-mongo spans (v1 and v2): a chained CommandMonitor captures the server that actually executed each command, with a static URI-derived fallback and no behavior when tracing is disabled.

## Requirements

### Requirement: Per-command server address capture
When tracing is enabled, `otel-mongo` (v1 `otelmongo/` and v2 `v2/`) SHALL derive the `server.address`/`server.port` attributes on a Collection CRUD CLIENT span from the actual MongoDB connection that carried that specific command, not from a value parsed once at `Connect` time.

#### Scenario: Command served by a non-first replica-set member
- **WHEN** a `Client` is connected with a multi-host replica-set URI and a Collection operation (e.g. `FindOne`) is served by a host other than the first host listed in the connection string
- **THEN** the operation's CLIENT span's `server.address` (and `server.port`, when non-default) reflect the host that actually served the command, not the first host in the URI

#### Scenario: Failover changes the serving host between two operations
- **WHEN** two sequential Collection operations on the same `Client` are served by different hosts (e.g. after a primary failover)
- **THEN** each operation's span independently reflects the host that served it — the second span's `server.address` differs from the first's if the serving host changed

#### Scenario: mongodb+srv:// connection string
- **WHEN** a `Client` is connected via a `mongodb+srv://` URI
- **THEN** Collection operation spans carry the resolved connection's actual host, not the unresolved SRV record name

#### Scenario: Retried operation
- **WHEN** a retryable Collection operation is retried once by the driver before succeeding
- **THEN** the operation's span's `server.address`/`server.port` reflect the connection used by the attempt that produced the returned result

### Requirement: Fallback to static URI-derived address
When no per-command server address was captured for an operation, `otel-mongo` SHALL fall back to the existing statically-parsed `Client.serverAddr`/`serverPort` (derived from the connection URI at `Connect`/`ConnectWithOptions` time) so the span still carries a best-effort `server.address` rather than omitting it.

#### Scenario: No command event captured
- **WHEN** a Collection operation completes without a corresponding `CommandStartedEvent` having been observed for its context (e.g. defensive/edge-case path)
- **THEN** the operation's span uses the statically-parsed `Client.serverAddr`/`serverPort` as `server.address`/`server.port`, identical to pre-change behavior

### Requirement: Caller-supplied CommandMonitor is chained, not replaced
When a caller passes their own `*options.ClientOptions` with `SetMonitor(...)` already set to `Connect`/`ConnectWithOptions`, `otel-mongo` SHALL preserve the caller's monitor callbacks by chaining: the package's own address-capture logic runs first, then the caller's original `Started`/`Succeeded`/`Failed` callbacks (for whichever of those the caller set) run unmodified with the same event.

#### Scenario: Caller has their own command monitor for APM
- **WHEN** a caller constructs `*options.ClientOptions` with `SetMonitor(&event.CommandMonitor{Started: myStartedFn, Succeeded: mySucceededFn})` and passes it to `otelmongo.ConnectWithOptions`
- **THEN** `myStartedFn` and `mySucceededFn` are still invoked for every command, receiving the same events they would have received without `otel-mongo`'s instrumentation

#### Scenario: Caller sets only a subset of monitor callbacks
- **WHEN** a caller's `event.CommandMonitor` only sets `Succeeded` (leaving `Started`/`Failed` nil)
- **THEN** `otel-mongo`'s own `Started` callback still runs (to capture the address) and the caller's `Succeeded` callback still runs unmodified; no nil-function-call panic occurs

### Requirement: No new tracing behavior when tracing is disabled
When `OTEL_MONGO_TRACING_ENABLED` (combined with the global gate) is off, `otel-mongo` SHALL NOT register a `CommandMonitor` for address capture, consistent with the existing disabled-mode invariant that the disabled path performs no additional instrumentation-related work.

#### Scenario: Disabled tracing registers no command monitor
- **WHEN** `Connect`/`ConnectWithOptions` is called with tracing disabled (module or global gate off)
- **THEN** no address-capture `CommandMonitor` is attached to the resulting `*mongo.Client`'s options, and any caller-supplied `SetMonitor` passes through completely untouched
