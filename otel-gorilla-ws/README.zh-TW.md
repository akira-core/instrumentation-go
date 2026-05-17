# otel-gorilla-ws

**[English](README.md)**

---

`otel-gorilla-ws` 包裝 [gorilla/websocket](https://github.com/gorilla/websocket)，於 WebSocket 訊息內容中以 W3C Trace Context 進行 OpenTelemetry 分散式追蹤傳播。

外送訊息使用共享 envelope 格式（與 JS 套件 `otel-ws`、`otel-rxjs-ws` 相容）：

```json
{
  "header": { "traceparent": "...", "tracestate": "..." },
  "data": <original-payload>
}
```

`data` 若為合法 JSON 則原樣保留；非 JSON bytes 則以 JSON 字串編碼。

接收訊息支援兩種格式：
1. **Envelope 格式**（如上）— 新版 Go 與 JS client 採用。
2. **Legacy flat 格式** — 與舊版 Go-only 部署回相容：`{ "traceparent": "...", "tracestate": "...", ...fields }`。

## 安裝

```bash
go get github.com/Marz32onE/instrumentation-go/otel-gorilla-ws
```

## 使用方式

### 功能旗標 (Feature flags)

`otel-gorilla-ws` 讀取兩個 env 變數；**未設值時一律預設關閉**：

| 變數 | 層級 | 預設 | 作用 |
|---|---|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | global master | OFF | per-module flag 之硬性前置 |
| `OTEL_GORILLA_WS_TRACING_ENABLED` | module tracing | OFF | wrapper send/receive span + JSON envelope 包裝/還原 |

Truthy = 任何非 `0` / `false` / `no` / `off` 的值（不分大小寫、會 trim 空白）。process 內以 `sync.Once` 快取；首次 gate 讀取後變更 env 不會生效。

優先順序：
1. 全域 off 時，無論 module flag 為何皆停用 ws tracing。
2. 否則由 module flag 決定 ws tracing。

關閉時 send/receive span 與 trace-context 傳播皆停用 — wrapper 直接委派給 `gorilla/websocket`，wire 輸出 byte-identical。採 2-tier（無獨立 propagation flag）係因 JSON envelope 為 inline 構建；subprotocol 協商提供與 propagation flag 相當的 per-connection opt-out 機制。

---

## 內部結構 (Internals overview)

```
otel-gorilla-ws/
├── conn.go                 # facade Conn{*websocket.Conn; impl shared.ConnImpl}
├── upgrader.go message.go options.go env_flags.go version.go doc.go
└── internal/
    ├── flags/              # 共享 gate helper（四模組 byte-identical）
    ├── shared/             # ConnImpl interface、envelope wire 格式（MarshalWire、TryUnmarshalWire）
    ├── direct/             # disabled-mode 實作 — ZERO go.opentelemetry.io/otel/sdk 與 otel/exporters import
    └── traced/             # enabled-mode 實作 — 完整 instrumentation + propagationEnabled gate
```

Constructor（`NewConn` / `Dial` / `Upgrade`）僅呼叫 `wsTracingEnabled()` 與 subprotocol 協商**一次**，然後挑選 `direct.NewConn` 或 `traced.NewConn`。Public `WriteMessage` / `ReadMessage` 皆為單行 `c.impl.<Method>(...)` 委派 — hot path 無 runtime gate 分支。`conn.go` 內的編譯期斷言 `var _ shared.ConnImpl = (*direct.Conn)(nil)` / `(*traced.Conn)(nil)` 確保新 interface method 在兩種 impl 都實作。

```go
raw, _, _ := websocket.DefaultDialer.DialContext(ctx, serverURL, nil)
conn := otelgorillaws.NewConn(raw)

_ = conn.WriteMessage(ctx, websocket.TextMessage, []byte("hello"))
recvCtx, msgType, data, _ := conn.ReadMessage(context.Background())
_, _ = recvCtx, msgType
_ = data
```

---

## 版本

以 `otel-gorilla-ws/v<x.y.z>` tag 發版。版本常數位於 `version.go`。任何程式碼變更於推送 release tag 前須先 bump（per-package event-driven bump 政策）。
