import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { test } from 'node:test'
import { setTimeout } from 'node:timers/promises'
import { isDeepStrictEqual } from 'node:util'
import { createStack } from './stack.js'

const windowMs = 60_000

// 每個上限使用自己的資料庫，避免先前的登入消耗來源配額。
async function startStack(t) {
  const stack = await createStack()
  t.after(() => stack.cleanup())
  stack.start()
  return stack
}

async function attemptLogin(stack, username, expectedStatus, phase, headers = {}) {
  const response = await stack.send('/api/login', {
    method: 'POST',
    body: JSON.stringify({ username, password: 'invalid-password-probe' }),
    headers: { Origin: stack.origin, 'X-CSRF-Protection': '1', 'Content-Type': 'application/json', ...headers },
  })
  const { status } = response
  const expected = Array.isArray(expectedStatus) ? expectedStatus : [expectedStatus]
  assert.ok(expected.includes(status), `${phase}，預期 HTTP ${expected.join('/')}，實得 ${status}`)
  const body = await response.json().catch(() => { throw new Error(`${phase}應回傳有效 JSON`) })
  const error = { 401: 'invalid_credentials', 429: 'rate_limited', 503: 'service_unavailable' }[status]
  assert.ok(isDeepStrictEqual(body, { error }), `${phase}錯誤內容不符`)
  if (status === 429) {
    const retry = Number(response.headers.get('Retry-After'))
    assert.ok(Number.isInteger(retry) && retry >= 1 && retry <= 60, `${phase}須提供有效的 Retry-After`)
  }
  return status
}

test('登入限流保留每帳號 5 次配額，並隨各筆嘗試到期逐步恢復', { timeout: 600_000 }, async (t) => {
  const stack = await startStack(t)
  const accountKey = createHash('sha256').update('hq').digest('hex')
  const accountAttempts = () => stack.sql(`
    SELECT count(*) || '/' || count(*) FILTER (WHERE expires_at > statement_timestamp())
    FROM login_attempts WHERE scope = 'account' AND key_hash = decode('${accountKey}', 'hex');
  `)
  const started = performance.now()
  await attemptLogin(stack, 'hq', 401, '重啟前第 1 次')
  const firstCompleted = performance.now()
  // 錯開第 1、2 筆，讓測試能觀察只釋出第 1 筆配額的時間區間。
  await setTimeout(5000, undefined, { signal: t.signal })
  const secondStarted = performance.now()
  const results = await Promise.allSettled(Array.from({ length: 8 }, (_, index) =>
    attemptLogin(stack, 'hq', [401, 429], `重啟前併發請求 ${index + 1}`)))
  const statuses = results.map(result => {
    if (result.status === 'rejected') throw result.reason
    return result.value
  })
  assert.deepEqual(statuses.sort(), [401, 401, 401, 401, 429, 429, 429, 429], '併發請求只能使用剩餘的 4 個配額')

  stack.stop()
  stack.start()
  assert.ok(performance.now() - started < windowMs, '帳號配額須在原視窗內完成重啟')
  await attemptLogin(stack, 'hq', 429, '重啟後原帳號')
  await attemptLogin(stack, 'north', 401, '重啟後另一帳號仍可驗證憑證')
  assert.ok(performance.now() - started < windowMs, '重啟後的帳號檢查須在原視窗內完成')
  assert.equal(accountAttempts(), '5/5', '重啟後須保有原帳號的 5 筆有效紀錄')

  // 首筆到期後先觀察背景清理，保留時間驗證下一批到期前的 HTTP 行為。
  await setTimeout(Math.ceil(firstCompleted + windowMs - performance.now()), undefined, { signal: t.signal })
  let remaining = accountAttempts()
  while (remaining !== '4/4' && performance.now() < secondStarted + windowMs - 1000) {
    await setTimeout(200, undefined, { signal: t.signal })
    remaining = accountAttempts()
  }
  assert.equal(remaining, '4/4', '閒置清理須實際移除首筆，並保留仍有效的 4 筆')
  await attemptLogin(stack, 'hq', 401, '首筆到期後恢復一次登入機會')
  await attemptLogin(stack, 'hq', 429, '次筆到期前其餘配額仍保留')
  assert.ok(performance.now() - secondStarted < windowMs, '恢復檢查須在第二筆到期前完成')
})

test('登入限流保留每直連來源 30 次配額，並忽略偽造轉送標頭', { timeout: 600_000 }, async (t) => {
  const stack = await startStack(t)
  // 在隔離資料庫中只阻止帳號寫入，確認它失敗時較早寫入的來源配額一併回滾。
  stack.sql("ALTER TABLE login_attempts ADD CONSTRAINT test_reject_account CHECK (scope <> 'account') NOT VALID;")
  await attemptLogin(stack, 'hq', 503, '帳號配額寫入失敗')
  assert.equal(stack.sql("SELECT count(*) FROM login_attempts WHERE scope = 'source';"), '0', '來源配額不得部分提交')
  stack.sql('ALTER TABLE login_attempts DROP CONSTRAINT test_reject_account;')
  const started = performance.now()
  for (let attempt = 1; attempt <= 31; attempt++) {
    const username = attempt <= 6 ? 'hq' : `rate-probe-${attempt}`
    // 第 6 次被帳號限制擋下，仍消耗同一來源的一次機會。
    const status = attempt === 6 || attempt === 31 ? 429 : 401
    await attemptLogin(stack, username, status, `重啟前來源第 ${attempt} 次`, {
      'X-Forwarded-For': `198.51.100.${attempt}`, 'X-Real-IP': `203.0.113.${attempt}`,
    })
  }

  stack.stop()
  stack.start()
  assert.ok(performance.now() - started < windowMs, '來源配額須在原視窗內完成重啟')
  await attemptLogin(stack, 'rate-probe-new', 429, '重啟後來源仍限制未使用過的帳號', {
    'X-Forwarded-For': '198.51.100.32', 'X-Real-IP': '203.0.113.32',
  })
  assert.ok(performance.now() - started < windowMs, '重啟後的來源檢查須在原視窗內完成')
})
