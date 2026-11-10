<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

type Alert = {
  id: string
  deviceId: string
  type: 'temperature' | 'offline'
  status: 'active' | 'recovered' | 'closed'
  temperature?: number
  detectedAt: string
  recoveredAt: string | null
  deadlineAt?: string
  detectedAtRestart?: boolean
  closure?: { actor: string; closedAt: string; result: string }
}

type AlertPage = { items: Alert[]; total: number; limit: number; offset: number }

const props = defineProps<{
  api: {
    readApi: <T>(path: string, generation: number, controller: AbortController) => Promise<T>
    writeApi: <T>(path: string, init: RequestInit) => Promise<T>
    apiMessage: (error: unknown, fallback: string, notFound: string) => string
    expireSession: (generation: number) => void
    isCurrentIdentity: (generation: number) => boolean
    ApiError: new (status: number, code: string) => Error & { status: number; code: string }
    csrfHeaders: Record<string, string>
  }
  generation: number
  receiptTime: Intl.DateTimeFormat
  pageSize: number
}>()

const emit = defineEmits<{ pending: [pending: boolean] }>()
const { api, generation, pageSize } = props
const types = { temperature: '溫度異常', offline: '離線' }
const statuses = { active: '異常持續中', recovered: '已恢復、待結案', closed: '已結案' }
const notFound = '找不到這筆告警，或您沒有權限查看。'
const invalidResult = '請輸入 1～2,000 個字元的非空白純文字，並移除換行以外的控制字元。'
const heading = ref<HTMLElement | null>(null)
const resultInput = ref<HTMLTextAreaElement | null>(null)
const page = ref<AlertPage | null>(null)
const offset = ref(0)
const pageLoading = ref(false)
const pageError = ref('')
const selectedId = ref<string | null>(null)
const selectedAlert = ref<Alert | null>(null)
const detailLoading = ref(false)
const detailError = ref('')
const detailUnavailable = ref(false)
const draft = ref('')
const validationError = ref('')
const closureMessage = ref('')
const closurePending = ref(false)
const needsVerification = ref(false)

let readController: AbortController | null = null
let closeController: AbortController | null = null
let mounted = true

// 以 Unicode 碼位計數，讓非 BMP 字元也只算一個。
const resultLength = computed(() => [...draft.value].length)

// 在同一畫面內切換查詢時取消前一個讀取，並用控制器辨識回應歸屬。
function startRead(): AbortController {
  readController?.abort()
  readController = new AbortController()
  return readController
}

// 回應須同時屬於目前查詢及原登入，才可更新畫面。
function ownsRead(controller: AbortController): boolean {
  return readController === controller && api.isCurrentIdentity(generation)
}

// 結案結果只能更新發出寫入時的告警詳情。
function ownsAlert(id: string): boolean {
  return mounted
    && selectedId.value === id
    && api.isCurrentIdentity(generation)
}

// 讓每次切換清單或詳情後，鍵盤與讀屏使用者能從新標題開始。
async function focusHeading(controller?: AbortController): Promise<void> {
  await nextTick()
  if (controller && !ownsRead(controller)) return
  heading.value?.focus()
}

// 讀取伺服器已限制場域的告警頁，失敗時以相同偏移量提供重試。
async function loadPage(nextOffset: number): Promise<void> {
  if (
    pageLoading.value
    || selectedId.value
    || !api.isCurrentIdentity(generation)
  ) return
  const controller = startRead()
  const changingPage = nextOffset !== offset.value
  offset.value = nextOffset
  page.value = null
  pageError.value = ''
  pageLoading.value = true
  try {
    const loaded = await api.readApi<AlertPage>(`/api/alerts?limit=${pageSize}&offset=${nextOffset}`, generation, controller)
    if (!ownsRead(controller)) return
    page.value = loaded
    offset.value = loaded.offset
  } catch (error) {
    if (error instanceof api.ApiError && error.status === 401) api.expireSession(generation)
    else if (ownsRead(controller)) pageError.value = api.apiMessage(error, '告警歷程載入失敗，請重試。', notFound)
  } finally {
    if (ownsRead(controller)) {
      pageLoading.value = false
      // 換頁會移除原本的分頁按鈕，讀取結束後把鍵盤焦點移回歷程標題。
      if (changingPage) void focusHeading(controller)
    }
  }
}

