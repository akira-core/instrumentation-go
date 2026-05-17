# otel-mongo（otelmongo）

**[English](README.md)**

---

以 [MongoDB Go Driver](https://www.mongodb.com/docs/drivers/go/current/) 為基礎的 OpenTelemetry 包裝。寫入時將 **W3C Trace Context** 注入文件的 **`_oteltrace`** 欄位，讀取時還原，使同一條 trace 可跨服務延續。依 [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation) 規範：套件僅透過 option 接受 **TracerProvider** 與 **Propagators**，不提供 InitTracer；由應用程式在啟動時設定 global provider 與 propagator（見 **examples/**）。

支援兩種 driver 版本（Go 慣例：v2 使用 import path `.../v2`）：
- **v2**：`import "github.com/Marz32onE/instrumentation-go/otel-mongo/v2"`（MongoDB driver v2，建議）
- **v1**：`import "github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo"`（MongoDB driver v1）

---

## 目錄結構

```
otel-mongo/
├── otelmongo/                  # MongoDB driver v1 wrapper (root module)
│   ├── client.go database.go collection.go cursor.go results.go     # facade
│   ├── tracing.go env_flags.go version.go doc.go                    # facade 輔助
│   └── internal/
│       ├── flags/              # 共享 gate helper（四模組 byte-identical）
│       ├── shared/             # impls.go（CursorImpl / SingleResultImpl / ChangeStreamImpl interfaces）、
│       │                       # bulkwrite.go semconv.go tracing.go — direct/traced 共用 helper
│       ├── direct/             # disabled-mode 實作 — ZERO otel/sdk 與 otel/exporters import
│       └── traced/             # enabled-mode 實作 — 完整 instrumentation + ClientState / DatabaseState
├── v2/                         # MongoDB driver v2 wrapper（獨立 module，import .../v2）
│   └── （internal/{flags,shared,direct,traced}/ 結構與 v1 相同；見 otel-mongo/v2/README.md）
├── examples/                   # TracerProvider + global + otelmongo（使用 v2）
├── tests/integration/          # testcontainers-based；standalone MongoDB（無需 replica-set）
└── README.md
```

Client + Database 採 **nullable traced-pointer** 變體（`facade.Client{*mongo.Client; traced *traced.ClientState}` — `nil` ⇔ disabled）。Collection / Cursor / SingleResult / ChangeStream 採 **full strategy-split** 變體（facade 持有 `impl <X>Impl` interface）。`cursor.go`、`results.go`、`collection.go` 內的編譯期斷言（`var _ shared.CursorImpl = (*direct.Cursor)(nil)` 等）確保新方法在兩種 impl 都實作。

- **Trace 儲存：** 寫入/更新的文件會有保留欄位 **`_oteltrace`**。對 raw BSON（例如 change stream）可使用 **ContextFromDocument(ctx, raw)** 還原 context。
- **兩層：** (1) **Driver** 使用 contrib otelmongo Monitor 產生連線/指令 span。(2) **Document** 層在 CRUD 寫入時注入 `_oteltrace`，讀取時支援 span link 與傳播。

---

## 使用方式

### Tracing 功能旗標

`otel-mongo`（v1 + v2）使用一個全域開關加兩個模組開關：

- `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`（全域總開關）
- `OTEL_MONGO_TRACING_ENABLED`（控制本套件的 wrapper **CLIENT** span、deliver-span 路徑、與 noop vs 實際 tracer — driver/contrib command span 不受此影響）
- `OTEL_MONGO_PROPAGATION_ENABLED`（控制 wrapped Collection/Cursor/ChangeStream 的 `_oteltrace` 注入/還原，以及 **ContextFromDocument** / **ContextFromRawDocument**）

預設值：未設定即停用。值為 `false/0/no/off` 視為停用。

優先順序：
1. 若**全域**停用，所有模組旗標與 **`WithTracePropagationEnabled(true)`** 皆強制停用 — 不會產生 wrapper span，也不會做 `_oteltrace` 注入/還原。
2. 若全域啟用但 **`OTEL_MONGO_TRACING_ENABLED`** 停用，本套件視 Mongo tracing 為關閉：wrapper CLIENT span 改用 noop tracer，**同時** `_oteltrace` 注入/還原也一併停用。`WithTracePropagationEnabled(true)` 無法跨越此 gate — propagation 與 tracing 共用同一個開關。
3. 只有當全域與 `OTEL_MONGO_TRACING_ENABLED` **皆啟用**時，`OTEL_MONGO_PROPAGATION_ENABLED` 才會作為 `_oteltrace` 的預設值；在兩個 tracing gate 都開啟期間，`ConnectWithOptions` 內的 **`WithTracePropagationEnabled`** 可覆寫該預設。

設計理由：關閉 Mongo tracing 時連同 Mongo trace propagation 也一併關閉，呼叫端只需一個 kill switch — 不會出現 wrapper span 為 noop 但文件仍被寫入 `_oteltrace` 的情境。

當 tracing 旗標未設定或停用時，本套件的 wrapper 不會送出 Mongo CLIENT span 到你配置的 TracerProvider（noop），**且**寫入的文件不會帶有 `_oteltrace`。Deliver span 另外還需 `OTEL_EXPORTER_OTLP_ENDPOINT`，見下文。

### 1. 初始化 Provider 與 Propagator（應用程式負責）

見 **examples/main.go**：建立 TracerProvider（如 OTLP）、設定 `otel.SetTracerProvider(tp)` 與 `otel.SetTextMapPropagator(prop)`、defer shutdown。

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

需與寫入相同的 propagation 環境變數：**`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`、`OTEL_MONGO_TRACING_ENABLED`、`OTEL_MONGO_PROPAGATION_ENABLED` 三者都要啟用**，或兩個 tracing gate 啟用搭配 `ConnectWithOptions` 的 `WithTracePropagationEnabled(true)`。當任一 gate 關閉時，`ContextFromDocument` / `ContextFromRawDocument` 會回傳零值或不變的 ctx — 忽略 `ok` 回傳值的舊呼叫端會靜默變成 no-op。

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
