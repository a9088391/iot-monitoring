import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'
import { createStack } from './stack.js'

const alertsPath = '/api/alerts'
const recovered = '已恢復、待結案'
const closed = '已結案'
const success = '已成功結案。'

// 同時支援測試堆疊與瀏覽器的回應，解析失敗時只回報測試階段。
async function json(response, status, phase) {
  assert.equal(typeof response.status === 'function' ? response.status() : response.status, status, phase)
  try {
    return await response.json()
  } catch {
    throw new Error(`${phase}：回應不是有效 JSON`)
  }
}

// 沿用實際登入表單，避免 Playwright 的填寫失敗紀錄帶出密碼。
async function signIn(page, username, password, origin) {
  let response
  try {
    await page.getByLabel('帳號', { exact: true }).fill(username)
    const secret = page.getByLabel('密碼', { exact: true })
    await secret.fill(password)
    const [loginResponse] = await Promise.all([
      page.waitForResponse(item =>
        item.url() === origin + '/api/login'
        && item.request().method() === 'POST'),
      secret.press('Enter'),
    ])
    response = loginResponse
  } catch {
    throw new Error('瀏覽器登入表單操作失敗')
  }
  await json(response, 200, '登入')
  await page.getByRole('heading', { name: '設備清單', level: 2, exact: true }).waitFor()
}

// 同時驗證外層頁面及長純文字的儲存格，避免內層裁切掩蓋溢出。
async function checkLayout(page, width, height, result) {
  await page.setViewportSize({ width, height })
  const size = await page.evaluate(() => ({
    width: innerWidth, height: innerHeight, contentWidth: document.documentElement.scrollWidth,
  }))
  assert.deepEqual(size, { width, height, contentWidth: width })
  if (result) {
    assert.ok(await result.evaluate(element => element.scrollWidth <= element.clientWidth),
      '長處理結果不得超出內容欄')
  }
}

