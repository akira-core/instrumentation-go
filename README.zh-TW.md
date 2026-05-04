# instrumentation-go

本倉庫提供 **NATS**（核心與 JetStream）、**MongoDB**（Go driver v1 與 v2）以及 **gorilla/websocket** 的 OpenTelemetry 封裝，設計上對齊 [OTel Go Contrib 儀表化指引](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation)。

共有 **四個獨立的 Go 模組**（各目錄自有 `go.mod`），**版本與 Git tag 分開管理**。模組使用 **Go 1.24**。CI 會對每個模組執行 `go build`、`go test -race`、**golangci-lint**，通過後再跑需 Docker 的 **整合測試**（testcontainers）— 見 [.github/workflows/ci.yml](.github/workflows/ci.yml)。

封裝**不會**自行建立全域 `TracerProvider`，預設使用 `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()`；需要時可透過 `WithTracerProvider`、`WithPropagators` 覆寫。**應用程式**須在啟動時安裝 TracerProvider 與 W3C 傳播器（各模組的 **example/** 有完整範例）。

**English:** [README.md](README.md)

## 套件一覽

| 套件 | Import 路徑 | 原始碼版本 | 說明 |
|------|-------------|------------|------|
| **otel-mongo** (v1) | `github.com/Marz32onE/instrumentation-go/otel-mongo/otelmongo` | 0.2.11 | MongoDB driver v1 封裝；寫入時注入 `_oteltrace`；`ContextFromDocument` 與解碼輔助；可選 deliver span。 |
| **otel-mongo/v2** | `github.com/Marz32onE/instrumentation-go/otel-mongo/v2` | 0.2.11 | MongoDB driver v2 封裝；與 v1 行為對齊。 |
| **otel-nats** | `github.com/Marz32onE/instrumentation-go/otel-nats/otelnats` | 0.2.11 | 核心 NATS；W3C 脈絡在訊息標頭；deliver span。 |
| **otel-nats** | `github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream` | 0.2.11 | JetStream 發布／消費／fetch；deliver span。 |
| **otel-gorilla-ws** | `github.com/Marz32onE/instrumentation-go/otel-gorilla-ws` | 0.2.11 | 在 JSON 訊息本文內傳遞 trace context（信封格式）；`NewConn` / `Conn.Dial`。 |

各模組詳細文件：[otel-mongo/README.md](otel-mongo/README.md)、[otel-nats/README.md](otel-nats/README.md)、[otel-gorilla-ws/README.md](otel-gorilla-ws/README.md)；Mongo 與 NATS 另有繁中：[otel-mongo/README.zh-TW.md](otel-mongo/README.zh-TW.md)、[otel-nats/README.zh-TW.md](otel-nats/README.zh-TW.md)。

## 安裝

依模組路徑搭配對應 **Git tag**（前綴與模組一致，例如 `otel-mongo/v0.2.11`）：

```bash
go get github.com/Marz32onE/instrumentation-go/otel-mongo@otel-mongo/v0.2.11
go get github.com/Marz32onE/instrumentation-go/otel-mongo/v2@otel-mongo/v2/v0.2.11
go get github.com/Marz32onE/instrumentation-go/otel-nats@otel-nats/v0.2.11
go get github.com/Marz32onE/instrumentation-go/otel-gorilla-ws@otel-gorilla-ws/v0.2.11
```

程式中再 import 子套件（`.../otelmongo`、`.../otelnats`、`.../oteljetstream`；WebSocket 為根套件）。

## 追蹤功能開關

環境變數為**未設定視為關閉**。設成 `0`、`false`、`no`、`off`（不分大小寫）亦為關閉；其餘非空字串視為**開啟**。

| 環境變數 | 作用範圍 | 未設定時 | 說明 |
|----------|----------|----------|------|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | 全部模組 | 關 | 總開關；須開啟後，各模組追蹤旗標與（Mongo 的）文件傳播才會生效。 |
| `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo` + `otel-mongo/v2` | 關 | CLIENT span、deliver span 相關邏輯、非 noop tracer。 |
| `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo` + `otel-mongo/v2` | 關 | `_oteltrace` 寫入／讀取抽取；仍受上述總開關約束。 |
| `OTEL_NATS_TRACING_ENABLED` | `otelnats` + `oteljetstream` | 關 | NATS／JetStream 封裝追蹤。 |
| `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` | 關 | WebSocket 封裝追蹤。 |

若**總開關**關閉，模組層旗標不會生效。Mongo 的 `WithTracePropagationEnabled` 無法在總開關關閉時繞過並啟用文件傳播。

## 目錄結構

```
instrumentation-go/
├── otel-mongo/
│   ├── otelmongo/           # v1 封裝（模組根）
│   ├── v2/                  # v2 封裝（獨立 go.mod）
│   ├── example/
│   ├── tests/integration/   # Docker：testcontainers
│   └── README.md
├── otel-nats/
│   ├── otelnats/
│   ├── oteljetstream/
│   ├── example/
│   ├── tests/integration/
│   ├── go.mod
│   └── README.md
├── otel-gorilla-ws/
│   ├── example/
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

可執行範例：**otel-nats/example**、**otel-mongo/example**、**otel-gorilla-ws/example**。

## 診斷日誌

各套件使用 [`log/slog`](https://pkg.go.dev/log/slog)；預設 handler 下通常**不會輸出**，除非提高層級。

| 套件 | 層級 | 內容 |
|------|------|------|
| `otel-nats` | `DEBUG` | 伺服器位址解析失敗、deliver tracer 初始化成功 |
| `otel-nats` | `WARN` | Deliver tracer 初始化失敗（端點缺漏或無法連線） |
| `otel-mongo` | `DEBUG` | Deliver tracer 初始化成功 |
| `otel-mongo` | `WARN` | OTLP exporter／resource 建立失敗 |

範例：

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

日誌會帶前綴（如 `otelnats:`、`otelmongo:`）與結構化欄位（`reason`、`error`、`service`、`endpoint`）。

---

## `OTEL_EXPORTER_OTLP_ENDPOINT` 格式

**Deliver span**（otel-mongo、otel-nats）會讀此變數以建立獨立 exporter，產生中介／broker 類型的合成 span。端點須寫清楚：

| 協定 | 格式 | 範例 |
|------|------|------|
| OTLP/HTTP | 含 scheme 的完整 URL | `http://otel-collector:4318` |
| OTLP/gRPC | `host:port`（無 scheme） | `otel-collector:4317` |

僅寫主機名、無 port 或無 scheme（例如單獨 `otel-collector`）**不支援**。
