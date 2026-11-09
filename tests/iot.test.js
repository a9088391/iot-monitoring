import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { once } from 'node:events'
import { existsSync, lstatSync, readFileSync } from 'node:fs'
import { createServer } from 'node:http'
import { join } from 'node:path'
import { test } from 'node:test'
import { isDeepStrictEqual } from 'node:util'
import { createStack } from './stack.js'

const north = ['north-01', 'north-02', 'north-03']
const south = ['south-01', 'south-02', 'south-03']
const inventory = [...north, ...south]
const users = {
  hq: { username: 'hq', role: 'headquarters', siteId: null },
  north: { username: 'north', role: 'site', siteId: 'north' },
  south: { username: 'south', role: 'site', siteId: 'south' },
}
const credentialPattern = /^[A-Za-z0-9_-]{43}$/
const wrongPassword = 'invalid-password-probe'

function check(actual, expected, message) {
  // 禁止測試失敗時印出來自 HTTP、資料庫或瀏覽器的實際值。
  assert.ok(isDeepStrictEqual(actual, expected), message)
}

function parseJson(raw) {
  try {
    return JSON.parse(raw)
  } catch {
    throw new Error('預期有效 JSON')
  }
}

async function json(response) {
  return parseJson(await response.text())
}

function digest(cookie) {
  return createHash('sha256').update(cookie.slice('iot_session='.length)).digest('hex')
}

function cookieFrom(response) {
  const cookies = response.headers.getSetCookie()
  assert.equal(cookies.length, 1, '登入須設定一個 Cookie')
  const [cookie, ...attributes] = cookies[0].split('; ')
  assert.ok(/^iot_session=[A-Za-z0-9_-]{43}$/.test(cookie), '須使用強隨機 session 識別格式')
  // 完整比對屬性，確認沒有 Domain 可擴大 Cookie 範圍。
  check(attributes.sort(), ['HttpOnly', 'Max-Age=28800', 'Path=/', 'SameSite=Strict'].sort(), '本機 HTTP Cookie 屬性不符')
  return cookie
}

function privateFile(path, directory = false) {
  const stat = lstatSync(path)
  assert.ok(directory ? stat.isDirectory() : stat.isFile(), '私密檔案或目錄類型不符')
  assert.equal(stat.mode & 0o7777, directory ? 0o700 : 0o600, '私密檔案或目錄權限不符')
}

