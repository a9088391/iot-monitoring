# IoT 設備監控與告警｜操作與工程手冊

本文件提供目前的操作、安全與 API 說明，並保留 v0.1 歷史效能證據。預設頁面為臺中市政府官方環境觀測；原有自動回報與告警流程保留在「模擬展示」。歷史量測不代表官方資料介面已完成效能驗收。快速入口見[專案首頁](../README.md)。

本機模擬展示包含兩個場域、六台模擬設備與三個示範帳號。總部可查看全部設備並控制模擬回報，場域帳號只能查看所屬場域。設備溫度、在線狀態與告警會即時更新。異常恢復後，可由有權查看該告警的帳號填寫處理結果並結案。

模擬展示的溫度門檻僅供演示，官方頁不套用模擬門檻、在線或告警判定。離線表示在期限內未收到有效回報，畫面無法據此判斷設備是否斷電或網路是否中斷。

## 啟動與操作

需要透過本機 Unix socket 連線的 Docker Engine、支援 `build --builder` 與 `up --wait` 的 Docker Compose，以及 OpenSSL。在專案根目錄執行下列指令。

```sh
./bin/start
```

啟動器會在建置前檢查本機 Docker 端點、Compose 專案名稱和既有資源歸屬。若同名資源沒有此工作目錄的歸屬資訊，啟動會中止並保留原資源。

