# Tracing feature flags(繁體中文)

> English version: [feature-flags.md](feature-flags.md)

每一個 instrumentation 模組都可以被關掉。本文件是這個決策的**唯一參考**:每個開關做什麼、由誰擁有、以及
process 執行期間什麼能改、什麼不能改。

適用於 `otel-mongo` 0.9.0、`otel-mongo/v2` 2.9.0、`otel-nats` 0.8.0、`otel-gorilla-ws` 0.8.0 以後的版本。
設計記錄:`openspec/changes/openfeature-dynamic-flags/design.md`。

## 一段話講完這個模型

Instrumentation 由**部署**啟用,由**維運者**撤銷。透過 [OpenFeature] 連上的 [GO Feature Flag] relay proxy
只扮演**緊急煞車**:一個設成 `false` 的 flag 會在 provider 觀察到的當下把執行中的模組關掉,而 relay 上
**沒有任何東西能把任何東西打開**。所有開著的東西,都是某次有人審查過的部署打開的。當 relay 不可達、設定錯誤
或根本不存在時,每個 flag 都讀作「不要干涉」,由環境獨自決定 —— 所以一個從未安裝 provider 的應用程式,
行為就跟它的環境變數說的一模一樣。

[GO Feature Flag]: https://gofeatureflag.org
[OpenFeature]: https://openfeature.dev

## 三層

```
tracing = gate1 && OTEL_<MODULE>_TRACING_ENABLED && relay verdict
```

| 層 | 擁有者 | 關掉時 | 不重新部署能改嗎? |
|---|---|---|---|
| **`gate1`** — `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` **或** `WithTracingEnabled`,不可兩者皆設 | 部署者,或建構 wrapper 的呼叫端 | process 內每個模組都關,只配置 passthrough 實作,任何 OpenFeature 程式路徑都不可達 | 否 |
| **`OTEL_<MODULE>_TRACING_ENABLED`** | 部署者 | 該模組關閉,只配置它的 passthrough 實作,它的 relay flag 從不被評估 | 否 |
| **relay flag `otel-<module>-tracing`** | 維運者 | 該模組在執行中的 process 上,從下一次操作起停止產出 | **可以 —— 只有這一層可以** |

前兩層都由環境推導、都在建構時固定。它們只差在作用範圍(整個 process vs 單一模組)與擁有者。第三層是唯一
動態的,而且**只能從前兩層允許的範圍往下扣**。

## 解析 `gate1`

`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` 與建構選項 `WithTracingEnabled(v bool)` 是同一個開關的**兩種
拼法**。**只能設一個。** 兩個都給是設定錯誤,由建構子回報,**即使兩者一致也一樣** —— 規則是「設一個」,
不是「設成一樣」。

| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `WithTracingEnabled` | `gate1` |
|---|---|---|
| 未設 | 未傳 | 停用 |
| 未設 | `true` | 啟用 |
| 未設 | `false` | 停用 |
| 已設,truthy | 未傳 | 啟用 |
| 已設,falsy | 未傳 | 停用 |
| **已設,任何值** | **已傳,任何值** | **建構錯誤** |

錯誤會包裝一個各模組自己的 sentinel,可以用 `errors.Is` 比對,並且會列出兩邊觀察到的值:

```go
conn, err := otelnats.ConnectWithOptions(url, nil, otelnats.WithTracingEnabled(true))
if errors.Is(err, otelnats.ErrTracingConfigConflict) {
    // OTEL_INSTRUMENTATION_GO_TRACING_ENABLED 也設了 —— 拿掉其中一個
}
```

`otel-mongo` 對 `OTEL_MONGO_PROPAGATION_ENABLED` 與 `WithTracePropagationEnabled` 套用同一條規則,有自己的
sentinel。同時違反兩條規則的呼叫,會得到一個 `errors.Join` 過的錯誤,兩個 sentinel 都比對得到 —— 這樣你
不會修好一個之後,下一次執行才發現另一個。

## 有效的 tracing

