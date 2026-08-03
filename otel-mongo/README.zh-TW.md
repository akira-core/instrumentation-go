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
- **兩層：** (1) **Client span：** 每個 Collection 方法（insert/find/update/delete/aggregate/distinct/bulkWrite 等）都在 `internal/traced/collection.go` 直接產生自己的 span；另有一個**串接式** driver `CommandMonitor`（僅在啟用 tracing 時註冊，且串接在你自行設定的 monitor 之後）負責擷取每個指令實際命中的伺服器位址，寫入 span 的 `server.*` 屬性。(2) **Document** 層在 CRUD 寫入時注入 `_oteltrace`，讀取時支援 span link 與傳播。

---

## 使用方式

### Tracing 功能旗標

```
tracing     = gate1 && OTEL_MONGO_TRACING_ENABLED && relay otel-mongo-tracing
propagation = tracing && OTEL_MONGO_PROPAGATION_ENABLED && relay otel-mongo-propagation
```

`gate1` 是 `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` **或** `WithTracingEnabled(v)` —— 同一個開關的兩種
拼法,**兩個都給是設定錯誤**(`ErrTracingConfigConflict`),即使兩者一致也一樣。
`OTEL_MONGO_PROPAGATION_ENABLED` 與 `WithTracePropagationEnabled` 適用同一條規則,對應
`ErrTracePropagationConfigConflict`。

環境層在**建構時讀一次**,決定 instrumented 實作要不要被建立。relay verdict 在**每次操作**解析,而且
**只能撤銷** —— relay 上沒有任何東西能打開部署沒打開的東西。開關只有設成 `1`、`true`、`yes`、`on` 才算開。

`WithTracingEnabled` **不會**把 client 釘死:它只供給 `gate1`,帶著它的 client 在 relay 撤銷時仍然會停。

兩個模組專屬要點:

- **`_oteltrace` 會改變持久化內容。** 每份文件約 90 bytes,由 `InsertOne`、`InsertMany`、`UpdateOne`、
  `UpdateMany`、`UpdateByID`、`ReplaceOne`、`BulkWrite` 寫入。**沒有任何東西會移除它** —— 關掉傳播只會停止
  新的寫入,不會還原舊的;清理要靠 `$unset` migration。使用 `$jsonSchema` + `additionalProperties: false`
  的 collection 在它開著時會拒絕每一次寫入。對已經有這個欄位的文件重新注入會**取代**而不是重複。
- **`ContextFromDocument` / `ContextFromRawDocument` 完全沒有閘門。** 它們不開 span、不寫入任何東西、不初始化
  OTel SDK 的任何部分 —— 只從你手上已有的值讀一個欄位。**撤銷不會停掉它們**,這使它們成為「library 被靜音後
  仍要維持 trace 連結」的受支援做法。`Cursor.DecodeAndTrace` / `ChangeStream.DecodeAndTrace` **有**閘門,
  因為它們會送出 span。

> 完整參考 —— 全部解析表格、零程式碼連上 relay、撤銷延遲、針對單一服務的 targeting、維運速查:
> **[feature-flags.zh-TW.md](../feature-flags.zh-TW.md)** · English:**[feature-flags.md](../feature-flags.md)**

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

可選：**ConnectWithOptions(ctx, traceOpts, mongoOpts)**（v1）或 **ConnectWithOptions(traceOpts, mongoOpts)**（v2），搭配 **WithTracerProvider(tp)**、**WithPropagators(p)** 或 **WithTracingEnabled(v bool)**。

### 3. 從文件還原 trace（例如 change stream）

`ContextFromDocument` / `ContextFromRawDocument` **沒有任何 feature-flag 閘門**。它們不開 span、不寫入任何東西,所以沒有什麼需要 kill switch 保護 —— relay 撤銷也不會停掉它們。只有在文件沒有 `_oteltrace`、或 `traceparent` 缺漏/無效時才回傳零值 / `ok == false`。

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
| **NewClient** | 可選 **WithTracerProvider**、**WithPropagators**、**WithTracingEnabled**。 |
| **ContextFromDocument** | 從文件的 `_oteltrace` 還原 trace context。 |
| **ScopeName / Version()** | 建立 Tracer 時使用（OTel contrib 規範）。 |

---

## Span kind

MongoDB 是資料庫，不是訊息系統：每個操作 span 一律使用 `CLIENT`（`Watch` 的 change-stream 讀取 span 也不例外 — 它是同步的 `getMore` 呼叫，不是非同步遞送）。純本地端工作（`Cursor.DecodeAndTrace` 未觸發往返時）使用 `INTERNAL`。

