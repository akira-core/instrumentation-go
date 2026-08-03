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

```
capability = gate1 && OTEL_GORILLA_WS_TRACING_ENABLED        （建構時固定）
span gate  = capability && relay otel-gorilla-ws-tracing      （每次呼叫重讀）
```

`gate1` 是 `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` **或** `WithTracingEnabled(v)` —— 同一個開關的兩種
拼法,**兩個都給是設定錯誤**(`ErrTracingConfigConflict`)。開關只有設成 `1`、`true`、`yes`、`on` 才算開。

**capability** 決定是否提出(`Dial`)或確認(`Upgrader.Upgrade`)`otel-ws`,在 handshake **之前**解析,
因為 handshake 無法重來。它刻意排除 relay —— 在只能撤銷的 relay 下這零代價。**span gate** 是疊在上面的
relay verdict,每次讀寫重讀,所以執行中的連線會跟上撤銷。

三件要知道的事:

- capability 只箝制**寫入**路徑。對端是否包 envelope 是 handshake 的事實,所以 capability 關掉、卻包裝了
  已協商連線的 wrapper 會寫原始幀,但**讀取時仍然解包** —— 否則你的應用程式會收到原始的
  `{"header":…,"data":…}` bytes。
- 撤銷會停掉 span 與 injection,但**不會**停掉已協商連線上的 envelope:對端把每一幀都當 envelope 解析。
  這是唯一撤銷後回不到零成本路徑的模組;要移除那個 wire 開銷必須重新部署。
- 自己處理 handshake?提出或回應 `SubprotocolOTelWS`,並在 `NewConn` 前用 `IsOTelNegotiated(raw)` 檢查 ——
  見 [otel-ws.md](../otel-ws.md)。

> 完整參考 —— 全部解析表格、零程式碼連上 relay、撤銷延遲、針對單一服務的 targeting、維運速查:
> **[feature-flags.zh-TW.md](../feature-flags.zh-TW.md)** · English:**[feature-flags.md](../feature-flags.md)**

### NewConn 與 Dial / Upgrader 的差異

上述有效功能旗標控制 tracing 是否運作。至於 wire envelope 是否寫入/讀取，則取決於**建立 `Conn` 的建構子**（以及 Dial/Upgrade 是否協商到 otel-ws）：

- **`NewConn(rawConn, opts...) (*Conn, error)`** 包裝你自己已經 dial/upgrade 好的 `*websocket.Conn`。**只有在原始連線協商出的 subprotocol 證明了 `otel-ws` 時**才啟用 envelope wrapping —— 在你的 handshake 裡提出或回應 `SubprotocolOTelWS`,並用 `IsOTelNegotiated(raw)` 驗證。設定矛盾時回傳錯誤(`ErrTracingConfigConflict`)。
- **`Dial(ctx, urlStr, requestHeader, subprotocols, opts...)`** 是符合規格的 client 進入點。它會在 handshake 中注入 `otel-ws` subprotocol；只有當伺服器以 `otel-ws`/`otel-ws+<proto>` subprotocol 確認支援時，才會啟用 envelope wrapping。
- **`Upgrader{}.Upgrade(w, r, responseHeader)`** 是符合規格的 server 進入點（對應 `websocket.Upgrader.Upgrade`）。它會偵測 client 提出的 subprotocol 清單中是否含有 `otel-ws`，並以 `otel-ws`/`otel-ws+<proto>` 回應；只有在此接受路徑下才會啟用 envelope wrapping。

對 `Dial`/`Upgrade` 而言，若對端未協商出 `otel-ws`，連線會靜默退回 passthrough 模式：只要功能旗標開啟，send/receive span 仍會建立，但不會在 wire 上寫入或讀取 envelope。

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
// 支援 otel-ws 協商的符合規格 client/server 進入點：
conn, resp, err := otelgorillaws.Dial(ctx, wsURL, nil, []string{"json"})
// ...
upgrader := otelgorillaws.Upgrader{AppSubprotocols: []string{"json"}}
conn, err := upgrader.Upgrade(w, r, nil)
```

完整的 TracerProvider/propagator 初始化範例（在使用 `NewConn` 之前）請見 `examples/main.go`。

### 子協定協商設計筆記

完整的情境表格（涵蓋標準 WebSocket subprotocol 協商、`otel-ws` 隱藏協議注入機制，以及 `Dial`/`Upgrader` 在每種情境下的行為，包含伺服器回傳不支援/空協議等邊緣情況）請見 [`../otel-ws.md`](../otel-ws.md)。修改 `conn.go` 的 `Dial` 或 `upgrader.go` 的 `Upgrade` 協商邏輯時，請一併檢視該文件以保持同步。
