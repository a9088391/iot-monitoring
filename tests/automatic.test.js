import assert from 'node:assert/strict'
import { EventEmitter, once } from 'node:events'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'
import { setTimeout as wait } from 'node:timers/promises'
import { createStack } from './stack.js'

const ids = ['north-01', 'north-02', 'north-03', 'south-01', 'south-02', 'south-03']
const listPath = '/api/devices?limit=50&offset=0'

// 只由正式回應推導顯示值；初始與後續採用值另核對資料庫序列及時間。
const textTemperature = value => `${Number.isInteger(value) ? value.toFixed(1) : value}°C`

// 真實啟動先於登入與瀏覽器；後半段 GET 夾具只控制讀取順序，不假冒控制寫入。
test('自動回報：六台真實首報、預選、慢讀取及行動版返回', { timeout: 600_000 }, async t => {
  const stack = await createStack()
  t.after(() => stack.cleanup())
  stack.start()
  const passwords = JSON.parse(readFileSync(join(stack.state, 'demo-passwords.json'), 'utf8'))
  const secrets = Object.values(passwords)
  const send = (path, cookie, body) => stack.send(path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { Cookie: cookie, ...(body === undefined ? {} : {
      Origin: stack.origin, 'X-CSRF-Protection': '1', 'Content-Type': 'application/json',
    }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  // 啟動資料在任何登入、模擬 POST 或頁面之前即存在。
  const samples = () => JSON.parse(stack.sql(`SELECT json_agg(json_build_object(
    'id', device_id, 'sequence', sequence::text, 'received', received_at_ns::text, 'temperature', temperature)
    ORDER BY device_id) FROM latest_measurements;`))
  const first = samples()
  assert.deepEqual(first.map(item => item.id), ids)
  assert.equal(new Set(first.map(item => item.temperature)).size, 6)
  assert.ok(first.every(item => item.temperature >= 18 && item.temperature <= 28))
  assert.equal(stack.sql('SELECT count(*) FROM simulation_history;'), '0', '啟動不得虛構人類控制')
  async function login(username) {
    const response = await send('/api/login', '', { username, password: passwords[username] })
    assert.equal(response.status, 200, '隔離帳號登入')
    const cookie = response.headers.getSetCookie()[0]?.split(';')[0]
    assert.ok(cookie?.startsWith('iot_session='))
    secrets.push(cookie.slice('iot_session='.length))
    return cookie
  }
  const cookie = await login('hq')
  async function inventory() {
    const response = await send(listPath, cookie)
    assert.equal(response.status, 200)
    return response.json()
  }
  const initial = await inventory()
  assert.deepEqual(initial.items.map(item => item.id), ids)
  assert.equal(new Set(initial.items.map(item => item.latestMeasurement.temperature)).size, 6)
  assert.ok(initial.items.every(item => item.latestMeasurement.temperature >= 18 && item.latestMeasurement.temperature <= 28))
  for (const item of initial.items) {
    assert.equal(item.onlineStatus, 'online')
    const response = await send(`/api/devices/${item.id}/simulation`, cookie)
    assert.equal(response.status, 200)
    const state = await response.json()
    assert.equal(state.mode, 'normal')
    assert.equal(state.canReplay, true)
  }
  const executablePath = process.env.CHROME_EXECUTABLE ?? '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
  assert.ok(existsSync(executablePath))
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  const { chromium } = await import('playwright-core')
  const browser = await chromium.launch({ executablePath, timeout: 15_000 })
  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
    await context.addCookies([{ name: 'iot_session', value: cookie.slice('iot_session='.length), url: stack.origin,
      httpOnly: true, sameSite: 'Strict' }])
    const page = await context.newPage()
    page.setDefaultTimeout(8_000)
    const errors = []
    const controls = []
    let expected503 = 0
    // 只容許明示的唯讀錯誤；錯誤診斷先移除所有已知憑證。
    function monitor(target) {
      const record = message => errors.push(secrets.reduce((value, secret) => value.replaceAll(secret, '[redacted]'), message).slice(0, 500))
      target.on('pageerror', error => record(error.message))
      target.on('console', message => {
        if (message.type() !== 'error') return
        const { url } = message.location()
        if (url === stack.origin + '/favicon.ico' && message.text().includes('status of 404')) return
        if (expected503 && url === stack.origin + listPath && message.text().includes('status of 503')) {
          expected503 -= 1
          return
        }
        record(`${message.text()} @ ${url}`)
      })
      target.on('request', request => {
        if (request.method() === 'POST' && /\/simulation$/.test(new URL(request.url()).pathname)) controls.push(request.url())
      })
    }
    monitor(page)
    const live = page.locator('#live-status').filter({ hasText: /^即時更新中$/ })
    const detail = name => page.getByRole('region', { name, exact: true })
    const northOne = detail('北區設備 01')
    const northTwo = detail('北區設備 02')
    const row = name => page.getByRole('region', { name: '設備監控', exact: true }).getByRole('button', { name: new RegExp(name) })
    const reload = page.locator('.reload-button')
    const history = page.getByRole('button', { name: '告警歷程', exact: true })
    const devices = page.getByRole('button', { name: '模擬展示', exact: true })
    await page.goto(stack.origin + '/?view=simulation')
    await northOne.locator('.temperature-value').waitFor()
    await live.waitFor()
    assert.equal(await row('北區設備 01').getAttribute('aria-pressed'), 'true')
    assert.equal(new Set(await page.locator('.device-list .row-temperature').allTextContents()).size, 6,
      '真實六筆不同值也須呈現在六列清單')

    // 等實際週期採用，不用固定睡眠或畫面自造值替代序列／時間證據。
    let next
    const deadline = Date.now() + 8_000
    while (Date.now() < deadline) {
      next = samples()
      if (next.every((item, index) => BigInt(item.sequence) > BigInt(first[index].sequence))) break
      await wait(100, undefined, { signal: t.signal })
    }
    for (const [index, item] of next.entries()) {
      assert.ok(BigInt(item.sequence) > BigInt(first[index].sequence))
      assert.ok(BigInt(item.received) > BigInt(first[index].received))
      assert.notEqual(item.temperature, first[index].temperature)
      assert.ok(item.temperature >= 18 && item.temperature <= 28)
    }
    const [readback] = await Promise.all([
      page.waitForResponse(response => response.url() === stack.origin + '/api/devices/north-01' && response.status() === 200),
      page.getByRole('button', { name: '重新載入設備狀態', exact: true }).click(),
    ])
    const current = await readback.json()
    await northOne.getByText(textTemperature(current.latestMeasurement.temperature), { exact: true }).waitFor()
    await row('北區設備 01').getByText(textTemperature(current.latestMeasurement.temperature), { exact: true }).waitFor()
    assert.equal(await northOne.locator('time').getAttribute('datetime'), current.latestMeasurement.receivedAt)
    await row('北區設備 02').click()
    await northTwo.locator('.temperature-value').waitFor()
    await Promise.all([
      page.waitForResponse(response => response.url() === stack.origin + listPath && response.status() === 200),
      reload.click(),
    ])
    await page.waitForFunction(() => !document.querySelector('.reload-button').disabled)
    await live.waitFor()
    assert.equal(await row('北區設備 02').getAttribute('aria-pressed'), 'true')
    await history.click()
    await devices.click()
    await northOne.locator('.temperature-value').waitFor()
    await live.waitFor()

    // 同一路徑 ABA：舊手動清單被取消，舊 finally 不能清掉新進頁的 loading。
    const reads = new EventEmitter()
    let hold = true
    await page.route(stack.origin + listPath, async route => {
      if (!hold) return route.continue()
      const release = Promise.withResolvers()
      const settled = Promise.withResolvers()
      const response = await inventory()
      reads.emit('ready', { route, response, release, settled })
      try {
        await release.promise
        await route.fulfill({ json: response })
      } finally {
        settled.resolve()
      }
    })
    const [[older]] = await Promise.all([once(reads, 'ready', { signal: AbortSignal.timeout(8_000) }), reload.click()])
    await history.click()
    const aborted = page.waitForEvent('requestfailed', { predicate: request => request === older.route.request() })
    const [[latest]] = await Promise.all([once(reads, 'ready', { signal: AbortSignal.timeout(8_000) }), devices.click()])
    assert.equal((await aborted).failure()?.errorText, 'net::ERR_ABORTED')
    older.release.resolve()
    await older.settled.promise
    assert.equal(await reload.isDisabled(), true)
    assert.equal(await reload.textContent().then(text => text.includes('載入中')), true)
    latest.release.resolve()
    await latest.settled.promise
    await northOne.locator('.temperature-value').waitFor()
    hold = false
    await live.waitFor()

    // 慢讀取期間手動改選，晚到清單仍不能把詳情搶回第一台。
    hold = true
    const [[selectionRead]] = await Promise.all([once(reads, 'ready', { signal: AbortSignal.timeout(8_000) }), reload.click()])
    await row('北區設備 02').click()
    await northTwo.locator('.temperature-value').waitFor()
    hold = false
    selectionRead.release.resolve()
    await selectionRead.settled.promise
    await page.waitForFunction(() => !document.querySelector('.reload-button').disabled)
    await live.waitFor()
    assert.equal(await row('北區設備 02').getAttribute('aria-pressed'), 'true')

    // 行動版返回也是明確選取意圖；晚到清單與下一輪 SSE 均不能再自動開詳情。
    await page.setViewportSize({ width: 390, height: 844 })
    hold = true
    const [[backRead]] = await Promise.all([once(reads, 'ready', { signal: AbortSignal.timeout(8_000) }), reload.click()])
    await page.getByRole('button', { name: /返回設備清單/ }).click()
    hold = false
    backRead.release.resolve()
    await backRead.settled.promise
    await page.waitForFunction(() => !document.querySelector('.reload-button').disabled)
    await live.waitFor()
    assert.equal(await row('北區設備 01').isVisible(), true)
    assert.equal(await page.locator('.device-details').count(), 0)
    const beforeMobile = samples()[0].sequence
    const mobileDeadline = Date.now() + 8_000
    while (samples()[0].sequence === beforeMobile && Date.now() < mobileDeadline) await wait(100, undefined, { signal: t.signal })
    const afterMobile = samples()[0]
    assert.notEqual(afterMobile.sequence, beforeMobile)
    await row('北區設備 01').getByText(textTemperature(afterMobile.temperature), { exact: true }).waitFor()
    await live.waitFor()
    assert.equal(await row('北區設備 01').isVisible(), true)
    assert.equal(await page.locator('.device-details').count(), 0)
    await page.unroute(stack.origin + listPath)

    // 分頁與空／錯誤清單只替換 GET，不增加正式資料或控制操作。
    await page.setViewportSize({ width: 1280, height: 900 })
    const original = await inventory()
    let pageKind = 'first'
    await page.route(stack.origin + '/api/devices?*', async route => {
      if (pageKind === 'error') return route.fulfill({ status: 503, json: { error: 'service_unavailable' } })
      const items = pageKind === 'empty' ? [] : pageKind === 'first' ? original.items.slice(0, 2) : original.items.slice(4)
      await route.fulfill({ json: { items, total: pageKind === 'empty' ? 0 : 100, limit: 50, offset: pageKind === 'second' ? 50 : 0 } })
    })
    await reload.click()
    await northOne.locator('.temperature-value').waitFor()
    // 舊詳情或模擬讀取尚未完成時，換頁仍須接手新頁的預選與取消範圍。
    for (const pending of [{ label: '設備資料', suffix: '' }, { label: '模擬狀態', suffix: '/simulation' }]) {
      await t.test(`SPEC-002：舊${pending.label}讀取不能阻止新頁預選`, async child => {
        pageKind = 'first'
        await reload.click()
        await row('北區設備 01').click()
        await northOne.locator('.temperature-value').waitFor()
        await page.waitForFunction(() => !document.querySelector('.reload-button').disabled)
        await live.waitFor()
        const readPath = '/api/devices/north-02' + pending.suffix
        const ready = new EventEmitter()
        const release = Promise.withResolvers()
        const settled = Promise.withResolvers()
        let oldRequest
        let abortedError
        const recordAbort = request => {
          if (request === oldRequest) {
            abortedError = request.failure()?.errorText
            ready.emit('aborted')
          }
        }
        const holdRead = async route => {
          oldRequest = route.request()
          try {
            assert.equal(oldRequest.method(), 'GET')
            const response = await send(readPath, cookie)
            assert.equal(response.status, 200)
            const snapshot = await response.json()
            ready.emit('held')
            await release.promise
            await route.fulfill({ json: snapshot })
          } finally {
            settled.resolve()
          }
        }
        page.on('requestfailed', recordAbort)
        await page.route(stack.origin + readPath, holdRead, { times: 1 })
        try {
          await Promise.all([
            once(ready, 'held', { signal: AbortSignal.timeout(8_000) }),
            row('北區設備 02').click(),
          ])
          const nextPage = page.getByRole('button', { name: '下一頁', exact: true })
          assert.equal(await nextPage.isEnabled(), true, '純讀取等待不等於控制寫入中的離開保護')
          pageKind = 'second'
          const [pageResponse] = await Promise.all([
            page.waitForResponse(response => response.url() === stack.origin + '/api/devices?limit=50&offset=50'
              && response.status() === 200),
            nextPage.click(),
          ])
          assert.deepEqual((await pageResponse.json()).items.map(item => item.id), ['south-02', 'south-03'])
          await row('南區設備 02').waitFor()
          assert.deepEqual(await page.locator('.device-list .device-name').allTextContents(), ['南區設備 02', '南區設備 03'])
          const selected = detail('南區設備 02')
          await selected.locator('.temperature-value').waitFor()
          assert.equal(await row('南區設備 02').getAttribute('aria-pressed'), 'true')
          if (abortedError === undefined) await once(ready, 'aborted', { signal: AbortSignal.timeout(8_000) })
          assert.equal(abortedError, 'net::ERR_ABORTED', '改選新頁首台須取消舊設備的讀取')
          release.resolve()
          await settled.promise
          assert.equal(await row('南區設備 02').getAttribute('aria-pressed'), 'true')
          assert.equal(await detail('北區設備 02').count(), 0, '晚回覆不得帶回前頁詳情')

          // 必須收到時間較新的真實背景 GET，不能只以已有的 live 標籤當作下一輪 SSE 證據。
          const baselineResponse = await send('/api/devices/south-02', cookie)
          assert.equal(baselineResponse.status, 200)
          const baseline = await baselineResponse.json()
          const updatedResponse = await page.waitForResponse(async response => {
            if (response.url() !== stack.origin + '/api/devices/south-02' || response.status() !== 200) return false
            const updated = await response.json()
            return Date.parse(updated.latestMeasurement.receivedAt) > Date.parse(baseline.latestMeasurement.receivedAt)
          })
          const updated = await updatedResponse.json()
          await selected.getByText(textTemperature(updated.latestMeasurement.temperature), { exact: true }).waitFor()
          await row('南區設備 02').getByText(textTemperature(updated.latestMeasurement.temperature), { exact: true }).waitFor()
          assert.equal(await selected.locator('time').getAttribute('datetime'), updated.latestMeasurement.receivedAt)
          assert.equal(await row('南區設備 02').getAttribute('aria-pressed'), 'true')
          assert.deepEqual(await page.locator('.device-list .device-name').allTextContents(), ['南區設備 02', '南區設備 03'])
        } finally {
          release.resolve()
          if (oldRequest) await settled.promise
          page.off('requestfailed', recordAbort)
          await page.unroute(stack.origin + readPath, holdRead)
          child.diagnostic(JSON.stringify({ oldRead: pending.label, cancelled: abortedError ?? null,
            list: await page.locator('.device-list .device-name').allTextContents(),
            detail: await page.locator('.detail-pane h2').allTextContents(), cleanup: 'released and unregistered' }))
        }
      })
    }
    await t.test('SPEC-002：較晚手選取消舊換頁並保留原頁', async child => {
      pageKind = 'first'
      await reload.click()
      await row('北區設備 01').click()
      await northOne.locator('.temperature-value').waitFor()
      await page.waitForFunction(() => !document.querySelector('.reload-button').disabled)
      const ready = new EventEmitter()
      const release = Promise.withResolvers()
      const settled = Promise.withResolvers()
      let oldPageRequest
      let cancelled
      const recordAbort = request => {
        if (request === oldPageRequest) cancelled = request.failure()?.errorText
      }
      // 此例依真實 offset 回頁，避免後續 SSE 用舊的 fixture phase 把錯誤頁面洗回去。
      const pageRoute = async route => {
        const second = new URL(route.request().url()).searchParams.get('offset') === '50'
        if (second && !oldPageRequest) {
          oldPageRequest = route.request()
          try {
            ready.emit('held')
            await release.promise
            await route.fulfill({ json: { items: original.items.slice(4), total: 100, limit: 50, offset: 50 } })
          } finally {
            settled.resolve()
          }
        } else await route.fulfill({ json: { items: second ? original.items.slice(4) : original.items.slice(0, 2),
          total: 100, limit: 50, offset: second ? 50 : 0 } })
      }
      page.on('requestfailed', recordAbort)
      await page.route(stack.origin + '/api/devices?*', pageRoute)
      try {
        await Promise.all([
          once(ready, 'held', { signal: AbortSignal.timeout(8_000) }),
          page.getByRole('button', { name: '下一頁', exact: true }).click(),
        ])
        await row('北區設備 02').click()
        await northTwo.locator('.temperature-value').waitFor()
        await northTwo.getByText('模擬模式：正常回報', { exact: true }).waitFor()
        assert.equal(await northTwo.getByRole('button', { name: '恢復正常', exact: true }).isEnabled(), true)
        release.resolve()
        await settled.promise
        await page.waitForFunction(() => !document.querySelector('.reload-button').disabled)
        assert.deepEqual(await page.locator('.device-list .device-name').allTextContents(), ['北區設備 01', '北區設備 02'],
          '較晚手選必須保留其原頁，不能採用較早換頁結果')
        assert.equal(cancelled, 'net::ERR_ABORTED')
        assert.equal(await row('北區設備 02').getAttribute('aria-pressed'), 'true')
        const baselineResponse = await send('/api/devices/north-02', cookie)
        assert.equal(baselineResponse.status, 200)
        const baseline = await baselineResponse.json()
        const newerResponse = await page.waitForResponse(async response => response.url() === stack.origin + '/api/devices/north-02'
          && response.status() === 200
          && Date.parse((await response.json()).latestMeasurement.receivedAt) > Date.parse(baseline.latestMeasurement.receivedAt))
        const newer = await newerResponse.json()
        await northTwo.getByText(textTemperature(newer.latestMeasurement.temperature), { exact: true }).waitFor()
        await row('北區設備 02').getByText(textTemperature(newer.latestMeasurement.temperature), { exact: true }).waitFor()
        await reload.click()
        await page.waitForFunction(() => !document.querySelector('.reload-button').disabled)
        assert.equal(await row('北區設備 02').getAttribute('aria-pressed'), 'true')
        await page.getByRole('button', { name: '下一頁', exact: true }).click()
        await detail('南區設備 02').locator('.temperature-value').waitFor()
        assert.equal(await row('南區設備 02').getAttribute('aria-pressed'), 'true')
      } finally {
        release.resolve()
        if (oldPageRequest) await settled.promise
        page.off('requestfailed', recordAbort)
        await page.unroute(stack.origin + '/api/devices?*', pageRoute)
        child.diagnostic(JSON.stringify({ case: 'later-manual-selection', cancelled: cancelled ?? null,
          list: await page.locator('.device-list .device-name').allTextContents(),
          detail: await page.locator('.detail-pane h2').allTextContents(), cleanup: 'released and unregistered' }))
      }
    })
    pageKind = 'first'
    await reload.click()
    await row('北區設備 01').click()
    await northOne.locator('.temperature-value').waitFor()
    await page.waitForFunction(() => !document.querySelector('.reload-button').disabled)
    pageKind = 'second'
    await page.getByRole('button', { name: '下一頁', exact: true }).click()
    await detail('南區設備 02').locator('.temperature-value').waitFor()
    assert.deepEqual(await page.locator('.device-list .device-name').allTextContents(), ['南區設備 02', '南區設備 03'])
    const detailsRequested = []
    const trackDetail = request => {
      if (/\/api\/devices\/[^/]+$/.test(new URL(request.url()).pathname)) detailsRequested.push(request.url())
    }
    page.on('request', trackDetail)
    pageKind = 'empty'
    await reload.click()
    await page.getByText('目前沒有可查看的設備。', { exact: true }).waitFor()
    assert.equal(await page.locator('.device-details').count(), 0)
    pageKind = 'error'
    expected503 = 1
    await reload.click()
    await page.getByRole('alert').filter({ hasText: '服務暫時無法使用，請稍後重試。' }).waitFor()
    assert.deepEqual(detailsRequested, [], '空或失敗清單不可對不存在的預選設備發 GET')
    page.off('request', trackDetail)
    await page.unroute(stack.origin + '/api/devices?*')

    const northCookie = await login('north')
    const mobile = await browser.newContext({ viewport: { width: 390, height: 844 } })
    await mobile.addCookies([{ name: 'iot_session', value: northCookie.slice('iot_session='.length), url: stack.origin,
      httpOnly: true, sameSite: 'Strict' }])
    const mobilePage = await mobile.newPage()
    mobilePage.setDefaultTimeout(8_000)
    monitor(mobilePage)
    await mobilePage.goto(stack.origin + '/?view=simulation')
    await mobilePage.getByRole('region', { name: '北區設備 01', exact: true }).locator('.temperature-value').waitFor()
    assert.equal(await mobilePage.locator('.device-list > li').count(), 3)
    assert.equal(await mobilePage.locator('.device-list').textContent().then(text => text.includes('南區')), false)
    assert.equal(await mobilePage.getByRole('heading', { name: '展示情境', exact: true }).count(), 0)
    await mobilePage.getByRole('button', { name: /返回設備清單/ }).click()
    await mobilePage.getByRole('button', { name: /北區設備 01/ }).waitFor()
    await mobilePage.getByRole('button', { name: '登出', exact: true }).click()
    await mobilePage.getByText('已登出。', { exact: true }).waitFor()
    assert.equal(await mobilePage.locator('.device-list, .device-details').count(), 0)
    assert.deepEqual(controls, [], '監控、預選、讀取與導航不可自動送出模擬 POST')
    assert.deepEqual(errors, [])
  } finally {
    await browser.close()
  }
})
