# IoT 設備監控

本機展示包含兩個場域、六台設備與三個示範帳號。總部可查看全部設備，場域帳號只能查看所屬場域。登入後可查看清單及單台設備資料，也可手動登出。

## 啟動與操作

需要透過本機 Unix socket 連線的 Docker Engine、支援 `build --builder` 與 `up --wait` 的 Docker Compose，以及 OpenSSL。在專案根目錄執行下列指令。

```sh
./bin/start
```

啟動器會在建置前檢查本機 Docker 端點、Compose 專案名稱和既有資源歸屬。若同名資源沒有此工作目錄的歸屬資訊，啟動會中止並保留原資源。

使用 Chrome 或 Chromium 開啟 [http://iot.localhost:18241](http://iot.localhost:18241)。曾用舊的 `127.0.0.1` 網址登入時，切換後需重新登入。瀏覽器仍會依原期限保存舊網址的 Cookie。首次啟動後，可在自己的終端機查看隨機示範密碼。

```sh
cat .local/demo-passwords.json
```

| 帳號 | 可見設備 |
| --- | --- |
| `hq` | 北區與南區，共六台 |
| `north` | 北區，三台 |
| `south` | 南區，三台 |

以任一帳號登入，選擇設備可查看詳細資料。登出後換另一個帳號，即可比較可見範圍。登入八小時後到期。登入嘗試受每帳號每分鐘五次與每來源每分鐘三十次的限制。 配額以 PostgreSQL 的 60 秒滾動視窗計算，重啟會沿用未到期的紀錄。

以 `hq` 登入並開啟設備詳情，即可控制模擬回報。開始正常或異常回報時，設備會立即送出 23°C 或 30°C，之後每五秒回報。停止只停止產生新測量，畫面仍顯示最新有效溫度。重複回報會原樣重送最近一筆原始測量。若畫面提示狀態已變更，請確認讀回的新模式，再手動操作。

用 `docker compose stop` 停止服務，用 `./bin/start` 再啟動。PostgreSQL 資料保存在本專案的 `data` 資料卷，`.local/` 保存在本機。請一併保留資料卷與 `.local/` 以便重啟，並妥善保管憑證。若啟動失敗，執行 `docker compose ps` 檢查服務狀態，或執行 `docker compose logs app db` 查看啟動原因。

## 架構與開發

- Go 1.27.3／Gin 1.12.0 提供 API 和 Vue 靜態檔案。伺服器依 session 決定場域，並使用 PostgreSQL 啟動鎖限制為單一實例。
- Vue 3.5.45／TypeScript 6.0.3／Vite 8.3.6 提供同來源的登入和設備查詢介面。瀏覽器透過限定於 `iot.localhost` 主機的 `HttpOnly`、`SameSite=Strict` Cookie 使用伺服器端 session。
- PostgreSQL 18.6 保存帳號雜湊、場域、設備、session、每台設備的最新測量、溫度告警與模擬控制歷程。Compose 只將網站發布到 `127.0.0.1`，資料庫留在 Compose 網路。這套 HTTP 設定供本機展示使用。

前端依賴及映像均已固定於 lockfile、`Dockerfile` 與 `compose.yaml`。本機開發檢查可使用 Go 1.27.3 和 Node.js 24.22.2。

```sh
(cd backend && go test ./... && go vet ./... && go build -o /dev/null .)
(cd frontend && npm ci && npm run typecheck && npm run build)
```

## 溫度與告警 API

在後端以 `SubmitMeasurement(ctx, Measurement)` 送入應用內模擬器回報。回報須符合設備目前允許的資料世代和執行識別，並通過順序與生成時間檢查。抵達與採用前的回報年齡均須在五秒內，包含恰好五秒。設備的最新測量與告警在同一筆交易提交，方法以 `(adopted, err)` 區分已採用、略過與資料庫錯誤。

服務啟動後，各設備的模擬維持停止，直到總部啟動回報。重啟會撤銷舊執行並清除可重送的原始測量。最新有效溫度、首筆期待時間、告警與控制歷程持續保存在資料庫。

總部可用 `GET /api/devices/:id/simulation` 讀取 `mode`、`version` 與 `canReplay`。寫入同一路徑時，沿用登入 Cookie，並帶上與網站相同的 `Origin` 和 CSRF 標頭。`version` 使用 GET 取得的十進位字串，例如首次操作時的 `"0"`。`action` 可用 `normal`、`abnormal`、`stop` 或 `replay`。

```http
POST /api/devices/north-01/simulation
Origin: http://iot.localhost:18241
X-CSRF-Protection: 1
Content-Type: application/json

{"action":"normal","version":"0"}
```

成功時回傳 `200` 與更新後的模擬狀態。控制成功表示指令已提交，設備 API 顯示已採用的溫度。`409 conflict` 表示版本已變更，請先重新讀取狀態。`409 no_measurement` 表示目前沒有可重送的原始測量。

登入後可透過 `GET /api/devices` 和 `GET /api/devices/:id` 讀取已採用的 `latestMeasurement`，其中 `receivedAt` 是入列前的抵達時間。`GET /api/alerts` 提供告警清單，`GET /api/alerts/:id` 讀取單筆告警。場域帳號只能讀取本場域，總部可跨場域查閱。清單以 `limit` 和 `offset` 分頁，預設 50 筆，最多 100 筆。

低於 18°C 或高於 28°C 會建立一筆溫度告警。同設備持續超標沿用該筆，收到 18～28°C 回報後標記恢復。恢復後再超標會建立新告警。

## 端到端測試

準備 Node.js 24.22.2、正在執行的本機 Docker，以及 Chrome 或 Chromium。若瀏覽器安裝於其他位置，可用 `CHROME_EXECUTABLE` 指定執行檔。

```sh
(cd tests && npm ci && npm test)
```

測試透過 `./bin/start` 建置和啟動獨立 Compose 專案，以真實 PostgreSQL、HTTP 和 Chrome 驗證登入防護、場域權限、重啟及瀏覽器操作。測試會使用自己的暫存目錄、連接埠和資料卷，並在結束時清理。已在 macOS arm64、OrbStack Docker Engine 29.8.2、Compose 5.5.0 與 Chrome 156.0.8078.82 驗證。

測量整合測試另需 Go 1.27.3 與 `compose.yaml` 指定的本機 PostgreSQL 映像。它使用可控時鐘呼叫測量介面，透過正式 HTTP 路由驗證提交與回滾。測試使用無外部網路的 PostgreSQL 容器及專用資料卷，並在資料庫重啟後從新的 Go 程序驗證溫度與告警讀回。結束時清理測試容器與資料卷。

```sh
./bin/test-measurements
./bin/test-measurements simulation
```
