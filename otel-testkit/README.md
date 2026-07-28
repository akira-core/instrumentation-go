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

- [開始之前(onboarding)](#開始之前onboarding)
- [運作原理](#運作原理)
- [兩種 span(你開的 vs library 產生的)](#兩種-span你開的-vs-library-產生的)
- [能測什麼(測試類別)](#能測什麼測試類別)
- [需求](#需求)
- [快速開始](#快速開始)
- [API 一覽](#api-一覽)
- [3 步測你的 library](#3-步測你的-library)
  - [範例:pull 型(MongoDB)](#範例pull-型mongodb)
  - [範例:push 型(HTTP / gRPC)](#範例push-型http--grpc)
- [最小骨架(複製即改)](#最小骨架複製即改)
- [控制送幾條 trace / 每條幾個 span](#控制送幾條-trace--每條幾個-span)
- [trace 怎麼過界(propagation)](#trace-怎麼過界propagation)
- [同步 vs 非同步匯出(斷言計時)](#同步-vs-非同步匯出斷言計時)
- [測同一 library 的不同傳遞方法](#測同一-library-的不同傳遞方法)
- [feature-flag 矩陣](#feature-flag-矩陣)
- [套用到真實 application](#套用到真實-application)
- [失敗怎麼定位](#失敗怎麼定位)
- [疑難排解](#疑難排解)

---

## 開始之前(onboarding)

### 5 分鐘第一個綠燈

最短 path,先確認鏈路通(只需 Docker,不必懂 mongo):

```bash
make test-integration-http-direct
```

綠燈 = sink、collector 容器、OTLP 鏈路、consistent sampler 全部就緒。接著打開
[`examples/httpdirect/http_test.go`](examples/httpdirect/http_test.go),它就是一個最小可執行範本 ——
對照三段:**setup**(`StartSink`/`StartCollector`)→ **drive**(種 rv、跑流程)→ **assert**(`AssertConsistentRV`/`AssertAppSpanCounts`)。

### 先讀這 3 節,其餘按需再讀

| 一定要讀 | 何時再讀 |
|---|---|
| [運作原理](#運作原理)、[3 步測你的 library](#3-步測你的-library)、[最小骨架](#最小骨架複製即改) | — |
| — | 你的 library **有 feature flag** → [feature-flag 矩陣](#feature-flag-矩陣) |
| — | library 會**自開 broker 節點 span 且批次匯出** → [同步 vs 非同步](#同步-vs-非同步匯出斷言計時) + `SpansOfRun` |
| — | 想驗**統計取樣率** → [控制送幾條 trace](#控制送幾條-trace--每條幾個-span) 的統計段 |

### 決策樹:我該照哪個範例

```
你的 library 怎麼傳 trace?
├─ push(同步 RPC:HTTP / gRPC,送出即送達)
│     → 照 examples/httpdirect(parent-child;對 head 發一個請求)
└─ pull(消費端主動讀:DB / queue / change stream)
      → 照 otel-mongo/v2/.../sampling(produce → consume)
        消費端怎麼接上游?
        ├─ 續接 remote parent(同 trace)        → parent-child
        └─ 開新 root + link 到上游              → span-link
        (兩者取樣決策相同,都用 AssertConsistentRV 驗)

其他軸(與上面正交,有才讀):
• 有 tracing / propagation 開關?  → 宣告 GateEnv,跑 env 矩陣
• library 會自開 broker 節點 span?→ 用 SpansOfRun,並拉長 WaitFor 或觸發 flush
```

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

## 兩種 span(你開的 vs library 產生的)

> 常見疑問:「我用 library 做一個操作,不就產生一個 span 嗎?」——**不一定**。一次被 instrument 的呼叫可能產生**不只一個** span,而且它們來源不同、用途也不同。先分清楚這兩種:

- **application span**(你開的):你在測試/應用裡 `tracer.Start(...)` 自己開的 span,並帶上 `harness.RunAttr`。它是**你定義「一次邏輯操作」的單位**——例如「svc1 處理了這筆訊息」。
- **library-emitted span**(library 自動產生的):你呼叫一個被 instrument 的操作(`InsertOne`、`http.Get`、`publish`…)時,**library 內部自動**多開的 span —— 通常是 CLIENT / SERVER span;**某些 library 還會額外開一個 span 來模擬 broker 節點**(例如 otel-mongo / otel-nats 的「deliver span」,讓 service graph 出現一個 `mongodb://…` / `nats://…` 節點)。這種 broker span 是**個別 library 的選配產物、屬 transport 細節**,不是通用要件。

所以「做一個操作」實際可能是:**你的 app span(可選)＋ library 自動開的一個或多個 span**。

### library-emitted span 不帶 `RunAttr`,sink 怎麼分辨它屬於哪條 run?

這是最容易卡住的點,拆開講:

- `RunAttr` 是**測試碼自己**塞的:`tracer.Start(ctx, name, trace.WithAttributes(attribute.String(harness.RunAttr, runID)))`。library-emitted span 在 **library 內部**被建立,你的測試碼**碰不到那個 `Start` 呼叫**,塞不進屬性。
- **OTel 的屬性不會 parent→child 繼承**:就算 library 的 CLIENT span 是你 app span 的子 span,它也**不會**自動拿到 `RunAttr`。要它帶上,只能**改 library** 去傳一個測試專用屬性 → 違反 black-box(整套架構的前提是「不改 library」)。**所以是做不到,不是選擇不做。**
- sink **不靠 `RunAttr`** 認 library span,**靠 trace 圖譜**。`harness.SpansOfRun(all, runID)` 的做法:先用 `RunAttr` 找出這條 run 的 app span 當**錨點** → 收齊所有**同 traceID** 的子孫(library 的 CLIENT / broker span)→ 再沿 **span link** 遞迴收 link 到的 trace。每條 run 都用 `SeedContextRV(rv)` 起一個**新的隨機 traceID**,所以 traceID(+ link)足以把不同 run 切開,根本不需要 library span 也帶 runID。

因此有**兩種取法,各司其職**:

| 取法 | 拿到什麼 | 用途 |
|---|---|---|
| `SpansByAttr(spans, RunAttr, runID)` | **只有**你的 app span | 精確計數(`AssertAppSpanCounts`)。否則同 `service.name` 的 library CLIENT span 會混進來灌爆計數 |
| `SpansOfRun(spans, runID)` | 整條 run 的**全部** span(app + library) | 要連 library-emitted span 一起驗 randomness 時用 |

> 靠 traceID 譜系其實**比「人人都帶 RunAttr」更穩**:不依賴 library 願不願意傳屬性,也天然涵蓋跨 span-link 的新 trace。

---

## 能測什麼(測試類別)

不論你包的是什麼 transport,這個工具箱讓你對「跨 service 的取樣行為」做這幾類斷言。**先看你要驗哪一類,再去寫流程**:

| 想驗什麼 | 用的 helper | 範例(在哪個測試) |
|---|---|---|
| 同一條 trace 的 randomness 跨 service **不變** | `AssertConsistentRV` | 幾乎每個測試 |
| service 依自身取樣率 **被取樣 / 被 drop** | `AssertPresence` / `AssertAppSpanCounts`(⇔ `ExpectedSampled`) | `TestMongoSamplingSuite`、HTTP |
| **拓樸無關**:parent-child 與 span-link 給出**相同**取樣決策 | 跨拓樸 `AssertConsistentRV` + present-set 比對 | `TestMongoTopologyIndependence` |
| **統計取樣率**:大量 trace 的取樣比例 ≈ rate | `AssertSampledFraction` | `TestMongoSamplingRate` |
| **feature flag 停用**時 library-emitted span 不出 / propagation 關時 rv 不一致 | `AssertNoWrapperSpans` / `DistinctRVs` | `TestMongoSamplingSuite` 的停用分支 |
| **library-emitted span**(含模擬 broker 的 deliver span)也與 application span 同 randomness | `SpansOfRun` + 角色計數 | `TestMongoFullSpanShape` |

這些斷言都**只看 sink 收到的 span**(讀 tracestate 的 `rv`、service.name、scope、links),與你用哪種 library 無關。

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
> `rv`(randomness value)是 consistent probabilistic sampling 寫在 `tracestate` 裡的 56-bit 隨機數;一個 span 被取樣 iff `rv ≥ threshold(rate)`。它是什麼、為何讓不同 rate 的 service 對同一條 trace 做出一致決策,見 [otel-sampler](../otel-sampler)。
- `SeedContextRV(rv) context.Context` — 回一個帶 `ot=rv:` tracestate 的 remote root context,當流程的頭(等同正常 inbound 載體帶 rv)。
- `RandomRV() uint64`、`ExpectedSampled(rate, rv) bool`、`EnvSamplerArg(def) float64`。

**驅動一條 run**
- `RunAttrOption(runID) trace.SpanStartOption` — `tracer.Start(ctx, name, harness.RunAttrOption(runID))` 的簡寫,把 `RunAttr` 蓋上你的 app span。
- `CountSampled(rates, rv) int` — 預測該 rv 下有幾個 service 會被取樣(= 該等幾個 app span)。
- `WaitForAppSpans(t, sink, runID, want, timeout) []Span` — 等到 `want` 個帶 `RunAttr` 的 app span 到齊才回;**逾時會 dump 全部 span 並 `t.Fatalf`**(取代靜默回 partial)。

**分組 / 切片**
- `SpansByAttr(spans, attr, val)`、`SpansByService(spans, name)`、`SpansByScope(spans, scope)`、`SpansByServicePrefix(spans, prefix)`。
- `SpansOfRun(all, runID)` — 收齊整條 run 的**所有** span(app + library-emitted span + 經 span-link 連到的 trace);要連 library-emitted span 一起驗時用它。

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

**除錯(失敗定位)**
- `DumpOnFailure(t, sink)` — 一行掛上;測試失敗時自動把 sink 收到的全部 span 印成可讀的單行清單。
- `DumpSpans(t, header, spans)` / `Span.String()` — 手動傾印任意 span 集合。見 [失敗怎麼定位](#失敗怎麼定位)。

`Span` 是攤平、好斷言的 view:`ServiceName`、`Scope`、`Name`、`TraceID`、`SpanID`、`ParentSpanID`、
`TraceState`、`Attributes map[string]string`、`Links`,加上 `RV() (uint64, bool)` / `TH() (string, bool)`。

### 分組一條 run:`RunAttr`

一次測試常驅動多條 run,sink 會把**所有 run、所有 service、加上 library-emitted span 全部累積**。要對「一條 run」斷言,先用 `SpansByAttr(spans, harness.RunAttr, runID)` 篩出來。

- **`RunAttr` 是測試碼加的**(你 `tracer.Start` 時 `trace.WithAttributes(attribute.String(harness.RunAttr, runID))`),**不改 instrumentation library**。
- **不能用 traceID 分組**:span-link hop 會開新 traceID,一條邏輯 run 橫跨多個 trace。
- **只有你開的 application span 帶 `RunAttr`**;library-emitted span(CLIENT 子 span 屬性不繼承、broker span 在獨立 TP)**都不帶**,所以它們不會混進 app 計數,`AssertAppSpanCounts` 才精確。**要連 library-emitted span 一起看,用 `SpansOfRun`**(沿 traceID + link 收齊)。詳見 [兩種 span](#兩種-span你開的-vs-library-產生的)。

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
c0, s0 := tp0.Tracer("chain").Start(harness.SeedContextRV(rv), "svc0", harness.RunAttrOption(runID))
coll0.InsertOne(c0, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}})
s0.End()

// svc1 consume:讀回文件、用 library 取出遠端 trace、續接
sr := coll1.FindOne(ctx, bson.D{{Key: "run", Value: runID}, {Key: "hop", Value: 0}})
_, s1 := tp1.Tracer("chain").Start(sr.TraceContext(), "svc1", harness.RunAttrOption(runID))
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
    ctx, span := s.tracer.Start(ctx, s.name, harness.RunAttrOption(r.URL.Query().Get("run")))
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

## 最小骨架(複製即改)

整段複製,把 3 個 `// TODO` 換成你的 library 呼叫即可。結構固定:**setup → 每個 service 一個 TP → 種 rv 驅動一條 run → 斷言**。

```go
package mylib_sampling

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akira-core/instrumentation-go/otel-testkit/harness"
	// TODO: import 你的 instrumentation library
)

func TestMyLibSamplingConsistency(t *testing.T) {
	// 1) 基礎設施:in-process sink + collector 容器,並把 exporter 指過去
	sink := harness.StartSink(t)
	harness.DumpOnFailure(t, sink) // 失敗時自動印出收到的全部 span
	endpoint := harness.StartCollector(context.Background(), t, sink.Port())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)

	// 2) 每個 service 一個 TracerProvider(務必用 ConsistentSampler)
	rates := []float64{0.9, 0.5}
	tp0 := harness.BuildTracerProvider(t, "svc0", harness.ConsistentSampler(rates[0]), endpoint)
	tp1 := harness.BuildTracerProvider(t, "svc1", harness.ConsistentSampler(rates[1]), endpoint)
	// TODO: 用 tp0 / tp1 建你的 library client/server(WithTracerProvider(tpN))

	// 3) 種一個已知 rv,驅動「一條 run」
	rv := uint64(1) << 55 // 挑遠離門檻的值:svc0(0.9)、svc1(0.5) 都會取樣
	runID := uuid.NewString()

	// 頭:從 seeded rv 起 svc0 的 application span
	c0, s0 := tp0.Tracer("chain").Start(harness.SeedContextRV(rv), "svc0", harness.RunAttrOption(runID))
	// TODO: producer 用你的 library 送出(inject 由 library 自動做)
	s0.End()
	_ = c0

	// 對端:consumer 用你的 library 收 + extract,續接成 svc1 的 application span
	// ctx := <library 從載體 extract 出的 context>
	// _, s1 := tp1.Tracer("chain").Start(ctx, "svc1", harness.RunAttrOption(runID))
	// TODO: 上面兩行換成你的 consume 流程; s1.End()

	// 4) 等齊這條 run 的 app span(逾時會自動 dump + Fatalf),再斷言
	want := harness.CountSampled(rates, rv) // 預測該 rv 下幾個 service 會出現
	run := harness.WaitForAppSpans(t, sink, runID, want, 15*time.Second)

	harness.AssertConsistentRV(t, run) // 跨 service rv 一致
	harness.AssertAppSpanCounts(t, run, map[string]float64{"svc0": rates[0], "svc1": rates[1]}, rv, 1)
}
```

可執行的真實版本:**push** 看 [`examples/httpdirect/http_test.go`](examples/httpdirect/http_test.go);
**pull** 看 [`otel-mongo/v2/tests/integration/sampling/`](../otel-mongo/v2/tests/integration/sampling)。

---

## 控制送幾條 trace / 每條幾個 span

兩個數字你都自己掌握,**harness 不規定**:

### 幾條 trace —— 由你在 head 的迴圈次數決定

「驅動一次你的流程」= **一條 run** = 一個 `runID`(你在 head 用 `SeedContextRV(rv)` 起頭,並把 `runID` 蓋在每個 application span 的 `RunAttr` 上)。要送 **N 條** trace,就在 head **loop N 次**,每次給新的 `runID` 與新的 `rv`:

```go
for i := 0; i < N; i++ {
    runID := uuid.NewString()
    rv := harness.RandomRV()            // 或指定一個已知 rv
    drive(t, runID, harness.SeedContextRV(rv)) // ← 你的流程:produce → consume
}
```

- 想要**確定性**斷言(某 service 一定出現/缺席):用**指定的 rv**(挑遠離門檻的值,如 `1<<55`)。
- 想要**統計**斷言(取樣比例 ≈ rate):用 `harness.RandomRV()` 送很多條,再 `harness.AssertSampledFraction(runs, svc, rate, delta)`。`runs` 就是你蒐集的每條 run 的 span 集合 `[][]Span`。

> 一個 process 內可以送任意多條 run;sink 會累積全部,你用 `runID` 把每條分開(見下)。

### 每條 run 幾個 span —— 由你的 library + 流程決定

一條 run 的 span 數 = **你開的 application span 數** + **library 每個被 instrument 的呼叫自動產生的 library-emitted span 數**(CLIENT / SERVER,某些 library 還有模擬 broker 的 span)。harness 不增不減。

- 你**自己開的** application span:帶 `RunAttr` → 用 `harness.SpansByAttr(spans, harness.RunAttr, runID)` 取得**只有這些**。
- library **自動產生的** library-emitted span:**不帶** `RunAttr`(子 span 不繼承屬性、broker span 在 library 自己的 TP)→ 要連它們一起看,用 `harness.SpansOfRun(spans, runID)`(從你的 app span 沿 traceID + link 收齊整條 run)。為什麼不帶、又怎麼分辨,見 [兩種 span](#兩種-span你開的-vs-library-產生的)。

每個 transport 一次「過界」會送多少 span,取決於它的 library 怎麼設計。例如:

| transport | 一次 produce 大致送出的 span | 一次 consume |
|---|---|---|
| MongoDB | 你的 app span + `insert` CLIENT span + `mongodb://…` DELIVER span | 你的 continuation app span(+ 讀取的 CLIENT/DELIVER span,落在讀取端自己的 trace) |
| HTTP(真 otelhttp) | 你的 app span + CLIENT span | SERVER span + 你的 app span |
| 純 propagator(本 repo 的 HTTP 範例) | 只有你的 app span | 只有你的 app span |

> 重點:**presence / 計數類斷言要先用 `RunAttr` 篩你的 app span**(否則 library-emitted span 同 `service.name` 會混進來);**要驗 library-emitted span 時才用 `SpansOfRun`**。

---

## trace 怎麼過界(propagation)

**harness 不做 inject / extract** —— trace 是靠你的 **library 自己的載體**過界的,這正是要驗的東西:

| transport | 載體 | producer | consumer |
|---|---|---|---|
| MongoDB | 文件的 `_oteltrace` 欄位 | 寫入時 inject | 讀回時 `TraceContext()` / `DecodeWithContext()` extract |
| NATS / JetStream | message headers | publish 時 inject | 收到時從 header extract |
| HTTP / gRPC | request headers / metadata | 送出時 `Inject` | server middleware / interceptor `Extract` |

producer 端的 instrumented 呼叫把當前 SpanContext(含 `tracestate` 的 `ot=rv:`)寫進載體;consumer 端的 instrumented 呼叫/middleware 取回。**`rv` 就是這樣被帶到下一個 service** 的,下游 sampler 才能讀到同一個 `rv` 做一致決策。

**怎麼測 propagation 開/關**:用你 library 的 propagation gate(見 [feature-flag 矩陣](#feature-flag-矩陣))。關掉時 producer 不 inject → consumer extract 不到 → 各 service 從自己的 traceID 自生 `rv` → 斷言 `len(harness.DistinctRVs(run)) > 1`(rv **不再一致**)。這正是 `TestMongoSamplingSuite` 的 propagation-off 反證分支。

---

## 同步 vs 非同步匯出(斷言計時)

`harness.BuildTracerProvider` 用 **`WithSyncer`** —— 你開的 application span **一 End 就匯出**,`WaitFor` 幾乎立刻拿到。

但 **library 內部自帶的 TracerProvider**(例如代表 broker 節點的 deliver-span TP)常用 **batch**,或只在 **關閉 / flush** 時才送出。對這類 span 斷言時:

- 把 `sink.WaitFor(timeout, pred)` 的 `timeout` 設**夠長**(吸收 batch 排程延遲,通常數秒);**或**
- 主動觸發 library 的 flush(關閉連線 / client 等),讓它把 batch 送完再斷言。

> 範例:otel-mongo 的 deliver span 走 batch(預設約 5s 排程)或 `client.Disconnect` 時 flush ——
> `TestMongoFullSpanShape` 用較長的 `WaitFor` 等它,`TestMongoDeliverSpanReachesCollector` 則先 `Disconnect` 再斷言。
> 你的 application span 不受影響(同步即到)。

---

## 測同一 library 的不同傳遞方法

「傳遞方法」是參數而非型別:同一個 library 的每種收送方式寫成一個獨立流程 / 子測試,套**同一組 helper**。
不同方法常會帶出不同拓樸,而 harness 不需任何改動:

| library | 傳遞方法 | 拓樸(由消費端怎麼接決定) |
|---|---|---|
| MongoDB | `InsertOne`→`FindOne`(`SingleResult.TraceContext`) | parent-child(同一 trace 續接) |
| MongoDB | `InsertOne`→`Aggregate`(`Cursor.DecodeWithContext`) | **span-link**(新 trace,link 到 origin) |
| MongoDB | 模擬 change-stream consumer(讀回後手動開 root + link) | **span-link**(新 trace id) |
| HTTP | 不同 endpoint / method | parent-child |

> 注意:`Cursor.DecodeWithContext` 內部會 detach 當前 span、用 origin 開一個 **link**([traced/cursor.go](../otel-mongo/v2/internal/traced/cursor.go)),
> 所以 `Aggregate` 路徑是 span-link 而非 parent-child;`SingleResult.TraceContext` 才是 parent-child 續接。
> 「change-stream」那列目前以 `FindOne` 讀回後**手動開 linked root** 模擬(尚未直接驅動 `Watch`)。

mongo 測試檔即示範這幾種:`TestMongoSamplingSuite`(find,parent-child)用 `AssertAppSpanCounts`+`AssertSameTrace`;
`TestMongoAggregateDelivery`(aggregate)用 `AssertPresence`+`AssertConsistentRV`;`TestMongoSpanLinkConsistency`(link)用 `AssertLinkedTrace`。
`TestMongoTopologyIndependence` 再在 3-node 下跑 `[PC,PC]/[PC,link]/[link,PC]/[link,link]` 四種組合,
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

大多 instrumentation library 有「開關 tracing / propagation」的環境變數。要驗這些 flag,**模式是通用的**:

1. **宣告你的 library 的 gate**:
   ```go
   var gate = harness.GateEnv{
       Global:      "OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", // 共用總開關
       Tracing:     "OTEL_<YOURLIB>_TRACING_ENABLED",           // 你的 library tracing 開關
       Propagation: "OTEL_<YOURLIB>_PROPAGATION_ENABLED",       // 沒有獨立 propagation 開關就留空
   }
   ```
2. **`harness.ExpectationFromEnv(gate)`** 把當前 env 轉成 `{TracingEnabled, PropagationEnabled}`,你的測試據此**分流**該跑哪組斷言。
3. **flag gate 每個 process 只快取一次** → 每個 flag 組合要用**不同 env 各跑一次 `go test`**(別在單一 run 內切換)。

> **把 sampling 測試放在獨立 package**:很多模組的 `tests/integration` 有 `TestMain` 會**強制設好所有 flag**(force-enable)。若你把 flag 矩陣測試放進那個 package,env 會被 `TestMain` 蓋掉、矩陣失效。參考做法:mongo 的 sampling 測試自成一個 `sampling` package(見 [`chain.go`](../otel-mongo/v2/tests/integration/sampling/chain.go) 開頭註解),讓 Makefile/CI 從外部用 env 控制旗標軸。

依 expectation 該驗什麼(通用):

| tracing | propagation | 驗證 |
|---|---|---|
| 開 | 開 | `AssertConsistentRV` + `AssertPresence` / `AssertAppSpanCounts`(取樣一致性核心) |
| 開 | 關 | `DistinctRVs(run) > 1`:rv **不再一致**(載體沒帶 rv,各 service 用自身 rv) |
| 關 | — | `AssertNoWrapperSpans(spans, scope)`:**無 library-emitted span,但 application span 仍在** |

> **disabled-mode invariant**:library 的 tracing flag 關掉時,wrapper 應改用 noop tracer → **不產生任何 library-emitted span**(無 CLIENT,也無模擬 broker 的 span);但**你自己開的 application span 照樣匯出**(它用的是測試的真 `TracerProvider`,與 library flag 無關)。這就是「關 → `AssertNoWrapperSpans` 通過、app span 仍在」的由來。

> `OTEL_TRACES_SAMPLER_ARG` 同時是 sampler 的機率來源,**也是 library 內部 provider(如 broker/deliver span)的取樣率** —— 與各 service 自己的 node rate 解耦。

**範例**:mongo 的完整矩陣由 `make test-integration-sampling` 跑(`OTEL_EXPORTER_OTLP_ENDPOINT` 與各 service 取樣率由測試在行程內帶入,你只需控制旗標軸 + `OTEL_TRACES_SAMPLER_ARG`):

| Row | GLOBAL | MONGO_TRACING | MONGO_PROP | SAMPLER_ARG | 驗證重點 |
|---|---|---|---|---|---|
| 1 | 1 | 1 | 1 | 1.0 | 全鏈路 + 一致性 + deliver span |
| 2 | 1 | 1 | 1 | 0.5 | 取樣率 + 一致性 |
| 3 | 1 | 1 | 0 | 1.0 | propagation 關:rv 不一致 |
| 4 | 1 | 0 | 1 | 1.0 | mongo tracing 關:無 wrapper span |
| 5 | 0 | 1 | 1 | 1.0 | global 關:全關 |

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

## 失敗怎麼定位

斷言失敗時最先想知道的是「sink 到底收到哪些 span」。掛一行就好:

```go
sink := harness.StartSink(t)
harness.DumpOnFailure(t, sink) // 失敗時自動把 sink 收到的全部 span 印出來
```

`DumpOnFailure` 註冊一個 `t.Cleanup`,只在 `t.Failed()` 時傾印;也可在流程中任意點手動印:
`harness.DumpSpans(t, "after drive", sink.Spans())`。每行格式:

```
svc=svc1 scope=otelmongo name=svc1 trace=3f2a9c01 span=88ab12cd parent=00000000 rv=2a000000000000 th=- links=[3f2a9c01] run=7b3e…
```

照這三步看:

1. **預期的 `service.name` 在不在?** 缺了通常是「被取樣 drop」(對照 rv 與 rate,正常)或「沒連到 collector / sink」(連線問題)。
2. **`rv` 是否每行都一樣?** 不一樣 → propagation 沒把 rv 帶過界(載體沒注入/取出,或 propagation flag 關了)。
3. **app span 的 `run` 有沒有值?** 是 `-` → 你忘了在 `tracer.Start` 帶 `RunAttr`,`SpansByAttr` 就篩不到它。

> library-emitted span 的 `run` 本來就是 `-`(見 [兩種 span](#兩種-span你開的-vs-library-產生的));它們靠 `trace=` 欄位與你的 app span 同 traceID 來歸戶。

---

## 疑難排解

| 症狀 | 處理 |
|---|---|
| 斷言失敗、不知 sink 收到什麼 span | 加 `harness.DumpOnFailure(t, sink)` —— 失敗時自動傾印全部 span(見 [失敗怎麼定位](#失敗怎麼定位)) |
| `collector` 容器拉不下來 / tag 不存在 | 用 `OTEL_COLLECTOR_IMAGE=...` 覆寫成可用版本 |
| Ryuk / reaper 在沙箱中起不來 | `export TESTCONTAINERS_RYUK_DISABLED=true` |
| sink 收不到 span(`WaitFor` 逾時) | 確認容器能連到 host:多半是 `host.testcontainers.internal` / host-gateway 在你的 Docker / Podman 設定下不可用 |
| mongo `ReplicaSetNoPrimary` / server selection timeout | 用 `directConnection=true` 直連 mapped port(見 pull 範例註解);別讓 driver 走 replica-set 探索到容器內部 IP |
| deliver span 匯不出 / `tls: first record does not look like a TLS handshake` | collector 是明文 gRPC;設 `OTEL_EXPORTER_OTLP_INSECURE=true` 讓 wrapper 的 deliver exporter 走 insecure |
| 旗標「開/關」兩種狀態互相干擾 | gate 每 process 快取一次——務必用 env 矩陣**分多次** `go test`,勿在單一 run 內切換 |
| 一致性斷言偶發失敗且 rv 在門檻邊界 | 選 rv 時遠離 `threshold ≈ (1-p)·2^56`(範例的 rv ladder 已刻意挑大邊際) |
| Docker daemon 卡住(volume metadata DB timeout) | 環境問題,非測試問題;清掉 stale 的 dockerd 鎖後重啟 daemon |
