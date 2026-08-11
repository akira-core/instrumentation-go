# instrumentation-go

本倉庫提供 **NATS**（核心與 JetStream）、**MongoDB**（Go driver v1 與 v2）以及 **gorilla/websocket** 的 OpenTelemetry 封裝，設計上對齊 [OTel Go Contrib 儀表化指引](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation)。

共有 **四個獨立的 instrumentation 模組**（各目錄自有 `go.mod`），**版本與 Git tag 分開管理**；另有 **三個支援模組**——`otel-flags`（對外發布的共享功能開關層，四個 wrapper 都 require 它）、`otel-sampler`（對外發布的一致機率取樣器，由應用程式引入）與 `otel-testkit`（不打 tag、僅供測試的 E2E harness）。模組使用 **Go 1.25**。CI 會對每個模組執行 `go build`、`go test -race`、**golangci-lint**，通過後再跑需 Docker 的 **整合測試** 與 **一致取樣 E2E**（testcontainers）— 見 [.github/workflows/ci.yml](.github/workflows/ci.yml)。

封裝**不會**自行建立全域 `TracerProvider`，預設使用 `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()`；需要時可透過 `WithTracerProvider`、`WithPropagators` 覆寫。**應用程式**須在啟動時安裝 TracerProvider 與 W3C 傳播器（各模組的 **examples/** 有完整範例）。

**English:** [README.md](README.md)

## 套件一覽

| 套件 | Import 路徑 | 原始碼版本 | 說明 |
|------|-------------|------------|------|
| **otel-mongo** (v1) | `github.com/akira-core/instrumentation-go/otel-mongo/otelmongo` | 0.8.0 | MongoDB driver v1 封裝；寫入時注入 `_oteltrace`；`ContextFromDocument` 與解碼輔助。 |
| **otel-mongo/v2** | `github.com/akira-core/instrumentation-go/otel-mongo/v2` | 2.8.0 | MongoDB driver v2 封裝；與 v1 行為對齊。走固定主版號的 `2.x` 線——詳見 [VERSIONING.md](VERSIONING.md)。 |
| **otel-nats** | `github.com/akira-core/instrumentation-go/otel-nats/otelnats` | 0.9.1 | 核心 NATS；W3C 脈絡在訊息標頭。 |
| **otel-nats** | `github.com/akira-core/instrumentation-go/otel-nats/oteljetstream` | 0.9.1 | JetStream 發布／消費／fetch。 |
| **otel-gorilla-ws** | `github.com/akira-core/instrumentation-go/otel-gorilla-ws` | 0.8.0 | 在 JSON 訊息本文內傳遞 trace context（信封格式）；`NewConn` / `Dial`。 |

### 支援模組

| 套件 | Import 路徑 | 原始碼版本 | 說明 |
|------|-------------|------------|------|
| **otel-flags** | `github.com/akira-core/instrumentation-go/otel-flags` | 0.2.0 | 共享功能開關層:優先級階梯、OpenFeature resolver、單一 provider 保證。四個 wrapper 都 require 它;應用程式很少直接引入。本身不產生 span。 |
| **otel-sampler** | `github.com/akira-core/instrumentation-go/otel-sampler/otelsampler` | 0.1.1 | 一致機率取樣器（`ot=th:`／`ot=rv:`）與 `WithSingleLinkSeed`，讓 span-link 消費者的取樣決策與 parent-child 一致。本身不產生 span。 |
| **otel-testkit** | `github.com/akira-core/instrumentation-go/otel-testkit/harness` | 未打 tag | 黑箱 E2E harness（行程內 OTLP sink + collector + 斷言），供本倉庫取樣測試使用。僅供測試，不保證 API 穩定。 |

`otel-sampler` 已發布的 `v0.1.0` tag 指向 rebase 前的 commit，已作廢——請從 `0.1.1` 起用。`otel-mongo/v2` 歷史上的 `otel-mongo/v2/v0.x.y` tag 從來就無法被 `go get` 解析，僅保留作為歷史紀錄——請改用 `otel-mongo/v2.x.y` 形式。詳見 [VERSIONING.md](VERSIONING.md)。

各模組詳細文件：[otel-mongo/README.md](otel-mongo/README.md)、[otel-nats/README.md](otel-nats/README.md)、[otel-gorilla-ws/README.md](otel-gorilla-ws/README.md)；三個模組皆另有繁中版：[otel-mongo/README.zh-TW.md](otel-mongo/README.zh-TW.md)、[otel-nats/README.zh-TW.md](otel-nats/README.zh-TW.md)、[otel-gorilla-ws/README.zh-TW.md](otel-gorilla-ws/README.zh-TW.md)。

## 安裝

依模組路徑搭配對應 **Git tag**（前綴與模組一致，例如 `otel-mongo/v0.8.0`）：

```bash
go get github.com/akira-core/instrumentation-go/otel-mongo@otel-mongo/v0.8.0
go get github.com/akira-core/instrumentation-go/otel-mongo/v2@otel-mongo/v2.8.0
go get github.com/akira-core/instrumentation-go/otel-nats@otel-nats/v0.9.1
go get github.com/akira-core/instrumentation-go/otel-gorilla-ws@otel-gorilla-ws/v0.8.0
```

`otel-mongo/v2` 是前綴規則唯一的例外：它的 module path 帶有 `/v2` 主版號後綴，Go 會把該後綴從 tag 前綴中去掉，因此它的 tag 形式是 `otel-mongo/v2.x.y`——**不是** `otel-mongo/v2/v…`。

程式中再 import 子套件（`.../otelmongo`、`.../otelnats`、`.../oteljetstream`；WebSocket 為根套件）。

## 追蹤功能開關

每個模組都可以透過 [OpenFeature](https://openfeature.dev) 連上的
[GO Feature Flag](https://gofeatureflag.org) relay proxy **打開或關掉** —— 不需要重啟應用程式。

每個開關沿著一道四階梯解析,最先表態的那一層贏:

```
relay  >  env  >  option(With*Enabled)  >  寫死的預設值
```

relay **兩個方向都有權威**。讓這件事安全的是**預設值**,不是對 relay 的限制:每個 per-module 開關預設
**關閉**,所以什麼都沒設定的 process 不會產生任何 trace。既沒安裝 provider 也沒設 endpoint 的應用程式
完全不會執行任何 OpenFeature 程式碼,行為就跟它的環境變數與選項說的一模一樣。

| Relay flag key | 配對的環境變數 | 選項 | 預設值 | 範圍 |
|---|---|---|---|---|
| `otel-instrumentation-go-tracing` | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | — | `true` | process 層級**否決權**:`false` 停掉一切,`true` 什麼都不做 |
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-mongo` + `otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `WithTracePropagationEnabled` | `false` | 寫進你自己 document 的 `_oteltrace` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otelnats` + `oteljetstream` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-gorilla-ws` |

`tracing = master && moduleTracing`;`propagation = tracing && mongoPropagation`。

要連上 relay,設一個環境變數就好,不用寫任何程式碼:

```sh
OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT=http://relay:1031
```

開關只由 `1`/`true`/`yes`/`on` 或 `0`/`false`/`no`/`off` 決定。未設定代表「沒有意見」,往下一層掉。
**其他任何值——包含空字串——都是建構錯誤**,所以升級前請先稽核你的 `OTEL_*_ENABLED` 值。

> **其餘全部在 [docs/feature-flags.zh-TW.md](docs/feature-flags.zh-TW.md)**:完整的解析表與實例、為什麼選項排在環境變數之下、
> 其他 relay 連線變數、針對單一服務的 targeting、一次改動要多久生效、必須在建構 wrapper **之前**安裝
> provider 的規則、關掉一個模組**不會**停掉什麼,以及維運速查。只有一份,兩邊不會漂移。
> 實作教學:[docs/otel-nats-kill-switch.zh-TW.html](docs/otel-nats-kill-switch.zh-TW.html)
> · English:[docs/feature-flags.md](docs/feature-flags.md) ·
> [docs/otel-nats-kill-switch.en-US.html](docs/otel-nats-kill-switch.en-US.html)

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
├── otel-flags/              # 共享功能開關層（已發布，四個 wrapper 都 require）
├── otel-sampler/
│   └── otelsampler/         # 一致機率取樣器（已發布，由應用程式引入）
├── otel-testkit/
│   ├── harness/             # 黑箱 E2E harness（不打 tag、僅供測試）
│   └── examples/            # httpdirect / httpdirect-stdlib 參考範本
├── docs/
│   ├── feature-flags.md              # Tracing feature-flag 完整參考（英文）
│   ├── feature-flags.zh-TW.md        # …繁體中文翻譯
│   ├── otel-nats-kill-switch.en-US.html  # Relay 實作教學（en-US）
│   └── otel-nats-kill-switch.zh-TW.html  # Relay 實作教學（zh-TW）
├── openspec/                # Capability 規格與變更歷史（見 openspec/specs/）
├── VERSIONING.md            # 各模組 tag 線與發布順序
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