async function browserFlow(origin, passwords, tokens) {
  const executablePath = process.env.CHROME_EXECUTABLE ?? [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/google-chrome', '/usr/bin/chromium',
  ].find(path => existsSync(path))
  assert.ok(executablePath && existsSync(executablePath), '請安裝 Chrome／Chromium 或設定 CHROME_EXECUTABLE')
  // 避免 Playwright 的診斷記錄輸入欄位或瀏覽器通訊內容。
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  const { chromium } = await import('playwright-core')
  const browser = await chromium.launch({ executablePath, timeout: 15_000 })
  try {
    const context = await browser.newContext()
    const page = await context.newPage()
    page.setDefaultTimeout(8_000)
    await page.goto(origin, { timeout: 10_000 })
    const account = page.getByLabel('帳號', { exact: true })
    const password = page.getByLabel('密碼', { exact: true })
    const logout = page.getByRole('button', { name: '登出', exact: true })
    const listItems = page.getByRole('listitem')
    const deviceName = (id) => `${id.startsWith('north') ? '北區' : '南區'}設備 ${id.slice(-2)}`

    async function signIn(username, secret) {
      try {
        await account.fill(username)
        await password.fill(secret)
        await password.press('Enter')
      } catch {
        throw new Error(`瀏覽器登入表單操作失敗（${username}）`)
      }
    }

    async function checkList(ids) {
      await page.getByRole('button', { name: new RegExp(deviceName(ids.at(-1))) }).waitFor()
      const actual = (await listItems.allInnerTexts()).map(text => text.replace(/\s+/g, ' ').trim())
      const expected = ids.map(id => `${deviceName(id)} ${id.startsWith('north') ? '北區場域' : '南區場域'}`)
      check(actual, expected, '瀏覽器設備清單與登入範圍不符')
    }

    async function signOut(interrupted = false) {
      const active = (await context.cookies()).find(cookie => cookie.name === 'iot_session')
      assert.ok(active, '瀏覽器須持有有效登入 Cookie')
      tokens.push(`iot_session=${active.value}`)
      if (interrupted) {
        await page.route(origin + '/api/logout', route => route.abort(), { times: 1 })
        await logout.click()
        await page.getByRole('alert').filter({ hasText: '登出結果未確認' }).waitFor()
        check(await listItems.count(), 0, '登出結果未確認時須隱藏設備資料')
        assert.ok(await account.isDisabled(), '登出結果未確認時須停用登入')
      }
      const action = interrupted ? page.getByRole('button', { name: '確認並重試登出' }) : logout
      const [response] = await Promise.all([
        page.waitForResponse(item => item.url() === origin + '/api/logout' && item.request().method() === 'POST'),
        action.click(),
      ])
      check(response.status(), 204, '瀏覽器登出請求應成功')
      await page.getByRole('alert').filter({ hasText: '已登出。' }).waitFor()
      check(await listItems.count(), 0, '登出後不得保留設備資料')
      const cookies = await context.cookies()
      assert.ok(!cookies.some(cookie => cookie.name === 'iot_session'), '登出後瀏覽器須移除 session Cookie')
    }

    await signIn('hq', wrongPassword)
    await page.getByRole('alert').filter({ hasText: '帳號或密碼錯誤。' }).waitFor()
    await signIn('hq', passwords.hq)
    await checkList(inventory)
    const hasAppSession = (await context.cookies(origin)).some(cookie =>
      cookie.name === 'iot_session' && cookie.domain === new URL(origin).hostname)
    assert.ok(hasAppSession, '瀏覽器須持有正式主機的登入 Cookie')

    // 以同一個已登入的瀏覽器造訪另一個本機服務，接收端只保留 Cookie 是否存在。
    let receivedSession
    const otherService = createServer((req, res) => {
      if (req.url === '/cookie-probe') {
        receivedSession = /(?:^|;\s*)iot_session=/.test(req.headers.cookie ?? '')
      }
      res.end()
    })
    otherService.listen(0, '127.0.0.1')
    await once(otherService, 'listening')
    try {
      const response = await page.goto(`http://127.0.0.1:${otherService.address().port}/cookie-probe`)
      assert.equal(response?.status(), 200, '瀏覽器須抵達另一個本機服務')
    } finally {
      await otherService[Symbol.asyncDispose]()
    }
    await page.goto(origin)
    await checkList(inventory)
    assert.notEqual(receivedSession, undefined, '另一個本機服務須收到探測請求')
    assert.equal(receivedSession, false, '另一個本機服務不得收到 iot_session')

    await page.getByRole('button', { name: /北區設備 01/ }).click()
    await page.getByRole('heading', { name: '北區設備 01', exact: true }).waitFor()
    await page.getByText('north-01', { exact: true }).waitFor()
    await page.getByRole('button', { name: /返回設備清單/ }).click()
    await checkList(inventory)
    await page.reload()
    await checkList(inventory)
    await signOut()
    await page.reload()
    await account.waitFor()
    for (const [username, ids] of [['north', north], ['south', south]]) {
      await signIn(username, passwords[username])
      await checkList(ids)
      await signOut(username === 'south')
    }
  } finally {
    await browser.close()
  }
}

