<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AlertHistory from './AlertHistory.vue'
import TaichungMonitoring from './TaichungMonitoring.vue'
import { useTaichungMonitoring } from './useTaichungMonitoring'
import { useMonitoringSession } from './useMonitoringSession'
import { useDeviceMonitoring } from './useDeviceMonitoring'
import { isAbnormal } from './monitoringRules'
import {
  formatTemperature,
  noReplayMessage,
  onlineStatuses,
  receiptTime,
  simulationActions,
  simulationModes,
} from './monitoringPresentation'
import { liveMessages } from './live'
import { pageSize } from './api'

const view = ref<'list' | 'detail' | 'alerts'>('list')
const source = ref<'official' | 'simulation'>(new URL(window.location.href).searchParams.get('view') === 'simulation' ? 'simulation' : 'official')
const alertPending = ref(false)
const alertHistory = ref<InstanceType<typeof AlertHistory> | null>(null)
let deviceSelectionGeneration = 0

// 回呼只在掛載後的會話操作使用；先完成 setup，才啟用工作區的請求。
const session = useMonitoringSession({
  clear: () => {
    deviceSelectionGeneration += 1
    deviceMonitoring.clear()
    officialMonitoring.clear()
    view.value = 'list'
    alertPending.value = false
  },
  load: async generation => {
    if (source.value === 'official') await officialMonitoring.load(true, generation)
    else await loadDevices(0, generation)
  },
  canRefresh: () => canRefreshLive.value,
  refresh: async (generation, controller) => source.value === 'official' ? false : view.value === 'alerts'
    ? await alertHistory.value?.refreshLive(controller) ?? false
    : deviceMonitoring.refreshLive(generation, controller),
})
const deviceMonitoring = useDeviceMonitoring(session.access)
const officialMonitoring = useTaichungMonitoring(session.access)
const { loading: officialLoading } = officialMonitoring
const {
  identity, initialized, sessionError, username, password, loginMessage, loginPending,
  logoutPending, logoutUnconfirmed, liveState, retryLive,
  restoreSession, login, logout, confirmAndRetryLogout,
} = session
const {
  devices, total, offset, devicesLoading, devicesError, selectedDeviceId, selectedDevice,
  detailLoading, detailError, detailCanRetry, simulation, simulationLoading, simulationPending,
  simulationReadError, simulationActionError, simulationFeedback, pageCount, currentPage,
  deviceRange, selectedTemperature, simulationBusy,
  refreshDevice, loadSimulation, changeSimulation, loadDevice,
} = deviceMonitoring

// 手動查詢、寫入及結果確認有自己的畫面狀態，等它們結束後才補讀。
const canRefreshLive = computed(() => {
  if (source.value === 'official') return false
  if (view.value === 'alerts') return alertHistory.value?.liveReady === true
  if (view.value === 'detail') return deviceMonitoring.detailReady.value
  return !devicesLoading.value
})
// 結案仍待確認時保留警示，即使串流連線正常。
const liveMessage = computed(() =>
  alertHistory.value?.needsVerification
  && (
    liveState.value === 'live'
    || liveState.value === 'syncing'
  )
    ? liveMessages.stale
    : liveMessages[liveState.value])

// 只有明確進頁、換頁或重載可預選；晚到清單不得搶走手動選取或行動版返回。
async function loadDevices(
  nextOffset: number,
  generation = session.access.currentGeneration(),
): Promise<void> {
  const selectionGeneration = ++deviceSelectionGeneration
  const loaded = await deviceMonitoring.loadDevices(nextOffset, generation)
  // 新選取會取消舊詳情讀取；只有未完成寫入需要保留原設備。
  if (
    !loaded
    || selectionGeneration !== deviceSelectionGeneration
    || !session.access.isCurrentIdentity(generation)
    || view.value === 'alerts'
    || simulationPending.value
  ) return

  // 重載保留仍在本頁的選取；換頁或空清單則依目前可見範圍處理。
  if (devices.value.some(device => device.id === selectedDeviceId.value)) return
  const first = devices.value[0]
  if (first) await openDevice(first.id)
  else returnToList()
}

// 返回設備清單不觸發讀取或預選，讓行動版能停留在清單。
function returnToList(): void {
  deviceSelectionGeneration += 1
  deviceMonitoring.clearDetail()
  view.value = 'list'
}

// 切到詳情後立即載入指定設備，讓等待與錯誤都顯示在詳情畫面。
async function openDevice(id: string): Promise<void> {
  deviceSelectionGeneration += 1
  view.value = 'detail'
  await loadDevice(id)
}

// 切換功能時清除設備詳情，並保留原本的設備清單頁碼。
function navigateTo(section: 'list' | 'alerts'): void {
  if (
    !identity.value
    || alertPending.value
    || simulationPending.value
    || (source.value === 'simulation' && view.value === section)
  ) return
  const returningFromAlerts = view.value === 'alerts'
  const switchingSource = source.value !== 'simulation'
  officialMonitoring.clear()
  source.value = 'simulation'
  updateSourceUrl()
  deviceSelectionGeneration += 1
  if (view.value === 'detail') returnToList()
  view.value = section
  if (section === 'list' && (returningFromAlerts || switchingSource)) void loadDevices(offset.value)
}

