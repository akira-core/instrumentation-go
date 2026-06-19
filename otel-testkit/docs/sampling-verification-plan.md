# Plan: 強化 otel-testkit 黑箱驗證 — 統計、結構/拓樸、wrapper+deliver 全 run 驗證、sampler 便利函式 + doc 修正

> 規劃文件(尚未實作)。記錄與使用者逐輪釐清後確認的方向,供後續實作對照。

## Context

otel-testkit 已是黑箱工具箱。使用者讀 README 後提出多項需求,逐一釐清後確認方向如下。所有改動以**黑箱、不改 instrumentation library** 為前提(deliver-rate 一節已確認不改 library、測現況)。

要解決的點:
1. **沒有「次數」概念**:範例一次只驅動一條 trace,無法做統計型取樣(送 N 條隨機 rv、斷言比例 ≈ rate)。次數在 head 指定一次。
2. **真實-app sampler doc 寫錯(doc-only)**:[README.md:243](../README.md) 漏了 `WithSingleLinkSeed`。**程式不用改**——harness 刻意 sampler-agnostic(`BuildTracerProvider` 收任意 sampler);範例本來就包對。少了它:span-link consumer 會自生 rv(`ProbabilitySampler` 無視 links,`otel-sampler/otelsampler/sampler.go:31`),且 root 不會寫 `ot=rv:`(`otel-sampler/otelsampler/link_seed.go:40`)。
3. **斷言只看值不看結構**:現有 `otel-testkit/harness/assert.go` 只有 `AssertConsistentRV` + `AssertPresence`,不算數量、不驗 PC 同 traceID、不驗 span-link 的 `Link` 指向。
4. **多-node 混合拓樸**:需在 3-node 下驗 `[link,link]`、`[link,PC]` 等組合,最後 node 的 rv == 第一 node。
5. **wrapper + deliver span 也要驗**:`InsertOne` 產生 CLIENT span(producer)+ DELIVER span(`mongodb://`,SpanKindConsumer,child of client,`otel-mongo/v2/internal/traced/collection.go:58-82`);文件 `_oteltrace` 帶的是 **deliver span 的 context**。需驗 producer/consumer/deliver **randomness 全相同**,並做**角色計數**。
   - **現況**(已確認不改):client/app span 用該 node 的 rate(`WithTracerProvider` 傳入);**所有 deliver span 用全域 `OTEL_TRACES_SAMPLER_ARG`**(deliver TP 寫死 `ProbabilitySamplerFromEnv(1.0)`:`otel-mongo/v2/client.go:255`、`otel-nats/otelnats/conn.go:226`、mongo v1 同)。
   - **`RunAttr` 是測試碼加的**(`tracer.Start` 時 `WithAttributes`),不改 library。只有自己開的 app span 帶它;wrapper/deliver span 不帶 → 用來分組 app span,並作為 `SpansOfRun` 的錨。

## 1. harness 新增 — `otel-testkit/harness/`

**sampler 便利函式(Q2 根治 footgun)→ 新 `sampler.go`**(轉呼叫 otelsampler):
- `ConsistentSampler(rate float64) sdktrace.Sampler` = `WithSingleLinkSeed(ProbabilitySampler(rate))`。
- `ConsistentSamplerFromEnv(def float64) sdktrace.Sampler` = `WithSingleLinkSeed(ProbabilitySamplerFromEnv(def))`。

**`assert.go` 新增**(沿用 `SpansByAttr`/`SpansByService`/`ExpectedSampled`/`Span.RV()`/`Span.Links`):
- 分組/切片:`SpansByScope(spans, scope)`、`SpansByServicePrefix(spans, prefix)`、
  `SpansOfRun(all []Span, runID string) []Span` —— 以 `RunAttr` 的 app span 為錨收集其 traceID,再沿 `Links` 遞移展開,回傳整條 run 的**所有** span(app + wrapper client + deliver + consumer)。
- 統計(Q1):`SampledFraction(runs [][]Span, service string) float64`;`AssertSampledFraction(t, runs, service, rate, delta)`。
- 數量(Q3/Q5):`AssertAppSpanCounts(t, spans, want map[string]float64, rv uint64, countIfSampled int)`(present⇔`ExpectedSampled`,精確計數)。
- 連結(Q3):`AssertSameTrace(t, spans)`(全同一 TraceID,PC 段用);`AssertLinkedTrace(t, spans, fromService, toService)`(`to` 有 `Link.TraceID == from.TraceID` 且兩者 TraceID 不同)。

## 2. mongo 測試 — `otel-mongo/v2/tests/integration/sampling/{chain.go,sampling_test.go}`

