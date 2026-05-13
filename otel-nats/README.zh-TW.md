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
