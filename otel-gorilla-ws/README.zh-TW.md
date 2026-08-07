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
effective tracing = master && wsTracing        （各自走完整階梯）
negotiation       = effective tracing          （handshake 前解析一次）
span gate         = effective tracing          （每次讀寫重讀）
```

每個開關沿著一道四階梯解析,最先表態的那一層贏:

```
relay  >  env  >  option(WithTracingEnabled)  >  寫死的預設值
```

relay **兩個方向都有權威**。安全性來自預設值:總開關 `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` 預設
`true` 且是**否決權**(只有 `false` 有效果,且不接受 option),而 `OTEL_GORILLA_WS_TRACING_ENABLED`
預設**關閉**。

**選項排在它的環境變數之下**,與 `0.7.0` 相反。即使 Go 程式碼傳了 `WithTracingEnabled(true)`,
`OTEL_GORILLA_WS_TRACING_ENABLED=false` 依然能關掉這個模組。變數未設定時由選項決定。

開關只由 `1`/`true`/`yes`/`on` 或 `0`/`false`/`no`/`off` 決定,未設定代表「沒有意見」。
**其他任何值——包含空字串——都會讓建構失敗。** 互斥規則與 `ErrTracingConfigConflict` **已移除**。

**negotiation 與 span gate 是同一個運算式,只是解析時機不同。** 這是本模組全部的微妙之處:handshake 無法
重來,所以線路決策只做一次,而 span 決策每次呼叫都做。由此產生的不對稱必須事先規劃:

- **打開只影響之後建立的連線。** 在模組關閉期間建立的連線永遠不會取得 envelope,`WithTracingEnabled(true)`
  也救不回來 —— 沒有協商 `otel-ws` 的對端不會去解析它。這種連線仍可產生**本地** span,只是無法
  inject/extract。需要它被追蹤就必須重連。
- **關閉會立刻影響每條連線的 span 與 inject/extract,但不影響 envelope**,因為對端還在解析它。
  這是唯一關掉後回不到零成本路徑的模組;要移除那份 wire 開銷必須讓連線重連。

**沒有配置 relay 的部署**,協商結果與 `0.7.0` 完全相同,wire 一個 byte 都不會變。

三件要知道的事:

- capability(本地的 `relayPossible || (master && module)`,建構時固定)只箝制**寫入**路徑。對端是否包
  envelope 是 handshake 的事實,所以 capability 關掉、卻包裝了已協商連線的 wrapper 會寫原始幀,但
  **讀取時仍然解包** —— 否則你的應用程式會收到原始的 `{"header":…,"data":…}` bytes。
- 關閉會停掉 span 與 injection,但**不會**停掉已協商連線上的 envelope:對端把每一幀都當 envelope 解析。
- 自己處理 handshake?提出或回應 `SubprotocolOTelWS`,並在 `NewConn` 前用 `IsOTelNegotiated(raw)` 檢查 ——
  見 [otel-ws.md](../otel-ws.md)。

> 完整參考 —— 全部解析表格、零程式碼連上 relay、撤銷延遲、針對單一服務的 targeting、維運速查:
> **[docs/feature-flags.zh-TW.md](../docs/feature-flags.zh-TW.md)** · English:**[docs/feature-flags.md](../docs/feature-flags.md)**

### NewConn 與 Dial / Upgrader 的差異

上述有效功能旗標控制 tracing 是否運作。至於 wire envelope 是否寫入/讀取，則取決於**建立 `Conn` 的建構子**（以及 Dial/Upgrade 是否協商到 otel-ws）：

- **`NewConn(rawConn, opts...) (*Conn, error)`** 包裝你自己已經 dial/upgrade 好的 `*websocket.Conn`。**只有在原始連線協商出的 subprotocol 證明了 `otel-ws` 時**才啟用 envelope wrapping —— 在你的 handshake 裡提出或回應 `SubprotocolOTelWS`,並用 `IsOTelNegotiated(raw)` 驗證。它讀取的 `OTEL_*_ENABLED` 變數若持有既非真值也非假值的內容,回傳錯誤(包裝 `otelflags.ErrInvalidFlagValue`)。
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