// 入口與重整後的資料來源一致，不改變同源 Cookie 或既有 API 位址。
function updateSourceUrl(): void {
  const url = new URL(window.location.href)
  if (source.value === 'simulation') url.searchParams.set('view', 'simulation')
  else url.searchParams.delete('view')
  window.history.replaceState(null, '', url)
}

// 官方頁擁有獨立輪詢；離開模擬頁先取消舊清單與詳情查詢。
function showOfficial(): void {
  if (!identity.value || alertPending.value || simulationPending.value || source.value === 'official') return
  deviceSelectionGeneration += 1
  deviceMonitoring.clear()
  source.value = 'official'
  view.value = 'list'
  updateSourceUrl()
  void officialMonitoring.load()
}

// 在舊查詢有機會套用前，同步切斷上一個頁面或手動操作前的被動讀取。
watch(
  [source, view, selectedDeviceId, offset, () => alertHistory.value?.liveTarget],
  session.invalidateLiveTarget,
  { flush: 'sync' },
)
watch(canRefreshLive, session.updateLiveReadiness, { flush: 'sync' })

</script>

<template>
  <div v-if="!identity" class="login-layout">
    <aside class="login-intro" aria-label="產品介紹">
      <div class="brand">
        <svg viewBox="0 0 32 32" aria-hidden="true">
          <path d="M5 26v-5m7 5V15m8 11V9m7 17V3" />
        </svg>IoT 設備監控</div>
      <div class="intro-copy">
        <h1>看見設備狀態，<br />掌握每一次異常。</h1>
        <p>查看臺中市官方環境觀測，或進入模擬展示追蹤告警恢復與處理紀錄。</p>
      </div>
      <div class="intro-steps" aria-label="監控流程">
        <div>
          <svg viewBox="0 0 32 32" aria-hidden="true">
            <rect x="5" y="5" width="22" height="9" rx="2" />
            <rect x="5" y="18" width="22" height="9" rx="2" />
            <path d="M10 9.5h.1m12 0h.1M10 22.5h.1m12 0h.1" />
          </svg>
          <span>設備回報</span>
        </div>
        <span class="step-line" aria-hidden="true"></span>
        <div>
          <svg viewBox="0 0 32 32" aria-hidden="true">
            <rect x="4" y="5" width="24" height="18" rx="2" />
            <path d="M16 23v5m-6 0h12" />
          </svg>
          <span>即時監控</span>
        </div>
        <span class="step-line" aria-hidden="true"></span>
        <div>
          <svg viewBox="0 0 32 32" aria-hidden="true">
            <path d="M5 24h22l-3-5v-7a8 8 0 0 0-16 0v7Zm8 4a3 3 0 0 0 6 0M16 2v2" />
          </svg>
          <span>告警處理</span>
        </div>
      </div>
    </aside>
    <main class="login-main">
      <section v-if="!initialized" class="login-panel" aria-live="polite">
        <p>正在確認登入狀態…</p>
      </section>
      <section
        v-else-if="sessionError"
        class="login-panel"
        aria-labelledby="session-error-title"
      >
        <h2 id="session-error-title">目前無法載入</h2>
        <p class="notice" role="alert">{{ sessionError }}</p>
        <button type="button" class="button" @click="restoreSession">重新嘗試</button>
      </section>
      <section v-else class="login-panel" aria-labelledby="login-title">
        <h2 id="login-title">登入監控工作台</h2>
        <p class="login-subtitle">總部與場域帳號共用登入入口</p>
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
          >{{ loginPending ? '登入中…' : '登入' }}</button>
        </form>
        <p v-if="loginMessage" class="notice" role="alert">{{ loginMessage }}</p>
        <button
          v-if="logoutUnconfirmed"
          type="button"
          class="button button-secondary"
          :disabled="logoutPending"
          @click="confirmAndRetryLogout"
        >{{ logoutPending ? '正在確認登出…' : '確認並重試登出' }}</button>
        <p v-if="logoutUnconfirmed" class="muted">系統會先確認登入狀態；若仍有效，會重新送出登出。</p>
        <p class="login-note muted">請使用已取得授權的帳號登入。</p>
      </section>
    </main>
  </div>

  <div v-else class="app-layout">
    <aside class="sidebar">
      <div class="brand">IoT 設備監控</div>
      <nav class="section-nav" aria-label="監控功能">
        <button
          type="button"
          :aria-current="source === 'official' ? 'page' : undefined"
          :disabled="alertPending || simulationPending"
          @click="showOfficial"
        ><svg viewBox="0 0 24 24" aria-hidden="true">
            <path d="M8 6h13M8 12h13M8 18h13M3 6h.1M3 12h.1M3 18h.1" />
          </svg>設備清單</button>
        <button
          type="button"
          :aria-current="source === 'simulation' && view !== 'alerts' ? 'page' : undefined"
          :disabled="alertPending || simulationPending"
          @click="navigateTo('list')"
        ><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 5 11 7-11 7ZM4 5v14" /></svg>模擬展示</button>
        <button
          type="button"
          :aria-current="view === 'alerts' ? 'page' : undefined"
          :disabled="alertPending || simulationPending"
          @click="navigateTo('alerts')"
        ><svg viewBox="0 0 24 24" aria-hidden="true">
            <rect x="4" y="2" width="16" height="20" rx="1" />
            <path d="M8 7h8M8 11h8M8 15h5" />
          </svg>告警歷程</button>
      </nav>
      <div class="account">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <circle cx="12" cy="7" r="4" />
          <path d="M3 22v-3a9 9 0 0 1 18 0v3Z" />
        </svg>
        <div>
          <strong>{{ identity.username }}</strong>
          <span>{{ identity.role === 'headquarters' ? '總部' : '場域' }}</span>
        </div>
        <button
          type="button"
          :disabled="logoutPending"
          @click="logout"
        >{{ logoutPending ? '正在登出…' : '登出' }}<span aria-hidden="true"> ↗</span></button>
      </div>
    </aside>

    <main class="workspace" aria-labelledby="page-title">
      <header class="page-header">
        <div>
          <h1 id="page-title">{{ view === 'alerts' ? '告警歷程' : '設備清單' }}</h1>
          <p class="muted">{{ source === 'official' ? '臺中市環境感測器 · 官方資料觀測' : view === 'alerts' ? '模擬展示 · 追蹤異常、恢復與結案結果' : '模擬展示 · 查看場域設備的最新有效回報' }}</p>
        </div>
        <div class="header-actions">
          <div
            v-if="source === 'simulation'"
            class="live-status"
            :class="{ 'is-live': liveState === 'live' && !alertHistory?.needsVerification }"
          >
            <p id="live-status" role="status">{{ liveMessage }}</p>
            <button
              v-if="liveState === 'disconnected' || liveState === 'stale'"
              type="button"
              class="button button-secondary"
              @click="retryLive"
            >重新同步</button>
          </div>
          <button
            v-if="source === 'official'"
            type="button"
            class="button button-secondary reload-button official-resample"
            @click="officialMonitoring.load()"
          ><span aria-hidden="true">↻</span> {{ officialLoading ? '重新抽選中…' : '重新抽選六台' }}</button>
          <button
            v-else
            type="button"
            class="button button-secondary reload-button"
            :disabled="view === 'alerts' ? !alertHistory?.liveReady : devicesLoading || simulationBusy"
            @click="view === 'alerts' ? alertHistory?.reload() : loadDevices(offset)"
          ><span aria-hidden="true">↻</span> {{ devicesLoading ? '載入中…' : '重新載入' }}</button>
        </div>
      </header>

      <TaichungMonitoring v-if="source === 'official'" :monitoring="officialMonitoring" />
      <AlertHistory
        v-else-if="view === 'alerts'"
        ref="alertHistory"
        :session="session.access"
        @pending="alertPending = $event"
      />

      <template v-else>
        <section
          class="monitor-board"
          :class="{ 'has-selection': selectedDeviceId }"
          aria-label="設備監控"
        >
          <div class="list-pane">
            <div class="table-heading device-columns" aria-hidden="true">
              <span>設備</span>
              <span>場域</span>
              <span>在線狀態</span>
              <span>最新有效溫度</span>
            </div>
            <p
              v-if="devicesLoading && devices.length === 0"
              class="pane-message"
              role="status"
            >正在載入設備…</p>
            <p v-if="devicesError" class="notice pane-message" role="alert">{{ devicesError }}</p>
            <p
              v-else-if="!devicesLoading && devices.length === 0"
              class="muted pane-message"
            >目前沒有可查看的設備。</p>
            <ul v-if="devices.length" class="device-list">
              <li v-for="device in devices" :key="device.id">
                <button
                  type="button"
                  class="device-button device-columns"
                  :aria-pressed="selectedDeviceId === device.id"
                  :disabled="simulationPending"
                  @click="openDevice(device.id)"
                >
                  <span class="device-name">{{ device.name }}</span>
                  <span class="device-site">{{ device.siteName }}</span>
                  <span class="status" :class="device.onlineStatus">{{ onlineStatuses[device.onlineStatus] }}</span>
                  <span
                    class="row-temperature"
                    :class="{ abnormal: isAbnormal(device.latestMeasurement?.temperature), stale: device.onlineStatus === 'offline' }"
                  >{{ device.latestMeasurement ? `${formatTemperature(device.latestMeasurement.temperature)}°C` : '—' }}<small v-if="device.onlineStatus === 'offline' && device.latestMeasurement">資料已非即時</small></span>
                </button>
              </li>
            </ul>
            <footer class="list-footer">
              <p class="muted">{{ deviceRange }}</p>
              <nav v-if="total > pageSize" class="pagination" aria-label="設備清單分頁">
                <button
                  type="button"
                  class="button button-secondary"
                  :disabled="
                    devicesLoading
                    || simulationPending
                    || offset === 0
                  "
                  @click="loadDevices(Math.max(0, offset - pageSize))"
                >上一頁</button>
                <span aria-live="polite">第 {{ currentPage }} / {{ pageCount }} 頁</span>
                <button
                  type="button"
                  class="button button-secondary"
                  :disabled="
                    devicesLoading
                    || simulationPending
                    || offset + devices.length >= total
                  "
                  @click="loadDevices(offset + pageSize)"
                >下一頁</button>
              </nav>
            </footer>
          </div>

          <section class="detail-pane" aria-labelledby="device-detail-title">
            <template v-if="selectedDeviceId">
              <button
                type="button"
                class="back-button"
                :disabled="simulationPending"
                @click="returnToList"
              >← 返回設備清單</button>
              <div class="detail-heading">
                <div>
                  <h2 id="device-detail-title">{{ selectedDevice?.name ?? '設備詳細資料' }}</h2>
                  <p v-if="selectedDevice" class="muted">{{ selectedDevice.id }} · {{ selectedDevice.siteName }}</p>
                </div>
                <span
                  v-if="selectedDevice"
                  class="status"
                  :class="selectedDevice.onlineStatus"
                >{{ onlineStatuses[selectedDevice.onlineStatus] }}</span>
              </div>
              <p v-if="detailLoading" role="status">正在載入設備資料…</p>
              <p v-if="detailError" class="notice" role="alert">{{ detailError }}</p>
              <dl v-if="selectedDevice" class="device-details">
                <div class="temperature-reading">
                  <dt>最新有效溫度</dt>
                  <dd :class="{ abnormal: isAbnormal(selectedTemperature), stale: selectedDevice.onlineStatus === 'offline' }">
                    <span class="temperature-value">{{ selectedTemperature !== undefined ? formatTemperature(selectedTemperature) : '—' }}<small v-if="selectedTemperature !== undefined">°C</small></span>
                    <span
                      v-if="selectedDevice.onlineStatus === 'offline' && selectedDevice.latestMeasurement"
                      class="reading-note"
                    >資料已非即時</span>
                    <span
                      v-else-if="selectedTemperature !== undefined"
                      class="reading-note"
                    >{{ isAbnormal(selectedTemperature) ? '超出正常範圍' : '正常範圍內' }}</span>
                  </dd>
                </div>
                <div class="range-indicator">
                  <dt class="sr-only">展示正常範圍</dt>
                  <dd>
                    <div class="range-labels" aria-hidden="true">
                      <span>18°C</span>
                      <span>28°C</span>
                    </div>
                    <div class="range-track" aria-hidden="true">
                      <span
                        v-if="selectedTemperature !== undefined"
                        class="range-marker"
                        :class="{ abnormal: isAbnormal(selectedTemperature) }"
                        :style="{ left: `${Math.max(0, Math.min(100, (selectedTemperature - 14) * 5))}%` }"
                      ></span>
                    </div>
                    <p class="muted">展示正常範圍 18–28°C</p>
                  </dd>
                </div>
                <div class="receipt-row">
                  <dt>最後有效回報時間</dt>
                  <dd>
                    <time
                      v-if="selectedDevice.latestMeasurement"
                      :datetime="selectedDevice.latestMeasurement.receivedAt"
                    >{{ receiptTime.format(new Date(selectedDevice.latestMeasurement.receivedAt)) }}</time>
                    <template v-else>—</template>
                  </dd>
                </div>
              </dl>
              <button
                v-if="selectedDeviceId && (selectedDevice || (detailError && detailCanRetry))"
                type="button"
                class="text-button detail-reload"
                :disabled="simulationBusy"
                @click="selectedDevice ? refreshDevice() : loadDevice(selectedDeviceId)"
              >重新載入設備狀態</button>

              <section
                v-if="selectedDevice && identity.role === 'headquarters'"
                class="simulation-controls"
                aria-labelledby="simulation-title"
              >
                <h3 id="simulation-title">展示情境</h3>
                <p class="muted">設備會自動回報，可切換情境觀察異常與恢復。</p>
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
                    class="button"
                    :class="{ 'button-secondary': action !== 'normal', 'danger-button': action === 'abnormal' }"
                    :disabled="simulationBusy || (action === 'replay' && !simulation.canReplay)"
                    @click="changeSimulation(action)"
                  >{{ option.label }}</button>
                </div>
                <p
                  v-if="simulation && !simulation.canReplay && simulationActionError !== noReplayMessage"
                  class="muted"
                >{{ noReplayMessage }}</p>
                <button
                  v-if="simulationReadError"
                  type="button"
                  class="button button-secondary"
                  :disabled="simulationBusy"
                  @click="loadSimulation()"
                >重新載入模擬狀態</button>
                <p class="control-help muted">每 5 秒回報；超過 15 秒未收到有效回報即判定離線。</p>
              </section>
            </template>
            <div v-else class="empty-detail">
              <svg viewBox="0 0 32 32" aria-hidden="true">
                <rect x="4" y="4" width="24" height="24" rx="3" />
                <path d="M10 11h12M10 16h12M10 21h7" />
              </svg>
              <h2 id="device-detail-title">選擇一台設備</h2>
              <p class="muted">查看最新有效溫度、回報時間與設備狀態。</p>
            </div>
          </section>
        </section>
        <p class="workspace-note muted">離線表示逾時未收到有效回報，最後有效溫度不代表目前溫度。</p>
      </template>
    </main>
  </div>
