### 1. 標準 WebSocket Subprotocol 行為（無 OTEL-WS）

| 情境 | 客戶端類型 | 客戶端發送的 Subprotocol | 伺服器類型               | 伺服器回傳的 Subprotocol | Trace Propagation | 結果               | 說明與 RFC 6455 規範                                                                      |
| ---- | ---------- | ------------------------ | ------------------------ | ------------------------ | ----------------- | ------------------ | ----------------------------------------------------------------------------------------- |
| A    | ws client  | ""（無協議 / empty）     | ws server (support json) | false                    | -                 | **使用者決定結果** | 若客戶端未指定 sub-protocol，伺服器應拒絕（或不接受）。強制要求明確協議，避免未定義行為。 |
| B    | ws client  | "json"（或多個協議）     | ws server (support json) | "json"（第一個支援的）   | -                 | **連線成功**       | 伺服器選擇第一個它支援的協議回傳。若無支援則拒絕。                                        |

**相關規範重點**：根據 RFC 6455，客戶端可在 handshake 時提出多個 subprotocol，伺服器必須回傳其中一個它接受的（或不回傳表示拒絕）。空協議通常導致拒絕或降級處理。

### 2. OTEL-WS Client 行為（隱藏協議注入機制）

OTEL-WS Client 會在使用者指定的協議**最前面注入隱藏協議 `otel-ws`**，用來協商是否啟用分散式追蹤。

| 情境 | 客戶端類型     | 客戶端發送的 Subprotocol | 伺服器類型                 | 伺服器回傳的 Subprotocol | Trace Propagation | 結果               | 說明                                                                                  |
| ---- | -------------- | ------------------------ | -------------------------- | ------------------------ | ----------------- | ------------------ | ------------------------------------------------------------------------------------- |
| C    | otel-ws client | "otel-ws,json"           | ws server (support json)   | "json"                   | **Disabled**      | 連線成功，但無追蹤 | OTEL-WS Client 偵測到回傳的不是 `otel-ws+...` 前綴 → 停用追蹤，純透傳 payload。       |
| D    | otel-ws client | "otel-ws,json"           | ws server (support binary) | false                    | **Disabled**      | 連線成功，但無追蹤 | 伺服器不支援任何提出的協議 → OTEL-WS Client 偵測到 empty → 連線保持存活，降級為 passthrough（不強制關閉連線）。 |
| E    | otel-ws client | ""（無協議）             | ws server (support json)   | false                    | **Disabled**      | 連線成功，但無追蹤 | 不注入 `otel-ws`；握手維持空協議，OTEL-WS Client 只做轉傳。                            |

### 3. OTEL-WS Server 行為

OTEL-WS Server 會檢查收到的協議是否帶有 `otel-ws` 前綴，決定是否啟用追蹤。

| 情境 | 客戶端類型        | 客戶端發送的 Subprotocol | 伺服器類型     | 伺服器回傳的 Subprotocol | Trace Propagation | 結果                | 說明                                                                              |
| ---- | ----------------- | ------------------------ | -------------- | ------------------------ | ----------------- | ------------------- | --------------------------------------------------------------------------------- |
| F    | ws client（普通） | ""（無協議）             | otel-ws server | false                    | **Disabled**      | 連線成功，但無追蹤  | 不拒絕連線；OTEL-WS Server 只做轉傳。                                               |
| G    | otel-ws client    | "otel-ws,json"           | otel-ws server | "otel-ws+json"           | **Enabled**       | 連線成功 + 啟用追蹤 | 伺服器偵測到 `otel-ws` 前綴 → 啟用 trace propagation，並在線上回傳 `otel-ws+json`（`Conn.Subprotocol()` 會去除 `otel-ws+` 前綴，於應用層回傳 `json`）。 |
| H    | ws client（普通） | "json"                   | otel-ws server | "json"                   | **Disabled**      | 連線成功，但無追蹤  | 伺服器檢查輸入協議不含 OTEL 前綴 → 停用追蹤，正常透傳所有 payload（保持相容性）。 |

### 4. OTEL-WS 核心設計原則與邊緣情境總結

