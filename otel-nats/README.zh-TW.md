# otel-nats（otelnats + oteljetstream）

**[English](README.md)**

---

為 [NATS](https://nats.io/) 與 [NATS JetStream](https://docs.nats.io/nats-concepts/jetstream) 提供 OpenTelemetry 追蹤，對齊官方 `nats.go` / `nats.go/jetstream` API，並在訊息 header 中傳播 W3C Trace Context。`oteljetstream` 已完整包裝 JetStream consumer 管理 API（`JetStream` 的 `StreamConsumerManager` 與 `Stream` 的 `ConsumerManager`），同時維持既有訊息 publish/consume 的 tracing 行為。依 [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation) 規範：套件僅透過 option 接受 **TracerProvider** 與 **Propagators**，不提供 InitTracer；由應用程式在啟動時設定 global provider 與 propagator（見 **examples/**）。

---

## 目錄結構

```
otel-nats/
├── otelnats/               # Core NATS：Connect、Conn、Publish、Subscribe、Request、HeaderCarrier
│   ├── connect.go          # Connect、ConnectWithOptions、ConnectTLS、ConnectWithCredentials
│   ├── conn.go             # Conn、connImpl 介面、Option（WithTracerProvider、WithPropagators、WithTraceDestination）
│   ├── conn_traced.go      # tracedConn：完整 instrumented 的 connImpl（span、propagation）
│   ├── conn_direct.go      # directConn：tracing 停用時使用的 passthrough connImpl
│   ├── traceevent.go       # WithTraceDestination / SubscribeTraceEvents / TraceEvent / TraceHop（NATS 2.11+ 追蹤事件）
│   ├── propagation.go      # HeaderCarrier（nats.Header ↔ TextMapCarrier）
│   ├── env_flags.go        # 本模組的 flag key、環境變數、預設值與 gateState
│   ├── internal/flags/     # 共用的 EnvEnabled/Gate helper（跨模組保持 byte-identical）
│   └── doc.go
├── oteljetstream/          # JetStream：New、JetStream、Stream、Consumer、Consume、Messages、Fetch
│   ├── jetstream.go        # New(conn)、JetStream 介面、共用型別（ConsumerConfig、StreamConfig 等）
│   ├── jetstream_traced.go # tracedJSImpl：完整 instrumented 的 JetStream 實作
│   ├── jetstream_direct.go # directJSImpl：passthrough 的 JetStream 實作
│   ├── stream.go           # Stream 介面（consumer-manager 方法）
│   ├── stream_traced.go    # tracedStream：完整 instrumented 的 Stream 實作
│   ├── stream_direct.go    # directStream：passthrough 的 Stream 實作
│   ├── consumer.go         # Consumer 介面、Msg、MessageBatch、MessagesContext
│   ├── consumer_traced.go  # tracedConsumer：帶 span 的 Consume/Messages/Next/Fetch
│   ├── consumer_direct.go  # directConsumer：passthrough 的 Consumer 實作
│   └── doc.go
├── examples/            # 如何建立 TracerProvider、設定 global、使用 otelnats/oteljetstream
├── go.mod
└── README.md
```

---

## 使用方式

### 追蹤功能旗標

```
tracing = master && natsTracing
```

每個開關沿著一道四階梯解析,最先表態的那一層贏:

```
relay  >  env  >  option(With*Enabled)  >  寫死的預設值
```

relay **兩個方向都有權威** —— 能關掉執行中的模組,也能打開部署原本沒開的模組。安全性來自**預設值**:
總開關 `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` 預設 `true` 且是**否決權**(只有 `false` 有效果,
且不接受任何 option),而每個 per-module 開關預設**關閉**。

**選項排在它的環境變數之下**,與 `0.7.0` 相反。即使 Go 程式碼傳了 `WithTracingEnabled(true)`,
``OTEL_NATS_TRACING_ENABLED`=false` 依然能關掉這個模組,所以維運者握有一個程式碼無法覆寫的單模組設定。變數未設定時
由選項決定,所以同一個 process 裡兩條連線仍然可以不同。

開關只由 `1`/`true`/`yes`/`on` 或 `0`/`false`/`no`/`off` 決定,未設定代表「沒有意見」。
**其他任何值——包含空字串——都會讓建構失敗**,錯誤包裹 `otelflags.ErrInvalidFlagValue`。

`WithTracingEnabled` **不會**把任何東西釘死:帶著它的 wrapper 每次操作仍然解析總開關與 relay。

互斥規則與 `ErrTracingConfigConflict` **已移除**:選項與變數同時出現是一般設定,變數贏。

訂閱與 JetStream consumer **每則訊息**重新解析,所以 flag 改變前建立的訂閱不用重建就會跟上。

> 完整參考 —— 全部解析表格、零程式碼連上 relay、撤銷延遲、針對單一服務的 targeting、維運速查:
> **[docs/feature-flags.zh-TW.md](../docs/feature-flags.zh-TW.md)** ·
> 教學:**[docs/otel-nats-kill-switch.zh-TW.html](../docs/otel-nats-kill-switch.zh-TW.html)** ·
> English:**[docs/feature-flags.md](../docs/feature-flags.md)** ·
> **[docs/otel-nats-kill-switch.en-US.html](../docs/otel-nats-kill-switch.en-US.html)**

### 1. 初始化 Provider 與 Propagator（應用程式負責）

在程式啟動時建立 TracerProvider（例如 OTLP）、設定 global provider 與 propagator 一次。完整可執行範例見 **examples/main.go**。

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/sdk/resource"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// 於 main：
tp, err := newTracerProvider() // 以 OTLP exporter + resource 建立
if err != nil { log.Fatal(err) }
defer func() { _ = tp.Shutdown(ctx) }()

otel.SetTracerProvider(tp)
otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{},
    propagation.Baggage{},
))
```

### 2. Core NATS：Connect、Publish、Subscribe

```go
import (
    "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

conn, err := otelnats.Connect(natsURL, nil)
if err != nil { log.Fatal(err) }
defer conn.Close()

conn.Publish(ctx, "subject", []byte("data"))
conn.Subscribe("subject", func(m otelnats.Msg) {
    // m.Msg、m.Context() — 從 header 解出的 trace
})
conn.QueueSubscribe("subject", "queue", handler)
```

可選：使用 **ConnectWithOptions** 並傳入 **WithTracerProvider(tp)**、**WithPropagators(p)** 或 **WithTracingEnabled(v bool)** 覆寫 global。

### 3. Request/Reply

`Conn.Request` / `RequestWithContext` / `RequestMsg` / `RequestMsgWithContext` 完全對齊 `nats.Conn` 的同名方法，但會為這次 RPC 開啟一個 CLIENT span（`request {subject}`），並為回覆開啟第二個連結的 CLIENT span —— 裸 `receive`，不帶 destination 區段，因為回覆送達的是自動產生、只用一次的 inbox（`_INBOX.<nuid>`），而 semconv v1.39.0 規定沒有低基數值可用時就省略 `{destination}`。inbox subject 仍可透過 `messaging.destination.name`、`messaging.destination.temporary=true`、`messaging.destination.anonymous=true` 與 `messaging.message.conversation_id` 查詢：

```go
reply, err := conn.RequestWithContext(ctx, "subject", []byte("ping"))
if err != nil { log.Fatal(err) }
// reply.Data — request/reply 的 trace context 記錄在 CLIENT span 上；
// 回覆本身則以連結（link）的 CLIENT「receive」span 記錄。
```

`Request` / `RequestMsg` 沒有 `context.Context` 參數（對齊 `nats.Conn`），因此其 producer span 以 `context.Background()` 為 parent — 若需要串接既有 trace，請改用 `RequestWithContext` / `RequestMsgWithContext`。

### 4. JetStream

```go
import (
    "github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
    "github.com/akira-core/instrumentation-go/otel-nats/otelnats"
)

conn, _ := otelnats.Connect(natsURL, nil)
defer conn.Close()

js, err := oteljetstream.New(conn)
// 建立 stream/consumer 之後：
cons.Consume(func(m oteljetstream.Msg) {
    // m.Data()、m.Ack()、m.Context() — 從訊息 header 解出的 trace
})
```

或以 `Messages()` 手動迭代：

```go
iter, err := cons.Messages()
if err != nil { log.Fatal(err) }
defer iter.Stop() // 釋放 iterator 的 goroutine 並結束尚在進行中的 span

for {
    ctx, msg, err := iter.Next()
    if err != nil { break } // iterator 已停止/耗盡
    _ = ctx // 從 msg header 解出的 trace context
    _ = msg.Ack()
}
```

> **Push consumer** 已被包裝（`JetStream` 與 `Stream` 上的 `PushConsumer`/`CreatePushConsumer`/`CreateOrUpdatePushConsumer`/`UpdatePushConsumer`）；回傳的 `PushConsumer.Consume` 會攜帶 trace context。純管理型 API（`PauseConsumer`/`ResumeConsumer`/`UnpinConsumer`）直接在 `Stream` 上以未追蹤的 passthrough 形式提供（`ResetConsumer`/`ResetConsumerToSequence` 未提供 — 需 nats.go v1.52.0，高於本模組釘選的 v1.50.0）；`Unwrap()` 僅存在於 `JetStream`，用於取用包裝器未再提供的 API（`KeyValue`/`ObjectStore`/`AccountInfo`/`Conn`/`Options`）。非同步 publish（`PublishAsync`/`PublishMsgAsync`）未被包裝：這兩個 API 不接受 `context.Context`，且回傳非阻塞的 `PubAckFuture` 而非同步 ack，與本包裝器的 context 傳播模型不相容（見 `oteljetstream/doc.go`）。

### 5. 測試

在 Connect 前設定 global provider（與必要時 propagator），無需 InitTracer。

```go
otel.SetTracerProvider(tp)
otel.SetTextMapPropagator(prop) // 測試傳播時
conn, err := otelnats.Connect(url, nil)
```

---

## API 摘要

| 項目 | 說明 |
|------|------|
| **Connect** | 使用 `otel.GetTracerProvider()` 與 `otel.GetTextMapPropagator()`；可透過 ConnectWithOptions 以 option 覆寫。 |
| **ConnectWithOptions** | 可傳入 **WithTracerProvider(tp)**、**WithPropagators(p)**、**WithTracingEnabled(v bool)**。 |
| **ConnectTLS** | `ConnectTLS(url, certFile, keyFile, caFile string, natsOpts ...nats.Option)`。以雙向 TLS 建立連線。 |
| **ConnectWithCredentials** | `ConnectWithCredentials(url, credFile string, natsOpts ...nats.Option)`。以 JWT/NKey 憑證建立連線。 |
| **ScopeName / Version()** | 建立 Tracer 時使用（OTel contrib 規範）。 |
| **Request / RequestWithContext / RequestMsg / RequestMsgWithContext** | 對齊 `nats.Conn` 的 RPC helper；為請求開啟 CLIENT span，並為回覆接收開啟一個連結的 CLIENT span。 |
| **JetStream consumer manager** | `JetStream` 完整包裝 `StreamConsumerManager`；`Stream` 完整包裝 `ConsumerManager`。所有回傳 `Consumer` 或 `PushConsumer` 的方法仍會回傳具 trace 包裝的型別（見 JetStream 章節）。 |
| **WithTraceDestination / SubscribeTraceEvents** | 將 NATS 2.11+ 的基礎設施追蹤事件轉換為 OTel span（見 **NATS 2.11+ 追蹤事件**）。 |
| **Inbox span 名稱** | 解析出的 destination 若是無界的回覆 inbox，該 span 的名稱去掉 destination（`publish`、`process`、`receive`），inbox 保留在屬性上。JetStream 亦適用 —— stream 可能捕捉 inbox subject（見 **Span 名稱**）。 |
| **`Conn.InboxPrefixes()`** | 本連線認得的 inbox prefix（`0.9.1+`）。供 `oteljetstream` 使用；應用程式很少需要。 |
| **測試** | 在 Connect 前呼叫 `otel.SetTracerProvider(tp)`（必要時 `otel.SetTextMapPropagator(prop)`）。 |

---

## Span kind

Span kind 依 OTel messaging「Span kind」對照表（`send` → `PRODUCER`、`receive`（pull）→ `CLIENT`、`process`（push）→ `CONSUMER`）：

```
Publish / PublishMsg                     PRODUCER（send）
Request / RequestWithContext / ...       CLIENT（request，等待回覆）
  └── receive                            CLIENT（連結的回覆接收，pull；裸名，不帶 destination）
Subscribe / QueueSubscribe handler       CONSUMER（process，push 遞送）

JetStream publish                        PRODUCER（send）
JetStream Consume handler                CONSUMER（process，push 遞送 callback）
JetStream Fetch / Messages / Next        CLIENT（連結的 receive，pull）
```

JetStream 的 `receive`／`process` span 另外帶有 `messaging.consumer.group.name`（durable/consumer 名稱）；core NATS 的 span 則不帶此屬性。

---

## Span 名稱

Span 名稱遵循 OTel messaging semconv v1.39.0 的格式 `{messaging.operation.name} {destination}`：

| 操作 | Span 名稱 | 備註 |
|---|---|---|
| Publish（core NATS 或 JetStream） | `publish {subject}` | `0.9.0` 之前是 `send {subject}` |
| Request | `request {subject}` | `0.9.0` 之前是 `{subject} request` |
| 回覆接收 | `receive` | 裸名，不帶 destination —— inbox 自動產生且只用一次；`0.9.0` 之前是 `receive {inbox}` |
| 發布到回覆 inbox | `publish` | 裸名 —— 手動回覆的那一半，`conn.Publish(msg.Reply, …)` |
| Subscribe/QueueSubscribe handler | `process {destination}` | |
| 訂閱 inbox 的 handler | `process` | 裸名 —— 手動請求的那一半 |
| JetStream consumer receive/process | `receive {destination}` / `process {destination}` | `0.9.1` 起 inbox 判斷也適用 —— stream 可能捕捉 inbox subject |
| JetStream 走捕捉 inbox 的 stream | `receive` / `process` / `publish` | 解析出的 destination 是無界 inbox 時為裸名 |

`{destination}` 的解析順序：訂閱 subject 或單一值的 JetStream consumer filter subject → 具體訊息 subject。解析結果與具體 subject 不同時（wildcard 訂閱或 filter），額外記錄 `messaging.destination.template`；`messaging.destination.name` 一律保留具體 subject。兩者都是 library 已經握有的事實 —— 它不會去猜 subject 的哪一段是識別碼。

解析出的 destination 若是**無界的回覆 inbox**，就整段從 span 名稱移除，對應 semconv「沒有低基數值可用時省略 `{destination}`」的規定。inbox 在屬性上仍完全可查：`messaging.destination.name`、`messaging.message.conversation_id`、`messaging.destination.temporary=true`、`messaging.destination.anonymous=true`。

關鍵在「無界」。**只由 inbox prefix 加 wildcard 構成**的 filter —— `_INBOX.>`，正是歸檔回覆的 consumer 會宣告的形狀 —— 是訂閱端自己選定的固定字串，因此保留在 span 名稱中並記錄為 `messaging.destination.template`。semconv 把 temporary/anonymous 的排除條款掛在 `messaging.destination.name`（`{destination}` 的**第二**順位）上，而不是掛在 `messaging.destination.template`（第一順位）上。含有字面 token 的 filter（如 `_INBOX.<nuid>.>`）則是逐請求產生的，與具體 inbox 一樣移除。無論名稱保留與否，temporary/anonymous/`conversation_id` 三個標記都照樣記錄：它們描述的是這次投遞，不是名稱。

inbox 以 subject prefix 辨識，且**認兩個 prefix**：本連線自己的（`nats.CustomInboxPrefix(p)` ⇒ `p + "."`）以及永遠認預設的 `_INBOX.`。只認本地 prefix 會在使用 custom prefix 的部署失效 —— responder 在 `msg.Reply` 看到的是**請求端**的 inbox，而請求端才是會換 prefix 的一方：給它 `subscribe: _INBOX.>` 等於讓它收得到所有其他 client 的回覆，而 responder 完全不需要 inbox 權限。

### 殘餘的 span 名稱基數

library **看得見**的無界 span 名稱都已由上述規則收斂。剩兩個來源，兩者在結構上都是它看不到的：

**使用本連線不認得的 custom inbox prefix 的對端。** 兩端各用**不同** custom prefix 時，光憑具體 subject 彼此認不出來。回覆接收 span 不受影響 —— 那條路徑在結構上就知道自己拿的是 inbox，不管 prefix 是什麼 —— 任何固定的訂閱或 consumer filter 也不受影響，因為宣告出來的 filter 無論 prefix 為何都是有界的。剩下的是手動 `conn.Publish(peerInbox, …)`，以及直接訂閱在外來 prefix inbox 上的 handler。

**內嵌識別碼的 subject。** 像 `orders.12345.created` 這種 subject，本模組**不會**替它產生樣板：沒有任何 library 能判斷哪一段是識別碼，而 semconv 允許記錄「已知的」`messaging.destination.template`，不允許推導一個出來。兩種情況會維持高基數：

- publish 與 request span，沒有訂閱或 filter 可以解析；以及
- **沒有** filter、或有**多個** wildcard filter subject 的 JetStream consumer。

兩者都交給下游改寫 —— OTel Collector 的 `span` processor 不需要動應用程式碼：

```yaml
span/to_attributes:
  name:
    to_attributes:
      rules:
        # 內嵌識別碼的 subject。
        - ^receive orders\.(?P<orderId>[^.]+)\.created$
        # 本連線不認得的外來 custom inbox prefix。
        - ^(?P<op>publish|process) SVCB\.(?P<inbox>[^.]+)
# "receive orders.12345.created" -> "receive orders.{orderId}.created"，orderId=12345
```

### 同一個操作的三種寫法

一個 publish span 上會同時出現三種拼法，全都是 semconv 要求的，沒有一個是 bug：

| | 值 | 為什麼 |
|---|---|---|
| `messaging.operation.type` | `send` | **固定列舉**：`create`、`send`、`receive`、`process`、`settle` |
| `messaging.operation.name` | `publish`（或 `request`） | 系統**自己的動詞** —— NATS 叫它 Publish |
| span 名稱 | `publish {subject}` | semconv 的 `{messaging.operation.name} {destination}` |

span 名稱跟隨 `operation.name`，不是 `operation.type`。

---

## NATS 2.11+ 追蹤事件

NATS server 2.11+ 可以為任何帶有 `Nats-Trace-Dest` header 的訊息發布基礎設施層級的追蹤事件（ingress、egress、JetStream store、subject-mapping、stream-export、service-import）。`otel-nats` 能消費這些事件並將每個 hop 轉換為一個 OTel span。

### Producer：設定追蹤目的地

```go
conn, err := otelnats.ConnectWithOptions(natsURL, nil,
    otelnats.WithTraceDestination("nats.trace.events"),
)
```

當 tracing 啟用時，透過 `conn.Publish`/`conn.PublishMsg` 送出的每則訊息都會帶上 `Nats-Trace-Dest` header，於是 server 會針對訊息經過的每個 hop，將 `TraceEvent` payload 發布到 `nats.trace.events`。

### Consumer：將事件轉換為 span

```go
sub, err := otelnats.SubscribeTraceEvents(conn, "nats.trace.events")
if err != nil { log.Fatal(err) }
defer sub.Unsubscribe()
```

每個 `otelnats.TraceEvent` payload 對應一台 server，內含一組 `otelnats.TraceHop`。`SubscribeTraceEvents` 會為每個 hop 產生一個時間點 span（命名為 `nats.<KIND>.<type>`，例如 `nats.CLIENT.ingress`），並透過事件請求 header 中內嵌的 `traceparent` 連結回原始 publisher span。

需要 NATS server 2.11+。`SubscribeTraceEvents` 只有在該連線的 tracing gate 開啟時才會發出 span；tracing 停用時則會捨棄事件（訂閱本身仍會成功，因此 `Unsubscribe` 的生命週期管理不受影響）。

---

## MessageBatch（`Fetch` / `FetchBytes` / `FetchNoWait`）

迭代 `Messages()` 以取得每則訊息及其解出的 trace context。每個 batch 應在下一次 `Fetch` 前完整耗盡 channel。

```go
batch, err := consumer.Fetch(10)
if err != nil { ... }
for m := range batch.Messages() {
    _ = m.Context()
    _ = m.Ack()
}
if err := batch.Error(); err != nil { ... }
```

`MessageBatch.Stop()` 會釋放內部的 goroutine，並結束任何尚在進行中的 span。若呼叫端完整耗盡 channel 直到關閉，則不需呼叫它；若呼叫端在 channel 關閉前就 `break`/`return`，**必須**呼叫它（通常透過 `defer`）以避免 goroutine 與最後一個 consumer span 洩漏：

```go
batch, err := consumer.Fetch(10)
if err != nil { ... }
defer batch.Stop()

for m := range batch.Messages() {
    if shouldStopEarly(m) {
        break // deferred 的 batch.Stop() 會結束進行中的 span 並停止 goroutine
    }
    _ = m.Context()
    _ = m.Ack()
}
```

---

## 診斷日誌

使用 [`log/slog`](https://pkg.go.dev/log/slog) — 預設無輸出。

| 等級 | 事件 |
|-------|------|
| `DEBUG` | `serverAttrsFromConn` 中的伺服器位址解析失敗；收到追蹤事件（`traceevent.go`） |
| `WARN` | 追蹤事件 JSON 解碼失敗（`traceevent.go`） |

啟動時啟用 debug 等級的 slog handler：

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

Log 項目使用 `otelnats:` 前綴。連線相關的 log（`conn.go`）使用 `addr`、`error`；追蹤事件的 log（`traceevent.go`）使用 `raw`、`server`、`hops`、`events`、`error`、`request_headers`。

---

## Dependencies

- `github.com/nats-io/nats.go`（含 JetStream）
- `go.opentelemetry.io/otel` 及其 SDK（trace、propagation）
- Go 1.24+

測試使用 `github.com/stretchr/testify`，整合測試使用 `nats-server/v2`。
