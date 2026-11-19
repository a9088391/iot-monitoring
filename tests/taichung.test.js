import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'
import { createStack } from './stack.js'

const apiPath = '/api/taichung/sensors'
const sourceUrl = 'https://newdatacenter.taichung.gov.tw/api/v1/no-auth/resource.download?rid=5a16b622-95ca-4e80-9d9f-e62a42962f4e'
const measuredAt = '2026-11-19T02:44:47Z'
const fetchedAt = '2026-11-19T03:00:00Z'
const timeText = new Intl.DateTimeFormat('zh-TW', {
  timeZone: 'Asia/Taipei', dateStyle: 'short', timeStyle: 'medium', hourCycle: 'h23',
})
const temperatureText = value => Number.isInteger(value) ? value.toFixed(1) : String(value)

// DTO 夾具控制讀取順序與來源缺值；不代表已連線政府來源，也不替代真實登入或模擬寫入。
function snapshot(prefix, count = 6) {
  return {
    items: Array.from({ length: count }, (_, index) => ({
      id: `${prefix}-${index + 1}`, device: `環境感測器${prefix}${index + 1}`,
      district: index === 0 ? '和平區' : '南區',
      landmark: index === 0 ? '臺中市和平區梨山里環山部落公共活動中心旁環境感測站' : `市民公園${index + 1}`,
      name: index === 0 ? '和平區 · 臺中市和平區梨山里環山部落公共活動中心旁環境感測站' : `南區 · 市民公園${index + 1}`,
      temperature: 11.25 + index, humidity: index === 0 ? null : 60 + index,
      pm25: index === 0 ? null : 10.5 + index, measuredAt,
    })),
    total: count === 6 ? 1164 : count, fetchedAt, sourceUrl,
  }
}

