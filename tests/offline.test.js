import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'
import { createStack } from './stack.js'

// 以設備識別組出正式單筆路徑，讓 API 與瀏覽器比對共用同一入口。
const devicePath = id => `/api/devices/${id}`
// 沿用設備路徑加入模擬子路徑，讓讀取版本與寫入控制指向同一設備。
const simulationPath = id => `${devicePath(id)}/simulation`
const offlineTimeoutNs = 15_000_000_000n

// 只把整秒交給 Date.parse，小數秒以 BigInt 保留至奈秒。
function timestampNs(value) {
  const parts = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?Z$/.exec(value)
  assert.ok(parts, '時間須為 UTC RFC3339Nano')
  const [, second, fraction = ''] = parts
  return BigInt(Date.parse(`${second}Z`)) * 1_000_000n + BigInt(fraction.padEnd(9, '0'))
}

// 期限後 1 ns 仍在同一毫秒內，但必須能區分為兩個時間。
test('IoT 票 04：UTC 時間比較保留奈秒', () => {
  assert.equal(timestampNs('1970-01-01T00:00:15Z'), offlineTimeoutNs)
  assert.equal(timestampNs('1970-01-01T00:00:15.123456790Z') - timestampNs('1970-01-01T00:00:15.123456789Z'), 1n)
})

// 先驗證 HTTP 狀態，JSON 解析失敗只回報案例名稱以免顯示回應本文。
async function json(response, status, label) {
  assert.equal(response.status, status, label)
  try {
    return await response.json()
  } catch {
    throw new Error(`${label}：回應不是有效 JSON`)
  }
}

// 以同一個 HTTP 入口發送讀取與控制；只對寫入加上 CSRF 與 JSON 標頭。
function api(stack, cookie, path, { method = 'GET', body } = {}) {
  return stack.send(path, {
    method,
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: {
      ...(cookie ? { Cookie: cookie } : {}),
      ...(method === 'POST' ? {
        Origin: stack.origin,
        'X-CSRF-Protection': '1',
        'Content-Type': 'application/json',
      } : {}),
    },
  })
}

// 讀回正式設備狀態，讓在線判定以 API 對外結果為準。
async function device(stack, cookie, id) {
  return json(await api(stack, cookie, devicePath(id)), 200, `讀取 ${id} 設備狀態`)
}

// 讀回正式模擬狀態，讓控制使用目前版本並可在重啟後核對。
async function simulation(stack, cookie, id) {
  return json(await api(stack, cookie, simulationPath(id)), 200, `讀取 ${id} 模擬狀態`)
}

// 先讀取控制版本再送操作，重現有權限的客戶端操作順序。
async function control(stack, cookie, id, action) {
  const state = await simulation(stack, cookie, id)
  return json(await api(stack, cookie, simulationPath(id), {
    method: 'POST', body: { action, version: state.version },
  }), 200, `${id} ${action} 控制`)
}

// 以最大分頁讀取隔離展示資料，讓告警檢查可在同一份結果核對多台設備。
async function alerts(stack, cookie) {
  return (await json(await api(stack, cookie, '/api/alerts?limit=100'), 200, '讀取告警清單')).items
}

// 從同一份清單篩出設備與告警類型，將重複告警也視為驗收失敗。
function oneAlert(items, id, type) {
  const found = items.filter(item => item.deviceId === id && item.type === type)
  assert.equal(found.length, 1, `${id} 應只有一筆 ${type} 告警`)
  return found[0]
}

// 擷取最後有效回報的溫度、順序與兩種時間，供離線和重啟前後逐欄比對。
function measurementSnapshot(item) {
  assert.ok(item?.receivedAt, '設備應保留最後有效回報')
  return {
    temperature: item.temperature,
    sequence: item.sequence,
    generatedAt: item.generatedAt,
    receivedAt: item.receivedAt,
  }
}

// 遮蔽已知密碼、登入識別及 Cookie 字串，再限制可輸出的瀏覽器診斷長度。
function safeDiagnostic(value, secrets) {
  // 逐一替換測試持有的敏感值，避免瀏覽器錯誤直接帶出憑證。
  return secrets
    .reduce((text, secret) => text.replaceAll(secret, '[redacted]'), value)
    .replace(/iot_session=[A-Za-z0-9_-]+/g, 'iot_session=[redacted]')
    .slice(0, 300)
}

