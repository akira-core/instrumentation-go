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
│   ├── env_flags.go        # tracing 功能旗標 gate（OTEL_INSTRUMENTATION_GO_TRACING_ENABLED + OTEL_NATS_TRACING_ENABLED）
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

`otel-nats`（`otelnats` + `oteljetstream`）支援：

- `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`（全域總開關）
- `OTEL_NATS_TRACING_ENABLED`（nats 模組開關）

預設值：**未設定即停用** — 兩個環境變數都必須明確設為 truthy 值才能啟用 tracing。值為 `false/0/no/off`（不分大小寫）視為停用；其他任何已設定的值皆視為啟用。

優先順序：
1. 全域關閉時，無論模組旗標為何，皆停用所有 nats tracing。
2. 否則由模組旗標控制 nats tracing。

停用時，span 建立與 W3C header 傳播皆會關閉。

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

可選：使用 **ConnectWithOptions** 並傳入 **WithTracerProvider(tp)** 或 **WithPropagators(p)** 覆寫 global。

### 3. Request/Reply

`Conn.Request` / `RequestWithContext` / `RequestMsg` / `RequestMsgWithContext` 完全對齊 `nats.Conn` 的同名方法，但會為這次 RPC 開啟一個 CLIENT span，並為回覆開啟第二個連結的 CLIENT span（`receive` — 依 OTel messaging span-kind 對照表，pull 屬於 CLIENT）：

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
| **ConnectWithOptions** | 可傳入 **WithTracerProvider(tp)**、**WithPropagators(p)**。 |
| **ConnectTLS** | `ConnectTLS(url, certFile, keyFile, caFile string, natsOpts ...nats.Option)`。以雙向 TLS 建立連線。 |
| **ConnectWithCredentials** | `ConnectWithCredentials(url, credFile string, natsOpts ...nats.Option)`。以 JWT/NKey 憑證建立連線。 |
| **ScopeName / Version()** | 建立 Tracer 時使用（OTel contrib 規範）。 |
| **Request / RequestWithContext / RequestMsg / RequestMsgWithContext** | 對齊 `nats.Conn` 的 RPC helper；為請求開啟 CLIENT span，並為回覆接收開啟一個連結的 CLIENT span。 |
| **JetStream consumer manager** | `JetStream` 完整包裝 `StreamConsumerManager`；`Stream` 完整包裝 `ConsumerManager`。所有回傳 `Consumer` 或 `PushConsumer` 的方法仍會回傳具 trace 包裝的型別（見 JetStream 章節）。 |
| **WithTraceDestination / SubscribeTraceEvents** | 將 NATS 2.11+ 的基礎設施追蹤事件轉換為 OTel span（見 **NATS 2.11+ 追蹤事件**）。 |
| **測試** | 在 Connect 前呼叫 `otel.SetTracerProvider(tp)`（必要時 `otel.SetTextMapPropagator(prop)`）。 |

---

## Span kind

Span kind 依 OTel messaging「Span kind」對照表（`send` → `PRODUCER`、`receive`（pull）→ `CLIENT`、`process`（push）→ `CONSUMER`）：

```
Publish / PublishMsg                     PRODUCER（send）
Request / RequestWithContext / ...       CLIENT（request，等待回覆）
  └── receive <reply-subject>            CLIENT（連結的回覆接收，pull）
Subscribe / QueueSubscribe handler       CONSUMER（process，push 遞送）

JetStream publish                        PRODUCER（send）
JetStream Consume handler                CONSUMER（process，push 遞送 callback）
JetStream Fetch / Messages / Next        CLIENT（連結的 receive，pull）
```

JetStream 的 `receive`／`process` span 另外帶有 `messaging.consumer.name`（durable/consumer 名稱）；core NATS 的 span 則不帶此屬性。

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