| `gate1` | `OTEL_<MODULE>_TRACING_ENABLED` | relay `otel-<module>-tracing` | Tracing | 有問 relay 嗎? |
|---|---|---|---|---|
| 停用 | 任何值 | 任何值 | **關** | 否 |
| 啟用 | 未設或 falsy | 任何值 | **關** | 否 |
| 啟用 | truthy | `false` | **關** | 是 |
| 啟用 | truthy | `true` | **開** | 是 |
| 啟用 | truthy | 沒有意見 | **開** | 是 |

從這張表掉出兩個性質,值得單獨說:

- **relay 無法啟用任何東西。** 第 2 列不管 relay 送什麼都成立。要把一個模組交給 relay 控制,你必須用開著的
  環境開關部署它,然後用 relay 把它按住。
- **在環境中關掉的模組不花任何成本。** 第 1、2 列只配置 passthrough 實作,而且從不評估 flag,零成本路徑
  完整保留。

## 「沒有意見」涵蓋什麼

relay verdict 的 evaluation default 是 `true`,而 OpenFeature 在**所有**失敗路徑都回傳這個 default。
所以以下每一種情況都代表*不要干涉*:

- 沒有安裝任何 OpenFeature provider
- 有安裝 provider 但還沒 ready
- relay 設定裡沒有這個 key
- 評估發生錯誤
- flag 存在但不是 boolean
- relay 不可達且 provider 沒有快取設定

這些跟 relay 明確送 `true` 無法區分,也沒有理由區分:兩者都表示由環境決定。

## 真值判定

只有設成 `1`、`true`、`yes`、`on`(先轉小寫、去頭尾空白)才算啟用。**其他一律停用**,包含未設與空字串。

| 值 | 結果 |
|---|---|
| 未設 | 停用 |
| `1` / `true` / `yes` / `on` | 啟用 |
| `TRUE` / `On` / `  yes  ` | 啟用 |
| `0` / `false` / `no` / `off` | 停用 |
| `` (已設但為空,`export VAR=`) | **停用** |
| `enabled` / `2` / `y` / `t` | **停用** |

最後兩列是最會踩到的。`export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=` 不會打開閘門,`=enabled` 也不會。
**設了但無法辨識的值會發出一則 warning**,指名變數、觀察到的值與接受的值,所以這不會無聲失敗。如果某個
開關的行為不如預期,先比對這四個字,再看別的地方。

## Flag keys

每個 relay flag 配一個環境變數。這個配對是給維運者的**慣例** —— 環境變數是另一個獨立的、需同時成立的層級,
**不是** flag 的 evaluation default。

| Relay flag key | 配對的環境變數 | 模組 |
|---|---|---|
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `otel-mongo`、`otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `otel-mongo`、`otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `otel-gorilla-ws` |

**`gate1` 沒有 relay key**,因此也沒有任何單一開關能從 relay 靜音整個 process。要停掉每一個模組,就得逐一
撤銷這四個 flag。

`otel-mongo` v1 與 v2 共用兩個 key:撤銷 `otel-mongo-tracing` 會同時停掉兩者。

另外三個變數設定的是「怎麼連上 relay」而不是任何模組的行為,而且沒有 relay 對應項:

| 變數 | 用途 |
|---|---|
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | relay proxy URL;未設 ⇒ 不安裝任何 provider |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | relay API key |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | 輪詢間隔,Go duration 字串,預設 `60s` |

四個模組都透過**單一** OpenFeature domain `otel-instrumentation-go` 解析。一個 provider 服務全部;沒有
per-module provider。

## otel-mongo:`_oteltrace` 文件傳播

Mongo 多一個開關,位在 tracing 之下一層,控制是否把 `_oteltrace` 子文件寫進**你的文件**。

| 有效的 tracing | `gateProp`(`OTEL_MONGO_PROPAGATION_ENABLED` 或 `WithTracePropagationEnabled`) | relay `otel-mongo-propagation` | `_oteltrace` 寫入與讀取 |
|---|---|---|---|
| **關** | 任何值 | 任何值 | **否** |
| 開 | 停用 | 任何值 | **否** |
| 開 | 啟用 | `false` | **否** |
| 開 | 啟用 | `true` 或沒有意見 | **是** |

這個開關要比其他幾個看得更仔細,因為它是唯一會改變**持久化內容**的:

