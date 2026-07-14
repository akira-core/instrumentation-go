# Branch Code Review

- 範圍：`git diff main...HEAD`
- `main`：`1eab409d50dd38176ce3a883924c4ed814850dc8`
- Merge base：`1eab409d50dd38176ce3a883924c4ed814850dc8`
- `HEAD`：`05138eeb7935511fffe2dfe7ec8e3c6dc54e6355`（`feat/address-o11y-feedback`）
- 初次 review HEAD：`d324be0588860cd02d2ceb9d451cc7bb927252e3`
- 結果（初次）：6 項 findings（3 Important、3 Moderate）
- **Re-check（2026-07-13）**：1 Fixed / 1 Partial / 5 Open（含 #2 殘餘）

## Re-check summary（2026-07-13）

| # | Severity | Status | 現況 |
|---|----------|--------|------|
| 1 | Important | **Open** | `!negotiateOTel` 仍未 strip `responseHeader` 的 otel-ws |
| 2 | Important | **Partial → Fixed core** | `defer setCapturedServerAttrs` + 測試已落地（`9b0c911`）；inject 失敗路徑仍缺 `RecordSpanError` |
| 3 | Important | **Open** | CHANGELOG 仍標 `backward-compatible` |
| 4 | Moderate | **Open**（低優先） | 仍只有 exact + MIME canonical |
| 5 | Moderate | **Open** | README 版本表仍為 `0.6.2`／`0.6.0` |
| 6 | Moderate | **Open** | 四份 CHANGELOG 仍連已刪的 `RELEASE-NOTES-0.6.0.md` |

---

## Findings

### 1. Important — feature-off WebSocket 仍可透過 response header 協商 `otel-ws`

**狀態：Open（未修）**

**位置：** `otel-gorilla-ws/upgrader.go:121-126`

當 `WithTracingEnabled(false)` 或環境 gate 關閉時，`negotiateOTel` 為 false；但若 `appProtocols` 為 nil，程式仍將原始 `responseHeader` 傳給 gorilla upgrader。gorilla 在 `Inner.Subprotocols == nil` 時會接受 `responseHeader` 中的 `Sec-WebSocket-Protocol`（`selectSubprotocol` else 分支），因此呼叫者提供 `otel-ws`／`otel-ws+json` 仍會讓對端啟用 envelope，而本端建立的 `Conn` 卻保持 `tracingEnabled=false`（`newConnFromConfig(..., negotiateOTel=false, ...)`）。

**Re-check 證據：** `else` 分支僅 `inner.Subprotocols = appProtocols`，無 `cloneHeader`／strip。既有測試涵蓋「feature-off 不 offer／不 confirm client offer」，**沒有**覆蓋 caller 手動塞 responseHeader 的路徑。

**建議：** 在 `!negotiateOTel` 路徑複製 header 並移除或拒絕任何 `otel-ws` protocol；新增 feature-off、空 app protocol、response header 含 `otel-ws+json` 的回歸測試。

### 2. Important — Mongo trace 注入提前失敗時遺失 static `server.*` fallback

**狀態：Fixed（核心）／Partial（Decision Log 擴充）**

**位置：**

- `otel-mongo/otelmongo/internal/traced/collection.go`（各 CRUD 的 `WithAddrCapture` 後）
- `otel-mongo/v2/internal/traced/collection.go`

`server.address`／`server.port` 已由 span 建立時移至 driver 呼叫後的 `setCapturedServerAttrs`。但 `InsertOne`、`InsertMany`、`ReplaceOne`、`BulkWrite` 在 `_oteltrace` 注入或 BSON 轉換失敗時會提前返回，因而完全跳過 static fallback。

**Re-check：** 已由 `9b0c911` 修復——所有 CRUD 在 `WithAddrCapture` 後 `defer t.setCapturedServerAttrs`；v1／v2 各有 `TestCollection_InsertOne_InjectFailureKeepsStaticAddr`、`TestCollection_BulkWrite_InjectFailureKeepsStaticAddr`。

**殘餘（Decision Log 擴充，仍未修）：** `InsertOne`／`InsertMany`／`ReplaceOne` 注入失敗 early return 仍跳過 `shared.RecordSpanError`（僅 `BulkWrite` 有記）。失敗 span 現在有 `server.*`，但 error status／exception 仍缺。

### 3. Important — `Upgrader.Upgrade` 的 variadic 變更破壞公開 API 型別相容性

**狀態：Open（未修）**

**位置：**

- `otel-gorilla-ws/upgrader.go:69`
- `otel-gorilla-ws/CHANGELOG.md:21`

將 `Upgrade(w, r, header)` 改成 `Upgrade(w, r, header, opts ...Option)` 雖不影響一般三參數呼叫，但 method value 型別與介面 method signature 已改變。既有的函式指派或介面實作檢查會在升級後無法編譯；CHANGELOG 將其標示為 backward-compatible 並不正確。

**Re-check 證據：** `CHANGELOG.md:21` 仍寫 `(backward-compatible — ...)`，且仍列在 `### Added` 而非 `### Changed — BREAKING`。

