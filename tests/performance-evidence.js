import { assessDisplays, formalEnd, intervalNs, nanos, plannedSample, totalSlots } from './performance-score.js'

const offlineNs = 15_000_000_000n
const sites = { north: '北區場域', south: '南區場域' }
const temp = /^在線 · (\d+(?:\.\d+)?)°C$/
const offline = /^離線 · (\d+(?:\.\d+)?)°C ?（資料已非即時）$/
const alert = /^#(\d+) · (溫度異常|離線) · ((north|south)-\d{3})$/

// 入口時間缺少或無效時不能推算離線期限，讓該筆保留無時基或缺漏結果。
function offlineDeadline(entryNs) {
  try {
    return (nanos(entryNs) + offlineNs).toString()
  } catch {
    return undefined
  }
}

// 個別核對正式及例外槽位，避免漏排程、拒絕或提早送出混入有效量。
export function verifyGenerator(events, startNs, result) {
  const start = nanos(startNs)
  const failures = []
  const rows = new Map()
  const slots = []
  const plan = Array.from({ length: totalSlots }, (_, slot) => plannedSample(slot)).filter(Boolean)
  for (const [slot, original, offset] of [[-1, 7199, 100_000_000n], [-2, 7299, 200_000_000n]]) {
    plan.push({ ...plannedSample(original), slot, phase: 'invalid', offset })
  }
  const expectedSlots = new Set(plan.map(sample => sample.slot))
  if (result?.complete !== true || result.events !== events.length) failures.push({ kind: 'incomplete-generator' })
  for (const event of events) {
    if (rows.has(event.slot) || !expectedSlots.has(event.slot)) {
      failures.push({ kind: 'unexpected-generator-slot', slot: event.slot })
      continue
    }
    rows.set(event.slot, event)
  }
  for (const sample of plan) {
    const { slot, phase, deviceId, sequence, temperatureCode } = sample
    const actual = rows.get(slot)
    const due = start + (slot >= 0 ? BigInt(slot) * intervalNs : BigInt(formalEnd) * intervalNs + sample.offset)
    const expectedOutcome = phase === 'invalid' ? 'rejected' : 'adopted'
    let status = 'PASS'
    if (!actual) status = 'MISSING'
    else if (
      actual.phase !== phase
      || actual.deviceId !== deviceId
      || actual.sequence !== sequence
      || Math.round(actual.temperature * 100_000) !== temperatureCode
      || actual.plannedNs !== due.toString()
    ) status = 'WRONG_SLOT'
    else if (actual.outcome !== expectedOutcome) status = String(actual.outcome).toUpperCase()
    else {
      try {
        const sent = nanos(actual.sentNs)
        const entry = nanos(actual.entryNs)
        const resultTime = nanos(actual.resultNs)
        if (
          sent < due
          || sent >= due + intervalNs
          || entry < sent
          || resultTime < entry
        ) status = 'WRONG_CLOCK'
      } catch {
        status = 'WRONG_CLOCK'
      }
    }
    slots.push({ slot, phase, deviceId, sequence, plannedNs: due.toString(), expectedOutcome, actual, status })
    if (status !== 'PASS') failures.push({ kind: 'generator-slot', slot, status })
  }
  return { rows, plan, slots, failures }
}

// 兩次溫度異常與離線各自有同 ID 的恢復，單純持續異常不能產生新的告警。
function alertPlan(watcher, rowFor) {
  const south = watcher.role === 'south'
  const device = south ? 'south-001' : 'north-001'
  const index = south ? 50 : 0
  const list = [[16, '異常持續中', 0], [18, '已恢復、待結案', 0], [20, '異常持續中', 1], [21, '已恢復、待結案', 1]]
    .map(([sequence, state, episode], order) => ({
      key: `${watcher.id}/alert/temperature/${order}`, watcher: watcher.id, kind: 'temperature-alert',
      device, state, episode, entryNs: rowFor((sequence - 1) * 100 + index)?.entryNs,
    }))
  if (!south) {
    const last = rowFor(7100)?.entryNs
    list.push(
      {
        key: `${watcher.id}/alert/offline/0`, watcher: watcher.id, kind: 'offline-alert', device,
        state: '異常持續中', episode: 0, entryNs: offlineDeadline(last), strictlyAfter: true,
      },
      {
        key: `${watcher.id}/alert/offline/1`, watcher: watcher.id, kind: 'offline-alert', device,
        state: '已恢復、待結案', episode: 0, entryNs: rowFor(7600)?.entryNs,
      },
    )
  }
  return list
}

