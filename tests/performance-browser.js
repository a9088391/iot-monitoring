import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { existsSync, lstatSync, mkdirSync, readFileSync, realpathSync } from 'node:fs'
import { join } from 'node:path'
import { setTimeout as wait } from 'node:timers/promises'
import { chromium } from 'playwright-core'
import { observeBeforeDeadline } from './performance-observation.js'
import { roster } from './performance-score.js'

const width = 500
const height = 375
const profiles = [
  ['hq', 'devices', 0], ['hq', 'devices', 1], ['hq', 'devices', 0], ['hq', 'devices', 1],
  ['north', 'devices', 0], ['south', 'devices', 0], ['north', 'devices', 0], ['south', 'devices', 0],
  ['north', 'alerts', 0], ['south', 'alerts', 0],
]
// 場域清單及告警的末列沒有分頁留白，避免下方視窗遮住已滾到底的文字。
const positions = [0, 1, 4, 5, 2, 3, 10, 11, 8, 9]

export const watching = profiles.map(([role, view, page], index) => ({
  id: `watch-${index + 1}`, role, view, page,
  devices: roster.filter(device => device.siteId === (role === 'hq' ? (page ? 'south' : 'north') : role))
    .map(device => device.id),
}))

// 既有主機期限也包住啟動步驟；如果送出前已中止，不再送到 Chrome。
function startupStep(operation, signal, timeoutMs) {
  return observeBeforeDeadline(() => {
    signal.throwIfAborted()
    return operation()
  }, signal, timeoutMs)
}

// 專用原生瀏覽器使用預設 context，避免 Playwright 對新 context 開啟焦點模擬。
async function openVisibleBrowser(profile, owned, signal) {
  signal.throwIfAborted()
  const bounded = (operation, timeoutMs) => startupStep(operation, signal, timeoutMs)
  const executablePath = process.env.CHROME_EXECUTABLE ?? [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/google-chrome', '/usr/bin/chromium',
  ].find(path => existsSync(path))
  assert.ok(executablePath && existsSync(executablePath), '找不到可見 Chrome 執行檔')
  delete process.env.DEBUG
  delete process.env.PWDEBUG
  mkdirSync(profile, { mode: 0o700 })
  const child = spawn(realpathSync(executablePath), [
    `--user-data-dir=${profile}`, '--remote-debugging-address=127.0.0.1', '--remote-debugging-port=0',
    '--enable-automation', '--no-first-run', '--no-default-browser-check', '--disable-background-networking',
    '--disable-component-update', '--disable-sync', '--no-startup-window',
  ], { stdio: 'ignore' })
  const owner = { child, outcome: null }
  owner.ended = new Promise(resolve => {
    // 只有這次 spawn 的程序可被回收；保留退出狀態以辨識啟動錯誤。
    function finish(outcome) {
      owner.outcome = outcome
      resolve(outcome)
    }
    child.once('error', error => finish(error.code ?? 'spawn error'))
    child.once('exit', (code, signal) => finish(signal ?? code))
  })
  owned.push(owner)
  const endpoint = await nativeEndpoint(profile, owner, signal)
  await auditedTransport(endpoint, owner, signal)
  const browser = await bounded(() => chromium.connectOverCDP(owner.transport, {
    noDefaults: true, isLocal: true, timeout: 8_000,
  }), 8_000)
  owner.browser = browser
  const control = await bounded(() => browser.newBrowserCDPSession())
  const { processInfo } = await bounded(() => control.send('SystemInfo.getProcessInfo'))
  assert.equal(processInfo.find(process => process.type === 'browser')?.id, child.pid, 'CDP 必須屬於本次啟動的 Chrome')
  owner.control = control
  const { arguments: args } = await bounded(() => control.send('Browser.getBrowserCommandLine'))
  const override = /^--(headless|disable-background-timer-throttling|disable-backgrounding-occluded-windows|disable-renderer-backgrounding|(?:enable|disable)-features)(=|$)/
  assert.ok(args.includes(`--user-data-dir=${profile}`) && !args.some(arg => override.test(arg)),
    'Chrome 須使用本次 profile 且無節流或特性覆寫旗標')
  return { owner, args, version: browser.version() }
}

