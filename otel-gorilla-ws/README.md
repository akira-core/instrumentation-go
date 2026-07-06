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
go get github.com/Marz32onE/instrumentation-go/otel-gorilla-ws
```

## Usage

### Tracing feature flags

`otel-gorilla-ws` supports:

- `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` (global master switch)
- `OTEL_GORILLA_WS_TRACING_ENABLED` (ws module switch)

Defaults: disabled when unset (opt-in) — both vars must be explicitly set to a truthy value to enable tracing. Values `false/0/no/off` (case-insensitive) explicitly disable; any other set value (including empty string) is truthy.

Priority:
1. Global off disables ws tracing regardless of module flag.
2. Otherwise module flag controls ws tracing.

When disabled, both send/receive spans and trace-context propagation are turned off (the connection delegates straight to the underlying `*websocket.Conn`).

### NewConn vs. Dial / Upgrader

The feature flags above only gate whether tracing runs at all. Separately, whether the wire envelope (the `traceparent`/`tracestate` JSON wrapper) gets written/read depends on **which constructor created the `Conn`**:

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
