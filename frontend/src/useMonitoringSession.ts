import { onMounted, onUnmounted, ref } from 'vue'
import {
  ApiError,
  apiMessage,
  csrfHeaders,
  isAbort,
  request,
  responseData,
  sessionData,
  type SessionSnapshot,
} from './api'
import type { IdentityDto, LoginRequestDto } from './monitoringDtos'
import { liveMessages, startLiveMonitor } from './live'

type MonitoringWorkspace = {
  clear: () => void
  load: (generation: number) => Promise<void>
  canRefresh: () => boolean
  refresh: (generation: number, controller: AbortController) => Promise<boolean>
}

// 同一登入的 Cookie 比對、期限、串流與請求取消共用一個生命週期。
export function useMonitoringSession(workspace: MonitoringWorkspace) {
  const identity = ref<IdentityDto | null>(null)
  const initialized = ref(false)
  const sessionError = ref('')
  const username = ref('')
  const password = ref('')
  const loginMessage = ref('')
  const loginPending = ref(false)
  const logoutPending = ref(false)
  const logoutUnconfirmed = ref(false)
  const liveState = ref<keyof typeof liveMessages>('connecting')
  let liveMonitor: ReturnType<typeof startLiveMonitor> | null = null
  let identityGeneration = 0
  let activeSession: SessionSnapshot | null = null
  let logoutViewer: string | null = null
  const readControllers = new Set<AbortController>()

  // 讓逾期或舊世代的操作在發送及套用回應前失效。
  function protectedViewer(generation: number): string {
    if (
      !activeSession
      || generation !== identityGeneration
      || liveMonitor?.expired()
    ) throw new DOMException('Superseded request', 'AbortError')
    return activeSession.viewer
  }

  // 受保護寫入要比對原登入，即使瀏覽器已因其他分頁換發 Cookie。
  async function writeApi<T>(path: string, init: RequestInit, generation: number): Promise<T> {
    const result = await responseData<T>(await request(path, init, protectedViewer(generation)))
    protectedViewer(generation)
    return result
  }

  // 查詢需在取得回應及讀取本文後核對世代和取消狀態。
  async function read<T>(
    path: string,
    generation: number,
    controller: AbortController,
    viewer: string | undefined,
    decode: (response: Response) => Promise<T>,
  ): Promise<T> {
    readControllers.add(controller)
    try {
      const response = await request(path, { signal: controller.signal }, viewer)
      if (generation !== identityGeneration || controller.signal.aborted) throw new DOMException('Superseded request', 'AbortError')
      const result = await decode(response)
      if (generation !== identityGeneration || controller.signal.aborted) throw new DOMException('Superseded request', 'AbortError')
      return result
    } finally {
      // 只追蹤仍在處理的查詢，讓後續身分切換能取消它們。
      readControllers.delete(controller)
    }
  }

  // 所有設備、告警和背景查驗都沿用當前頁面原先驗證過的登入。
  async function readApi<T>(path: string, generation: number, controller = new AbortController()): Promise<T> {
    const result = await read(path, generation, controller, protectedViewer(generation), responseData<T>)
    protectedViewer(generation)
    return result
  }

  // 結束目前身分的資料使用期限，讓登入、登出及卸載共用同一個清除入口。
  function clearProtectedState(): number {
    // 先更新世代再取消讀取，讓尚未完成的舊流程失去更新畫面的資格。
    identityGeneration += 1
    activeSession = null
    logoutViewer = null
    liveMonitor?.stop()
    liveMonitor = null
    for (const controller of readControllers) controller.abort()
    readControllers.clear()

    // 清除受保護資料和操作狀態，讓下一個身分從清單起點開始。
    identity.value = null
    workspace.clear()
    loginPending.value = false
    logoutPending.value = false
    logoutUnconfirmed.value = false
    return identityGeneration
  }

  // 只讓目前身分的失效回應清除受保護資料，避免舊請求使新登入登出。
  function expireSession(generation: number): void {
    if (generation !== identityGeneration) return
    clearProtectedState()
    initialized.value = true
    loginMessage.value = '登入狀態已失效，請重新登入。'
  }

  // 告警操作在 Vue 卸載畫面前也能判定原登入已失效。
  function isCurrentIdentity(generation: number): boolean {
    return generation === identityGeneration
  }

  // 串流失敗不公開 HTTP 狀態，只用同一 viewer 的查詢辨識明確的登入失效。
  async function checkLiveSession(generation: number, controller: AbortController): Promise<void> {
    try {
      await readApi<IdentityDto>('/api/session', generation, controller)
    } catch (error) {
      if (
        !controller.signal.aborted
        && error instanceof ApiError
        && error.status === 401
      ) expireSession(generation)
    }
  }

  // 用已驗證的登入先安排絕對期限，再開放畫面與受保護查詢。
  function activateSession(generation: number, session: SessionSnapshot): boolean {
    if (!isCurrentIdentity(generation)) return false
    if (session.expiresAt <= Date.now()) {
      expireSession(generation)
      return false
    }
    activeSession = session
    liveMonitor = startLiveMonitor({
      viewer: session.viewer,
      expiresAt: session.expiresAt,
      setState: state => {
        if (isCurrentIdentity(generation)) liveState.value = state
      },
      canRefresh: () => isCurrentIdentity(generation) && workspace.canRefresh(),
      refresh: controller => workspace.refresh(generation, controller),
      checkSession: controller => checkLiveSession(generation, controller),
      expire: () => expireSession(generation),
    })
    if (liveMonitor.expired()) return false
    identity.value = session.identity
    return true
  }

  // 重新查驗伺服器上的登入狀態，確認身分後才載入受保護清單。
  async function restoreSession(): Promise<void> {
    const generation = clearProtectedState()
    initialized.value = false
    sessionError.value = ''
    loginMessage.value = ''
    try {
      const session = await read('/api/session', generation, new AbortController(), undefined, sessionData)
      if (!activateSession(generation, session)) return
      initialized.value = true
      await workspace.load(generation)
    } catch (error) {
      // 未登入時顯示登入表單，其餘失敗讓使用者重試身分查驗。
      if (generation !== identityGeneration || isAbort(error)) return
      initialized.value = true
      if (error instanceof ApiError && error.status === 401) return
      sessionError.value = apiMessage(error, '無法確認登入狀態，請檢查服務後重試。')
    }
  }

  // 用伺服器確認的身分啟動新畫面，避免沿用先前登入留下的資料。
  async function login(): Promise<void> {
    if (
      loginPending.value
      || logoutPending.value
      || logoutUnconfirmed.value
    ) return
    // 擷取本次送出的憑證後清空密碼欄，並建立新的身分世代。
    const credentials: LoginRequestDto = { username: username.value, password: password.value }
    password.value = ''
    const generation = clearProtectedState()
    initialized.value = true
    sessionError.value = ''
    loginMessage.value = ''
    loginPending.value = true
    try {
      const response = await request('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...csrfHeaders },
        body: JSON.stringify(credentials),
      })
      // 登入結果仍屬於目前世代時，才開放設備清單。
      if (!activateSession(generation, await sessionData(response))) return
      loginPending.value = false
      await workspace.load(generation)
    } catch (error) {
      // 將憑證錯誤和暫時的服務問題分開呈現。
      if (generation !== identityGeneration) return
      if (error instanceof ApiError && error.status === 401) loginMessage.value = '帳號或密碼錯誤。'
      else loginMessage.value = apiMessage(error, '登入失敗，請確認服務狀態後重試。')
    } finally {
      if (generation === identityGeneration) loginPending.value = false
    }
  }

  // 立即清除受保護畫面，再等待伺服器確認登出。
  async function logout(): Promise<void> {
    const viewer = activeSession?.viewer
    if (
      !viewer
      || logoutPending.value
      || logoutUnconfirmed.value
    ) return
    const generation = clearProtectedState()
    logoutViewer = viewer
    initialized.value = true
    logoutPending.value = true
    loginMessage.value = '正在登出…'
    try {
      await responseData<void>(await request('/api/logout', { method: 'POST', headers: csrfHeaders }, viewer))
      if (generation === identityGeneration) {
        logoutViewer = null
        loginMessage.value = '已登出。'
      }
    } catch (error) {
      // 寫入失敗時尚無法確認伺服器的登入狀態，先暫停登入。
      if (generation === identityGeneration) {
        if (error instanceof ApiError && error.status === 401) {
          logoutViewer = null
          loginMessage.value = '已確認登出。'
        } else {
          logoutUnconfirmed.value = true
          loginMessage.value = '登出結果未確認。請先確認狀態，再繼續登入。'
        }
      }
    } finally {
      if (generation === identityGeneration) logoutPending.value = false
    }
  }

  // 先讀回登入狀態，仍有有效登入才重新送出登出。
  async function confirmAndRetryLogout(): Promise<void> {
    const viewer = logoutViewer
    if (
      !viewer
      || !logoutUnconfirmed.value
      || logoutPending.value
      || loginPending.value
    ) return
    const generation = identityGeneration
    logoutPending.value = true
    loginMessage.value = '正在確認登出狀態…'
    try {
      // 不可將原登入不明的登出結果轉成對新 Cookie 的登出請求。
      await read('/api/session', generation, new AbortController(), viewer, responseData<IdentityDto>)
      if (generation !== identityGeneration) return
      loginMessage.value = '登入仍有效，正在重新送出登出…'
      await responseData<void>(await request('/api/logout', { method: 'POST', headers: csrfHeaders }, viewer))
      if (generation === identityGeneration) {
        logoutViewer = null
        logoutUnconfirmed.value = false
        loginMessage.value = '已登出。'
      }
    } catch (error) {
      // 伺服器回覆未登入即可開放重新登入，其餘錯誤保留確認入口。
      if (generation !== identityGeneration) return
      if (error instanceof ApiError && error.status === 401) {
        logoutViewer = null
        logoutUnconfirmed.value = false
        loginMessage.value = '已確認登出。'
      } else {
        logoutUnconfirmed.value = true
        loginMessage.value = '仍無法確認登出狀態，請再次確認。'
      }
    } finally {
      if (generation === identityGeneration) logoutPending.value = false
    }
  }

  // 畫面範圍切換時立即撤銷舊快照，避免回應更新另一個詳情。
  function invalidateLiveTarget(): void {
    liveMonitor?.invalidate(true)
  }

  // 手動讀寫結束後才補讀，仍使用同一個登入的串流排程。
  function updateLiveReadiness(ready: boolean): void {
    if (ready) liveMonitor?.resume()
    else liveMonitor?.invalidate()
  }

  // 重試只要求重新同步，未知的業務寫入不會因此自動重送。
  function retryLive(): void {
    liveMonitor?.retry()
  }

  // 初始化及卸載都沿用同一清除入口，舊請求不能替下一次登入更新狀態。
  onMounted(restoreSession)
  onUnmounted(clearProtectedState)

  // 提供同步的世代快照，告警可在掛載時固定原登入所有權。
  const access = {
    identity,
    currentGeneration: () => identityGeneration,
    readApi,
    writeApi,
    expireSession,
    isCurrentIdentity,
  }
  return {
    identity, initialized, sessionError, username, password, loginMessage,
    loginPending, logoutPending, logoutUnconfirmed, liveState, retryLive, invalidateLiveTarget, updateLiveReadiness,
    restoreSession, login, logout, confirmAndRetryLogout, access,
  }
}

export type MonitoringSessionAccess = ReturnType<typeof useMonitoringSession>['access']
