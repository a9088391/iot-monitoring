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

// 只比對實際值與期望值是否一致，避免 HTTP、資料庫或瀏覽器憑證出現在斷言輸出。
function check(actual, expected, message) {
  assert.ok(isDeepStrictEqual(actual, expected), message)
}

// 解析失敗時只回報格式問題，避免原始本文出現在例外訊息。
function parseJson(raw) {
  try {
    return JSON.parse(raw)
  } catch {
    throw new Error('預期有效 JSON')
  }
}

// 統一讀取 HTTP 回應本文，讓解析失敗也沿用遮蔽診斷。
async function json(response) {
  return parseJson(await response.text())
}

// 重算資料庫使用的 token 摘要，讓查詢可定位測試 session 而無須送入明文。
function digest(cookie) {
  return createHash('sha256')
    .update(cookie.slice('iot_session='.length))
    .digest('hex')
}

// 驗證登入 Cookie 的識別格式及完整屬性，再交給需要維持登入的測試。
function cookieFrom(response) {
  const cookies = response.headers.getSetCookie()
  assert.equal(cookies.length, 1, '登入須設定一個 Cookie')
  const [cookie, ...attributes] = cookies[0].split('; ')
  assert.ok(/^iot_session=[A-Za-z0-9_-]{43}$/.test(cookie), '須使用強隨機 session 識別格式')
  // 完整比對屬性，確認沒有 Domain 可擴大 Cookie 範圍。
  check(attributes.sort(), ['HttpOnly', 'Max-Age=28800', 'Path=/', 'SameSite=Strict'].sort(), '本機 HTTP Cookie 屬性不符')
  return cookie
}

// 直接檢查路徑的類型和權限，避免以符號連結指向的檔案誤認為受保護的本機憑證。
function privateFile(path, directory = false) {
  const stat = lstatSync(path)
  assert.ok(directory ? stat.isDirectory() : stat.isFile(), '私密檔案或目錄類型不符')
  assert.equal(stat.mode & 0o7777, directory ? 0o700 : 0o600, '私密檔案或目錄權限不符')
}