// 從正式控制產生告警，串起實際寫入、回應遺失、讀回與重啟。
test('IoT 票 05：真實瀏覽器結案、場域歷程及失敗復原', { timeout: 600_000 }, async t => {
  const stack = await createStack()
  // 在啟動前登記清理，故障注入中止時也能回收這份資料庫。
  t.after(() => stack.cleanup())
  stack.start()
  const passwords = JSON.parse(readFileSync(join(stack.state, 'demo-passwords.json'), 'utf8'))
  const secrets = [...Object.values(passwords), readFileSync(join(stack.state, 'db-password'), 'utf8').trim()]

  // 只有寫入附加正式來源及 CSRF 標頭，Cookie 只在隔離堆疊內傳送。
  function send(path, cookie, body) {
    return stack.send(path, body === undefined
      ? { headers: { Cookie: cookie } }
      : {
          method: 'POST',
          headers: { Cookie: cookie, Origin: stack.origin, 'X-CSRF-Protection': '1', 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
  }

  const login = await send('/api/login', '', { username: 'hq', password: passwords.hq })
  await json(login, 200, '總部控制登入')
  const hqCookie = login.headers.getSetCookie()[0]?.split(';')[0]
  assert.ok(hqCookie?.startsWith('iot_session='), '須取得總部控制 session')
  secrets.push(hqCookie.slice('iot_session='.length))

  // 只用 SQL 建立分頁用歷史資料；操作對象由真實模擬控制產生。
  stack.sql(`
    INSERT INTO alerts (device_id, type, temperature, detected_at_ns, recovered_at_ns)
    SELECT 'north-02', 'temperature', 30, n, n + 1 FROM generate_series(1, 49) AS n;
  `)
  for (const [device, actions] of [
    ['north-01', ['abnormal', 'normal', 'abnormal', 'normal', 'abnormal']],
    ['south-01', ['abnormal', 'normal']],
  ]) {
    const path = `/api/devices/${device}/simulation`
    let state = await json(await send(path, hqCookie), 200, '讀取控制版本')
    for (const action of actions) {
      state = await json(await send(path, hqCookie, { action, version: state.version }), 200, '產生溫度告警')
    }
  }
  const history = await json(await send(alertsPath + '?limit=100', hqCookie), 200, '讀取告警夾具')
  const [northActive, northUnknown, northRecovered] = history.items.filter(item => item.deviceId === 'north-01')
  const [southRecovered] = history.items.filter(item => item.deviceId === 'south-01')
  assert.deepEqual(
    [northActive?.status, northUnknown?.status, northRecovered?.status, southRecovered?.status],
    ['active', 'recovered', 'recovered', 'recovered'],
  )
  assert.equal(history.total, 53)

  const executablePath = process.env.CHROME_EXECUTABLE ?? [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/google-chrome', '/usr/bin/chromium',
  ].find(path => existsSync(path))
  assert.ok(executablePath && existsSync(executablePath), '請安裝 Chrome／Chromium 或設定 CHROME_EXECUTABLE')
  // 禁用可能輸出登入表單內容的 Playwright 診斷。
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  const { chromium } = await import('playwright-core')
  const browser = await chromium.launch({ executablePath, timeout: 15_000 })
  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, timezoneId: 'UTC' })
    const page = await context.newPage()
    page.setDefaultTimeout(8_000)
    const historyButton = page.getByRole('button', { name: '告警歷程', exact: true })
    const deviceButton = page.getByRole('button', { name: '設備清單', exact: true })
    const historyTitle = page.getByRole('heading', { name: '告警歷程', exact: true })
    const detailTitle = page.getByRole('heading', { name: '告警詳情', exact: true })
    const back = page.getByRole('button', { name: '返回告警歷程' })
    const input = page.getByLabel('處理結果', { exact: true })
    const submit = page.getByRole('button', { name: '結案', exact: true })
    const readAgain = page.getByRole('button', { name: '重新讀取告警', exact: true })
    const accountMessage = page.getByText('登入狀態已失效，請重新登入。', { exact: true })
    const focus = page.locator(':focus')
    // 以 API 識別碼鎖定操作的告警，避免選中同設備其他紀錄。
    const row = item => page.getByRole('button', { name: `#${item.id} · 溫度異常 · ${item.deviceId}` })
    const detailPath = item => `${alertsPath}/${item.id}`
    const closePath = item => `${detailPath(item)}/close`
    const detailValue = label => page.locator('dl > div')
      .filter({ has: page.getByText(label, { exact: true }) })
      .locator('dd')

    const browserErrors = []
    const closeRequests = []
    let restarting = false
    // 僅放行已知階段、精確路徑及有限次數的資源錯誤。
    const expectedErrors = [
      ['/api/session', 'status of 401', 1],
      ['/favicon.ico', 'status of 404', 2],
      [closePath(northRecovered), 'status of 400', 1],
      [closePath(northRecovered), 'status of 503', 1],
      [closePath(northUnknown), 'net::ERR_CONNECTION_RESET', 1],
      [detailPath(northUnknown), 'net::ERR_CONNECTION_RESET', 1],
      [closePath(southRecovered), 'status of 401', 1],
    ]
    // 移除所有已知憑證再儲存短診斷，避免測試斷言輸出敏感值。
    function diagnostic(message) {
      return secrets
        .reduce((value, secret) => value.replaceAll(secret, '[redacted]'), message)
        .replace(/iot_session=[A-Za-z0-9_-]+/g, 'iot_session=[redacted]')
        .slice(0, 300)
    }
    page.on('pageerror', error => browserErrors.push(diagnostic(error.message)))
    page.on('console', message => {
      if (message.type() !== 'error') return
      const { url } = message.location()
      const description = message.text()
      // 刻意停機時，串流及其身分查驗可能先收到連線錯誤。
      if (
        restarting
        && (url === stack.origin + '/api/events' || url === stack.origin + '/api/session')
        && /net::ERR_(CONNECTION_REFUSED|CONNECTION_RESET|EMPTY_RESPONSE)$/.test(description)
      ) return
      const expected = expectedErrors.find(([path, reason, remaining]) =>
        remaining > 0
        && url === stack.origin + path
        && description.startsWith('Failed to load resource:')
        && description.includes(reason))
      if (expected) expected[2] -= 1
      else browserErrors.push(diagnostic(`${description} @ ${url}`))
    })
    page.on('request', request => {
      const { pathname } = new URL(request.url())
      if (request.method() === 'POST' && pathname.startsWith(alertsPath + '/')) closeRequests.push(pathname)
    })

    // 擷取登入 Cookie 供獨立的伺服器讀回，並加入測試日誌的敏感值檢查。
    async function browserSignIn(username) {
      await signIn(page, username, passwords[username], stack.origin)
      const cookie = (await context.cookies(stack.origin)).find(item => item.name === 'iot_session')
      assert.ok(cookie?.value, '登入後應取得正式 session')
      secrets.push(cookie.value)
      return `iot_session=${cookie.value}`
    }

    // 只等待指定操作對象的寫入回應，排除背景讀取或其他告警。
    async function submitResponse(item, action = () => submit.click()) {
      const [response] = await Promise.all([
        page.waitForResponse(candidate =>
          candidate.url() === stack.origin + closePath(item)
          && candidate.request().method() === 'POST'),
        action(),
      ])
      return response
    }

    // 使用鍵盤走到結案按鈕，確認表單欄位及提交控制可操作。
    async function submitWithKeyboard() {
      await input.press('Tab')
      await focus.and(submit).waitFor()
      await page.keyboard.press('Enter')
    }

    // 從歷程標題按 Tab 經過重新載入及每筆告警，才到可用的分頁按鈕。
    async function changePageWithKeyboard(button, rowCount) {
      for (let step = 0; step < rowCount + 2; step += 1) {
        await page.keyboard.press('Tab')
      }
      assert.equal(await focus.and(button).count(), 1, '按 Enter 前應已聚焦分頁按鈕')
      await page.keyboard.press('Enter')
      await focus.and(historyTitle).waitFor()
    }

    // 核對唯讀結果的 DOM 原文、結案人、UTC 屬性與臺灣時間。
    async function expectReadOnly(closure) {
      await page.getByText(closed, { exact: true }).waitFor()
      const result = detailValue('處理結果')
      assert.equal(await result.textContent(), closure.result)
      assert.equal(await result.evaluate(element => element.childElementCount), 0, '純文字不得成為 HTML 節點')
      assert.equal(await detailValue('結案人').textContent(), closure.actor)
      const time = detailValue('結案時間').locator('time')
      assert.match(closure.closedAt, /Z$/)
      assert.equal(await time.getAttribute('datetime'), closure.closedAt)
      const formatter = new Intl.DateTimeFormat('zh-TW', {
        timeZone: 'Asia/Taipei', dateStyle: 'short', timeStyle: 'medium', hourCycle: 'h23',
      })
      // Chrome 與 Node 的 ICU 版本可能使用不同的日期／時間分隔空白。
      assert.equal(
        (await time.textContent()).replace(/\s+/gu, ' '),
        formatter.format(new Date(closure.closedAt)).replace(/\s+/gu, ' '),
      )
      assert.equal(await input.count(), 0)
      assert.equal(await submit.count(), 0)
      return result
    }

    await page.goto(stack.origin)
    const northCookie = await browserSignIn('north')
    assert.equal(await page.evaluate(() => Intl.DateTimeFormat().resolvedOptions().timeZone), 'UTC')
    await historyButton.press('Enter')
    await focus.and(historyTitle).waitFor()
    await page.getByText('共 52 筆告警', { exact: true }).waitFor()

    const pager = page.getByRole('navigation', { name: '告警歷程分頁' })
    const previous = pager.getByRole('button', { name: '上一頁', exact: true })
    const next = pager.getByRole('button', { name: '下一頁', exact: true })
    const northIDs = history.items
      .filter(item => item.deviceId.startsWith('north-'))
      .map(item => item.id)
    const firstPageIDs = northIDs.slice(0, 50)
    const secondPageIDs = northIDs.slice(50)
    const rowIDs = () => page.locator('button.alert-button > span:first-child')
      .evaluateAll(elements => elements.map(element => element.textContent.match(/^#(\d+) · /)?.[1]))
    await pager.getByText('第 1 / 2 頁', { exact: true }).waitFor()
    assert.equal(await previous.isDisabled(), true)
    assert.deepEqual(await rowIDs(), firstPageIDs)
    assert.equal(await row(southRecovered).count(), 0)
    await checkLayout(page, 390, 844)
    await changePageWithKeyboard(next, firstPageIDs.length)
    await pager.getByText('第 2 / 2 頁', { exact: true }).waitFor()
    assert.equal(await next.isDisabled(), true)
    assert.deepEqual(await rowIDs(), secondPageIDs)
    await changePageWithKeyboard(previous, secondPageIDs.length)
    await pager.getByText('第 1 / 2 頁', { exact: true }).waitFor()
    assert.deepEqual(await rowIDs(), firstPageIDs)
    await checkLayout(page, 1280, 900)

    await row(northActive).press('Enter')
    await page.getByText('異常持續中', { exact: true }).waitFor()
    assert.equal(await input.count(), 0)
    assert.equal(await submit.count(), 0)
    await back.press('Enter')
    await row(northRecovered).press('Enter')
    await focus.and(detailTitle).waitFor()
    await page.getByText(recovered, { exact: true }).waitFor()
    await page.keyboard.press('Tab')
    await focus.and(input).waitFor()

    // 先走瀏覽器端驗證，再以內含 Tab 的本文走真實服務輸入錯誤。
    await input.fill('　 ')
    await submitWithKeyboard()
    await page
      .getByRole('alert')
      .filter({ hasText: '請輸入 1～2,000 個字元' })
      .waitFor()
    await focus.and(input).waitFor()
    assert.deepEqual(closeRequests, [])
    const invalid = '第一行\t第二行'
    await input.fill(invalid)
    assert.equal(await input.inputValue(), invalid)
    const invalidResponse = await submitResponse(northRecovered, submitWithKeyboard)
    assert.deepEqual(invalidResponse.request().postDataJSON(), { result: invalid })
    assert.deepEqual(await json(invalidResponse, 400, '實際服務驗證'), { error: 'invalid_input' })
    await page
      .getByRole('alert')
      .filter({ hasText: '請輸入 1～2,000 個字元' })
      .waitFor()
    await focus.and(input).waitFor()
    assert.equal(await input.inputValue(), invalid)

    const prefix = ' <b>測試已復原</b>\n'
    const result = prefix + '😀'.repeat(1999 - [...prefix].length) + '\n'
    assert.equal([...result].length, 2000)
    await input.fill(result)
    await page.getByText(/已輸入 2000 個字元/).waitFor()
    await checkLayout(page, 390, 844)
    // 在真正提交時模擬容量不足，讓結案交易自行回滾。
    stack.sql(`
      CREATE FUNCTION fail_browser_closure() RETURNS trigger LANGUAGE plpgsql AS $$
      BEGIN
        RAISE EXCEPTION 'isolated browser closure fixture' USING ERRCODE = '53100';
      END $$;
      CREATE CONSTRAINT TRIGGER fail_browser_closure AFTER INSERT ON alert_closures
      DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_browser_closure();
    `)
    const failed = await submitResponse(northRecovered, submitWithKeyboard)
    assert.deepEqual(await json(failed, 503, '真正的結案交易提交失敗'), { error: 'service_unavailable' })
    await page.getByText('告警目前仍待結案，請確認草稿後再送出。', { exact: true }).waitFor()
    await page
      .locator('textarea:not([readonly])')
      .and(input)
      .waitFor()
    assert.equal(await input.inputValue(), result)
    assert.equal(await input.getAttribute('readonly'), null)
    assert.equal(await submit.isEnabled(), true)
    assert.equal(await page.getByText(success, { exact: true }).count(), 0)
    assert.equal(stack.sql('SELECT count(*) FROM alert_closures;'), '0')
    assert.deepEqual(closeRequests, [closePath(northRecovered), closePath(northRecovered)])

    stack.sql('DROP TRIGGER fail_browser_closure ON alert_closures; DROP FUNCTION fail_browser_closure();')
    const saved = await json(await submitResponse(northRecovered, submitWithKeyboard), 200, '使用者手動重試結案')
    await page.getByText(success, { exact: true }).waitFor()
    assert.equal(saved.id, northRecovered.id)
    assert.equal(saved.status, 'closed')
    assert.equal(saved.closure.actor, 'north')
    assert.equal(saved.closure.result, result)
    assert.deepEqual(await json(await send(detailPath(northRecovered), northCookie), 200, '結案正式讀回'), saved)
    assert.deepEqual(await json(await send(detailPath(northActive), northCookie), 200, '同設備新告警讀回'), northActive)
    const shownResult = await expectReadOnly(saved.closure)
    await checkLayout(page, 390, 844, shownResult)
    await checkLayout(page, 1280, 900, shownResult)

    await back.click()
    await row(northUnknown).click()
    await page.getByText(recovered, { exact: true }).waitFor()
    const unknownDraft = '第二筆已確認\n保持原文'
    await input.fill(unknownDraft)
    let committed
    let droppedReadback
    let droppingReadback = false
    const releaseReadback = Promise.withResolvers()
    // 以瀏覽器原始本文與憑證轉送一次真實 POST，保留已提交回應供斷言。
    await page.route(stack.origin + closePath(northUnknown), async route => {
      const request = route.request()
      const headers = await request.allHeaders()
      assert.equal(request.method(), 'POST')
      assert.equal(headers.origin, stack.origin)
      assert.equal(headers['x-csrf-protection'], '1')
      assert.ok(headers.cookie === northCookie, '轉送必須使用原瀏覽器 session')
      assert.deepEqual(request.postDataJSON(), { result: unknownDraft })
      const response = await stack.send(closePath(northUnknown), {
        method: request.method(),
        headers: {
          Cookie: headers.cookie, Origin: headers.origin, 'X-CSRF-Protection': headers['x-csrf-protection'],
          'Content-Type': headers['content-type'],
        },
        body: request.postData(),
      })
      committed = await json(response, 200, '遺失回應前的真實提交')
      await route.abort('connectionreset')
    }, { times: 1 })
    // 首次讀回也查詢真實服務，在頁面處於等待時驗證離開保護。
    await page.route(stack.origin + detailPath(northUnknown), async route => {
      // 提交之前的 SSE 背景讀取照常送出，不能消耗本次故障注入。
      if (!committed || droppingReadback) {
        await route.continue()
        return
      }
      droppingReadback = true
      assert.equal(route.request().method(), 'GET')
      droppedReadback = await json(await send(detailPath(northUnknown), northCookie), 200, '遺失的首次讀回')
      await releaseReadback.promise
      await route.abort('connectionreset')
    })
    try {
      await submit.click()
      await page.getByText('正在讀取告警…', { exact: true }).waitFor()
      assert.equal(await input.getAttribute('readonly'), '')
      assert.equal(await submit.isDisabled(), true)
      assert.equal(await back.isDisabled(), true)
      assert.equal(await deviceButton.isDisabled(), true)
    } finally {
      releaseReadback.resolve()
    }
    await page
      .getByRole('alert')
      .filter({ hasText: '仍無法確認告警狀態' })
      .waitFor()
    await readAgain.waitFor()
    assert.equal(await input.inputValue(), unknownDraft)
    assert.equal(await input.getAttribute('readonly'), '')
    assert.equal(await submit.isDisabled(), true)
    assert.equal(await page.getByText(success, { exact: true }).count(), 0)
    assert.ok(committed && droppedReadback, '第一次提交及讀回都必須實際到達服務')
    assert.equal(committed.closure.result, unknownDraft)
    assert.deepEqual(droppedReadback, committed)
    assert.equal(closeRequests.filter(path => path === closePath(northUnknown)).length, 1)

    await readAgain.click()
    await page.getByText('已讀取告警目前的狀態。', { exact: true }).waitFor()
    await page.unroute(stack.origin + detailPath(northUnknown))
    await expectReadOnly(committed.closure)
    assert.equal(await page.getByText(success, { exact: true }).count(), 0)
    assert.equal(closeRequests.filter(path => path === closePath(northUnknown)).length, 1)

    // 用原登入重新載入重啟後的服務，並從歷程選取原本結案。
    const live = page.locator('#live-status').filter({ hasText: /^即時更新中$/ })
    await live.waitFor()
    restarting = true
    stack.stop()
    stack.start()
    await page.reload()
    await live.waitFor()
    restarting = false
    await historyButton.click()
    await row(northRecovered).click()
    await expectReadOnly(saved.closure)
    assert.deepEqual(await json(await send(detailPath(northRecovered), northCookie), 200, '重啟後結案讀回'), saved)
    assert.deepEqual(await json(await send(detailPath(northUnknown), northCookie), 200, '重啟後不明結果讀回'), committed)

    await page.getByRole('button', { name: '登出', exact: true }).click()
    await page.getByText('已登出。', { exact: true }).waitFor()
    assert.equal((await send(detailPath(northRecovered), northCookie)).status, 401)
    await browserSignIn('south')
    await historyButton.click()
    await row(southRecovered).waitFor()
    assert.equal(await row(northRecovered).count(), 0)
    assert.equal(await row(northUnknown).count(), 0)
    await row(southRecovered).click()
    await input.fill('登入失效前的草稿')
    // 只調整隔離測試的南區登入期限，讓原畫面送出時收到真正 401。
    assert.equal(stack.sql(`
      UPDATE sessions SET expires_at = statement_timestamp()
      WHERE username = 'south' RETURNING 1;
    `), '1')
    const expired = await submitResponse(southRecovered)
    assert.deepEqual(await json(expired, 401, '結案期間登入失效'), { error: 'unauthenticated' })
    await accountMessage.waitFor()
    assert.equal(await detailTitle.count(), 0)
    assert.equal(await input.count(), 0)
    await browserSignIn('south')
    await historyButton.click()
    await row(southRecovered).click()
    await page.getByText(recovered, { exact: true }).waitFor()
    assert.equal(await input.inputValue(), '', '重新登入應建立新草稿')
    await page.getByRole('button', { name: '登出', exact: true }).click()
    await page.getByText('已登出。', { exact: true }).waitFor()
    await browserSignIn('hq')
    await historyButton.click()
    await row(northRecovered).waitFor()
    await row(southRecovered).click()
    await page.getByText(recovered, { exact: true }).waitFor()
    assert.equal(await input.inputValue(), '', '切換帳號不得帶入原草稿')
    assert.deepEqual(closeRequests, [
      closePath(northRecovered), closePath(northRecovered), closePath(northRecovered),
      closePath(northUnknown), closePath(southRecovered),
    ])

    // 檢查實際 PostgreSQL 故障來源，以及整段操作中曾使用的敏感值。
    stack.collectLogs()
    const logs = stack.logs.join('\n')
    assert.ok(logs.includes('isolated browser closure fixture'), '應記錄隔離 PostgreSQL 故障')
    for (const secret of [...secrets, prefix.trim(), unknownDraft]) {
      assert.ok(secret && !logs.includes(secret), '日誌不可包含憑證或結案本文')
    }
    assert.equal(await page.locator('vite-error-overlay, #webpack-dev-server-client-overlay').count(), 0)
    assert.deepEqual(browserErrors, [], '瀏覽器不可有非預期錯誤')
  } finally {
    await browser.close()
  }
})