// 透過 Playwright 支援的 CDP 傳輸留存方法名稱，不保存可含密碼的請求參數。
async function auditedTransport(endpoint, owner, signal) {
  signal.throwIfAborted()
  const socket = new WebSocket(endpoint)
  const methods = new Map()
  const transport = {
    send(message) {
      methods.set(message.method, (methods.get(message.method) ?? 0) + 1)
      socket.send(JSON.stringify(message))
    },
    close() { socket.close() },
  }
  // 連線仍在交握時也先登記，確保中止可關閉本次 WebSocket。
  owner.transport = transport
  owner.methods = methods
  socket.addEventListener('message', event => transport.onmessage?.(JSON.parse(event.data)))
  socket.addEventListener('close', event => transport.onclose?.(`Chrome CDP closed (${event.code})`))
  try {
    await once(socket, 'open', { signal: AbortSignal.any([signal, AbortSignal.timeout(8_000)]) })
    signal.throwIfAborted()
  } catch {
    transport.close()
    signal.throwIfAborted()
    throw new Error('隔離 Chrome 的 CDP WebSocket 無法開啟')
  }
}

// 只從這次新建的 profile 取得短小的啟動憑據，端點固定為 IPv4 loopback。
async function nativeEndpoint(profile, owner, signal) {
  const path = join(profile, 'DevToolsActivePort')
  const deadline = performance.now() + 12_000
  while (performance.now() < deadline) {
    signal.throwIfAborted()
    assert.equal(owner.outcome, null, 'Chrome 在開啟原生除錯端點前退出')
    if (existsSync(path)) {
      const metadata = lstatSync(path)
      assert.ok(metadata.isFile() && metadata.size < 1024, 'Chrome 除錯端點檔案不符')
      const raw = readFileSync(path, 'utf8').trim()
      const address = /^([1-9]\d{0,4})\r?\n(\/devtools\/browser\/[0-9a-f-]{36})$/.exec(raw)
      if (address && Number(address[1]) <= 65535) return `ws://127.0.0.1:${address[1]}${address[2]}`
    }
    await wait(25, undefined, { signal })
  }
  throw new Error('未收到本次 Chrome 的有效 DevToolsActivePort')
}

// 先讓已驗證的 Chrome 正常退出，逾時才停止本次 spawn 的程序。
export async function closeWatchers(owned) {
  const outcomes = await Promise.allSettled(owned.map(async owner => {
    if (owner.outcome === null && owner.control) {
      const closing = owner.control.send('Browser.close').catch(() => {})
      await Promise.race([closing, wait(1_000, undefined, { ref: false })])
      await Promise.race([owner.ended, wait(2_000, undefined, { ref: false })])
    }
    if (owner.outcome === null) {
      owner.child.kill('SIGTERM')
      await Promise.race([owner.ended, wait(2_000, undefined, { ref: false })])
    }
    if (owner.outcome === null) {
      owner.child.kill('SIGKILL')
      await Promise.race([owner.ended, wait(2_000, undefined, { ref: false })])
    }
    owner.transport?.close()
    assert.notEqual(owner.outcome, null, '隔離 Chrome 未結束')
  }))
  assert.ok(outcomes.every(outcome => outcome.status === 'fulfilled'), '隔離 Chrome 清理未完成')
}

