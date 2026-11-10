<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import AlertHistory from './AlertHistory.vue'

type Identity = {
  username: string
  role: 'headquarters' | 'site'
  siteId: string | null
}

type Device = {
  id: string
  name: string
  siteId: string
  siteName: string
  onlineStatus: 'unreported' | 'awaiting' | 'online' | 'offline'
  latestMeasurement?: { temperature: number; receivedAt: string }
}

type Simulation = {
  mode: 'stopped' | 'normal' | 'abnormal'
  version: string
  canReplay: boolean
}

type DevicePage = {
  items: Device[]
  total: number
  limit: number
  offset: number
}

class ApiError extends Error {
  // 保留 HTTP 狀態和 API 錯誤代碼，讓各操作能分辨登入失效、拒絕和暫時故障。
  constructor(readonly status: number, readonly code: string) {
    super(code)
    this.name = 'ApiError'
  }
}

const pageSize = 50
const csrfHeaders = { 'X-CSRF-Protection': '1' }
const onlineStatuses = { unreported: '尚未回報', awaiting: '等待首筆回報', online: '在線', offline: '離線' }
// 回報時間固定以臺灣時區顯示，避免受瀏覽器所在時區影響。
const receiptTime = new Intl.DateTimeFormat('zh-TW', {
  timeZone: 'Asia/Taipei',
  dateStyle: 'short',
  timeStyle: 'medium',
  hourCycle: 'h23',
})
const simulationModes = { stopped: '已停止', normal: '正常回報', abnormal: '異常回報' }
const simulationActions = {
  normal: { label: '開始正常回報', success: '已開始正常回報。' },
  abnormal: { label: '開始異常回報', success: '已開始異常回報。' },
  stop: { label: '停止回報', success: '已停止模擬回報。' },
  replay: { label: '重複回報', success: '已重送最近一筆原始測量。' },
}
const noReplayMessage = '尚無可重送的測量。'
const identity = ref<Identity | null>(null)
const initialized = ref(false)
const sessionError = ref('')
const username = ref('')
const password = ref('')
const loginMessage = ref('')
const loginPending = ref(false)
const logoutPending = ref(false)
const logoutUnconfirmed = ref(false)
const view = ref<'list' | 'detail' | 'alerts'>('list')
const alertPending = ref(false)
const devices = ref<Device[]>([])
const total = ref(0)
const offset = ref(0)
const devicesLoading = ref(false)
const devicesError = ref('')
const selectedDeviceId = ref<string | null>(null)
const selectedDevice = ref<Device | null>(null)
const detailLoading = ref(false)
const detailError = ref('')
const detailCanRetry = ref(false)
const simulation = ref<Simulation | null>(null)
const simulationLoading = ref(false)
const simulationPending = ref(false)
const simulationReadError = ref('')
const simulationActionError = ref('')
const simulationFeedback = ref('')

let identityGeneration = 0
let detailController: AbortController | null = null
const readControllers = new Set<AbortController>()

// 由後端回傳的筆數與偏移量推導分頁資訊，空清單也保有第一頁。
const pageCount = computed(() => Math.max(1, Math.ceil(total.value / pageSize)))
const currentPage = computed(() => Math.floor(offset.value / pageSize) + 1)
const deviceRange = computed(() => total.value === 0
  ? '共 0 台設備'
  : `${offset.value + 1}–${Math.min(offset.value + devices.value.length, total.value)} / ${total.value} 台設備`)

// 設備或模擬狀態尚在讀取，或指令仍在處理時，暫停新的模擬操作。
const simulationBusy = computed(() =>
  detailLoading.value
  || simulationLoading.value
  || simulationPending.value)

// 只採用字串型態的 API 錯誤代碼，其他回應交由呼叫端使用備用訊息。
function errorCode(value: unknown): string {
  if (
    typeof value === 'object'
    && value !== null
    && 'error' in value
    && typeof value.error === 'string'
  ) {
    return value.error
  }
  return ''
}