// 詳情與結案共用同一筆告警的路徑。
function alertPath(id: string): string {
  return `/api/alerts/${encodeURIComponent(id)}`
}

// 暫停詳情與上層功能切換，直到這次結案或狀態確認完成。
function setClosurePending(pending: boolean): void {
  closurePending.value = pending
  emit('pending', pending)
}

// 已確認沒有權限或告警不存在時，撤下曾顯示的內容及草稿。
function hideAlert(error: unknown): void {
  selectedAlert.value = null
  detailUnavailable.value = true
  detailError.value = api.apiMessage(error, notFound, notFound)
  draft.value = ''
  validationError.value = ''
  closureMessage.value = ''
  needsVerification.value = false
}

// 查詢單筆告警；寫入結果不明時，只有讀回目前狀態才能解除重送限制。
async function loadAlert(): Promise<void> {
  const id = selectedId.value
  if (
    !id
    || detailLoading.value
    || !api.isCurrentIdentity(generation)
  ) return

  const controller = startRead()
  const verifying = needsVerification.value
  const manualVerification = verifying && !closurePending.value
  if (manualVerification) setClosurePending(true)
  detailLoading.value = true
  detailError.value = ''
  try {
    const loaded = await api.readApi<Alert>(alertPath(id), generation, controller)
    if (!ownsRead(controller) || !ownsAlert(id)) return
    if (loaded?.id !== id) throw new Error('Unexpected alert response')
    selectedAlert.value = loaded
    if (verifying) {
      // 讀回只證明目前的狀態，無法據此判定是誰的請求成功。
      needsVerification.value = false
      closureMessage.value = loaded.status === 'recovered'
        ? '告警目前仍待結案，請確認草稿後再送出。'
        : '已讀取告警目前的狀態。'
      if (loaded.status !== 'recovered') void focusHeading()
    }
  } catch (error) {
    if (error instanceof api.ApiError && error.status === 401) {
      api.expireSession(generation)
      return
    }
    if (!ownsRead(controller) || !ownsAlert(id)) return
    if (
      error instanceof api.ApiError
      && (
        error.status === 403
        || error.status === 404
      )
    ) {
      hideAlert(error)
    } else {
      detailError.value = verifying
        ? '仍無法確認告警狀態。請重新讀取告警，確認後才能再次結案。'
        : api.apiMessage(error, '告警詳情載入失敗，請重試。', notFound)
    }
  } finally {
    if (ownsRead(controller) && ownsAlert(id)) {
      detailLoading.value = false
      if (manualVerification) setClosurePending(false)
    }
  }
}

// 開啟告警後以單筆 API 重新確認權限與狀態，避免使用過時的清單資料結案。
function openAlert(id: string): void {
  if (pageLoading.value || selectedId.value) return
  selectedId.value = id
  void loadAlert()
  void focusHeading()
}

// 離開詳情時丟棄該筆草稿，以重新查詢的清單呈現最新狀態。
function returnToHistory(): void {
  if (closurePending.value) return
  selectedId.value = null
  selectedAlert.value = null
  detailLoading.value = false
  detailError.value = ''
  detailUnavailable.value = false
  draft.value = ''
  validationError.value = ''
  closureMessage.value = ''
  needsVerification.value = false
  void loadPage(offset.value)
  void focusHeading()
}