| 項目               | 描述                                               | 處理方式                                     | 優點 / 注意事項                           |
| ------------------ | -------------------------------------------------- | -------------------------------------------- | ----------------------------------------- |
| **隱藏協議注入**   | OTEL-WS Client 自動在最前面加入 `otel-ws`          | 格式如：`"otel-ws,json"` 或 `"otel-ws+json"` | 不影響原有協議清單，實現透明追蹤協商      |
| **協議前綴識別**   | Client 收到 `otel-ws+xxx` 或 Server 收到 `otel-ws` | 解析前綴後啟用追蹤                           | 成功協商後才啟用，避免不相容時的開銷      |
| **不相容降級**     | 對方不支援 OTEL 協議                               | 停用 trace propagation，純透傳               | 保持最大相容性，不會破壞原有連線          |
| **空協議處理**     | OTEL-WS client/server 任一方未提供 sub-protocol    | 允許連線並降級為 passthrough（不封裝）       | 不破壞既有連線，同時維持 send/receive span |
| **多協議支援**     | 客戶端可傳多個協議                                 | 伺服器回傳第一個支援的                       | 符合 RFC 6455 標準協商規則                |
| **Binary vs JSON** | 不同 payload 類型                                  | 協議名稱區分（json / binary）                | OTEL-WS 不干涉 payload 本身，只處理協議層 |

**邊緣情境與注意事項**：

- **如果伺服器只支援 binary**：OTEL-WS Client 注入 `otel-ws,json` 後通常會收到 empty → 連線保持存活，降級為 passthrough（不強制關閉連線）。
- **追蹤開銷**：僅在成功協商 `otel-ws` 前綴時啟用，避免不必要的 header 或 context 注入。
- **相容性優先**：OTEL-WS 設計目標是「不破壞原有非 OTEL WebSocket 連線」，降級時完全不影響功能。
- **安全性**：強制 sub-protocol 可減少攻擊面（例如防止未經驗證的連線）。

這個表格已涵蓋 Excalidraw 圖中所有矩形區塊、箭頭標籤（如 `""`、` "json"`、` "otel-ws,json"`）、側邊說明文字，以及 OTEL-WS 的隱藏注入與檢查邏輯。

### 5. Feature-flag 對協商的閘控(0.7.0+;0.9.0 起 relay 納入協商決策)

上述 C–H 情境全部以「該連線具備 envelope 能力」為前提。協商與 span 產生用的是**同一個運算式**,差別只在
**何時解析**——這個差別是整節的重點:

| 決策 | 決定什麼 | 由什麼決定 | 何時解析 |
| --- | --- | --- | --- |
| **negotiation** | Dial 是否 offer、Upgrade 是否 confirm `otel-ws` | effective tracing:`master && module`,每層都走 `relay > env > option > default` | handshake **之前一刻**,一次,終生不變 |
| **span gate** | 每次 `WriteMessage`/`ReadMessage` 是否建 span、是否 inject/extract | 同一個運算式 | **每次呼叫**,連線存活期間可變 |
| **capability** | 這條連線上是否可能跑到任何 OTel SDK 路徑(是否建真的 tracer) | `relayPossible \|\| (masterLocal && tracingLocal)` | 建構時,一次 |

協商表:

| handshake 當下 effective tracing | Dial 行為 | Upgrade 行為 | 結果 |
| --- | --- | --- | --- |
| **off** | 完全不注入 `otel-ws` token | 即使客戶端提出 `otel-ws` 也不回傳確認,改走一般協議選擇(等同情境 H) | 雙方都不封裝 envelope,純透傳 |
| **on** | 依情境 C–E 注入並判定 | 依情境 F–H 判定 | 原表格行為 |

**為什麼協商現在要看 relay。** 0.9.0 之前 relay 只能撤銷,所以把它排除在協商之外是零代價的。現在 relay
**兩個方向都有權威**:排除它會讓維運者剛剛打開的模組,在新連線上仍然沒有能力攜帶 trace context。
參見 `design.md` D9。

**代價是一個不對稱,兩個方向都要規劃:**