// 依 HTTP 狀態區分成功與錯誤，讓呼叫端能據此選擇復原方式。
async function responseData<T>(response: Response): Promise<T> {
  if (!response.ok) {
    // 錯誤回應即使沒有可解析的 JSON，也保留原本的 HTTP 狀態。
    const body: unknown = await response.json().catch(() => null)
    throw new ApiError(response.status, errorCode(body))
  }
  // 例如登出成功時沒有回應本文，直接完成即可。
  if (response.status === 204) return undefined as T
  return await response.json() as T
}

// 以同源 Cookie 送出寫入，並讓呼叫端依回應判定結果。
async function writeApi<T>(path: string, init: RequestInit): Promise<T> {
  const response = await fetch(path, { ...init, credentials: 'same-origin' })
  return responseData<T>(response)
}

// 將查詢綁定到發出時的身分世代，讓切換身分時可取消並捨棄舊查詢。
async function readApi<T>(path: string, generation: number, controller = new AbortController()): Promise<T> {
  readControllers.add(controller)
  try {
    // 收到回應及解析成功後都確認世代，因為讀取本文期間也可能切換身分。
    const response = await fetch(path, { credentials: 'same-origin', signal: controller.signal })
    if (generation !== identityGeneration) throw new DOMException('Superseded request', 'AbortError')
    const result = await responseData<T>(response)
    if (generation !== identityGeneration) throw new DOMException('Superseded request', 'AbortError')
    return result
  } finally {
    // 只追蹤仍在處理的查詢，讓後續身分切換能取消它們。
    readControllers.delete(controller)
  }
}

// 結束目前身分的資料使用期限，讓登入、登出及卸載共用同一個清除入口。
function clearProtectedState(): number {
  // 先更新世代再取消讀取，讓尚未完成的舊流程失去更新畫面的資格。
  identityGeneration += 1
  for (const controller of readControllers) controller.abort()
  readControllers.clear()
  detailController = null

  // 清除受保護資料和操作狀態，讓下一個身分從清單起點開始。
  identity.value = null
  devices.value = []
  total.value = 0
  offset.value = 0
  selectedDeviceId.value = null
  selectedDevice.value = null
  view.value = 'list'
  devicesLoading.value = false
  devicesError.value = ''
  detailLoading.value = false
  detailError.value = ''
  detailCanRetry.value = false
  clearSimulation()
  alertPending.value = false
  loginPending.value = false
  logoutPending.value = false
  logoutUnconfirmed.value = false
  return identityGeneration
}

// 將已取消或已被取代的查詢視為流程結束，避免畫面顯示載入失敗。
function isAbort(error: unknown): boolean {
  return error instanceof Error && error.name === 'AbortError'
}

// 將可辨識的 API 錯誤轉為操作提示，其餘保留呼叫端提供的情境訊息。
function apiMessage(error: unknown, fallback: string, notFound = '找不到這台設備，或您沒有權限查看。'): string {
  if (!(error instanceof ApiError)) return fallback
  if (error.status === 403 || error.code === 'forbidden') return '此操作未獲允許。'
  if (error.status === 404 || error.code === 'not_found') return notFound
  if (error.status === 429 || error.code === 'rate_limited') return '登入嘗試次數已達限制，請稍後再試。'
  if (error.status === 503 || error.code === 'service_unavailable') return '服務暫時無法使用，請稍後重試。'
  return fallback
}

// 只讓目前身分的失效回應清除受保護資料，避免舊請求使新登入登出。
function expireSession(generation: number): void {
  if (generation !== identityGeneration) return
  clearProtectedState()
  initialized.value = true
  loginMessage.value = '登入狀態已失效，請重新登入。'
}

// 讓告警畫面沿用目前登入世代與 API 契約，回應晚到時仍可核對發出請求的身分。
const alertApi = { readApi, writeApi, apiMessage, expireSession, ApiError, csrfHeaders, isCurrentIdentity }