- `buildServices` 改用 `harness.ConsistentSampler(rate)`。把 `driveFindChain` 一般化為 `driveChain(t, sink, svcs, rates, hops []topo, rv)`,`type topo int{parentChild, spanLink}`;consumer 依 `hops[i]` 選 `Start(remoteCtx,…)`(PC)或 `Start(ctx.Background(),…,WithLinks(SpanContextFromContext(remoteCtx)))`(link)。`driveFindChain` = `driveChain(..., allPC)`。
- **預設分支**:加 `AssertAppSpanCounts(run, want, rv, 1)`,`countSampled>0`(全 PC)時 `AssertSameTrace(run)`。
- **`TestMongoAggregateDelivery`**:加 `AssertSameTrace(run)`。
- **`TestMongoSpanLinkConsistency`**:加 `AssertLinkedTrace(run, "svc0", "svc1")`。
- **`TestMongoTopologyIndependence`(第 4 點)**:`PropagationEnabled` 才跑;rates `[0.9,0.5,0.1]` + rv ladder;四組拓樸 `[PC,PC]/[PC,link]/[link,PC]/[link,link]`。每組 `AssertAppSpanCounts` + `AssertConsistentRV(run)`(最後 node rv == head 種下 rv);跨四組斷言 present-set 與 rv 完全一致(rv 穿過被 drop 的中間 node)。
- **`TestMongoSamplingRate`(Q1 統計)**:`PropagationEnabled` 才跑;`rate := harness.EnvSamplerArg(0.5)`;三同 rate service;head loop M(例 30)次隨機 `RandomRV()` 驅動 `driveChain([PC,PC])`,蒐集 `[][]Span`;非空 run `AssertConsistentRV`+`AssertAppSpanCounts(...,1)`;最後 `AssertSampledFraction(runs, "svc0", rate, 0.2)`。由 row1/row2 涵蓋。
- **`TestMongoFullSpanShape`(第 5 點,核心新增)**:`PropagationEnabled` 才跑。producer/consumer **不同 rate**:`rp=0.9, rc=0.1`;deliver rate = `harness.EnvSamplerArg(...)`(全域);選 `rv = 3<<54`(≈0.75·2^56,遠離各門檻邊際)。驅動 insert→find(2-node PC)後:
  - `full := harness.SpansOfRun(sink.Spans(), runID)`;`harness.AssertConsistentRV(full)` —— **producer client + deliver + 仍出現的 span 的 rv 全相同**。
  - 角色計數(用 `SpansByAttr`/`SpansByService`/`SpansByScope(otelmongo.ScopeName)`/`SpansByServicePrefix("mongodb")` 組合):
    - producer node(svc0,rp=0.9):app ×1、wrapper client ×1(present,因 `ExpectedSampled(rp,rv)`);
    - consumer node(svc1,rc=0.1):app/client ×0(`!ExpectedSampled(rc,rv)`);
    - deliver span(全域 rate):數量 = `ExpectedSampled(EnvSamplerArg, rv) ? 2 : 0`(insert+find 各一,與 node rate **脫鉤**——正是要驗的現況)。
  - 註解清楚標明「deliver span 用全域 `OTEL_TRACES_SAMPLER_ARG`,與 node rate 解耦」。

## 3. HTTP 範例 — `otel-testkit/examples/httpdirect/http_test.go`

- `startChain` 改用 `harness.ConsistentSampler(rate)`;`TestHTTP*` 在 sampled>0 時加 `AssertSameTrace(run)` + `AssertAppSpanCounts(run, want, rv, 1)`。

## 4. README — `otel-testkit/README.md`

- **Q2 修正**:真實-app sampler 改 `harness.ConsistentSamplerFromEnv(...)` 並說明兩原因 + 「harness 不強制、由你的服務 sampler 決定」;3 步小節點明預設用 `ConsistentSampler`。
- **`RunAttr` 分組**:① 一次測試驅動多 run + sink 累積所有 span,須先 `SpansByAttr(spans, RunAttr, runID)` 篩單一 run;② 不能用 traceID(span-link 換 traceID);③ **只有你開的 app span 帶 RunAttr**,wrapper/deliver 不帶,故 `InsertOne` 多出的 client/deliver span 不會混進 app 計數;**要連 wrapper/deliver 一起看時用 `SpansOfRun`**(沿 traceID+link 收齊整條 run)。
- **斷言兩類**:拓樸無關核心(`AssertConsistentRV`/`AssertPresence`/`AssertAppSpanCounts`/`SampledFraction`,只需 rates)vs 選用結構性(`AssertSameTrace`/`AssertLinkedTrace`,需知 PC/link)。不在乎圖形狀就不傳拓樸。
- **wrapper + deliver 驗證 + deliver-rate 現況**:新增小節說明 `InsertOne` 的 client+deliver span 結構、`SpansOfRun`+`AssertConsistentRV` 驗 producer/consumer/deliver randomness 一致;**明載「deliver span 取樣率 = 全域 `OTEL_TRACES_SAMPLER_ARG`,與 node rate 解耦(目前 library 行為)」**。
- **API 一覽**補所有新函式。

## 不需更動

- instrumentation library(mongo v1/v2、nats)產品碼——確認不改,測現況。
- harness 基礎設施與兩個整合連線修正(mongo `directConnection=true`、`OTEL_EXPORTER_OTLP_INSECURE=true`)。

## 驗證

每個 `.go` 變更後兩模組各跑 `go build ./... && go vet ./... && golangci-lint run ./...`(0 issues)。整合(需 Docker):
1. `make test-integration-sampling` —— 五列全綠;`TestMongoTopologyIndependence`(四拓樸 rv/present 一致)、`TestMongoSamplingRate`(比例≈rate)、`TestMongoFullSpanShape`(producer/consumer/deliver rv 一致 + 角色計數,deliver 跟全域 rate)在 enabled 列通過,disabled 列維持 skip/停用斷言。
2. `make test-integration-http-direct` —— `TestHTTP*` 含結構斷言通過。
3. 抽驗:暫時拿掉某 service 的 `WithSingleLinkSeed`,確認 `[link,*]` 的 `AssertConsistentRV` 因 rv 不一致/缺 rv 失敗(佐證 Q2),驗後復原。
