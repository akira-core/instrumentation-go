# otel-nats（otelnats + oteljetstream）

**[English](README.md)**

---

為 [NATS](https://nats.io/) 與 [NATS JetStream](https://docs.nats.io/nats-concepts/jetstream) 提供 OpenTelemetry 追蹤，對齊官方 `nats.go` / `nats.go/jetstream` API，並在訊息 header 中傳播 W3C Trace Context。`oteljetstream` 已完整包裝 JetStream consumer 管理 API（`JetStream` 的 `StreamConsumerManager` 與 `Stream` 的 `ConsumerManager`），同時維持既有訊息 publish/consume 的 tracing 行為。依 [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation) 規範：套件僅透過 option 接受 **TracerProvider** 與 **Propagators**，不提供 InitTracer；由應用程式在啟動時設定 global provider 與 propagator（見 **examples/**）。

---

## 目錄結構

```
otel-nats/
├── otelnats/           # Core NATS：Connect、Conn、Publish、Subscribe、HeaderCarrier
├── oteljetstream/      # JetStream：New、JetStream、Stream、Consumer、PushConsumer、完整 consumer-manager 包裝、Consume、Messages、Fetch
├── examples/            # 如何建立 TracerProvider、設定 global、使用 otelnats/oteljetstream
├── go.mod
└── README.md
```

---

## 使用方式

### 功能旗標 (Feature flags)

`otel-nats`（`otelnats` + `oteljetstream`）讀取三個 env 變數；**未設值時一律預設關閉**：

| 變數 | 層級 | 預設 | 作用 |
|---|---|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | global master | OFF | 所有 per-module flag 之硬性前置 |
| `OTEL_NATS_TRACING_ENABLED` | module tracing | OFF | Publish / Subscribe / Request 及 JetStream consumer 路徑之 wrapper span |
| `OTEL_NATS_PROPAGATION_ENABLED` | module propagation | OFF | publish 時 W3C `traceparent` / `tracestate` header 注入 + subscribe 時 extract |

Truthy 值 = 任何不是 `0` / `false` / `no` / `off` 的字串（大小寫無關、trim 空白）。Process lifetime 內由 `sync.Once` 快取，第一次讀取後 env 異動將被忽略。

**四種可觀察狀態：**

| `OTEL_NATS_TRACING_ENABLED` | `OTEL_NATS_PROPAGATION_ENABLED` | Wrapper span | Wire `traceparent` |
|---|---|---|---|
| OFF（任意值） | （任意值） | — | — |
| ON | OFF（預設） | 有 | **無** |
| ON | ON | 有 | 有 |
| ON | falsy | 有 | 無 |

tracing 開但 propagation 關（新預設）時，wrapper span 仍會建立 — 適用於本地 fan-out 指標 / dead-letter 診斷 — 但 publish 不注入 header、subscribe 不執行 Extract。在下游 consumer 非 OTel-aware 或 wire size 為瓶頸時實用。

#### Propagation flag（自 v0.3.x 起之 env-var 行為變更）

先前僅啟用 tracing 而未顯式設 propagation flag 之部署，**必須加上** `OTEL_NATS_PROPAGATION_ENABLED=true` 才能維持 `traceparent` 注入 — 否則跨服務 trace 將於每個 NATS hop 處斷裂。尋找受影響配置：

```bash
grep -rE 'OTEL_NATS_TRACING_ENABLED' deploy/ config/ docker-compose*.yml
```

逐筆對 enable tracing 之設定加上 propagation env 變數。Before/after wire 輸出範例見 `CHANGELOG.md`。

仍可透過 `ConnectWithOptions(..., WithTracerProvider(tp), WithPropagators(p))` 做 per-connection 覆寫；tracing gate 關閉時此覆寫不生效。

### 1. 初始化 Provider 與 Propagator（應用程式負責）

在程式啟動時建立 TracerProvider（例如 OTLP）、設定 global provider 與 propagator 一次。完整可執行範例見 **examples/main.go**。

```go
otel.SetTracerProvider(tp)
otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{},
    propagation.Baggage{},
))
```

### 2. Core NATS：Connect、Publish、Subscribe

```go
conn, err := otelnats.Connect(natsURL, nil)
defer conn.Close()

conn.Publish(ctx, "subject", []byte("data"))
conn.Subscribe("subject", func(m otelnats.Msg) {
    // m.Msg、m.Context() — 從 header 解出的 trace
})
```

可選：使用 **ConnectWithOptions** 並傳入 **WithTracerProvider(tp)** 或 **WithPropagators(p)** 覆寫 global。

### 3. JetStream

```go
js, _ := oteljetstream.New(conn)
cons.Consume(func(m oteljetstream.Msg) {
    // m.Data()、m.Ack()、m.Context()
})

pushCons, _ := js.CreateOrUpdatePushConsumer(ctx, "MYSTREAM", oteljetstream.ConsumerConfig{
    Durable:        "push-consumer",
    DeliverSubject: "push.deliver",
    FilterSubject:  "events.push",
})
pushCons.Consume(func(m oteljetstream.Msg) {
    _ = m.Ack() // 與 pull consume 相同的 trace context 行為
})
```

### 4. 測試

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
| **JetStream consumer manager** | `JetStream` 完整包裝 `StreamConsumerManager`；`Stream` 完整包裝 `ConsumerManager`。所有回傳 `Consumer`/`PushConsumer` 的方法仍會回傳具 trace 包裝的型別。 |
| **ScopeName / Version()** | 建立 Tracer 時使用（OTel contrib 規範）。 |
| **測試** | 在 Connect 前呼叫 `otel.SetTracerProvider(tp)`（必要時 `otel.SetTextMapPropagator(prop)`）。 |
