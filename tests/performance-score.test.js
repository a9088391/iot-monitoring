import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'
import { verifyDisplays, verifyGenerator } from './performance-evidence.js'
import { copyRunEvidence, exposedText, observeBeforeDeadline } from './performance-observation.js'
import { assessDisplays, clockBounds, plannedSample } from './performance-score.js'

const sample = { key: 'north/device', watcher: 'north', entryNs: '1000000000' }
const evidence = { key: sample.key, visible: true, atNs: '2999999990' }
const source = { low: 0n, high: 10n }
const browser = new Map([['north', { low: 0n, high: 20n }]])

// 缺樣本或逾時不能由有效樣本的平均值抵銷。
test('效能逐筆判分拒絕缺少、逾時、重複、不可見和無時基證據', () => {
  assert.equal(assessDisplays([sample], [], source, browser)[0].status, 'MISSING')
  const late = assessDisplays([sample], [evidence], source, browser)[0]
  assert.equal(late.status, 'LATE')
  assert.equal(late.delayHighNs, '2000000010')
  assert.equal(assessDisplays([sample], [evidence, evidence], source, browser)[0].status, 'DUPLICATE')
  assert.equal(assessDisplays([sample], [{ ...evidence, visible: false }], source, browser)[0].status, 'HIDDEN')
  assert.equal(assessDisplays([sample], [evidence], source, new Map())[0].status, 'INVALID_CLOCK')
  const boundary = assessDisplays([sample], [{ ...evidence, atNs: '2999999980' }], source, browser)[0]
  assert.equal(boundary.status, 'PASS')
})

// 選快探查或反向區間會低估真正的最壞延遲。
test('效能時基聯集涵蓋每次探查，拒絕反向或過寬區間', () => {
  const first = { beforeNs: '100', afterNs: '120', remoteNs: '200' }
  const second = { beforeNs: '130', afterNs: '160', remoteNs: '220' }
  assert.deepEqual(clockBounds([first, second]), { low: -100n, high: -60n })
  assert.throws(() => clockBounds([{ ...first, afterNs: '99' }]), /反向/)
  assert.throws(() => clockBounds([{ ...first, afterNs: '1000000101' }]), /過寬/)
})

// 以固定槽位覆蓋暖機、正式、失聯及恢復的邊界。
test('負載計畫固定 6000 正式槽位且只排除四個刻意離線槽', () => {
  const slots = Array.from({ length: 7700 }, (_, slot) => plannedSample(slot)).filter(Boolean)
  assert.equal(slots.filter(row => row.phase === 'formal').length, 6000)
  assert.equal(slots.length, 7696)
  assert.deepEqual(plannedSample(1500), {
    slot: 1500, phase: 'formal', deviceId: 'north-001', sequence: 16, temperatureCode: 3001601,
  })
  assert.equal(plannedSample(7500), null)
  assert.equal(plannedSample(7600).phase, 'recovery')
})

// 每筆正式回報即使未送出也要留下評分項，重複及無效回報不能補足。
test('產生器判分保留漏排程及被重複的正式槽位', () => {
  const event = {
    phase: 'formal', slot: 1200, deviceId: 'north-001', sequence: 13, temperature: 20.01301,
    plannedNs: '61000000000', outcome: 'missed',
  }
  const result = verifyGenerator([event, event], '1000000000', { complete: true, events: 2 })
  const formal = result.slots.filter(row => row.phase === 'formal')
  assert.equal(formal.length, 6000)
  assert.equal(formal[0].status, 'MISSED')
  assert.equal(formal[1].status, 'MISSING')
  assert.ok(result.failures.some(failure => failure.kind === 'unexpected-generator-slot'))
})

// 擷取到正確資料仍不能抵銷同一觀看頁出現別的場域設備。
test('畫面判分拒絕同頁越權與錯誤設備狀態', () => {
  const watcher = { id: 'site', role: 'north', view: 'devices', devices: ['north-002'] }
  const item = plannedSample(1201)
  const rows = new Map([[1201, { ...item, outcome: 'adopted', entryNs: '1000000000' }]])
  const frames = new Map([['site', [
    { type: 'row', parts: ['south-002', '南區場域', '在線 · 20.01302°C'], atNs: '1100000000', visible: true },
    { type: 'row', parts: ['north-002', '北區場域', '離線 · 20.01302°C（資料已非即時）'], atNs: '1100000000', visible: true },
    { type: 'row', parts: ['north-002', '北區場域', '在線 · 20.01302°C'], atNs: '1100000000', visible: true },
  ]]])
  const result = verifyDisplays([watcher], frames, { rows, plan: [item] }, source,
    new Map([['site', { low: 0n, high: 20n }]]))
  assert.equal(result.assessments[0].status, 'PASS')
  assert.deepEqual(result.failures.map(failure => failure.kind), ['unexpected-device-scope', 'unexpected-device-state'])
})

