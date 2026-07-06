### 1. 標準 WebSocket Subprotocol 行為（無 OTEL-WS）

| 情境 | 客戶端類型 | 客戶端發送的 Subprotocol | 伺服器類型               | 伺服器回傳的 Subprotocol | Trace Propagation | 結果               | 說明與 RFC 6455 規範                                                                      |
| ---- | ---------- | ------------------------ | ------------------------ | ------------------------ | ----------------- | ------------------ | ----------------------------------------------------------------------------------------- |
| A    | ws client  | ""（無協議 / empty）     | ws server (support json) | false                    | -                 | **使用者決定結果** | 若客戶端未指定 sub-protocol，伺服器應拒絕（或不接受）。強制要求明確協議，避免未定義行為。 |
| B    | ws client  | "json"（或多個協議）     | ws server (support json) | "json"（第一個支援的）   | -                 | **連線成功**       | 伺服器選擇第一個它支援的協議回傳。若無支援則拒絕。                                        |

**相關規範重點**：根據 RFC 6455，客戶端可在 handshake 時提出多個 subprotocol，伺服器必須回傳其中一個它接受的（或不回傳表示拒絕）。空協議通常導致拒絕或降級處理。

### 2. OTEL-WS Client 行為（隱藏協議注入機制）

OTEL-WS Client 會在使用者指定的協議**最前面注入隱藏協議 `otel-ws`**，用來協商是否啟用分散式追蹤。

| 情境 | 客戶端類型     | 客戶端發送的 Subprotocol | 伺服器類型                 | 伺服器回傳的 Subprotocol | Trace Propagation | 結果               | 說明                                                                                  |
| ---- | -------------- | ------------------------ | -------------------------- | ------------------------ | ----------------- | ------------------ | ------------------------------------------------------------------------------------- |
| C    | otel-ws client | "otel-ws,json"           | ws server (support json)   | "json"                   | **Disabled**      | 連線成功，但無追蹤 | OTEL-WS Client 偵測到回傳的不是 `otel-ws+...` 前綴 → 停用追蹤，純透傳 payload。       |
| D    | otel-ws client | "otel-ws,json"           | ws server (support binary) | false                    | **Disabled**      | 連線成功，但無追蹤 | 伺服器不支援任何提出的協議 → OTEL-WS Client 偵測到 empty → 連線保持存活，降級為 passthrough（不強制關閉連線）。 |
| E    | otel-ws client | ""（無協議）             | ws server (support json)   | false                    | **Disabled**      | 連線成功，但無追蹤 | 不注入 `otel-ws`；握手維持空協議，OTEL-WS Client 只做轉傳。                            |

### 3. OTEL-WS Server 行為

OTEL-WS Server 會檢查收到的協議是否帶有 `otel-ws` 前綴，決定是否啟用追蹤。

| 情境 | 客戶端類型        | 客戶端發送的 Subprotocol | 伺服器類型     | 伺服器回傳的 Subprotocol | Trace Propagation | 結果                | 說明                                                                              |
| ---- | ----------------- | ------------------------ | -------------- | ------------------------ | ----------------- | ------------------- | --------------------------------------------------------------------------------- |
| F    | ws client（普通） | ""（無協議）             | otel-ws server | false                    | **Disabled**      | 連線成功，但無追蹤  | 不拒絕連線；OTEL-WS Server 只做轉傳。                                               |
| G    | otel-ws client    | "otel-ws,json"           | otel-ws server | "otel-ws+json"           | **Enabled**       | 連線成功 + 啟用追蹤 | 伺服器偵測到 `otel-ws` 前綴 → 啟用 trace propagation，並在線上回傳 `otel-ws+json`（`Conn.Subprotocol()` 會去除 `otel-ws+` 前綴，於應用層回傳 `json`）。 |
| H    | ws client（普通） | "json"                   | otel-ws server | "json"                   | **Disabled**      | 連線成功，但無追蹤  | 伺服器檢查輸入協議不含 OTEL 前綴 → 停用追蹤，正常透傳所有 payload（保持相容性）。 |

### 4. OTEL-WS 核心設計原則與邊緣情境總結

| 項目               | 描述                                               | 處理方式                                     | 優點 / 注意事項                           |
| ------------------ | -------------------------------------------------- | -------------------------------------------- | ----------------------------------------- |
| **隱藏協議注入**   | OTEL-WS Client 自動在最前面加入 `otel-ws`          | 格式如：`"otel-ws,json"` 或 `"otel-ws+json"` | 不影響原有協議清單，實現透明追蹤協商      |
| **協議前綴識別**   | Client 收到 `otel-ws+xxx` 或 Server 收到 `otel-ws` | 解析前綴後啟用追蹤                           | 成功協商後才啟用，避免不相容時的開銷      |
| **不相容降級**     | 對方不支援 OTEL 協議                               | 停用 trace propagation，純透傳               | 保持最大相容性，不會破壞原有連線          |
| **空協議處理**     | OTEL-WS client/server 任一方未提供 sub-protocol    | 允許連線並降級為 passthrough（不封裝）       | 不破壞既有連線，同時維持 send/receive span |
| **多協議支援**     | 客戶端可傳多個協議                                 | 伺服器回傳第一個支援的                       | 符合 RFC 6455 標準協商規則                |
| **Binary vs JSON** | 不同 payload 類型                                  | 協議名稱區分（json / binary）                | OTEL-WS 不干涉 payload 本身，只處理協議層 |

**邊緣情境與注意事項**：

- **如果伺服器只支援 binary**：OTEL-WS Client 注入 `otel-ws,json` 後通常會收到 empty → 連線保持存活，降級為 passthrough（不強制關閉連線）。
- **追蹤開銷**：僅在成功協商 `otel-ws` 前綴時啟用，避免不必要的 header 或 context 注入。
- **相容性優先**：OTEL-WS 設計目標是「不破壞原有非 OTEL WebSocket 連線」，降級時完全不影響功能。
- **安全性**：強制 sub-protocol 可減少攻擊面（例如防止未經驗證的連線）。

這個表格已涵蓋 Excalidraw 圖中所有矩形區塊、箭頭標籤（如 `""`、` "json"`、` "otel-ws,json"`）、側邊說明文字，以及 OTEL-WS 的隱藏注入與檢查邏輯。
