# otel-gorilla-ws

**[English](README.md)**

---

`otel-gorilla-ws` 包裝 [gorilla/websocket](https://github.com/gorilla/websocket)，透過 WebSocket 訊息內容傳播 W3C Trace Context，加入 OpenTelemetry 分散式追蹤。

外送訊息使用共用的 envelope 格式（與 `otel-ws`、`otel-rxjs-ws` JS 套件相容）：

```json
{
  "header": { "traceparent": "...", "tracestate": "..." },
  "data": <original-payload>
}
```

若原始 payload 為合法 JSON，`data` 直接保留原值；非 JSON 位元組則編碼為 JSON 字串。

接收訊息支援兩種格式：
1. **Envelope 格式**（如上）— 新版 Go 與 JS client 使用。
2. **舊版扁平格式** — 相容舊版純 Go 部署：`{ "traceparent": "...", "tracestate": "...", ...fields }`。

## 安裝

```bash
go get github.com/akira-core/instrumentation-go/otel-gorilla-ws
```

## 使用方式

### Tracing 功能旗標

`otel-gorilla-ws` 支援：

- `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`（全域總開關）
- `OTEL_GORILLA_WS_TRACING_ENABLED`（ws 模組開關）

預設值：未設定即停用（opt-in）— 經 env 啟用時兩個變數都必須為 truthy。值為 `false/0/no/off`（不分大小寫）停用；其他已設定值（含空字串）視為 truthy。

停用時，send/receive span 與 envelope 注入/抽取皆關閉（直接委派 `*websocket.Conn`）。

#### Env × `WithTracingEnabled`（`featureEnabled`）

`NewConn`、`Dial`、`Upgrader.Upgrade` 的 `WithTracingEnabled(v bool)` 會針對該 `Conn` 覆寫兩個環境變數（`featureEnabled` — 是否跑任何 OTel SDK 路徑）。沒傳時聽 env。

| Env（`GLOBAL` ∧ `OTEL_GORILLA_WS_TRACING_ENABLED`） | `WithTracingEnabled` | 有效功能 |
|----------------------------------------------------|----------------------|----------|
| 關（未設或 falsy） | （無） | **關** |
| 關（未設或 falsy） | `true` | **開** |
| 關（未設或 falsy） | `false` | **關** |
| 開 | （無） | **開** |
| 開 | `false` | **關** |
| 開 | `true` | **開** |

對 `Dial`／`Upgrader.Upgrade`，**有效**功能旗標會在 handshake **之前**解析：關閉時不會 offer／confirm `otel-ws`（避免 wire 損壞）。`WithTracingEnabled(true)` 仍無法把 envelope 強加給未協商 otel-ws 的對端 — 那是 `Conn.tracingEnabled`（協商結果），與 `featureEnabled` 是兩個布林。

### NewConn 與 Dial / Upgrader 的差異

上述有效功能旗標控制 tracing 是否運作。至於 wire envelope 是否寫入/讀取，則取決於**建立 `Conn` 的建構子**（以及 Dial/Upgrade 是否協商到 otel-ws）：

- **`NewConn(rawConn, opts...)`** 包裝你自己已經 dial/upgrade 好的 `*websocket.Conn`。只要功能旗標開啟，無論 subprotocol 為何，一律啟用 envelope wrapping — 這是為了相容自行處理 handshake 的呼叫端而保留的行為。
- **`Dial(ctx, urlStr, requestHeader, subprotocols, opts...)`** 是符合規格的 client 進入點。它會在 handshake 中注入 `otel-ws` subprotocol；只有當伺服器以 `otel-ws`/`otel-ws+<proto>` subprotocol 確認支援時，才會啟用 envelope wrapping。
- **`Upgrader{}.Upgrade(w, r, responseHeader)`** 是符合規格的 server 進入點（對應 `websocket.Upgrader.Upgrade`）。它會偵測 client 提出的 subprotocol 清單中是否含有 `otel-ws`，並以 `otel-ws`/`otel-ws+<proto>` 回應；只有在此接受路徑下才會啟用 envelope wrapping。

對 `Dial`/`Upgrade` 而言，若對端未協商出 `otel-ws`，連線會靜默退回 passthrough 模式：只要功能旗標開啟，send/receive span 仍會建立，但不會在 wire 上寫入或讀取 envelope。

```go
raw, _, _ := websocket.DefaultDialer.DialContext(ctx, serverURL, nil)
conn := otelgorillaws.NewConn(raw)

_ = conn.WriteMessage(ctx, websocket.TextMessage, []byte("hello"))
recvCtx, msgType, data, _ := conn.ReadMessage(context.Background())
_, _ = recvCtx, msgType
_ = data
```

```go
// 支援 otel-ws 協商的符合規格 client/server 進入點：
conn, resp, err := otelgorillaws.Dial(ctx, wsURL, nil, []string{"json"})
// ...
upgrader := otelgorillaws.Upgrader{AppSubprotocols: []string{"json"}}
conn, err := upgrader.Upgrade(w, r, nil)
```

完整的 TracerProvider/propagator 初始化範例（在使用 `NewConn` 之前）請見 `examples/main.go`。

### 子協定協商設計筆記

完整的情境表格（涵蓋標準 WebSocket subprotocol 協商、`otel-ws` 隱藏協議注入機制，以及 `Dial`/`Upgrader` 在每種情境下的行為，包含伺服器回傳不支援/空協議等邊緣情況）請見 [`../otel-ws.md`](../otel-ws.md)。修改 `conn.go` 的 `Dial` 或 `upgrader.go` 的 `Upgrade` 協商邏輯時，請一併檢視該文件以保持同步。