**建議（Decision Log 採替代方案）：** 條目移到 Changed — BREAKING 並加 migration 說明（method value／interface 指派需重新包裝為三參數閉包）。不拆 `UpgradeWithOptions`。

### 4. Moderate — `HeaderCarrier` 只支援 exact／MIME canonical casing

**狀態：Open（低優先，可否決）**

**位置：** `otel-nats/otelnats/propagation.go:23-46`

新增 fallback 只嘗試 `traceparent` 與 `Traceparent`。其他合法大小寫（例如 `TRACEPARENT`、`TraceParent`）仍無法擷取，跨語言 producer 可能因此遺失 trace context 或 baggage。

**Re-check 證據：** `Get`／`Values` 仍為 exact → `CanonicalMIMEHeaderKey`；無 `strings.EqualFold` 掃描。

**建議：** exact 與 canonical 查找失敗後，以 `strings.EqualFold` 掃描 keys；保留 exact-key 優先與空值語意，並補任意 casing 測試。

### 5. Moderate — root README 的 source version 與原始碼不一致

**狀態：Open（未修）**

**位置：**

- `README.md:15-19`
- `README.zh-TW.md:15-19`

版本表標示為 `Version (source)`，但仍列出 `0.6.2`／`0.6.0`，而本分支四個 instrumentation version 與 CHANGELOG 均已是 `0.7.0`。

**Re-check 證據：** version constants 皆為 `0.7.0`；兩份 README 表格仍為舊值。Install 範例維持 `v0.6.0` tag 屬預期（tag 尚未發布）。

**建議：** 表格全部更新為 `0.7.0`。

### 6. Moderate — 四份 CHANGELOG 連到已刪除的 release notes

**狀態：Open（未修）**

**位置：**

- `otel-gorilla-ws/CHANGELOG.md:25`
- `otel-mongo/CHANGELOG.md:30`
- `otel-mongo/v2/CHANGELOG.md:31`
- `otel-nats/CHANGELOG.md:32`

本分支刪除了 `RELEASE-NOTES-0.6.0.md`，但四份 CHANGELOG 仍連到該檔案，發布後連結會失效。

**Re-check 證據：** `ls RELEASE-NOTES*` → 無檔案；四處連結句仍在。

**建議（Decision Log 採替代方案）：** 移除該連結句，保留既有 highlights。

## 驗證

初次 review：

- `otel-mongo`: `go test -race ./...` — 144 passed
- `otel-mongo/v2`: `go test -race ./...` — 165 passed
- `otel-nats`: `go test -race ./...` — 88 passed
- `otel-gorilla-ws`: `go test -race ./...` — 62 passed
- `git diff --check main...HEAD` — passed

Re-check（2026-07-13）：以原始碼對照 findings，未重跑全套 test；#2 相關測試檔已存在於 v1／v2 `internal/traced/collection_test.go`。

未執行各 module 的 Docker/testcontainers integration test，也未在實際 tag push 上執行 GitHub Actions release guard。

## Review notes

以下候選問題經驗證後未列為 branch finding：

- NATS reply receive 的 parent/link 邏輯雖有疑點，但相關 extraction 程式在此 branch diff 前已存在。
- `MessageBatch.Stop()` 不等待 forwarding goroutine 結束的語意也已存在；本分支新增 receive-side cancellation select，並未造成該行為回歸。
- Tag-push workflow 只能在 tag 已建立後報錯，無法撤銷遠端 tag；這是目前 release 流程的殘餘風險，應搭配 protected tags 或由受控 release workflow 建立 tag。

---

# Decision Log（2026-07-13）

逐項對照現行原始碼驗證後的處置決定。初次寫入時**尚未做任何程式變更**；之後 #2 核心已由 `9b0c911` 落地。

結論：6 項全數成立。3 項照建議修（#1、#2、#5）、2 項採替代方案修（#3、#6）、1 項接受但列低優先（#4）。#2 實作範圍略擴大（見下）。

## 1. Feature-off 仍可透過 response header 協商 otel-ws — 接受，修正

**驗證結果：成立。** `upgrader.go` 的 `!negotiateOTel` 分支把 `inner.Subprotocols` 設為 `appProtocols`；當其為 nil（未設定 `Subprotocols`／`AppSubprotocols`）時，gorilla 會從呼叫者的 `responseHeader` 讀取 `Sec-WebSocket-Protocol` 並回覆給對端，而本端 `Conn` 維持 passthrough——wire corruption 成立。

**理由：** 觸發需多個少見條件同時成立（feature off、無 app subprotocol 設定、caller 手動在 responseHeader 塞 otel-ws、client 也有 offer otel-ws），實務機率低；但它違反 0.7.0「feature-off 不得協商 otel-ws」的核心保證，且修正成本低，所以照修。

**修法：** `!negotiateOTel` 路徑 clone `responseHeader` 並移除其中的 otel-ws token；補回歸測試（feature-off ＋ 空 app protocols ＋ responseHeader 帶 `otel-ws+json`）。實作時同步檢查 `Dial` 是否有對稱漏洞（caller 在 requestHeader 手動塞 otel-ws）。

