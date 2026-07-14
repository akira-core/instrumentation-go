# otel-gorilla-ws

`otel-gorilla-ws` wraps [gorilla/websocket](https://github.com/gorilla/websocket) and adds OpenTelemetry distributed tracing with W3C Trace Context propagation inside WebSocket message bodies.

Outgoing messages use the shared envelope format (compatible with `otel-ws` and `otel-rxjs-ws` JS packages):

```json
{
  "header": { "traceparent": "...", "tracestate": "..." },
  "data": <original-payload>
}
```

`data` is the original payload as-is if it is valid JSON, or a JSON-encoded string for non-JSON bytes.

Incoming messages support two formats:
1. **Envelope format** (above) — used by new Go and JS clients.
2. **Legacy flat format** — backward compatible with old Go-only deployments: `{ "traceparent": "...", "tracestate": "...", ...fields }`.

## Installation

```bash
go get github.com/akira-core/instrumentation-go/otel-gorilla-ws
```

## Usage

### Tracing feature flags

`otel-gorilla-ws` supports:

- `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global master switch)
- `OTEL_GORILLA_WS_TRACING_ENABLED` (ws module switch)

Defaults: disabled when unset (opt-in) — both vars must be truthy to enable via env. Values `false/0/no/off` (case-insensitive) disable; any other set value (including empty string) is truthy.

When disabled, send/receive spans and envelope inject/extract are off (passthrough to `*websocket.Conn`).

#### Env × `WithTracingEnabled` (`featureEnabled`)

`WithTracingEnabled(v bool)` on `NewConn`, `Dial`, or `Upgrader.Upgrade` overrides the two env vars for that `Conn` only (`featureEnabled` — whether any OTel SDK path runs). When absent, env decides.

| Env (`GLOBAL` ∧ `OTEL_GORILLA_WS_TRACING_ENABLED`) | `WithTracingEnabled` | Effective feature |
|----------------------------------------------------|----------------------|-------------------|
| off (unset or falsy) | *(absent)* | **off** |
| off (unset or falsy) | `true` | **on** |
| off (unset or falsy) | `false` | **off** |
| on | *(absent)* | **on** |
| on | `false` | **off** |
| on | `true` | **on** |

For `Dial` / `Upgrader.Upgrade`, the **effective** feature is also resolved **before** the handshake: when off, the side neither offers nor confirms `otel-ws` (avoids wire corruption). `WithTracingEnabled(true)` still cannot force the envelope onto a peer that did not negotiate otel-ws — that outcome is `Conn.tracingEnabled` (negotiation), a separate boolean from `featureEnabled`.

### NewConn vs. Dial / Upgrader

The effective feature flag above gates whether tracing runs at all. Separately, whether the wire envelope gets written/read depends on **which constructor** created the `Conn` (and, for Dial/Upgrade, whether otel-ws was negotiated):

- **`NewConn(rawConn, opts...)`** wraps a `*websocket.Conn` you already dialed/upgraded yourself. It always enables envelope wrapping when the feature flags are on, regardless of subprotocol — kept for backward compatibility with callers that manage their own handshake.
- **`Dial(ctx, urlStr, requestHeader, subprotocols, opts...)`** is the spec-compliant client entry point. It injects the `otel-ws` subprotocol into the handshake; envelope wrapping is enabled only if the server confirms support by returning an `otel-ws`/`otel-ws+<proto>` subprotocol.
- **`Upgrader{}.Upgrade(w, r, responseHeader)`** is the spec-compliant server entry point (mirrors `websocket.Upgrader.Upgrade`). It detects `otel-ws` in the client's proposed subprotocols and responds with `otel-ws`/`otel-ws+<proto>`, enabling envelope wrapping only on that acceptance path.

For `Dial`/`Upgrade`, when the peer does not negotiate `otel-ws`, the connection silently falls back to passthrough mode: send/receive spans are still created (as long as the feature flags are on), but no envelope is written or read on the wire.

```go
raw, _, _ := websocket.DefaultDialer.DialContext(ctx, serverURL, nil)
conn := otelgorillaws.NewConn(raw)

_ = conn.WriteMessage(ctx, websocket.TextMessage, []byte("hello"))
recvCtx, msgType, data, _ := conn.ReadMessage(context.Background())
_, _ = recvCtx, msgType
_ = data
```

```go
// Spec-compliant client/server entry points with otel-ws negotiation:
conn, resp, err := otelgorillaws.Dial(ctx, wsURL, nil, []string{"json"})
// ...
upgrader := otelgorillaws.Upgrader{AppSubprotocols: []string{"json"}}
conn, err := upgrader.Upgrade(w, r, nil)
```

See `examples/main.go` for a full example of bootstrapping a TracerProvider/propagator before using `NewConn`.

### Subprotocol negotiation design notes

For the full scenario table covering standard WebSocket subprotocol negotiation, the `otel-ws` hidden-protocol injection scheme, and how `Dial`/`Upgrader` behave in each case (including edge cases like an unsupported/empty server response), see [`../otel-ws.md`](../otel-ws.md). Review that doc alongside any change to `conn.go`'s `Dial` or `upgrader.go`'s `Upgrade` negotiation logic to keep it in sync.
