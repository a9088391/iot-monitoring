import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'
import { setTimeout } from 'node:timers/promises'
import { createStack } from './stack.js'

const simulationPath = '/api/devices/north-01/simulation'
const controlLabels = ['開始正常回報', '開始異常回報', '停止回報', '重複回報']
const loginFormError = '瀏覽器登入表單操作失敗'

// 正規化 Node 與 Playwright 回應的狀態介面，解析失敗只保留驗收階段。
async function responseJson(response, status, label) {
  assert.equal(typeof response.status === 'function' ? response.status() : response.status, status, label)
  try {
    return await response.json()
  } catch {
    throw new Error(`${label}：回應不是有效 JSON`)
  }
}

// 瀏覽器錯誤進入測試報告前先遮蔽密碼與 Cookie，並限制輸出長度。
function redact(message, passwords) {
  // 依測試已知的密碼逐一替換，避免把原始瀏覽器訊息直接輸出。
  return Object.values(passwords)
    .reduce((text, password) => text.replaceAll(password, '[redacted]'), message)
    .replace(/iot_session=[A-Za-z0-9_-]+/g, 'iot_session=[redacted]')
    .slice(0, 300)
}

// 收集頁面例外與 console 錯誤，只有符合指定條件的資源回應可略過。
function monitorBrowserErrors(page, passwords, errors, origin, expectedResources, isInjectedUnknown) {
  // 以 URL、訊息、注入狀態及次數限縮例外，避免吞掉意外的 401、409 或 503。
  page.on('console', message => {
    if (message.type() !== 'error') return
    const url = message.location().url
    // 只採用仍有剩餘次數、且符合目前注入情境的預期項目。
    const expected = expectedResources.find(resource =>
      resource.url === url
      && resource.message === message.text()
      && (
        !resource.injectedOnly
        || isInjectedUnknown()
      )
      && resource.count < resource.maximum)
    if (expected) {
      expected.count += 1
      return
    }
    errors.push(`console: ${redact(`${message.text()} @ ${url || origin}`, passwords)}`)
  })
  // 對未攔截的腳本例外也做遮蔽，保留類型與訊息供結尾檢查。
  page.on('pageerror', error => errors.push(`page: ${redact(`${error.name}: ${error.message}`, passwords)}`))
}

// 以實際表單和 Enter 驗證登入，失敗時用固定訊息避免洩漏密碼。
async function signIn(page, username, password, origin) {
  const account = page.getByLabel('帳號', { exact: true })
  const secret = page.getByLabel('密碼', { exact: true })
  let response
  try {
    // 驗證鍵盤焦點仍在密碼欄位，並在按 Enter 前先監聽登入請求。
    await account.fill(username)
    await secret.fill(password)
    // 在瀏覽器內比對焦點元素，確保 Enter 會由密碼欄位送出。
    assert.equal(await secret.evaluate(element => document.activeElement === element), true, '密碼欄位須保有鍵盤焦點')
    const [loginResponse] = await Promise.all([
      // 只等待正式登入端點的 POST，排除其他頁面請求。
      page.waitForResponse(item => item.url() === `${origin}/api/login` && item.request().method() === 'POST'),
      secret.press('Enter'),
    ])
    response = loginResponse
  } catch {
    // Playwright 失敗時會附上輸入值，不保留原始例外。
    throw new Error(loginFormError)
  }
  // 登入成功須同時收到 HTTP 回應並進入設備清單。
  await responseJson(response, 200, '登入')
  await page.getByRole('heading', { name: '設備清單', level: 2, exact: true }).waitFor()
}