**實作狀態：未開始。**

## 2. 注入失敗提前返回遺失 server.* fallback — 接受，修正（範圍略擴大）

**驗證結果：成立**，全部 8 個位置（v1／v2 各 4）確認。此為本分支引入的回歸：`server.*` 從 span 建立時的 static attrs 移到 driver 呼叫後的 `setCapturedServerAttrs`，early return 會跳過它。

**額外發現（review 未提）：** `InsertOne`／`InsertMany`／`ReplaceOne` 的注入失敗路徑同時跳過 `shared.RecordSpanError`（`BulkWrite` 有記）——失敗 span 既無 `server.*` 也無 error status。一併修。

**修法：** `WithAddrCapture` 之後 `defer t.setCapturedServerAttrs(span, capture)`（defer LIFO 保證在 deferred `span.End()` 之前執行）；insert／replace 路徑補 `RecordSpanError`；v1／v2 各補「文件含 channel、無法 BSON 編碼」的注入失敗測試。

**實作狀態：**
- ✅ `defer` + InsertOne／BulkWrite 測試（`9b0c911`）
- ❌ `RecordSpanError` 於 InsertOne／InsertMany／ReplaceOne inject 失敗路徑（仍缺）

## 3. Upgrade variadic 破壞型別相容性 — 接受事實，採替代方案（只改 CHANGELOG，不拆新方法）

**驗證結果：成立。** Method value／interface 相容性確實破壞；且 `VERSIONING.md` 自己的 breaking 定義（"changes an exported Go API signature"）即涵蓋此變更——`CHANGELOG.md:21` 標 backward-compatible 是錯的。

**不採 `UpgradeWithOptions` 拆分，理由：** pre-1.0 政策允許 minor 版 breaking；0.7.0 本來就是 breaking release（同檔已有 Changed — BREAKING 區段）；一般三參數呼叫完全 source-compatible；拆分會長期留下雙 API。reviewer 自己也列了此替代方案。

**修法：** 把該條目移到 Changed — BREAKING 並加 migration 說明（既有 method value／interface 指派需重新包裝為三參數閉包）。

**實作狀態：未開始。**

## 4. HeaderCarrier 任意 casing — 接受，低優先（可否決）

**驗證結果：事實正確**，但風險偏理論：W3C 規範 header 名為小寫；已知真實 producer 只有 verbatim 小寫與 Go canonical（0.7.0 fallback 針對的具體案例）；`TRACEPARENT`／`TraceParent` 目前無已知來源。接受的理由是成本低（僅 exact＋canonical 都 miss 時才線性掃描）且符合 header 慣例的 case-insensitive 語意。

**修法：** `Get`／`Values` 在雙重 miss 後以 `strings.EqualFold` 掃描 keys；維持「以 key 存在與否觸發 fallback」的既有語意；多個不同 casing 並存屬病態輸入，行為標為未定義（map 迭代序）；補任意 casing 測試。**若認定 YAGNI，跳過此項也合理——請於審閱時裁決。**

**實作狀態：未開始。**

## 5. README source version 過期 — 接受，修正

**驗證結果：成立。** 欄位名確為 `Version (source)`，而四個 version constant 已是 `0.7.0`。

**修法：** 兩份 README 表格更新為 `0.7.0`。Install 範例維持 `v0.6.0` tag 不動——`0.7.0` tag 尚未存在，改掉會讓 `go get` 失敗；待實際 release 時一併更新。

**實作狀態：未開始。**

## 6. CHANGELOG 死連結 — 接受，採替代方案（移除連結，不恢復檔案）

**驗證結果：成立**，四處連結確認、檔案確已刪除。

**不恢復檔案，理由：** 刪除是本分支刻意行為（`bd8bde6 remove 0.6.0 release notes`），且 `VERSIONING.md` 政策本就把 root release notes 定為 optional、module CHANGELOG 為 canonical。

**修法：** 四份 CHANGELOG 移除該連結句，保留既有 per-module highlights（內容已自足）。0.6.0 的 GitHub Release 存在的話，可改連該處。

**實作狀態：未開始。**

## Review notes 三項（非 findings）

同意不列為 branch findings，本次不處理。tag 無法撤銷的殘餘風險屬 release 流程改善（protected tags／受控 tag 建立），另行追蹤。

## 實作順序（待核可）

1. ~~#2 server.* defer~~ — ✅ 已完成（`9b0c911`）
2. #2 殘餘 — insert／replace inject 失敗補 `RecordSpanError`
3. #1 — Important，wire corruption 防護：strip ＋ 測試，順檢 `Dial`
4. #3 — CHANGELOG relabel ＋ migration 說明
5. #6 — 四份 CHANGELOG 移除死連結
6. #5 — README 版本表
7. #4 — 低優先，依審閱結果決定做或跳過

每步依 CLAUDE.md 在該 module 目錄內跑 `go build`、`go test -race ./...`、`golangci-lint run ./...`。