// 使用真實瀏覽器驗證表單、Cookie 隔離與登入後的頁面生命週期。
async function browserFlow(origin, passwords, tokens) {
  // 優先採用指定的 Chrome，未指定時只從本機常見路徑選取已安裝版本。
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
    // 建立獨立瀏覽器環境，讓登入及主機隔離檢查共用同一份 Cookie 狀態。
    const context = await browser.newContext()
    const page = await context.newPage()
    page.setDefaultTimeout(8_000)
    await page.goto(origin, { timeout: 10_000 })
    const account = page.getByLabel('帳號', { exact: true })
    const password = page.getByLabel('密碼', { exact: true })
    const logout = page.getByRole('button', { name: '登出', exact: true })
    const listItems = page.getByRole('listitem')
    // 依展示設備識別組成可見名稱，以驗證畫面標籤而非內部資料。
    const deviceName = (id) => `${id.startsWith('north') ? '北區' : '南區'}設備 ${id.slice(-2)}`

    // 用 Enter 送出登入表單，操作失敗只回報帳號以免 Playwright 例外附上密碼。
    async function signIn(username, secret) {
      try {
        await account.fill(username)
        await password.fill(secret)
        await password.press('Enter')
      } catch {
        throw new Error(`瀏覽器登入表單操作失敗（${username}）`)
      }
    }

    // 等最後一項出現後比對整份清單，避免只驗證有一筆資料或忽略多餘項目。
    async function checkList(ids) {
      await page.getByRole('button', { name: new RegExp(deviceName(ids.at(-1))) }).waitFor()
      // 新設備的即時狀態也須在清單上以文字顯示。
      const actual = (await listItems.allInnerTexts()).map(text => text.replace(/\s+/g, ' ').trim())
      const expected = ids.map(id => `${deviceName(id)} ${id.startsWith('north') ? '北區場域' : '南區場域'} 尚未回報`)
      check(actual, expected, '瀏覽器設備清單與登入範圍不符')
    }

    // 檢查登出從請求到 Cookie 移除的結果，也能驗證第一次中斷後的手動復原。
    async function signOut(interrupted = false) {
      // 先留下有效 Cookie 供最後的日誌敏感值掃描。
      const active = (await context.cookies()).find(cookie => cookie.name === 'iot_session')
      assert.ok(active, '瀏覽器須持有有效登入 Cookie')
      tokens.push(`iot_session=${active.value}`)
      // 中斷一次登出請求，確認結果未明時先收起設備資料並停用登入。
      if (interrupted) {
        // 只中斷第一筆 POST，讓後續手動重試可以抵達服務。
        await page.route(origin + '/api/logout', route => route.abort(), { times: 1 })
        await logout.click()
        await page
          .getByRole('alert')
          .filter({ hasText: '登出結果未確認' })
          .waitFor()
        check(await listItems.count(), 0, '登出結果未確認時須隱藏設備資料')
        assert.ok(await account.isDisabled(), '登出結果未確認時須停用登入')
      }
      // 先監聽對應的登出 POST，再按下正常登出或確認重試按鈕。
      const action = interrupted ? page.getByRole('button', { name: '確認並重試登出' }) : logout
      const [response] = await Promise.all([
        page.waitForResponse(item => item.url() === origin + '/api/logout' && item.request().method() === 'POST'),
        action.click(),
      ])
      // 以伺服器回應、畫面及瀏覽器 Cookie 同時核對登出結果。
      check(response.status(), 204, '瀏覽器登出請求應成功')
      await page
        .getByRole('alert')
        .filter({ hasText: '已登出。' })
        .waitFor()
      check(await listItems.count(), 0, '登出後不得保留設備資料')
      const cookies = await context.cookies()
      // 確認瀏覽器已清掉登入識別，避免重載時再送出舊 Cookie。
      assert.ok(!cookies.some(cookie => cookie.name === 'iot_session'), '登出後瀏覽器須移除 session Cookie')
    }

    // 先觀察錯誤密碼提示，再用同一張表單登入，核對總部資料及正式主機 Cookie。
    await signIn('hq', wrongPassword)
    await page
      .getByRole('alert')
      .filter({ hasText: '帳號或密碼錯誤。' })
      .waitFor()
    await signIn('hq', passwords.hq)
    await checkList(inventory)
    // 以 Cookie 的名稱與網域確認登入憑證屬於正式主機。
    const hasAppSession = (await context.cookies(origin)).some(cookie =>
      cookie.name === 'iot_session' && cookie.domain === new URL(origin).hostname)
    assert.ok(hasAppSession, '瀏覽器須持有正式主機的登入 Cookie')

    // 以同一個已登入的瀏覽器造訪另一個本機服務，接收端只保留 Cookie 是否存在。
    let receivedSession
    // 探測端只記錄 Cookie 是否存在，避免讀取或保存憑證值。
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
    // 回到正式主機仍能讀清單，並確認探測服務確實收到請求且未收到 session。
    await page.goto(origin)
    await checkList(inventory)
    assert.notEqual(receivedSession, undefined, '另一個本機服務須收到探測請求')
    assert.equal(receivedSession, false, '另一個本機服務不得收到 iot_session')

    // 從清單進出詳細頁，再以重載驗證瀏覽器仍能恢復有效登入。
    await page.getByRole('button', { name: /北區設備 01/ }).click()
    await page.getByRole('heading', { name: '北區設備 01', exact: true }).waitFor()
    await page.getByText('north-01', { exact: true }).waitFor()
    await page.getByRole('button', { name: /返回設備清單/ }).click()
    await checkList(inventory)
    await page.reload()
    await checkList(inventory)
    // 總部登出後重載回到表單，再分別核對北區與南區帳號能看到的設備。
    await signOut()
    await page.reload()
    await account.waitFor()
    for (const [username, ids] of [['north', north], ['south', south]]) {
      await signIn(username, passwords[username])
      await checkList(ids)
      await signOut(username === 'south')
    }
  } finally {
    // 即使操作或斷言失敗也關閉瀏覽器，讓外層清理能接著回收服務。
    await browser.close()
  }
}

