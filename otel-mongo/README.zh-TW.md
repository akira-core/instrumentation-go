# otel-mongo（otelmongo）

**[English](README.md)**

---

以 [MongoDB Go Driver](https://www.mongodb.com/docs/drivers/go/current/) 為基礎的 OpenTelemetry 包裝。寫入時將 **W3C Trace Context** 注入文件的 **`_oteltrace`** 欄位，讀取時還原，使同一條 trace 可跨服務延續。依 [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation) 規範：套件僅透過 option 接受 **TracerProvider** 與 **Propagators**，不提供 InitTracer；由應用程式在啟動時設定 global provider 與 propagator（見 **example/**）。

支援兩種 driver 版本（Go 慣例：v2 使用 import path `.../v2`）：
- **v2**：`import "github.com/Marz32onE/instrumentation-go/otel-mongo/v2"`（MongoDB driver v2，建議）
- **v1**：`import "github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo"`（MongoDB driver v1）

---

## 目錄結構

```
otel-mongo/
├── otelmongo/     # MongoDB driver v1 包裝（root module）
├── v2/             # MongoDB driver v2 包裝（import .../v2）
├── example/        # 使用 v2 的範例
└── README.md
```

- **Trace 儲存：** 寫入/更新的文件會有保留欄位 **`_oteltrace`**。對 raw BSON（例如 change stream）可使用 **ContextFromDocument(ctx, raw)** 還原 context。
- **兩層：** (1) **Driver** 使用 contrib otelmongo Monitor 產生連線/指令 span。(2) **Document** 層在 CRUD 寫入時注入 `_oteltrace`，讀取時支援 span link 與傳播。

---

## 使用方式

### 1. 初始化 Provider 與 Propagator（應用程式負責）

見 **example/main.go**：建立 TracerProvider（如 OTLP）、設定 `otel.SetTracerProvider(tp)` 與 `otel.SetTextMapPropagator(prop)`、defer shutdown。

### 2. Connect 與 CRUD

**v2**：`import ".../v2"`，`otelmongo.Connect(options.Client().ApplyURI(uri))`（無 ctx）。

**v1**：`import ".../otelmongo"`，`otelmongo.Connect(ctx, options.Client().ApplyURI(uri))`。

```go
// v2 範例
client, err := otelmongo.Connect(options.Client().ApplyURI(uri))
defer client.Disconnect(ctx)

db := client.Database("mydb")
coll := db.Collection("mycoll")
// InsertOne、Find、UpdateOne 等會自動處理 _oteltrace
```

可選：**ConnectWithOptions(traceOpts, mongoOpts)** 傳入 **WithTracerProvider(tp)** 或 **WithPropagators(p)**。

### 3. 從文件還原 trace（例如 change stream）

需與寫入相同的 propagation 環境變數（`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`、`OTEL_MONGO_PROPAGATION_ENABLED`，或 global 開啟時用 `ConnectWithOptions` 的 `WithTracePropagationEnabled`）。當 gates 關閉時，`ContextFromDocument` / `ContextFromRawDocument` 會回傳零值或不變的 ctx — 忽略 `ok` 回傳值的舊呼叫端會靜默變成 no-op。

```go
if sc, ok := otelmongo.ContextFromDocument(ctx, fullDoc); ok {
	next := trace.ContextWithRemoteSpanContext(ctx, sc)
	_ = next // 用於後續 span 或轉發（例如 NATS）
}
```

### 4. 測試

```go
otel.SetTracerProvider(trace.NewTracerProvider())
client, err := otelmongo.Connect(opts)
```

### 5. 降低高頻 driver spans（例如 `getMore`）

MongoDB driver monitor（`contrib otelmongo.NewMonitor`）會替所有 command 建立 span，包含游標常見的 `getMore`。

可使用 `SkipDBOperationsExporter` 在 export 前過濾指定 DB 操作：

```go
exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint))
if err != nil { log.Fatal(err) }

// 過濾 db.operation.name（大小寫不敏感）
exp = otelmongo.SkipDBOperationsExporter(exp, []string{"getMore"})

tp := sdktrace.NewTracerProvider(
    sdktrace.WithBatcher(exp),
    sdktrace.WithResource(res),
)
otel.SetTracerProvider(tp)
```

此過濾只影響匯出的 spans，不改變 CRUD 行為與 `_oteltrace` 傳播機制。

---

## API 摘要

| 項目 | 說明 |
|------|------|
| **Connect / ConnectWithOptions** | 未傳入 option 時使用 `otel.GetTracerProvider()`。 |
| **NewClient** | 可選 **WithTracerProvider**、**WithPropagators**。 |
| **ContextFromDocument** | 從文件的 `_oteltrace` 還原 trace context。 |
| **ScopeName / Version()** | 建立 Tracer 時使用（OTel contrib 規範）。 |
| **SkipDBOperationsExporter** | 包裝 `SpanExporter`，依 `db.operation.name` 略過匯出（僅影響匯出）。 |
