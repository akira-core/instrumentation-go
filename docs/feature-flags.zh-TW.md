# Tracing feature flags(繁體中文)

> English version: [feature-flags.md](feature-flags.md) ·
> 實作教學:[otel-nats-kill-switch.zh-TW.html](otel-nats-kill-switch.zh-TW.html) /
> [otel-nats-kill-switch.en-US.html](otel-nats-kill-switch.en-US.html)

每一個 instrumentation 模組都可以被打開或關掉。本文件是這個決策的**唯一參考**:每個開關做什麼、由誰擁有、
哪些能在 process 執行期間改變、哪些不能。

適用於 `otel-flags` 0.1.0、`otel-mongo` 0.8.0、`otel-mongo/v2` 2.8.0、`otel-nats` 0.8.0、
`otel-gorilla-ws` 0.8.0 以後的版本。設計記錄:`openspec/changes/openfeature-dynamic-flags/design.md`。

## 一段話講完這個模型

每個開關沿著一道四階梯解析,**最先表態的那一層贏**。透過 [OpenFeature] 連上的 [GO Feature Flag] relay
proxy 站在最上層,而且**兩個方向都有權威**:它能把執行中的模組關掉,也能把部署原本沒開的模組打開。它下面
依序是環境變數、建構選項、寫死的預設值。讓這件事安全的不是對 relay 的限制,而是**預設值**:每個 per-module
開關預設**關閉**,所以什麼都沒設定的 process 不會產生任何 trace;而 process 層級的總開關預設**開啟**,
純粹因為它是「否決權」而不是「啟用鍵」。沒有配置 relay 時,完全不會有任何 OpenFeature 程式碼執行,由環境
變數與選項獨自決定。

[GO Feature Flag]: https://gofeatureflag.org
[OpenFeature]: https://openfeature.dev

## 誰贏

```
relay  >  env  >  option(With*Enabled)  >  寫死的預設值
```

這個順序是**每個來源被決定的時間先後**:預設值在編譯時、選項在建構 wrapper 時、環境變數在部署時、relay 在
執行時。越晚的階段覆寫越早的階段。這就是一般的設定分層,不需要額外記一條規則。

