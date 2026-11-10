import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { EventEmitter, once } from 'node:events'
import { existsSync, lstatSync, readFileSync, writeFileSync } from 'node:fs'
import { basename, dirname, join } from 'node:path'
import { test } from 'node:test'
import { createStack } from './stack.js'

const alertsPath = '/api/alerts'
const devicePath = '/api/devices/north-01'
const sessionPath = '/api/session'
const simulationPath = devicePath + '/simulation'
const eventPath = '/api/events'
const devices = ['north-01', 'north-02', 'north-03', 'south-01', 'south-02', 'south-03']
const resetTables = [
  'sites', 'devices', 'accounts', 'latest_measurements', 'measurement_runs', 'alerts', 'alert_closures',
  'device_simulations', 'simulation_history', 'sessions', 'login_attempts',
]

// 只比對 HTTP 狀態與預期的業務資料，不輸出憑證或請求本文。
async function json(response, status, phase) {
  assert.equal(response.status, status, phase)
  return response.json()
}

// 透過正式指令操作同一個已核對的 Compose 堆疊，驗證拒絕、回滾及重設。
test('IoT 票 07：本機重設守門、交易回滾與瀏覽器舊登入失效', { timeout: 600_000 }, async t => {
  const stack = await createStack()
  t.after(() => stack.cleanup())
  stack.start()
  const root = dirname(stack.state)
  const project = basename(root)
  const confirm = `RESET ${project}\n`
  const dbPasswordPath = join(stack.state, 'db-password')
  const demoPasswordPath = join(stack.state, 'demo-passwords.json')
  const originalDbPassword = readFileSync(dbPasswordPath)
  const originalDemoPasswords = readFileSync(demoPasswordPath)
  const passwords = JSON.parse(originalDemoPasswords)
  const secrets = [...Object.values(passwords), originalDbPassword.toString().trim()]

  // 沿用正式 Origin、CSRF 與登入，從真實服務建立重設前的業務資料。
  function send(path, cookie, body) {
    const headers = cookie ? { Cookie: cookie } : {}
    if (body === undefined) return stack.send(path, { headers })
    return stack.send(path, {
      method: 'POST',
      headers: { ...headers, Origin: stack.origin, 'X-CSRF-Protection': '1', 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
  }

  // 取得這次由服務簽發的 Cookie，保留它以核對重設是否確實撤銷。
  async function login(username) {
    const response = await send('/api/login', '', { username, password: passwords[username] })
    const actor = await json(response, 200, '使用既有示範憑證登入')
    const cookie = response.headers.getSetCookie()[0]?.split(';')[0]
    assert.ok(cookie?.startsWith('iot_session='), '服務須簽發正式 Cookie')
    secrets.push(cookie.slice('iot_session='.length))
    return { actor, cookie }
  }

  // 透過正式控制入口讀取版本再寫入，避免測試自行建立模擬執行。
  async function control(cookie, action) {
    const current = await json(await send(simulationPath, cookie), 200, '讀取模擬狀態')
    const updated = await json(await send(simulationPath, cookie, { action, version: current.version }), 200, '提交模擬控制')
    assert.equal(updated.mode, action === 'stop' ? 'stopped' : action)
  }

  // 用相同的資料庫指紋跨越取消和回滾；只排除按真實時間新增的離線告警。
  function preservedData() {
    return stack.sql(`
      SELECT md5(concat(
        (SELECT jsonb_agg(d ORDER BY id) FROM devices d),
        (SELECT jsonb_agg(a ORDER BY username) FROM accounts a),
        (SELECT jsonb_agg(m ORDER BY device_id) FROM latest_measurements m),
        (SELECT jsonb_agg(r ORDER BY device_id) FROM measurement_runs r),
        (SELECT jsonb_agg(a ORDER BY id) FROM alerts a WHERE type = 'temperature'),
        (SELECT jsonb_agg(c ORDER BY alert_id) FROM alert_closures c),
        (SELECT jsonb_agg(s ORDER BY device_id) FROM device_simulations s),
        (SELECT jsonb_agg(h ORDER BY device_id, version) FROM simulation_history h),
        (SELECT jsonb_agg(s ORDER BY token_hash) FROM sessions s),
        (SELECT jsonb_agg(a ORDER BY scope, key_hash, expires_at) FROM login_attempts a),
        (SELECT generation FROM iot_data_generation WHERE singleton)
      ));
    `)
  }

  // 比對提示中的資料庫身分與實際連線，避免只用畫面字串證明目標。
  function targetToken() {
    return stack.sql(`
      SELECT system_identifier::text || ':' || generation::text
      FROM pg_control_system(), iot_data_generation WHERE singleton;
    `)
  }

  const { cookie: oldCookie } = await login('hq')
  await control(oldCookie, 'abnormal')
  await control(oldCookie, 'normal')
  await control(oldCookie, 'stop')
  const { items } = await json(await send(alertsPath, oldCookie), 200, '讀取重設前告警')
  const oldAlert = items.find(item => item.deviceId === 'north-01' && item.type === 'temperature')
  assert.equal(oldAlert?.status, 'recovered')
  const note = '重設前的私有處理結果'
  const closed = await json(await send(`${alertsPath}/${oldAlert.id}/close`, oldCookie, { result: note }), 200, '建立已結案資料')
  assert.equal(closed.closure.result, note)

  const originalTarget = targetToken()
  const [cluster, generation] = originalTarget.split(':')
  const originalResources = stack.resetResources()
  const originalData = preservedData()

  // 取消、拒絕及預檢失敗均不能停止容器或撤銷原本可用的登入。
  async function expectUnchanged() {
    assert.deepEqual(stack.resetResources(), originalResources)
    assert.equal(preservedData(), originalData)
    assert.equal((await send(sessionPath, oldCookie)).status, 200)
  }

  // 原樣核對提示中的操作目標、清除範圍與資料復原限制。
  function expectPrompt(output) {
    for (const text of [
      `本機重設目標：${project}（${root}）`,
      `PostgreSQL：iot，資料卷：${originalResources.volume.name}，叢集與資料世代：${originalTarget}`,
      '設備回報、告警、結案、模擬與歷程、登入與限流資料',
      '沒有備份時無法復原',
      `輸入 RESET ${project} 確認：`,
    ]) {
      assert.ok(output.includes(text), '確認前須顯示已核對的目標與完整清除範圍')
    }
  }

  for (const input of ['RESET wrong-target\n', '']) {
    const output = stack.reset(input, 1)
    expectPrompt(output)
    assert.ok(output.includes('未確認，已取消'))
    await expectUnchanged()
  }

  const wrongTarget = stack.reset(confirm, 1, { remoteTarget: true })
  assert.ok(wrongTarget.includes('Docker 必須使用本機 Unix socket'))
  assert.ok(!wrongTarget.includes('本機重設目標'))
  await expectUnchanged()

  // 使用格式與權限均有效、但和資料庫不同的示範密碼，走真實唯讀預檢。
  const wrongPassword = randomBytes(32).toString('base64url')
  assert.ok(!Object.values(passwords).includes(wrongPassword))
  secrets.push(wrongPassword)
  try {
    writeFileSync(demoPasswordPath, JSON.stringify({ ...passwords, hq: wrongPassword }))
    assert.equal(lstatSync(demoPasswordPath).mode & 0o777, 0o600)
    const invalidCredentials = stack.reset(confirm, 1)
    assert.ok(invalidCredentials.includes('資料庫或既有示範憑證核對失敗'))
    assert.ok(!invalidCredentials.includes('本機重設目標'))
    await expectUnchanged()
  } finally {
    writeFileSync(demoPasswordPath, originalDemoPasswords)
  }

  // 舊設備不驗證此約束，新插入必定失敗，讓真正清除後的重建觸發回滾。
  stack.sql("ALTER TABLE devices ADD CONSTRAINT test_reset_seed_failure CHECK (id <> 'north-01') NOT VALID;")
  try {
    const failed = stack.reset(confirm, 1)
    expectPrompt(failed)
    assert.ok(failed.includes('insert seed sites and devices'), '故障須發生在交易內重建設備時')
    assert.ok(failed.includes('應用保持停止'), '不可將失敗重設回報為已重啟')
    const stopped = stack.resetResources()
    assert.deepEqual(stopped, { ...originalResources, app: { ...originalResources.app, running: false } })
    assert.equal(preservedData(), originalData, '重建失敗須完整保留原業務資料、登入及世代')
  } finally {
    stack.sql('ALTER TABLE devices DROP CONSTRAINT test_reset_seed_failure;')
  }
  stack.start()
  const recovered = stack.resetResources()
  assert.equal(recovered.app.id, originalResources.app.id)
  assert.deepEqual(recovered.db, originalResources.db)
  assert.deepEqual(recovered.volume, originalResources.volume)
  assert.equal((await send(sessionPath, oldCookie)).status, 200)
  assert.equal(preservedData(), originalData)

  await control(oldCookie, 'abnormal')
  assert.equal(stack.sql("SELECT data_generation FROM measurement_runs WHERE device_id = 'north-01';"), generation)
  const executablePath = process.env.CHROME_EXECUTABLE ?? [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/google-chrome', '/usr/bin/chromium',
  ].find(path => existsSync(path))
  assert.ok(executablePath && existsSync(executablePath), '請安裝 Chrome／Chromium 或設定 CHROME_EXECUTABLE')
  // 禁用可能輸出憑證或瀏覽器通訊的除錯模式。
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  const { chromium } = await import('playwright-core')
  const browser = await chromium.launch({ executablePath, timeout: 15_000 })
  try {
    const context = await browser.newContext()
    await context.addCookies([{
      name: 'iot_session', value: oldCookie.slice('iot_session='.length), url: stack.origin,
      httpOnly: true, sameSite: 'Strict',
    }])
    const page = await context.newPage()
    page.setDefaultTimeout(8_000)
    const history = page.getByRole('button', { name: '告警歷程', exact: true })
    const privateNote = page.getByText(note, { exact: true })
    const liveStatus = page.locator('#live-status')
    const live = liveStatus.filter({ hasText: /^即時更新中$/ })
    const browserErrors = []
    let resetting = false
    // 只保留遮蔽過的短診斷，以辨識真正非預期的瀏覽器錯誤。
    function recordBrowserError(message) {
      const redacted = secrets.reduce((value, secret) => value.replaceAll(secret, '[redacted]'), message)
      browserErrors.push(redacted.slice(0, 300))
    }
    // 只略過重設期間原檢視與串流的網路斷線或撤銷登入回應。
    page.on('console', message => {
      if (message.type() !== 'error') return
      const { url } = message.location()
      const text = message.text()
      const resetEndpoint = [eventPath, sessionPath, `${alertsPath}/${oldAlert.id}`]
        .some(path => url === stack.origin + path)
      const transportError = /net::ERR_(CONNECTION_REFUSED|CONNECTION_RESET|EMPTY_RESPONSE)$/.test(text)
      if (url === stack.origin + '/favicon.ico' && text.includes('status of 404')) return
      if (
        resetting
        && resetEndpoint
        && (transportError || text.includes('status of 401'))
      ) return
      recordBrowserError(`${text} @ ${url}`)
    })
    page.on('pageerror', error => recordBrowserError(error.message))

    // 用 Chrome 協定確認畫面使用的同一條真實 SSE 已接到版本，並觀察該連線終止。
    const network = await context.newCDPSession(page)
    await network.send('Network.enable')
    const [[frame]] = await Promise.all([
      once(network, 'Network.eventSourceMessageReceived', { signal: AbortSignal.timeout(8_000) }),
      page.goto(stack.origin),
    ])
    assert.equal(frame.eventName, 'version')
    const stream = new EventEmitter()
    let streamClosed = false
    // HTTP 完成或網路中止均代表原本的 SSE 回應已經終止。
    function observeEnd(event) {
      if (event.requestId !== frame.requestId) return
      streamClosed = true
      stream.emit('ended')
    }
    network.on('Network.loadingFinished', observeEnd)
    network.on('Network.loadingFailed', observeEnd)
    await history.press('Enter')
    await page.getByRole('button', { name: new RegExp(`#${oldAlert.id} · 溫度異常 · north-01`) }).click()
    await privateNote.waitFor()
    await live.waitFor()
    assert.equal(streamClosed, false, '重設前應仍持有原先的 SSE 連線')

    resetting = true
    const [, resetOutput] = await Promise.all([
      once(stream, 'ended', { signal: AbortSignal.timeout(120_000) }),
      stack.resetWithStopBarrier(async () => {
        // 第一個已確認重設停在實際 stop 前，第二個看到相同舊世代並送出正確確認。
        const beforeStop = stack.resetResources()
        const competing = stack.reset(confirm, 1)
        expectPrompt(competing)
        assert.ok(competing.includes('無法取得本案維護鎖'), '競爭指令須由維護鎖拒絕')
        assert.deepEqual(stack.resetResources(), beforeStop, '被拒絕的指令不可停止原應用')
        assert.equal(targetToken(), originalTarget, '被拒絕的指令不可輪替資料世代')
        const preserved = await json(await send(`${alertsPath}/${oldAlert.id}`, oldCookie), 200, '競爭時原登入及告警')
        assert.deepEqual(preserved, closed)
        assert.equal(await privateNote.isVisible(), true)
        assert.equal(streamClosed, false)
      }),
    ])
    expectPrompt(resetOutput)
    assert.ok(resetOutput.includes('本機重設完成，請重新登入'))
    await page.getByText('登入狀態已失效，請重新登入。', { exact: true }).waitFor()
    assert.equal(await privateNote.count(), 0)
    assert.equal(await history.count(), 0)
    assert.equal(await liveStatus.count(), 0)
    resetting = false

    const resources = stack.resetResources()
    assert.equal(resources.app.id, originalResources.app.id)
    assert.equal(resources.app.running, true)
    assert.notEqual(resources.app.startedAt, recovered.app.startedAt)
    assert.equal(existsSync(join(stack.state, '.maintenance-lock')), false, '成功重設後須釋放維護鎖')
    assert.deepEqual(resources.db, originalResources.db)
    assert.deepEqual(resources.volume, originalResources.volume)
    assert.equal(targetToken(), `${cluster}:${BigInt(generation) + 1n}`)
    for (const [path, original] of [[demoPasswordPath, originalDemoPasswords], [dbPasswordPath, originalDbPassword]]) {
      const metadata = lstatSync(path)
      assert.ok(metadata.isFile() && (metadata.mode & 0o777) === 0o600, '憑證須維持權限 0600 的一般檔案')
      assert.ok(readFileSync(path).equals(original), '重設須沿用原憑證內容')
    }

    for (const path of [sessionPath, '/api/devices', alertsPath, eventPath]) {
      assert.equal((await send(path, oldCookie)).status, 401, '重設前 Cookie 不得讀取受保護內容')
    }
    const counts = JSON.parse(stack.sql(
      `SELECT json_build_array(${resetTables.map(table => `(SELECT count(*) FROM ${table})`).join(',')});`,
    ))
    assert.deepEqual(counts, [2, 6, 3, 0, 0, 0, 0, 0, 0, 0, 0], '重設後須只保留預載場域、設備與帳號')

    // 同一個瀏覽器用原密碼重新登入，確認私有歷程已換成重設後的空狀態。
    try {
      await page.getByLabel('帳號', { exact: true }).fill('hq')
      const password = page.getByLabel('密碼', { exact: true })
      await password.fill(passwords.hq)
      await password.press('Enter')
    } catch {
      throw new Error('重設後的瀏覽器登入操作失敗')
    }
    await page.getByRole('button', { name: /北區設備 01.*尚未回報/ }).waitFor()
    await live.waitFor()
    await history.press('Enter')
    await page.getByText('目前沒有可查看的告警。', { exact: true }).waitFor()
    const session = (await context.cookies(stack.origin)).find(cookie => cookie.name === 'iot_session')
    assert.ok(session?.value, '重新登入須取得新 Cookie')
    const currentCookie = `iot_session=${session.value}`
    assert.ok(currentCookie !== oldCookie, '重設後登入不可沿用舊 Cookie')
    secrets.push(session.value)

    for (const site of ['north', 'south']) {
      const { actor, cookie } = await login(site)
      assert.deepEqual(actor, { username: site, role: 'site', siteId: site })
      const { items: ownDevices } = await json(await send('/api/devices', cookie), 200, '驗證重建後場域範圍')
      assert.deepEqual(ownDevices.map(item => item.id), devices.filter(id => id.startsWith(site + '-')))
    }
    const inventory = await json(await send('/api/devices', currentCookie), 200, '重設後查閱全部設備')
    assert.deepEqual(inventory.items.map(item => item.id), devices)
    for (const item of inventory.items) {
      assert.equal(item.onlineStatus, 'unreported')
      assert.equal(item.latestMeasurement, undefined)
      const simulation = await json(await send(`/api/devices/${item.id}/simulation`, currentCookie), 200, '重設後模擬狀態')
      assert.deepEqual(simulation, { mode: 'stopped', version: '0', canReplay: false })
    }
    await control(currentCookie, 'normal')
    const fresh = await json(await send(devicePath, currentCookie), 200, '讀取重設後新測量')
    assert.equal(fresh.onlineStatus, 'online')
    assert.equal(fresh.latestMeasurement?.temperature, 23)
    assert.equal(stack.sql("SELECT data_generation FROM measurement_runs WHERE device_id = 'north-01';"),
      (BigInt(generation) + 1n).toString())
    assert.deepEqual(browserErrors, [], '瀏覽器不得有非預期錯誤')
  } finally {
    await browser.close()
  }

  stack.collectLogs()
  const logs = stack.logs.join('\n')
  for (const secret of secrets) {
    assert.ok(secret && !logs.includes(secret), '重設及服務日誌不可包含憑證或 Cookie')
  }
})