使用 Chrome 或 Chromium 手動開啟 [http://iot.localhost:18241](http://iot.localhost:18241)，一般操作只需一個視窗。曾用舊的 `127.0.0.1` 網址登入時，切換後需重新登入。瀏覽器仍會依原期限保存舊網址的 Cookie。首次啟動後，可在自己的終端機查看隨機示範密碼。

```sh
cat .local/demo-passwords.json
```

| 帳號 | 模擬展示可見設備 |
| --- | --- |
| `hq` | 北區與南區，共六台 |
| `north` | 北區，三台 |
| `south` | 南區，三台 |

以任一帳號登入，預設「設備清單」隨機抽選最多六台官方設備並預選第一台。名稱使用來源行政區與地點（沒有地點時使用原始設備名稱），另顯示原始設備名稱與 ID。按「重新抽選六台」或重新整理瀏覽器會重新取樣，新舊集合可能重疊。背景約每分鐘使用目前 ID 同步，保留手動選取；窄螢幕按「返回官方設備清單」後，背景同步也保留清單畫面。來源不足六台、沒有有效資料、設備消失或來源失敗時不補入模擬資料；失敗時提供重試並將保留資料標為上次成功快照。

選「模擬展示」或直接開啟 `/?view=simulation`，才進入原有設備與控制流程。側欄切換會同步網址，重整後仍使用所選來源。「告警歷程」只追蹤模擬設備，頁面會明確標示；可隨時以「設備清單」回官方頁，或以「模擬展示」回模擬設備頁。以下設備操作與權限皆指模擬展示，官方公開觀測對所有已登入帳號相同。模擬頁會預選目前可見的第一台設備並載入詳情。從告警歷程返回模擬設備頁會選取仍可見的設備；手動改選後，即時同步及重新載入會保留仍可見的選取。換頁時若原設備不在該頁，改選該頁第一台；空清單不查詳情。窄螢幕可按「返回設備清單」，背景同步不會立刻把畫面帶回詳情。登出後換另一個帳號，即可比較可見範圍。登入八小時後到期。登入嘗試受每帳號每分鐘五次與每來源每分鐘三十次的限制。配額以 PostgreSQL 的 60 秒滾動視窗計算，重啟會沿用未到期的紀錄。

服務啟動後，六台預載設備會自動正常回報，各自以不同基準每五秒小幅變動，維持在 18～28°C；開頁或登入只讀取資料，不會自動送出控制指令。以 `hq` 登入後，詳情的「展示情境」可切換故障與恢復：按「開始異常回報」立即送出 30°C，按「恢復正常」回到該設備持續小幅變動的正常值。停止只停止產生新測量，畫面仍顯示最新有效溫度；該設備會維持停止，直到恢復回報或服務再次啟動。重複回報會原樣重送最近一筆原始測量。若畫面提示狀態已變更，請確認讀回的新模式，再手動操作。

設備清單與詳情會顯示目前的在線狀態及最近溫度。設備詳情也提供「重新載入設備狀態」。一般展示在開頁時已有自動回報；若設備尚無有效測量，仍依實際狀態顯示「尚未回報」或「等待首筆回報」。背景檢查在距最後有效回報或首筆期待起點超過十五秒後判定離線。離線時保留最後有效溫度與臺灣時間，並標示「資料已非即時」。

登入後選擇「告警歷程」查看目前帳號有權查閱的模擬告警。異常持續中的告警只可查閱。恢復後可選取該筆告警、輸入 1～2,000 個 Unicode 字元的非空白處理結果，再按「結案」。已結案的處理人、臺灣時間及結果為唯讀。若操作結果未確認，請先讀回該筆告警再決定是否重試。

登入後，模擬設備和告警畫面會透過 SSE 自動讀回已提交的變更；官方資料另以每分鐘輪詢同步，不由模擬 SSE 觸發。畫面顯示即時連線及同步狀態。斷線期間可辨識資料可能過時。連線恢復後，系統會重新取得目前畫面的快照。需要時可用「重新同步」立即再試。

要停止服務，請在啟動時使用的專案根目錄與 Docker 環境執行下列指令。指令先核對兩個容器的工作目錄、設定檔、專案與服務標籤，再用核對過的 ID 依序停止應用與資料庫。

```sh
(
  set -eu
  if [ -n "${DOCKER_CONTEXT:-}" ] || [ -z "${DOCKER_HOST:-}" ]; then
    unset DOCKER_HOST
    context=$(docker context show)
    endpoint=$(docker context inspect "$context" --format '{{.Endpoints.docker.Host}}')
  else
    endpoint=$DOCKER_HOST
  fi
  case "$endpoint" in
    unix:///?*) ;;
    *) printf '%s\n' '需要與啟動時相同的本機 Docker Unix socket。' >&2; exit 1 ;;
  esac
  unset DOCKER_CONTEXT DOCKER_HOST
  root=$(pwd -P)
  project=iot-monitoring
  owned_id() {
    service=$1
    inspected=$(docker --host "$endpoint" container inspect --format \
      '{{.Id}}|{{index .Config.Labels "com.docker.compose.project.working_dir"}}|{{index .Config.Labels "com.docker.compose.project.config_files"}}|{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}' \
      "$project-$service-1") || return 1
    if [ "${inspected#*|}" != "$root|$root/compose.yaml|$project|$service" ]; then
      printf '%s\n' 'Compose 容器不屬於目前的工作目錄。' >&2
      return 1
    fi
    printf '%s\n' "${inspected%%|*}"
  }
  app_id=$(owned_id app)
  db_id=$(owned_id db)
  docker --host "$endpoint" container stop "$app_id"
  docker --host "$endpoint" container stop "$db_id"
)
```

用 `./bin/start` 再啟動。PostgreSQL 資料保存在本專案的 `data` 資料卷，`.local/` 保存在本機。請一併保留並妥善保管憑證。若要查閱狀態或日誌，使用同一個 Docker 端點和已核對的容器 ID 執行 `docker container inspect` 或 `docker container logs`。

## 展示流程

1. 啟動服務並以 `hq` 登入，選擇「模擬展示」（或直接開啟 `/?view=simulation`）。確認畫面預選「北區設備 01」，六台設備已有不同正常值且每五秒小幅變動。按「開始異常回報」，確認設備在線且最新有效溫度為 30°C。到「告警歷程」找到該設備本次的「溫度異常」，記下編號並確認「異常持續中」。
2. 回到同一台設備按「停止回報」。等畫面顯示「離線」及「資料已非即時」，最後有效溫度仍為 30°C。再按「開始異常回報」，確認恢復在線，且同一編號的溫度告警仍為「異常持續中」。
3. 回到設備按「恢復正常」，確認溫度回到 18～28°C 並持續小幅變動。回到「告警歷程」開啟剛才記下的告警，確認「已恢復、待結案」。輸入展示用處理結果並按「結案」，確認畫面顯示「已結案」和原文。
4. 依上方指令停止並再啟動服務。重新載入模擬展示頁，確認六台設備自動恢復正常回報，且告警歷程仍可讀回同一筆結案和處理結果。若停機已超過回報期限，歷程應保留重啟補判的離線與其後有效回報的恢復紀錄。
5. 若這份資料可清除，再依下節執行重設。確認舊頁面要求重新登入。用原密碼登入後，六台設備已重新自動正常回報，告警歷程不再包含重設前紀錄。

## 本機展示重設

先以 `./bin/start` 建置包含重設功能的應用，並讓 PostgreSQL 維持執行。要清除本案的展示資料時，在專案根目錄執行下列指令。

```sh
./bin/reset
```

指令先核對本機 Docker、本案容器、資料卷、資料庫和原有憑證。確認顯示的工作目錄及資料庫後，輸入 `RESET iot-monitoring`。取消或輸入其他文字會保留原資料。

確認後，指令停止本案應用，清除測量、告警、結案、模擬與控制歷程、登入及限流資料，再用原有憑證重建預載帳號、場域與設備。沒有備份時，清除的資料無法復原。資料卷和 `.local/` 會保留。

重設提交後，指令會啟動同一個應用容器。舊回報失效，六台預載設備使用新資料世代自動正常回報；原有瀏覽器頁面會在重新連線確認舊登入失效後清除受保護內容。請用原有示範密碼重新登入。若交易失敗，資料會回滾。若指令回報無法確認結果，應用會保持停止，請先查驗資料庫。若已回報資料重設成功但應用未啟動，可執行 `./bin/start` 恢復服務。

啟動和已確認的重設會共用本機維護鎖。若程序遭強制終止並留下 `.local/.maintenance-lock`，先確認本案沒有啟動或重設程序執行，再用 `rmdir .local/.maintenance-lock` 移除空目錄。

## 架構與開發

- Go 1.27.3／Gin 1.12.0 提供 API 和 Vue 靜態檔案。伺服器依 session 決定場域，並使用 PostgreSQL 啟動鎖限制為單一實例。
- Vue 3.5.45／TypeScript 6.0.3／Vite 8.3.6 提供同來源的登入、設備查詢及告警歷程介面。瀏覽器透過限定於 `iot.localhost` 主機的 `HttpOnly`、`SameSite=Strict` Cookie 使用伺服器端 session。
- PostgreSQL 18.6 保存帳號雜湊、場域、設備、session、每台設備的最新測量、溫度與離線告警、結案紀錄及模擬控制歷程。Compose 只將網站發布到 `127.0.0.1`，資料庫留在 Compose 網路。這套 HTTP 設定供本機展示使用。

前端依賴及映像均已固定於 lockfile、`Dockerfile` 與 `compose.yaml`。本機開發檢查可使用 Go 1.27.3 和 Node.js 24.22.2。

```sh
(cd backend && go test ./... && go vet ./... && go build -o /dev/null .)
(cd frontend && npm ci && npm run typecheck && npm run build)
```

## 官方資料 API 與更新界線

官方資料來自[臺中市環境感測資料集](https://data.gov.tw/dataset/152428)，採政府資料開放授權條款第 1 版，無需 API key。固定 JSON 來源為 <https://newdatacenter.taichung.gov.tw/api/v1/no-auth/resource.download?rid=5a16b622-95ca-4e80-9d9f-e62a42962f4e>。來源描述每分鐘量測、資料集約每十分鐘更新；本機不控制來源發布時間。

已登入的瀏覽器透過同源 `GET /api/taichung/sensors` 讀取，無 query 時隨機抽選最多六台有效且不重複設備。`?ids=ID1,ID2` 可指定一至六個唯一非空 ID，依指定順序回傳仍存在的設備；消失設備不另外補抽。回應欄位為：

```json
{"items":[{"id":"001","device":"環境感測器一","name":"西屯區 · 市政公園","district":"西屯區","landmark":"市政公園","temperature":23.4,"humidity":65,"pm25":12.5,"measuredAt":"2026-11-19T02:44:47Z"}],"total":1,"fetchedAt":"2026-11-19T03:00:00Z","sourceUrl":"https://newdatacenter.taichung.gov.tw/api/v1/no-auth/resource.download?rid=5a16b622-95ca-4e80-9d9f-e62a42962f4e"}
```

以上為欄位示例。`total` 是來源全部有效唯一設備數，`items` 可能不足六台或為空。`humidity`、`pm25` 沒有有效值時為 `null`；`temperature` 必須是有限數值。每筆名稱、數值和 `measuredAt` 都來自同一筆觀測。`measuredAt` 保留來源量測時間，`fetchedAt` 是後端成功取得整份來源快照的時間；API 以 UTC RFC3339 回傳，畫面以臺灣時間顯示。下載成功不代表設備在線，官方資料不套用 18～28°C、十五秒離線或本機模擬告警規則。

後端只讀取固定官方 HTTPS 來源，不接受使用者指定上游網址；成功快照共用快取十分鐘，來源失敗短暫節流五秒。瀏覽器每分鐘以目前 ID 同步；手動重抽會重新抽樣，但快取和來源更新頻率可能使數值與時間維持不變。來源失敗時 API 回 `503 service_unavailable`，不以過期快取當成成功；頁面保留上次成功快照時會顯示警示與重試入口。未登入回 `401 unauthenticated`，不合法、重複或未知 query 回 `400 invalid_input`。沿用既有 Host、session 與 Cookie 規則，瀏覽器不直接呼叫外部來源。

原有完整效能測試驗證可控制的模擬管線；其五秒回報及兩秒畫面門檻不能當成官方來源的新鮮度或效能保證。本次官方介面的實測須另外核對當次來源時間、快取與網路結果。

## 模擬溫度與告警 API

在後端以 `SubmitMeasurement(ctx, Measurement)` 送入應用內模擬器回報。回報須符合設備目前允許的資料世代和執行識別，並通過順序與生成時間檢查。抵達與採用前的回報年齡均須在五秒內，包含恰好五秒。設備的最新測量與告警在同一筆交易提交，方法以 `(adopted, err)` 區分已採用、略過與資料庫錯誤。

服務啟動後先停止舊模擬、撤銷舊執行並清除可重送的原始測量，再沿用最後有效回報或首筆期待時間補判停機期間的離線，最後自動啟動六台預載設備的正常回報。新的有效測量仍經共同採用入口提交，按既有規則更新溫度、在線及告警恢復；不刪除歷史，也不自動結案。最新有效測量、首筆期待時間、告警與控制歷程仍持續保存在資料庫，最新值只在有效新回報採用後更新。自動產生器限定六台預載設備，不另外啟動效能測試的 100 台夾具。

總部可用 `GET /api/devices/:id/simulation` 讀取 `mode`、`version` 與 `canReplay`。寫入同一路徑時，沿用登入 Cookie，並帶上與網站相同的 `Origin` 和 CSRF 標頭。`version` 必須使用這次 GET 回傳的十進位字串，不可假設自動啟動後仍為 `"0"`。下例僅示意當次讀回 `"1"`；實際操作請使用讀回的值。`action` 可用 `normal`、`abnormal`、`stop` 或 `replay`。

```http
POST /api/devices/north-01/simulation
Origin: http://iot.localhost:18241
X-CSRF-Protection: 1
Content-Type: application/json

{"action":"normal","version":"1"}
```

成功時回傳 `200` 與更新後的模擬狀態。控制成功表示指令已提交，設備 API 顯示已採用的溫度。`409 conflict` 表示版本已變更，請先重新讀取狀態。`409 no_measurement` 表示目前沒有可重送的原始測量。

登入後可透過 `GET /api/devices` 和 `GET /api/devices/:id` 讀取已採用的 `latestMeasurement`，其中 `receivedAt` 是入列前的抵達時間。`GET /api/alerts` 提供告警清單，`GET /api/alerts/:id` 讀取單筆告警。場域帳號只能讀取本場域，總部可跨場域查閱。清單以 `limit` 和 `offset` 分頁，預設 50 筆，最多 100 筆。

設備的 `onlineStatus` 為 `unreported`、`awaiting`、`online` 或 `offline`。離線告警的 `deadlineAt` 記錄回報期限，`detectedAt` 記錄服務何時發現，`detectedAtRestart` 標示是否在重啟時補判。服務在停機期間無法即時偵測設備。有效回報恢復在線時，溫度告警仍依該筆溫度獨立判定。

低於 18°C 或高於 28°C 會建立一筆溫度告警。同設備持續超標沿用該筆，收到 18～28°C 回報後標記恢復。恢復後再超標會建立新告警。

告警歷程依 ID 倒序分頁，狀態為 `active`、`recovered` 或 `closed`。已恢復的告警可用 `POST /api/alerts/:id/close` 提交 `{"result":"處理結果"}`，沿用登入 Cookie、同源 Origin 與 CSRF 標頭。成功回傳 `200` 及完整告警，新增 `closure` 欄位內的 `actor`、UTC `closedAt` 和純文字 `result`。

`409 alert_conflict` 表示該筆仍在異常中或已有結案。其他場域的 ID 與不存在的 ID 都回傳 `404 not_found`。資料庫無法提交時回傳 `503 service_unavailable`。結案不影響同設備另一筆異常，也不覆寫既有結案結果。

## 即時更新

`GET /api/events` 使用同源 session Cookie。連線建立後，伺服器送出目前可見範圍的版本，後續只在該範圍有成功提交的變更時推進版本。總部涵蓋全部場域，場域帳號只收到自己場域的通知。

```text
event: version
data: {"version":"12","viewer":"<session-view>","expiresAt":"2026-01-01T00:00:00Z"}
```

版本是十進位字串，只能在同一連線的通知間比較。重新連線先訂閱，再用已授權的設備、告警或模擬端點重讀畫面。若讀取途中收到新版本，讀取完成後再確認較新的狀態。串流只通知版本，告警和測量內容由原查詢端點授權。

登入與身分查詢以 `X-IOT-Session-View` 與 `X-IOT-Session-Expires-At` 標頭回傳 session 識別與絕對期限。瀏覽器會比對事件的 `viewer`，也會在後續請求附上原識別，避免不同頁籤換發 Cookie 後混用身分。識別值只供比對，實際授權仍依 Cookie。即使串流連不上，頁面也會在期限到達時清除受保護內容。

伺服器每 10 秒傳送 `heartbeat`。慢連線只保留最新版本，單次寫入有時限。登出、Cookie 換發或 session 到期會終止相應串流。讀取逾時會顯示資料待更新，可按「重新同步」。

## 端到端測試

準備 Node.js 24.22.2、正在執行的本機 Docker，以及 Chrome 或 Chromium。若瀏覽器安裝於其他位置，可用 `CHROME_EXECUTABLE` 指定執行檔。

```sh
(cd tests && npm ci && npm test -- --test-concurrency=1)
```

原有模擬瀏覽器案例明確使用 `/?view=simulation`，不依賴外部官方資料。測試透過 `./bin/start` 建置和啟動獨立 Compose 專案，以真實 PostgreSQL、HTTP 和 Chrome 驗證登入防護、場域權限、重啟及瀏覽器操作。測試會使用自己的暫存目錄、連接埠和資料卷，並在結束時清理。已在 macOS arm64、OrbStack Docker Engine 29.8.2、Compose 5.5.0 與 Chrome 156.0.8078.82 驗證。

不加 `--race` 的測量整合測試另需本機 Go 1.27.3；兩種模式皆使用 `compose.yaml` 指定的本機 PostgreSQL 映像。它使用可控時鐘呼叫測量介面，透過正式 HTTP 路由驗證提交與回滾。測試使用無外部網路的 PostgreSQL 容器及專用資料卷，並在資料庫重啟後從新的 Go 程序驗證溫度與告警讀回。結束時清理測試容器與資料卷。

```sh
./bin/test-measurements
./bin/test-measurements automatic
./bin/test-measurements simulation
./bin/test-measurements offline
./bin/test-measurements closure
./bin/test-measurements sse
./bin/test-measurements reset
```

各組都可在組別前加 `--race`，例如 `./bin/test-measurements --race automatic`、`./bin/test-measurements --race reset`；不指定組別則執行測量案例。此模式由專用 Docker 測試目標提供 Go、gcc 與 musl headers，測試及重設子程序均啟用 race detector，正式映像不包含測試工具鏈。不加 `--race` 的模式仍需要本機 Go 1.27.3。

## 效能驗收

在已登入桌面的 Mac 上準備本機 Docker、Node.js 24.22.2 與 Chrome，先在 `tests` 執行 `npm ci`，再回到專案根目錄。正式量測需要能同時露出十個 Chrome 視窗的桌面。執行期間保持測試視窗在前景，避免鎖定螢幕或切換工作區。`caffeinate` 只在這次指令執行時保持螢幕與主機清醒。

```sh
caffeinate -di node tests/performance.js
```

效能瀏覽器明確使用 `/?view=simulation`。指令建置隔離的 Compose 專案。應用與 PostgreSQL 各限制在 2 vCPU／2 GiB，啟動後再核對實際容器配額。負載由同一應用程序內的正式採用入口處理。100 台設備各每五秒回報一次，錯開為每秒 20 筆，暖機 60 秒後量測 300 秒。離線、恢復觀看及無效回報在正式計數後另測。程式會以十個獨立 Chrome 設定檔開啟已登入、可見的原生視窗，以原生捲動把變動的列帶入視窗，逐筆核對文字在未遮蔽區域的繪製時間。

指令會印出證據目錄。以其中的 `report.json` 判讀 `PASS`、`FAIL` 或 `INCOMPLETE`，並查閱逐筆採用、繪製、時基區間、容器配額與主機紀錄。每筆必要顯示以含校時誤差的延遲上界核對兩秒門檻。缺樣、超時、拒絕及漏排程均會保留。只有正式量測通過會以結束碼 0 返回。

效能產生器以單調時鐘的絕對 deadline 主動檢查到期，並透過 `runtime.Gosched()` 讓其他 Go 工作進展；主動等待的 CPU 成本仍計入原應用容器的 2 vCPU／2 GiB 上限。延遲達 50 ms（含）仍判為 MISS，不補送、不提前送出、不挪動後續 deadline；正式 6,000 筆、逐筆兩秒、十個原生可見 Chrome 視窗與容器配額均維持原要求。

每槽事件的 `wakeNs` 記錄逾期檢查點的時刻，`monoLagNs` 記錄該點相對預定時刻的單調時鐘延遲，並非核心 timer 觸發時間。正式量測已移除會干擾後續時序的常駐 FlightRecorder 與 trace 專用流程，不再產生 `performance-scheduler.trace`。`performance-result.json` 的 `diagnostics` 保留 `start`、`firstMissObserved`、`end` 的輕量 cgroup 統計及已完成 GC 次數 `gcCount`，各以 `beginNs`／`endNs` 界定觀測起訖；`firstMissObserved` 是事件 encoder 收到首個 MISS 後的取樣，累積 counter 差值不能指認單槽原因。缺少 `cpu.stat.local` 時明示 `UNAVAILABLE`，不當成零值，也不改變原正式資格要求。既有原始 trace 與失敗證據保留；另一次 PASS 不能證明歷史首輪漏排程根因已修正。

可加 `--probe` 核對隔離堆疊、配額、校時和十個真實視窗是否可用。探查不送出正式負載，即使 `probePassed` 為真，仍回報 `INCOMPLETE`。測試結束後清理本次隔離容器與資料卷，證據目錄保留。

### 已量測的效能結果

v0.1 歷史效能驗收使用 Apple M3（arm64）與 24 GiB 記憶體、OrbStack Docker Engine 29.8.2、Compose 5.5.0、Node.js 24.22.2、Chrome 156.0.8078.82。應用及 PostgreSQL 容器各為 2 vCPU／2 GiB。暖機採用 1,200 筆，300 秒正式區間採用 6,000／6,000 筆。10 個可見視窗的 24,018／24,018 筆必要畫面更新均有逐列證據，無漏排程、缺樣或逾時。另送兩筆無效資料，兩筆均遭拒絕。

| 項目 | 最慢觀測值（秒，向上取至小數第三位） | 門檻 |
| --- | ---: | ---: |
| 溫度顯示 | 0.547 | 2 秒 |
| 溫度告警顯示 | 0.227 | 2 秒 |
| 離線狀態／告警顯示 | 1.782／1.773 | 2 秒 |
| 恢復在線顯示 | 0.223 | 2 秒 |
| 視窗恢復觀看後同步 50 列 | 0.830 | 2 秒 |

正式量測結果為 `PASS`。完成量測後補上的啟動中止保護另以正常原生短測及啟動卡住後中止實測。正式取證、時基校正與判分沿用這次量測結果。可使用上方指令在自己的機器產生完整逐筆報告。

2026-11-19 修正後單次正式驗收為 `PASS`（結束碼 0）：Apple M3／24 GiB、Go 1.27.3、Chrome 156.0.8078.82，實際 Node.js 24.11.1，與上方文件指定的 24.22.2 有版本差異。應用及 PostgreSQL 各維持 2 vCPU／2 GiB，十個原生可見 Chrome 視窗；暖機採用 1,200 筆，正式 6,000／6,000 筆（100 台各 60 筆、計畫槽距 50 ms），全程 7,698 個事件為 7,696 筆採用及兩筆無效拒絕，零漏排程。24,018／24,018 筆必要畫面更新全數通過逐筆兩秒門檻，最大延遲上界向上取至三位為 0.813 秒；恢復觀看的 50 列最大同步延遲為 0.822 秒。整個應用含服務與 pacer 平均使用約 1.21 個 CPU 核心，成本計入原兩核心配額；最大喚醒延遲 46.371717 ms，低於 50 ms，期間仍有一次自身配額節流、累積 29.542 ms。這是本平台單次結果，不代表所有機器保證，也未確證歷史首輪 XNU 細項根因。