// 透過正式啟動入口、真實 PostgreSQL 及 Chrome 驗證本機登入、權限與重啟。
test('IoT 票 01 的本機端到端驗收', { timeout: 600_000 }, async (t) => {
  const stack = await createStack()
  // 啟動前先註冊清理，確保失敗時仍能回收已建立的專案資源。
  t.after(() => stack.cleanup())
  stack.start()
  // 讀取隔離專案憑證並保留原始值，供重啟與日誌檢查比對。
  const { origin, loopbackOrigin, sql } = stack
  const passwordPath = join(stack.state, 'demo-passwords.json')
  const dbSecretPath = join(stack.state, 'db-password')
  const originalPasswords = readFileSync(passwordPath, 'utf8')
  const passwords = parseJson(originalPasswords)
  const dbSecret = readFileSync(dbSecretPath, 'utf8')
  const tokens = []
  const csrf = { Origin: origin, 'X-CSRF-Protection': '1' }

  // 依案例保留指定 Cookie、Host 或傳輸方式，並只用路徑與狀態回報失敗。
  async function send(path, status, { body, chunks, cookie, headers = {}, post = false } = {}) {
    const response = await stack.send(path, {
      method: post ? 'POST' : 'GET', body, chunks,
      headers: { ...(cookie ? { Cookie: cookie } : {}), ...headers },
    })
    assert.equal(response.status, status, `${post ? 'POST' : 'GET'} ${path}`)
    return response
  }

  // 以正式 Origin 與 JSON 標頭送登入，讓失敗案例可集中改變帳號或密碼。
  const login = (username, password, status, cookie) =>
    send('/api/login', status, {
      post: true, body: JSON.stringify({ username, password }), cookie,
      headers: { ...csrf, 'Content-Type': 'application/json' },
    })

  // 先確認登入身分及 Cookie 契約，再將識別收進待掃描的敏感值清單。
  async function sessionCookie(response, username) {
    check(await json(response), users[username], '登入身分不符')
    const cookie = cookieFrom(response)
    tokens.push(cookie)
    return cookie
  }

  // 比對固定錯誤回應結構，避免錯誤本文直接出現在測試報告。
  async function errorBody(response, error) {
    check(await json(response), { error }, 'HTTP 錯誤內容不符')
  }

  // 以穩定順序保存業務資料快照，供查詢及重啟後比對。
  const businessState = `
    SELECT jsonb_build_array(
      (SELECT jsonb_agg(to_jsonb(sites) ORDER BY id) FROM sites),
      (SELECT jsonb_agg(to_jsonb(devices) ORDER BY id) FROM devices),
      (SELECT jsonb_agg(to_jsonb(accounts) ORDER BY username) FROM accounts));
  `
  const initialData = sql(businessState)

  // 核對預載筆數與私密檔案保護，並直接查看資料庫驗證 Argon2id 儲存方式。
  await t.test('初始化資料、受保護的獨立密碼與 Argon2id', () => {
    check(sql("SELECT (SELECT count(*) FROM sites) || ',' || (SELECT count(*) FROM devices) || ',' || (SELECT count(*) FROM accounts);"),
      '2,6,3', '預載資料筆數不符')
    privateFile(stack.state, true)
    privateFile(passwordPath)
    privateFile(dbSecretPath)
    assert.ok(/^[a-f0-9]{64}\n$/.test(dbSecret), '資料庫須使用獨立產生的憑證')
    check(Object.keys(passwords).sort(), ['hq', 'north', 'south'], '示範帳號清單不符')
    // 檢查產生的密碼格式及互異性，避免三個示範帳號共用憑證。
    const secrets = Object.values(passwords)
    assert.ok(secrets.every(secret => credentialPattern.test(secret)) && new Set(secrets).size === 3,
      '示範密碼須為各自生成的強隨機值')
    // 逐筆取出雜湊並擷取 salt，檢查核准的 Argon2id 參數及個別 salt。
    const hashes = sql('SELECT password_hash FROM accounts ORDER BY username;').split('\n')
    const pattern = /^\$argon2id\$v=19\$m=19456,t=2,p=1\$([A-Za-z0-9+/]{22})\$[A-Za-z0-9+/]{43}$/
    const salts = hashes.map(hash => pattern.exec(hash)?.[1])
    // 每筆格式有效且 salt 不同，才符合獨立的雜湊設定。
    assert.ok(salts.every(Boolean) && new Set(salts).size === 3, 'Argon2id 參數或獨立 salt 不符')
    // 對照明文與資料快照，失敗時只回報是否發現敏感值。
    assert.ok(secrets.every(secret => !initialData.includes(secret)), '資料庫不得保存示範明文密碼')
  })

  let hqCookie
  // 從 API 驗證身分決定可見設備，並用讀取前後快照確認查詢不會變更資料。
  await t.test('三種身分、穩定分頁、偽造參數與越權隱藏', async () => {
    // 未登入時，身分、清單及單筆查詢都須走同一個拒絕契約。
    for (const path of ['/api/session', '/api/devices', '/api/devices/north-01']) {
      await errorBody(await send(path, 401), 'unauthenticated')
    }
    // 分別建立總部與場域登入，以各自的 session 驗證可見範圍。
    for (const [username, ids] of [['hq', inventory], ['north', north], ['south', south]]) {
      const cookie = await sessionCookie(await login(username, passwords[username], 200), username)
      if (username === 'hq') hqCookie = cookie
      const before = sql('SELECT * FROM sessions ORDER BY token_hash;')
      check(await json(await send('/api/session', 200, { cookie })), users[username], '目前登入身分不符')
      // 涵蓋預設、上限、位移、空頁與重查，確認分頁排序及總數一致。
      for (const [query, expected, limit, offset] of [
        ['', ids, 50, 0], ['?limit=100', ids, 100, 0], ['?limit=2', ids.slice(0, 2), 2, 0],
        ['?limit=2&offset=2', ids.slice(2, 4), 2, 2], ['?offset=99', [], 50, 99], ['?limit=2', ids.slice(0, 2), 2, 0],
      ]) {
        const page = await json(await send('/api/devices' + query, 200, { cookie }))
        // 只擷取每筆設備識別，與分頁欄位一起對照各身分的固定清單。
        check([page.items.map(item => item.id), page.total, page.limit, page.offset],
          [expected, ids.length, limit, offset], '設備分頁結果或排序不符')
      }
      // 單筆讀取也按角色範圍核對，並確認設備與場域識別相符。
      for (const id of ids) {
        const detail = await json(await send('/api/devices/' + id, 200, { cookie }))
        check([detail.id, detail.siteId], [id, id.split('-')[0]], '設備單筆內容不符')
      }
      // 送出偽造權限和無效分頁參數，確認伺服器不採用用戶端指定的範圍。
      for (const query of ['siteId=south', 'role=headquarters', 'limit=101', 'limit=0', 'limit=2.5', 'limit=1&limit=2', 'offset=-1']) {
        await errorBody(await send('/api/devices?' + query, 400, { cookie }), 'invalid_input')
      }
      await errorBody(await send('/api/devices/' + ids[0] + '?siteId=north&role=headquarters', 400, { cookie }), 'invalid_input')
      // 場域帳號讀取越權與不存在設備時須收到相同回應，避免洩漏存在性。
      if (username !== 'hq') {
        const blocked = await send('/api/devices/' + (username === 'north' ? south[0] : north[0]), 404, { cookie })
        const missing = await send('/api/devices/missing-01', 404, { cookie })
        check(await json(blocked), { error: 'not_found' }, '跨場域單筆須隱藏存在性')
        await errorBody(missing, 'not_found')
      }
      // 比對 session 和業務資料快照，確認查詢不會延長登入或寫入資料。
      check(sql('SELECT * FROM sessions ORDER BY token_hash;'), before, '讀取不得延長或更改 session')
      check(sql(businessState), initialData, '讀取不得改變業務資料')
    }
  })

  // 以實際請求區分拒絕原因，再核對 session 換發、到期及登出撤銷。
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
    // 保留原始 JSON 字串，讓重複欄位與多餘物件能原樣抵達解析入口。
    for (const body of [
      '{"username":"south","password":123}', '{"username":null,"password":"x"}',
      '{"username":"south","password":"x","role":"headquarters"}', '{"username":"south","username":"hq","password":"x"}',
      '{"username":"south","password":"x","password":"y"}', '{"username":"south"}', '[]', valid + '{}',
    ]) {
      await errorBody(await send('/api/login', 400, { post: true, body, headers: jsonHeaders }), 'invalid_input')
    }
    await errorBody(await send('/api/login', 415, { post: true, body: valid, headers: csrf }), 'unsupported_media_type')
    // 同時檢查固定長度和分段上傳的超限拒絕，並留精確上限的本文稍後驗證成功。
    const boundary = valid.padEnd(16384, ' ')
    check(Buffer.byteLength(boundary), 16384, '測試本文須恰好 16 KiB')
    const oversized = boundary + ' '
    await errorBody(await send('/api/login', 413, { post: true, body: oversized, headers: jsonHeaders }), 'body_too_large')
    await errorBody(await send('/api/login', 413, {
      post: true, chunks: [oversized.slice(0, 8192), oversized.slice(8192)],
      headers: { ...jsonHeaders, 'Transfer-Encoding': 'chunked' },
    }), 'body_too_large')
    // 對照既有帳號密碼錯誤及不存在帳號，避免回應揭露帳號是否存在。
    for (const username of ['hq', 'unknown-user']) {
      const response = await login(username, wrongPassword, 401)
      await errorBody(response, 'invalid_credentials')
      check(response.headers.getSetCookie().length, 0, '失敗登入不得設定 Cookie')
    }
    // 用恰好 16 KiB 的有效本文登入，再帶原 Cookie 登入以證明舊識別失效。
    const first = await sessionCookie(await send('/api/login', 200, { post: true, body: boundary, headers: jsonHeaders }), 'south')
    const second = await sessionCookie(await login('south', passwords.south, 200, first), 'south')
    assert.ok(first !== second, '登入須換發 session 識別')
    await errorBody(await send('/api/session', 401, { cookie: first }), 'unauthenticated')
    // 透過摘要定位剛換發的 session，依資料庫時鐘核對八小時期限。
    const tokenHash = digest(second)
    const lifetime = Number(sql(`
      SELECT EXTRACT(EPOCH FROM expires_at - statement_timestamp())
      FROM sessions WHERE token_hash = decode('${tokenHash}', 'hex') AND username = 'south';
    `))
    assert.ok(lifetime > 28795 && lifetime <= 28800, '新 session 的絕對期限須為 8 小時')
    check(sql('SELECT bool_and(octet_length(token_hash) = 32) FROM sessions;'), 't', '資料庫只保存 session 摘要')
    // 只將這筆 session 調整為過期，再走正式 API 驗證拒絕。
    check(sql(`
      UPDATE sessions SET expires_at = statement_timestamp() - interval '1 second'
      WHERE token_hash = decode('${tokenHash}', 'hex') RETURNING 1;
    `), '1', '只讓測試用 session 到期')
    await errorBody(await send('/api/session', 401, { cookie: second }), 'unauthenticated')

    // 用另一筆有效登入驗證遭拒的登出無副作用，成功登出後則立即失效。
    const live = await sessionCookie(await login('south', passwords.south, 200), 'south')
    for (const headers of [{ Origin: origin }, { 'X-CSRF-Protection': '1' }, { ...csrf, Origin: 'https://example.invalid' }]) {
      await errorBody(await send('/api/logout', 403, { post: true, cookie: live, headers }), 'forbidden')
      await send('/api/session', 200, { cookie: live })
    }
    const logout = await send('/api/logout', 204, { post: true, cookie: live, headers: csrf })
    // 確認清除標頭同時指定空識別與零效期，再以原 Cookie 讀回確認撤銷。
    assert.ok(logout.headers.getSetCookie().some(cookie => cookie.startsWith('iot_session=;') && cookie.includes('Max-Age=0')),
      '登出須清除 Cookie')
    await errorBody(await send('/api/session', 401, { cookie: live }), 'unauthenticated')
    check(sql(`SELECT count(*) FROM sessions WHERE token_hash = decode('${digest(live)}', 'hex');`), '0', '登出須立即刪除 session')
  })

  // 將瀏覽器流程列為獨立驗收，讓 HTTP 與瀏覽器失敗可分開定位。
  await t.test('真實 Chrome 鍵盤登入、Cookie 隔離、清單／詳細資料、重載與登出', async () => {
    await browserFlow(origin, passwords, tokens)
  })

  // 先確認第二實例遭拒且原服務仍可用，再走正常停止及啟動驗證資料持久性。
  await t.test('第二實例遭啟動鎖拒絕，停止再啟動保留資料、憑證與既有登入', async () => {
    assert.ok(hqCookie, '重啟前須有總部 session')
    stack.secondInstance()
    await send('/healthz', 204)
    await send('/api/session', 200, { cookie: hqCookie })
    stack.stop()
    stack.start()
    // 重啟後比對憑證、業務快照及原登入，確認沿用同一份展示資料。
    check(readFileSync(passwordPath, 'utf8'), originalPasswords, '重啟須重用示範憑證')
    check(readFileSync(dbSecretPath, 'utf8'), dbSecret, '重啟須重用資料庫憑證')
    check(sql(businessState), initialData, '重啟須保留業務資料及原本的密碼雜湊')
    check(await json(await send('/api/session', 200, { cookie: hqCookie })), users.hq, '重啟須保留有效 session')
  })

  // 彙集啟動與容器日誌後掃描實際用過的敏感值，斷言只揭露有無洩漏。
  await t.test('日誌不含憑證、session、雜湊或敏感登入本文', () => {
    stack.collectLogs()
    const hashes = sql('SELECT password_hash FROM accounts;').split('\n')
    // 從已記錄的 Cookie 取出裸 token，也能找到沒有 Cookie 名稱前綴的洩漏。
    const liveTokens = tokens.map(cookie => cookie.slice('iot_session='.length))
    const allLogs = stack.logs.join('\n')
    for (const secret of [...Object.values(passwords), dbSecret.trim(), ...hashes, ...liveTokens, wrongPassword]) {
      assert.ok(secret && !allLogs.includes(secret), '日誌含敏感資料')
    }
  })
})