- 每份文件約 90 bytes BSON,有 `tracestate` 時更多。由 `InsertOne`、`InsertMany`、`UpdateOne`、
  `UpdateMany`、`UpdateByID`、`ReplaceOne`、`BulkWrite` 寫入。
- **沒有任何東西會移除它。** 模組讀 `_oteltrace` 來還原 trace context,但**從不**在解碼時把它剝掉,所以
  一旦寫入,你的應用程式在之後每次讀取都會看到它。
- **關掉它不會還原任何東西。** 新的寫入不再帶這個欄位;已經有的文件仍然有。清理是應用程式端的 `$unset`
  migration。
- 使用 `$jsonSchema` 驗證且 `additionalProperties: false` 的 collection,在這個開關開著時會**拒絕每一次
  寫入**。

正因如此,只有部署能打開它。跟 tracing 一樣,relay 只能撤銷。

## otel-gorilla-ws:三個不同的布林值

WebSocket 模組有三個名字相近、生命週期不同的值。只有最後一個是動態的。

| 名稱 | 如何解析 | 決定什麼 | 對連線固定嗎? |
|---|---|---|---|
| **capability** | `gate1 && OTEL_GORILLA_WS_TRACING_ENABLED` | 是否提出(`Dial`)或確認(`Upgrade`)`otel-ws` subprotocol,以及是否建立真的 tracer | 是 —— 在 handshake 之前解析 |
| **negotiation outcome** | handshake 結果,或 `NewConn` 時原始連線的 subprotocol 證明了 `otel-ws` | **對端**是否對每一幀包 envelope | 是 —— handshake 無法重來 |
| **span gate** | capability `&&` relay verdict | 是否建立 span、是否 inject/extract trace context | **否 —— 每次讀寫都重新讀** |

capability **只箝制寫入端的決定**。對端是否包 envelope 是 handshake 的**事實**,不是本端閘門有權管的事,
所以 capability 關掉的 wrapper 在包裝一條已協商的連線時會寫**原始幀**(安全 —— 對端的探測會退回 payload),
但**讀取時仍然解包**。把讀取路徑綁在 capability 上,會把原始的 `{"header":…,"data":…}` bytes 交給你的
應用程式。

| capability | negotiated | span gate | 送出的 wire | 收到的 wire | Spans | Trace 傳播 |
|---|---|---|---|---|---|---|
| false | false | 不評估 | 原始 | 原始 | 無 | 無 |
| false | true | 不評估 | 原始 | **解包** | 無 | 無 |
| true | false | false | 原始 | 原始 | 無 | 無 |
| true | false | true | 原始 | 原始 | 只有本地 | 無 —— 沒有載體 |
| true | true | false | envelope,空 header | 解包 | 無 | 無 |
| true | true | true | envelope 帶 `traceparent` | 解包 | 有 | 有 |

第 5 列是「讓傳播保持可能」的代價:已協商 `otel-ws` 的連線在撤銷後仍然繼續寫 envelope,因為它的對端把每一幀
都當 envelope 解析。它不帶 trace context,也不建立 span。

**撤銷 `otel-gorilla-ws` 停的是遙測,不是開銷。** 這是四個模組裡唯一撤銷後回不到零成本路徑的:每次寫入
仍然要序列化 envelope,每次讀取仍然要跑探測。要移除這個 wire 開銷,必須把 `OTEL_GORILLA_WS_TRACING_ENABLED`
關掉並重新部署。改成撤銷時直接不包 envelope 會讓 wire 與仍在包的對端失去同步,而且會無聲拆解任何長得像
envelope 的應用層 payload。

envelope 結構 `{"header":…,"data":…}` 在 `otel-ws` 連線上是**保留結構** —— 見 [otel-ws.md](otel-ws.md)。

## 什麼沒有被閘門管

`otelmongo.ContextFromDocument` 與 `otelmongo.ContextFromRawDocument` **完全沒有 flag 閘門**。它們從你
已經拿在手上的文件裡讀出 `_oteltrace` 欄位,回傳它編碼的 span context。它們不開 span、不配置 attribute、
不初始化 OTel SDK 的任何部分、不寫入任何地方 —— 而且你只有在想做 trace 抽取時才會呼叫它們,所以沒有任何
東西需要 kill switch 來保護你。