// 分開驗證回報期限、實際發現時間及補判標記，避免混用三者。
function assertOfflineDeadline(alert, baselineNs, atRestart) {
  assert.equal(alert.status, 'active', '離線告警應持續中')
  assert.equal(alert.recoveredAt, null, '離線告警尚未恢復')
  assert.equal(alert.detectedAtRestart, atRestart, '離線告警須標記實際發現方式')
  // 用原始 UTC 時間換算奈秒，確保比較可區分恰好 15 秒與超過期限。
  const deadline = timestampNs(alert.deadlineAt)
  assert.equal(deadline, baselineNs + offlineTimeoutNs,
    '離線期限須以最後有效回報或首筆期待時間加 15 秒計算')
  assert.ok(timestampNs(alert.detectedAt) > deadline,
    '發現時間須晚於回報期限，不能冒充實際斷線時間')
  assert.equal(alert.temperature, undefined, '離線告警不得帶入溫度告警資料')
}

// 透過設備與告警的正式讀取入口輪詢，等背景檢查將兩個結果都更新。
async function waitForOffline(stack, cookie, id, timeoutMs = 22_000) {
  const until = Date.now() + timeoutMs
  while (Date.now() < until) {
    const [item, allAlerts] = await Promise.all([device(stack, cookie, id), alerts(stack, cookie)])
    // 只取該設備的離線告警，必須恰有一筆才完成等待。
    const offline = allAlerts.filter(alert => alert.deviceId === id && alert.type === 'offline')
    if (item.onlineStatus === 'offline' && offline.length === 1) return { item, alert: offline[0] }
    // 短暫讓出時間給背景檢查，之後再讀取兩個結果。
    await new Promise(resolve => setTimeout(resolve, 500))
  }
  throw new Error(`${id} 未在期限內進入離線並建立告警`)
}

