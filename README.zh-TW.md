# instrumentation-go

本倉庫提供 **NATS**（核心與 JetStream）、**MongoDB**（Go driver v1 與 v2）以及 **gorilla/websocket** 的 OpenTelemetry 封裝，設計上對齊 [OTel Go Contrib 儀表化指引](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation)。

共有 **四個獨立的 instrumentation 模組**（各目錄自有 `go.mod`），**版本與 Git tag 分開管理**；另有 **兩個支援模組**——`otel-sampler`（對外發布的一致機率取樣器，由應用程式引入）與 `otel-testkit`（不打 tag、僅供測試的 E2E harness）。模組使用 **Go 1.25**。CI 會對每個模組執行 `go build`、`go test -race`、**golangci-lint**，通過後再跑需 Docker 的 **整合測試** 與 **一致取樣 E2E**（testcontainers）— 見 [.github/workflows/ci.yml](.github/workflows/ci.yml)。

封裝**不會**自行建立全域 `TracerProvider`，預設使用 `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()`；需要時可透過 `WithTracerProvider`、`WithPropagators` 覆寫。**應用程式**須在啟動時安裝 TracerProvider 與 W3C 傳播器（各模組的 **examples/** 有完整範例）。

**English:** [README.md](README.md)

## 套件一覽

| 套件 | Import 路徑 | 原始碼版本 | 說明 |
|------|-------------|------------|------|
| **otel-mongo** (v1) | `github.com/akira-core/instrumentation-go/otel-mongo/otelmongo` | 0.7.0 | MongoDB driver v1 封裝；寫入時注入 `_oteltrace`；`ContextFromDocument` 與解碼輔助。 |
| **otel-mongo/v2** | `github.com/akira-core/instrumentation-go/otel-mongo/v2` | 0.7.0 | MongoDB driver v2 封裝；與 v1 行為對齊。 |
| **otel-nats** | `github.com/akira-core/instrumentation-go/otel-nats/otelnats` | 0.7.0 | 核心 NATS；W3C 脈絡在訊息標頭。 |
| **otel-nats** | `github.com/akira-core/instrumentation-go/otel-nats/oteljetstream` | 0.7.0 | JetStream 發布／消費／fetch。 |
| **otel-gorilla-ws** | `github.com/akira-core/instrumentation-go/otel-gorilla-ws` | 0.7.0 | 在 JSON 訊息本文內傳遞 trace context（信封格式）；`NewConn` / `Dial`。 |

### 支援模組

| 套件 | Import 路徑 | 原始碼版本 | 說明 |
|------|-------------|------------|------|
| **otel-sampler** | `github.com/akira-core/instrumentation-go/otel-sampler/otelsampler` | 0.1.1 | 一致機率取樣器（`ot=th:`／`ot=rv:`）與 `WithSingleLinkSeed`，讓 span-link 消費者的取樣決策與 parent-child 一致。本身不產生 span。 |
| **otel-testkit** | `github.com/akira-core/instrumentation-go/otel-testkit/harness` | 未打 tag | 黑箱 E2E harness（行程內 OTLP sink + collector + 斷言），供本倉庫取樣測試使用。僅供測試，不保證 API 穩定。 |

`otel-sampler` 已發布的 `v0.1.0` tag 指向 rebase 前的 commit，已作廢——請從 `0.1.1` 起用。詳見 [VERSIONING.md](VERSIONING.md)。

各模組詳細文件：[otel-mongo/README.md](otel-mongo/README.md)、[otel-nats/README.md](otel-nats/README.md)、[otel-gorilla-ws/README.md](otel-gorilla-ws/README.md)；三個模組皆另有繁中版：[otel-mongo/README.zh-TW.md](otel-mongo/README.zh-TW.md)、[otel-nats/README.zh-TW.md](otel-nats/README.zh-TW.md)、[otel-gorilla-ws/README.zh-TW.md](otel-gorilla-ws/README.zh-TW.md)。

## 安裝

依模組路徑搭配對應 **Git tag**（前綴與模組一致，例如 `otel-mongo/v0.6.0`）：

```bash
go get github.com/akira-core/instrumentation-go/otel-mongo@otel-mongo/v0.6.0
go get github.com/akira-core/instrumentation-go/otel-mongo/v2@otel-mongo/v2/v0.6.0
go get github.com/akira-core/instrumentation-go/otel-nats@otel-nats/v0.6.0
go get github.com/akira-core/instrumentation-go/otel-gorilla-ws@otel-gorilla-ws/v0.6.0
```

程式中再 import 子套件（`.../otelmongo`、`.../otelnats`、`.../oteljetstream`；WebSocket 為根套件）。

## 追蹤功能開關

開關透過 [OpenFeature](https://openfeature.dev) 於**執行期**解析,operator 可經由 GO Feature Flag relay proxy **不重啟應用程式**即開關追蹤。未安裝 OpenFeature provider 時,每個開關回退到對應環境變數,行為與導入動態旗標之前完全相同。

環境變數為**未設定視為關閉**。設成 `0`、`false`、`no`、`off`(不分大小寫)亦為關閉;其餘非空字串視為**開啟**。

| OpenFeature flag key | 對應環境變數 | 作用範圍 | 說明 |
|---|---|---|---|
| *(無 —— 僅環境變數)* | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | 全部模組 | **總斷路器**。關閉時完全不進行 OpenFeature 求值,relay 上任何值都無法開啟。 |
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo` + `otel-mongo/v2` | CLIENT span。 |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo` + `otel-mongo/v2` | `_oteltrace` 寫入／讀取抽取;仍受有效 tracing 約束。 |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otelnats` + `oteljetstream` | NATS／JetStream 封裝追蹤。 |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` | WebSocket span 產生。 |

總開關刻意**在 relay 上沒有對應 flag**:它是 relay 無法連線或設定寫錯時仍然有效的 out-of-band 煞車。

### 接上 provider

instrumentation 模組**永不**安裝 OpenFeature provider —— 這與「模組永不初始化 `TracerProvider`」是同一條規則。由應用程式在啟動時接上,位置就在既有的 OTel 初始化旁邊:

```go
import (
    "github.com/open-feature/go-sdk/openfeature"
    gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
)

provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
    Endpoint: "http://relay-proxy:1031",
})
if err != nil {
    return err
}
if err := openfeature.SetProviderAndWait(provider); err != nil {
    return err
}
// 選用:讓 relay 依行程層級屬性做分流
openfeature.SetEvaluationContext(openfeature.NewTargetlessEvaluationContext(map[string]any{
    "service.name": "checkout-api",
}))
```

GO Feature Flag provider 是**應用程式端**依賴;instrumentation 模組只依賴 `github.com/open-feature/go-sdk`。

解析結果以模組為單位快取 **1 秒**,因此 hot path 永遠不會進入 OpenFeature 的求值管線。也因為該快取是行程層級的,分流可依行程屬性(service、environment、host)進行,但**無法**依單一請求的屬性分流。

### 優先順序

| 優先 | 來源 | 說明 |
|---|---|---|
| 1 | `WithTracingEnabled(v)` | 該連線／client 成為完全**靜態** —— 不進行任何 OpenFeature 求值,relay 的變更也影響不到它。 |
| 2 | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | 斷路器。關閉即關閉,無條件。 |
| 3 | relay flag | relay 有值時由它決定。 |
| 4 | 模組環境變數 | 作為 OpenFeature 求值的 default value。 |

各模組建構時皆可傳 `WithTracingEnabled(v bool)`:

| 斷路器 | relay／模組 flag | `WithTracingEnabled` | 有效 tracing |
|---|---|---|---|
| 關 | 任意 | *(未傳)* | **關** —— 不進行求值 |
| 關 | 任意 | `true` | **開** |
| 關 | 任意 | `false` | **關** |
| 開 | 開 | *(未傳)* | **開** |
| 開 | 關 | *(未傳)* | **關** |
| 開 | 任意 | `false` | **關** |
| 開 | 任意 | `true` | **開** |

Mongo 專屬:`WithTracePropagationEnabled` 僅在有效 tracing 為**開**時控制該 client 的 `_oteltrace`;有效 tracing 為關時無法用它開啟傳播。package-level 的 `ContextFromDocument` / `ContextFromRawDocument` 跟隨 relay(它們沒有 client 可查,因此忽略 per-client option)。傳播子表見 [otel-mongo/README.md](otel-mongo/README.md)。

WebSocket 專屬:`otel-ws` subprotocol 協商**僅**由斷路器閘控,不看 relay flag。handshake 無法事後重來,若以 relay 值閘控,則 flag 關閉期間建立的連線在 flag 打開後將永遠無法傳遞 trace context。代價是:兩端都使用本 library 且斷路器為開時,即使 tracing 關閉,訊息仍帶 JSON envelope。

## 目錄結構

```
instrumentation-go/
├── otel-mongo/
│   ├── otelmongo/           # v1 封裝（模組根）
│   ├── v2/                  # v2 封裝（獨立 go.mod，另有自己的 tests/integration/）
│   │   └── tests/integration/
│   ├── examples/
│   ├── tests/integration/   # Docker：testcontainers（v1）
│   └── README.md
├── otel-nats/
│   ├── otelnats/
│   ├── oteljetstream/
│   ├── examples/
│   ├── tests/integration/
│   ├── go.mod
│   └── README.md
├── otel-gorilla-ws/
│   ├── examples/
│   ├── tests/integration/
│   ├── go.mod
│   └── README.md
├── otel-ws.md               # 子協定／傳播設計筆記（跨語言）
├── CLAUDE.md                # 貢獻者／代理用說明
└── README.md
```

## 使用方式

1. **應用程式**建立 `TracerProvider`（例如 OTLP），呼叫 `otel.SetTracerProvider(tp)` 與 `otel.SetTextMapPropagator(...)`，並在結束時 shutdown。
2. **應用程式**以封裝建立連線：`otelnats.Connect(url, nil)`、`otelmongo.Connect(ctx, opts...)`、`otelgorillaws.NewConn(raw, opts...)` 等。

可執行範例：**otel-nats/examples**、**otel-mongo/examples**、**otel-gorilla-ws/examples**。

## 診斷日誌

各套件使用 [`log/slog`](https://pkg.go.dev/log/slog)；預設 handler 下通常**不會輸出**，除非提高層級。

| 套件 | 層級 | 內容 |
|------|------|------|
| `otel-nats` | `DEBUG` | 伺服器位址解析失敗 |
| `otel-nats` | `DEBUG`／`WARN` | trace event 解析失敗（使用 `WithTraceDestination` 時） |

範例：

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

日誌帶前綴 `otelnats:` 與結構化欄位（`reason`、`error`、`addr`）。
