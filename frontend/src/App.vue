<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'

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
}

type DevicePage = {
  items: Device[]
  total: number
  limit: number
  offset: number
}

class ApiError extends Error {
  constructor(readonly status: number, readonly code: string) {
    super(code)
    this.name = 'ApiError'
  }
}

const pageSize = 50
const csrfHeaders = { 'X-CSRF-Protection': '1' }
const identity = ref<Identity | null>(null)
const initialized = ref(false)
const sessionError = ref('')
const username = ref('')
const password = ref('')
const loginMessage = ref('')
const loginPending = ref(false)
const logoutPending = ref(false)
const logoutUnconfirmed = ref(false)
const view = ref<'list' | 'detail'>('list')
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

let identityGeneration = 0
let detailController: AbortController | null = null
const readControllers = new Set<AbortController>()

const pageCount = computed(() => Math.max(1, Math.ceil(total.value / pageSize)))
const currentPage = computed(() => Math.floor(offset.value / pageSize) + 1)
const deviceRange = computed(() => total.value === 0
  ? '共 0 台設備'
  : `${offset.value + 1}–${Math.min(offset.value + devices.value.length, total.value)} / ${total.value} 台設備`)

function errorCode(value: unknown): string {
  if (typeof value === 'object' && value !== null && 'error' in value && typeof value.error === 'string') {
    return value.error
  }
  return ''
}

async function responseData<T>(response: Response): Promise<T> {
  if (!response.ok) {
    const body: unknown = await response.json().catch(() => null)
    throw new ApiError(response.status, errorCode(body))
  }
  if (response.status === 204) return undefined as T
  return await response.json() as T
}

async function writeApi<T>(path: string, init: RequestInit): Promise<T> {
  const response = await fetch(path, { ...init, credentials: 'same-origin' })
  return responseData<T>(response)
}

async function readApi<T>(path: string, generation: number, controller = new AbortController()): Promise<T> {
  readControllers.add(controller)
  try {
    const response = await fetch(path, { credentials: 'same-origin', signal: controller.signal })
    if (generation !== identityGeneration) throw new DOMException('Superseded request', 'AbortError')
    const result = await responseData<T>(response)
    if (generation !== identityGeneration) throw new DOMException('Superseded request', 'AbortError')
    return result
  } finally {
    readControllers.delete(controller)
  }
}

function clearProtectedState(): number {
  identityGeneration += 1
  for (const controller of readControllers) controller.abort()
  readControllers.clear()
  detailController = null
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
  loginPending.value = false
  logoutPending.value = false
  logoutUnconfirmed.value = false
  return identityGeneration
}

function isAbort(error: unknown): boolean {
  return error instanceof Error && error.name === 'AbortError'
}

function apiMessage(error: unknown, fallback: string): string {
  if (!(error instanceof ApiError)) return fallback
  if (error.status === 403 || error.code === 'forbidden') return '此操作未獲允許。'
  if (error.status === 404 || error.code === 'not_found') return '找不到這台設備，或您沒有權限查看。'
  if (error.status === 429 || error.code === 'rate_limited') return '登入嘗試次數已達限制，請稍後再試。'
  if (error.status === 503 || error.code === 'service_unavailable') return '服務暫時無法使用，請稍後重試。'
  return fallback
}

function expireSession(generation: number): void {
  if (generation !== identityGeneration) return
  clearProtectedState()
  initialized.value = true
  loginMessage.value = '登入狀態已失效，請重新登入。'
}

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
    if (generation !== identityGeneration || isAbort(error)) return
    if (error instanceof ApiError && error.status === 401) expireSession(generation)
    else devicesError.value = apiMessage(error, '設備清單載入失敗，請重試。')
  } finally {
    if (generation === identityGeneration) devicesLoading.value = false
  }
}

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
    if (generation !== identityGeneration || isAbort(error)) return
    initialized.value = true
    if (error instanceof ApiError && error.status === 401) return
    sessionError.value = apiMessage(error, '無法確認登入狀態，請檢查服務後重試。')
  }
}

async function login(): Promise<void> {
  if (loginPending.value || logoutPending.value || logoutUnconfirmed.value) return
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
    if (generation !== identityGeneration) return
    identity.value = signedInIdentity
    loginPending.value = false
    await loadDevices(0, generation)
  } catch (error) {
    if (generation !== identityGeneration) return
    if (error instanceof ApiError && error.status === 401) loginMessage.value = '帳號或密碼錯誤。'
    else loginMessage.value = apiMessage(error, '登入失敗，請確認服務狀態後重試。')
  } finally {
    if (generation === identityGeneration) loginPending.value = false
  }
}

