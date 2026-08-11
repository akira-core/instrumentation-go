# otel-nats (otelnats + oteljetstream)

[繁體中文 (Traditional Chinese)](README.zh-TW.md)

---

OpenTelemetry tracing for [NATS](https://nats.io/) and [NATS JetStream](https://docs.nats.io/nats-concepts/jetstream), aligned with the official `nats.go` / `nats.go/jetstream` APIs. Propagates W3C Trace Context in message headers. `oteljetstream` now fully wraps JetStream consumer management APIs (`StreamConsumerManager` on `JetStream` and `ConsumerManager` on `Stream`) while keeping message publish/consume tracing behavior unchanged. Per [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation): packages accept **TracerProvider** and **Propagators** via options; they do **not** provide InitTracer. Set the global provider and propagator at process startup (see **examples/**).

---

## Layout

```
otel-nats/
├── otelnats/               # Core NATS: Connect, Conn, Publish, Subscribe, Request, HeaderCarrier
│   ├── connect.go          # Connect, ConnectWithOptions, ConnectTLS, ConnectWithCredentials
│   ├── conn.go             # Conn, connImpl interface, Options (WithTracerProvider, WithPropagators, WithTraceDestination)
│   ├── conn_traced.go      # tracedConn: instrumented connImpl (spans, propagation)
│   ├── conn_direct.go      # directConn: passthrough connImpl used when tracing is disabled
│   ├── traceevent.go       # WithTraceDestination / SubscribeTraceEvents / TraceEvent / TraceHop (NATS 2.11+ trace events)
│   ├── propagation.go      # HeaderCarrier (nats.Header ↔ TextMapCarrier)
│   ├── env_flags.go        # this module's flag key, env var, default, and gateState
│   ├── internal/flags/     # shared EnvEnabled/Gate helpers (byte-identical across instrumentation modules)
│   └── doc.go
├── oteljetstream/          # JetStream: New, JetStream, Stream, Consumer, Consume, Messages, Fetch
│   ├── jetstream.go        # New(conn), JetStream interface, shared types (ConsumerConfig, StreamConfig, ...)
│   ├── jetstream_traced.go # tracedJSImpl: instrumented JetStream impl
│   ├── jetstream_direct.go # directJSImpl: passthrough JetStream impl
│   ├── stream.go           # Stream interface (consumer-manager methods)
│   ├── stream_traced.go    # tracedStream: instrumented Stream impl
│   ├── stream_direct.go    # directStream: passthrough Stream impl
│   ├── consumer.go         # Consumer interface, Msg, MessageBatch, MessagesContext
│   ├── consumer_traced.go  # tracedConsumer: Consume/Messages/Next/Fetch with spans
│   ├── consumer_direct.go  # directConsumer: passthrough Consumer impl
│   └── doc.go
├── examples/            # How to create TracerProvider + set global + use otelnats/oteljetstream
├── go.mod
└── README.md
```

---

## Usage

### Tracing feature flags

```
tracing = master && natsTracing
```

Each switch resolves down a four-step ladder, first source with an opinion winning:

```
relay  >  env  >  option (With*Enabled)  >  hardcoded default
```

The relay is authoritative in **both** directions — it can disable a running module and enable one
the deployment left off. Safety comes from the defaults: the master switch
`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` defaults to `true` and is a **veto** (only `false` has an
effect; it accepts no option), while every per-module switch defaults to **off**.

**The option sits below its environment variable**, reversing `0.7.0`. ``OTEL_NATS_TRACING_ENABLED`=false` disables
this module even where the Go code passed `WithTracingEnabled(true)`, so an operator has a per-module
setting application code cannot override. With the variable unset the option decides, so two
connections in one process can still differ.

A switch is decided only by `1`/`true`/`yes`/`on` or `0`/`false`/`no`/`off`. Unset means "no opinion".
**Anything else — including the empty string — fails construction** with an error wrapping
`otelflags.ErrInvalidFlagValue`.

`WithTracingEnabled` does **not** pin anything: a wrapper carrying it resolves the master switch and
the relay on every operation.

The mutual-exclusion rule and `ErrTracingConfigConflict` are **gone**: supplying an option alongside
its variable is ordinary configuration, and the variable wins.

Subscriptions and JetStream consumers re-resolve per **message**, so one created before a flag
change follows it without being re-established.

> Full reference — every resolution table, connecting a relay with no application code, revocation
> latency, per-service targeting, and the operational summary:
> **[docs/feature-flags.md](../docs/feature-flags.md)** ·
> Tutorial: **[docs/otel-nats-kill-switch.en-US.html](../docs/otel-nats-kill-switch.en-US.html)** ·
> 繁體中文:**[docs/feature-flags.zh-TW.md](../docs/feature-flags.zh-TW.md)** ·
> **[docs/otel-nats-kill-switch.zh-TW.html](../docs/otel-nats-kill-switch.zh-TW.html)**

### 1. Initialize provider and propagator (application responsibility)

Create a TracerProvider (e.g. OTLP) and set the global provider and propagator once at startup. See **examples/main.go** for a full runnable.

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/sdk/resource"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// In main:
tp, err := newTracerProvider() // create with OTLP exporter + resource
if err != nil { log.Fatal(err) }
defer func() { _ = tp.Shutdown(ctx) }()

otel.SetTracerProvider(tp)
otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{},
    propagation.Baggage{},
))
```

### 2. Core NATS: Connect, Publish, Subscribe

```go
import (
    "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

conn, err := otelnats.Connect(natsURL, nil)
if err != nil { log.Fatal(err) }
defer conn.Close()

conn.Publish(ctx, "subject", []byte("data"))
conn.Subscribe("subject", func(m otelnats.Msg) {
    // m.Msg, m.Context() — trace from headers in m.Context()
})
conn.QueueSubscribe("subject", "queue", handler)
```

Optional: pass **WithTracerProvider(tp)**, **WithPropagators(p)**, or **WithTracingEnabled(v bool)** to **ConnectWithOptions** for per-connection overrides.

### 3. Request/Reply

`Conn.Request` / `RequestWithContext` / `RequestMsg` / `RequestMsgWithContext` mirror the equivalent `nats.Conn` methods exactly, but open a CLIENT span for the RPC (`request {subject}`) and a second, linked CLIENT span for the reply — bare `receive`, with no destination segment, since the reply arrives on an auto-generated, single-use inbox (`_INBOX.<nuid>`) and semconv v1.39.0 says to omit `{destination}` when no low-cardinality value exists. The inbox subject stays queryable via `messaging.destination.name`, `messaging.destination.temporary=true`, `messaging.destination.anonymous=true`, and `messaging.message.conversation_id`:

```go
reply, err := conn.RequestWithContext(ctx, "subject", []byte("ping"))
if err != nil { log.Fatal(err) }
// reply.Data — trace context for the request/reply pair is recorded on the CLIENT span;
// the reply itself is recorded as a linked CLIENT "receive" span (bare, no subject).
```

`Request` / `RequestMsg` have no `context.Context` parameter (mirroring `nats.Conn`), so their producer span is rooted at `context.Background()` — use `RequestWithContext` / `RequestMsgWithContext` to chain into an existing trace.

### 4. JetStream

```go
import (
    "github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
    "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

conn, _ := otelnats.Connect(natsURL, nil)
defer conn.Close()

js, err := oteljetstream.New(conn)
// After creating stream/consumer:
cons.Consume(func(m oteljetstream.Msg) {
    // m.Data(), m.Ack(), m.Context() — trace from message headers
})
```

Or iterate manually with `Messages()`:

```go
iter, err := cons.Messages()
if err != nil { log.Fatal(err) }
defer iter.Stop() // release the iterator goroutine and end any in-flight span

for {
    ctx, msg, err := iter.Next()
    if err != nil { break } // iterator stopped/drained
    _ = ctx // trace context extracted from msg headers
    _ = msg.Ack()
}
```

> **Push consumers** are wrapped (`PushConsumer`/`CreatePushConsumer`/`CreateOrUpdatePushConsumer`/`UpdatePushConsumer` on both `JetStream` and `Stream`); the returned `PushConsumer.Consume` carries trace context. Management-only APIs (`PauseConsumer`/`ResumeConsumer`/`UnpinConsumer`) are exposed directly on `Stream` as untraced passthroughs (`ResetConsumer`/`ResetConsumerToSequence` are not exposed — they require nats.go v1.52.0, above this module's v1.50.0 pin); `Unwrap()` exists only on `JetStream`, for APIs the wrapper does not re-expose (`KeyValue`/`ObjectStore`/`AccountInfo`/`Conn`/`Options`). Async publish (`PublishAsync`/`PublishMsgAsync`) is not wrapped: these take no `context.Context` and return a non-blocking `PubAckFuture` instead of a synchronous ack, which doesn't fit this wrapper's context-propagation model (see `oteljetstream/doc.go`).

### 5. Tests

Set the global provider (and optionally propagator) before Connect; no InitTracer.

```go
otel.SetTracerProvider(tp)
otel.SetTextMapPropagator(prop) // if testing propagation
conn, err := otelnats.Connect(url, nil)
```

---

## API summary

| Item | Description |
|------|-------------|
| **Connect** | `Connect(url string, natsOpts ...nats.Option)`. Uses `otel.GetTracerProvider()` and `otel.GetTextMapPropagator()` unless overridden via ConnectWithOptions. |
| **ConnectWithOptions** | Same with optional **WithTracerProvider(tp)**, **WithPropagators(p)**, and **WithTracingEnabled(v bool)**. |
| **ConnectTLS** | `ConnectTLS(url, certFile, keyFile, caFile string, natsOpts ...nats.Option)`. Connects with mutual TLS. |
| **ConnectWithCredentials** | `ConnectWithCredentials(url, credFile string, natsOpts ...nats.Option)`. Connects with JWT/NKey credentials. |
| **ScopeName / Version()** | Used when creating Tracer (OTel contrib guideline). |
| **Request / RequestWithContext / RequestMsg / RequestMsgWithContext** | RPC helpers mirroring `nats.Conn`; open a CLIENT span named `request {subject}` and a linked CLIENT span named bare `receive` for the reply. |
| **Inbox span names** | A span whose resolved destination is an unbounded reply inbox drops the destination from its name (`publish`, `process`, `receive`) and keeps it on the attributes. Applies to JetStream too — a stream may capture inbox subjects (see **Span names**). |
| **`Conn.InboxPrefixes()`** | The inbox prefixes this connection recognises (`0.9.1+`). Used by `oteljetstream`; rarely needed by applications. |
| **JetStream consumer managers** | `JetStream` fully wraps `StreamConsumerManager`; `Stream` fully wraps `ConsumerManager`. Methods returning `Consumer` or `PushConsumer` remain trace-enabled wrappers (see JetStream section). |
| **WithTraceDestination / SubscribeTraceEvents** | Convert NATS 2.11+ infrastructure trace events into OTel spans (see **NATS 2.11+ Trace Events**). |
| **Tests** | Use `otel.SetTracerProvider(tp)` (and `otel.SetTextMapPropagator(prop)` if needed) before Connect. |

---

## Span kinds

Span kind follows the OTel messaging "Span kind" mapping (`send` → `PRODUCER`, `receive` (pull) → `CLIENT`, `process` (push) → `CONSUMER`):

```
Publish / PublishMsg                     PRODUCER  (send)
Request / RequestWithContext / ...       CLIENT    (request, awaits reply)
  └── receive                            CLIENT    (linked reply receive, pull — bare name, no destination)
Subscribe / QueueSubscribe handler       CONSUMER  (process, push-delivered)

JetStream publish                        PRODUCER  (send)
JetStream Consume handler                CONSUMER  (process, push-delivered callback)
JetStream Fetch / Messages / Next        CLIENT    (linked receive, pull)
```

JetStream `receive`/`process` spans additionally carry `messaging.consumer.group.name` (the durable/consumer name); core NATS spans do not.

---

## Span names

Span names follow the OTel messaging semconv v1.39.0 format `{messaging.operation.name} {destination}`:

| Operation | Span name | Notes |
|---|---|---|
| Publish (core NATS or JetStream) | `publish {subject}` | was `send {subject}` before `0.9.0` |
| Request | `request {subject}` | was `{subject} request` before `0.9.0` |
| Reply receive | `receive` | bare, no destination — the inbox is auto-generated and single-use; was `receive {inbox}` before `0.9.0` |
| Publish to a reply inbox | `publish` | bare — the manual responder half, `conn.Publish(msg.Reply, …)` |
| Subscribe/QueueSubscribe handler | `process {destination}` | |
| Handler on a reply inbox subscription | `process` | bare — the manual requester half |
| JetStream consumer receive/process | `receive {destination}` / `process {destination}` | inbox test applies here too since `0.9.1` — a stream may capture inbox subjects |
| JetStream over an inbox-capturing stream | `receive` / `process` / `publish` | bare, when the resolved destination is an unbounded inbox |

`{destination}` resolves in this order: the subscription or single-valued JetStream consumer filter subject → the concrete message subject. A resolved destination that differs from the concrete subject (a wildcard subscription or filter) is additionally recorded on the span as `messaging.destination.template`; `messaging.destination.name` always carries the concrete subject. Both are facts the library already holds — it never guesses which token of a subject is an identifier.

The resolved destination is then dropped from the span name when it is an **unbounded reply inbox**, matching semconv's rule to omit `{destination}` when no low-cardinality value is available. The inbox stays fully queryable on the attributes: `messaging.destination.name`, `messaging.message.conversation_id`, `messaging.destination.temporary=true` and `messaging.destination.anonymous=true`.

"Unbounded" is the operative word. A filter that is **nothing but an inbox prefix plus wildcards** — `_INBOX.>`, the shape a consumer archiving replies declares — is a fixed string the subscriber chose, so it stays in the span name and is recorded as `messaging.destination.template`. semconv attaches the temporary/anonymous exclusion to `messaging.destination.name` (its *second* choice for `{destination}`), not to `messaging.destination.template` (its first). A filter carrying a literal token, such as `_INBOX.<nuid>.>`, is per-request and is dropped like any concrete inbox. The temporary/anonymous/`conversation_id` markers are recorded either way: they describe the delivery, not the name.

Inboxes are recognised by subject prefix, and **two prefixes are recognised**: this connection's own (`nats.CustomInboxPrefix(p)` ⇒ `p + "."`) and the default `_INBOX.` always. Recognising only the local prefix would fail exactly where custom prefixes are used — a responder sees the *requester's* inbox in `msg.Reply`, and the requester is the side that customises, because granting it `subscribe: _INBOX.>` would hand it every other client's replies while a responder needs no inbox permission at all.

### Residual span-name cardinality

Every unbounded span name the library can *see* is bounded by the rules above. Two sources remain, both structurally invisible to it:

**A peer on a custom inbox prefix this connection does not share.** Two peers using two *different* custom prefixes will not recognise each other's inboxes from a concrete subject alone. Reply-receive spans are unaffected — that path knows structurally that it holds an inbox, whatever its prefix — and so is any fixed subscription or consumer filter, which is bounded regardless of prefix. What remains is a manual `conn.Publish(peerInbox, …)` or a handler subscribed directly on a foreign-prefix inbox.

**Subjects that embed identifiers.** A subject like `orders.12345.created` is not templated by this module: no library can tell which token is an identifier, and semconv permits recording a `messaging.destination.template` that is already known, not inferring one. Two cases stay high-cardinality:

- publish and request spans, which have no subscription or filter to resolve against; and
- JetStream consumers with **no** filter subject, or with **several** wildcard filter subjects.

Rewrite both downstream, where the pattern is known — the OTel Collector `span` processor does it without touching application code:

```yaml
span/to_attributes:
  name:
    to_attributes:
      rules:
        # Subjects embedding an identifier.
        - ^receive orders\.(?P<orderId>[^.]+)\.created$
        # A foreign custom inbox prefix this connection does not recognise.
        - ^(?P<op>publish|process) SVCB\.(?P<inbox>[^.]+)
# "receive orders.12345.created" -> "receive orders.{orderId}.created", orderId=12345
```

### Three words for one operation

A publish span carries three spellings, all required by semconv, none of them a bug:

| | Value | Why |
|---|---|---|
| `messaging.operation.type` | `send` | a **fixed enum**: `create`, `send`, `receive`, `process`, `settle` |
| `messaging.operation.name` | `publish` (or `request`) | the **system's own verb** — NATS calls it Publish |
| span name | `publish {subject}` | semconv's `{messaging.operation.name} {destination}` |

The span name follows `operation.name`, not `operation.type`.

---

## NATS 2.11+ Trace Events

NATS server 2.11+ can publish infrastructure-level trace events (ingress, egress, JetStream store, subject-mapping, stream-export, service-import) for any message carrying a `Nats-Trace-Dest` header. `otel-nats` can consume these events and convert each hop into an OTel span.

### Producer: set the trace destination

```go
conn, err := otelnats.ConnectWithOptions(natsURL, nil,
    otelnats.WithTraceDestination("nats.trace.events"),
)
```

While tracing is enabled, every message sent via `conn.Publish`/`conn.PublishMsg` carries the `Nats-Trace-Dest` header, so the server emits a `TraceEvent` payload to `nats.trace.events` for each hop the message takes.

### Consumer: convert events into spans

```go
sub, err := otelnats.SubscribeTraceEvents(conn, "nats.trace.events")
if err != nil { log.Fatal(err) }
defer sub.Unsubscribe()
```

Each `otelnats.TraceEvent` payload covers one server and carries a list of `otelnats.TraceHop`s. `SubscribeTraceEvents` emits one point-in-time span per hop (named `nats.<KIND>.<type>`, e.g. `nats.CLIENT.ingress`), linked to the original publisher span via the `traceparent` header embedded in the event's request headers.

Requires NATS server 2.11+. `SubscribeTraceEvents` only emits spans when the connection's tracing gate is on; with tracing disabled it discards events instead (subscription still succeeds so `Unsubscribe` lifecycle works either way).

---

## MessageBatch (`Fetch` / `FetchBytes` / `FetchNoWait`)

Iterate `Messages()` to receive each message with its extracted trace context. Drain the channel completely for each batch before the next `Fetch`.

```go
batch, err := consumer.Fetch(10)
if err != nil { ... }
for m := range batch.Messages() {
    _ = m.Context()
    _ = m.Ack()
}
if err := batch.Error(); err != nil { ... }
```

`MessageBatch.Stop()` releases the internal goroutine and ends any in-flight span. Callers that fully drain the channel until it closes need not call it; callers that `break`/`return` before the channel closes **must** call it (typically via `defer`) to avoid leaking the goroutine and the last consumer span:

```go
batch, err := consumer.Fetch(10)
if err != nil { ... }
defer batch.Stop()

for m := range batch.Messages() {
    if shouldStopEarly(m) {
        break // deferred batch.Stop() ends the in-flight span and stops the goroutine
    }
    _ = m.Context()
    _ = m.Ack()
}
```

---

## Diagnostic logging

Uses [`log/slog`](https://pkg.go.dev/log/slog) — no output by default.

| Level | Events |
|-------|--------|
| `DEBUG` | Server address parse failure in `serverAttrsFromConn`; trace event received (`traceevent.go`) |
| `WARN` | Trace event JSON unmarshal failure (`traceevent.go`) |

Enable with a debug-level slog handler at startup:

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

Log entries use the `otelnats:` prefix. The connection log line (`conn.go`) uses `addr` and `error`; trace-event log lines (`traceevent.go`) use `raw`, `server`, `hops`, `events`, `error`, and `request_headers`.

---

## Dependencies

- `github.com/nats-io/nats.go` (includes JetStream)
- `go.opentelemetry.io/otel` and SDK (trace, propagation)
- Go 1.24+

Tests use `github.com/stretchr/testify` and `nats-server/v2` for integration tests.