`Cursor.DecodeAndTrace` 與 `ChangeStream.DecodeAndTrace` 看起來很像,但**有**閘門:它們每次呼叫都會開始並
結束一個 `mongo.cursor.decode` span,所以會產出遙測,屬於開關管轄。

**因此撤銷不會停止 trace context 的抽取。** 這點值得明說,否則「要立刻停掉一個模組,把它的 relay flag 設成
`false`」會讀起來像是全部都停了:

| | 撤銷之後 |
|---|---|
| `Collection.InsertOne` 等 | 無 span,不寫 `_oteltrace` |
| `Cursor.DecodeAndTrace` | 無 span,而且**不抽取** —— 原樣回傳 `ctx` |
| `ContextFromDocument` / `ContextFromRawDocument` | **仍然抽取**,與之前完全相同 |

`DecodeAndTrace` 上的閘門管的是它送出的 span,不是連結本身。如果你要讓連結在 library 被靜音後仍然存活,
就呼叫 `Decode` 然後 `ContextFromDocument` —— 這是**受支援的做法**,不是漏洞。

## 連上 relay

**設一個環境變數。沒有程式碼要寫。**

```sh
OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT=http://relay:1031
```

只要設了它、而且你沒有安裝自己的 OpenFeature provider,第一次 instrumented operation 就會建立一個
GO Feature Flag provider、把它綁到 OpenFeature domain `otel-instrumentation-go`,之後每個模組都透過它解析。
另外兩個變數用來調整:

| 變數 | 意義 | 預設 |
|---|---|---|
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | Relay proxy URL。**未設 ⇒ 什麼都不安裝**,不寫入任何 OpenFeature 狀態 | 未設 |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | API key(relay 有啟用認證時)。永不寫入 log | 空 |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | provider 多久輪詢一次 relay。**只吃 Go duration 字串** —— `60` 會被拒絕,`60s` 不會 | `60s` |

格式錯誤的輪詢間隔會發出 warning 並退回 `60s`;它**不會**中止安裝,因為一個選用調校值上的錯字,不該無聲
刪掉你的 kill switch。

有兩個設定是寫死的、刻意不開放,因為任何一個弄錯都會把 relay 中斷變成應用程式停頓:`DataCollectorDisabled:
true` 與 in-process evaluation。兩者在下面說明。

### 改成自己安裝 provider

當你已經安裝了 provider,library 會完全讓路 —— 觸發條件要求 `otel-instrumentation-go` 這個 domain 上沒有
綁定 provider,而且也沒有安裝 default provider。以下情況適合自己裝:你需要生命週期控制、需要跟自己的業務
flag 共用一個 provider、或需要阻塞式安裝:

```go
import (
    gofeatureflag "github.com/open-feature/go-sdk-contrib/providers/go-feature-flag/pkg"
    "github.com/open-feature/go-sdk/openfeature"
)

provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
    Endpoint:              "http://relay:1031",
    DataCollectorDisabled: true,   // 必要 —— 見下
})
if err != nil {
    return err
}
if err := openfeature.SetProviderAndWait(provider); err != nil {
    // 記錄後繼續。不要讓啟動失敗 —— 見下。
    logger.Error("feature flag provider unavailable; continuing without relay control", "error", err)
}
```

在啟動時安裝,放在 `otelsetup.Init()` 旁邊,而且要在**建構任何 wrapper 之前**。不要同時設
`OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`:它會被忽略,無害但會誤導。

無論走哪一條路,library 都不碰 **default** provider、全域 evaluation context、hooks 或 `Shutdown`。
它做的任何事都無法改變你自己的 feature flag 怎麼解析。

### 關掉 data collector

零程式碼路徑已經寫死這一項。它在你自己安裝 provider 時才有意義,此時
`DataCollectorDisabled: true` 是**必要的**,不是調校。

provider 的 data collector 預設是開的。它每次評估都往記憶體緩衝區追加一筆事件,並用兩分鐘的 ticker 沖出到
relay。有兩個細節讓它對這個 library 的使用模式(每次 instrumented operation 一次評估)變得危險:

- **失敗的**沖出**不會**清空緩衝區。
- 一旦緩衝區到達上限(預設 100,000 筆),**之後每一次 `AddEvent` 都會在評估的 goroutine 上、持有緩衝區
  mutex 的情況下同步沖出。**

