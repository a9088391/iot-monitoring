import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'
import { setTimeout } from 'node:timers/promises'
import { createStack } from './stack.js'

const simulationPath = '/api/devices/north-01/simulation'
const controlLabels = ['開始正常回報', '開始異常回報', '停止回報', '重複回報']
const loginFormError = '瀏覽器登入表單操作失敗'

async function responseJson(response, status, label) {
  assert.equal(typeof response.status === 'function' ? response.status() : response.status, status, label)
  try {
    return await response.json()
  } catch {
    throw new Error(`${label}：回應不是有效 JSON`)
  }
}

function redact(message, passwords) {
  return Object.values(passwords)
    .reduce((text, password) => text.replaceAll(password, '[redacted]'), message)
    .replace(/iot_session=[A-Za-z0-9_-]+/g, 'iot_session=[redacted]')
    .slice(0, 300)
}

function monitorBrowserErrors(page, passwords, errors, origin, expectedResources, isInjectedUnknown) {
  page.on('console', message => {
    if (message.type() !== 'error') return
    const url = message.location().url
    const expected = expectedResources.find(resource => resource.url === url && resource.message === message.text() &&
      (!resource.injectedOnly || isInjectedUnknown()) && resource.count < resource.maximum)
    if (expected) {
      expected.count += 1
      return
    }
    errors.push(`console: ${redact(`${message.text()} @ ${url || origin}`, passwords)}`)
  })
  page.on('pageerror', error => errors.push(`page: ${redact(`${error.name}: ${error.message}`, passwords)}`))
}

async function signIn(page, username, password, origin) {
  const account = page.getByLabel('帳號', { exact: true })
  const secret = page.getByLabel('密碼', { exact: true })
  let response
  try {
    await account.fill(username)
    await secret.fill(password)
    assert.equal(await secret.evaluate(element => document.activeElement === element), true, '密碼欄位須保有鍵盤焦點')
    const [loginResponse] = await Promise.all([
      page.waitForResponse(item => item.url() === `${origin}/api/login` && item.request().method() === 'POST'),
      secret.press('Enter'),
    ])
    response = loginResponse
  } catch {
    // Playwright 失敗時會附上輸入值，不保留原始例外。
    throw new Error(loginFormError)
  }
  await responseJson(response, 200, '登入')
  await page.getByRole('heading', { name: '設備清單', level: 2, exact: true }).waitFor()
}