</template>

<style>
:root {
  color: #101923;
  background: #f3f6f9;
  font-family: "Noto Sans TC", "PingFang TC", "Microsoft JhengHei", ui-sans-serif, system-ui, -apple-system, sans-serif;
  font-synthesis: none;
  text-rendering: optimizeLegibility;
  font-variant-numeric: tabular-nums;
}
* {
  box-sizing: border-box;
}
body {
  margin: 0;
  min-width: 320px;
}
button, input, textarea {
  font: inherit;
}
button {
  cursor: pointer;
}
button:disabled {
  cursor: not-allowed;
  opacity: .55;
}
button:focus-visible, input:focus-visible, textarea:focus-visible, [tabindex="-1"]:focus-visible {
  outline: 3px solid #245edb;
  outline-offset: 3px;
}
svg {
  width: 24px;
  height: 24px;
  fill: none;
  stroke: currentColor;
  stroke-width: 1.8;
  stroke-linecap: round;
  stroke-linejoin: round;
  flex-shrink: 0;
}
h1, h2, h3, p {
  margin-top: 0;
}
h1 {
  margin-bottom: 4px;
  font-size: clamp(30px, 2.8vw, 42px);
  line-height: 1.25;
  letter-spacing: -.035em;
}
h2 {
  margin-bottom: 4px;
  font-size: 28px;
  letter-spacing: -.025em;
}
h3 {
  margin-bottom: 10px;
  font-size: 23px;
}
p {
  line-height: 1.65;
}
.muted {
  color: #566170;
}
.notice {
  color: #af2e25;
  overflow-wrap: anywhere;
}
.button {
  min-height: 46px;
  padding: 10px 18px;
  border: 1px solid #245edb;
  border-radius: 6px;
  background: #245edb;
  color: #fff;
  font-weight: 650;
  line-height: 1.4;
}
.button:hover:not(:disabled) {
  background: #1d4eb7;
}
.button-secondary {
  border-color: #97a6b7;
  background: #fff;
  color: #152638;
}
.button-secondary:hover:not(:disabled) {
  background: #edf3fc;
}
.danger-button {
  color: #c72c28;
}
.text-button, .back-button {
  padding: 3px 0;
  border: 0;
  background: transparent;
  color: #245edb;
  text-align: left;
}
.text-button:hover, .back-button:hover {
  text-decoration: underline;
}
.back-button {
  margin-bottom: 14px;
  font-size: 14px;
}
.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip-path: inset(50%);
  white-space: nowrap;
  border: 0;
}
.app-layout {
  display: grid;
  grid-template-columns: 248px minmax(0, 1fr);
  min-height: 100dvh;
}
.sidebar {
  position: sticky;
  top: 0;
  display: flex;
  flex-direction: column;
  height: 100dvh;
  padding: 30px 14px 24px;
  background: #152638;
  color: #fff;
}
.brand {
  display: flex;
  align-items: center;
  gap: 16px;
  font-size: 26px;
  font-weight: 750;
  white-space: nowrap;
}
.sidebar .brand {
  padding: 0 10px;
}
.section-nav {
  display: grid;
  gap: 8px;
  margin-top: 42px;
}
.section-nav button {
  display: flex;
  align-items: center;
  gap: 18px;
  min-height: 54px;
  padding: 13px 18px;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: #fff;
  text-align: left;
  font-size: 22px;
  font-weight: 650;
}
.section-nav button:hover {
  background: #203951;
}
.section-nav [aria-current] {
  background: #245edb;
}
.section-nav [aria-current]:hover {
  background: #1d4eb7;
}
.account {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  margin-top: auto;
  padding: 20px 10px 0;
  border-top: 1px solid #708090;
}
.account div {
  display: grid;
  gap: 3px;
}
.account button {
  margin-left: auto;
  padding: 6px 0;
  border: 0;
  background: transparent;
  color: #fff;
  white-space: nowrap;
}
.workspace {
  min-width: 0;
  padding: 28px 24px 20px;
}
.page-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;
  margin-bottom: 26px;
}
.page-header p {
  margin-bottom: 0;
  font-size: 18px;
}
.header-actions, .live-status {
  display: flex;
  align-items: center;
  gap: 20px;
}
.header-actions {
  padding-top: 4px;
}
.live-status {
  gap: 12px;
  max-width: 380px;
  color: #8a5510;
}
.live-status p {
  font-size: 16px;
  font-weight: 650;
}
.live-status p::before {
  display: inline-block;
  width: 10px;
  height: 10px;
  margin-right: 9px;
  border-radius: 50%;
  background: currentColor;
  content: "";
}
.live-status.is-live {
  color: #178236;
}
.reload-button {
  white-space: nowrap;
}
.reload-button span {
  margin-right: 6px;
  font-size: 24px;
  line-height: 0;
}
.monitor-board {
  display: grid;
  grid-template-columns: minmax(0, 1.35fr) minmax(0, 1fr);
  min-height: calc(100dvh - 210px);
  border-radius: 8px;
  background: #fff;
  overflow: clip;
}
.list-pane {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.device-columns {
  display: grid;
  grid-template-columns: minmax(0, 1.15fr) minmax(88px, .65fr) minmax(125px, .9fr) minmax(0, 1fr);
  align-items: center;
  gap: 12px;
}
.table-heading {
  min-height: 70px;
  padding: 20px 24px;
  border-bottom: 1px solid #cbd4df;
  font-size: 20px;
  font-weight: 650;
}
.device-list {
  margin: 0;
  padding: 0;
  list-style: none;
}
.device-list li {
  border-bottom: 1px solid #cbd4df;
}
.device-button {
  width: 100%;
  min-height: 94px;
  padding: 22px 24px;
  border: 0;
  border-radius: 0;
  background: #fff;
  color: inherit;
  text-align: left;
  font-size: 22px;
}
.device-button:hover:not(:disabled) {
  background: #f1f6fd;
}
.device-button[aria-pressed="true"] {
  background: #e1edff;
  box-shadow: inset 5px 0 #245edb;
}
.device-name {
  font-weight: 700;
  overflow-wrap: anywhere;
}
.device-site {
  color: #263444;
}
.status {
  display: inline-flex;
  align-items: center;
  gap: 9px;
  font-size: 19px;
  line-height: 1.5;
  font-weight: 600;
}
.status::before {
  width: 10px;
  height: 10px;
  border-radius: 50%;
  background: #85909c;
  content: "";
  flex-shrink: 0;
}
.status.online {
  color: #178236;
}
.status.online::before {
  background: #178236;
}
.status.offline, .status.active {
  color: #a86200;
}
.status.offline::before, .status.active::before {
  background: #e99a08;
}
.status.awaiting, .status.unreported, .status.closed {
  color: #566170;
}
.status.recovered {
  color: #245edb;
}
.status.recovered::before {
  background: #245edb;
}
.row-temperature {
  min-width: 0;
  font-size: 30px;
  white-space: normal;
  overflow-wrap: anywhere;
}
.abnormal {
  color: #d32624;
}
.stale {
  color: #616b78;
}
.row-temperature small {
  display: block;
  margin-top: 3px;
  color: #566170;
  font-size: 14px;
  white-space: normal;
}
.list-footer {
  margin-top: auto;
  padding: 16px 24px;
  border-top: 1px solid #dbe2eb;
}
.list-footer p {
  margin-bottom: 0;
}
.pagination {
  display: flex;
  align-items: center;
  justify-content: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-top: 14px;
  font-size: 14px;
}
.pagination .button {
  min-height: 38px;
  padding: 7px 12px;
}
.detail-pane {
  min-width: 0;
  padding: 24px 28px;
  border-left: 1px solid #a9b5c4;
}
.detail-heading {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 12px;
  padding-bottom: 18px;
  border-bottom: 1px solid #cbd4df;
}
.detail-heading h2 {
  font-size: clamp(24px, 2vw, 32px);
  overflow-wrap: anywhere;
}
.detail-heading p {
  margin-bottom: 0;
  font-size: 17px;
}
.detail-heading .status {
  padding-top: 7px;
  white-space: nowrap;
}
.device-details {
  margin: 0;
}
.device-details dt {
  color: #465362;
}
.device-details dd {
  margin: 0;
  min-width: 0;
  overflow-wrap: anywhere;
}
.temperature-reading {
  padding-top: 18px;
}
.temperature-reading dt {
  font-size: 18px;
  font-weight: 600;
}
.temperature-value {
  display: block;
  font-size: clamp(60px, 5.8vw, 86px);
  font-weight: 650;
  letter-spacing: -.045em;
}
.temperature-value small {
  margin-left: 8px;
  font-size: .48em;
  font-weight: 500;
  letter-spacing: -.02em;
}
.reading-note {
  display: block;
  margin: 2px 0 18px;
  font-size: 18px;
  font-weight: 600;
}
.range-indicator {
  margin-bottom: 22px;
}
.range-labels {
  display: flex;
  justify-content: space-between;
  width: 50%;
  margin-left: 20%;
  font-size: 15px;
}
.range-track {
  position: relative;
  height: 10px;
  margin: 12px 8px 14px;
  border-radius: 5px;
  background: linear-gradient(to right, #e0e6ed 20%, #3eaf47 20%, #3eaf47 70%, #e0e6ed 70%);
}
.range-marker {
  position: absolute;
  top: -4px;
  width: 18px;
  height: 18px;
  border: 2px solid #fff;
  border-radius: 50%;
  background: #178236;
  transform: translateX(-50%);
}
.range-marker.abnormal {
  background: #d32624;
}
.range-indicator p {
  margin: 0;
  font-size: 14px;
}
.receipt-row {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  align-items: center;
  gap: 12px;
  padding: 10px 12px;
  border: 1px solid #e0e6ed;
  border-radius: 5px;
  background: #f7f9fc;
  font-size: 14px;
}
.detail-reload {
  margin-top: 10px;
  font-size: 13px;
}
.simulation-controls {
  margin-top: 22px;
  padding-top: 20px;
  border-top: 1px solid #cbd4df;
}
.simulation-controls p {
  margin-bottom: 12px;
}
.simulation-actions {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px;
  margin: 16px 0 12px;
}
.simulation-actions .button {
  padding: 11px 8px;
}
.control-help {
  margin: 14px 0 0;
  font-size: 14px;
}
.empty-detail {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  min-height: 420px;
  height: 100%;
  text-align: center;
}
.empty-detail svg {
  width: 52px;
  height: 52px;
  margin-bottom: 18px;
  color: #94a4b7;
}
.empty-detail h2 {
  font-size: 23px;
}
.empty-detail p {
  max-width: 260px;
  margin: 8px 0;
}
.pane-message {
  padding: 20px 24px;
}
.workspace-note {
  margin: 16px 8px 0;
  font-size: 14px;
}
.login-layout {
  display: grid;
  grid-template-columns: 43% minmax(0, 1fr);
  min-height: 100dvh;
  background: #fff;
}
.login-intro {
  display: flex;
  flex-direction: column;
  justify-content: space-between;
  gap: 54px;
  padding: 54px 54px 18vh;
  background: #152638;
  color: #fff;
}
.login-intro .brand {
  font-size: 30px;
}
.login-intro .brand svg {
  width: 36px;
  height: 36px;
  stroke-width: 4;
}
.intro-copy h1 {
  margin-bottom: 28px;
  font-size: clamp(32px, 3.8vw, 58px);
  line-height: 1.55;
  letter-spacing: .015em;
}
.intro-copy p {
  max-width: 21em;
  margin: 0;
  font-size: 24px;
  line-height: 1.8;
}
.intro-steps {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 10px;
}
.intro-steps div {
  display: grid;
  justify-items: center;
  gap: 15px;
  white-space: nowrap;
  font-size: 22px;
  font-weight: 650;
}
.intro-steps svg {
  width: 96px;
  height: 96px;
  padding: 22px;
  border: 2px solid #fff;
  border-radius: 50%;
}
.step-line {
  flex: 1;
  height: 1px;
  margin-top: 48px;
  background: #cbd4df;
}
.login-main {
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 60px 9%;
}
.login-panel {
  width: 100%;
  max-width: 580px;
}
.login-panel h2 {
  font-size: clamp(30px, 3.4vw, 48px);
  font-weight: 750;
}
.login-subtitle {
  margin: 12px 0 42px;
  color: #566170;
  font-size: 23px;
}
form {
  display: grid;
  gap: 10px;
}
label {
  margin-top: 12px;
  font-weight: 650;
}
input, textarea {
  width: 100%;
  min-width: 0;
  padding: 12px 16px;
  border: 1px solid #8998aa;
  border-radius: 5px;
  background: #fff;
  color: #101923;
}
.login-panel input {
  min-height: 60px;
  font-size: 22px;
}
.login-panel label {
  font-size: 21px;
}
.login-panel .button {
  margin-top: 28px;
  min-height: 60px;
  font-size: 22px;
}
.login-note {
  margin: 30px 0 0;
  font-size: 18px;
}
@media (max-width: 1200px) {
  .app-layout {
    grid-template-columns: 210px minmax(0, 1fr);
  }
  .sidebar .brand {
    font-size: 22px;
  }
  .workspace {
    padding: 24px 18px;
  }
  .page-header {
    flex-wrap: wrap;
  }
  .header-actions {
    margin-left: auto;
  }
  .monitor-board {
    grid-template-columns: minmax(0, 1.1fr) minmax(0, 1fr);
  }
  .device-columns {
    grid-template-columns: minmax(0, 1fr) 50px minmax(65px, .7fr) minmax(90px, .8fr);
    gap: 8px;
  }
  .table-heading, .device-button {
    padding-left: 16px;
    padding-right: 16px;
    font-size: 14px;
  }
  .row-temperature {
    font-size: 20px;
  }
  .status {
    font-size: 14px;
  }
  .detail-pane {
    padding: 22px;
  }
  .receipt-row {
    grid-template-columns: 1fr;
    gap: 4px;
  }
  .login-intro {
    padding: 40px 32px 12vh;
  }
  .login-main {
    padding: 48px 7%;
  }
  .intro-steps div {
    font-size: 16px;
  }
  .intro-steps svg {
    width: 62px;
    height: 62px;
    padding: 14px;
  }
  .step-line {
    margin-top: 31px;
  }
}
@media (max-width: 960px) {
  .app-layout {
    grid-template-columns: 1fr;
  }
  .sidebar {
    position: static;
    height: auto;
    padding: 18px 22px;
    display: grid;
    grid-template-columns: 1fr auto;
    gap: 16px;
  }
  .sidebar .brand {
    padding: 0;
  }
  .section-nav {
    display: flex;
    grid-column: 1 / -1;
    grid-row: 2;
    gap: 10px;
    margin: 0;
  }
  .section-nav button {
    min-height: 46px;
    padding: 10px 18px;
    font-size: 16px;
  }
  .account {
    margin: 0;
    padding: 0;
    border: 0;
    align-items: center;
    font-size: 14px;
  }
  .account > svg, .account div span {
    display: none;
  }
  .account button {
    margin-left: 6px;
  }
  .monitor-board {
    grid-template-columns: 1fr;
    min-height: 600px;
  }
  .detail-pane {
    display: none;
    border-left: 0;
  }
  .monitor-board.has-selection .list-pane {
    display: none;
  }
  .monitor-board.has-selection .detail-pane {
    display: block;
  }
  .device-columns {
    grid-template-columns: minmax(0, 1.15fr) minmax(50px, .6fr) minmax(95px, 1fr) minmax(120px, 1fr);
  }
  .device-button, .table-heading {
    padding: 20px 24px;
    font-size: 17px;
  }
  .status {
    font-size: 16px;
  }
  .detail-heading h2 {
    font-size: 30px;
  }
  .temperature-value {
    font-size: 76px;
  }
  .receipt-row {
    grid-template-columns: auto 1fr;
  }
  .login-layout {
    grid-template-columns: 38% minmax(0, 1fr);
  }
  .login-intro {
    padding: 32px 24px;
    justify-content: flex-start;
  }
  .login-intro .brand {
    gap: 10px;
    font-size: 21px;
  }
  .intro-copy h1 {
    font-size: 30px;
  }
  .intro-copy p {
    font-size: 17px;
  }
  .intro-steps {
    display: none;
  }
  .login-main {
    padding: 44px 32px;
  }
  .login-subtitle {
    font-size: 18px;
  }
}
@media (max-width: 600px) {
  .sidebar {
    padding: 16px;
    gap: 14px 8px;
  }
  .sidebar .brand {
    font-size: 20px;
  }
  .account {
    gap: 4px;
    font-size: 13px;
  }
  .section-nav button {
    flex: 1;
    justify-content: center;
    gap: 10px;
    padding: 10px;
    min-width: 0;
    font-size: 15px;
  }
  .section-nav button svg {
    display: none;
  }
  .workspace {
    padding: 22px 14px;
  }
  .page-header {
    gap: 16px;
    margin-bottom: 18px;
  }
  .page-header p {
    font-size: 15px;
  }
  .header-actions {
    justify-content: space-between;
    flex-wrap: wrap;
    gap: 12px;
    width: 100%;
    padding: 0;
  }
  .live-status {
    max-width: 100%;
    flex-wrap: wrap;
    gap: 8px;
  }
  .live-status p {
    font-size: 14px;
  }
  .reload-button {
    min-height: 38px;
    padding: 8px 12px;
    font-size: 14px;
  }
  .monitor-board {
    min-height: 530px;
  }
  .device-columns {
    grid-template-columns: minmax(0, 1fr) minmax(104px, auto);
    gap: 6px 12px;
  }
  .table-heading {
    display: none;
  }
  .device-button {
    min-height: 126px;
    padding: 18px 20px;
    font-size: 17px;
  }
  .device-name {
    grid-column: 1;
    grid-row: 1;
  }
  .device-site {
    grid-column: 1;
    grid-row: 2;
    font-size: 14px;
  }
  .device-button .status {
    grid-column: 1;
    grid-row: 3;
    font-size: 14px;
  }
  .row-temperature {
    grid-column: 2;
    grid-row: 1 / 4;
    text-align: right;
    font-size: 24px;
  }
  .list-footer {
    padding: 16px 20px;
  }
  .detail-pane {
    padding: 20px;
  }
  .detail-heading {
    gap: 8px;
    flex-wrap: wrap;
  }
  .detail-heading h2 {
    font-size: 26px;
  }
  .detail-heading p {
    font-size: 15px;
  }
  .detail-heading .status {
    font-size: 14px;
  }
  .temperature-value {
    font-size: 74px;
  }
  .receipt-row {
    grid-template-columns: 1fr;
  }
  .simulation-actions {
    gap: 8px;
  }
  .simulation-actions .button {
    font-size: 14px;
    padding: 12px 5px;
  }
  .workspace-note {
    margin: 14px 4px 0;
    font-size: 12px;
  }
  .login-layout {
    grid-template-columns: 1fr;
  }
  .login-intro {
    padding: 25px 24px;
    gap: 24px;
  }
  .login-intro .brand {
    font-size: 22px;
  }
  .login-intro .brand svg {
    width: 27px;
    height: 27px;
  }
  .intro-copy h1 {
    margin-bottom: 12px;
    font-size: 27px;
    line-height: 1.5;
  }
  .intro-copy p {
    font-size: 14px;
  }
  .login-main {
    align-items: flex-start;
    padding: 30px 24px 40px;
  }
  .login-panel h2 {
    font-size: 29px;
  }
  .login-subtitle {
    margin: 10px 0 22px;
    font-size: 16px;
  }
  .login-panel label {
    font-size: 17px;
  }
  .login-panel input {
    min-height: 50px;
    font-size: 18px;
  }
  .login-panel .button {
    min-height: 50px;
    margin-top: 20px;
    font-size: 18px;
  }
  .login-note {
    font-size: 14px;
    margin-top: 24px;
  }
}
</style>
