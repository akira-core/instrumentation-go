# otel-gorilla-ws

[繁體中文 (Traditional Chinese)](README.zh-TW.md)

---

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

`otel-gorilla-ws` reads two env vars; **all default to OFF when unset**:

| Variable | Tier | Default | Effect |
|---|---|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | global master | OFF | hard prerequisite for the per-module flag |
| `OTEL_GORILLA_WS_TRACING_ENABLED` | module tracing | OFF | wrapper send/receive spans + JSON envelope wrap/unwrap |

Truthy = any value other than `0`, `false`, `no`, `off` (case-insensitive, whitespace-trimmed). Cached for the process lifetime via `sync.Once`; env changes after the first gate read are ignored.

Priority:
1. Global off disables ws tracing regardless of module flag.
2. Otherwise module flag controls ws tracing.

When disabled, both send/receive spans and trace-context propagation are turned off — the wrapper delegates straight to `gorilla/websocket` with byte-identical wire output. 2-tier (no separate propagation flag) because the JSON envelope is constructed inline; subprotocol negotiation provides the same per-connection opt-out as a propagation flag.

---

## Internals overview

```
otel-gorilla-ws/
├── conn.go                 # facade Conn{*websocket.Conn; impl shared.ConnImpl}
├── upgrader.go message.go options.go env_flags.go version.go doc.go
└── internal/
    ├── flags/              # shared gate helper (byte-identical across all four modules)
    ├── shared/             # ConnImpl interface, envelope wire format (MarshalWire, TryUnmarshalWire)
    ├── direct/             # disabled-mode impl — ZERO go.opentelemetry.io/otel/sdk or otel/exporters imports
    └── traced/             # enabled-mode impl — full instrumentation + propagationEnabled gate
```

Constructor (`NewConn` / `Dial` / `Upgrade`) calls `wsTracingEnabled()` + subprotocol negotiation **once**, then picks `direct.NewConn` or `traced.NewConn`. Public `WriteMessage` / `ReadMessage` delegate via single-line `c.impl.<Method>(...)` calls — zero runtime gate branches in the hot path. Compile-time assertions `var _ shared.ConnImpl = (*direct.Conn)(nil)` / `(*traced.Conn)(nil)` in `conn.go` ensure new interface methods are implemented in both flavours.

```go
raw, _, _ := websocket.DefaultDialer.DialContext(ctx, serverURL, nil)
conn := otelgorillaws.NewConn(raw)

_ = conn.WriteMessage(ctx, websocket.TextMessage, []byte("hello"))
recvCtx, msgType, data, _ := conn.ReadMessage(context.Background())
_, _ = recvCtx, msgType
_ = data
```
