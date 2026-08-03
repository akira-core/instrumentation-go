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

```
capability = gate1 && OTEL_GORILLA_WS_TRACING_ENABLED        (fixed at construction)
span gate  = capability && relay otel-gorilla-ws-tracing      (re-read every call)
```

`gate1` is `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` **or** `WithTracingEnabled(v)` — two spellings
of one switch, and supplying **both is a configuration error** (`ErrTracingConfigConflict`). A
switch is on only when set to `1`, `true`, `yes` or `on`.

**Capability** decides whether `otel-ws` is offered (`Dial`) or confirmed (`Upgrader.Upgrade`), and
is resolved *before* the handshake because a handshake cannot be revisited. It deliberately excludes
the relay, which costs nothing under a revoke-only relay. **The span gate** is the relay verdict on
top, re-read on every read and write, so a live connection follows a revocation.

Three things to know:

- Capability clamps the **write** path only. Whether the peer envelopes is a fact of the handshake,
  so a capability-off wrapper of a negotiated connection writes raw frames but **still unwraps on
  read** — otherwise your application would receive raw `{"header":…,"data":…}` bytes.
- Revoking stops spans and injection but **not** the envelope on an already-negotiated connection:
  the peer parses every frame as one. This module alone does not return to the zero-cost path on
  revocation; removing that wire overhead needs a redeploy.
- Running your own handshake? Offer or echo `SubprotocolOTelWS` and check `IsOTelNegotiated(raw)`
  before `NewConn` — see [otel-ws.md](../otel-ws.md).

> Full reference — every resolution table, connecting a relay with no application code, revocation
> latency, per-service targeting, and the operational summary:
> **[feature-flags.md](../feature-flags.md)** · 繁體中文:**[feature-flags.zh-TW.md](../feature-flags.zh-TW.md)**

### NewConn vs. Dial / Upgrader

The effective feature flag above gates whether tracing runs at all. Separately, whether the wire envelope gets written/read depends on **which constructor** created the `Conn` (and, for Dial/Upgrade, whether otel-ws was negotiated):

- **`NewConn(rawConn, opts...) (*Conn, error)`** wraps a `*websocket.Conn` you already dialed/upgraded yourself. It enables envelope wrapping **only when the raw connection's negotiated subprotocol proves `otel-ws`** — offer or echo `SubprotocolOTelWS` during your handshake, and check `IsOTelNegotiated(raw)` to verify. It returns an error when the configuration is contradictory (`ErrTracingConfigConflict`).
- **`Dial(ctx, urlStr, requestHeader, subprotocols, opts...)`** is the spec-compliant client entry point. It injects the `otel-ws` subprotocol into the handshake; envelope wrapping is enabled only if the server confirms support by returning an `otel-ws`/`otel-ws+<proto>` subprotocol.
- **`Upgrader{}.Upgrade(w, r, responseHeader)`** is the spec-compliant server entry point (mirrors `websocket.Upgrader.Upgrade`). It detects `otel-ws` in the client's proposed subprotocols and responds with `otel-ws`/`otel-ws+<proto>`, enabling envelope wrapping only on that acceptance path.

For `Dial`/`Upgrade`, when the peer does not negotiate `otel-ws`, the connection silently falls back to passthrough mode: send/receive spans are still created (as long as the feature flags are on), but no envelope is written or read on the wire.

```go
raw, _, _ := websocket.DefaultDialer.DialContext(ctx, serverURL, nil)
conn, err := otelgorillaws.NewConn(raw)
if err != nil {
	return err
}

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
