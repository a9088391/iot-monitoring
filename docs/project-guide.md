# IoT 設備監控與告警 v0.1｜專案與測試指南

這是可用 Docker 在本機重現的多場域設備監控系統。展示資料包含北區、南區及六台模擬設備。維運人員能辨識設備是否持續回報、溫度異常是否已恢復，以及告警是否已由人員結案。

本文件保留模擬展示的架構與 v0.1 歷史成果。目前登入後預設顯示臺中市官方資料，操作下列模擬流程前請先選擇「模擬展示」。最新啟動、停止與重設命令見 [README](../README.md) 與[操作手冊](operations.md)。

## 使用的程式語言與技術

| 層面 | 技術 | 用途 |
| --- | --- | --- |
| 後端 | Go 1.27.3、Gin 1.12.0 | 提供 HTTP API，處理模擬回報、離線巡檢、告警、結案與登入授權，並提供前端靜態檔案 |
| 前端 | TypeScript 6.0.3、Vue 3.5.45、Vite 8.3.6、HTML、CSS | 顯示登入、設備清單與詳情、模擬控制及告警歷程 |
| 資料庫 | PostgreSQL 18.6、SQL、pgx | 保存帳號、場域、登入工作階段、每台設備的最新有效測量、告警與結案紀錄 |
| 即時更新 | Server-Sent Events（SSE） | 依登入者的場域範圍通知已提交的版本，瀏覽器再讀回授權快照。斷線後重新同步 |
| 本機環境 | Docker Compose、Dockerfile、POSIX Shell | 建置應用、啟停服務、隔離測試與安全重設展示資料 |
| 自動化測試 | Go 測試、JavaScript、Node.js 24.22.2、Playwright Core、Chrome | 以真實 PostgreSQL、HTTP、瀏覽器及容器驗證功能、競爭、重啟和效能 |

## 程式責任與 DTO

前端按實際用例採 DDD-Lite 的責任分工，保持會一起變動的狀態與防護在同一處；不建立通用 repository、DI 容器或逐表包裝層。

| 入口 | 責任 |
| --- | --- |
| [App.vue](../frontend/src/App.vue)、[AlertHistory.vue](../frontend/src/AlertHistory.vue) | 組合畫面、導覽、DOM 焦點與操作事件，保留既有語意標籤及響應式版面 |
| [monitoringDtos.ts](../frontend/src/monitoringDtos.ts) | 定義登入、設備、模擬、告警與結案的請求／回應欄位；不把畫面狀態當成可寫入的資料實體 |
| [api.ts](../frontend/src/api.ts) | 處理同源 HTTP、CSRF 標頭、回應與錯誤解碼；從同一登入回應取得身分、viewer 與期限 |
| [useMonitoringSession.ts](../frontend/src/useMonitoringSession.ts) | 持有原登入世代、受保護讀寫、取消與到期清除，以及該登入的 SSE 生命週期 |
| [useDeviceMonitoring.ts](../frontend/src/useDeviceMonitoring.ts) | 協調設備清單、詳情與模擬控制版本；不明操作只讀回狀態，不自動重送 |
| [useAlertHistory.ts](../frontend/src/useAlertHistory.ts) | 協調告警分頁、詳情、草稿與結案回讀，保留待確認與舊回應所有權防護 |
| [monitoringRules.ts](../frontend/src/monitoringRules.ts)、[monitoringPresentation.ts](../frontend/src/monitoringPresentation.ts) | 共用溫度正常區間、送出前結案檢查、文字標籤、臺灣時間與完整精度的溫度格式 |

