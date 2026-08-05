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
effective tracing = master && wsTracing        (each down the full ladder)
negotiation       = effective tracing          (resolved ONCE, just before the handshake)
span gate         = effective tracing          (re-read every read and write)
```

Each switch resolves down a four-step ladder, first source with an opinion winning:

```
relay  >  env  >  option (WithTracingEnabled)  >  hardcoded default
```

The relay is authoritative in **both** directions. Safety comes from the defaults: the master switch
`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` defaults to `true` and is a **veto** (only `false` has an
effect; it accepts no option), while `OTEL_GORILLA_WS_TRACING_ENABLED` defaults to **off**.

**The option sits below its environment variable**, reversing `0.7.0`.
`OTEL_GORILLA_WS_TRACING_ENABLED=false` disables this module even where the Go code passed
`WithTracingEnabled(true)`. With the variable unset the option decides.

A switch is decided only by `1`/`true`/`yes`/`on` or `0`/`false`/`no`/`off`. Unset means "no opinion".
**Anything else — including the empty string — fails construction.** The mutual-exclusion rule and
`ErrTracingConfigConflict` are **gone**.

**Negotiation and the span gate are the same expression, evaluated at different times.** That is the
whole subtlety of this module: a handshake cannot be revisited, so the wire decision is made once and
the span decision is made per call. The asymmetry it produces must be planned around:

- **Enabling reaches only connections opened afterwards.** A connection opened while this module was
  off never gains the envelope, and `WithTracingEnabled(true)` cannot restore it — a peer that did
  not negotiate `otel-ws` will not parse one. Such a connection can still emit **local** spans; it
  just cannot inject or extract. Cycle the connection if you need it instrumented.
- **Disabling reaches every connection immediately for spans and inject/extract, but not for the
  envelope**, which the peer is still parsing. This module alone does not return to the zero-cost
  path when you turn it off; removing that wire overhead requires cycling the connection.

With **no relay configured**, negotiation resolves to exactly what `0.7.0` computed, so such
deployments see the previous release's wire byte for byte.

Three things to know:

- Capability (`relayPossible || (master && module)` locally, fixed at construction) clamps the
  **write** path only. Whether the peer envelopes is a fact of the handshake, so a capability-off
  wrapper of a negotiated connection writes raw frames but **still unwraps on read** — otherwise your
  application would receive raw `{"header":…,"data":…}` bytes.
- Disabling stops spans and injection but **not** the envelope on an already-negotiated connection:
  the peer parses every frame as one. This module alone does not return to the zero-cost path on
  revocation; removing that wire overhead needs a redeploy.
- Running your own handshake? Offer or echo `SubprotocolOTelWS` and check `IsOTelNegotiated(raw)`
  before `NewConn` — see [otel-ws.md](../otel-ws.md).

> Full reference — every resolution table, connecting a relay with no application code, revocation
> latency, per-service targeting, and the operational summary:
> **[docs/feature-flags.md](../docs/feature-flags.md)** · 繁體中文:**[docs/feature-flags.zh-TW.md](../docs/feature-flags.zh-TW.md)**

### NewConn vs. Dial / Upgrader

The effective feature flag above gates whether tracing runs at all. Separately, whether the wire envelope gets written/read depends on **which constructor** created the `Conn` (and, for Dial/Upgrade, whether otel-ws was negotiated):

- **`NewConn(rawConn, opts...) (*Conn, error)`** wraps a `*websocket.Conn` you already dialed/upgraded yourself. It enables envelope wrapping **only when the raw connection's negotiated subprotocol proves `otel-ws`** — offer or echo `SubprotocolOTelWS` during your handshake, and check `IsOTelNegotiated(raw)` to verify. It returns an error when an `OTEL_*_ENABLED` variable it reads holds a value that is neither truthy nor falsy (wrapping `otelflags.ErrInvalidFlagValue`).
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