relay 掛掉時,那個同步沖出會在 HTTP client 的 timeout(預設 10 秒)後失敗,而緩衝區永遠不會被排空,所以
下一次評估又會發生一次,同時其他所有評估中的 goroutine 都排在同一個 mutex 後面。relay 中斷因此會拖住你
應用程式自己的 Mongo 查詢與 NATS 發布 —— 正是這整個設計要防止的事。

關掉它不會失去什麼。collector 把 flag 評估的分析資料回報到 relay 的 dashboard;在 process 級 flag、每次
operation 評估一次的情況下,那份分析等於是你流量的一份副本。

### relay 不是啟動相依

如果 process 啟動時 relay 不可達,provider 的第一次抓取會失敗。零程式碼路徑會記錄後繼續;若你自己安裝
provider,請**記錄後繼續**而不是回傳錯誤。中止啟動會讓 relay 變成你服務的硬相依,這跟煞車的用意完全相反。

繼續下去只有一個代價,而且無法避免:在 relay 掛掉期間啟動的 process 無法得知有一個生效中的撤銷,所以它會
以環境宣告的狀態上線。讀不到的撤銷就是讀不到。

一旦 provider 裝好而且成功抓取過,之後的 relay 中斷不會改變任何事:in-process 評估器會繼續服務它最後一次
成功抓取的設定,所以生效中的撤銷可以撐過中斷。只有評估錯誤才會退回「允許」—— 而一個手上握有設定的
in-process provider 不會發生評估錯誤。

### 啟動窗口,以及如何關掉它

一個無法解析的 flag 代表「允許」,所以還沒抓到設定的 provider **無法撤銷任何東西**。零程式碼安裝刻意採用
非阻塞 —— 煞車不該變成延遲來源,而阻塞會把一次 relay 往返擺在你第一個 Mongo 查詢前面 —— 所以從安裝到
provider 第一次抓取之間,每個 flag 都讀作啟用。

會讓這件事變重要的情境很普通:維運者為了止血撤銷了一個模組,而 process 因為無關的原因重啟。它會以
instrumented 狀態回來,直到 provider 追上。對 `otel-mongo` 而言,那個窗口不只是 span:在窗口內寫進去的
`_oteltrace` 是**永久的**,清理要靠 `$unset` migration。

**要關掉它,就在建構任何 wrapper 之前用 `openfeature.SetProviderAndWait` 自己安裝 provider。** 這同時做到
阻塞等待 ready、以及讓自動安裝讓路。這個窗口能不能接受是**你的**決定,不是 library 的 —— 所以它寫在這裡,
而不是被強制。

### 只支援 in-process evaluation

provider 在背景輪詢 relay,每次 flag 查詢都是本地的。零程式碼路徑把 `EvaluationType` 寫死為 `INPROCESS`;
若你自己安裝 provider,請用這個預設值。**不支援 remote evaluation**:它會把每次評估變成一個 HTTP request,
等於把網路 I/O 放到 Mongo 查詢或 NATS 發布的路徑上。

### 針對單一服務而非整個機隊

relay flag 會套用到每一個解析它的 process,所以除非規則分得出服務,`otel-mongo-tracing: false` 會停掉你
**整個機隊**的 tracing。

設定 **`OTEL_SERVICE_NAME`** —— 你的 exporter 本來就會讀的 OpenTelemetry 規格變數 —— 零程式碼路徑就會在
每次評估時把它當成 `service.name` 屬性送出:

```yaml
otel-mongo-tracing:
  variations: { enabled: true, disabled: false }
  targeting:
    - query: service.name eq "checkout-api"
      variation: disabled     # 只停這個服務
  defaultRule:
    variation: enabled
```

兩個限制。這個屬性**只在**零程式碼路徑供給:如果你自己安裝 provider,evaluation context 就歸你所有,由你用
`openfeature.SetEvaluationContext` 自己設。另外它是 process 級的,所以 targeting 可以依 process 級屬性
(service、environment、host)判斷,但**不能**依 per-request 屬性。

## 撤銷延遲

撤銷**不是瞬間的**,而且延遲不在這個 library。