// 只提交已恢復告警的原始純文字；伺服器仍負責輸入與交易狀態驗證。
async function closeAlert(): Promise<void> {
  const alert = selectedAlert.value
  if (
    alert?.status !== 'recovered'
    || closurePending.value
    || detailLoading.value
    || needsVerification.value
    || !ownsAlert(alert.id)
  ) return

  validationError.value = ''
  closureMessage.value = ''
  const result = draft.value
  if (!/[^\p{White_Space}]/u.test(result) || resultLength.value > 2000) {
    validationError.value = invalidResult
    resultInput.value?.focus()
    return
  }

  const id = alert.id
  const controller = new AbortController()
  closeController = controller
  setClosurePending(true)
  try {
    const updated = await api.writeApi<Alert>(`${alertPath(id)}/close`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...api.csrfHeaders },
      body: JSON.stringify({ result }),
      signal: controller.signal,
    })
    if (!ownsAlert(id)) return
    // 只有原請求收到已提交的結案紀錄，才顯示送出成功。
    if (
      updated?.id !== id
      || updated.status !== 'closed'
      || !updated.closure
    ) throw new Error('Unexpected closure response')
    selectedAlert.value = updated
    draft.value = ''
    closureMessage.value = '已成功結案。'
    void focusHeading()
  } catch (error) {
    if (error instanceof api.ApiError && error.status === 401) {
      api.expireSession(generation)
      return
    }
    if (!ownsAlert(id)) return
    if (error instanceof api.ApiError) {
      if (error.status === 403 || error.status === 404) {
        hideAlert(error)
        return
      }
      if (error.status === 400) {
        validationError.value = invalidResult
        resultInput.value?.focus()
        return
      }
    }
    // 狀態衝突或結果未確認時，只讀回一次，並等待使用者決定下一步。
    needsVerification.value = true
    closureMessage.value = error instanceof api.ApiError && error.code === 'alert_conflict'
      ? '告警狀態已變更，正在重新讀取。'
      : '未能確認結案結果，正在重新讀取。'
    await loadAlert()
    if (ownsAlert(id) && needsVerification.value) closureMessage.value = ''
  } finally {
    if (closeController === controller) closeController = null
    if (ownsAlert(id)) setClosurePending(false)
  }
}

// 進入功能時讀取第一頁，讓登入後的設備頁不額外發出告警查詢。
onMounted(() => {
  void loadPage(0)
  void focusHeading()
})

// 登出或離開功能後讓未完成的回應失效，並取消仍在傳輸的請求。
onBeforeUnmount(() => {
  mounted = false
  readController?.abort()
  readController = null
  closeController?.abort()
})
</script>

