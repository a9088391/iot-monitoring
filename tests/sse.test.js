import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { EventEmitter, once } from 'node:events'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'
import { createStack } from './stack.js'

const listPath = '/api/devices?limit=50&offset=0'
const eventPath = '/api/events'
const sessionPath = '/api/session'
const unauthorized = 'Failed to load resource: the server responded with a status of 401 (Unauthorized)'
const unavailable = 'Failed to load resource: the server responded with a status of 503 (Service Unavailable)'

// 確認真實 API 的狀態後再使用 JSON，讓失敗訊息不含請求或憑證。
async function json(response, status, phase) {
  assert.equal(response.status, status, phase)
  return response.json()
}

// 以已登入的北區瀏覽器驗證實際串流、重連及舊登入失效。
test('IoT 票 06：瀏覽器即時快照、斷線重連與身分隔離', { timeout: 600_000 }, async t => {
  const stack = await createStack()
  t.after(() => stack.cleanup())
  stack.start()
  const passwords = JSON.parse(readFileSync(join(stack.state, 'demo-passwords.json'), 'utf8'))
  const secrets = Object.values(passwords)

  // 由隔離環境送正式 HTTP 操作，保留應用程式的權限與寫入保護。
  function send(path, cookie, body, method = 'GET') {
    return stack.send(path, {
      method,
      headers: {
        Cookie: cookie,
        ...(method === 'POST' ? {
          Origin: stack.origin, 'X-CSRF-Protection': '1', 'Content-Type': 'application/json',
        } : {}),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
  }

  // 取得服務簽發的 Cookie，供獨立控制端及瀏覽器原登入共用。
  async function login(username) {
    const response = await send('/api/login', '', { username, password: passwords[username] }, 'POST')
    assert.equal(response.status, 200, '隔離帳號登入')
    const cookie = response.headers.getSetCookie()[0]?.split(';')[0]
    assert.ok(cookie?.startsWith('iot_session='), '應取得正式登入')
    secrets.push(cookie.slice('iot_session='.length))
    return cookie
  }

  // 只縮短夾具指定南區 Cookie 的期限，SQL 只接觸雜湊值。
  function shortenExpiry(cookie, seconds) {
    const hash = createHash('sha256')
      .update(cookie.slice('iot_session='.length))
      .digest('hex')
    const expiresAt = Number(stack.sql(`
      UPDATE sessions SET expires_at = statement_timestamp() + make_interval(secs => ${seconds})
      WHERE username = 'south' AND token_hash = decode('${hash}', 'hex')
      RETURNING floor(extract(epoch FROM expires_at) * 1000)::bigint;
    `))
    assert.ok(expiresAt > Date.now(), '只可更新目前南區登入，且期限應尚未到達')
    return expiresAt
  }

  const hqCookie = await login('hq')
  const northCookie = await login('north')
  // 以讀回的真實版本控制，並確認服務已提交這次模式。
  async function control(device, action) {
    const path = `/api/devices/${device}/simulation`
    const { version } = await json(await send(path, hqCookie), 200, '讀取控制版本')
    const updated = await json(await send(path, hqCookie, { action, version }, 'POST'), 200, '送出控制')
    assert.equal(updated.mode, action === 'stop' ? 'stopped' : action)
  }

  await control('north-01', 'normal')
  // 暫停週期回報，讓延遲快照期間只收到測試明確提交的變動。
  await control('north-01', 'stop')
  await control('south-01', 'abnormal')
  await control('south-01', 'normal')
  const { items: seededAlerts } = await json(await send('/api/alerts', hqCookie), 200, '讀取南區告警')
  const southAlert = seededAlerts.find(item => item.deviceId === 'south-01' && item.type === 'temperature')
  assert.equal(southAlert?.status, 'recovered')

  const executablePath = process.env.CHROME_EXECUTABLE ?? [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/google-chrome', '/usr/bin/chromium',
  ].find(path => existsSync(path))
  assert.ok(executablePath && existsSync(executablePath), '請安裝 Chrome／Chromium 或設定 CHROME_EXECUTABLE')
  // 禁用可能輸出登入表單或瀏覽器通訊的除錯紀錄。
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  const { chromium } = await import('playwright-core')
  const browser = await chromium.launch({ executablePath, timeout: 15_000 })
  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
    await context.addCookies([{
      name: 'iot_session', value: northCookie.slice('iot_session='.length), url: stack.origin,
      httpOnly: true, sameSite: 'Strict',
    }])
    const page = await context.newPage()
    page.setDefaultTimeout(8_000)
    await page.clock.install()
    const liveStatus = page.locator('#live-status')
    const live = liveStatus.filter({ hasText: /^即時更新中$/ })
    const disconnected = liveStatus.filter({ hasText: /^即時連線中斷，資料可能過時。$/ })
    const stale = liveStatus.filter({ hasText: /^資料待更新。$/ })
    const northDevice = page.getByRole('button', { name: /北區設備 01/ })
    const southDevice = page.getByRole('button', { name: /南區設備 01/ })
    const history = page.getByRole('button', { name: '告警歷程', exact: true })
    const devices = page.getByRole('button', { name: '設備清單', exact: true })
    const retry = page.getByRole('button', { name: '重新同步', exact: true })
    const expiredMessage = page.getByText('登入狀態已失效，請重新登入。', { exact: true })
    // 登入失敗時使用固定診斷，避免瀏覽器工具將表單值寫入日誌。
    async function signIn(username) {
      try {
        await page.getByLabel('帳號', { exact: true }).fill(username)
        const password = page.getByLabel('密碼', { exact: true })
        await password.fill(passwords[username])
        await password.press('Enter')
      } catch {
        throw new Error('瀏覽器登入表單操作失敗')
      }
    }

    // 告警以 ID 選取，避免同設備重複異常時誤開另一筆。
    const alertRow = item => page.getByRole('button', { name: new RegExp(`#${item.id} · 溫度異常 · ${item.deviceId}`) })
    const errors = []
    let favicon404 = 0
    let restarting = false
    let failedProbe = false
    let noStream = false
    let revoked = false
    let rotating = false
    let southExpiresAt = null
    let expiry401 = false
    // 診斷只記錄非預期錯誤，並先移除已知的憑證值。
    function recordError(message) {
      const redacted = secrets.reduce((value, secret) => value.replaceAll(secret, '[redacted]'), message)
      errors.push(redacted.slice(0, 300))
    }
    page.on('pageerror', error => recordError(error.message))
    page.on('console', message => {
      if (message.type() !== 'error') return
      const { url } = message.location()
      const text = message.text()
      const isLiveEndpoint = url === stack.origin + eventPath || url === stack.origin + sessionPath
      if (
        url === stack.origin + '/favicon.ico'
        && text.includes('status of 404')
        && favicon404 === 0
      ) {
        favicon404 += 1
        return
      }
      if (
        restarting
        && isLiveEndpoint
        && /net::ERR_(CONNECTION_REFUSED|CONNECTION_RESET|EMPTY_RESPONSE)$/.test(text)
      ) return
      if (
        restarting
        && failedProbe
        && url === stack.origin + sessionPath
        && text === unavailable
      ) return
      if (
        noStream
        && url === stack.origin + eventPath
        && text === unavailable
      ) return
      if (
        (revoked || rotating)
        && isLiveEndpoint
        && text === unauthorized
      ) return
      // 期限到達時，若伺服器先關閉串流，只允許隨後的一次身分查驗 401。
      if (
        southExpiresAt !== null
        && Date.now() >= southExpiresAt
        && !expiry401
        && url === stack.origin + sessionPath
        && text === unauthorized
      ) {
        expiry401 = true
        return
      }
      recordError(`${text} @ ${url}`)
    })

    // 直接觀察 Chrome 收到的真實 SSE 訊框，讓快照釋放與重連依實際版本排序。
    const network = await context.newCDPSession(page)
    await network.send('Network.enable')
    let version = null
    let frames = 0
    let streamExpiresAt = null
    network.on('Network.eventSourceMessageReceived', frame => {
      frames += 1
      if (frame.eventName !== 'version') return
      const event = JSON.parse(frame.data)
      version = BigInt(event.version)
      streamExpiresAt = Date.parse(event.expiresAt)
    })
    const snapshotRead = new EventEmitter()
    const releaseSnapshot = Promise.withResolvers()
    let reads = 0
    // 第一筆清單是一般載入，延遲下一筆由串流觸發的真實快照。
    await page.route(stack.origin + listPath, async route => {
      reads += 1
      if (reads !== 2) {
        await route.continue()
        return
      }
      const response = await send(listPath, northCookie)
      const snapshot = await json(response, 200, '延遲的北區快照')
      snapshotRead.emit('ready', snapshot)
      await releaseSnapshot.promise
      await route.fulfill({ json: snapshot })
    })
    try {
      const [[older]] = await Promise.all([
        once(snapshotRead, 'ready', { signal: AbortSignal.timeout(8_000) }),
        page.goto(stack.origin),
      ])
      assert.deepEqual(older.items.map(item => item.id), ['north-01', 'north-02', 'north-03'])
      assert.equal(older.items[0].latestMeasurement.temperature, 23)
      await northDevice.getByText('在線 · 23°C', { exact: true }).waitFor()
      await liveStatus.filter({ hasText: /^正在同步…$/ }).waitFor()
      const previousVersion = version
      assert.ok(previousVersion !== null, '延遲快照前應已收到初始版本')
      // 服務提交異常溫度後，先確認變動已進入瀏覽器，才放行舊的 23°C 快照。
      const [[nextFrame]] = await Promise.all([
        once(network, 'Network.eventSourceMessageReceived', { signal: AbortSignal.timeout(8_000) }),
        control('north-01', 'abnormal'),
      ])
      assert.equal(nextFrame.eventName, 'version')
      assert.ok(BigInt(JSON.parse(nextFrame.data).version) > previousVersion)
      assert.equal(await northDevice.getByText('在線 · 30°C', { exact: true }).count(), 0)
    } finally {
      releaseSnapshot.resolve()
    }
    await northDevice.getByText('在線 · 30°C', { exact: true }).waitFor()
    await live.waitFor()
    assert.ok(reads >= 3, '延遲快照期間的通知須觸發後續讀取')
    await page.unroute(stack.origin + listPath)
    assert.equal(await southDevice.count(), 0)

    // 告警詳情自行從異常轉為恢復，再顯示另一個已授權操作者提交的結案。
    await history.press('Enter')
    const { items: northAlerts } = await json(await send('/api/alerts', northCookie), 200, '讀取北區告警')
    const northAlert = northAlerts.find(item => item.deviceId === 'north-01' && item.type === 'temperature')
    assert.equal(northAlert?.status, 'active')
    await alertRow(northAlert).waitFor()
    assert.equal(await alertRow(southAlert).count(), 0)
    await alertRow(northAlert).press('Enter')
    await page.getByText('異常持續中', { exact: true }).waitFor()
    await live.waitFor()
    await control('north-01', 'normal')
    await page.getByText('已恢復、待結案', { exact: true }).waitFor()
    const note = '總部確認已復原'
    const closed = await json(await send(`/api/alerts/${northAlert.id}/close`, hqCookie, { result: note }, 'POST'),
      200, '總部提交結案')
    assert.equal(closed.closure.actor, 'hq')
    await page.getByText('已結案', { exact: true }).waitFor()
    await page.getByText(note, { exact: true }).waitFor()
    assert.equal(await page.getByLabel('處理結果', { exact: true }).count(), 0)
    assert.equal(await page.getByText('已成功結案。', { exact: true }).count(), 0)

    await devices.click()
    await northDevice.getByText('在線 · 23°C', { exact: true }).waitFor()
    await live.waitFor()
    const beforeRestart = version
    const allowReconnect = Promise.withResolvers()
    const releaseProbe = Promise.withResolvers()
    // 斷線探查先回 503，其餘探查若出現就暫留，驗證真實串流自行取得快照。
    await page.route(stack.origin + sessionPath, async route => {
      if (failedProbe) {
        await releaseProbe.promise
        await route.continue()
        return
      }
      failedProbe = true
      await route.fulfill({ status: 503, json: { error: 'service_unavailable' } })
    })
    // 真停機切斷既有串流，暫留下一次訂閱以便在瀏覽器未同步時提交變動。
    await page.route(stack.origin + eventPath, async route => {
      await allowReconnect.promise
      await route.continue()
    }, { times: 1 })
    try {
      try {
        restarting = true
        const probeResponse = page.waitForResponse(response =>
          response.url() === stack.origin + sessionPath && response.status() === 503)
        stack.stop()
        await Promise.all([disconnected.waitFor(), probeResponse])
        stack.start()
        await control('north-01', 'abnormal')
        assert.equal(await northDevice.getByText('在線 · 23°C', { exact: true }).count(), 1)
        assert.equal(await retry.isVisible(), true)
      } finally {
        allowReconnect.resolve()
      }
      await northDevice.getByText('在線 · 30°C', { exact: true }).waitFor()
      await live.waitFor()
    } finally {
      releaseProbe.resolve()
      await page.unroute(stack.origin + sessionPath)
    }
    assert.equal(failedProbe, true, '重連前的身分探查須實際收到 503')
    restarting = false
    assert.ok(version < beforeRestart, '重啟後較小的程序版本仍須同步')
    assert.equal(await southDevice.count(), 0)

    const oldRead = new EventEmitter()
    const releaseLateSnapshot = Promise.withResolvers()
    const lateSnapshotSettled = Promise.withResolvers()
    // 延遲已讀到的真實快照，讓瀏覽器在 SSE 仍連線時自己取消逾時請求。
    await page.route(stack.origin + listPath, async route => {
      const snapshot = await json(await send(listPath, northCookie), 200, '逾時前的真實快照')
      oldRead.emit('ready', snapshot)
      await releaseLateSnapshot.promise
      await route.fulfill({ json: snapshot })
      lateSnapshotSettled.resolve()
    }, { times: 1 })
    try {
      const [[older]] = await Promise.all([
        once(oldRead, 'ready', { signal: AbortSignal.timeout(8_000) }),
        control('north-01', 'stop'),
      ])
      assert.equal(older.items[0].latestMeasurement.temperature, 30)
      const [aborted] = await Promise.all([
        page.waitForEvent('requestfailed', { predicate: request => request.url() === stack.origin + listPath }),
        stale.waitFor(),
      ])
      assert.equal(aborted.failure()?.errorText, 'net::ERR_ABORTED')
      assert.equal(await northDevice.getByText('在線 · 30°C', { exact: true }).count(), 1)
      await page.setViewportSize({ width: 390, height: 844 })
      assert.deepEqual(await page.evaluate(() => [innerWidth, innerHeight, document.documentElement.scrollWidth]),
        [390, 844, 390])
      await control('north-01', 'normal')
      await control('north-01', 'stop')
      await retry.press('Enter')
      await northDevice.getByText('在線 · 23°C', { exact: true }).waitFor()
      await live.waitFor()
    } finally {
      releaseLateSnapshot.resolve()
    }
    await lateSnapshotSettled.promise
    // 舊快照在新快照完成後才放行，畫面不得退回舊溫度。
    await northDevice.getByText('在線 · 23°C', { exact: true }).waitFor()
    await live.waitFor()
    assert.equal(await retry.count(), 0)

    const allowHeartbeatReconnect = Promise.withResolvers()
    // 保留下一次連線，讓快轉的瀏覽器時鐘先呈現無心跳時的斷線提示。
    await page.route(stack.origin + eventPath, async route => {
      await allowHeartbeatReconnect.promise
      await route.continue()
    }, { times: 1 })
    try {
      const receivedFrames = frames
      await page.clock.fastForward(26_000)
      await disconnected.waitFor()
      assert.equal(frames, receivedFrames, '快轉期間沒有收到任何 SSE 訊框')
    } finally {
      await page.clock.setSystemTime(Date.now())
      allowHeartbeatReconnect.resolve()
    }
    await live.waitFor()

    // 撤銷瀏覽器正在使用的 Cookie，驗證原串流終止並清除私有設備。
    revoked = true
    const logout = await send('/api/logout', northCookie, undefined, 'POST')
    assert.equal(logout.status, 204)
    await expiredMessage.waitFor()
    assert.equal(await northDevice.count(), 0)
    assert.equal(await history.count(), 0)
    assert.equal(await liveStatus.count(), 0)
    revoked = false

    await signIn('north')
    await northDevice.waitFor()
    await live.waitFor()
    // 同一 Cookie jar 真實換發時，記錄舊頁是否曾短暫顯示新身分的設備。
    const scopeObserver = await page.evaluateHandle(() => {
      const root = document.documentElement
      root.dataset.scopeLeak = 'false'
      const observer = new MutationObserver(records => {
        for (const record of records) {
          const changed = record.type === 'characterData' ? [record.target] : record.addedNodes
          if (Array.from(changed).some(node => node.textContent?.includes('南區設備'))) {
            root.dataset.scopeLeak = 'true'
          }
        }
      })
      observer.observe(root, { childList: true, characterData: true, subtree: true })
      return observer
    })
    rotating = true
    let replacement
    try {
      replacement = await context.request.post(stack.origin + '/api/login', {
        headers: { Origin: stack.origin, 'X-CSRF-Protection': '1' },
        data: { username: 'south', password: passwords.south },
      })
    } catch {
      throw new Error('共用 Cookie 的登入換發請求失敗')
    }
    assert.equal(replacement.status(), 200)
    assert.equal((await replacement.json()).username, 'south')
    const replacementCookie = (await context.cookies(stack.origin)).find(item => item.name === 'iot_session')
    assert.ok(replacementCookie?.value, '換發須更新共用 Cookie')
    secrets.push(replacementCookie.value)
    const current = await context.request.get(stack.origin + sessionPath)
    assert.equal(current.status(), 200)
    assert.equal((await current.json()).username, 'south')
    await expiredMessage.waitFor()
    assert.equal(await northDevice.count(), 0)
    assert.equal(await southDevice.count(), 0)
    assert.equal(await liveStatus.count(), 0)
    assert.equal(await page.locator('html').getAttribute('data-scope-leak'), 'false')
    await scopeObserver.evaluate(observer => observer.disconnect())
    await scopeObserver.dispose()
    rotating = false

    // 重新登入後，須從新身分建立串流和範圍，不能沿用北區快照。
    await control('south-01', 'normal')
    let southCookie
    // 在首次訂閱前，只縮短這次南區登入的絕對期限，讓兩端使用同一個真實期限。
    await page.route(stack.origin + eventPath, async route => {
      const headers = await route.request().allHeaders()
      const token = /(?:^|;\s*)iot_session=([A-Za-z0-9_-]+)(?:;|$)/.exec(headers.cookie ?? '')?.[1]
      assert.ok(token, '南區訂閱須使用新簽發的登入')
      secrets.push(token)
      southCookie = `iot_session=${token}`
      southExpiresAt = shortenExpiry(southCookie, 8)
      await route.continue()
    }, { times: 1 })
    await signIn('south')
    await southDevice.getByText('在線 · 23°C', { exact: true }).waitFor()
    await live.waitFor()
    assert.equal(await northDevice.count(), 0)
    assert.equal(await page.getByRole('listitem').count(), 3)
    await history.press('Enter')
    await alertRow(southAlert).waitFor()
    await live.waitFor()
    assert.equal(await alertRow(northAlert).count(), 0)
    assert.equal(streamExpiresAt, southExpiresAt, '真實串流須宣告縮短後的絕對期限')
    assert.ok(Date.now() < southExpiresAt, '南區設備與告警須在到期前驗證')
    assert.equal(await expiredMessage.count(), 0)

    // 不送任何畫面操作，等真正到期後核對授權回應與私有 DOM 都已清除。
    await expiredMessage.waitFor({ timeout: 12_000 })
    assert.equal((await send(sessionPath, southCookie)).status, 401)
    assert.equal(await alertRow(southAlert).count(), 0)
    assert.equal(await page.getByRole('listitem').count(), 0)
    assert.equal(await history.count(), 0)
    assert.equal(await liveStatus.count(), 0)

    // 佔滿隔離服務的訂閱槽，從真實 HTTP 503 驗證從未收到首訊框時的到期。
    const restoredCookie = await login('south')
    const slots = await Promise.allSettled(Array.from({ length: 64 }, () => send(eventPath, hqCookie)))
    const streams = slots
      .filter(slot => slot.status === 'fulfilled')
      .map(slot => slot.value)
    try {
      assert.equal(streams.length, 64, '須成功開啟隔離的訂閱槽')
      assert.ok(streams.every(stream => stream.status === 200), '每個訂閱槽均須由真實串流佔用')
      // 隔離 HTTP 請求最長六秒，讓此登入先到期以維持 503 邊界。
      southExpiresAt = shortenExpiry(restoredCookie, 4)
      expiry401 = false
      noStream = true
      await context.addCookies([{
        name: 'iot_session', value: restoredCookie.slice('iot_session='.length), url: stack.origin,
        httpOnly: true, sameSite: 'Strict',
      }])
      const framesBeforeRestore = frames
      const [unavailableStream] = await Promise.all([
        page.waitForResponse(response => response.url() === stack.origin + eventPath),
        page.reload(),
      ])
      assert.equal(unavailableStream.status(), 503)
      assert.deepEqual(await unavailableStream.json(), { error: 'service_unavailable' })
      await southDevice.waitFor()
      await disconnected.waitFor()
      assert.equal(await northDevice.count(), 0)
      assert.equal(await page.getByRole('listitem').count(), 3)
      assert.ok(Date.now() < southExpiresAt, '授權清單須在到期前由 HTTP 身分恢復')
      await expiredMessage.waitFor({ timeout: 8_000 })
      assert.equal(frames, framesBeforeRestore, '整次登入不得收到任何串流訊框')
      assert.equal((await send(sessionPath, restoredCookie)).status, 401)
      assert.equal(await page.getByRole('listitem').count(), 0)
      assert.equal(await history.count(), 0)
      assert.equal(await liveStatus.count(), 0)
    } finally {
      await Promise.allSettled(streams.map(stream => stream.body?.cancel()))
    }
    assert.deepEqual(errors, [], '不得有非預期的瀏覽器錯誤')
  } finally {
    await browser.close()
  }
})