// 以三台設備區分即時發現、停機期間逾時及首筆未到，再驗證原告警恢復。
test('IoT 票 04：離線狀態、重啟補判與回報恢復', { timeout: 600_000 }, async t => {
  const stack = await createStack()
  // 啟動前先登記清理，以回收本案資料庫、應用程式及暫存資料。
  t.after(() => stack.cleanup())
  stack.start()

  // 先經正式 API 取得總部登入，讓控制、告警讀取及後續瀏覽器共用這筆 session。
  const passwords = JSON.parse(readFileSync(join(stack.state, 'demo-passwords.json'), 'utf8'))
  const loginResponse = await api(stack, undefined, '/api/login', {
    method: 'POST', body: { username: 'hq', password: passwords.hq },
  })
  const login = await json(loginResponse, 200, '總部登入')
  assert.deepEqual(login, { username: 'hq', role: 'headquarters', siteId: null }, '登入須取得總部身分')
  const cookies = loginResponse.headers.getSetCookie()
  assert.equal(cookies.length, 1, '登入須設定一個 session Cookie')
  const cookie = cookies[0].split('; ', 1)[0]
  assert.ok(/^iot_session=[A-Za-z0-9_-]{43}$/.test(cookie), '登入須設定有效 session Cookie')
  const hqCookie = cookie
  const diagnosticSecrets = [...Object.values(passwords), hqCookie.slice('iot_session='.length)]
  assert.deepEqual(await json(await api(stack, hqCookie, '/api/session'), 200, '確認總部 session'), login,
    '伺服器須接受剛建立的登入')

  // 優先採用指定的 Chrome，未指定時從本機常見路徑查找。
  const executablePath = process.env.CHROME_EXECUTABLE ?? [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/google-chrome', '/usr/bin/chromium',
  ].find(path => existsSync(path))
  assert.ok(executablePath && existsSync(executablePath), '請安裝 Chrome／Chromium 或設定 CHROME_EXECUTABLE')
  // 關閉可能記錄憑證或瀏覽器通訊的 Playwright 診斷。
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  const { chromium } = await import('playwright-core')
  const browser = await chromium.launch({ executablePath, timeout: 15_000 })
  const consoleErrors = []
  let favicon404 = 0

  try {
    // 將既有登入放進獨立的瀏覽器環境，讓此案例專注於設備狀態與手動刷新。
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
    await context.addCookies([{
      name: 'iot_session', value: hqCookie.slice('iot_session='.length), url: stack.origin,
      httpOnly: true, sameSite: 'Strict',
    }])
    const page = await context.newPage()
    page.setDefaultTimeout(8_000)
    // 只略過一次預期的 favicon 404，其餘 console 錯誤經遮蔽後保留。
    page.on('console', message => {
      if (message.type() !== 'error') return
      if (
        message.location().url === stack.origin + '/favicon.ico'
        && message.text() === 'Failed to load resource: the server responded with a status of 404 (Not Found)'
        && favicon404 === 0
      ) {
        favicon404 += 1
      } else {
        consoleErrors.push(safeDiagnostic(`${message.text()} @ ${message.location().url}`, diagnosticSecrets))
      }
    })
    // 未攔截的頁面例外也經同一遮蔽流程收集，供測試結尾統一驗證。
    page.on('pageerror', error => consoleErrors.push(safeDiagnostic(`${error.name}: ${error.message}`, diagnosticSecrets)))
    // 先確認詳情顯示尚未回報，避免用已有測量的畫面當初始條件。
    await page.goto(stack.origin, { timeout: 10_000 })
    await page.getByRole('heading', { name: '設備清單', level: 2, exact: true }).waitFor()
    await page.getByRole('button', { name: /北區設備 01/ }).click()
    await page.getByText('north-01', { exact: true }).waitFor()
    await page.getByText('尚未回報', { exact: true }).waitFor()

    // 檢查實際視窗尺寸、橫向溢位及手動刷新按鈕，避免只比較設定值。
    async function checkViewport(width, height) {
      // 讀取瀏覽器實際視窗及文件尺寸，作為畫面排列的驗證依據。
      const actual = await page.evaluate(() => ({
        width: window.innerWidth,
        height: window.innerHeight,
        documentWidth: document.documentElement.scrollWidth,
      }))
      assert.deepEqual(actual, { width, height, documentWidth: width }, `設備詳細頁在 ${width}px 應正常排列`)
      assert.equal(await page.getByRole('button', { name: '重新載入設備狀態', exact: true }).isVisible(), true,
        `視窗 ${width}px 時須能手動重新載入設備狀態`)
    }

    // 同一頁分別以桌面與手機尺寸驗證，讓後續流程也經過窄版畫面。
    await checkViewport(1280, 900)
    await page.setViewportSize({ width: 390, height: 844 })
    await checkViewport(390, 844)

    // 先確認設備未曾回報，再以異常操作建立有效測量及獨立溫度告警。
    const initialDevice = await device(stack, hqCookie, 'north-01')
    const initialSimulation = await simulation(stack, hqCookie, 'north-01')
    assert.equal(initialDevice.onlineStatus, 'unreported', '新設備尚未開始回報')
    assert.equal(initialSimulation.mode, 'stopped', '新設備模擬須保持停止')
    const started = await control(stack, hqCookie, 'north-01', 'abnormal')
    assert.equal(started.mode, 'abnormal', '正常服務須接受總部異常回報控制')
    const firstOnline = await device(stack, hqCookie, 'north-01')
    assert.equal(firstOnline.onlineStatus, 'online', '首筆有效測量須使設備在線')
    assert.equal(firstOnline.latestMeasurement?.temperature, 30, '異常模擬須回報 30°C')
    const temperatureBefore = oneAlert(await alerts(stack, hqCookie), 'north-01', 'temperature')
    assert.equal(temperatureBefore.status, 'active', '超標後溫度告警須持續中')
    assert.equal(temperatureBefore.temperature, 30, '溫度告警須保留原始異常值')

    // 在點擊前監聽指定設備的 GET，將實際刷新回應與畫面時間比對。
    const refreshButton = page.getByRole('button', { name: '重新載入設備狀態', exact: true })
    const firstRefresh = page.waitForResponse(response =>
      response.url() === stack.origin + devicePath('north-01') && response.request().method() === 'GET')
    await refreshButton.click()
    const firstRefreshResponse = await firstRefresh
    assert.equal(firstRefreshResponse.status(), 200, '畫面須重新讀取正式設備 API')
    const firstDisplayedDevice = await firstRefreshResponse.json()
    assert.equal(firstDisplayedDevice.onlineStatus, 'online', '畫面須顯示目前在線狀態')
    assert.equal(firstDisplayedDevice.latestMeasurement?.temperature, 30, '畫面須顯示最新異常溫度')
    await page.getByText('在線', { exact: true }).waitFor()
    await page.getByText('30°C', { exact: true }).waitFor()
    const firstDisplayedReceipt = await page.locator('time[datetime]').getAttribute('datetime')
    assert.equal(firstDisplayedReceipt, firstDisplayedDevice.latestMeasurement.receivedAt,
      '畫面時間屬性須保留 API 的原始回報時間')

    // 停止只切斷後續產出；先於期限內讀回在線狀態，再等背景檢查判為離線。
    const stopped = await control(stack, hqCookie, 'north-01', 'stop')
    assert.equal(stopped.mode, 'stopped', '停止只停止後續生產')
    const afterStop = await device(stack, hqCookie, 'north-01')
    const stoppedMeasurement = measurementSnapshot(afterStop.latestMeasurement)
    const lastReceipt = stoppedMeasurement.receivedAt
    assert.ok(BigInt(Date.now()) * 1_000_000n < timestampNs(lastReceipt) + offlineTimeoutNs,
      '停止控制案例須在最後回報的 15 秒期限內讀回')
    assert.equal(afterStop.onlineStatus, 'online', '停止控制本身不可直接宣告離線')
    // 離線後比對最後測量與獨立溫度告警，確認只新增離線告警。
    const liveOffline = await waitForOffline(stack, hqCookie, 'north-01')
    assert.deepEqual(measurementSnapshot(liveOffline.item.latestMeasurement), stoppedMeasurement,
      '離線後須保留最後溫度與回報時間')
    assertOfflineDeadline(liveOffline.alert, timestampNs(lastReceipt), false)
    const temperatureDuringOffline = oneAlert(await alerts(stack, hqCookie), 'north-01', 'temperature')
    assert.deepEqual(temperatureDuringOffline, temperatureBefore, '離線不能覆蓋或恢復獨立的溫度告警')

    // 手動刷新前保留舊快照，避免頁面自行推算離線或改寫回報時間。
    await page.getByText('在線', { exact: true }).waitFor()
    assert.equal(await page.getByText('資料已非即時', { exact: true }).count(), 0,
      '手動更新前瀏覽器仍顯示先前快照')
    assert.equal(await page.locator('time[datetime]').getAttribute('datetime'), firstDisplayedReceipt,
      '手動更新前瀏覽器不可自行改寫回報時間')
    // 監聽指定設備的刷新請求，確認讀回離線時仍顯示原溫度與回報時間。
    const offlineRefresh = page.waitForResponse(response =>
      response.url() === stack.origin + devicePath('north-01') && response.request().method() === 'GET')
    await refreshButton.click()
    const offlineRefreshResponse = await offlineRefresh
    assert.equal(offlineRefreshResponse.status(), 200, '離線後手動刷新須讀取設備 API')
    const offlineDisplayedDevice = await offlineRefreshResponse.json()
    await page.getByText('離線', { exact: true }).waitFor()
    await page.getByText('資料已非即時', { exact: true }).waitFor()
    await page.getByText('30°C', { exact: true }).waitFor()
    assert.equal(offlineDisplayedDevice.latestMeasurement?.receivedAt, lastReceipt,
      '離線 API 須保留最後一筆回報時間')
    assert.equal(await page.locator('time[datetime]').getAttribute('datetime'), lastReceipt,
      '離線畫面仍顯示最後有效回報時間')

    // 第二台先收到有效回報再停止，建立能在停機期間到期的基準。
    const secondStarted = await control(stack, hqCookie, 'north-02', 'abnormal')
    assert.equal(secondStarted.mode, 'abnormal', '重啟案例須先開始另一台設備回報')
    const secondStopped = await control(stack, hqCookie, 'north-02', 'stop')
    assert.equal(secondStopped.mode, 'stopped', '取得重啟基準前須先停止第二台設備回報')
    const secondBeforeRestart = await device(stack, hqCookie, 'north-02')
    assert.equal(secondBeforeRestart.onlineStatus, 'online', '重啟前第二台設備須有有效回報')
    assert.equal(secondBeforeRestart.latestMeasurement?.temperature, 30, '重啟前第二台設備須保留 30°C')

    // 只在隔離資料庫阻擋第三台的測量寫入，保留正式控制入口建立的首筆期待起點。
    stack.sql("ALTER TABLE latest_measurements ADD CONSTRAINT test_first_report_failure CHECK (device_id <> 'north-03');")
    const thirdStarted = await control(stack, hqCookie, 'north-03', 'normal')
    assert.equal(thirdStarted.mode, 'normal', '重啟前第三台設備須已開始模擬')
    const thirdBeforeRestart = await device(stack, hqCookie, 'north-03')
    assert.equal(thirdBeforeRestart.onlineStatus, 'awaiting', '重啟前第三台設備須仍等待首筆回報')
    assert.equal(thirdBeforeRestart.latestMeasurement, undefined, '第三台設備的首筆回報尚未成功寫入')
    // 直接讀取已持久化的首筆期待奈秒值，供重啟前後逐值比對。
    const readFirstExpected = () => stack.sql("SELECT first_expected_at_ns FROM device_simulations WHERE device_id = 'north-03';")
    const firstExpectedBeforeRestart = readFirstExpected()
    assert.match(firstExpectedBeforeRestart, /^[1-9]\d*$/, '首筆期待起點須已持久保存為整數奈秒')

    stack.stop()
    // 停止容器後實際經過離線期限，再由正式啟動掃描補判。
    await new Promise(resolve => setTimeout(resolve, 15_250))
    stack.start()
    assert.deepEqual(await json(await api(stack, hqCookie, '/api/session'), 200, '服務重啟後 HQ session'), login,
      '服務重啟後原總部 session 仍有效')

    // 重啟後從正式 API 一次讀取三台設備與告警，比對各自的起點及模擬終態。
    const [northOneAfterRestart, northTwoAfterRestart, northThreeAfterRestart,
      northOneSimulation, northTwoSimulation, northThreeSimulation, alertsAfterRestart] = await Promise.all([
      device(stack, hqCookie, 'north-01'),
      device(stack, hqCookie, 'north-02'),
      device(stack, hqCookie, 'north-03'),
      simulation(stack, hqCookie, 'north-01'),
      simulation(stack, hqCookie, 'north-02'),
      simulation(stack, hqCookie, 'north-03'),
      alerts(stack, hqCookie),
    ])
    assert.equal(northOneAfterRestart.onlineStatus, 'offline', '既有離線狀態須跨服務重啟保存')
    assert.equal(northTwoAfterRestart.onlineStatus, 'offline', '服務停止期間超時的設備須在啟動時離線')
    assert.equal(northThreeAfterRestart.onlineStatus, 'offline', '停機時超過首筆期待期限的設備須在啟動時離線')
    assert.equal(northThreeAfterRestart.latestMeasurement, undefined, '啟動補判不可為第三台設備補造測量')
    assert.equal(readFirstExpected(), firstExpectedBeforeRestart, '重啟須保存原首筆期待起點')
    assert.deepEqual(measurementSnapshot(northOneAfterRestart.latestMeasurement), measurementSnapshot(liveOffline.item.latestMeasurement),
      '重啟須保留既有離線設備的原始測量')
    assert.deepEqual(measurementSnapshot(northTwoAfterRestart.latestMeasurement), measurementSnapshot(secondBeforeRestart.latestMeasurement),
      '啟動補判須保留第二台設備最後有效測量')
    // 三種情境都須停止模擬並撤銷舊執行的重送資格。
    assert.deepEqual([northOneSimulation.mode, northOneSimulation.canReplay], ['stopped', false],
      '重啟後既有模擬須停止且舊執行不可重送')
    assert.deepEqual([northTwoSimulation.mode, northTwoSimulation.canReplay], ['stopped', false],
      '重啟後新離線設備的模擬須停止且舊執行不可重送')
    assert.deepEqual([northThreeSimulation.mode, northThreeSimulation.canReplay], ['stopped', false],
      '重啟須停止第三台設備原本仍執行中的模擬')

    // 原有告警須原樣保留，另兩台依各自基準建立標為重啟補判的告警。
    const northOneOfflineAfterRestart = oneAlert(alertsAfterRestart, 'north-01', 'offline')
    assert.deepEqual(northOneOfflineAfterRestart, liveOffline.alert, '既有離線告警 ID 與補判標記須原樣保留')
    const northTwoOfflineAfterRestart = oneAlert(alertsAfterRestart, 'north-02', 'offline')
    assertOfflineDeadline(northTwoOfflineAfterRestart, timestampNs(secondBeforeRestart.latestMeasurement.receivedAt), true)
    const northThreeOfflineAfterRestart = oneAlert(alertsAfterRestart, 'north-03', 'offline')
    assertOfflineDeadline(northThreeOfflineAfterRestart, BigInt(firstExpectedBeforeRestart), true)
    // 完成補判驗證後移除測量失敗條件，再確認既有溫度告警仍在。
    stack.sql('ALTER TABLE latest_measurements DROP CONSTRAINT test_first_report_failure;')
    const temperatureAfterRestart = oneAlert(alertsAfterRestart, 'north-01', 'temperature')
    assert.deepEqual(temperatureAfterRestart, temperatureBefore, '服務重啟不得清除仍持續的溫度告警')

    // 以最新控制版本開始新一輪異常回報，驗證在線恢復和溫度異常可同時存在。
    const recoveredRun = await control(stack, hqCookie, 'north-01', 'abnormal')
    assert.equal(recoveredRun.mode, 'abnormal', '重啟後可用目前控制版本開始新的異常回報')
    const recoveredDevice = await device(stack, hqCookie, 'north-01')
    assert.equal(recoveredDevice.onlineStatus, 'online', '新有效回報須恢復在線')
    assert.equal(recoveredDevice.latestMeasurement?.temperature, 30, '恢復回報仍須保留異常溫度')
    assert.ok(timestampNs(recoveredDevice.latestMeasurement.receivedAt) >
      timestampNs(liveOffline.item.latestMeasurement.receivedAt),
      '恢復後必須有更新的有效回報時間')
    // 恢復應更新原離線告警的狀態與恢復時間，保留期限和發現方式。
    const recoveredAlerts = await alerts(stack, hqCookie)
    const recoveredOffline = oneAlert(recoveredAlerts, 'north-01', 'offline')
    assert.equal(recoveredOffline.id, liveOffline.alert.id, '恢復須更新原本的離線告警')
    assert.equal(recoveredOffline.status, 'recovered', '新有效回報須恢復離線告警')
    assert.ok(recoveredOffline.recoveredAt, '離線恢復須保存恢復時間')
    assert.equal(recoveredOffline.detectedAtRestart, false, '恢復不得改寫原本即時發現標記')
    assert.equal(recoveredOffline.deadlineAt, liveOffline.alert.deadlineAt, '恢復不得改寫原回報期限')
    assert.deepEqual(oneAlert(recoveredAlerts, 'north-01', 'temperature'), temperatureBefore,
      '異常回報恢復在線時，溫度告警仍持續中')

    // 恢復後仍須等手動讀取才更新瀏覽器快照，並以新 API 回報時間核對畫面。
    await page.getByText('離線', { exact: true }).waitFor()
    await page.getByText('資料已非即時', { exact: true }).waitFor()
    assert.equal(await page.locator('time[datetime]').getAttribute('datetime'),
      liveOffline.item.latestMeasurement.receivedAt,
      '恢復資料尚未手動重新載入時仍保留舊畫面')
    // 監聽本次設備 GET，區分新快照與瀏覽器保留的舊回報時間。
    const recoveryRefresh = page.waitForResponse(response =>
      response.url() === stack.origin + devicePath('north-01') && response.request().method() === 'GET')
    await refreshButton.click()
    const recoveryRefreshResponse = await recoveryRefresh
    assert.equal(recoveryRefreshResponse.status(), 200, '恢復後手動刷新須讀取設備 API')
    const recoveredDisplayedDevice = await recoveryRefreshResponse.json()
    await page.getByText('在線', { exact: true }).waitFor()
    await page.getByText('30°C', { exact: true }).waitFor()
    assert.equal(await page.getByText('資料已非即時', { exact: true }).count(), 0,
      '恢復在線並刷新後須清除非即時提示')
    assert.equal(await page.locator('time[datetime]').getAttribute('datetime'), recoveredDisplayedDevice.latestMeasurement.receivedAt,
      '刷新後回報時間須與這次刷新的 API 資料相同')
    assert.notEqual(await page.locator('time[datetime]').getAttribute('datetime'),
      liveOffline.item.latestMeasurement.receivedAt,
      '刷新後畫面須顯示更新的回報時間')

    // 驗收結尾檢查實際頁面與遮蔽後的錯誤清單，讓建置或腳本問題也能使測試失敗。
    assert.equal(await page.locator('vite-error-overlay, #webpack-dev-server-client-overlay').count(), 0,
      '畫面不可出現前端建置錯誤覆蓋層')
    assert.deepEqual(consoleErrors, [], `瀏覽器不可有 JavaScript 或未預期的 console error：${consoleErrors.join(' | ')}`)
  } finally {
    // 任何斷言失敗都關閉瀏覽器，再由外層清理隔離專案。
    await browser.close()
  }
})