// 每個視窗使用獨立的真實登入；錯誤診斷不輸出 Playwright 的表單內容。
export async function openWatchers(profileRoot, origin, passwords, pages, owned, signal) {
  const tokens = new Set()
  const bounded = operation => startupStep(operation, signal, 10_000)
  for (const [index, watcher] of watching.entries()) {
    // 逐個等候同一視窗的啟動步驟，任何逾時或中止都不能繼續開啟下一個視窗。
    signal.throwIfAborted()
    const { owner, args, version } = await openVisibleBrowser(join(profileRoot, watcher.id), owned, signal)
    const [context] = owner.browser.contexts()
    assert.ok(context && owner.browser.contexts().length === 1, '必須使用 Chrome 既有的唯一預設 context')
    const page = await bounded(() => context.newPage())
    page.setDefaultTimeout(10_000)
    const cdp = await bounded(() => context.newCDPSession(page))
    const { windowId } = await bounded(() => cdp.send('Browser.getWindowForTarget'))
    const screen = await bounded(() => page.evaluate(() => ({
      left: screen.availLeft, top: screen.availTop, width: screen.availWidth, height: screen.availHeight,
    })))
    const stepX = Math.floor((screen.width - width) / 3)
    const stepY = Math.floor((screen.height - height) / 2)
    assert.ok(stepX > 0 && stepY > 0, '原生桌面不足以配置十個視窗')
    const position = positions[index]
    const bounds = {
      left: screen.left + position % 4 * stepX,
      top: screen.top + Math.floor(position / 4) * stepY,
      width, height,
    }
    await bounded(() => cdp.send('Browser.setWindowBounds', { windowId, bounds }))
    const opened = { ...watcher, page, cdp, windowId, screen, bounds, args, version, pid: owner.child.pid, errors: [] }
    pages.push(opened)

    // 只保留錯誤類別和本機路徑；首次登入前的 session 查驗與 favicon 例外有明確範圍。
    let initialSession = true
    page.on('pageerror', error => opened.errors.push({ type: 'page', name: error.name }))
    page.on('response', response => {
      const { pathname } = new URL(response.url())
      const status = response.status()
      if (initialSession && pathname === '/api/session' && status === 401) {
        initialSession = false
        return
      }
      if (status < 400 || (pathname === '/favicon.ico' && status === 404)) return
      opened.errors.push({ type: 'response', pathname, status })
    })
    await bounded(() => page.goto(origin + '/?view=simulation'))
    try {
      await bounded(() => page.getByLabel('帳號', { exact: true }).fill(watcher.role))
      const password = page.getByLabel('密碼', { exact: true })
      await bounded(() => password.fill(passwords[watcher.role]))
      await bounded(() => password.press('Enter'))
    } catch (error) {
      signal.throwIfAborted()
      if (error.code === 'BROWSER_OBSERVATION_TIMEOUT') throw error
      throw new Error(`${watcher.id} 的登入操作失敗`)
    }
    await bounded(() => page.getByRole('heading', { name: '設備清單', level: 1, exact: true }).waitFor())
    const cookies = await bounded(() => context.cookies(origin))
    const token = cookies.find(cookie => cookie.name === 'iot_session')?.value
    assert.ok(token && !tokens.has(token), '十個觀看工作階段須有相異登入')
    tokens.add(token)
    // 窄視窗預選詳情後須明確返回，才能以原生清單列取得效能證據。
    await bounded(() => page.locator('.detail-pane .device-details').waitFor())
    await bounded(() => page.getByRole('button', { name: /返回設備清單/ }).click())
    if (watcher.view === 'alerts') {
      await bounded(() => page.getByRole('button', { name: '告警歷程', exact: true }).click())
      await bounded(() => page.getByText('目前沒有可查看的告警。', { exact: true }).waitFor())
    } else {
      if (watcher.page === 1) {
        await bounded(() => page.getByRole('button', { name: '下一頁', exact: true }).click())
        await bounded(() => page.locator('.detail-pane .device-details').waitFor())
        await bounded(() => page.getByRole('button', { name: /返回設備清單/ }).click())
      }
      await bounded(() => page.getByRole('button', { name: new RegExp(`^${watcher.devices[0]} `) }).waitFor())
      const actual = await bounded(() => page.locator('.device-button:not(.alert-button) > span:first-child').allTextContents())
      assert.deepEqual(actual, watcher.devices, '起始設備頁的識別或可見範圍不符')
    }
    await bounded(() => page
      .locator('#live-status')
      .filter({ hasText: /^即時更新中$/ })
      .waitFor())
  }
  assert.equal(new Set(pages.map(page => page.pid)).size, 10, '必須實際開出十個獨立 Chrome 程序及視窗')
}