- **打開只影響之後建立的連線。** 一條在模組關閉期間建立的連線永遠不會取得 envelope,`WithTracingEnabled(true)`
  也救不回來——沒有協商 `otel-ws` 的對端不會去解析它。這種連線在 flag 打開後仍可產生**本地** span,
  只是無法 inject/extract。需要既有連線被追蹤就必須重連。
- **關閉會立刻停掉每條連線的 span 與 inject/extract,但不會停掉 envelope。** 已協商的連線仍照常寫出
  envelope(header 為空)、讀取端仍照常解包,因為對端還在把每個 frame 當 envelope 解析。**關閉停的是
  遙測,不是開銷**——要移除這份 wire 成本必須讓連線重連。

**為什麼 effective tracing off 時不能協商。** off 的一方不會解包 JSON envelope。若仍允許協商成功
(0.7.0 之前的行為),對端會封裝每一則訊息,而 off 端的 `ReadMessage` 直通路徑把原始
`{"header":...,"data":...}` bytes 交給應用層 —— 靜默資料損毀。

**反向不成立。** `WithTracingEnabled(true)` 或 relay 打開,都無法強迫未協商 otel-ws 的對端使用 envelope;
協商結果仍需雙方同意。

**沒有配置 relay 的部署,wire 行為與 `0.7.0` 完全相同。** `relayPossible` 為 false 時,effective tracing
就是 `master && module` 由環境變數與選項算出來的值,正是前一版的運算式。

**capability 只箝制寫入端。** 對端是否包 envelope 是 handshake 的**事實**,不是本端閘門有權管的事。所以一個
capability 關掉、卻包裝了已協商連線的 `Conn`(只有 `NewConn` 產得出這種狀態)會寫**原始幀** —— 安全,
因為對端的探測會退回 payload —— 但**讀取時仍然解包**。把讀取路徑綁在 capability 上,會把原始的
`{"header":...,"data":...}` bytes 交給應用層。

### 5.1 自行處理 handshake

`NewConn` 只在原始連線的 negotiated subprotocol 證明了 otel-ws 時才啟用 envelope,而且**沒有**「宣稱已協商」的逃生選項 —— 那會讓強制 envelope 的 wire 損毀回來。自行處理 handshake 的呼叫端要用這兩個導出符號:

```go
// 提出(client)或回應(server)這個 token
const SubprotocolOTelWS = "otel-ws"

// 驗證 NewConn 會不會在這條連線上啟用 envelope
func IsOTelNegotiated(conn *websocket.Conn) bool
```

```go
dialer := &websocket.Dialer{
    Subprotocols: []string{otelgorillaws.SubprotocolOTelWS, "json"},
}
raw, _, err := dialer.Dial(url, hdr)
if !otelgorillaws.IsOTelNegotiated(raw) {
    logger.Warn("otel-ws not negotiated; no WS trace propagation on this conn")
}
conn, err := otelgorillaws.NewConn(raw)
```

限制:原生的 `websocket.Dialer` / `Upgrader` 只能達成**裸 `otel-ws`** 形式,因為 gorilla 只原樣回應完全比對的項目。`otel-ws+<app>` 這種同時帶應用層協定的複合形式,只有本 package 的 `Upgrader.Upgrade` 產得出來。

## 6. Envelope 是保留結構

在一條協商了 `otel-ws` 的連線上,以下 JSON 外層結構是**保留的**:

```json
{"header": { ... }, "data": <payload>}
```

讀取端會把符合這個形狀的訊息當成 envelope 解開,把 `data` 交給應用層,外層結構捨棄。因此**應用層 payload 不得使用這個外層形狀** —— 否則會被無聲拆解。

收緊比對(例如要求 `header` 只能含 `traceparent` / `tracestate`)是刻意**不做**的:JS 端未來若在 header 多加一個成員,整包會掉進 legacy 分支,結果更糟。

反過來,不帶 trace key 的一般 JSON object 是**位元組透明**的:探測認不出它,就原樣回傳呼叫端的位元組,不會重排 key、不會正規化空白。做簽章驗證或雜湊的呼叫端因此安全。
