import { copyFileSync, lstatSync } from 'node:fs'

// 主機期限不依賴 renderer；逾時或中止後，晚到的觀察結果不能交給呼叫端寫入。
export async function observeBeforeDeadline(operation, signal, timeoutMs = 5_000) {
  signal.throwIfAborted()
  let timer
  let abort
  const interrupted = new Promise((_, reject) => {
    abort = () => reject(signal.reason)
    signal.addEventListener('abort', abort, { once: true })
    timer = setTimeout(() => {
      const error = new Error('瀏覽器觀察超過主機期限')
      error.code = 'BROWSER_OBSERVATION_TIMEOUT'
      reject(error)
    }, timeoutMs)
  })
  try {
    const value = await Promise.race([Promise.resolve().then(operation), interrupted])
    signal.throwIfAborted()
    return value
  } finally {
    clearTimeout(timer)
    signal.removeEventListener('abort', abort)
  }
}

// 以內外視窗大小差包住原生裝飾的偏移，只有兩幀的完整文字都在獨占區才算畫面證據。
export function exposedText(row, bounds, core) {
  if (
    row.visible !== true
    || !core
    || JSON.stringify(row.bounds) !== JSON.stringify(bounds)
  ) return null
  const inViewport = rect => rect
    && rect.x1 >= 0
    && rect.y1 >= 0
    && rect.x2 > rect.x1
    && rect.y2 > rect.y1
    && rect.x2 <= bounds.innerWidth
    && rect.y2 <= bounds.innerHeight
  if (!inViewport(row.before) || !inViewport(row.after)) return null
  const rect = {
    x1: bounds.left + Math.min(row.before.x1, row.after.x1),
    x2: bounds.left + bounds.width - bounds.innerWidth + Math.max(row.before.x2, row.after.x2),
    y1: bounds.top + Math.min(row.before.y1, row.after.y1),
    y2: bounds.top + bounds.height - bounds.innerHeight + Math.max(row.before.y2, row.after.y2),
  }
  return rect.x1 >= core.x1
    && rect.x2 <= core.x2
    && rect.y1 >= core.y1
    && rect.y2 <= core.y2 ? rect : null
}

// 單一檔案複製失敗時繼續保全其餘檔案，並交由呼叫端保留整個私有原件目錄。
export function copyRunEvidence(files) {
  const failures = []
  for (const { name, source, destination } of files) {
    let metadata
    try {
      metadata = lstatSync(source)
    } catch (error) {
      if (error.code !== 'ENOENT') failures.push({ name, code: error.code ?? 'stat failed' })
      continue
    }
    if (!metadata.isFile()) {
      failures.push({ name, code: 'not a regular file' })
      continue
    }
    try {
      copyFileSync(source, destination)
    } catch (error) {
      failures.push({ name, code: error.code ?? 'copy failed' })
    }
  }
  return failures
}