DTO 是前端的明確傳輸型別；執行階段的授權、嚴格 JSON／控制字元驗證與交易一致性仍由 Go 負責。Go 沿用登入授權、測量採用、告警、模擬控制與查詢模組，使用 `Measurement`、設備／告警清單與明細、模擬狀態等輸入或回應 DTO，不直接輸出資料庫模型。[完整 API 與安全契約](operations.md#模擬溫度與告警-api)

## 已交付的功能

- **登入與場域隔離。** 總部帳號 `hq` 可查看兩個場域並控制模擬，`north` 與 `south` 只能查看所屬場域的設備與告警，並可結案該場域已恢復的告警。登入有期限、限流及同源寫入保護。
- **自動回報與展示情境。** 六台預載設備由服務啟動正常回報，各自以不同基準每五秒小幅變動。進入設備頁預選目前可見的第一台，手動改選後保留仍可見的設備。總部可恢復正常、開始異常、停止回報，或原樣重送最近一筆。過期、亂序、重複或舊執行世代的資料不更新最新測量，也不延長在線期限。
- **溫度、離線與告警。** 18～28°C 為展示用正常區間，兩個端點都包含在內。超過 15 秒未收到有效回報時判定離線，並保留最後有效溫度。離線與溫度異常各自判定及恢復。
- **告警歷程與人工結案。** 持續異常沿用同一筆告警。異常恢復後，授權人員可輸入處理結果並結案，結案人、時間及結果留在歷程。
- **即時同步、重啟與重設。** 畫面更新設備與告警，並在斷線後讀回最新快照。重啟保留資料，先撤銷舊模擬並補判離線，再自動正常回報。確認重設後，原登入及舊回報失效，系統重建預載資料並自動啟動六台設備。

v0.1 使用應用內的模擬設備，網站綁定本機 loopback。溫度門檻供展示，離線表示超過期限未收到有效回報。

## 如何測試

以下指令皆從專案根目錄執行。一般啟動和手動操作只需一個瀏覽器視窗。

### 手動展示

1. 準備本機 Docker Engine、Docker Compose 和 OpenSSL，執行 `./bin/start`。開啟 <http://iot.localhost:18241>，從本機 `.local/demo-passwords.json` 取得示範密碼。
2. 以 `hq` 登入，選擇「模擬展示」（或開啟 `/?view=simulation`），確認預選北區設備 01，六台設備已有不同的正常值且每五秒小幅變動；再開始異常回報。設備應在線、顯示 30°C，告警歷程應出現溫度異常。
3. 停止回報，等待畫面顯示離線及「資料已非即時」。再開始異常回報，設備應恢復在線，原溫度告警仍持續。按「恢復正常」後，畫面應回到 18～28°C 並持續小幅變動，該筆溫度告警應變成「已恢復、待結案」。
4. 填寫結果並結案。依操作手冊的停機指令停止服務，再以 `./bin/start` 啟動。確認六台設備自動正常回報，原結案仍可讀回；停機超時的離線／恢復歷程保留。
5. 登出並改用場域帳號，確認只能看到所屬場域。若展示資料可以清除，可依操作手冊執行 `./bin/reset` 並確認目標。重設後，舊頁面應要求重新登入，六台設備以新世代自動正常回報，舊告警已清除。

停機指令、重設範圍及失敗復原步驟見[啟動與操作](operations.md#啟動與操作)及[本機展示重設](operations.md#本機展示重設)。

### 自動化功能測試

準備 Node.js 24.22.2、可執行的本機 Docker 及 Chrome 或 Chromium。瀏覽器位於非預設路徑時，使用 `CHROME_EXECUTABLE` 指定執行檔。

```sh
(cd tests && npm ci && npm test -- --test-concurrency=1)
```

測試使用獨立的 Compose 專案、暫存目錄、連接埠和資料卷，涵蓋登入、場域隔離、模擬、告警、SSE、重啟與重設。下列七組測試另需 Go 1.27.3 和 `compose.yaml` 指定的本機 PostgreSQL 映像。每組先測真實資料庫寫入，再重啟同一資料卷核對讀回。

```sh
./bin/test-measurements
./bin/test-measurements automatic
./bin/test-measurements simulation
./bin/test-measurements offline
./bin/test-measurements closure
./bin/test-measurements sse
./bin/test-measurements reset
```

### 靜態檢查與建置

```sh
(cd backend && go test ./... && go vet ./... && go build -o /dev/null .)
(cd frontend && npm ci && npm run typecheck && npm run build)
```

### 正式效能驗收

先在 `tests` 安裝鎖定依賴。使用已解鎖的 macOS 桌面，保持十個原生 Chrome 測試視窗可見。

```sh
(cd tests && npm ci)
caffeinate -di node tests/performance.js
```

測試以隔離容器執行，暖機一分鐘，正式五分鐘。100 台模擬設備固定以每秒 20 筆回報，核對 6,000 筆正式採用及十個視窗的逐筆畫面延遲。從指令印出的證據目錄讀取 `report.json`。只有正式量測通過才回傳結束碼 0。可加 `--probe` 檢查環境是否可用，探查成功仍會標為 `INCOMPLETE`。

## v0.1 實測結果

以下為自動回報與預選設備變更前的歷史結果，不代表目前版本已完成相同驗收。

- 單視窗走完登入、模擬、離線恢復、結案、停機、重啟與重設，畫面及 API 讀回一致。
- 最終 Node.js 循序回歸 **35/35**，六組 PostgreSQL 整合與重啟共 **12/12**。Go 測試、race、vet、建置及 Vue 型別和建置檢查通過。
- 本機正式效能驗收 **PASS**。6,000 筆正式回報全數採用，24,018 筆必要畫面更新皆有逐列可視證據。離線狀態最慢延遲上界約 **1.782 秒**，恢復觀看後 50 列最慢約 **0.830 秒**。環境、配額和各類延遲見[正式效能驗收](operations.md#已量測的效能結果)。