test('IoT 票 03：真實瀏覽器模擬控制與衝突復原', { timeout: 600_000 }, async t => {
  const stack = await createStack()
  t.after(() => stack.cleanup())
  stack.start()

  const passwords = JSON.parse(readFileSync(join(stack.state, 'demo-passwords.json'), 'utf8'))
  const executablePath = process.env.CHROME_EXECUTABLE ?? [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/google-chrome', '/usr/bin/chromium',
  ].find(path => existsSync(path))
  assert.ok(executablePath && existsSync(executablePath), '請安裝 Chrome／Chromium 或設定 CHROME_EXECUTABLE')
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  const { chromium } = await import('playwright-core')
  const browser = await chromium.launch({ executablePath, timeout: 15_000 })
  const browserErrors = []
  let injectedUnknownResponse = false
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
    await assert.rejects(signIn(hqPage, 'fixture', syntheticPassword, stack.origin), error => {
      assert.ok(error.message === loginFormError && !error.message.includes(syntheticPassword) && !('cause' in error),
        '登入表單失敗須只回報固定訊息，且不得附上密碼或原始例外')
      return true
    })
    assert.equal(await hqPage.getByLabel('帳號', { exact: true }).inputValue(), 'fixture', '須到達密碼欄位才失敗')
    hqPage.setDefaultTimeout(8_000)
    monitorBrowserErrors(hqPage, passwords, browserErrors, stack.origin, expectedResources, () => injectedUnknownResponse)
    await hqPage.goto(stack.origin, { timeout: 10_000 })
    await signIn(hqPage, 'hq', passwords.hq, stack.origin)
    const hqSession = (await hqContext.cookies(stack.origin)).find(cookie => cookie.name === 'iot_session')
    assert.ok(hqSession, '瀏覽器登入後須持有總部 session')
    const hqCookie = `iot_session=${hqSession.value}`
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

    await hqPage.getByRole('button', { name: /北區設備 01/ }).click()
    await hqPage.getByRole('heading', { name: '模擬控制', level: 3, exact: true }).waitFor()
    await hqPage.getByText('north-01', { exact: true }).waitFor()
    await hqPage.getByText('尚未回報', { exact: true }).waitFor()
    await hqPage.getByText('模擬模式：已停止', { exact: true }).waitFor()
    const replayButton = hqPage.getByRole('button', { name: '重複回報', exact: true })
    assert.equal(await replayButton.isDisabled(), true, '沒有原始測量時不得啟用重送')
    await hqPage.getByText('尚無可重送的測量。', { exact: true }).waitFor()

    async function checkViewport(width, height) {
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
      assert.equal(await hqPage.evaluate(() => window.innerHeight), height, '瀏覽器需使用指定檢查高度')
    }

    await checkViewport(1280, 900)
    await hqPage.setViewportSize({ width: 390, height: 844 })
    await checkViewport(390, 844)

    const pageControls = []
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

    const normalButton = hqPage.getByRole('button', { name: '開始正常回報', exact: true })
    await normalButton.focus()
    assert.equal(await normalButton.evaluate(element => document.activeElement === element), true, '控制項須可由鍵盤取得焦點')
    const [normalResponse] = await Promise.all([
      hqPage.waitForResponse(item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST'),
      normalButton.press('Enter'),
    ])
    const normalState = await responseJson(normalResponse, 200, '開始正常回報')
    assert.equal(normalState.mode, 'normal', '開始正常回報須切換至正常模式')
    await hqPage.getByText('模擬模式：正常回報', { exact: true }).waitFor()
    await hqPage.getByText('23°C', { exact: true }).waitFor()
    await hqPage.getByText('已開始正常回報。', { exact: true }).waitFor()

    const externalResponse = await sendAsHq(simulationPath, 'POST', { action: 'abnormal', version: normalState.version })
    const externalState = await responseJson(externalResponse, 200, '第二個總部控制')
    assert.equal(externalState.mode, 'abnormal', '第二個總部控制須切換至異常模式')
    assert.equal(externalState.canReplay, true, '異常回報須保存可重送的原始測量')
    await hqPage.getByText('模擬模式：正常回報', { exact: true }).waitFor()

    const [conflictResponse] = await Promise.all([
      hqPage.waitForResponse(item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST'),
      normalButton.click(),
    ])
    assert.equal(conflictResponse.status(), 409, '瀏覽器送出舊版本時須收到衝突')
    await hqPage.getByRole('alert').filter({ hasText: '模擬狀態已變更，請確認目前模式後再操作。' }).waitFor()
    await hqPage.getByText('模擬模式：異常回報', { exact: true }).waitFor()
    await hqPage.getByText('30°C', { exact: true }).waitFor()
    assert.deepEqual(pageControls.map(control => control.action), ['normal', 'normal'], '衝突控制不得自動重送')
    assert.equal(pageControls[1].version, normalState.version, '衝突請求須保留瀏覽器先前讀到的版本')

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
    assert.deepEqual(pageControls.map(control => control.action), ['normal', 'normal', 'replay', 'stop'],
      '只允許明確操作的瀏覽器控制請求')

    const finalSimulation = await responseJson(await sendAsHq(simulationPath), 200, '停止後狀態讀回')
    const finalDevice = await responseJson(await sendAsHq('/api/devices/north-01'), 200, '停止後設備讀回')
    assert.equal(finalSimulation.mode, 'stopped', '停止後伺服器狀態須保持停止')
    assert.equal(finalDevice.latestMeasurement?.temperature, 30, '停止只停產，不清除最後有效溫度')

    const siteContext = await browser.newContext({ viewport: { width: 1280, height: 900 } })
    const sitePage = await siteContext.newPage()
    sitePage.setDefaultTimeout(8_000)
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

    let injectedRequest
    let committedStatus
    let committedState
    let injectionFailed = false
    await hqPage.route(stack.origin + simulationPath, async route => {
      if (route.request().method() !== 'POST') {
        await route.continue()
        return
      }
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
        injectionFailed = true
        await route.abort()
      }
    })
    injectedUnknownResponse = true
    const [unknownResponse] = await Promise.all([
      hqPage.waitForResponse(item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST'),
      hqPage.getByRole('button', { name: '開始正常回報', exact: true }).click(),
    ])
    await hqPage.unroute(stack.origin + simulationPath)
    assert.equal(injectionFailed, false, '未知結果案例必須先成功送達實際服務')
    assert.equal(committedStatus, 200, '瀏覽器收到合成錯誤前，伺服器控制必須已提交')
    assert.equal(committedState.mode, 'normal', '未知結果案例的伺服器寫入須切換為正常模式')
    assert.equal(unknownResponse.status(), 503, '瀏覽器應收到寫入提交後的合成服務錯誤')
    assert.deepEqual(injectedRequest, { action: 'normal', version: finalSimulation.version },
      '未知結果案例須轉送瀏覽器原本的控制本文')
    await hqPage.getByRole('alert').filter({ hasText: '未能確認操作結果。請確認目前模擬狀態後再操作。' }).waitFor()
    await hqPage.getByText('模擬模式：正常回報', { exact: true }).waitFor()
    await hqPage.getByText('23°C', { exact: true }).waitFor()
    const unknownReadback = await responseJson(await sendAsHq(simulationPath), 200, '未知結果後模擬讀回')
    const unknownDevice = await responseJson(await sendAsHq('/api/devices/north-01'), 200, '未知結果後設備讀回')
    assert.equal(unknownReadback.mode, 'normal', '未知結果後讀回須辨識已提交的正常模式')
    assert.equal(unknownDevice.latestMeasurement?.temperature, 23, '未知結果後讀回須辨識已採用的測量')
    assert.deepEqual(pageControls.map(control => control.action), ['normal', 'normal', 'replay', 'stop', 'normal'],
      '未知結果不得自動重送控制')

    const manualStopResponsePromise = hqPage.waitForResponse(
      item => item.url() === stack.origin + simulationPath && item.request().method() === 'POST',
    )
    await hqPage.getByRole('button', { name: '停止回報', exact: true }).click()
    const manualStopState = await responseJson(await manualStopResponsePromise, 200, '未知結果後手動停止')
    assert.equal(manualStopState.mode, 'stopped', '使用者後續手動操作須正常執行')
    await hqPage.getByText('已停止模擬回報。', { exact: true }).waitFor()
    await hqPage.getByText('模擬模式：已停止', { exact: true }).waitFor()
    assert.equal(await hqPage.getByRole('alert').filter({
      hasText: '未能確認操作結果。請確認目前模擬狀態後再操作。',
    }).count(), 0, '後續操作成功後須清除未知結果提示')
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

    assert.equal(expectedResources.find(resource => resource.url === stack.origin + '/api/session')?.count, 2,
      '只允許兩筆未登入時的 session 探查回應')
    for (const page of [hqPage, sitePage]) {
      assert.equal(await page.locator('vite-error-overlay, #webpack-dev-server-client-overlay').count(), 0,
        '畫面不可出現前端建置錯誤覆蓋層')
    }
    assert.deepEqual(browserErrors, [], `瀏覽器不可有 JavaScript 或 console error：${browserErrors.join(' | ')}`)
  } finally {
    await browser.close()
  }
})