// 以溫度唯一碼和場域對照每個 DOM 變更；畫面額外值、倒退及未知告警均保留錯誤。
export function verifyDisplays(watchers, frames, generator, sourceClock, browserClocks, resumedWatcher) {
  const failures = []
  const observations = []
  const expected = []
  const values = new Map()
  const rowFor = slot => generator.rows.get(slot)
  const plan = generator.plan.filter(sample => sample.phase !== 'invalid')
  const byValue = new Map(plan.map(sample => [sample.temperatureCode, sample]))
  for (const watcher of watchers) {
    const visible = frames.get(watcher.id) ?? []
    const scope = new Set(watcher.devices)
    const alertExpected = watcher.view === 'alerts' ? alertPlan(watcher, rowFor) : []
    const indices = new Map()
    const alertIds = new Map()
    const temperatures = new Map()
    const actual = []
    values.set(watcher.id, actual)
    if (watcher.view === 'alerts') expected.push(...alertExpected)
    else {
      for (const sample of plan) {
        if (!scope.has(sample.deviceId) || !['formal', 'recovery'].includes(sample.phase)) continue
        expected.push({
          key: `${watcher.id}/device/${sample.slot}`, watcher: watcher.id,
          kind: sample.phase === 'recovery' ? 'recovery-device' : 'temperature',
          slot: sample.slot, device: sample.deviceId, entryNs: rowFor(sample.slot)?.entryNs,
        })
      }
      if (scope.has('north-001')) {
        const last = rowFor(7100)?.entryNs
        expected.push({
          key: `${watcher.id}/offline`, watcher: watcher.id, kind: 'offline-device',
          device: 'north-001', entryNs: offlineDeadline(last), strictlyAfter: true,
        })
      }
    }

    // 本地 DOM 收集器只報繪製的差異。此處保留原值以辨識遺漏、額外與越權狀態。
    for (const row of visible) {
      if (row.type === 'visibility') continue
      const wrong = kind => failures.push({ kind, watcher: watcher.id, row })
      if (row.type !== 'row') {
        wrong('unexpected-dom-change')
        continue
      }
      if (watcher.view === 'alerts') {
        const [label, state] = row.parts
        const parts = alert.exec(label)
        if (!parts || !scope.has(parts[3])) {
          wrong('unexpected-alert-scope')
          continue
        }
        const [, id, name, device] = parts
        const kind = name === '離線' ? 'offline-alert' : 'temperature-alert'
        const next = indices.get(kind) ?? 0
        const planned = alertExpected.filter(sample => sample.kind === kind)[next]
        const key = `${kind}/${planned?.episode}`
        const original = alertIds.get(key)
        if (
          !planned
          || planned.device !== device
          || planned.state !== state
          || row.parts.length !== 2
          || (original === undefined ? [...alertIds.values()].includes(id) : original !== id)
        ) {
          wrong('unexpected-alert-state')
          continue
        }
        indices.set(kind, next + 1)
        alertIds.set(key, id)
        observations.push({ ...row, key: planned.key })
        continue
      }

      const [device, site, state] = row.parts
      if (
        row.parts.length !== 3
        || !scope.has(device)
        || site !== sites[device.split('-')[0]]
      ) {
        wrong('unexpected-device-scope')
        continue
      }
      const previous = temperatures.get(device)
      if (state === '尚未回報' && !previous) continue
      const isOffline = offline.exec(state)
      const degrees = temp.exec(state) ?? isOffline
      const sample = degrees ? byValue.get(Math.round(Number(degrees[1]) * 100_000)) : undefined
      const adopted = sample ? rowFor(sample.slot) : undefined
      if (
        !sample
        || sample.deviceId !== device
        || adopted?.outcome !== 'adopted'
        || (isOffline && (device !== 'north-001' || sample.slot !== 7100))
        || (previous && sample.sequence < previous.sequence)
      ) {
        wrong('unexpected-device-state')
        continue
      }
      try {
        const bounds = browserClocks.get(watcher.id)
        const latestDisplay = nanos(row.atNs) + bounds.high
        const firstArrival = nanos(adopted.entryNs) + sourceClock.low
        if (latestDisplay < firstArrival) wrong('device-before-entry')
      } catch {
        wrong('invalid-browser-clock')
      }
      temperatures.set(device, sample)
      if (row.visible === true) actual.push({ ...row, device, sequence: sample.sequence })
      if (row.resumed && watcher.id === resumedWatcher) continue
      const key = isOffline ? `${watcher.id}/offline` : `${watcher.id}/device/${sample.slot}`
      observations.push({ ...row, key })
    }
  }
  const assessments = assessDisplays(expected, observations, sourceClock, browserClocks)
  return { assessments, failures, values }
}

// 恢復以同一瀏覽器的可見事件計時，核對全部授權設備至少達到恢復時可成立的最新版本。
export function verifyResume(watcher, frames, values, generator, sourceClock, browserClock) {
  const hidden = frames.filter(row => row.type === 'visibility' && row.visible === false)
  const shown = frames.filter(row => row.type === 'visibility' && row.visible === true)
  if (hidden.length !== 1 || shown.length !== 1) return { status: 'MISSING_VISIBILITY_BOUNDARY' }
  const start = nanos(shown[0].atNs)
  if (start <= nanos(hidden[0].atNs)) return { status: 'WRONG_VISIBILITY_ORDER' }
  const possibleResume = start + browserClock.high
  const hiddenBefore = nanos(hidden[0].atNs) + browserClock.high
  const latest = new Map()
  for (const event of generator.rows.values()) {
    if (
      !watcher.devices.includes(event.deviceId)
      || event.outcome !== 'adopted'
      || nanos(event.resultNs) + sourceClock.low > possibleResume
    ) continue
    if ((latest.get(event.deviceId)?.sequence ?? 0) < event.sequence) latest.set(event.deviceId, event)
  }
  let changed = 0
  const devices = watcher.devices.map(device => {
    const target = latest.get(device)
    if (!target) return { device, status: 'MISSING' }
    if (nanos(target.resultNs) + sourceClock.low > hiddenBefore) changed += 1
    const evidence = values.find(row =>
      row.device === device
      && row.sequence >= target.sequence
      && nanos(row.atNs) >= start)
    if (!evidence) return { device, sequence: target.sequence, status: 'MISSING' }
    const delay = nanos(evidence.atNs) - start + 100_000n
    return { device, sequence: target.sequence, delayNs: delay.toString(), status: delay <= 2_000_000_000n ? 'PASS' : 'LATE' }
  })
  return { status: changed > 0 && devices.every(device => device.status === 'PASS') ? 'PASS' : 'FAIL', changed, devices }
}
