export const intervalNs = 50_000_000n
export const limitNs = 2_000_000_000n
export const warmupSlots = 1_200
export const formalSlots = 6_000
export const tailSlots = 500
export const formalEnd = warmupSlots + formalSlots
export const totalSlots = formalEnd + tailSlots
export const roster = Array.from({ length: 100 }, (_, index) => {
  const siteId = index < 50 ? 'north' : 'south'
  return { id: `${siteId}-${String(index % 50 + 1).padStart(3, '0')}`, siteId }
})

// 計畫只根據核准的固定槽位與可辨識溫度，與服務實際回報獨立。
export function plannedSample(slot) {
  if (slot < 0 || slot >= totalSlots) return null
  const index = slot % roster.length
  if (
    index === 0
    && slot >= formalEnd
    && slot < 7_600
  ) return null
  const sequence = Math.floor(slot / roster.length) + 1
  const abnormal = index % 50 === 0 && [16, 17, 20].includes(sequence)
  const temperatureCode = (abnormal ? 30 : 20) * 100_000 + sequence * 100 + index + 1
  const phase = slot < warmupSlots ? 'warmup' : slot < formalEnd ? 'formal' : slot === 7_600 ? 'recovery' : 'tail'
  return { slot, phase, deviceId: roster[index].id, sequence, temperatureCode }
}

// 奈秒採十進位整數，避免 JSON 浮點在跨時基相減時丟失精度。
export function nanos(value) {
  if (typeof value !== 'string' || !/^(0|[1-9]\d*)$/.test(value)) throw new Error('無效奈秒時間')
  return BigInt(value)
}

// 每次校時保留來回區間的聯集，漂移或慢探查不會讓延遲被算得更短。
export function clockBounds(samples) {
  if (!samples?.length) throw new Error('缺少校時樣本')
  let low
  let high
  for (const sample of samples) {
    const before = nanos(sample.beforeNs)
    const after = nanos(sample.afterNs)
    const remote = nanos(sample.remoteNs)
    if (after < before) throw new Error('校時時間反向')
    const lower = before - remote
    const upper = after - remote
    if (low === undefined || lower < low) low = lower
    if (high === undefined || upper > high) high = upper
  }
  if (high - low > 1_000_000_000n) throw new Error('校時區間過寬')
  return { low, high }
}

// 同一筆事件只接受一次可見的繪製證據，延遲上界含兩端校時誤差。
export function assessDisplays(expected, observations, sourceClock, browserClocks) {
  const byKey = new Map()
  for (const observation of observations) {
    const values = byKey.get(observation.key) ?? []
    values.push(observation)
    byKey.set(observation.key, values)
  }
  return expected.map(sample => {
    const candidates = byKey.get(sample.key) ?? []
    const base = { ...sample, observations: candidates.length }
    if (candidates.length === 0) return { ...base, status: 'MISSING' }
    if (candidates.length !== 1) return { ...base, status: 'DUPLICATE' }
    const evidence = candidates[0]
    if (evidence.visible !== true) return { ...base, status: 'HIDDEN' }
    try {
      const browser = browserClocks.get(sample.watcher)
      if (!browser || !sourceClock) throw new Error('缺少時基')
      const shown = nanos(evidence.atNs)
      const entry = nanos(sample.entryNs)
      const low = shown + browser.low - entry - sourceClock.high
      const high = shown + browser.high - entry - sourceClock.low
      let status = 'PASS'
      if (high < 0n) status = 'PREMATURE'
      else if (high > limitNs) status = 'LATE'
      // 15 秒整仍未逾時，離線畫面的校正下界也必須落在期限後。
      else if (sample.strictlyAfter && low <= 0n) status = 'UNVERIFIED_OFFLINE_DEADLINE'
      return { ...base, status, atNs: evidence.atNs, delayLowNs: low.toString(), delayHighNs: high.toString() }
    } catch {
      return { ...base, status: 'INVALID_CLOCK' }
    }
  })
}