// Vue 的離線附註是獨立節點，正規化後的 DOM 在溫度與附註之間仍保留一個空格。
test('畫面判分辨識實際 DOM 的離線溫度與附註', () => {
  const watcher = { id: 'north', role: 'north', view: 'devices', devices: ['north-001'] }
  const item = plannedSample(7100)
  const rows = new Map([[item.slot, { ...item, outcome: 'adopted', entryNs: '1000000000' }]])
  const frames = new Map([['north', [
    { type: 'row', parts: ['north-001', '北區場域', '在線 · 20.07201°C'], atNs: '1100000000', visible: true },
    { type: 'row', parts: ['north-001', '北區場域', '離線 · 20.07201°C （資料已非即時）'], atNs: '16300000000', visible: true },
  ]]])
  const result = verifyDisplays([watcher], frames, { rows, plan: [item] }, source, browser)
  const offline = result.assessments.find(assessment => assessment.kind === 'offline-device')
  assert.equal(offline.status, 'PASS')
  assert.equal(offline.delayHighNs, '300000020')
  assert.deepEqual(result.failures, [])
})

// 超過 15 秒的條件必須在校時誤差的兩端都成立，溫度的獨特值仍能證明一般回報因果。
test('離線條件不接受跨越或恰好等於期限的時間區間', () => {
  const offline = { ...sample, strictlyAfter: true }
  const crossing = { ...evidence, atNs: '1000000009' }
  const equal = { ...evidence, atNs: '1000000010' }
  const after = { ...evidence, atNs: '1000000011' }
  assert.equal(assessDisplays([offline], [crossing], source, browser)[0].status, 'UNVERIFIED_OFFLINE_DEADLINE')
  assert.equal(assessDisplays([offline], [equal], source, browser)[0].status, 'UNVERIFIED_OFFLINE_DEADLINE')
  assert.equal(assessDisplays([offline], [after], source, browser)[0].status, 'PASS')
  assert.equal(assessDisplays([sample], [crossing], source, browser)[0].status, 'PASS')
})

// 即使 DOM 有正確文字，兩幀中任何一幀超出視口或落在別的視窗之下都無法取證。
test('原生曝光只接受兩幀完整位於獨占區的文字', () => {
  const bounds = { left: 400, top: 300, width: 500, height: 375, innerWidth: 500, innerHeight: 288 }
  const core = { x1: 430, x2: 870, y1: 387, y2: 610 }
  const rect = { x1: 50, x2: 170, y1: 90, y2: 151 }
  const row = { visible: true, bounds, before: rect, after: rect }
  assert.deepEqual(exposedText(row, bounds, core), { x1: 450, x2: 570, y1: 390, y2: 538 })
  assert.equal(exposedText({ ...row, before: { ...rect, x1: 20 } }, bounds, core), null)
  assert.equal(exposedText({ ...row, after: { ...rect, y2: 290 } }, bounds, core), null)
  assert.equal(exposedText({ ...row, bounds: { ...bounds, left: 401 } }, bounds, core), null)
})

// 尚未回覆的 renderer 不得阻止主機結束，更不能晚到後覆寫其他視窗的證據。
test('主機期限和中止會拒絕整批，晚回覆不交付給寫入端', async () => {
  const deferred = Promise.withResolvers()
  const controller = new AbortController()
  const writes = []
  const read = observeBeforeDeadline(() => Promise.all(['ready', deferred.promise]), controller.signal, 15)
    .then(rows => writes.push(...rows))
  await assert.rejects(read, { code: 'BROWSER_OBSERVATION_TIMEOUT' })
  deferred.resolve('late')
  await deferred.promise
  assert.deepEqual(writes, [])
  controller.abort(new Error('中止觀察'))
  await assert.rejects(observeBeforeDeadline(() => assert.fail('中止後不得開始觀察'), controller.signal), /中止觀察/)
})

// 模擬目的檔名被目錄佔用，仍須保留兩個原件且複製其他證據。
test('單筆證據複製失敗會回報原件，其他檔仍可複製', t => {
  const root = mkdtempSync('/tmp/iot08-copy-')
  t.after(() => rmSync(root, { recursive: true, force: true }))
  const files = ['blocked', 'copied'].map(name => ({
    name, source: join(root, name + '.source'), destination: join(root, name + '.target'),
  }))
  for (const file of files) writeFileSync(file.source, file.name)
  mkdirSync(files[0].destination)
  const failures = copyRunEvidence(files)
  assert.deepEqual(failures.map(failure => failure.name), ['blocked'])
  assert.equal(readFileSync(files[0].source, 'utf8'), 'blocked')
  assert.equal(readFileSync(files[1].destination, 'utf8'), 'copied')
})
