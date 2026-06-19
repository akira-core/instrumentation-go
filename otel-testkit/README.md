# otel-testkit

可重複使用的 **end-to-end 整合測試工具箱**,用來驗證 [`otel-sampler`](../otel-sampler) 的
*consistent probabilistic sampling* 與 *span-link randomness 傳遞*,在各 instrumentation
package(otel-mongo、otel-nats、HTTP、gRPC…)上跨 service、跨 feature flag 都正確生效。

它是 **工具箱,不是框架**:你用 instrumentation library **像真實應用程式一樣**驅動自己的流量,
每個 service 的 `TracerProvider` 把 span 匯到本套件啟動的 collector;你在**種下一個已知的 randomness
value(rv)** 後,直接對 collector 收到的 span 斷言。Trace 的 inject/extract 與
parent-child / span-link 拓樸**全部來自 library 本身** —— harness 不注入、不抽取、也不強制拓樸。

> **為什麼是 black-box?** instrumentation library 的設計就是讓 tracing 與一般資料收送透明融合:
> 應用程式正常呼叫(`InsertOne`、`http.Get`…),library 自動把 trace 放進載體、在對端取回。
> 測試只要把各 service 的 OTLP endpoint 指到我們的 collector、跑真實流程、再驗 span 即可。
> 同一套 helper 也能直接套到**真實 application**(見下文)。

---

## 目錄