| 來源 | 擁有者 | 範圍 | 不重新部署就能改? |
|---|---|---|---|
| relay flag | 維運者 | 整個艦隊,或單一服務(見[targeting](#只針對單一服務而非整個艦隊)) | **可以 —— 唯一可以的一層** |
| `OTEL_*` 環境變數 | 部署者 | 單一 process | 不行 |
| `With*Enabled` 選項 | 建構 wrapper 的呼叫方 | 單一 connection 或 client | 不行 |
| 寫死的預設值 | 這個函式庫 | 沒人表態的所有地方 | 不行 |

**選項排在它的環境變數之下。** 這是刻意的,也是這個版本唯一一處與 `0.7.0` 相反的地方(當時是選項贏)。
三個理由,依重要性排序:

1. 它給維運者一個**單模組**、程式碼無法覆寫的設定。即使 Go 程式碼傳了 `WithTracingEnabled(true)`,
   `OTEL_MONGO_TRACING_ENABLED=false` 依然能在那次部署關掉該模組——不必噤聲整個 process,也不必架 relay。
2. 它補上唯一一個會**寫入資料**的開關身上的不對稱。否則 `WithTracePropagationEnabled(true)` 能推翻
   `OTEL_MONGO_PROPAGATION_ENABLED=false`,開始把永久的 `_oteltrace` 欄位塞進維運者自己的 document。
   其他開關只是產生或不產生遙測;這一個會留下資料。
3. 階梯在部署時間軸上保持單調,一句話就講得完,而不是在相鄰兩層之間反轉的「具體性」規則。

**代價是什麼。** 選項只在它的環境變數未設定時才會被採用,所以有設該變數的 process 無法用選項區分兩條連線。
那是選項唯一能獨力表達的事——只追蹤兩個 Mongo client 其中一個——而它的存續條件是部署不要設那個變數。
在預設為「關」之下,那本來就是常態,不是犧牲。

## 三個開關

| 開關 | Relay key | 選項 | 環境變數 | 預設值 |
|---|---|---|---|---|
| master | `otel-instrumentation-go-tracing` | — | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | `true` |
| 各模組 tracing | `otel-<module>-tracing` | `WithTracingEnabled` | `OTEL_<MODULE>_TRACING_ENABLED` | `false` |
| Mongo propagation | `otel-mongo-propagation` | `WithTracePropagationEnabled` | `OTEL_MONGO_PROPAGATION_ENABLED` | `false` |

三者以 AND 組合:

```
tracing     = master && moduleTracing
propagation = tracing && mongoPropagation
```

**master 是否決權,不是啟用鍵。** 它的預設值 `true` 的意思是「不表示反對」。把它設成 `true`——不論在環境
變數還是 relay 上——完全不會改變任何事。唯一有效果的值是 `false`,它會停掉 process 裡的每一個模組,包含
那些在 Go 程式碼裡傳了選項的連線。**不要**在 relay 上建立 `otel-instrumentation-go-tracing: true` 期待它
打開什麼,那看起來會像一個壞掉的 flag。

**沒有任何東西是因為 master 是 `true` 才打開的。** 讓零設定的 process 保持安靜的,是各模組預設的 `false`。

### 實例

| 設定 | 結果 |
|---|---|
| 什麼都沒設 | 關 |
| 只設 `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true` | 關 —— master 不啟用任何東西 |
| 只設 `OTEL_NATS_TRACING_ENABLED=true` | **NATS 開** —— master 預設為 `true` |
| `WithTracingEnabled(true)`,沒有任何環境變數 | 該連線開 |
| `WithTracingEnabled(true)` + `OTEL_NATS_TRACING_ENABLED=false` | **關** —— 變數壓過選項 |
| `WithTracingEnabled(false)` + `OTEL_NATS_TRACING_ENABLED=true` | **開** —— 同一條規則,反方向 |
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` + 其他全開 | 關 —— 否決權壓過一切 |
| relay `otel-mongo-tracing: true`,`OTEL_MONGO_TRACING_ENABLED` 未設 | **Mongo 開** —— relay 能啟用 |
| relay `otel-mongo-tracing: false`,`OTEL_MONGO_TRACING_ENABLED=true` | 關 —— relay 能停用 |
| `OTEL_MONGO_TRACING_ENABLED=`(空字串) | **建構錯誤** —— 見下 |

## 環境變數是嚴格的三態

一個環境變數只有三種結果:

| 值 | 結果 |
|---|---|
| 未設定 | 沒有意見 —— 往下掉到選項,再往下到預設值 |
| `1` `true` `yes` `on` / `0` `false` `no` `off`(去空白、不分大小寫) | 由這一層決定 |
| 其他任何值,**包含空字串** | 建構子回傳包裹 `otelflags.ErrInvalidFlagValue` 的錯誤 |

**不猜。** 在階梯模型下沒有一個安全的猜測方向:master 那層預設 `true`,其他每一層預設 `false`,所以一個被
靜默讀成 `false` 的值會在某一層停掉整個艦隊、在其他層什麼都不做——同一個輸入代表兩件事,而唯一的證據只有
一行 log。

**`export VAR=` 是無效值,不是 falsy。** 兩種讀法都在某處是錯的:當成 `false`,會讓一個沒展開的
`${SOMETHING}` 模板變數替部署表達了它從沒有過的意見;當成未設定,會讓所有用它當關閉開關的部署靜默反轉。
這條規則沒有例外:**要嘛設成一個認得的值,要嘛就不要設。**

錯誤訊息會寫出變數名與觀察到的值。一個會讀多個開關的建構子會把**所有**壞掉的值合併在一個錯誤裡回報,
所以跑一次就知道全部要修什麼。

## 升級之前

**在部署設定裡 grep `OTEL_*_ENABLED`,確認每個值都在上面兩張清單之一。** 這是本次改動中唯一會讓 process
啟動不起來的變更:`=enabled`、`=2`、`=y` 和 `=`(空)以前會被容忍,現在會在第一個建構子失敗。Kubernetes
manifest 裡沒展開的 `${SOMETHING}` 正好落在空字串那一格。

接著重新理解預設值的意義。相對於 `0.7.0`:

- 只設**模組**變數而沒設全域變數,現在會生效。以前它是無效的,那是常見的「我設了 flag 但什麼都沒發生」
  客訴;修好了,但仍然是行為改變。
- 選項與它配對的環境變數同時出現時,現在是變數贏。
- 兩種情況都不影響 `_oteltrace`:propagation 預設 `false`,需要它自己的明確 `true`。

## Flag keys

| Flag key | 配對的環境變數 | 選項 | 預設值 | 模組 |
|---|---|---|---|---|
| `otel-instrumentation-go-tracing` | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | — | `true` | 全部 |
| `otel-mongo-tracing` | `OTEL_MONGO_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-mongo`、`otel-mongo/v2` |
| `otel-mongo-propagation` | `OTEL_MONGO_PROPAGATION_ENABLED` | `WithTracePropagationEnabled` | `false` | `otel-mongo`、`otel-mongo/v2` |
| `otel-nats-tracing` | `OTEL_NATS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-nats` |
| `otel-gorilla-ws-tracing` | `OTEL_GORILLA_WS_TRACING_ENABLED` | `WithTracingEnabled` | `false` | `otel-gorilla-ws` |

Key 是固定的,執行期無法覆寫。`otel-mongo` 與 `otel-mongo/v2` 共用這兩個 key,所以改一次 relay 兩邊都到。

## otel-mongo:`_oteltrace` document propagation

這是唯一一個「開啟」會留下東西的開關,也是最需要仔細讀的一個。

propagation 開啟時,`InsertOne`、`InsertMany`、`ReplaceOne`、`UpdateOne`、`UpdateMany`、`UpdateByID`
與 `BulkWrite` 會在它們寫入的 document 上附加一個 `_oteltrace` 子文件——大約 90 bytes 的 BSON,
有 `tracestate` 時更多。

- **讀取時永遠不會剝除。** 一旦寫入,該欄位就會在此後每一次讀取那份 document 時對你的應用程式可見,永久。
- **把開關關回去不會回收任何東西。** 新的寫入不再帶它,已存在的 document 仍然帶著。清理是你自己要跑的
  `$unset` migration。
- **對於設了 `$jsonSchema` 且 `additionalProperties: false` 的 collection,寫入會直接失敗。**

### relay 能打開這個

與被取代的「只能撤銷」模型不同,一個 relay flag 能啟動這些寫入。有四道界限,無法接受這個風險的站台可以
使用其中之一:

1. **master 否決權。** `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false` 會停掉 process 裡的一切,不論
   relay 說什麼。
2. **環境變數。** `OTEL_MONGO_PROPAGATION_ENABLED=false` 無法被應用程式碼覆寫——只有 relay 能。這正是
   選項排在它之下的原因。
3. **預設值 `false`。** 所有來源都沉默時,永遠不會啟用它。必須有東西明確說 `true`:一個選項、一個變數,
   或某人建立的 relay flag——而 relay 設定是由你自己的站台撰寫的。
4. **沒有 relay 就搆不到。** 一個既沒設 `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`、也沒有任何 provider
   綁定到 `otelflags.FlagDomain` 的 process,relay 根本觸及不到——見
   [先安裝 provider,再建構 wrapper](#先安裝-provider再建構-wrapper)。你為應用程式自己的旗標安裝的
   provider 不算數。

## otel-gorilla-ws:negotiation 是交握時的既成事實

三個容易混淆、實際上不同的布林值:

| | 意義 | 何時決定 |
|---|---|---|
| effective tracing | 這次呼叫要不要產生 span、要不要 inject/extract? | 每次 `WriteMessage`/`ReadMessage` |
| negotiated(`otel-ws`) | 對端是否把每個 frame 當 envelope 解析? | 交握時,一次 |
| capability | 這條連線上是否可能執行任何 OTel SDK 路徑? | 建構時,一次 |

`Dial` 提供、`Upgrader.Upgrade` 確認 `otel-ws` subprotocol 的條件,是該連線的 effective tracing 值——
master、模組、含 relay——在**交握前的那一刻**為開。交握無法重來,所以會產生一個必須事先規劃的不對稱:

- **啟用只會影響之後才建立的連線。** 一條在該模組關閉期間建立的長連線永遠不會取得 envelope,而且
  `WithTracingEnabled(true)` 也救不回來:沒有協商 `otel-ws` 的對端不會去解析它。這種連線在 flag 打開後
  仍然可以產生**本地** send/receive span,只是無法 inject 或 extract。維運者若需要既有連線被追蹤,
  必須讓它重連。
- **停用會立刻影響每一條連線的 span 與 inject/extract,但不影響 envelope。** 一條已協商 `otel-ws` 的
  連線在停用後仍然會寫 envelope、仍然會跑讀取端的探測,因為對端還在把每個 frame 當 envelope 解析。
  這是四個模組中唯一一個關掉後不會回到零成本路徑的;要移除那份線路成本必須讓連線重連。

`NewConn` 自己沒有交握:只有當原始連線協商出來的 subprotocol 證明是 `otel-ws` 時,它才啟用 envelope。
自行處理交握的呼叫方使用匯出的 `SubprotocolOTelWS` token,並可用 `IsOTelNegotiated(conn)` 驗證。
完整的協商矩陣見 `otel-ws.md`。

## 什麼沒有被 gate

`ContextFromDocument` 與 `ContextFromRawDocument`(`otel-mongo`,v1 與 v2)**完全沒有任何開關**——
不是 master、不是模組變數、不是選項、也不是 relay。

它們不開 span、不建 attribute、不初始化 OTel SDK 的任何部分、不寫入任何地方,也不做任何 OpenFeature
評估。它們從一個你已經持有的值裡讀出一個欄位,回傳它編碼的內容。開關的存在是為了阻止函式庫在你執行業務
操作時**順帶**替你做事;這兩個函式只做你明確呼叫它們去做的那件事。

**因此關掉一個模組並不會停止 trace context 的取出。** 這是刻意的,而且是在函式庫被噤聲時保留 trace 連結
的官方做法:用 `Decode` 加 `ContextFromDocument`,而不是 `DecodeAndTrace`。

`Cursor.DecodeAndTrace` 與 `ChangeStream.DecodeAndTrace` **有** gate,因為它們每次呼叫都會開啟並結束一個
真正的 `mongo.cursor.decode` span。

## 連上 relay

### 零程式碼路徑

設環境變數就好。不用 import、不用寫 Go 程式碼、沒有其他要記的事。

| 變數 | 意義 |
|---|---|
| `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` | relay proxy URL。未設 ⇒ 不安裝任何東西、不寫入任何 OpenFeature 狀態、永遠不做評估 |
| `OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY` | 選用;永遠不會被記進 log |
| `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` | 選用;只接受 Go duration 字串(`30s`、`2m`),預設 `60s`。格式錯誤會警告、退回預設值,並**仍然安裝**。這個值設定的是輪詢週期的中心,不是精確值:每個 process 會抽一次、偏移最多 ±10% |
| `OTEL_SERVICE_NAME` | 選用;提供 `service.name` targeting 屬性,僅限這條路徑 |

函式庫會把 GO Feature Flag provider 以**具名(named)**provider 的身分註冊在 `otel-instrumentation-go`
domain 上,而且只在你的應用程式沒有安裝自己的 provider 時才會這麼做。它寫死了
`DataCollectorDisabled: true` 與 in-process 評估,所以這條路徑不可能被設定成下面描述的那兩種失敗。

不論 binary 連結了幾個模組,**每個 process 只會安裝一個 provider**:共享的 `otel-flags` module 用單一
一把鎖守住那次安裝。

函式庫永遠不碰**預設(default)**provider、全域 evaluation context、hooks 或 shutdown。它做的任何事都不會
改變你自己的 feature flag 如何解析。

### 或者你自己安裝 provider

```go
provider, err := gofeatureflag.NewProvider(gofeatureflag.ProviderOptions{
    Endpoint: "http://relay:1031",

    // 必要。見下面「關閉 data collector」。
    DataCollectorDisabled: true,
})
if err != nil {
    slog.Warn("feature flag provider unavailable; switches are environment-only", "error", err)
} else if err := otelflags.InstallProvider(provider); err != nil {
    // 記 log 後繼續:relay 是控制平面,不是前置條件。
    slog.Warn("feature flag provider registration failed", "error", err)
}
```

`otelflags.InstallProvider` 會綁定**具名** domain `otel-instrumentation-go`(`otelflags.FlagDomain`)、
等待 provider 完成初始化,並記錄「這個 process 刻意給了 instrumentation 開關一個 relay」。呼叫它之後,
零程式碼安裝會自動讓位,provider 的生命週期由你擁有。

provider **不必是 GO Feature Flag** —— 任何 OpenFeature provider 都可以。這就是 embedding SDK 用來完整
掌管初始化、evaluation context、logger 與 shutdown 的接縫;見
[Embedding SDK:自己擁有 provider](#embedding-sdk自己擁有-provider)。

`openfeature.SetNamedProviderAndWait(otelflags.FlagDomain, provider)` 仍然可用,也仍然偵測得到。優先用
`InstallProvider` 只有一個理由:要偵測一個原生綁定,就得問 OpenFeature SDK「哪個 provider 綁在這個 domain
上」,而那個問題沒有精確答案(見下);`InstallProvider` 記下的東西則是精確的。

**永遠不要為了這個目的去綁預設 provider。** 你用 `openfeature.SetProvider` 安裝的 provider —— 你應用程式
自己的旗標 —— 刻意**不會**被當成 instrumentation 開關的 relay:它不會讓 `RelayPossible()` 為真、不會導致
instrumented 實作被建構,也永遠不會有 instrumentation key 拿去對它評估。

### Embedding SDK:自己擁有 provider

如果你發布的 SDK 包住這些模組,而且想自己擁有 OpenFeature 的生命週期、而非繼承零程式碼路徑,就在你自己的
初始化流程裡用 `otelflags.InstallProvider` 安裝任何你想要的 provider,並在建構任何 wrapper 之前完成。
從那一刻起:

- **初始化、logger、輪詢週期與 shutdown 都是你的。** 本 library 什麼都不安裝,也永遠不會對一個不是自己
  安裝的 provider 呼叫 `Shutdown`。
- **evaluation context 是你的。** 本 library 只在零程式碼的自動安裝路徑上附加 `service.name` targeting
  屬性;當 provider 由你擁有時,它傳入的是空的 invocation context,所以你用
  `openfeature.SetEvaluationContext` 設的 API 級全域 context 會原封不動地進入每一次評估。見
  [只針對單一服務而非整個艦隊](#只針對單一服務而非整個艦隊)。
- **預設 provider 兩個方向都不受影響。** 你應用程式自己的旗標繼續由你綁在預設槽位上的東西解析,而那個
  provider 永遠不會被問到任何一個 instrumentation key。

### 先安裝 provider,再建構 wrapper

「relay 是否可能存在」是在**建構 wrapper 時解析一次**的——endpoint 有設,或已經有 provider 綁定到
`otelflags.FlagDomain`。在這兩件事都還不成立時建構的 wrapper,終其一生都從自己的環境變數與選項解析,
**永遠不會去問 relay**,即使你下一刻就安裝了 provider。

所以:先安裝 provider,**再**建構你的 client 與連線。走零程式碼路徑的應用程式不受影響,因為 endpoint
變數在 process 啟動前就存在了。

這同時也是其他人維持「動態化之前」成本輪廓的原因:沒有 relay 的 process 不會配置它用不到的 instrumented
實作、不會註冊 MongoDB command monitor,也永遠不會初始化 OpenFeature SDK 的任何部分。

### 關閉 data collector

在你自己設定 provider 的那條路徑上,`DataCollectorDisabled: true` **不是選用的**。

provider 的 data collector 預設開啟。它每次評估都往一個有上限的記憶體 buffer 追加一筆事件,而且 flush
失敗時不會清空該 buffer。buffer 滿了之後,**每一次後續的追加都會在評估中的 goroutine 上同步 flush,
並持有 buffer 的 mutex**——所以 relay 中斷會讓每一個被 instrument 的操作卡在一個會逾時 10 秒的請求後面。
因為 flag 是每次操作解析,這個 buffer 會與你的流量成正比地填滿。

關掉它不會失去什麼:它餵的是 relay 的評估分析儀表板,而對於每次操作評估的 process 層級 flag 而言,
那些分析只是你流量數字的副本。

### relay 不是啟動的前置條件

如果安裝 provider 時 relay 不可達,記個 log 然後繼續。process 會以它的環境變數與選項宣告的狀態啟動,
在 provider 成功抓取之前沒有 relay 控制。

一旦成功抓取過一次,之後的中斷不會改變任何事:in-process 評估器會服務它最後一次成功抓取的設定,
而且任何評估都不會產生網路 I/O。

### 啟動視窗

零程式碼安裝是非阻塞的,所以從安裝到 provider 第一次成功抓取之間,每個開關都解析為它的**本地**值——
環境變數、選項、預設值。

對**啟用**而言這是 fail-safe 的。這個視窗可能延遲一次 relay 驅動的啟用;它永遠不可能引入一次啟用,而對
`otel-mongo` 而言,它永遠不可能寫入一個你的部署沒有設定過的 `_oteltrace` 欄位。

**對關閉而言則不是,而且這是刻意的:relay 的 `false` 不會跨重啟延續。** 如果你的部署裡有
`OTEL_NATS_TRACING_ENABLED=true`、而你在 relay 上把該模組關掉,重啟後的 process 會再次 trace,直到它第一次
成功抓取為止 —— relay 不可達時則是無限期。

這個不對稱是設計決定,不是疏漏。把「provider 尚未就緒」讀成 `false` 會逐 key 生效,而 master key 的本地
預設值是 `true`,所以每一個配置了 relay 的 process 每次重啟都會被**完全否決**,直到它第一次抓取;relay
停機多久就黑多久。控制平面不該變成可用性的相依,而一個沒有資料的來源也不該勝過部署明確寫下的值。

所以:**relay 是 runtime 控制;持久狀態屬於環境變數。** 事故煞車的正確程序是兩步,順序不可顛倒:

1. 翻動 relay 旗標。它會在輪詢週期內對所有執行中的 process 生效。
2. 在任何重啟發生之前,把同一個值落進部署的環境變數。兩邊一致之後,這個視窗就無害了。

若你希望第一個操作之前就拿到 relay 的答案,用 `otelflags.InstallProvider` 安裝你自己的 provider —— 它會
等待初始化完成,也會讓零程式碼安裝讓位。

### 只支援 in-process 評估

provider 的 in-process 模式——背景輪詢、本地查表——是唯一支援的模式,零程式碼路徑寫死了它。remote 評估
會把每一次評估變成一個 HTTP 請求,那等於在每一次 Mongo query 與 NATS publish 的路徑上放兩到三次同步網路
往返。

### 只針對單一服務而非整個艦隊

設 `OTEL_SERVICE_NAME`,並在 relay 規則裡對 `service.name` 撰寫條件:

```yaml
otel-mongo-tracing:
  variations: { enabled: true, disabled: false }
  targeting:
    - query: service.name eq "checkout-api"
      variation: enabled
  defaultRule: { variation: disabled }
```

沒有它的話,一個 relay flag 會套用到**該 relay 服務的每一個 process**——在 flag 能夠啟用之後,這件事更
重要了。

**如果你是用程式碼而非環境變數設定 `service.name`**,那個值靠它自己到不了本 library。OpenTelemetry 的
Resource 由環境建構、從不回寫環境,而且 `TracerProvider` 介面與 SDK 的具體型別都沒有提供讀回 Resource 的
方法。所以你在 Go 程式碼裡傳入的 `service.name`,在設計上就對這裡不可見。兩種補法:

- **由你擁有 provider**(SDK 通常的答案):設定一次 API 級的全域 evaluation context,它就會進入本 library
  的每一次評估。這裡不會覆寫它 —— 走這條路時 library 傳入的是空的 invocation context。

  ```go
  openfeature.SetEvaluationContext(openfeature.NewTargetlessEvaluationContext(
      map[string]any{"service.name": "checkout-api"}))
  ```

- **你走零程式碼路徑**:在你自己的初始化流程裡、第一個被 instrument 的操作之前,用同一個值設定
  `OTEL_SERVICE_NAME`。只在它不存在時才設,可以讓明確設定過它的部署繼續作主。

  ```go
  if _, ok := os.LookupEnv("OTEL_SERVICE_NAME"); !ok {
      os.Setenv("OTEL_SERVICE_NAME", serviceName)
  }
  ```

本 library 只在零程式碼路徑上提供這個屬性。

不支援 per-request targeting。resolver 不持有任何 request 狀態。

## 一次改動要多久才生效

**provider 的輪詢間隔——預設 60 秒,再放寬最多 10%。** 函式庫每次操作都重新解析,所以 provider
一拿到新設定,下一個操作就會用到;它唯一增加的延遲就是下面這段抖動。

| | 延遲 |
|---|---|
| 模組本身的解析 | 無 —— 每次操作都讀當下的值 |
| provider 輪詢間隔(零程式碼路徑) | **最多 66 秒** —— 設定的 60 秒再偏移最多 ±10%,可用 `OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL` 調整 |
| provider 輪詢間隔(你自己的 provider) | 你設多少就是多少;GO Feature Flag 的預設是 **120 秒**。你自己設定的間隔,本函式庫不會加抖動 |

偏移量在每個 process 只抽一次,之後固定不變,這樣同一次部署起來的整批 process 就不會一直用同一個週期
打 relay。規則與 relay proxy 的 `enablePollingJitter` 相同——後者做的是再上一跳,relay proxy 到你的
flag 儲存那段。

要講清楚兩者都沒有打散的東西:provider 初始化時會無條件把整份設定抓一次,之後才啟動 ticker。那一次請求
的時間點就是 process 自己的啟動時間,所以整批同時重啟時,它們仍然會一起打到 relay。刻意不去延後它:被
延長的那段窗口正是每個開關都讀 local 值的窗口,而該窗口對啟用是 fail-safe,對停用不是。

規劃事故應變時要以輪詢間隔為準,不要以「立即」為準——**兩個方向都一樣**,啟用與停用都受它約束。
如果 60 秒對你的風險輪廓來說太慢,把它調小:輪詢是一個帶 ETag 的條件式 `GET`,沒變動時回 304,
所以調緊很便宜。

## 解析的成本

一次被 instrument 的操作不只評估一次:

| 模組 | 每次被 instrument 的操作評估幾次 |
|---|---|
| `otel-nats`、`otel-gorilla-ws` | 2 —— master、模組 |
| `otel-mongo` 讀取 | 2 —— master、tracing |
| `otel-mongo` 寫入 | 3 —— master、tracing、propagation |

**數量級上,在開發機上一次評估約是個位數微秒與少量記憶體配置** —— 本 repo 沒有附上對應的 benchmark,
所以請把它當成形狀而非數字,規劃容量前先在你自己的工作負載上量測。

成本來自 OpenFeature SDK 的評估流水線——hook 鏈、evaluation context 合併、provider registry 的鎖——而不是
查 flag 本身,所以把設定放在記憶體裡也不會讓它變便宜。對 Mongo 的一次往返來說是雜訊;對 NATS publish
來說不是,它建立一個 span 的成本本來就在同一個量級。要注意的是,**這個成本與旗標的值無關**:一條 relay
搆得到、但模組旗標為 `false` 的連線,每次操作照樣評估;只有「relay 不可能存在」的 process 才會整條流水線
都跳過。

**沒有配置 relay 的 process 一毛都不用付**,也不會配置它搆不到的 instrumented 實作。

刻意不做快取:快取會讓一次 flag 改動比輪詢間隔所暗示的更晚生效。它藏在一個不會變的內部簽名後面,所以
若某天真實工作負載的 benchmark 顯示有必要,可以在不影響任何 API 的情況下加上去。理由記錄在設計文件裡。

## 旗標管不到什麼

這些開關管的是**本 library 的 instrumentation 路徑**,除此之外什麼都不管。有四條邊界值得明說,因為每一條
都曾被當成 bug:

**關掉一個模組會停掉 trace context 的傳遞,不只是 span。** disabled 路徑不會把 `traceparent` 注入 NATS
header、WebSocket envelope 或 Mongo document,也不會在進入時抽取。分散式 trace 因此**在該邊界斷鏈**:
對面的工作會開一條新的 trace,而不是接續你的。若你想在 library 被靜音時仍保留 trace 串接,`otel-mongo`
明確支援 —— 見[什麼沒有被 gate](#什麼沒有被-gate)。

**打開一個模組不可能產生你的應用程式並未匯出的遙測。** wrapper 用的是你給它的 `TracerProvider`,或全域
的那個。如果那是 no-op provider —— 因為應用程式從未設定 OTel SDK,或設定成關閉 —— relay 驅動的啟用改變的
是哪條程式路徑在跑、成本多少,但永遠不會有 span 被匯出。啟用 instrumentation 與啟用匯出是兩個獨立決定,
relay 只做得到第一個。

**master 旗標只管這些模組。** `otel-instrumentation-go-tracing` 會停掉本 repo 裡的每一個 instrumentation
模組。它對 embedding SDK 自己的 provider、它的其他整合、你應用程式的 feature flag,或你的匯出管線,
都沒有任何作用。

**handshake 無法重來。** 對 `otel-gorilla-ws` 而言,啟用只影響之後才建立的連線,而停用會停掉 span 與
inject/extract 但不會停掉 envelope。見
[otel-gorilla-ws:negotiation 是交握時的既成事實](#otel-gorilla-wsnegotiation-是交握時的既成事實)。

## Per-connection 選項

`WithTracingEnabled(v bool)` 為單一 connection 或 client 提供**模組**那一層,給無法設定 process 環境變數
的呼叫方使用——測試,或同一個 binary 裡多個各自設定的 client。它被 `otelnats.ConnectWithOptions` 及其 TLS
與 credentials 變體、`otelmongo.ConnectWithOptions` 與 `NewClient`(v1 與 v2)、以及
`otelgorillaws.NewConn` / `Dial` / `Upgrader.Upgrade` 接受。`otel-mongo` 另外接受
`WithTracePropagationEnabled(v bool)`。

- 它**不能**提供 master 開關,那是 process 層級的,不接受任何選項。帶著 `WithTracingEnabled(true)` 的
  連線在 master 被否決時一樣會停。
- 它**輸給**配對的環境變數,也輸給 relay。
- 它**不會**讓連線變成靜態。帶著它的連線每次操作仍然會解析 master 開關與 relay。
- 它與環境變數同時出現是合法的;變數贏。(這取代了先前「同時設定即為建構錯誤」的規則。)
- 即使傳了選項,環境變數的無效值仍然是錯誤——選項不能替一個排在它之上的變數開脫。

變數未設定時,選項就是決定性的那一層,所以一條追蹤與一條不追蹤的連線可以共存於同一個 process。

## 維運速查

**要立刻打開一個模組:** 把它的 relay flag 設成 `true`,在輪詢間隔內生效。對 `otel-gorilla-ws` 而言,
這只會影響之後才建立的連線。

**要立刻關掉一個模組:** 把它的 relay flag 設成 `false`。有四個限制要知道:

- 它停掉的是 **trace context 傳遞與 span 兩者**,所以分散式 trace 會在該邊界斷鏈;見「旗標管不到什麼」。
- 它不會停掉 `ContextFromDocument` / `ContextFromRawDocument`,那兩個依設計就沒有 gate;
  見「什麼沒有被 gate」。
- 對 `otel-gorilla-ws`,它會停掉 span 與 inject/extract,但**不會**停掉 JSON envelope,所以線路成本
  要等連線重連才會消失。
- 它不會跨重啟延續。請把同一個值落進環境變數;見「啟動視窗」。

**要停掉整個艦隊:** 把 `otel-instrumentation-go-tracing` 設成 `false`。它底下的任何東西都逃不掉,
包含那些在 Go 程式碼裡傳了選項的連線。

**要在單一部署停掉一切、且不用 relay:** 設 `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=false`。

**要在單一部署停掉單一模組、且不用 relay:** 設 `OTEL_<MODULE>_TRACING_ENABLED=false`。
應用程式碼無法覆寫它。

**要只停掉單一服務而非整個艦隊:** 設 `OTEL_SERVICE_NAME`,並在 relay 規則裡對 `service.name` 下條件。

**要讓一個模組可被 relay 控制:** 設 `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`,並在 relay 上建立它的
flag。不需要應用程式碼。部署成你想要的靜止狀態即可——relay 之後可以往任一方向移動它。

**如果你完全沒有 relay:** 由環境變數與選項決定,跟動態 flag 出現之前一模一樣,成本也一模一樣。