async function logout(): Promise<void> {
  if (!identity.value || logoutPending.value || logoutUnconfirmed.value) return
  const generation = clearProtectedState()
  initialized.value = true
  logoutPending.value = true
  loginMessage.value = '正在登出…'
  try {
    await writeApi<void>('/api/logout', { method: 'POST', headers: csrfHeaders })
    if (generation === identityGeneration) loginMessage.value = '已登出。'
  } catch {
    if (generation === identityGeneration) {
      logoutUnconfirmed.value = true
      loginMessage.value = '登出結果未確認。請先確認狀態，再繼續登入。'
    }
  } finally {
    if (generation === identityGeneration) logoutPending.value = false
  }
}

async function confirmAndRetryLogout(): Promise<void> {
  if (!logoutUnconfirmed.value || logoutPending.value || loginPending.value) return
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

async function loadDevice(id: string): Promise<void> {
  const generation = identityGeneration
  detailController?.abort()
  const controller = new AbortController()
  detailController = controller
  selectedDeviceId.value = id
  selectedDevice.value = null
  detailError.value = ''
  detailCanRetry.value = false
  detailLoading.value = true
  try {
    selectedDevice.value = await readApi<Device>(`/api/devices/${encodeURIComponent(id)}`, generation, controller)
  } catch (error) {
    if (generation !== identityGeneration || isAbort(error)) return
    if (error instanceof ApiError && error.status === 401) expireSession(generation)
    else {
      detailError.value = apiMessage(error, '設備資料載入失敗，請重試。')
      detailCanRetry.value = !(error instanceof ApiError && error.status === 404)
    }
  } finally {
    if (detailController === controller) {
      detailController = null
      if (generation === identityGeneration) detailLoading.value = false
    }
  }
}

function returnToList(): void {
  detailController?.abort()
  detailController = null
  selectedDeviceId.value = null
  selectedDevice.value = null
  detailLoading.value = false
  detailError.value = ''
  detailCanRetry.value = false
  view.value = 'list'
}

function openDevice(id: string): void {
  view.value = 'detail'
  void loadDevice(id)
}

onMounted(restoreSession)
onUnmounted(() => {
  for (const controller of readControllers) controller.abort()
  readControllers.clear()
})
</script>

<template>
  <main class="page" aria-labelledby="page-title">
    <header class="page-header">
      <div>
        <p class="eyebrow">IoT 設備監控</p>
        <h1 id="page-title">{{ view === 'detail' ? '設備資料' : '設備清單' }}</h1>
      </div>
      <div v-if="identity" class="account">
        <span>{{ identity.username }}（{{ identity.role === 'headquarters' ? '總部' : '場域' }}）</span>
        <button type="button" class="button button-secondary" :disabled="logoutPending" @click="logout">
          {{ logoutPending ? '正在登出…' : '登出' }}
        </button>
      </div>
    </header>

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
        <input id="username" v-model="username" name="username" autocomplete="username" required :disabled="loginPending || logoutPending || logoutUnconfirmed" />
        <label for="password">密碼</label>
        <input id="password" v-model="password" name="password" type="password" autocomplete="current-password" required :disabled="loginPending || logoutPending || logoutUnconfirmed" />
        <button type="submit" class="button" :disabled="loginPending || logoutPending || logoutUnconfirmed">
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

    <section v-else class="panel" aria-labelledby="device-detail-title">
      <button type="button" class="back-button" @click="returnToList">← 返回設備清單</button>
      <h2 id="device-detail-title">{{ selectedDevice?.name ?? '設備詳細資料' }}</h2>
      <p v-if="detailLoading" aria-live="polite">正在載入設備資料…</p>
      <p v-else-if="detailError" class="notice" role="alert">{{ detailError }}</p>
      <dl v-else-if="selectedDevice" class="device-details">
        <div><dt>設備編號</dt><dd>{{ selectedDevice.id }}</dd></div>
        <div><dt>場域</dt><dd>{{ selectedDevice.siteName }}</dd></div>
      </dl>
      <button v-if="detailError && detailCanRetry && selectedDeviceId" type="button" class="button button-secondary" @click="loadDevice(selectedDeviceId)">重新載入</button>
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

@media (max-width: 36rem) {
  .page { padding: 1.5rem 0; }
  .page-header { align-items: flex-start; flex-direction: column; }
  .account { width: 100%; justify-content: space-between; }
  .section-heading { align-items: flex-start; }
  .device-button { align-items: flex-start; flex-direction: column; gap: 0.35rem; }
}
</style>