// 真實隔離 app／PostgreSQL 驗證身分；只在瀏覽器邊界提供官方 GET 的可控回應。
test('官方資料：抽選、同步、錯誤、行動版返回與登入生命週期', { timeout: 600_000 }, async t => {
  const stack = await createStack()
  t.after(() => stack.cleanup())
  stack.start()
  const passwords = JSON.parse(readFileSync(join(stack.state, 'demo-passwords.json'), 'utf8'))
  const secrets = Object.values(passwords)
  // 保留正式 Origin、CSRF 與 Cookie 規則，任何未授權回應都由實際路由決定。
  const send = (path, cookie = '', body) => stack.send(path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { Cookie: cookie, ...(body === undefined ? {} : {
      Origin: stack.origin, 'X-CSRF-Protection': '1', 'Content-Type': 'application/json',
    }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  assert.equal((await send(apiPath)).status, 401)
  // 所有會話都由實際登入 API 建立，避免用合成 session 冒充整合覆蓋。
  async function login(username) {
    const response = await send('/api/login', '', { username, password: passwords[username] })
    assert.equal(response.status, 200)
    const cookie = response.headers.getSetCookie()[0]?.split(';')[0]
    assert.ok(cookie?.startsWith('iot_session='))
    secrets.push(cookie.slice('iot_session='.length))
    return cookie
  }
  const hqCookie = await login('hq')
  const executablePath = process.env.CHROME_EXECUTABLE ?? '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
  assert.ok(existsSync(executablePath), '需要已安裝的 Chrome')
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  const { chromium } = await import('playwright-core')
  const browser = await chromium.launch({ executablePath, timeout: 15_000 })
  const contexts = []
  const errors = []
  let expected503 = 0
  // 預期資源錯誤依 URL、狀態和次數分類；其餘 console 與腳本錯誤必須失敗。
  function monitor(page) {
    const record = message => errors.push(secrets.reduce((value, secret) => value.replaceAll(secret, '[redacted]'), message).slice(0, 500))
    page.on('pageerror', error => record(error.message))
    page.on('console', message => {
      if (message.type() !== 'error') return
      const url = message.location().url
      if (url === stack.origin + '/favicon.ico' && message.text().includes('status of 404')) return
      if (url === stack.origin + apiPath && expected503 > 0 && message.text().includes('status of 503')) {
        expected503 -= 1
        return
      }
      record(`${message.text()} @ ${url}`)
    })
  }
  // 每個案例有獨立頁面與 Cookie；攔截範圍只有官方感測器的 GET。
  async function open(data, { width = 1280, cookie = hqCookie } = {}) {
    const context = await browser.newContext({ viewport: { width, height: width === 375 ? 812 : 900 } })
    contexts.push(context)
    await context.addCookies([{ name: 'iot_session', value: cookie.slice('iot_session='.length), url: stack.origin,
      httpOnly: true, sameSite: 'Strict' }])
    const page = await context.newPage()
    page.setDefaultTimeout(8_000)
    monitor(page)
    const requests = []
    let respond = async route => route.fulfill({ json: data })
    await page.route('**/api/taichung/sensors*', async route => {
      assert.equal(route.request().method(), 'GET')
      requests.push(new URL(route.request().url()))
      await respond(route)
    })
    await page.clock.install()
    await page.goto(stack.origin)
    return { page, requests, context, respond: handler => { respond = handler } }
  }
  // 從同一 DTO 核對每列身份、名稱、數值與來源時間，抓取時間另有獨立語意。
  async function assertRows(page, data) {
    const rows = page.locator('.official-device-button')
    await page.waitForFunction(items => {
      const elements = [...document.querySelectorAll('.official-device-button')]
      return elements.length === items.length && elements.every((element, index) => {
        const item = items[index]
        const temperature = Number.isInteger(item.temperature) ? item.temperature.toFixed(1) : String(item.temperature)
        return element.dataset.sensorId === item.id
          && element.querySelector('time')?.dateTime === item.measuredAt
          && element.querySelector('.official-row-temperature')?.textContent === `${temperature}°C`
      })
    }, data.items)
    assert.deepEqual(await rows.evaluateAll(elements => elements.map(element => element.dataset.sensorId)), data.items.map(item => item.id))
    assert.equal(new Set(await rows.evaluateAll(elements => elements.map(element => element.dataset.sensorId))).size, data.items.length)
    for (const item of data.items) {
      const actual = page.locator(`[data-sensor-id="${item.id}"]`)
      assert.ok((await actual.innerText()).includes(item.name))
      assert.ok((await actual.innerText()).includes(`${item.device} · ${item.id}`))
      assert.equal(await actual.locator('.official-row-temperature').innerText(), `${temperatureText(item.temperature)}°C`)
      assert.equal(await actual.locator('time').getAttribute('datetime'), item.measuredAt)
      assert.equal(await actual.locator('time').innerText(), `量測：${timeText.format(new Date(item.measuredAt))}`)
    }
    assert.equal(await page.locator('.official-fetch-time time').getAttribute('datetime'), data.fetchedAt)
    assert.equal(await page.getByRole('link', { name: '官方 JSON 來源', exact: true }).getAttribute('href'), sourceUrl)
    await page.getByRole('status').filter({ hasText: '正在讀取官方資料' }).waitFor({ state: 'hidden' })
  }
  // 詳情必須與選取列同筆，不能以快照抓取時間或其他設備數值替代。
  async function assertDetail(page, item) {
    await page.getByRole('heading', { name: item.name, exact: true }).waitFor()
    const detail = page.locator('.official-detail-pane')
    assert.equal(await detail.locator('.official-device-identity').innerText(), `${item.device} · ${item.id}`)
    assert.equal((await detail.locator('.official-temperature dd').innerText()).replace(/\s/g, ''), `${temperatureText(item.temperature)}°C`)
    assert.deepEqual(await detail.locator('.official-details dd').allTextContents(), [
      `${temperatureText(item.temperature)}°C`, item.humidity === null ? '來源未提供有效值' : `${item.humidity}%`,
      item.pm25 === null ? '來源未提供有效值' : `${item.pm25} μg/m³`, timeText.format(new Date(item.measuredAt)),
    ])
    assert.equal(await detail.locator('time').getAttribute('datetime'), item.measuredAt)
  }
  // 依正式 60 秒 timer 觸發一次背景讀取，不用真實等待，也不改正式排程程式。
  async function poll(page) {
    const response = page.waitForResponse(response => new URL(response.url()).pathname === apiPath)
    await page.clock.fastForward(60_001)
    await response
    await page.getByRole('status').filter({ hasText: '正在讀取官方資料' }).waitFor({ state: 'hidden' })
  }
  try {
    await t.test('預設六筆同列一致、鍵盤選取、固定 ID 同步、手動與瀏覽器重抽及導覽', async () => {
      const first = snapshot('first')
      const fixture = await open(first)
      const { page, requests } = fixture
      await assertRows(page, first)
      await assertDetail(page, first.items[0])
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '1280px 長名稱不可水平溢出')
      assert.equal(requests[0].search, '')
      const secondRow = page.locator(`[data-sensor-id="${first.items[1].id}"]`)
      await secondRow.focus()
      await page.keyboard.press('Enter')
      await assertDetail(page, first.items[1])
      const updated = structuredClone(first)
      updated.items.forEach(item => { item.temperature += 1; item.measuredAt = '2026-11-19T02:45:47Z' })
      updated.fetchedAt = '2026-11-19T03:10:00Z'
      fixture.respond(route => route.fulfill({ json: updated }))
      await poll(page)
      assert.deepEqual(requests.at(-1).searchParams.get('ids').split(','), first.items.map(item => item.id))
      await assertRows(page, updated)
      await assertDetail(page, updated.items[1])
      const next = snapshot('manual')
      fixture.respond(route => route.fulfill({ json: next }))
      await page.locator('.official-resample').focus()
      await page.keyboard.press('Enter')
      await assertRows(page, next)
      await assertDetail(page, next.items[0])
      assert.equal(requests.at(-1).search, '')
      const reloaded = snapshot('reload')
      fixture.respond(route => route.fulfill({ json: reloaded }))
      await page.reload()
      await assertRows(page, reloaded)
      assert.equal(requests.at(-1).search, '')
      await page.getByRole('button', { name: '模擬展示', exact: true }).click()
      await page.getByRole('region', { name: '設備監控', exact: true }).waitFor()
      assert.equal(new URL(page.url()).searchParams.get('view'), 'simulation')
      await page.reload()
      await page.getByRole('region', { name: '設備監控', exact: true }).waitFor()
      await page.getByRole('button', { name: '告警歷程', exact: true }).click()
      await page.getByRole('region', { name: '告警歷程', exact: true }).waitFor()
      await page.getByRole('button', { name: '設備清單', exact: true }).click()
      await assertRows(page, reloaded)
      assert.equal(new URL(page.url()).searchParams.has('view'), false)
      await page.reload()
      await assertRows(page, reloaded)
      await fixture.context.close()
    })
    await t.test('初始失敗與已載入後失敗均明示、可重試；空與不足六台如實呈現', async () => {
      const fixture = await open(snapshot('initial'))
      const { page } = fixture
      await assertRows(page, snapshot('initial'))
      // 重新開頁讓第一個官方請求失敗，避免只覆蓋已有快照的錯誤分支。
      fixture.respond(route => { expected503 += 1; return route.fulfill({ status: 503, json: { error: 'service_unavailable' } }) })
      await page.reload()
      const warning = page.getByRole('alert')
      await warning.filter({ hasText: '目前沒有可顯示的官方資料' }).waitFor()
      assert.equal(await page.locator('.official-device-button').count(), 0)
      const recovered = snapshot('recovered', 2)
      fixture.respond(route => route.fulfill({ json: recovered }))
      await page.getByRole('button', { name: '重試官方資料', exact: true }).click()
      await assertRows(page, recovered)
      await page.getByText('目前顯示 2 台 / 來源有效設備 2 台；', { exact: false }).waitFor()
      fixture.respond(route => { expected503 += 1; return route.fulfill({ status: 503, json: { error: 'service_unavailable' } }) })
      await page.locator('.official-resample').click()
      await warning.filter({ hasText: '保留上次成功快照，並非本次更新結果' }).waitFor()
      await assertRows(page, recovered)
      const empty = snapshot('empty', 0)
      fixture.respond(route => route.fulfill({ json: empty }))
      await page.getByRole('button', { name: '重試官方資料', exact: true }).click()
      await assertRows(page, empty)
      assert.equal(await warning.count(), 0)
      await page.getByText('目前沒有可顯示的官方感測資料。請稍後重新抽選。', { exact: true }).waitFor()
      const before = fixture.requests.length
      await page.clock.fastForward(60_001)
      assert.equal(fixture.requests.length, before, '空清單不得自行補抽')
      fixture.respond(route => route.fulfill({ json: snapshot('after-empty') }))
      await page.locator('.official-resample').click()
      await assertRows(page, snapshot('after-empty'))
      await fixture.context.close()
    })
    await t.test('375／760／761px 長名稱無溢出；返回清單後同步不搶選，明確重抽仍預選', async () => {
      const data = snapshot('mobile')
      const fixture = await open(data, { width: 375 })
      const { page } = fixture
      await assertRows(page, data)
      for (const width of [375, 760, 761]) {
        await page.setViewportSize({ width, height: width === 375 ? 812 : 900 })
        assert.equal(await page.evaluate(() => innerWidth), width)
        await assertDetail(page, data.items[0])
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${width}px 詳情不可水平溢出`)
        if (width <= 760) {
          await page.getByRole('button', { name: '← 返回官方設備清單', exact: true }).click()
          await page.locator('.official-list-pane').waitFor({ state: 'visible' })
          const changed = structuredClone(data)
          changed.items[0].temperature = 9.75
          fixture.respond(route => route.fulfill({ json: changed }))
          await poll(page)
          await page.locator('.official-list-pane').waitFor({ state: 'visible' })
          assert.equal(await page.locator('.official-detail-pane').isVisible(), false, 'TAICHUNG-001：被動同步不得搶走行動版返回')
          assert.equal(await page.locator('[aria-pressed="true"][data-sensor-id]').count(), 0)
          assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${width}px 長名稱清單不可水平溢出`)
          fixture.respond(route => route.fulfill({ json: data }))
          await page.locator('.official-resample').click()
          await assertDetail(page, data.items[0])
        }
      }
      await fixture.context.close()
    })
    await t.test('連續重抽取消舊讀取；登出撤銷持有回應，場域帳號不繼承私有資料', async () => {
      const fixture = await open(snapshot('race'))
      const { page } = fixture
      await assertRows(page, snapshot('race'))
      let release
      let entered
      const reached = new Promise(resolve => { entered = resolve })
      const gate = new Promise(resolve => { release = resolve })
      let settled
      const finished = new Promise(resolve => { settled = resolve })
      let calls = 0
      fixture.respond(async route => {
        if (++calls === 1) {
          entered()
          await gate
          try { await route.fulfill({ json: snapshot('stale') }) } finally { settled() }
        }
        else await route.fulfill({ json: snapshot('newer') })
      })
      try {
        await page.locator('.official-resample').click()
        await reached
        const cancelled = page.waitForEvent('requestfailed', request => new URL(request.url()).pathname === apiPath)
        await page.locator('.official-resample').click()
        assert.equal((await cancelled).failure().errorText, 'net::ERR_ABORTED')
        await assertRows(page, snapshot('newer'))
      } finally { release() }
      await finished
      await page.clock.runFor(1)
      await assertRows(page, snapshot('newer'))
      let releaseLogout
      let enteredLogout
      const logoutReached = new Promise(resolve => { enteredLogout = resolve })
      const logoutGate = new Promise(resolve => { releaseLogout = resolve })
      let logoutSettled
      const logoutFinished = new Promise(resolve => { logoutSettled = resolve })
      fixture.respond(async route => {
        enteredLogout()
        await logoutGate
        try { await route.fulfill({ json: snapshot('old-hq') }) } finally { logoutSettled() }
      })
      try {
        await page.locator('.official-resample').click()
        await logoutReached
        const cancelled = page.waitForEvent('requestfailed', request => new URL(request.url()).pathname === apiPath)
        const response = page.waitForResponse(response => response.url() === stack.origin + '/api/logout')
        await page.getByRole('button', { name: '登出', exact: true }).click()
        assert.equal((await response).status(), 204)
        assert.equal((await cancelled).failure().errorText, 'net::ERR_ABORTED')
        await page.getByRole('button', { name: '登入', exact: true }).waitFor()
      } finally { releaseLogout() }
      await logoutFinished
      await page.clock.runFor(1)
      assert.equal(await page.locator('.official-device-button').count(), 0)
      assert.equal((await send(apiPath, hqCookie)).status, 401, '舊登入必須由真實 app 撤銷')
      fixture.respond(route => route.fulfill({ json: snapshot('north-public') }))
      // 避免測試報告包含 fill 的密碼參數，登入操作只回報固定失敗訊息。
      try {
        await page.getByLabel('帳號', { exact: true }).fill('north')
        await page.getByLabel('密碼', { exact: true }).fill(passwords.north)
        await page.getByRole('button', { name: '登入', exact: true }).click()
      } catch { throw new Error('場域帳號真實登入表單失敗') }
      await assertRows(page, snapshot('north-public'))
      assert.equal(await page.getByText(/old-hq|newer-\d|環境感測器race/).count(), 0)
      await page.getByRole('button', { name: '模擬展示', exact: true }).click()
      const board = page.getByRole('region', { name: '設備監控', exact: true })
      await board.getByRole('listitem').first().waitFor()
      assert.equal(await board.getByRole('listitem').count(), 3)
      assert.equal(await page.getByRole('button', { name: /南區設備/ }).count(), 0)
      assert.equal(await page.getByRole('heading', { name: '展示情境', exact: true }).count(), 0)
      await page.getByRole('button', { name: '設備清單', exact: true }).click()
      await assertRows(page, snapshot('north-public'))
      await fixture.context.close()
    })
    assert.equal(expected503, 0, '預期 503 應有對應瀏覽器診斷')
    assert.deepEqual(errors, [], '非預期瀏覽器錯誤')
  } finally {
    await Promise.all(contexts.map(context => context.close()))
    await browser.close()
  }
})