<template>
  <section class="panel" aria-labelledby="alert-history-title">
    <template v-if="!selectedId">
      <div class="section-heading">
        <div>
          <h2 id="alert-history-title" ref="heading" tabindex="-1">告警歷程</h2>
          <p v-if="page" class="muted">共 {{ page.total }} 筆告警</p>
        </div>
        <button type="button" class="button button-secondary" :disabled="pageLoading" @click="loadPage(offset)">
          {{ pageLoading ? '載入中…' : '重新載入' }}
        </button>
      </div>

      <p v-if="pageLoading" role="status">正在載入告警歷程…</p>
      <p v-else-if="pageError" class="notice" role="alert">{{ pageError }}</p>
      <p v-else-if="!page?.items.length" class="muted">目前沒有可查看的告警。</p>
      <ul v-else class="device-list">
        <li v-for="alert in page.items" :key="alert.id">
          <button type="button" class="device-button alert-button" @click="openAlert(alert.id)">
            <span>#{{ alert.id }} · {{ types[alert.type] }} · {{ alert.deviceId }}</span>
            <span class="muted">{{ statuses[alert.status] }}</span>
          </button>
        </li>
      </ul>

      <nav v-if="page && page.total > page.limit" class="pagination alert-pagination" aria-label="告警歷程分頁">
        <button type="button" class="button button-secondary" :disabled="offset === 0" @click="loadPage(Math.max(0, offset - page.limit))">上一頁</button>
        <span aria-live="polite">第 {{ Math.floor(offset / page.limit) + 1 }} / {{ Math.ceil(page.total / page.limit) }} 頁</span>
        <button type="button" class="button button-secondary" :disabled="offset + page.limit >= page.total" @click="loadPage(offset + page.limit)">下一頁</button>
      </nav>
    </template>

    <template v-else>
      <button type="button" class="back-button" :disabled="closurePending" @click="returnToHistory">← 返回告警歷程</button>
      <h2 id="alert-history-title" ref="heading" tabindex="-1">告警詳情</h2>
      <p v-if="detailLoading" role="status">正在讀取告警…</p>
      <p v-else-if="closurePending" role="status">正在結案…</p>
      <p v-if="closureMessage" role="status">{{ closureMessage }}</p>
      <p v-if="detailError" class="notice" role="alert">{{ detailError }}</p>
      <button
        v-if="detailError && !detailUnavailable"
        type="button"
        class="button button-secondary"
        :disabled="closurePending || detailLoading"
        @click="loadAlert"
      >重新讀取告警</button>

      <dl v-if="selectedAlert" class="device-details">
        <div><dt>告警編號</dt><dd>{{ selectedAlert.id }}</dd></div>
        <div><dt>設備編號</dt><dd>{{ selectedAlert.deviceId }}</dd></div>
        <div><dt>類型</dt><dd>{{ types[selectedAlert.type] }}</dd></div>
        <div><dt>狀態</dt><dd>{{ statuses[selectedAlert.status] }}</dd></div>
        <div v-if="selectedAlert.temperature !== undefined"><dt>異常溫度</dt><dd>{{ selectedAlert.temperature }}°C</dd></div>
        <div v-if="selectedAlert.deadlineAt">
          <dt>離線期限</dt>
          <dd><time :datetime="selectedAlert.deadlineAt">{{ receiptTime.format(new Date(selectedAlert.deadlineAt)) }}</time></dd>
        </div>
        <div v-if="selectedAlert.detectedAtRestart !== undefined">
          <dt>重啟補判</dt><dd>{{ selectedAlert.detectedAtRestart ? '是' : '否' }}</dd>
        </div>
        <div>
          <dt>發現時間</dt>
          <dd><time :datetime="selectedAlert.detectedAt">{{ receiptTime.format(new Date(selectedAlert.detectedAt)) }}</time></dd>
        </div>
        <div>
          <dt>恢復時間</dt>
          <dd>
            <time v-if="selectedAlert.recoveredAt" :datetime="selectedAlert.recoveredAt">{{ receiptTime.format(new Date(selectedAlert.recoveredAt)) }}</time>
            <template v-else>—</template>
          </dd>
        </div>
        <template v-if="selectedAlert.closure">
          <div><dt>結案人</dt><dd>{{ selectedAlert.closure.actor }}</dd></div>
          <div>
            <dt>結案時間</dt>
            <dd><time :datetime="selectedAlert.closure.closedAt">{{ receiptTime.format(new Date(selectedAlert.closure.closedAt)) }}</time></dd>
          </div>
          <div><dt>處理結果</dt><dd class="closure-result">{{ selectedAlert.closure.result }}</dd></div>
        </template>
      </dl>

      <form v-if="selectedAlert?.status === 'recovered'" class="closure-form" :aria-busy="closurePending" @submit.prevent="closeAlert">
        <label for="closure-result">處理結果</label>
        <p id="closure-result-help" class="muted">輸入 1～2,000 個字元的非空白純文字，可換行。已輸入 {{ resultLength }} 個字元。</p>
        <textarea
          id="closure-result"
          ref="resultInput"
          v-model="draft"
          rows="5"
          required
          :aria-describedby="validationError ? 'closure-result-help closure-result-error' : 'closure-result-help'"
          :aria-invalid="validationError ? 'true' : undefined"
          :readonly="closurePending || needsVerification"
          @input="validationError = ''"
        ></textarea>
        <p v-if="validationError" id="closure-result-error" class="notice" role="alert">{{ validationError }}</p>
        <button
          type="submit"
          class="button"
          :disabled="
            closurePending
            || detailLoading
            || needsVerification
          "
        >結案</button>
      </form>
    </template>
  </section>
</template>

<style scoped>
.alert-button { overflow-wrap: anywhere; }
.alert-pagination { flex-wrap: wrap; }
.closure-form { margin-top: 1rem; }
.closure-form p { margin-bottom: 0; }
.closure-form button { justify-self: start; }
.closure-form textarea {
  width: 100%;
  padding: 0.6rem 0.75rem;
  border: 1px solid #aab8ad;
  border-radius: 0.5rem;
  color: inherit;
  font: inherit;
  resize: vertical;
}
.closure-form textarea:focus-visible { outline: 3px solid #45876b; outline-offset: 2px; }
.closure-result { white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