- [運作原理](#運作原理)
- [需求](#需求)
- [快速開始](#快速開始)
- [API 一覽](#api-一覽)
- [3 步測你的 library](#3-步測你的-library)
  - [範例:pull 型(MongoDB)](#範例pull-型mongodb)
  - [範例:push 型(HTTP / gRPC)](#範例push-型http--grpc)
- [測同一 library 的不同傳遞方法](#測同一-library-的不同傳遞方法)
- [feature-flag 矩陣](#feature-flag-矩陣)
- [套用到真實 application](#套用到真實-application)
- [疑難排解](#疑難排解)

---

## 運作原理

```
                          (測試行程 / host)
 ┌───────────────────────────────────────────────────────────────────────┐
 │  你寫的真實流程:svc0 ─(library 正常呼叫)─▶ svc1 ─▶ svc2 …               │
 │   每個 svc 一個 TracerProvider(各自取樣率)+ OTLP exporter             │
 │                     │ rv 由 library 真實 inject→extract 傳遞            │
 │                     ▼ OTLP/gRPC                                         │
 │               ┌───────────────┐   OTLP/gRPC                            │
 │               │ otel-collector│──────────────┐                         │
 │               │  (container)  │ host.test-    │                         │
 │               └───────────────┘ containers    ▼                         │
 │   broker / server(視 library 而定)          in-process OTLP sink      │
 │                                                (gRPC server,直接斷言)  │
 └───────────────────────────────────────────────────────────────────────┘
```

1. `BuildTracerProvider` 為每個 service 建一個 `TracerProvider`(service identity + 取樣率),OTLP 匯到 collector。
2. 你用 library **正常驅動**一條流程:trace 經 library 自己的載體(mongo `_oteltrace`、HTTP headers…)
   做真實的 inject→extract;`rv` tracestate 真的走了一遍。
3. **被 sampler drop 的 service 不會輸出 span**(sink 上缺席),但載體仍帶著 rv 傳給下一個 service。
4. collector 收到後再 OTLP 匯出回**行程內 sink**;測試直接對 sink 內的 `Span` 斷言。

> **為什麼用真的 collector 而不是 `tracetest.SpanRecorder`?** 取樣一致性的價值在**跨 service**——
> 多個各自取樣率不同的 service 對同一條 trace 必須做出一致(巢狀)的 sample/drop 決策;collector 是把
> 多個獨立 TracerProvider 匯出的 span 聚合、可一次斷言的天然匯流點,同時也驗證真實 OTLP 鏈路把
> `rv` tracestate 帶過去。

---

## 需求

- **Docker 或 Podman**(testcontainers 會拉 `mongo:7.0`、`otel/opentelemetry-collector` 等 image)。
- Go 1.24+。
- 可用 `OTEL_COLLECTOR_IMAGE` 覆寫 collector image(預設 `otel/opentelemetry-collector:0.119.0`)。
- 沙箱 / DinD 環境可設 `TESTCONTAINERS_RYUK_DISABLED=true` 略過 reaper sidecar。

---

## 快速開始

```bash
# mongo 的完整 flag 矩陣(5 種組合,各一次 go test)
make test-integration-sampling

# HTTP 黑箱示範
make test-integration-http-direct

# 手動跑單一組合(mongo)
cd otel-mongo/v2/tests/integration
OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=1 OTEL_MONGO_TRACING_ENABLED=1 \
OTEL_MONGO_PROPAGATION_ENABLED=1 OTEL_TRACES_SAMPLER_ARG=1.0 \
go test -race -timeout 600s ./sampling/
```

---

## API 一覽

工具箱由幾個彼此獨立、可自由組裝的元件構成(package `harness`):

**基礎設施**
- `StartSink(t) *Sink` — 行程內 OTLP/gRPC sink。`Sink` 提供 `Spans()`、`WaitFor(timeout, pred)`、`ByRun(runID)`、`Port()`。
- `StartCollector(ctx, t, sinkPort) (endpoint string)` — collector 容器,回傳 OTLP endpoint。
- `BuildTracerProvider(t, serviceName, sampler, endpoint) *sdktrace.TracerProvider` — 一個 service 的 TP(同步匯出,sampling 決策即時可見)。

**sampler(務必用這個)**
- `ConsistentSampler(rate) sdktrace.Sampler` = `WithSingleLinkSeed(ProbabilitySampler(rate))`。
- `ConsistentSamplerFromEnv(def) sdktrace.Sampler` = 同上,但機率讀 `OTEL_TRACES_SAMPLER_ARG`。
- **一定要 `WithSingleLinkSeed` 包起來**(`ConsistentSampler` 已內含):否則 (a) span-link consumer 會自生 rv(`ProbabilitySampler` 無視 links)→ 不一致;(b) root 不會寫 `ot=rv:` → 匯出的 span 沒有 rv,`AssertConsistentRV` 找不到值。harness 不強制 sampler,但你的服務該用這顆。

**rv 與取樣**
- `SeedContextRV(rv) context.Context` — 回一個帶 `ot=rv:` tracestate 的 remote root context,當流程的頭(等同正常 inbound 載體帶 rv)。
- `RandomRV() uint64`、`ExpectedSampled(rate, rv) bool`、`EnvSamplerArg(def) float64`。

**分組 / 切片**
- `SpansByAttr(spans, attr, val)`、`SpansByService(spans, name)`、`SpansByScope(spans, scope)`、`SpansByServicePrefix(spans, prefix)`。
- `SpansOfRun(all, runID)` — 收齊整條 run 的**所有** span(app + wrapper client + deliver + 經 span-link 連到的 trace);要連 wrapper/deliver 一起驗時用它。

**斷言 — 拓樸無關核心(只需 rates)**
- `AssertConsistentRV(t, spans) uint64` — 出現的 span 其 `ot=rv:` 全相等(核心不變式)。
- `AssertPresence(t, spans, want, rv)` / `AssertAppSpanCounts(t, spans, want, rv, countIfSampled)` — service 出現 / 精確 span 數 ⇔ `ExpectedSampled(rate, rv)`。
- `SampledFraction(runs, service)` / `AssertSampledFraction(t, runs, service, rate, delta)` — 多條隨機 rv run 的取樣比例 ≈ rate。
- `AssertNoWrapperSpans(t, spans, scope)`(flag 停用)、`DistinctRVs(spans)`(propagation 關掉時驗 rv 不一致)。

**斷言 — 結構性(需知該段是 PC 或 link;選用)**
- `AssertSameTrace(t, spans)` — 全部同一 TraceID(parent-child 段)。
- `AssertLinkedTrace(t, spans, fromService, toService)` — `to` 是 `from` 的 span-link consumer(不同 trace、且有 `Link` 指向 `from` 的 trace)。

> 不在乎 trace 圖形狀就只用核心斷言,不必傳拓樸。

**flag 推導**
- `GateEnv{Global, Tracing, Propagation}` + `ExpectationFromEnv(gate) Expectation{TracingEnabled, PropagationEnabled}` — 把當前 flag 矩陣轉成「該跑哪組斷言」。

`Span` 是攤平、好斷言的 view:`ServiceName`、`Scope`、`Name`、`TraceID`、`SpanID`、`ParentSpanID`、
`TraceState`、`Attributes map[string]string`、`Links`,加上 `RV() (uint64, bool)` / `TH() (string, bool)`。

### 分組一條 run:`RunAttr`

一次測試常驅動多條 run,sink 會把**所有 run、所有 service、加上 wrapper/deliver span 全部累積**。要對「一條 run」斷言,先用 `SpansByAttr(spans, harness.RunAttr, runID)` 篩出來。

- **`RunAttr` 是測試碼加的**(你 `tracer.Start` 時 `trace.WithAttributes(attribute.String(harness.RunAttr, runID))`),**不改 instrumentation library**。
- **不能用 traceID 分組**:span-link hop 會開新 traceID,一條邏輯 run 橫跨多個 trace。
- **只有你開的 application span 帶 `RunAttr`**;wrapper client span(子 span,屬性不繼承)與 deliver span(獨立 TP)**都不帶** → `InsertOne` 多送的 client/deliver span 不會混進 app 計數,`AssertAppSpanCounts` 才精確。**要連 wrapper/deliver 一起看,用 `SpansOfRun`**(沿 traceID + link 收齊)。

---

## 3 步測你的 library

1. **建 service 的 TracerProvider** → 指向 collector:
   `tp := harness.BuildTracerProvider(t, "svc0", harness.ConsistentSampler(rate), endpoint)`。
2. **寫真實流程**:用你的 library 正常 produce→consume(consumer 用 library 的 extract API 續接 trace);
   開 application span 時帶上 `harness.RunAttr`。topology 隨你用的方法自然成形。
3. **種 rv + 斷言**:`harness.SeedContextRV(rv)` 當流程頭,跑完用 `AssertConsistentRV` / `AssertPresence` /
   `AssertNoWrapperSpans` 驗 sink 上的 span。

> 沒有要實作的 harness 介面;不需要把 trace context 手動回傳給 harness。

### 範例:pull 型(MongoDB)

讀取端**主動**從文件取回 trace(`SingleResult.TraceContext()`),續接為 parent-child:

```go
// 每個 service 一個 client/TP,共用一個 collection
tp := harness.BuildTracerProvider(t, "svc1", harness.ConsistentSampler(0.5), endpoint)
client, _ := otelmongo.NewClient(uri, otelmongo.WithTracerProvider(tp))
coll := client.Database("testkit").Collection("chain")

// svc0 produce(從種下的 rv 起頭)
c0, s0 := tp0.Tracer("chain").Start(harness.SeedContextRV(rv), "svc0",
    trace.WithAttributes(attribute.String(harness.RunAttr, runID)))
coll0.InsertOne(c0, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}})
s0.End()

// svc1 consume:讀回文件、用 library 取出遠端 trace、續接
sr := coll1.FindOne(ctx, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}})
_, s1 := tp1.Tracer("chain").Start(sr.TraceContext(), "svc1",
    trace.WithAttributes(attribute.String(harness.RunAttr, runID)))
s1.End()

// 斷言
run := harness.SpansByAttr(sink.Spans(), harness.RunAttr, runID)
harness.AssertPresence(t, run, map[string]float64{"svc0": 0.9, "svc1": 0.5}, rv)
harness.AssertConsistentRV(t, run)
```

完整可執行版本(含 rv ladder、flag 矩陣、Aggregate 與 span-link 變體)在
[`otel-mongo/v2/tests/integration/sampling/`](../otel-mongo/v2/tests/integration/sampling)。

> **單節點 replica set 連線注意**:testcontainers 的 mongo 模組會把 replica-set 成員設成容器內部 IP,
> 並在連線字串附上 `replicaSet=rs0`,使 driver 走 SDAM 探索而連不到內部 IP。範例 `startMongo` 改用
> `directConnection=true` 直連 mapped port(單節點即 primary,讀寫 / change stream 皆可)。
> 又因 harness collector 是明文 gRPC,範例另設 `OTEL_EXPORTER_OTLP_INSECURE=true`,讓 wrapper 的
> deliver-span exporter 也以 insecure 連線。

### 範例:push 型(HTTP / gRPC)

同步 RPC 沒有「主動取回」:trace 在 **server 端 middleware** 被 extract 進 handler 的 `r.Context()`,
chain 由各 handler **往前呼叫**串起來。測試只對頭發一個帶 seeded rv 的請求,再對 sink 斷言:

```go
func (s *service) handle(w http.ResponseWriter, r *http.Request) {
    ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
    ctx, span := s.tracer.Start(ctx, s.name,
        trace.WithAttributes(attribute.String(harness.RunAttr, r.URL.Query().Get("run"))))
    defer span.End()
    if s.next != "" { // 往前呼叫後繼 service
        req, _ := http.NewRequestWithContext(ctx, r.Method, s.next+"/?run="+/*runID*/"", nil)
        otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
        resp, _ := http.DefaultClient.Do(req); resp.Body.Close()
    }
    w.WriteHeader(http.StatusOK)
}
```

完整可執行範例在 [`examples/httpdirect/`](examples/httpdirect)。**gRPC 同構**:server 端用 interceptor
`Extract(MetadataCarrier)`、client 端 `Inject` 進 `metadata.MD`,其餘斷言完全一樣。

---

## 測同一 library 的不同傳遞方法

「傳遞方法」是參數而非型別:同一個 library 的每種收送方式寫成一個獨立流程 / 子測試,套**同一組 helper**。
不同方法常會帶出不同拓樸,而 harness 不需任何改動:

| library | 傳遞方法 | 拓樸 |
|---|---|---|
| MongoDB | `InsertOne`→`FindOne`(`SingleResult.TraceContext`) | parent-child(續接) |
| MongoDB | `InsertOne`→`Aggregate`(`Cursor.DecodeWithContext`) | parent-child(不同指令) |
| MongoDB | change-stream consumer / link 到 producer | **span-link**(新 trace id) |
| HTTP | 不同 endpoint / method | parent-child |

mongo 的測試檔即示範 `find`、`aggregate`、span-link 三種,各自 `AssertConsistentRV` + `AssertAppSpanCounts`;
`TestMongoTopologyIndependence` 進一步在 3-node 下跑 `[PC,PC]/[PC,link]/[link,PC]/[link,link]` 四種組合,
斷言**最後一個 node 的 rv == head 種下的 rv**、且四種拓樸的 present-set 與 rv 完全一致。

### wrapper + deliver span 也要一起驗(`SpansOfRun`)

otel-mongo 的 `InsertOne` 會產生 **CLIENT span(producer)**,其下再掛一個 **DELIVER span**(service `mongodb://…`,
模擬「broker 收到」);文件 `_oteltrace` 帶的是 deliver span 的 context,consumer 由此接續。它們與 application span 同一條
trace、rv 一路繼承,所以 **producer / consumer / deliver 的 randomness 一定相同**。要一起驗:

```go
full := harness.SpansOfRun(sink.Spans(), runID) // app + client + deliver + 經 link 的 trace
harness.AssertConsistentRV(t, full)             // 三者 rv 全相同
// 角色計數:用 SpansByScope(otelmongo.ScopeName) 取 wrapper、SpansByServicePrefix("mongodb") 取 deliver
```

> **deliver span 的取樣率 = 全域 `OTEL_TRACES_SAMPLER_ARG`,與 node rate 解耦**(目前 library 行為:deliver TP 寫死
> `ProbabilitySamplerFromEnv(1.0)`)。`TestMongoFullSpanShape` 即驗這個現況(producer/consumer 用不同 node rate,deliver 用全域 rate)。
>
> 另注意:mongo 的**讀取**操作(`FindOne`/`Aggregate`)以自己的 context 執行,其 client/deliver span 落在**獨立的 trace**;
> 一條邏輯 run 裡的 deliver span 是 producer 的 **insert** delivery,consumer 的貢獻是它**接續**的 application span。

---

## feature-flag 矩陣

feature-flag gate 每個 process 只快取一次,所以**「開」與「關」各組合要用不同 env 各跑一次 `go test`**。
用 `ExpectationFromEnv(gate)` 推導當前該驗什麼:

| tracing | propagation | 驗證 |
|---|---|---|
| 開 | 開 | `AssertConsistentRV` + `AssertPresence`(取樣一致性核心) |
| 開 | 關 | `DistinctRVs(run)` > 1:rv **不再一致**(載體沒帶 rv,各 service 用自身 rv) |
| 關 | — | `AssertNoWrapperSpans(spans, scope)`:無 wrapper span,但 application span 仍在 |

mongo 的完整矩陣由 `make test-integration-sampling` 跑:

| Row | GLOBAL | MONGO_TRACING | MONGO_PROP | SAMPLER_ARG | 驗證重點 |
|---|---|---|---|---|---|
| 1 | 1 | 1 | 1 | 1.0 | 全鏈路 + 一致性 + deliver span |
| 2 | 1 | 1 | 1 | 0.5 | 取樣率 + 一致性 |
| 3 | 1 | 1 | 0 | 1.0 | propagation 關:rv 不一致 |
| 4 | 1 | 0 | 1 | 1.0 | mongo tracing 關:無 wrapper span |
| 5 | 0 | 1 | 1 | 1.0 | global 關:全關 |

> `OTEL_EXPORTER_OTLP_ENDPOINT` 與各 service 取樣率由測試在行程內設定/帶入;你只需控制旗標軸 + `OTEL_TRACES_SAMPLER_ARG`。

---

## 套用到真實 application

同一套工具直接套到**已用 instrumentation library 的真實服務**——不需要 library 範例那種手寫 service:

1. `sink := harness.StartSink(t)`;`endpoint := harness.StartCollector(ctx, t, sink.Port())`。
2. 把你的服務(docker-compose / 既有程式)的 `OTEL_EXPORTER_OTLP_ENDPOINT` 指向 `endpoint`,
   並用 consistent sampler **`harness.ConsistentSamplerFromEnv(...)`**(即 `WithSingleLinkSeed(ProbabilitySamplerFromEnv(...))` + `OTEL_TRACES_SAMPLER_ARG`)。
   **務必包 `WithSingleLinkSeed`**:span-link 邊的一致性、以及讓 root span 真的寫出 `ot=rv:`,都靠它(見 [API 一覽](#api-一覽))。harness 不強制,由你的服務 sampler 決定。
3. 對外觸發一條會跨多個服務的請求(可在入口帶上 `harness.RunAttr` 或自訂 correlation 屬性 / 已知 traceparent+tracestate)。
4. 用同一組 helper 斷言:

```go
spans := sink.WaitFor(20*time.Second, func(ss []harness.Span) bool { return len(ss) > 0 })
run := harness.SpansByAttr(spans, harness.RunAttr, runID)   // 或 sink.ByRun(runID)
harness.AssertConsistentRV(t, run)                          // 跨 service rv 一致
harness.AssertPresence(t, run, map[string]float64{ /* service.name → 取樣率 */ }, rv)
```

> library 測試與 app 測試是**同一套路**:差別只在 service 是你手寫的範本還是真實部署的服務。

---

## 疑難排解

| 症狀 | 處理 |
|---|---|
| `collector` 容器拉不下來 / tag 不存在 | 用 `OTEL_COLLECTOR_IMAGE=...` 覆寫成可用版本 |
| Ryuk / reaper 在沙箱中起不來 | `export TESTCONTAINERS_RYUK_DISABLED=true` |
| sink 收不到 span(`WaitFor` 逾時) | 確認容器能連到 host:多半是 `host.testcontainers.internal` / host-gateway 在你的 Docker / Podman 設定下不可用 |
| mongo `ReplicaSetNoPrimary` / server selection timeout | 用 `directConnection=true` 直連 mapped port(見 pull 範例註解);別讓 driver 走 replica-set 探索到容器內部 IP |
| deliver span 匯不出 / `tls: first record does not look like a TLS handshake` | collector 是明文 gRPC;設 `OTEL_EXPORTER_OTLP_INSECURE=true` 讓 wrapper 的 deliver exporter 走 insecure |
| 旗標「開/關」兩種狀態互相干擾 | gate 每 process 快取一次——務必用 env 矩陣**分多次** `go test`,勿在單一 run 內切換 |
| 一致性斷言偶發失敗且 rv 在門檻邊界 | 選 rv 時遠離 `threshold ≈ (1-p)·2^56`(範例的 rv ladder 已刻意挑大邊際) |
| Docker daemon 卡住(volume metadata DB timeout) | 環境問題,非測試問題;清掉 stale 的 dockerd 鎖後重啟 daemon |