```
InsertOne / FindOne / UpdateOne / ... (CLIENT)
Watch → change-stream 讀取 (CLIENT)
Cursor.DecodeAndTrace (INTERNAL；文件帶 `_oteltrace` 時連結回原始 span)
```

---

## v1 vs v2 API 差異

| 差異 | v1（`otelmongo`） | v2（`.../v2`） |
|------|------------------|---------------|
| `Connect` 簽名 | `Connect(ctx, opts...)` | `Connect(opts...)` |
| `NewClient` 簽名 | `NewClient(ctx, uri, traceOpts...)` | `NewClient(uri, traceOpts...)` |
| `Distinct` 回傳值 | `([]interface{}, error)` | `*mongo.DistinctResult` |
| `StartSession` 回傳值 | `mongo.Session, error` | `*mongo.Session, error` |
| `Cursor.DecodeAndTrace` | 兩者行為一致：一律在新（detached）trace 上發出 `mongo.cursor.decode` INTERNAL span，若文件的 `_oteltrace` metadata 存在且 propagation 已啟用，則附上指向來源 span 的 link。 | （同左） |

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

`NewCollection` 不接受任何 option,所以它自己解析環境層:`gate1` 與 `OTEL_MONGO_TRACING_ENABLED` 決定 instrumented 實作要不要被建立,`OTEL_MONGO_PROPAGATION_ENABLED` 在那之下管 `_oteltrace`。relay verdict 仍然每次操作生效。並沒有針對單一 collection 的 propagation functional option;若要用程式碼而非環境變數供給那一層,請使用 **`ConnectWithOptions`** 搭配 **`WithTracePropagationEnabled`**(注意:它無法跨越已停用的 tracing 層,也無法跨越 relay 撤銷)。

### Cursor 上的 DecodeAndTrace 與 Decode

`Cursor.DecodeAndTrace` 會從 `_oteltrace` 擷取來源的 trace context 並回傳強化過的 context — 當你需要將後續工作連結回文件的來源 trace 時使用。單純的 `Cursor.Decode` 行為與底層 driver 的 `Decode` 完全相同，會忽略 `_oteltrace`。

### FindOne 上的 span link

`SingleResult.Decode` 會對已取得文件中儲存的 `_oteltrace` 加上一個 **span link**（而非 parent-child 關係）。FindOne 的 span 會在第一次呼叫 `Decode`、`Raw` 或 `TraceContext` 時結束。每個 `SingleResult` 只能呼叫其中一種方法一次。

### `server.address` / `server.port` 的來源

啟用 tracing 時，Collection 的 CRUD CLIENT span（`InsertOne`、`Find`、`UpdateOne`、`Aggregate`、`Watch` 等）上的 `server.address`/`server.port`，來自實際處理該次指令的 MongoDB 連線 —— 透過在底層 driver client 上註冊 `event.CommandMonitor`擷取，而非僅在 `Connect` 時對連線字串做一次性解析。這讓多主機的 replica-set URI、`mongodb+srv://` 連線字串，以及 primary failover 後的情境，都能得到正確的 attribute（URI 中列出的第一台主機不一定是實際處理該次指令的主機）。

若某次呼叫沒有觀察到任何 command 事件（例如防禦性/邊界情況的程式路徑），span 會回退使用連線字串靜態解析出的位址 —— 與 0.6.1 之前的行為一致。

**呼叫端自行提供的 `SetMonitor` 會被串接，而非取代。** 若你在傳入 `Connect`/`ConnectWithOptions` 的 `*options.ClientOptions` 上呼叫了自己的 `SetMonitor(...)`，otelmongo 的位址擷取回呼會先執行，接著原封不動地呼叫你的 `Started`/`Succeeded`/`Failed` 回呼 —— 不會被靜默忽略。

此擷取機制僅在啟用 tracing 的路徑上執行；停用 tracing 時不會註冊任何 `CommandMonitor`，你所提供的 monitor 會完全原樣通過。

---

## Dependencies

- **v2**（`.../otel-mongo/v2`）：`go.mongodb.org/mongo-driver/v2`、`go.opentelemetry.io/otel` 及其 SDK。詳見 `v2/go.mod`。
- **otelmongo**（v1，root）：`go.mongodb.org/mongo-driver` v1、`go.opentelemetry.io/otel` 及其 SDK。詳見 root `go.mod`。
- Go 1.24+
