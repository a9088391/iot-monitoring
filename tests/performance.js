import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import {
  appendFileSync, existsSync, linkSync, mkdtempSync, readFileSync, realpathSync, renameSync, rmSync,
  writeFileSync,
} from 'node:fs'
import { cpus, loadavg, platform, release, totalmem } from 'node:os'
import { join } from 'node:path'
import { setTimeout as wait } from 'node:timers/promises'
import { closeWatchers, exclusiveContent, openWatchers, startRecorders } from './performance-browser.js'
import { verifyDisplays, verifyGenerator, verifyResume } from './performance-evidence.js'
import { copyRunEvidence, exposedText, observeBeforeDeadline } from './performance-observation.js'
import { clockBounds, formalEnd, formalSlots, intervalNs, nanos, roster, tailSlots, totalSlots, warmupSlots } from './performance-score.js'
import { createStack } from './stack.js'

const hostNow = () => process.hrtime.bigint()
const names = [
  'ready.json', 'command.json', 'started.json', 'events.ndjson', 'result.json', 'clock-request.json', 'clock-response.json',
]
const ns = 1_000_000_000n
const maxFrameGap = ns / 2n

// 未完成也保留本輪證據；正式量測與探查使用不同終態及固定規格。
async function run(probe) {
  const evidence = mkdtempSync(join(realpathSync('/tmp'), 'iot08-evidence-'))
  console.log(evidence)
  const profileRoot = mkdtempSync(join(realpathSync('/tmp'), 'iot08-chrome-'))
  const report = { status: 'INCOMPLETE', mode: probe ? 'probe' : 'qualification', failures: [] }
  const watchers = []
  const ownedBrowsers = []
  const frames = new Map()
  const browserSamples = new Map()
  const sourceSamples = []
  const stop = new AbortController()
  const signal = () => stop.abort(new Error('使用者中止量測'))
  process.once('SIGINT', signal)
  process.once('SIGTERM', signal)
  let stack
  let started = false
  let serverBounds
  let hiddenPhase = 'waiting'
  let resumedWatcher
  let settledBounds
  const bounded = (operation, timeoutMs) => observeBeforeDeadline(operation, stop.signal, timeoutMs)

  // 證據發布與控制檔都先完整寫入，再於同一目錄原子發布。
  function publish(path, value, exclusive = false) {
    const temporary = path + '.' + randomUUID()
    try {
      writeFileSync(temporary, JSON.stringify(value, null, 2) + '\n', { flag: 'wx', mode: 0o600 })
      if (exclusive) linkSync(temporary, path)
      else renameSync(temporary, path)
    } finally {
      rmSync(temporary, { force: true })
    }
  }

  // 每筆差異依原樣留存，避免在記憶體評分或最後寫檔失敗時遺失已量到的內容。
  function append(name, rows) {
    if (rows.length === 0) return
    appendFileSync(join(evidence, name), rows.map(row => JSON.stringify(row)).join('\n') + '\n', { mode: 0o600 })
  }

  const pathFor = name => join(stack.state, 'performance-' + name)
  // 保留原始區間與其中心，判分仍用完整範圍而非點估計。
  const calibration = (before, after, remoteNs) => ({
    beforeNs: before.toString(), afterNs: after.toString(), remoteNs,
    midpointNs: ((before + after) / 2n).toString(),
    uncertaintyNs: ((after - before + 1n) / 2n).toString(),
  })
  // 只接受已完成原子發布的內容；所有等待均有單調時鐘期限。
  async function readWhen(name, predicate = () => true, timeoutMs = 8_000) {
    const deadline = hostNow() + BigInt(timeoutMs) * 1_000_000n
    while (hostNow() < deadline) {
      stop.signal.throwIfAborted()
      const path = pathFor(name)
      if (existsSync(path)) {
        const value = JSON.parse(readFileSync(path, 'utf8'))
        if (predicate(value)) return value
      }
      await wait(10, undefined, { signal: stop.signal })
    }
    throw new Error(`未收到 ${name} 的完整證據`)
  }

  // 一次只發布一個請求。Go 必須先消費原檔，才允許下一個校時請求。
  async function sampleServerClock() {
    const id = randomUUID()
    const path = pathFor('clock-request.json')
    const before = hostNow()
    publish(path, { id }, true)
    const { serverNs } = await readWhen('clock-response.json', reply => reply.id === id)
    const after = hostNow()
    assert.equal(existsSync(path), false, 'Go 回覆時須已移除校時請求')
    sourceSamples.push(calibration(before, after, serverNs))
    append('server-clock.ndjson', sourceSamples.slice(-1))
    serverBounds = clockBounds(sourceSamples)
  }

  // 取樣前後包住瀏覽器時鐘讀值，同時取得可見、幀距及原生視窗位置。
  async function drainWindows(eligible) {
    const batches = await bounded(() => Promise.all(watchers.map(async watcher => {
      const before = hostNow() - 100_000n
      const batch = await watcher.page.evaluate(() => window.iotPerformanceDrain())
      const after = hostNow() + 100_000n
      return { watcher, batch, sample: calibration(before, after, batch.remoteNs) }
    })))
    // 所有視窗在期限內回覆後才交付；晚到的一批不能改寫已結束的驗收。
    const layout = settledBounds ?? new Map(batches.map(({ watcher, batch }) => [watcher.id, batch.bounds]))
    if (eligible && !settledBounds) settledBounds = layout
    const geometry = []
    for (const { watcher, batch, sample } of batches) {
      const allowedHidden = watcher === resumedWatcher && ['hiding', 'restoring'].includes(hiddenPhase)
      const { bounds, records, visible, ...heartbeat } = batch
      const stable = layout.get(watcher.id)
      const others = [...layout].filter(([id]) => id !== watcher.id).map(([, value]) => value)
      const core = exclusiveContent(stable, others)
      const painted = records.map(row => {
        if (row.type !== 'row' || row.visible !== true) return row
        const screenRect = exposedText(row, stable, core)
        return screenRect ? { ...row, screenRect } : { ...row, visible: false, viewportVisible: true, reason: 'native-obscured' }
      })
      const observations = frames.get(watcher.id) ?? []
      observations.push(...painted)
      frames.set(watcher.id, observations)
      const history = browserSamples.get(watcher.id) ?? []
      history.push(sample)
      browserSamples.set(watcher.id, history)
      append('browser-rows.ndjson', painted.map(row => ({ watcher: watcher.id, ...row })))
      geometry.push({ watcher: watcher.id, ...sample, ...heartbeat, bounds, visible, core, allowedHidden })
      if (!eligible) continue
      if (batch.overflow) report.failures.push({ kind: 'browser-buffer-overflow', watcher: watcher.id })
      if (!visible && allowedHidden) continue
      const screen = watcher.screen
      const exposed = core && core.x2 > core.x1 && core.y2 > core.y1
      if (
        !visible
        || !exposed
        || bounds.left < screen.left
        || bounds.top < screen.top
        || bounds.left + bounds.width > screen.left + screen.width
        || bounds.top + bounds.height > screen.top + screen.height
        || JSON.stringify(bounds) !== JSON.stringify(stable)
        || batch.geometryChanged
      ) report.failures.push({ kind: 'browser-not-exposed', watcher: watcher.id, bounds })
      if (
        nanos(batch.maxGapNs) > maxFrameGap
        || nanos(batch.remoteNs) - nanos(batch.lastFrameNs) > maxFrameGap
      ) report.failures.push({ kind: 'browser-frame-gap', watcher: watcher.id, atNs: batch.remoteNs })
    }
    append('browser-windows.ndjson', geometry)
  }

  // 保留主機背景負載，不用主機節流或容器全域配置改變負載結果。
  function sampleHost() {
    append('host-load.ndjson', [{
      wall: new Date().toISOString(), monoNs: hostNow().toString(), load: loadavg(),
      runnerCpu: process.cpuUsage(), runnerMemory: process.memoryUsage(),
    }])
  }

  // 使用原生視窗狀態變更，復原時回到已核對過的幾何位置。
  async function changeWindow(watcher, windowState) {
    const { cdp, windowId, bounds } = watcher
    await bounded(() => cdp.send('Browser.setWindowBounds', { windowId, bounds: { windowState } }))
    if (windowState === 'normal') await bounded(() => cdp.send('Browser.setWindowBounds', { windowId, bounds }))
  }

  // 探查等實際可見狀態及原生視窗一致，不在瀏覽器注入可見性或焦點設定。
  async function waitForNativeWindow(watcher, windowState, visibility) {
    const deadline = hostNow() + 5n * ns
    while (hostNow() < deadline) {
      stop.signal.throwIfAborted()
      const [{ bounds }, state] = await bounded(() => Promise.all([
        watcher.cdp.send('Browser.getWindowBounds', { windowId: watcher.windowId }),
        watcher.page.evaluate(() => document.visibilityState),
      ]))
      if (bounds.windowState === windowState && state === visibility) return { bounds, visibility: state }
      await wait(25, undefined, { signal: stop.signal })
    }
    throw new Error(`原生視窗未達到 ${windowState}/${visibility}`)
  }

  // 讓下一幀確認內容能實際繪製，主機上的期限也能攔截完全暫停的 renderer。
  function doubleFrame(page) {
    return bounded(() => page.evaluate(() => new Promise(resolve => {
      requestAnimationFrame(() => requestAnimationFrame(() => resolve(document.visibilityState)))
    })), 3_000).catch(error => {
      if (error.code === 'BROWSER_OBSERVATION_TIMEOUT') return 'timeout'
      throw error
    })
  }

  // 只記錄已核對歸屬的 Chrome 程序，避免將桌面的其他瀏覽器納入資源證據。
  function sampleChromeProcesses() {
    return bounded(() => Promise.all(ownedBrowsers.map(async ({ child, control }) => ({
      pid: child.pid, ...(await control.send('SystemInfo.getProcessInfo')),
    }))))
  }

  // 直接檢查協定命令，避免將跨 Chrome 程序的 hasFocus 語意當成是否模擬的判據。
  function checkChromeProtocol(name) {
    const forbidden = new Set([
      'Emulation.setFocusEmulationEnabled', 'Emulation.setVirtualTimePolicy', 'Page.setWebLifecycleState',
      'Page.startScreencast', 'Page.captureScreenshot',
    ])
    const protocols = ownedBrowsers.map(({ child, methods }) => ({ pid: child.pid, methods: Object.fromEntries(methods) }))
    publish(join(evidence, name), protocols)
    for (const { methods } of ownedBrowsers) {
      assert.ok(![...methods.keys()].some(method => forbidden.has(method)), 'Chrome CDP 使用了會影響自然繪製的指令')
    }
  }

  // 探查用真實控制更新原本在視口外的末列，並確認復原後所有列均取得原生繪製證據。
  async function probeNativeVisibility() {
    const device = resumedWatcher.devices.at(-1)
    const offscreen = await bounded(() => resumedWatcher.page.evaluate(id => {
      scrollTo({ top: 0, behavior: 'instant' })
      const row = [...document.querySelectorAll('.device-button')]
        .find(button => button.firstElementChild.textContent === id)
      const top = row.getBoundingClientRect().top
      return { top, innerHeight, status: row.lastElementChild.textContent.trim() }
    }, device))
    assert.ok(offscreen.top >= offscreen.innerHeight && offscreen.status === '尚未回報', '探查末列須在原生視口外且尚未回報')
    hiddenPhase = 'hiding'
    await changeWindow(resumedWatcher, 'minimized')
    const hidden = await waitForNativeWindow(resumedWatcher, 'minimized', 'hidden')
    const headquarters = watchers.find(watcher => watcher.role === 'hq')
    const result = await bounded(() => headquarters.page.evaluate(async id => {
      const path = `/api/devices/${id}`
      const state = await fetch(path + '/simulation')
      if (!state.ok) return { status: state.status }
      const { version } = await state.json()
      const control = await fetch(path + '/simulation', {
        method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Protection': '1' },
        body: JSON.stringify({ action: 'normal', version }),
      })
      if (!control.ok) return { status: control.status }
      const current = await fetch(path)
      if (!current.ok) return { status: current.status }
      return { status: 200, temperature: (await current.json()).latestMeasurement?.temperature }
    }, device))
    assert.deepEqual(result, { status: 200, temperature: 23 }, '隱藏期間須確實提交末列的新溫度')
    assert.equal(await doubleFrame(resumedWatcher.page), 'timeout', '原生隱藏期間不得完成雙 rAF')
    await drainWindows(true)

    hiddenPhase = 'restoring'
    await changeWindow(resumedWatcher, 'normal')
    const visible = await waitForNativeWindow(resumedWatcher, 'normal', 'visible')
    assert.equal(await doubleFrame(resumedWatcher.page), 'visible', '復原視窗須重新繪製')
    hiddenPhase = 'restored'
    const deadline = hostNow() + 4n * ns
    let resumeAt
    const painted = new Map()
    while (hostNow() < deadline) {
      await drainWindows(true)
      const rows = frames.get(resumedWatcher.id)
      const lastHidden = rows.findLastIndex(row => row.type === 'visibility' && row.visible === false)
      const resumed = rows.slice(lastHidden + 1).find(row => row.type === 'visibility' && row.visible === true)
      if (lastHidden >= 0 && resumed) {
        resumeAt = nanos(resumed.atNs)
        for (const row of rows) {
          if (
            row.type !== 'row'
            || row.visible !== true
            || nanos(row.atNs) < resumeAt
            || (row.key === device && row.parts[2] !== '在線 · 23°C')
          ) continue
          if (!painted.has(row.key)) painted.set(row.key, row)
        }
      }
      if (resumedWatcher.devices.every(id => painted.has(id))) break
      await wait(100, undefined, { signal: stop.signal })
    }
    const samples = resumedWatcher.devices.filter(id => painted.has(id)).map(id => {
      const row = painted.get(id)
      return { device: id, delayNs: (nanos(row.atNs) - resumeAt + 100_000n).toString(), screenRect: row.screenRect }
    })
    publish(join(evidence, 'probe-visibility.json'), {
      watcher: resumedWatcher.id, offscreen, hidden, visible, device, adoptedWhileHidden: result, samples,
    })
    assert.equal(samples.length, 50, '復原的每一列都須有原生可見繪製證據')
    assert.ok(samples.every(sample => nanos(sample.delayNs) <= 2n * ns), '所有探查列都須在復原後兩秒內繪製')
  }

  // 固定時槽由 Go 排程；主機只讀證據，並在正式最後一筆已過顯示期限後安排可見性驗收。
  async function observe(startNs) {
    const start = nanos(startNs)
    const hideAt = start + BigInt(formalEnd) * intervalNs + 2_200_000_000n
    const showAt = start + BigInt(formalEnd) * intervalNs + 8_000_000_000n
    const deadline = start + BigInt(totalSlots) * intervalNs + serverBounds.high + 25n * ns
    let nextCalibration = hostNow()
    let finishAfter
    while (hostNow() <= deadline) {
      stop.signal.throwIfAborted()
      const tick = hostNow()
      await drainWindows(true)
      if (tick >= nextCalibration) {
        await sampleServerClock()
        sampleHost()
        nextCalibration = tick + 10n * ns
      }
      if (hiddenPhase === 'waiting' && tick >= hideAt + serverBounds.high) {
        hiddenPhase = 'hiding'
        await changeWindow(resumedWatcher, 'minimized')
      }
      if (hiddenPhase === 'hiding' && tick >= showAt + serverBounds.high) {
        hiddenPhase = 'restoring'
        await changeWindow(resumedWatcher, 'normal')
      }
      if (hiddenPhase === 'restoring' && frames.get(resumedWatcher.id).some(row => row.type === 'visibility' && row.visible)) {
        hiddenPhase = 'restored'
      }
      if (finishAfter === undefined && existsSync(pathFor('result.json'))) finishAfter = tick + 2n * ns
      if (finishAfter !== undefined && tick >= finishAfter) return
      const remaining = ns - (hostNow() - tick)
      if (remaining > 0n) await wait(Number(remaining / 1_000_000n), undefined, { signal: stop.signal })
    }
    throw new Error('超過固定負載證據等待期限')
  }

  try {
    publish(join(evidence, 'report.json'), report)
    const cpu = cpus()
    publish(join(evidence, 'host.json'), {
      node: process.version, platform: platform(), release: release(), architecture: process.arch,
      cpu: { model: cpu[0]?.model, logical: cpu.length }, bytes: totalmem(), startedAt: new Date().toISOString(),
      runner: process.resourceUsage(),
    })
    stack = await createStack({ performance: true })
    stack.start()
    started = true
    const runtime = stack.performanceRuntime()
    const ready = await readWhen('ready.json')
    assert.equal(ready.namespace, runtime.namespace)
    assert.deepEqual(ready.devices, roster)
    assert.deepEqual([ready.intervalNs, ready.warmupSlots, ready.formalSlots, ready.tailSlots],
      [intervalNs.toString(), warmupSlots, formalSlots, tailSlots])
    publish(join(evidence, 'containers.json'), runtime)

    const passwords = JSON.parse(readFileSync(join(stack.state, 'demo-passwords.json'), 'utf8'))
    await openWatchers(profileRoot, stack.origin, passwords, watchers, ownedBrowsers, stop.signal)
    resumedWatcher = watchers.find(watcher => watcher.role === 'south' && watcher.view === 'devices')
    await bounded(() => watchers.at(-1).page.bringToFront())
    const actual = await bounded(() => Promise.all(watchers.map(async ({ cdp, page, ...meta }) => ({
      ...meta,
      actual: (await cdp.send('Browser.getWindowBounds', { windowId: meta.windowId })).bounds,
      documentVisibility: await page.evaluate(() => document.visibilityState),
      initial: await doubleFrame(page),
    }))))
    publish(join(evidence, 'browser.json'), { mode: 'native-default-context-noDefaults', windows: actual })
    const unavailable = actual.filter(window => window.actual.windowState !== 'normal' || window.initial !== 'visible')
    assert.deepEqual(unavailable.map(({ id, actual, documentVisibility, initial }) => ({ id, actual, documentVisibility, initial })),
      [], '十個原生視窗皆須處於正常與可見狀態')
    await bounded(() => startRecorders(watchers))
    const drawing = await Promise.all(watchers.map(watcher => doubleFrame(watcher.page)))
    assert.ok(drawing.every(visibility => visibility === 'visible'), '裝上收集器後仍須持續繪製')
    publish(join(evidence, 'browser-process-start.json'), await sampleChromeProcesses())
    checkChromeProtocol('browser-protocol-start.json')
    await drainWindows(false)
    await drainWindows(true)
    await sampleServerClock()
    sampleHost()
    assert.equal(report.failures.length, 0, '啟動前十個瀏覽器的視窗與繪製條件必須成立')
    if (probe) {
      await probeNativeVisibility()
      checkChromeProtocol('browser-protocol-end.json')
      assert.equal(report.failures.length, 0, '原生可見性探查不可有焦點或繪製異常')
      report.probePassed = true
      return
    }

    // 佈置視窗期間的可見性變動保存在原始檔，正式紀錄從已完成佈置後開始。
    frames.clear()
    publish(pathFor('command.json'), { startDelayMs: 3000 }, true)
    const { startNs } = await readWhen('started.json')
    report.startNs = startNs
    publish(join(evidence, 'report.json'), report)
    console.log(`STARTED ${startNs} ${evidence}`)
    await observe(startNs)
    const result = await readWhen('result.json')
    const events = readFileSync(pathFor('events.ndjson'), 'utf8')
      .trimEnd()
      .split('\n')
      .map(line => JSON.parse(line))
    const generator = verifyGenerator(events, startNs, result)
    append('generator-assessments.ndjson', generator.slots)
    const browserClocks = new Map()
    for (const [id, samples] of browserSamples) browserClocks.set(id, clockBounds(samples))
    const displays = verifyDisplays(watchers, frames, generator, serverBounds, browserClocks, resumedWatcher.id)
    const resume = verifyResume(resumedWatcher, frames.get(resumedWatcher.id), displays.values.get(resumedWatcher.id),
      generator, serverBounds, browserClocks.get(resumedWatcher.id))
    append('display-assessments.ndjson', displays.assessments)
    report.expectedDisplays = 4 * formalSlots + 18
    if (displays.assessments.length !== report.expectedDisplays) {
      report.failures.push({ kind: 'display-plan-count', actual: displays.assessments.length })
    }
    publish(join(evidence, 'resume.json'), resume)

    const reading = JSON.parse(stack.sql(`
      SELECT json_build_object(
        'latest', (SELECT json_agg(m ORDER BY device_id) FROM latest_measurements m),
        'alerts', (SELECT json_agg(a ORDER BY id) FROM alerts a)
      );
    `))
    publish(join(evidence, 'database-readback.json'), reading)
    for (const [type, deviceId, count] of [
      ['temperature', 'north-001', 2], ['temperature', 'south-001', 2], ['offline', 'north-001', 1],
    ]) {
      const items = reading.alerts?.filter(item => item.type === type && item.device_id === deviceId) ?? []
      if (items.length !== count || items.some(item => item.recovered_at_ns === null)) {
        report.failures.push({ kind: 'database-alert-state', type, deviceId, count: items.length })
      }
    }
    if (reading.alerts?.length !== 5) report.failures.push({ kind: 'database-alert-total' })
    for (const [index, device] of roster.entries()) {
      const row = reading.latest?.[index]
      const expected = generator.rows.get(7600 + index)
      if (
        row?.device_id !== device.id
        || row.sequence !== 77
        || row.temperature !== expected?.temperature
      ) report.failures.push({ kind: 'database-device-state', deviceId: device.id })
    }
    if (reading.latest?.length !== roster.length) report.failures.push({ kind: 'database-device-total' })

    for (const watcher of watchers) {
      const changes = frames.get(watcher.id).filter(row => row.type === 'visibility')
      if (watcher !== resumedWatcher && changes.length) report.failures.push({ kind: 'unexpected-visibility', watcher: watcher.id })
      for (const error of watcher.errors) report.failures.push({ kind: 'browser-error', watcher: watcher.id, error })
    }
    if (resume.status !== 'PASS') report.failures.push({ kind: 'visibility-resume', status: resume.status })
    report.failures.push(...generator.failures, ...displays.failures)
    report.displayStatus = {}
    let maximum = null
    for (const assessment of displays.assessments) {
      const { status, delayHighNs } = assessment
      report.displayStatus[status] = (report.displayStatus[status] ?? 0) + 1
      if (status !== 'PASS') report.failures.push({ kind: 'display', key: assessment.key, status })
      if (delayHighNs !== undefined && (maximum === null || BigInt(delayHighNs) > maximum)) maximum = BigInt(delayHighNs)
    }
    report.maximumDelayHighNs = maximum?.toString() ?? null
    report.formalAdopted = generator.slots.filter(row => row.phase === 'formal' && row.status === 'PASS').length
    report.formalExpected = formalSlots
    report.windows = watchers.length
    report.maximumResumeDelayNs = resume.devices?.reduce((max, device) => {
      const delay = Number(device.delayNs ?? 0)
      return Math.max(max, delay)
    }, 0) ?? null
    report.clockBounds = Object.fromEntries([['server', serverBounds], ...browserClocks].map(([id, value]) =>
      [id, { low: value.low.toString(), high: value.high.toString() }]))
    publish(join(evidence, 'browser-process-end.json'), await sampleChromeProcesses())
    checkChromeProtocol('browser-protocol-end.json')
    report.status = report.failures.length ? 'FAIL' : 'PASS'
  } catch (error) {
    report.status = 'INCOMPLETE'
    report.failures.push({ kind: 'runner', message: String(error.message).slice(0, 600) })
  } finally {
    // 先停止觀察和寫入來源，再保留可用檔案，最後只清理取得歸屬的隔離資源。
    try {
      await closeWatchers(ownedBrowsers)
      rmSync(profileRoot, { recursive: true, force: true })
    } catch {
      report.status = 'INCOMPLETE'
      report.failures.push({ kind: 'browser-cleanup' })
    }
    if (stack) {
      try {
        if (started) stack.stop()
      } catch {
        report.status = 'INCOMPLETE'
        report.failures.push({ kind: 'stack-stop' })
      }
      const copyFailures = copyRunEvidence(names.map(name => ({
        name, source: pathFor(name), destination: join(evidence, 'performance-' + name),
      })))
      if (copyFailures.length) {
        report.status = 'INCOMPLETE'
        report.failures.push({ kind: 'copy-evidence', files: copyFailures })
        report.privateEvidenceFallback = stack.state
        console.error(`EVIDENCE_FALLBACK ${stack.state}`)
      }
      try {
        stack.cleanup({ retainSuite: copyFailures.length > 0 })
      } catch {
        report.status = 'INCOMPLETE'
        report.failures.push({ kind: 'stack-cleanup' })
      }
    }
    process.removeListener('SIGINT', signal)
    process.removeListener('SIGTERM', signal)
    report.runner = process.resourceUsage()
    publish(join(evidence, 'report.json'), report)
    console.log(`${report.status} ${join(evidence, 'report.json')}`)
    process.exitCode = report.status === 'PASS' ? 0 : report.status === 'FAIL' ? 1 : 2
  }
}

const flags = process.argv.slice(2)
if (flags.length > 1 || (flags.length === 1 && flags[0] !== '--probe')) {
  console.error('用法：node tests/performance.js [--probe]')
  process.exitCode = 2
} else {
  await run(flags[0] === '--probe')
}