// 所有原生視窗完成佈置後才開始滾動，避免將啟動階段的遮蔽混入取證。
export function startRecorders(watchers) {
  return Promise.all(watchers.map(({ page, view }) => page.evaluate(installRecorder, view)))
}

// DOM 有變化才掃描，下一個 rAF 才確認前一幀可繪製；觀察和心跳留在瀏覽器本地。
function installRecorder(view) {
  const records = []
  const active = new Map()
  const queue = []
  const range = document.createRange()
  const selector = view === 'devices' ? '.device-button:not(.alert-button)' : '.alert-button'
  let painting = null
  let lastFrame = 0
  let maxGap = 0
  let dirty = true
  let overflow = false
  let freshView = false
  let frameBounds
  let geometryChanged = false
  const ns = ms => Math.round(ms * 1_000_000).toString()

  // 保留所有狀態改變及隱藏邊界；輸出緩衝滿載須明確拒絕該輪證據。
  function record(value) {
    if (records.length < 4000) records.push(value)
    else overflow = true
  }
  const observer = new MutationObserver(() => { dirty = true })
  observer.observe(document.querySelector('main'), { childList: true, characterData: true, subtree: true })
  document.addEventListener('visibilitychange', () => {
    const visible = document.visibilityState === 'visible'
    const atNs = ns(performance.now())
    record({ type: 'visibility', visible, atNs })
    if (painting) unpainted(painting.row, atNs, 'visibility-change')
    painting = null
    for (const row of queue.splice(0)) unpainted(row, atNs, 'visibility-change')
    if (visible) {
      dirty = true
      freshView = true
      lastFrame = 0
    }
  })

  const partsOf = button => Array.from(button.children, part => part.textContent.replace(/\s+/g, ' ').trim())
  const bounds = () => ({ left: screenX, top: screenY, width: outerWidth, height: outerHeight, innerWidth, innerHeight })

  // 真正變更若來不及繪製，仍須留下缺證；只淘汰復原時被新資料覆蓋的額外快照。
  function unpainted({ sample }, atNs, reason) {
    if (sample.resumed && reason === 'superseded') return
    record({ ...sample, atNs, visible: false, reason })
  }

  // 佇列保存每次真實狀態變更，避免不同設備同批更新時蓋掉前一列的證據。
  function scan(atNs) {
    dirty = false
    const seen = new Set()
    for (const button of document.querySelectorAll(selector)) {
      const parts = partsOf(button)
      const key = view === 'devices' ? parts[0] : parts[0]?.split(' ')[0]
      const state = JSON.stringify(parts)
      if (seen.has(key)) record({ type: 'duplicate', key, parts, atNs })
      seen.add(key)
      if (freshView || active.get(key) !== state) {
        const sample = { type: 'row', key, parts, resumed: freshView }
        if (queue.length < 4000) queue.push({ button, state, sample })
        else overflow = true
      }
      active.set(key, state)
    }
    for (const key of active.keys()) {
      if (seen.has(key)) continue
      record({ type: 'removed', key, atNs })
      active.delete(key)
    }
    freshView = false
  }

  // 量到的是識別及狀態文字的原生矩形，排除只剩按鈕邊框或被頁面自身擋住的情形。
  function textRect(button) {
    const parts = []
    for (const element of button.children) {
      range.selectNodeContents(element)
      const { left, top, right, bottom } = range.getBoundingClientRect()
      if (
        right <= left
        || bottom <= top
        || left < 0
        || top < 0
        || right > innerWidth
        || bottom > innerHeight
        || !button.contains(document.elementFromPoint((left + right) / 2, (top + bottom) / 2))
      ) return null
      parts.push({ x1: left, x2: right, y1: top, y2: bottom })
    }
    return parts.length ? {
      x1: Math.min(...parts.map(part => part.x1)), x2: Math.max(...parts.map(part => part.x2)),
      y1: Math.min(...parts.map(part => part.y1)), y2: Math.max(...parts.map(part => part.y2)),
    } : null
  }

  // 在下一個 rAF 核對同一列和文字，才承認兩幀間有可繪製的機會。
  function finishPaint(atNs, currentBounds) {
    const { row, before, window } = painting
    painting = null
    if (!row.button.isConnected || JSON.stringify(partsOf(row.button)) !== row.state) {
      unpainted(row, atNs, 'superseded')
      return
    }
    const after = textRect(row.button)
    const visible = after !== null && JSON.stringify(window) === JSON.stringify(currentBounds)
    record({ ...row.sample, atNs, visible, before, after, bounds: currentBounds,
      ...(visible ? {} : { reason: 'viewport-changed' }) })
  }

  // 每幀可完成前一列並滾到下一列，讓恢復時的五十列也保留獨立繪製證據。
  function render(frame) {
    if (document.visibilityState === 'visible') {
      const observed = ns(performance.now())
      const window = bounds()
      if (frameBounds && JSON.stringify(frameBounds) !== JSON.stringify(window)) geometryChanged = true
      frameBounds = window
      if (painting) finishPaint(observed, window)
      if (lastFrame) maxGap = Math.max(maxGap, frame - lastFrame)
      lastFrame = frame
      if (dirty) scan(observed)
      while (queue.length) {
        const row = queue.shift()
        if (!row.button.isConnected || JSON.stringify(partsOf(row.button)) !== row.state) {
          unpainted(row, observed, 'superseded')
          continue
        }
        row.button.scrollIntoView({ block: 'center', behavior: 'instant' })
        const before = textRect(row.button)
        if (before) {
          painting = { row, before, window }
          break
        }
        unpainted(row, observed, 'outside-viewport')
      }
    }
    requestAnimationFrame(render)
  }
  requestAnimationFrame(render)
  // 主機每輪僅取一次差異和繪製心跳，避免逐幀跨程序 IPC。
  window.iotPerformanceDrain = () => {
    const batch = {
      remoteNs: ns(performance.now()), lastFrameNs: ns(lastFrame), maxGapNs: ns(maxGap),
      visible: document.visibilityState === 'visible', focused: document.hasFocus(), overflow,
      bounds: bounds(), geometryChanged,
      records: records.splice(0),
    }
    maxGap = 0
    geometryChanged = false
    return batch
  }
}

// 從應用內容矩形扣除所有其他原生視窗，確保每個窗口都有獨立可見的內容。
export function exclusiveContent(bounds, others) {
  const inset = bounds.height - bounds.innerHeight
  let regions = [{
    x1: bounds.left, x2: bounds.left + bounds.innerWidth,
    y1: bounds.top + inset, y2: bounds.top + bounds.height,
  }]
  for (const other of others) {
    regions = regions.flatMap(region => {
      const x1 = Math.max(region.x1, other.left)
      const x2 = Math.min(region.x2, other.left + other.width)
      const y1 = Math.max(region.y1, other.top)
      const y2 = Math.min(region.y2, other.top + other.height)
      if (x1 >= x2 || y1 >= y2) return [region]
      return [
        { ...region, x2: x1 }, { ...region, x1: x2 },
        { x1, x2, y1: region.y1, y2: y1 }, { x1, x2, y1: y2, y2: region.y2 },
      ].filter(part => part.x1 < part.x2 && part.y1 < part.y2)
    })
  }
  return regions.sort((left, right) =>
    (right.x2 - right.x1) * (right.y2 - right.y1) - (left.x2 - left.x1) * (left.y2 - left.y1))[0]
}