模組在每次 instrumented operation 解析 verdict、不做任何快取,所以它們不加任何延遲。你等的是 **provider 的
輪詢間隔**:relay 在背景被輪詢,flag 的變更在下一次輪詢落地之前是看不見的。

| | 延遲 |
|---|---|
| 模組自身的解析 | 無 —— 每次操作都讀當下的值 |
| Provider 輪詢間隔(零程式碼路徑) | **最多 60 秒**,可用 `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` 調整 |
| Provider 輪詢間隔(自己安裝的 provider) | 由你設定;GO Feature Flag 的預設是 **120 秒** |

規劃事故應變時要以輪詢間隔為準,而不是以「立即」為準。如果 60 秒對你的風險承受度太慢,就調低它 —— 那個輪詢
是帶 ETag 的 conditional `GET`,沒有變更時回 304,所以縮短它很便宜。

## 解析的成本

relay verdict 在**每一次 instrumented operation** 解析;不做任何快取。以 in-memory provider 量測,一次評估
大約是 **2 µs 與 7 次配置**。那不是 flag 查詢本身 —— 而是它外圍的 OpenFeature SDK 評估流水線(hook 鏈、
evaluation context 合併、provider registry 的鎖),而且不會因為 provider 把設定放在記憶體就變便宜。

有兩件事限制了這個成本落在哪裡:

- 只有**實際在做 instrumentation** 的 wrapper 才會付。在環境中關掉的模組只配置 passthrough 實作,永遠不評估
  任何東西。
- 對一次 Mongo 往返而言它是雜訊。對一次 NATS 發布(本來就要付 1–3 µs 建立 span)而言,它大約讓
  instrumentation 的額外開銷加倍。

快取躲在一個不變的內部簽章後面,所以若真實工作負載的 benchmark 顯示它有影響,可以在不影響任何 API 的情況下
加回來。這是**刻意延後**而非排除;理由記錄在設計文件裡。

## Per-connection 選項

`WithTracingEnabled(v bool)` 為單一連線或 client 供給 `gate1`,給那些無法設定 process 環境變數的呼叫端 ——
測試,或同一個 binary 裡數個獨立設定的 client。接受它的有 `otelnats.ConnectWithOptions` 及其 TLS 與
credentials 變體、`otelmongo.ConnectWithOptions` 與 `NewClient`(v1 與 v2)、以及
`otelgorillaws.NewConn` / `Dial` / `Upgrader.Upgrade`。

它只供給一層,沒有別的。帶著它的連線仍然會在建構時讀它的模組環境變數、在每次操作解析 relay verdict,而且
relay 撤銷時仍然會停。**沒有任何方法能讓一條連線豁免於撤銷。**

它是 `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` 的另一種**拼法**,不是覆寫:兩個都給是設定錯誤,由建構子
回報。`otel-mongo` 對 `WithTracePropagationEnabled` 與 `OTEL_MONGO_PROPAGATION_ENABLED` 套用同一條規則。
見 *解析 `gate1`*。

## 維運速查

- **要讓一個模組可撤銷:** 用開著的 `gate1` 與模組開關部署,設定 `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`,
  並在 relay 上建立它的 flag。不需要應用程式碼。
- **要立刻停掉一個模組:** 把它的 relay flag 設成 `false`。它在下一次輪詢生效 —— 預設**最多 60 秒**,
  不是立即。見*撤銷延遲*。
- **要只停一個服務而不是整個機隊:** 設定 `OTEL_SERVICE_NAME`,並在 relay 規則裡針對 `service.name`。
- **要立刻停掉全部:** 撤銷四個 relay flag。沒有單一 key。
- **要永久停掉一個模組:** 改它的環境變數並重新部署。
- **要為了調查事故把 tracing 打開:** relay 做不到。改環境並重新部署。

撤銷**不會**做的兩件事,都很容易被誤以為會:

- 它不會停掉 `otelmongo.ContextFromDocument` / `ContextFromRawDocument`。那兩個沒有閘門 ——
  見*什麼沒有被閘門管*。
- 它不會移除 `otel-gorilla-ws` 在已協商連線上每則訊息的 envelope 開銷。那需要重新部署 ——
  見 *otel-gorilla-ws:三個不同的布林值*。