// 告警操作在 Vue 卸載畫面前也能判定原登入已失效。
function isCurrentIdentity(generation: number): boolean {
  return generation === identityGeneration
}

// 讀取目前身分可見的設備頁面，並以伺服器回傳的分頁資料更新清單。
async function loadDevices(nextOffset: number, generation = identityGeneration): Promise<void> {
  if (!identity.value || generation !== identityGeneration) return
  devicesLoading.value = true
  devicesError.value = ''
  try {
    const page = await readApi<DevicePage>(`/api/devices?limit=${pageSize}&offset=${nextOffset}`, generation)
    if (generation !== identityGeneration) return
    devices.value = page.items
    total.value = page.total
    offset.value = page.offset
  } catch (error) {
    // 只有目前身分的查詢可清除登入狀態或顯示查詢錯誤。
    if (generation !== identityGeneration || isAbort(error)) return
    if (error instanceof ApiError && error.status === 401) expireSession(generation)
    else devicesError.value = apiMessage(error, '設備清單載入失敗，請重試。')
  } finally {
    // 舊查詢結束時，保留新身分正在載入的狀態。
    if (generation === identityGeneration) devicesLoading.value = false
  }
}

// 重新查驗伺服器上的登入狀態，確認身分後才載入受保護清單。
async function restoreSession(): Promise<void> {
  const generation = clearProtectedState()
  initialized.value = false
  sessionError.value = ''
  loginMessage.value = ''
  try {
    const currentIdentity = await readApi<Identity>('/api/session', generation)
    if (generation !== identityGeneration) return
    identity.value = currentIdentity
    initialized.value = true
    await loadDevices(0, generation)
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
  const credentials = { username: username.value, password: password.value }
  password.value = ''
  const generation = clearProtectedState()
  initialized.value = true
  sessionError.value = ''
  loginMessage.value = ''
  loginPending.value = true
  try {
    const signedInIdentity = await writeApi<Identity>('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...csrfHeaders },
      body: JSON.stringify(credentials),
    })
    // 登入結果仍屬於目前世代時，才開放設備清單。
    if (generation !== identityGeneration) return
    identity.value = signedInIdentity
    loginPending.value = false
    await loadDevices(0, generation)
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
  if (
    !identity.value
    || logoutPending.value
    || logoutUnconfirmed.value
  ) return
  const generation = clearProtectedState()
  initialized.value = true
  logoutPending.value = true
  loginMessage.value = '正在登出…'
  try {
    await writeApi<void>('/api/logout', { method: 'POST', headers: csrfHeaders })
    if (generation === identityGeneration) loginMessage.value = '已登出。'
  } catch {
    // 寫入失敗時尚無法確認伺服器的登入狀態，先暫停登入。
    if (generation === identityGeneration) {
      logoutUnconfirmed.value = true
      loginMessage.value = '登出結果未確認。請先確認狀態，再繼續登入。'
    }
  } finally {
    if (generation === identityGeneration) logoutPending.value = false
  }
}

// 先讀回登入狀態，仍有有效登入才重新送出登出。
async function confirmAndRetryLogout(): Promise<void> {
  if (
    !logoutUnconfirmed.value
    || logoutPending.value
    || loginPending.value
  ) return
  const generation = identityGeneration
  logoutPending.value = true
  loginMessage.value = '正在確認登出狀態…'
  try {
    await readApi<Identity>('/api/session', generation)
    if (generation !== identityGeneration) return
    loginMessage.value = '登入仍有效，正在重新送出登出…'
    await writeApi<void>('/api/logout', { method: 'POST', headers: csrfHeaders })
    if (generation === identityGeneration) {
      logoutUnconfirmed.value = false
      loginMessage.value = '已登出。'
    }
  } catch (error) {
    // 伺服器回覆未登入即可開放重新登入，其餘錯誤保留確認入口。
    if (generation !== identityGeneration) return
    if (error instanceof ApiError && error.status === 401) {
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

// 更新目前詳情的設備資料，只接受仍屬於這次詳情的結果。
async function refreshDevice(): Promise<void> {
  const id = selectedDeviceId.value
  const controller = detailController
  const generation = identityGeneration
  if (
    !id
    || !controller
    || detailLoading.value
  ) return
  detailError.value = ''
  detailCanRetry.value = false
  detailLoading.value = true
  try {
    const device = await readApi<Device>(`/api/devices/${encodeURIComponent(id)}`, generation, controller)
    if (detailController === controller) selectedDevice.value = device
  } catch (error) {
    // 詳情已切換或請求取消時，略過這次查詢的錯誤。
    if (detailController !== controller || isAbort(error)) return
    if (error instanceof ApiError && error.status === 401) expireSession(generation)
    else {
      // 權限遭拒或設備已不存在時，清除先前顯示的設備資料。
      if (
        error instanceof ApiError
        && (
          error.status === 403
          || error.status === 404
        )
      ) selectedDevice.value = null
      detailError.value = apiMessage(error, '設備資料載入失敗，請重試。')
      detailCanRetry.value = !(error instanceof ApiError && error.status === 404)
    }
  } finally {
    if (detailController === controller) detailLoading.value = false
  }
}

// 結束目前設備的模擬畫面狀態，避免版本、忙碌旗標與訊息跟到下一台設備。
function clearSimulation(): void {
  simulation.value = null
  simulationLoading.value = false
  simulationPending.value = false
  simulationReadError.value = ''
  simulationActionError.value = ''
  simulationFeedback.value = ''
}

// 讓模擬查詢與寫入使用同一個路徑，設備編號固定編碼為一個路徑片段。
function simulationPath(id: string): string {
  return `/api/devices/${encodeURIComponent(id)}/simulation`
}

// 讀取總部畫面所需的模式和控制版本，供下一次手動操作使用。
async function loadSimulation(): Promise<void> {
  const device = selectedDevice.value
  const controller = detailController
  const generation = identityGeneration
  if (
    !device
    || !controller
    || identity.value?.role !== 'headquarters'
    || simulationLoading.value
  ) return
  simulationReadError.value = ''
  simulationLoading.value = true
  try {
    const state = await readApi<Simulation>(simulationPath(device.id), generation, controller)
    if (detailController === controller) simulation.value = state
  } catch (error) {
    // 保留目前詳情的查詢錯誤供重試，登入失效則交回共用清除流程。
    if (detailController !== controller || isAbort(error)) return
    if (error instanceof ApiError && error.status === 401) expireSession(generation)
    else simulationReadError.value = apiMessage(error, '模擬狀態載入失敗，請重試。')
  } finally {
    if (detailController === controller) simulationLoading.value = false
  }
}

// 以畫面目前的控制版本送出模擬指令，結果只更新原本的設備詳情。
async function changeSimulation(action: keyof typeof simulationActions): Promise<void> {
  const device = selectedDevice.value
  const state = simulation.value
  const controller = detailController
  const generation = identityGeneration
  if (
    !device
    || !state
    || !controller
    || identity.value?.role !== 'headquarters'
    || simulationBusy.value
    || (
      action === 'replay'
      && !state.canReplay
    )
  ) return

  // 開始新的手動操作時，先清除上一筆結果與錯誤提示。
  simulationPending.value = true
  simulationActionError.value = ''
  simulationFeedback.value = ''
  try {
    try {
      const updated = await writeApi<Simulation>(simulationPath(device.id), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...csrfHeaders },
        body: JSON.stringify({ action, version: state.version }),
      })
      if (detailController !== controller) return
      simulation.value = updated
      simulationFeedback.value = simulationActions[action].success
    } catch (error) {
      // 登入失效屬於整個身分，即使已切換詳情也交由原身分世代判斷。
      if (error instanceof ApiError && error.status === 401) {
        expireSession(generation)
        return
      }
      // 其他錯誤只影響原詳情，先丟棄舊版本以免繼續操作。
      if (detailController !== controller) return
      simulation.value = null
      if (
        error instanceof ApiError
        && (
          error.status === 403
          || error.status === 404
        )
      ) {
        simulationReadError.value = apiMessage(error, '無法操作這台設備。')
        return
      }
      if (error instanceof ApiError && error.code === 'conflict') {
        simulationActionError.value = '模擬狀態已變更，請確認目前模式後再操作。'
      } else if (error instanceof ApiError && error.code === 'no_measurement') {
        simulationActionError.value = noReplayMessage
      } else if (error instanceof ApiError && error.status === 400) {
        simulationActionError.value = '操作內容無效，請確認目前模擬狀態後再操作。'
      } else {
        simulationActionError.value = '未能確認操作結果。請確認目前模擬狀態後再操作。'
      }
      // 讀回目前版本供下一次手動操作，避免自動重送已執行或已過時的指令。
      await loadSimulation()
    }
    // 模擬模式回應不含最新溫度與在線狀態，仍在原詳情時再查設備。
    if (detailController === controller) await refreshDevice()
  } finally {
    // 舊操作結束時，保留新詳情自己的忙碌狀態。
    if (detailController === controller) simulationPending.value = false
  }
}

// 為每次開啟或重試詳情建立新的查詢範圍，讓前一次的回應失效。
async function loadDevice(id: string): Promise<void> {
  detailController?.abort()
  const controller = new AbortController()
  detailController = controller
  selectedDeviceId.value = id
  selectedDevice.value = null
  clearSimulation()
  detailLoading.value = false

  // 先確認設備仍可讀取，再查同一個詳情的模擬狀態。
  await refreshDevice()
  if (detailController === controller) await loadSimulation()
}

// 返回時取消詳情查詢並清除詳情狀態，讓清單保留原本頁碼。
function returnToList(): void {
  detailController?.abort()
  detailController = null
  selectedDeviceId.value = null
  selectedDevice.value = null
  detailLoading.value = false
  detailError.value = ''
  detailCanRetry.value = false
  clearSimulation()
  view.value = 'list'
}

// 切到詳情後立即載入指定設備，讓等待與錯誤都顯示在詳情畫面。
function openDevice(id: string): void {
  view.value = 'detail'
  void loadDevice(id)
}

// 切換功能時清除設備詳情，並保留原本的設備清單頁碼。
function navigateTo(section: 'list' | 'alerts'): void {
  if (
    !identity.value
    || alertPending.value
    || view.value === section
  ) return
  if (view.value === 'detail') returnToList()
  view.value = section
}

// 掛載時向伺服器查驗身分，卸載時取消查詢並清除受保護資料。
onMounted(restoreSession)
onUnmounted(clearProtectedState)
</script>

<template>
  <main class="page" aria-labelledby="page-title">
    <header class="page-header">
      <div>
        <p class="eyebrow">IoT 設備監控</p>
        <h1 id="page-title">{{ view === 'alerts'
          ? '告警'
          : view === 'detail'
            ? '設備資料'
            : '設備清單' }}</h1>
      </div>
      <div v-if="identity" class="account">
        <span>{{ identity.username }}（{{ identity.role === 'headquarters' ? '總部' : '場域' }}）</span>
        <button type="button" class="button button-secondary" :disabled="logoutPending" @click="logout">
          {{ logoutPending ? '正在登出…' : '登出' }}
        </button>
      </div>
    </header>

    <nav v-if="identity" class="section-nav" aria-label="監控功能">
      <button type="button" class="button button-secondary" :aria-current="view !== 'alerts' ? 'page' : undefined" :disabled="alertPending" @click="navigateTo('list')">設備清單</button>
      <button type="button" class="button button-secondary" :aria-current="view === 'alerts' ? 'page' : undefined" :disabled="alertPending" @click="navigateTo('alerts')">告警歷程</button>
    </nav>

    <section v-if="!initialized" class="panel" aria-live="polite">
      <p>正在確認登入狀態…</p>
    </section>

    <section v-else-if="sessionError && !identity" class="panel" aria-labelledby="session-error-title">
      <h2 id="session-error-title">目前無法載入</h2>
      <p role="alert">{{ sessionError }}</p>
      <button type="button" class="button" @click="restoreSession">重新嘗試</button>
    </section>

    <section v-else-if="!identity" class="panel login-panel" aria-labelledby="login-title">
      <h2 id="login-title">登入</h2>
      <form :aria-busy="loginPending" @submit.prevent="login">
        <label for="username">帳號</label>
        <input
          id="username"
          v-model="username"
          name="username"
          autocomplete="username"
          required
          :disabled="
            loginPending
            || logoutPending
            || logoutUnconfirmed
          "
        />
        <label for="password">密碼</label>
        <input
          id="password"
          v-model="password"
          name="password"
          type="password"
          autocomplete="current-password"
          required
          :disabled="
            loginPending
            || logoutPending
            || logoutUnconfirmed
          "
        />
        <button
          type="submit"
          class="button"
          :disabled="
            loginPending
            || logoutPending
            || logoutUnconfirmed
          "
        >
          {{ loginPending ? '登入中…' : '登入' }}
        </button>
      </form>
      <p v-if="loginMessage" class="notice" role="alert">{{ loginMessage }}</p>
      <button v-if="logoutUnconfirmed" type="button" class="button button-secondary" :disabled="logoutPending" @click="confirmAndRetryLogout">
        {{ logoutPending ? '正在確認登出…' : '確認並重試登出' }}
      </button>
      <p v-if="logoutUnconfirmed" class="muted">系統會先確認登入狀態；若仍有效，會重新送出登出。</p>
    </section>

    <section v-else-if="view === 'list'" class="panel" aria-labelledby="device-list-title">
      <div class="section-heading">
        <div>
          <h2 id="device-list-title">設備清單</h2>
          <p class="muted">{{ deviceRange }}</p>
        </div>
        <button type="button" class="button button-secondary" :disabled="devicesLoading" @click="loadDevices(offset)">
          {{ devicesLoading ? '載入中…' : '重新載入' }}
        </button>
      </div>

      <p v-if="devicesLoading && devices.length === 0" aria-live="polite">正在載入設備…</p>
      <p v-else-if="devicesError" class="notice" role="alert">{{ devicesError }}</p>
      <p v-else-if="devices.length === 0" class="muted">目前沒有可查看的設備。</p>
      <ul v-else class="device-list">
        <li v-for="device in devices" :key="device.id">
          <button type="button" class="device-button" @click="openDevice(device.id)">
            <span>{{ device.name }}</span>
            <span class="device-site">{{ device.siteName }}</span>
          </button>
        </li>
      </ul>

      <nav v-if="total > pageSize" class="pagination" aria-label="設備清單分頁">
        <button type="button" class="button button-secondary" :disabled="devicesLoading || offset === 0" @click="loadDevices(Math.max(0, offset - pageSize))">上一頁</button>
        <span aria-live="polite">第 {{ currentPage }} / {{ pageCount }} 頁</span>
        <button type="button" class="button button-secondary" :disabled="devicesLoading || offset + devices.length >= total" @click="loadDevices(offset + pageSize)">下一頁</button>
      </nav>
    </section>

    <AlertHistory
      v-else-if="view === 'alerts'"
      :api="alertApi"
      :generation="identityGeneration"
      :receipt-time="receiptTime"
      :page-size="pageSize"
      @pending="alertPending = $event"
    />

    <section v-else class="panel" aria-labelledby="device-detail-title">
      <button type="button" class="back-button" @click="returnToList">← 返回設備清單</button>
      <h2 id="device-detail-title">{{ selectedDevice?.name ?? '設備詳細資料' }}</h2>
      <p v-if="detailLoading" aria-live="polite">正在載入設備資料…</p>
      <p v-if="detailError" class="notice" role="alert">{{ detailError }}</p>
      <dl v-if="selectedDevice" class="device-details">
        <div><dt>設備編號</dt><dd>{{ selectedDevice.id }}</dd></div>
        <div><dt>場域</dt><dd>{{ selectedDevice.siteName }}</dd></div>
        <div><dt>在線狀態</dt><dd aria-live="polite">{{ onlineStatuses[selectedDevice.onlineStatus] }}</dd></div>
        <div>
          <dt>最新有效溫度</dt>
          <dd>
            <span>{{ selectedDevice.latestMeasurement ? `${selectedDevice.latestMeasurement.temperature}°C` : '—' }}</span>
            <span v-if="selectedDevice.onlineStatus === 'offline' && selectedDevice.latestMeasurement" class="muted">資料已非即時</span>
          </dd>
        </div>
        <div>
          <dt>最後有效回報時間</dt>
          <dd>
            <time v-if="selectedDevice.latestMeasurement" :datetime="selectedDevice.latestMeasurement.receivedAt">{{ receiptTime.format(new Date(selectedDevice.latestMeasurement.receivedAt)) }}</time>
            <template v-else>—</template>
          </dd>
        </div>
      </dl>
      <button
        v-if="
          selectedDeviceId
          && (
            selectedDevice
            || (
              detailError
              && detailCanRetry
            )
          )
        "
        type="button"
        class="button button-secondary"
        :disabled="simulationBusy"
        @click="selectedDevice ? refreshDevice() : loadDevice(selectedDeviceId)"
      >重新載入設備狀態</button>

      <section v-if="selectedDevice && identity.role === 'headquarters'" class="simulation-controls" aria-labelledby="simulation-title">
        <h3 id="simulation-title">模擬控制</h3>
        <p v-if="simulation" role="status">模擬模式：{{ simulationModes[simulation.mode] }}</p>
        <p v-if="simulationLoading" role="status">正在載入模擬狀態…</p>
        <p v-else-if="simulationPending" role="status">正在處理模擬操作…</p>
        <p v-if="simulationActionError" class="notice" role="alert">{{ simulationActionError }}</p>
        <p v-if="simulationReadError" class="notice" role="alert">{{ simulationReadError }}</p>
        <p v-if="simulationFeedback" role="status">{{ simulationFeedback }}</p>
        <div v-if="simulation" class="simulation-actions">
          <button
            v-for="(option, action) in simulationActions"
            :key="action"
            type="button"
            class="button button-secondary"
            :disabled="
              simulationBusy
              || (
                action === 'replay'
                && !simulation.canReplay
              )
            "
            @click="changeSimulation(action)"
          >{{ option.label }}</button>
        </div>
        <p
          v-if="
            simulation
            && !simulation.canReplay
            && simulationActionError !== noReplayMessage
          "
          class="muted"
        >{{ noReplayMessage }}</p>
        <button v-if="simulationReadError" type="button" class="button button-secondary" :disabled="simulationBusy" @click="loadSimulation">重新載入模擬狀態</button>
      </section>
    </section>
  </main>
</template>

<style>
:root {
  color: #1d2924;
  background: #f5f7f4;
  font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  font-synthesis: none;
  text-rendering: optimizeLegibility;
}

* { box-sizing: border-box; }
body { margin: 0; min-width: 320px; }
button, input { font: inherit; }
button { cursor: pointer; }
button:disabled { cursor: not-allowed; opacity: 0.58; }
button:focus-visible, input:focus-visible { outline: 3px solid #45876b; outline-offset: 2px; }

.page { width: min(100% - 2rem, 54rem); margin: 0 auto; padding: 3rem 0; }
.page-header, .section-heading, .account, .pagination { display: flex; align-items: center; justify-content: space-between; gap: 1rem; }
.page-header { margin-bottom: 1.5rem; }
.eyebrow { margin: 0 0 0.35rem; color: #52715f; font-size: 0.85rem; font-weight: 700; letter-spacing: 0.04em; }
h1, h2, p { margin-top: 0; }
h1 { margin-bottom: 0; font-size: clamp(1.65rem, 4vw, 2rem); }
h2 { margin-bottom: 0.75rem; font-size: 1.25rem; }
.account { color: #526258; font-size: 0.95rem; }
.panel { padding: clamp(1.25rem, 4vw, 2rem); border: 1px solid #dce5dd; border-radius: 1rem; background: #fff; box-shadow: 0 0.5rem 1.5rem rgb(27 55 38 / 5%); }
.login-panel { max-width: 30rem; }
form { display: grid; gap: 0.65rem; }
label { margin-top: 0.35rem; font-weight: 650; }
input { width: 100%; min-height: 2.8rem; padding: 0.6rem 0.75rem; border: 1px solid #aab8ad; border-radius: 0.5rem; background: #fff; color: inherit; }
.button { min-height: 2.65rem; padding: 0.55rem 1rem; border: 1px solid #246246; border-radius: 0.55rem; background: #246246; color: #fff; font-weight: 650; }
.button:hover:not(:disabled) { background: #194d35; }
.button-secondary { border-color: #a7b9ab; background: #fff; color: #244d36; }
.button-secondary:hover:not(:disabled) { background: #f0f5f1; }
.login-panel .button { margin-top: 0.6rem; }
.section-nav { display: flex; flex-wrap: wrap; gap: 0.65rem; margin-bottom: 1rem; }
.section-nav [aria-current] { border-color: #246246; background: #edf5ef; }
.notice { color: #8a321e; }
.muted, .device-site { color: #607066; }
.section-heading { margin-bottom: 1.25rem; }
.section-heading h2, .section-heading p { margin-bottom: 0.25rem; }
.device-list { display: grid; gap: 0.65rem; margin: 0; padding: 0; list-style: none; }
.device-button { display: flex; width: 100%; align-items: center; justify-content: space-between; gap: 1rem; padding: 1rem; border: 1px solid #dce5dd; border-radius: 0.65rem; background: #fbfcfb; color: #1d2924; text-align: left; }
.device-button:hover { border-color: #78a189; background: #f3f8f4; }
.device-button span:first-child { font-weight: 650; }
.pagination { justify-content: center; margin-top: 1.5rem; }
.pagination span { min-width: 7rem; color: #526258; text-align: center; }
.back-button { margin-bottom: 1.25rem; padding: 0; border: 0; background: transparent; color: #246246; font-weight: 650; }
.back-button:hover { text-decoration: underline; }
.device-details { margin-bottom: 0; }
.device-details div { display: grid; grid-template-columns: minmax(7rem, 0.35fr) 1fr; gap: 1rem; padding: 0.9rem 0; border-top: 1px solid #e7ede8; }
.device-details dt { color: #607066; }
.device-details dd { margin: 0; overflow-wrap: anywhere; }
.simulation-controls { margin-top: 1rem; padding-top: 1rem; border-top: 1px solid #e7ede8; }
.simulation-controls h3 { margin-top: 0; }
.simulation-actions { display: flex; flex-wrap: wrap; gap: 0.65rem; }
.simulation-actions + p { margin: 0.65rem 0 0; }

@media (max-width: 36rem) {
  .page { padding: 1.5rem 0; }
  .page-header { align-items: flex-start; flex-direction: column; }
  .account { width: 100%; justify-content: space-between; }
  .section-heading { align-items: flex-start; }
  .device-button { align-items: flex-start; flex-direction: column; gap: 0.35rem; }
}
</style>