test('IoT 票 01 的本機端到端驗收', { timeout: 600_000 }, async (t) => {
  const stack = await createStack()
  t.after(() => stack.cleanup())
  stack.start()
  const { origin, loopbackOrigin, sql } = stack
  const passwordPath = join(stack.state, 'demo-passwords.json')
  const dbSecretPath = join(stack.state, 'db-password')
  const originalPasswords = readFileSync(passwordPath, 'utf8')
  const passwords = parseJson(originalPasswords)
  const dbSecret = readFileSync(dbSecretPath, 'utf8')
  const tokens = []
  const csrf = { Origin: origin, 'X-CSRF-Protection': '1' }

  async function send(path, status, { body, chunks, cookie, headers = {}, post = false } = {}) {
    const response = await stack.send(path, {
      method: post ? 'POST' : 'GET', body, chunks,
      headers: { ...(cookie ? { Cookie: cookie } : {}), ...headers },
    })
    assert.equal(response.status, status, `${post ? 'POST' : 'GET'} ${path}`)
    return response
  }

  const login = (username, password, status, cookie) =>
    send('/api/login', status, {
      post: true, body: JSON.stringify({ username, password }), cookie,
      headers: { ...csrf, 'Content-Type': 'application/json' },
    })

  async function sessionCookie(response, username) {
    check(await json(response), users[username], '登入身分不符')
    const cookie = cookieFrom(response)
    tokens.push(cookie)
    return cookie
  }

  async function errorBody(response, error) {
    check(await json(response), { error }, 'HTTP 錯誤內容不符')
  }

  const businessState = `
    SELECT jsonb_build_array(
      (SELECT jsonb_agg(to_jsonb(sites) ORDER BY id) FROM sites),
      (SELECT jsonb_agg(to_jsonb(devices) ORDER BY id) FROM devices),
      (SELECT jsonb_agg(to_jsonb(accounts) ORDER BY username) FROM accounts));
  `
  const initialData = sql(businessState)

  await t.test('初始化資料、受保護的獨立密碼與 Argon2id', () => {
    check(sql("SELECT (SELECT count(*) FROM sites) || ',' || (SELECT count(*) FROM devices) || ',' || (SELECT count(*) FROM accounts);"),
      '2,6,3', '預載資料筆數不符')
    privateFile(stack.state, true)
    privateFile(passwordPath)
    privateFile(dbSecretPath)
    assert.ok(/^[a-f0-9]{64}\n$/.test(dbSecret), '資料庫須使用獨立產生的憑證')
    check(Object.keys(passwords).sort(), ['hq', 'north', 'south'], '示範帳號清單不符')
    const secrets = Object.values(passwords)
    assert.ok(secrets.every(secret => credentialPattern.test(secret)) && new Set(secrets).size === 3,
      '示範密碼須為各自生成的強隨機值')
    const hashes = sql('SELECT password_hash FROM accounts ORDER BY username;').split('\n')
    const pattern = /^\$argon2id\$v=19\$m=19456,t=2,p=1\$([A-Za-z0-9+/]{22})\$[A-Za-z0-9+/]{43}$/
    const salts = hashes.map(hash => pattern.exec(hash)?.[1])
    assert.ok(salts.every(Boolean) && new Set(salts).size === 3, 'Argon2id 參數或獨立 salt 不符')
    assert.ok(secrets.every(secret => !initialData.includes(secret)), '資料庫不得保存示範明文密碼')
  })

  let hqCookie
  await t.test('三種身分、穩定分頁、偽造參數與越權隱藏', async () => {
    for (const path of ['/api/session', '/api/devices', '/api/devices/north-01']) {
      await errorBody(await send(path, 401), 'unauthenticated')
    }
    for (const [username, ids] of [['hq', inventory], ['north', north], ['south', south]]) {
      const cookie = await sessionCookie(await login(username, passwords[username], 200), username)
      if (username === 'hq') hqCookie = cookie
      const before = sql('SELECT * FROM sessions ORDER BY token_hash;')
      check(await json(await send('/api/session', 200, { cookie })), users[username], '目前登入身分不符')
      for (const [query, expected, limit, offset] of [
        ['', ids, 50, 0], ['?limit=100', ids, 100, 0], ['?limit=2', ids.slice(0, 2), 2, 0],
        ['?limit=2&offset=2', ids.slice(2, 4), 2, 2], ['?offset=99', [], 50, 99], ['?limit=2', ids.slice(0, 2), 2, 0],
      ]) {
        const page = await json(await send('/api/devices' + query, 200, { cookie }))
        check([page.items.map(item => item.id), page.total, page.limit, page.offset],
          [expected, ids.length, limit, offset], '設備分頁結果或排序不符')
      }
      for (const id of ids) {
        const detail = await json(await send('/api/devices/' + id, 200, { cookie }))
        check([detail.id, detail.siteId], [id, id.split('-')[0]], '設備單筆內容不符')
      }
      for (const query of ['siteId=south', 'role=headquarters', 'limit=101', 'limit=0', 'limit=2.5', 'limit=1&limit=2', 'offset=-1']) {
        await errorBody(await send('/api/devices?' + query, 400, { cookie }), 'invalid_input')
      }
      await errorBody(await send('/api/devices/' + ids[0] + '?siteId=north&role=headquarters', 400, { cookie }), 'invalid_input')
      if (username !== 'hq') {
        const blocked = await send('/api/devices/' + (username === 'north' ? south[0] : north[0]), 404, { cookie })
        const missing = await send('/api/devices/missing-01', 404, { cookie })
        check(await json(blocked), { error: 'not_found' }, '跨場域單筆須隱藏存在性')
        await errorBody(missing, 'not_found')
      }
      check(sql('SELECT * FROM sessions ORDER BY token_hash;'), before, '讀取不得延長或更改 session')
      check(sql(businessState), initialData, '讀取不得改變業務資料')
    }
  })

  await t.test('Host、CSRF、16 KiB 邊界、輸入驗證、session 換發／到期／撤銷', async () => {
    // 分開驗證舊網址的 Host 與 Origin，讓兩個拒絕條件各自被觸發。
    await errorBody(await send('/', 403, { headers: { Host: new URL(loopbackOrigin).host } }), 'forbidden')
    const jsonHeaders = { ...csrf, 'Content-Type': 'application/json' }
    const valid = JSON.stringify({ username: 'south', password: passwords.south })
    for (const headers of [
      { ...jsonHeaders, Origin: loopbackOrigin },
      { ...jsonHeaders, Origin: 'http://localhost:80' },
      { 'X-CSRF-Protection': '1', 'Content-Type': 'application/json' },
      { Origin: origin, 'Content-Type': 'application/json' },
      { ...jsonHeaders, 'X-CSRF-Protection': '0' },
    ]) {
      await errorBody(await send('/api/login', 403, { post: true, body: valid, headers }), 'forbidden')
    }
    for (const body of [
      '{"username":"south","password":123}', '{"username":null,"password":"x"}',
      '{"username":"south","password":"x","role":"headquarters"}', '{"username":"south","username":"hq","password":"x"}',
      '{"username":"south","password":"x","password":"y"}', '{"username":"south"}', '[]', valid + '{}',
    ]) {
      await errorBody(await send('/api/login', 400, { post: true, body, headers: jsonHeaders }), 'invalid_input')
    }
    await errorBody(await send('/api/login', 415, { post: true, body: valid, headers: csrf }), 'unsupported_media_type')
    const boundary = valid.padEnd(16384, ' ')
    check(Buffer.byteLength(boundary), 16384, '測試本文須恰好 16 KiB')
    const oversized = boundary + ' '
    await errorBody(await send('/api/login', 413, { post: true, body: oversized, headers: jsonHeaders }), 'body_too_large')
    await errorBody(await send('/api/login', 413, {
      post: true, chunks: [oversized.slice(0, 8192), oversized.slice(8192)],
      headers: { ...jsonHeaders, 'Transfer-Encoding': 'chunked' },
    }), 'body_too_large')
    for (const username of ['hq', 'unknown-user']) {
      const response = await login(username, wrongPassword, 401)
      await errorBody(response, 'invalid_credentials')
      check(response.headers.getSetCookie().length, 0, '失敗登入不得設定 Cookie')
    }
    const first = await sessionCookie(await send('/api/login', 200, { post: true, body: boundary, headers: jsonHeaders }), 'south')
    const second = await sessionCookie(await login('south', passwords.south, 200, first), 'south')
    assert.ok(first !== second, '登入須換發 session 識別')
    await errorBody(await send('/api/session', 401, { cookie: first }), 'unauthenticated')
    const tokenHash = digest(second)
    const lifetime = Number(sql(`
      SELECT EXTRACT(EPOCH FROM expires_at - statement_timestamp())
      FROM sessions WHERE token_hash = decode('${tokenHash}', 'hex') AND username = 'south';
    `))
    assert.ok(lifetime > 28795 && lifetime <= 28800, '新 session 的絕對期限須為 8 小時')
    check(sql('SELECT bool_and(octet_length(token_hash) = 32) FROM sessions;'), 't', '資料庫只保存 session 摘要')
    check(sql(`
      UPDATE sessions SET expires_at = statement_timestamp() - interval '1 second'
      WHERE token_hash = decode('${tokenHash}', 'hex') RETURNING 1;
    `), '1', '只讓測試用 session 到期')
    await errorBody(await send('/api/session', 401, { cookie: second }), 'unauthenticated')

    const live = await sessionCookie(await login('south', passwords.south, 200), 'south')
    for (const headers of [{ Origin: origin }, { 'X-CSRF-Protection': '1' }, { ...csrf, Origin: 'https://example.invalid' }]) {
      await errorBody(await send('/api/logout', 403, { post: true, cookie: live, headers }), 'forbidden')
      await send('/api/session', 200, { cookie: live })
    }
    const logout = await send('/api/logout', 204, { post: true, cookie: live, headers: csrf })
    assert.ok(logout.headers.getSetCookie().some(cookie => cookie.startsWith('iot_session=;') && cookie.includes('Max-Age=0')),
      '登出須清除 Cookie')
    await errorBody(await send('/api/session', 401, { cookie: live }), 'unauthenticated')
    check(sql(`SELECT count(*) FROM sessions WHERE token_hash = decode('${digest(live)}', 'hex');`), '0', '登出須立即刪除 session')
  })

  await t.test('真實 Chrome 鍵盤登入、Cookie 隔離、清單／詳細資料、重載與登出', async () => {
    await browserFlow(origin, passwords, tokens)
  })

  await t.test('第二實例遭啟動鎖拒絕，停止再啟動保留資料、憑證與既有登入', async () => {
    assert.ok(hqCookie, '重啟前須有總部 session')
    stack.secondInstance()
    await send('/healthz', 204)
    await send('/api/session', 200, { cookie: hqCookie })
    stack.stop()
    stack.start()
    check(readFileSync(passwordPath, 'utf8'), originalPasswords, '重啟須重用示範憑證')
    check(readFileSync(dbSecretPath, 'utf8'), dbSecret, '重啟須重用資料庫憑證')
    check(sql(businessState), initialData, '重啟須保留業務資料及原本的密碼雜湊')
    check(await json(await send('/api/session', 200, { cookie: hqCookie })), users.hq, '重啟須保留有效 session')
  })

  await t.test('日誌不含憑證、session、雜湊或敏感登入本文', () => {
    stack.collectLogs()
    const hashes = sql('SELECT password_hash FROM accounts;').split('\n')
    const liveTokens = tokens.map(cookie => cookie.slice('iot_session='.length))
    const allLogs = stack.logs.join('\n')
    for (const secret of [...Object.values(passwords), dbSecret.trim(), ...hashes, ...liveTokens, wrongPassword]) {
      assert.ok(secret && !allLogs.includes(secret), '日誌含敏感資料')
    }
  })
})
