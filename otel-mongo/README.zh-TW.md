# otel-mongo（otelmongo）

**[English](README.md)**

---

以 [MongoDB Go Driver](https://www.mongodb.com/docs/drivers/go/current/) 為基礎的 OpenTelemetry 包裝。寫入時將 **W3C Trace Context** 注入文件的 **`_oteltrace`** 欄位，讀取時還原，使同一條 trace 可跨服務延續。依 [OTel Go Contrib](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation) 規範：套件僅透過 option 接受 **TracerProvider** 與 **Propagators**，不提供 InitTracer；由應用程式在啟動時設定 global provider 與 propagator（見 **examples/**）。

支援兩種 driver 版本（Go 慣例：v2 使用 import path `.../v2`）：
- **v2**：`import "github.com/akira-core/instrumentation-go/otel-mongo/v2"`（MongoDB driver v2，建議）
- **v1**：`import "github.com/akira-core/instrumentation-go/otel-mongo/otelmongo"`（MongoDB driver v1）

兩個套件提供相同的 API 介面（Client、Collection、Cursor、ContextFromDocument 等）與相同的 `_oteltrace` 文件層級傳播機制。

---

## 目錄結構

```
otel-mongo/
├── otelmongo/           # MongoDB driver v1 包裝（root module）
│   ├── version.go, client.go, database.go, collection.go, cursor.go
│   ├── tracing.go, results.go, env_flags.go
│   └── internal/
│       ├── shared/     # semconv.go, bulkwrite.go, tracing.go, impls.go — direct 與 traced 共用
│       ├── direct/     # passthrough 實作（不 import otel/sdk）— tracing 停用時使用
│       └── traced/     # 完整 instrumented 實作
├── v2/                  # MongoDB driver v2 包裝（獨立 module，import .../v2）
│   ├── go.mod           # module .../otel-mongo/v2，需要 go.mongodb.org/mongo-driver/v2
│   ├── version.go, client.go, database.go, collection.go, cursor.go
│   ├── tracing.go, results.go, env_flags.go
│   └── internal/        # shared/, direct/, traced/ — 與上方 otelmongo/internal/ 對應
├── examples/             # 使用 v2 的範例
└── README.md
```

- **Trace 儲存：** 寫入/更新的文件會有保留欄位 **`_oteltrace`**（W3C `traceparent` 及選填 `tracestate`）。對 raw BSON（例如 change stream）可使用 **ContextFromDocument(ctx, raw)** 還原 context。
- **兩層：** (1) **Client span：** 每個 Collection 方法（insert/find/update/delete/aggregate/distinct/bulkWrite 等）都在 `internal/traced/collection.go` 直接產生自己的 span，並無獨立的 driver 層 command monitor。(2) **Document** 層在 CRUD 寫入時注入 `_oteltrace`，讀取時支援 span link 與傳播。

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

**MongoDB driver v2**（建議；import path 符合 Go 慣例）：

```go
import (
    "github.com/akira-core/instrumentation-go/otel-mongo/v2"
    "go.mongodb.org/mongo-driver/v2/mongo/options"
)

client, err := otelmongo.Connect(options.Client().ApplyURI(uri))
if err != nil { log.Fatal(err) }
defer client.Disconnect(ctx)

db := client.Database("mydb")
coll := db.Collection("mycoll")
// InsertOne、Find、UpdateOne 等會自動處理 _oteltrace
```

**MongoDB driver v1**（相同 API，不同 import 與 Connect 簽名）：

```go
import (
    "context"
    "github.com/akira-core/instrumentation-go/otel-mongo/otelmongo"
    "go.mongodb.org/mongo-driver/mongo/options"
)

client, err := otelmongo.Connect(ctx, options.Client().ApplyURI(uri))
if err != nil { log.Fatal(err) }
defer client.Disconnect(ctx)

db := client.Database("mydb")
coll := db.Collection("mycoll")
// CRUD 與 _oteltrace 行為與 v2 包裝相同
```

可選：**ConnectWithOptions(ctx, traceOpts, mongoOpts)**（v1）或 **ConnectWithOptions(traceOpts, mongoOpts)**（v2），搭配 **WithTracerProvider(tp)** 或 **WithPropagators(p)**。

### 3. 從文件還原 trace（例如 change stream）

需與寫入相同的 propagation 環境變數：**`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`、`OTEL_MONGO_TRACING_ENABLED`、`OTEL_MONGO_PROPAGATION_ENABLED` 三者都要啟用**，或兩個 tracing gate 啟用搭配 `ConnectWithOptions` 的 `WithTracePropagationEnabled(true)`。當任一 gate 關閉時，`ContextFromDocument` / `ContextFromRawDocument` 會回傳零值或不變的 ctx — 忽略 `ok` 回傳值的舊呼叫端會靜默變成 no-op。

```go
fullDoc := changeStreamEvent.FullDocument
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

---

## API 摘要

| 項目 | 說明 |
|------|------|
| **Connect / ConnectWithOptions** | 未傳入 option 時使用 `otel.GetTracerProvider()`。 |
| **NewClient** | 可選 **WithTracerProvider**、**WithPropagators**。 |
| **ContextFromDocument** | 從文件的 `_oteltrace` 還原 trace context。 |
| **ScopeName / Version()** | 建立 Tracer 時使用（OTel contrib 規範）。 |

---

## Deliver Spans（服務關係圖）

當 `OTEL_EXPORTER_OTLP_ENDPOINT` 已設定時，otelmongo 會建立合成的「deliver」span，將 MongoDB 表示為 Grafana service graph 中的 broker 節點。`Connect` 與 `NewClient` 皆支援此功能 — server address 會從 `options.Client().ApplyURI(uri)` 提供的 URI 解析。

Endpoint 對 HTTP 必須是**完整 URL**（例如 `http://otel-collector:4318`），對 gRPC 則是 **host:port**（例如 `otel-collector:4317`）。不支援沒有 scheme 或 port 的裸主機名稱。

所有 CRUD 操作（insert、find、update、delete、replace、aggregate、bulk write、distinct、count 等）以及 cursor decode 與 change stream 路徑都會產生 deliver span。

### Write path

```
InsertOne (CLIENT, api)
  └── db.coll deliver (CONSUMER, mongodb)  ← 注入至 _oteltrace
```

### Read / delete path

```
FindOne / DeleteOne / ... (CLIENT, api)
  └── db.coll deliver (CONSUMER, mongodb)
```

### Change stream path

```
db.coll deliver (PRODUCER, mongodb)  ← 連結至 producer deliver
  └── watch coll (CONSUMER, dbwatcher) ← deliver 的子 span
```

### 產生的服務關係圖

```
api ──► mongodb ──► dbwatcher
```

---

## v1 vs v2 API 差異

| 差異 | v1（`otelmongo`） | v2（`.../v2`） |
|------|------------------|---------------|
| `Connect` 簽名 | `Connect(ctx, opts...)` | `Connect(opts...)` |
| `NewClient` 簽名 | `NewClient(ctx, uri, traceOpts...)` | `NewClient(uri, traceOpts...)` |
| `Distinct` 回傳值 | `([]interface{}, error)` | `*mongo.DistinctResult` |
| `StartSession` 回傳值 | `mongo.Session, error` | `*mongo.Session, error` |
| `Cursor.DecodeWithContext` | 兩者行為一致：一律在新（detached）trace 上發出 `mongo.cursor.decode` INTERNAL span，若文件的 `_oteltrace` metadata 存在且 propagation 已啟用，則附上指向來源 span 的 link。 | （同左） |

---

## 重要注意事項

### 文件中的 `_oteltrace` 欄位

每次 `InsertOne`、`InsertMany`、`ReplaceOne`、`UpdateOne`/`UpdateMany`/`UpdateByID` 呼叫時，只要 context 中有 active OTel span，就會在文件中（或 operator update 的 `$set` 中）注入保留欄位 **`_oteltrace`**。此欄位是一個子文件：

```bson
{ "traceparent": "00-<traceId>-<spanId>-01", "tracestate": "" }
```

**對 schema 的影響：** 使用嚴格 schema 驗證或指定欄位 projection 的應用程式/查詢會看到這個額外欄位。如有需要，請將 `_oteltrace` 加入允許清單或 projection。

**對文件大小的影響：** 每份文件約增加 100–120 bytes。當沒有 active span 時（例如測試中未設定 TracerProvider），不會注入 `_oteltrace` 欄位。

### Global OTel 狀態

傳入 `ConnectWithOptions` 的 `WithTracerProvider` 與 `WithPropagators` 只會儲存在 `Client` 上，**不會**呼叫 `otel.SetTracerProvider` / `otel.SetTextMapPropagator`。若省略這些選項，client 會在連線時使用 `otel.GetTracerProvider()` 與 `otel.GetTextMapPropagator()`。多數應用程式應在啟動時設定一次 global，之後呼叫 `Connect` / `NewClient` 時不帶 trace option。

### `NewCollection` 與 `Connect`

`NewCollection` 會依照與 `Connect` 相同的環境變數 gate（全域 + `OTEL_MONGO_TRACING_ENABLED` + `OTEL_MONGO_PROPAGATION_ENABLED`）設定**文件層級**的 `_oteltrace` 行為。當任一 tracing gate 關閉時，該 collection 會以停用 propagation 的狀態建立。並沒有針對單一 collection 的 propagation functional option；若需覆寫某個 client 的環境預設值，請使用 **`ConnectWithOptions`** 搭配 **`WithTracePropagationEnabled`**（注意：仍無法跨越已停用的 tracing gate）。

### Cursor 上的 DecodeWithContext 與 Decode

`Cursor.DecodeWithContext` 會從 `_oteltrace` 擷取來源的 trace context 並回傳強化過的 context — 當你需要將後續工作連結回文件的來源 trace 時使用。單純的 `Cursor.Decode` 行為與底層 driver 的 `Decode` 完全相同，會忽略 `_oteltrace`。

### FindOne 上的 span link

`SingleResult.Decode` 會對已取得文件中儲存的 `_oteltrace` 加上一個 **span link**（而非 parent-child 關係）。FindOne 的 span 會在第一次呼叫 `Decode`、`Raw` 或 `TraceContext` 時結束。每個 `SingleResult` 只能呼叫其中一種方法一次。

### `server.address` / `server.port` 的來源

啟用 tracing 時，Collection 的 CRUD CLIENT span（`InsertOne`、`Find`、`UpdateOne`、`Aggregate`、`Watch` 等）上的 `server.address`/`server.port`，來自實際處理該次指令的 MongoDB 連線 —— 透過在底層 driver client 上註冊 `event.CommandMonitor`擷取，而非僅在 `Connect` 時對連線字串做一次性解析。這讓多主機的 replica-set URI、`mongodb+srv://` 連線字串，以及 primary failover 後的情境，都能得到正確的 attribute（URI 中列出的第一台主機不一定是實際處理該次指令的主機）。

若某次呼叫沒有觀察到任何 command 事件（例如防禦性/邊界情況的程式路徑），span 會回退使用連線字串靜態解析出的位址 —— 與 0.6.1 之前的行為一致。

**呼叫端自行提供的 `SetMonitor` 會被串接，而非取代。** 若你在傳入 `Connect`/`ConnectWithOptions` 的 `*options.ClientOptions` 上呼叫了自己的 `SetMonitor(...)`，otelmongo 的位址擷取回呼會先執行，接著原封不動地呼叫你的 `Started`/`Succeeded`/`Failed` 回呼 —— 不會被靜默忽略。

此擷取機制僅在啟用 tracing 的路徑上執行；停用 tracing 時不會註冊任何 `CommandMonitor`，你所提供的 monitor 會完全原樣通過。

---

## 診斷紀錄

使用 [`log/slog`](https://pkg.go.dev/log/slog) — 預設無輸出。

| 等級 | 事件 |
|-------|------|
| `DEBUG` | Deliver tracer 初始化成功（記錄 `service` 與 `endpoint`） |
| `WARN` | OTLP exporter 建立失敗；resource 建立失敗 |

啟動時啟用 debug 等級的 slog handler：

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

Log 項目使用 `otelmongo:` 前綴，並附帶 `error`、`reason`、`service`、`endpoint` 等 key-value 欄位。

---

## Dependencies

- **v2**（`.../otel-mongo/v2`）：`go.mongodb.org/mongo-driver/v2`、`go.opentelemetry.io/otel` 及其 SDK。詳見 `v2/go.mod`。
- **otelmongo**（v1，root）：`go.mongodb.org/mongo-driver` v1、`go.opentelemetry.io/otel` 及其 SDK。詳見 root `go.mod`。
- Go 1.24+
