export const liveMessages = {
  connecting: '正在連線…',
  syncing: '正在同步…',
  live: '即時更新中',
  disconnected: '即時連線中斷，資料可能過時。',
  stale: '資料待更新。',
}

type LiveState = keyof typeof liveMessages
type LiveOptions = {
  viewer: string
  expiresAt: number
  setState: (state: LiveState) => void
  canRefresh: () => boolean
  refresh: (controller: AbortController) => Promise<boolean>
  checkSession: (controller: AbortController) => Promise<void>
  expire: () => void
}

const retryDelay = 1_500
const heartbeatDeadline = 25_000
const snapshotDeadline = 5_000

// HTTP 標頭與串流共用 UTC 期限格式，瀏覽器用毫秒計時且不進位延長登入。
export function parseSessionExpiry(value: unknown): number {
  if (typeof value !== 'string') return NaN
  const time = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?Z$/.exec(value)
  if (!time) return NaN
  const milliseconds = (time[2] ?? '').padEnd(3, '0').slice(0, 3)
  const expiry = Date.parse(`${time[1]}.${milliseconds}Z`)
  return Number.isFinite(expiry) && new Date(expiry).toISOString().startsWith(time[1]) ? expiry : NaN
}

// 每次登入有自己的串流、期限與查詢排程，避免把舊版本帶進新登入。
export function startLiveMonitor(options: LiveOptions) {
  let source: EventSource | null = null
  let version: bigint | null = null
  let dirty = false
  let stopped = false
  let expiresAt = options.expiresAt
  let expiryTimer: number | undefined
  let heartbeatTimer: number | undefined
  let reconnectTimer: number | undefined
  let refreshTimer: number | undefined
  let snapshotTimer: number | undefined
  let probeTimer: number | undefined
  let running: AbortController | null = null
  let probe: AbortController | null = null

  // 休眠時計時器可能暫停，使用資料或接收事件前仍檢查伺服器的絕對期限。
  function expired(): boolean {
    if (stopped) return true
    if (Date.now() < expiresAt) return false
    options.expire()
    return true
  }

  // 即使 SSE 首訊框尚未到達，仍依登入或身分回應的期限撤下資料。
  function scheduleExpiry(): void {
    window.clearTimeout(expiryTimer)
    expiryTimer = window.setTimeout(() => {
      if (!expired()) scheduleExpiry()
    }, Math.max(0, expiresAt - Date.now()))
  }

  // 切換檢視或連線時，讓舊讀取立即失去套用資格。
  function cancelRefresh(): void {
    window.clearTimeout(refreshTimer)
    window.clearTimeout(snapshotTimer)
    refreshTimer = undefined
    running?.abort()
  }

  // 同一連線版本可合併，多個新事件也只安排一輪。結束後再補讀期間出現的變動。
  function schedule(): void {
    if (
      version === null
      || !dirty
      || running
      || refreshTimer !== undefined
      || !options.canRefresh()
      || expired()
    ) return
    refreshTimer = window.setTimeout(() => {
      refreshTimer = undefined
      void refresh()
    }, 120)
  }

  // 給目前的授權查詢固定時間；新版本只安排補讀，不中斷正在處理的快照。
  async function refresh(): Promise<void> {
    if (
      version === null
      || !options.canRefresh()
      || expired()
    ) return
    dirty = false
    const controller = new AbortController()
    running = controller
    snapshotTimer = window.setTimeout(() => {
      controller.abort()
      if (!expired()) options.setState('stale')
    }, snapshotDeadline)
    try {
      const applied = await options.refresh(controller)
      if (!controller.signal.aborted && !expired()) {
        if (!applied) options.setState('stale')
        else if (!dirty) options.setState('live')
      }
    } finally {
      window.clearTimeout(snapshotTimer)
      running = null
      schedule()
    }
  }

  // 斷線查驗只負責辨識原 Cookie 已失效，收到有效首訊框即可取消。
  function cancelProbe(): void {
    window.clearTimeout(probeTimer)
    probe?.abort()
    probe = null
  }

  // 查驗本身也有限時；它的結果不決定同一 viewer 的新串流能否同步。
  async function checkSession(): Promise<void> {
    cancelProbe()
    const controller = new AbortController()
    probe = controller
    probeTimer = window.setTimeout(() => controller.abort(), retryDelay)
    try {
      await options.checkSession(controller)
    } finally {
      if (probe === controller) cancelProbe()
    }
  }

  // 關閉連線即丟棄版本範圍，讓重啟後較小的版本也能觸發新快照。
  function closeStream(): void {
    const closing = source
    source = null
    closing?.close()
    version = null
    dirty = false
    window.clearTimeout(heartbeatTimer)
    cancelRefresh()
  }

  // 讓握手及已連線的靜默都有上限，心跳不觸發資料讀取。
  function watchHeartbeat(): void {
    window.clearTimeout(heartbeatTimer)
    heartbeatTimer = window.setTimeout(disconnect, heartbeatDeadline)
  }

  // SSE 以自身 Cookie 授權，先比對原登入的 viewer 才能接受版本。
  function acceptVersion(event: MessageEvent<string>): void {
    let frame: { version?: unknown; viewer?: unknown; expiresAt?: unknown } | null
    try {
      frame = JSON.parse(event.data)
    } catch {
      frame = null
    }
    const expiry = parseSessionExpiry(frame?.expiresAt)
    if (frame?.viewer !== options.viewer || !Number.isFinite(expiry)) {
      options.expire()
      return
    }
    if (
      typeof frame.version !== 'string'
      || !/^(0|[1-9][0-9]{0,19})$/.test(frame.version)
    ) {
      disconnect()
      return
    }
    if (expiry < expiresAt) {
      // 授權回應給的期限可以因撤銷而縮短，任何訊框都不能延長它。
      expiresAt = expiry
      scheduleExpiry()
    }
    if (expired()) return
    watchHeartbeat()

    const nextVersion = BigInt(frame.version)
    if (version !== null && nextVersion <= version) return
    if (version === null) {
      cancelProbe()
      options.setState('syncing')
    }
    version = nextVersion
    dirty = true
    schedule()
  }

  // 統一管理網路錯誤、503 及無心跳時的重連，避免原生重試與手動重試重疊。
  function disconnect(): void {
    closeStream()
    if (expired()) return
    options.setState('disconnected')
    reconnectTimer = window.setTimeout(connect, retryDelay)
    void checkSession()
  }

  // 每次重建 EventSource 都先回收前一次的連線及自動重試。
  function connect(): void {
    window.clearTimeout(reconnectTimer)
    closeStream()
    if (stopped || Date.now() >= expiresAt) return
    const stream = new EventSource('/api/events')
    source = stream
    watchHeartbeat()
    stream.addEventListener('version', (event: Event) => {
      if (source === stream && event instanceof MessageEvent) acceptVersion(event)
    })
    stream.addEventListener('heartbeat', () => {
      if (
        source === stream
        && version !== null
        && !expired()
      ) watchHeartbeat()
    })
    stream.onerror = () => {
      if (source === stream) disconnect()
    }
  }

  // 進入另一檢視或開始手動操作時重新指定讀取範圍，等操作完成後補讀。
  function invalidate(syncing = false): void {
    cancelRefresh()
    if (version === null) return
    dirty = true
    if (syncing) options.setState('syncing')
    schedule()
  }

  // 休眠結束後補讀目前畫面，並立即確認絕對期限是否已過。
  function resumeVisible(): void {
    if (document.visibilityState === 'visible' && !expired()) invalidate()
  }

  // 讀取失敗可重試快照，連線失效時則直接重建串流。
  function retry(): void {
    if (expired()) return
    if (version !== null) invalidate(true)
    else connect()
  }

  // 登出及卸載會同步回收串流、計時器與尚未完成的工作。
  function stop(): void {
    stopped = true
    closeStream()
    cancelProbe()
    window.clearTimeout(expiryTimer)
    window.clearTimeout(reconnectTimer)
    document.removeEventListener('visibilitychange', resumeVisible)
  }

  options.setState('connecting')
  scheduleExpiry()
  connect()
  document.addEventListener('visibilitychange', resumeVisible)
  return { expired, invalidate, resume: schedule, retry, stop }
}