// 以真實畫面與服務驗證模擬控制、版本衝突、結果不明後的復原及重啟。
test('IoT 票 03：真實瀏覽器模擬控制與衝突復原', { timeout: 600_000 }, async t => {
  const stack = await createStack()
  // 啟動前登記專案清理，讓後續瀏覽器或 HTTP 斷言失敗時仍會回收容器。
  t.after(() => stack.cleanup())
  stack.start()

  // 讀取這次啟動產生的憑證；Chrome 優先採指定路徑，再查本機安裝。
  const passwords = JSON.parse(readFileSync(join(stack.state, 'demo-passwords.json'), 'utf8'))
  // 路徑探測只選擇存在的執行檔，沿用本機安裝的瀏覽器。
  const executablePath = process.env.CHROME_EXECUTABLE ?? [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/google-chrome', '/usr/bin/chromium',
  ].find(path => existsSync(path))
  assert.ok(executablePath && existsSync(executablePath), '請安裝 Chrome／Chromium 或設定 CHROME_EXECUTABLE')
  // 停用會記錄輸入或瀏覽器通訊的 Playwright 診斷。
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  const { chromium } = await import('playwright-core')
  const browser = await chromium.launch({ executablePath, timeout: 15_000 })
  const browserErrors = []
  let injectedUnknownResponse = false
  // 只容許預期次數的資源錯誤，其中合成 503 必須發生在注入情境。
  const expectedResources = [
    {
      url: stack.origin + '/api/session',
      message: 'Failed to load resource: the server responded with a status of 401 (Unauthorized)',
      count: 0,
      maximum: 2,
    },
    {
      url: stack.origin + '/favicon.ico',
      message: 'Failed to load resource: the server responded with a status of 404 (Not Found)',
      count: 0,
      maximum: 1,
    },
    {
      url: stack.origin + simulationPath,
      message: 'Failed to load resource: the server responded with a status of 409 (Conflict)',
      count: 0,
      maximum: 1,
    },
    {
      url: stack.origin + simulationPath,
      message: 'Failed to load resource: the server responded with a status of 503 (Service Unavailable)',
      count: 0,
      maximum: 1,
      injectedOnly: true,
    },
  ]

  try {
    const hqContext = await browser.newContext({ viewport: { width: 1280, height: 900 } })
    const hqPage = await hqContext.newPage()
    // 讓真正的瀏覽器拒絕填寫密碼欄位，只使用合成值檢查失敗訊息。
    hqPage.setDefaultTimeout(1_000)
    await hqPage.setContent('<label>帳號<input></label><label>密碼<input type="password" disabled></label>')
    const syntheticPassword = 'synthetic-password-marker'
    // 驗證固定錯誤訊息且沒有附帶密碼或原始例外。
    await assert.rejects(signIn(hqPage, 'fixture', syntheticPassword, stack.origin), error => {
      assert.ok(
        error.message === loginFormError
          && !error.message.includes(syntheticPassword)
          && !('cause' in error),
        '登入表單失敗須只回報固定訊息，且不得附上密碼或原始例外'
      )
      return true
    })
    assert.equal(await hqPage.getByLabel('帳號', { exact: true }).inputValue(), 'fixture', '須到達密碼欄位才失敗')
    // 恢復正常操作時限再進入正式頁面，依注入旗標判斷哪些資源錯誤可略過。
    hqPage.setDefaultTimeout(8_000)
    monitorBrowserErrors(hqPage, passwords, browserErrors, stack.origin, expectedResources, () => injectedUnknownResponse)
    await hqPage.goto(stack.origin, { timeout: 10_000 })
    await signIn(hqPage, 'hq', passwords.hq, stack.origin)
    // 取出正式主機的 session，供第二個控制端與操作後讀回共用。
    const hqSession = (await hqContext.cookies(stack.origin)).find(cookie => cookie.name === 'iot_session')
    assert.ok(hqSession, '瀏覽器登入後須持有總部 session')
    const hqCookie = `iot_session=${hqSession.value}`
    // 用同一總部 session 送獨立 API 請求，讓第二個控制端能改版而不更新原畫面。
    async function sendAsHq(path, method = 'GET', body) {
      return stack.send(path, {
        method,
        body: body === undefined ? undefined : JSON.stringify(body),
        headers: {
          Cookie: hqCookie,
          ...(method === 'POST' ? {
            Origin: stack.origin,
            'X-CSRF-Protection': '1',
            'Content-Type': 'application/json',
          } : {}),
        },
      })
    }

    // 進入新設備詳細頁，確認停止狀態與無原始測量時的重送限制。
    await hqPage.getByRole('button', { name: /北區設備 01/ }).click()
    await hqPage.getByRole('heading', { name: '模擬控制', level: 3, exact: true }).waitFor()
    await hqPage.getByText('north-01', { exact: true }).waitFor()
    await hqPage.getByText('尚未回報', { exact: true }).waitFor()
    await hqPage.getByText('模擬模式：已停止', { exact: true }).waitFor()
    const replayButton = hqPage.getByRole('button', { name: '重複回報', exact: true })
    assert.equal(await replayButton.isDisabled(), true, '沒有原始測量時不得啟用重送')
    await hqPage.getByText('尚無可重送的測量。', { exact: true }).waitFor()

    // 在實際瀏覽器驗證指定尺寸、控制按鈕及頁面是否水平溢位。
    async function checkViewport(width, height) {
      // 從頁面讀取實際寬度，區分視窗大小與文件溢位。
      const actual = await hqPage.evaluate(() => ({
        width: window.innerWidth,
        documentWidth: document.documentElement.scrollWidth,
      }))
      assert.equal(actual.width, width, '瀏覽器需使用指定檢查寬度')
      assert.ok(actual.documentWidth <= width, '設備詳細頁不可產生水平溢位')
      for (const label of controlLabels) {
        assert.equal(await hqPage.getByRole('button', { name: label, exact: true }).isVisible(), true,
          `視窗 ${width}px 時控制按鈕須可見`)
      }
      // 讀取實際視窗高度，以確認測試使用指定的瀏覽器尺寸。
      assert.equal(await hqPage.evaluate(() => window.innerHeight), height, '瀏覽器需使用指定檢查高度')
    }

    // 先以桌面再切換手機尺寸，後續控制流程也實際使用窄版。
    await checkViewport(1280, 900)
    await hqPage.setViewportSize({ width: 390, height: 844 })
    await checkViewport(390, 844)

    const pageControls = []
    // 記錄瀏覽器發出的控制操作與版本，讓自動重送或格式錯誤可在後續查出。
    hqPage.on('request', request => {
      if (request.url() === stack.origin + simulationPath && request.method() === 'POST') {
        try {
          const body = request.postDataJSON()
          pageControls.push({ action: body.action, version: body.version })
        } catch {
          pageControls.push({ invalid: true })
        }
      }
    })

    // 以鍵盤送出首次控制，並核對成功回應、模式、溫度及畫面提示。
    const normalButton = hqPage.getByRole('button', { name: '開始正常回報', exact: true })
    await normalButton.focus()
    // 在瀏覽器內確認實際焦點，驗證操作可用鍵盤完成。
    assert.equal(await normalButton.evaluate(element => document.activeElement === element), true, '控制項須可由鍵盤取得焦點')
    const [normalResponse] = await Promise.all([
      // 只監聽模擬控制的 POST，以免將狀態讀取視為控制成功。
      hqPage.waitForResponse(item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST'),
      normalButton.press('Enter'),
    ])
    const normalState = await responseJson(normalResponse, 200, '開始正常回報')
    assert.equal(normalState.mode, 'normal', '開始正常回報須切換至正常模式')
    await hqPage.getByText('模擬模式：正常回報', { exact: true }).waitFor()
    await hqPage.getByText('23°C', { exact: true }).waitFor()
    await hqPage.getByText('已開始正常回報。', { exact: true }).waitFor()

    // 在舊快照已讀取後保留它，直到瀏覽器送出控制，避免即時讀回提前消除版本衝突。
    const priorSnapshot = await responseJson(await sendAsHq(simulationPath), 200, '原模擬快照')
    assert.deepEqual(priorSnapshot, normalState)
    let conflictSent = false
    await hqPage.route(stack.origin + simulationPath, async route => {
      if (route.request().method() === 'POST') conflictSent = true
      if (conflictSent) await route.continue()
      else await route.fulfill({ json: priorSnapshot })
    })
    // 另一個真實請求先寫入新版本，讓舊快照在服務端確實失效。
    const externalResponse = await sendAsHq(simulationPath, 'POST', { action: 'abnormal', version: normalState.version })
    const externalState = await responseJson(externalResponse, 200, '第二個總部控制')
    assert.equal(externalState.mode, 'abnormal', '第二個總部控制須切換至異常模式')
    assert.equal(externalState.canReplay, true, '異常回報須保存可重送的原始測量')
    await hqPage.getByText('模擬模式：正常回報', { exact: true }).waitFor()

    // 再送出畫面持有的版本，驗證衝突後以讀取同步且不自動重送。
    const [conflictResponse] = await Promise.all([
      // 等待這次控制的 POST 回應，再檢查畫面讀回的新模式及溫度。
      hqPage.waitForResponse(item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST'),
      normalButton.click(),
    ])
    await hqPage.unroute(stack.origin + simulationPath)
    assert.equal(conflictResponse.status(), 409, '瀏覽器送出舊版本時須收到衝突')
    await hqPage
      .getByRole('alert')
      .filter({ hasText: '模擬狀態已變更，請確認目前模式後再操作。' })
      .waitFor()
    await hqPage.getByText('模擬模式：異常回報', { exact: true }).waitFor()
    await hqPage.getByText('30°C', { exact: true }).waitFor()
    // 從請求紀錄取出操作名稱，確認衝突後沒有另一筆寫入。
    assert.deepEqual(pageControls.map(control => control.action), ['normal', 'normal'], '衝突控制不得自動重送')
    assert.equal(pageControls[1].version, normalState.version, '衝突請求須保留瀏覽器先前讀到的版本')

    // 手動重送驗證刷新後可繼續操作，且重送維持目前模式與溫度。
    const replayResponsePromise = hqPage.waitForResponse(
      item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST',
    )
    await replayButton.click()
    const replayResponse = await replayResponsePromise
    const replayState = await responseJson(replayResponse, 200, '手動重送')
    assert.equal(replayState.mode, 'abnormal', '重送不得改變目前模擬模式')
    await hqPage.getByText('已重送最近一筆原始測量。', { exact: true }).waitFor()
    await hqPage.getByText('模擬模式：異常回報', { exact: true }).waitFor()
    await hqPage.getByText('30°C', { exact: true }).waitFor()

    // 手動停止後同時檢查畫面與控制次數，確認沒有額外的瀏覽器寫入。
    const stopResponsePromise = hqPage.waitForResponse(
      item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST',
    )
    await hqPage.getByRole('button', { name: '停止回報', exact: true }).click()
    const stopResponse = await stopResponsePromise
    const stoppedState = await responseJson(stopResponse, 200, '停止回報')
    assert.equal(stoppedState.mode, 'stopped', '停止操作須結束模擬回報')
    await hqPage.getByText('已停止模擬回報。', { exact: true }).waitFor()
    await hqPage.getByText('模擬模式：已停止', { exact: true }).waitFor()
    await hqPage.getByText('30°C', { exact: true }).waitFor()
    // 用紀錄中的操作順序核對畫面只送出使用者明確要求的控制。
    assert.deepEqual(pageControls.map(control => control.action), ['normal', 'normal', 'replay', 'stop'],
      '只允許明確操作的瀏覽器控制請求')

    // 從正式 API 讀回停止模式及最近測量，確認停止只停產。
    const finalSimulation = await responseJson(await sendAsHq(simulationPath), 200, '停止後狀態讀回')
    const finalDevice = await responseJson(await sendAsHq('/api/devices/north-01'), 200, '停止後設備讀回')
    assert.equal(finalSimulation.mode, 'stopped', '停止後伺服器狀態須保持停止')
    assert.equal(finalDevice.latestMeasurement?.temperature, 30, '停止只停產，不清除最後有效溫度')

    // 建立獨立場域登入，避免沿用總部 Cookie 並核對清單及控制入口的可見性。
    const siteContext = await browser.newContext({ viewport: { width: 1280, height: 900 } })
    const sitePage = await siteContext.newPage()
    sitePage.setDefaultTimeout(8_000)
    // 沿用有上限的診斷例外與注入旗標，讓場域頁面的意外錯誤也能被捕捉。
    monitorBrowserErrors(sitePage, passwords, browserErrors, stack.origin, expectedResources, () => injectedUnknownResponse)
    await sitePage.goto(stack.origin, { timeout: 10_000 })
    await signIn(sitePage, 'north', passwords.north, stack.origin)
    assert.equal(await sitePage.getByRole('listitem').count(), 3, '北區帳號只可見所屬場域設備')
    assert.equal(await sitePage.getByRole('button', { name: /南區設備/ }).count(), 0, '北區帳號不得看見南區設備')
    await sitePage.getByRole('button', { name: /北區設備 01/ }).click()
    await sitePage.getByText('north-01', { exact: true }).waitFor()
    assert.equal(await sitePage.getByRole('heading', { name: '模擬控制', level: 3, exact: true }).count(), 0,
      '場域帳號詳細頁不得提供模擬控制區')
    for (const label of controlLabels) {
      assert.equal(await sitePage.getByRole('button', { name: label, exact: true }).count(), 0,
        '場域帳號不得提供模擬操作按鈕')
    }

    // 模擬寫入已提交但瀏覽器收到 503，區分結果不明與寫入失敗。
    let injectedRequest
    let committedStatus
    let committedState
    let injectionFailed = false
    // 只攔截控制寫入；狀態讀取照常送到服務，讓畫面能讀回已提交結果。
    await hqPage.route(stack.origin + simulationPath, async route => {
      if (route.request().method() !== 'POST') {
        await route.continue()
        return
      }
      // 先轉送瀏覽器原本的本文並記錄真實回應，再回傳合成錯誤。
      try {
        injectedRequest = route.request().postDataJSON()
        const committed = await sendAsHq(simulationPath, 'POST', injectedRequest)
        committedStatus = committed.status
        committedState = await committed.json()
        await route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: '{"error":"service_unavailable"}',
        })
      } catch {
        // 若真實操作或合成回應失敗，記下測試注入失敗，避免把它當成有效情境。
        injectionFailed = true
        await route.abort()
      }
    })
    // 發出一次指定的使用者操作，收到結果後撤除攔截以便繼續驗證。
    injectedUnknownResponse = true
    const [unknownResponse] = await Promise.all([
      // 只接收這次控制的 POST，確認瀏覽器端確實收到合成錯誤。
      hqPage.waitForResponse(item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST'),
      hqPage.getByRole('button', { name: '開始正常回報', exact: true }).click(),
    ])
    await hqPage.unroute(stack.origin + simulationPath)
    // 先確認控制真實提交與回應替換都成立，再判斷畫面的復原結果。
    assert.equal(injectionFailed, false, '未知結果案例必須先成功送達實際服務')
    assert.equal(committedStatus, 200, '瀏覽器收到合成錯誤前，伺服器控制必須已提交')
    assert.equal(committedState.mode, 'normal', '未知結果案例的伺服器寫入須切換為正常模式')
    assert.equal(unknownResponse.status(), 503, '瀏覽器應收到寫入提交後的合成服務錯誤')
    assert.deepEqual(injectedRequest, { action: 'normal', version: finalSimulation.version },
      '未知結果案例須轉送瀏覽器原本的控制本文')
    // 結果不明時須提示使用者並從服務讀回狀態，不能再送同一筆控制。
    await hqPage
      .getByRole('alert')
      .filter({ hasText: '未能確認操作結果。請確認目前模擬狀態後再操作。' })
      .waitFor()
    await hqPage.getByText('模擬模式：正常回報', { exact: true }).waitFor()
    await hqPage.getByText('23°C', { exact: true }).waitFor()
    const unknownReadback = await responseJson(await sendAsHq(simulationPath), 200, '未知結果後模擬讀回')
    const unknownDevice = await responseJson(await sendAsHq('/api/devices/north-01'), 200, '未知結果後設備讀回')
    assert.equal(unknownReadback.mode, 'normal', '未知結果後讀回須辨識已提交的正常模式')
    assert.equal(unknownDevice.latestMeasurement?.temperature, 23, '未知結果後讀回須辨識已採用的測量')
    // 用請求紀錄確認未知結果的復原只讀不寫。
    assert.deepEqual(pageControls.map(control => control.action), ['normal', 'normal', 'replay', 'stop', 'normal'],
      '未知結果不得自動重送控制')

    // 由使用者再次明確操作停止，確認復原後可寫入且成功時清除舊提示。
    const manualStopResponsePromise = hqPage.waitForResponse(
      item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST',
    )
    await hqPage.getByRole('button', { name: '停止回報', exact: true }).click()
    const manualStopState = await responseJson(await manualStopResponsePromise, 200, '未知結果後手動停止')
    assert.equal(manualStopState.mode, 'stopped', '使用者後續手動操作須正常執行')
    await hqPage.getByText('已停止模擬回報。', { exact: true }).waitFor()
    await hqPage.getByText('模擬模式：已停止', { exact: true }).waitFor()
    // 只計算對應的未知結果提示，確認成功操作後已被清除。
    assert.equal(await hqPage
      .getByRole('alert')
      .filter({
        hasText: '未能確認操作結果。請確認目前模擬狀態後再操作。',
      })
      .count(), 0, '後續操作成功後須清除未知結果提示')
    // 對照瀏覽器實際請求順序，確認最後只追加一次手動停止。
    assert.deepEqual(pageControls.map(control => control.action), ['normal', 'normal', 'replay', 'stop', 'normal', 'stop'],
      '恢復後只執行使用者明確要求的停止操作')

    // 使用正式啟動入口重啟容器，確認資料保留且舊執行的原始測量不再可重送。
    const beforeRestart = await responseJson(
      await sendAsHq(simulationPath, 'POST', { action: 'normal', version: manualStopState.version }),
      200,
      '重啟前開始正常回報',
    )
    assert.equal(beforeRestart.mode, 'normal', '重啟前設備須處於有效模擬執行中')
    assert.equal(beforeRestart.canReplay, true, '重啟前須有可重送的原始測量')
    const beforeRestartDevice = await responseJson(
      await sendAsHq('/api/devices/north-01'), 200, '重啟前設備讀回',
    )
    assert.equal(beforeRestartDevice.latestMeasurement?.temperature, 23, '重啟前正常回報須已採用')
    // 瀏覽器驗證已結束，先關閉頁面及串流，後續由正式 API 驗證重啟。
    assert.equal(expectedResources.find(resource => resource.url === stack.origin + '/api/session')?.count, 2,
      '只允許兩筆未登入時的 session 探查回應')
    for (const page of [hqPage, sitePage]) {
      assert.equal(await page.locator('vite-error-overlay, #webpack-dev-server-client-overlay').count(), 0,
        '畫面不可出現前端建置錯誤覆蓋層')
      await page.close()
    }
    assert.deepEqual(browserErrors, [], `瀏覽器不可有 JavaScript 或 console error：${browserErrors.join(' | ')}`)
    // 以原 session 讀回重啟後的狀態，區分保留的測量與失效的模擬執行。
    stack.stop()
    stack.start()
    assert.deepEqual(await responseJson(await sendAsHq('/api/session'), 200, '重啟後 HQ session'), {
      username: 'hq', role: 'headquarters', siteId: null,
    }, '有效總部登入須跨服務重啟保留')
    const afterRestart = await responseJson(await sendAsHq(simulationPath), 200, '重啟後模擬狀態讀回')
    const afterRestartDevice = await responseJson(
      await sendAsHq('/api/devices/north-01'), 200, '重啟後設備讀回',
    )
    assert.equal(afterRestart.mode, 'stopped', '服務重啟後模擬須保持停止')
    assert.equal(afterRestart.canReplay, false, '服務重啟後不可重送舊執行原始測量')
    assert.equal(afterRestartDevice.latestMeasurement?.temperature, 23, '重啟須保留最新有效溫度')

    // 跨過五秒回報週期，確認重啟後沒有自行恢復產生測量。
    await setTimeout(5_250, undefined, { signal: t.signal })
    const laterSimulation = await responseJson(await sendAsHq(simulationPath), 200, '重啟觀察期後模擬狀態讀回')
    const laterDevice = await responseJson(await sendAsHq('/api/devices/north-01'), 200, '重啟觀察期後設備讀回')
    assert.equal(laterSimulation.mode, 'stopped', '重啟後跨過回報週期仍須保持停止')
    assert.equal(laterSimulation.canReplay, false, '重啟後跨過回報週期仍不可重送')
    assert.deepEqual(laterDevice.latestMeasurement, afterRestartDevice.latestMeasurement,
      '重啟後跨過回報週期不得出現新測量')

  } finally {
    // 即使中途失敗也先釋放 Chrome，讓外層回收測試專案。
    await browser.close()
  }
})
