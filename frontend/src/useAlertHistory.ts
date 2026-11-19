import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { ApiError, apiMessage, csrfHeaders, pageSize } from './api'
import type { AlertDto, AlertPageDto, CloseAlertRequestDto } from './monitoringDtos'
import { isValidClosureResult } from './monitoringRules'
import type { MonitoringSessionAccess } from './useMonitoringSession'

type AlertInteraction = {
  pending: (pending: boolean) => void
  focusHeading: (stillCurrent?: () => boolean) => Promise<void>
  focusResult: () => void
}

// 告警的讀取、草稿及結案回讀由同一實例持有，不跨頁或跨登入沿用。
export function useAlertHistory(api: MonitoringSessionAccess, interaction: AlertInteraction) {
  const generation = api.currentGeneration()
  const notFound = '找不到這筆告警，或您沒有權限查看。'
  const invalidResult = '請輸入 1～2,000 個字元的非空白純文字，並移除換行以外的控制字元。'
  const page = ref<AlertPageDto | null>(null)
  const offset = ref(0)
  const pageLoading = ref(false)
  const pageError = ref('')
  const selectedId = ref<string | null>(null)
  const selectedAlert = ref<AlertDto | null>(null)
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

  // 查詢範圍包含目前頁碼或告警編號，讓上層取消上一個範圍的被動讀取。
  const liveTarget = computed(() => selectedId.value ? `alert:${selectedId.value}` : `page:${offset.value}`)
  const liveReady = computed(() =>
    !pageLoading.value
    && !detailLoading.value
    && !closurePending.value
    && !needsVerification.value)

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

  // 讀取伺服器已限制場域的告警頁，失敗時以相同偏移量提供重試。
  async function loadPage(nextOffset: number, passive?: AbortController): Promise<boolean> {
    if (
      pageLoading.value
      || (selectedId.value && !passive)
      || !api.isCurrentIdentity(generation)
    ) return false
    const controller = passive ?? startRead()
    const changingPage = nextOffset !== offset.value
    if (!passive) {
      offset.value = nextOffset
      page.value = null
      pageError.value = ''
      pageLoading.value = true
    }
    try {
      const loaded = await api.readApi<AlertPageDto>(`/api/alerts?limit=${pageSize}&offset=${nextOffset}`, generation, controller)
      if (passive ? !ownsLiveRead(passive) : !ownsRead(controller)) return false
      page.value = loaded
      offset.value = loaded.offset
      pageError.value = ''
      return true
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) api.expireSession(generation)
      else if (!passive && ownsRead(controller)) pageError.value = apiMessage(error, '告警歷程載入失敗，請重試。', notFound)
      return false
    } finally {
      if (!passive && ownsRead(controller)) {
        pageLoading.value = false
        // 換頁會移除原分頁按鈕，讀取結束後把焦點移回告警監控區塊。
        if (changingPage) void interaction.focusHeading(() => ownsRead(controller))
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
    interaction.pending(pending)
  }

  // 已確認沒有權限或告警不存在時，撤下曾顯示的內容及草稿。
  function hideAlert(error: unknown): void {
    selectedAlert.value = null
    detailUnavailable.value = true
    detailError.value = apiMessage(error, notFound, notFound)
    draft.value = ''
    validationError.value = ''
    closureMessage.value = ''
    needsVerification.value = false
  }

  // 查詢單筆告警；寫入結果不明時，只有讀回目前狀態才能解除重送限制。
  async function loadAlert(passive?: AbortController): Promise<boolean> {
    const id = selectedId.value
    if (
      !id
      || detailLoading.value
      || !api.isCurrentIdentity(generation)
    ) return false

    const controller = passive ?? startRead()
    const verifying = needsVerification.value
    const manualVerification = verifying && !closurePending.value
    if (manualVerification) setClosurePending(true)
    if (!passive) {
      detailLoading.value = true
      detailError.value = ''
    }
    try {
      const loaded = await api.readApi<AlertDto>(alertPath(id), generation, controller)
      if (passive ? !ownsLiveRead(passive) : !ownsRead(controller)) return false
      if (!ownsAlert(id)) return false
      if (loaded?.id !== id) throw new Error('Unexpected alert response')
      selectedAlert.value = loaded
      if (page.value) page.value.items = page.value.items.map(item => item.id === loaded.id ? loaded : item)
      detailError.value = ''
      detailUnavailable.value = false
      if (verifying) {
        // 讀回只證明目前的狀態，無法據此判定是誰的請求成功。
        needsVerification.value = false
        closureMessage.value = loaded.status === 'recovered'
          ? '告警目前仍待結案，請確認草稿後再送出。'
          : '已讀取告警目前的狀態。'
        if (loaded.status !== 'recovered') void interaction.focusHeading()
      }
      return true
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        api.expireSession(generation)
        return false
      }
      if (passive ? !ownsLiveRead(passive) : !ownsRead(controller)) return false
      if (!ownsAlert(id)) return false
      if (
        error instanceof ApiError
        && (
          error.status === 403
          || error.status === 404
        )
      ) {
        hideAlert(error)
      } else if (!passive) {
        detailError.value = verifying
          ? '仍無法確認告警狀態。請重新讀取告警，確認後才能再次結案。'
          : apiMessage(error, '告警詳情載入失敗，請重試。', notFound)
      }
      return false
    } finally {
      if (
        !passive
        && ownsRead(controller)
        && ownsAlert(id)
      ) {
        detailLoading.value = false
        if (manualVerification) setClosurePending(false)
      }
    }
  }

  // 被動讀取離開原範圍或碰到手動操作後，即使回應已在傳輸中也不得套用。
  function ownsLiveRead(controller: AbortController): boolean {
    return mounted
      && !controller.signal.aborted
      && api.isCurrentIdentity(generation)
  }

  // 同輪同步並列的清單與詳情，不清除草稿或手動結案確認狀態。
  async function refreshLive(controller: AbortController): Promise<boolean> {
    if (!liveReady.value || !ownsLiveRead(controller)) return false
    const pageLoaded = await loadPage(offset.value, controller)
    if (!pageLoaded || !ownsLiveRead(controller)) return false
    if (selectedId.value) return loadAlert(controller)
    return true
  }

  // 頁首重新載入沿用目前清單或詳情的手動讀取入口。
  async function reload(): Promise<void> {
    if (!liveReady.value) return
    if (selectedId.value) await loadAlert()
    else await loadPage(offset.value)
  }

  // 開啟告警後以單筆 API 重新確認權限與狀態，避免使用過時的清單資料結案。
  function openAlert(id: string): void {
    if (
      pageLoading.value
      || detailLoading.value
      || closurePending.value
      || needsVerification.value
      || selectedId.value === id
    ) return
    // 改選等同先返回再開啟；寫入及結果未確認時禁止改選，以保留原草稿。
    selectedAlert.value = null
    detailError.value = ''
    detailUnavailable.value = false
    draft.value = ''
    validationError.value = ''
    closureMessage.value = ''
    selectedId.value = id
    void loadAlert()
    void interaction.focusHeading()
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
    void interaction.focusHeading()
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
    if (!isValidClosureResult(result)) {
      validationError.value = invalidResult
      interaction.focusResult()
      return
    }

    const id = alert.id
    const controller = new AbortController()
    closeController = controller
    setClosurePending(true)
    try {
      const input: CloseAlertRequestDto = { result }
      const updated = await api.writeApi<AlertDto>(`${alertPath(id)}/close`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...csrfHeaders },
        body: JSON.stringify(input),
        signal: controller.signal,
      }, generation)
      if (!ownsAlert(id)) return
      // 只有原請求收到已提交的結案紀錄，才顯示送出成功。
      if (
        updated?.id !== id
        || updated.status !== 'closed'
        || !updated.closure
      ) throw new Error('Unexpected closure response')
      selectedAlert.value = updated
      if (page.value) page.value.items = page.value.items.map(item => item.id === updated.id ? updated : item)
      draft.value = ''
      closureMessage.value = '已成功結案。'
      void interaction.focusHeading()
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        api.expireSession(generation)
        return
      }
      if (!ownsAlert(id)) return
      if (error instanceof ApiError) {
        if (error.status === 403 || error.status === 404) {
          hideAlert(error)
          return
        }
        if (error.status === 400) {
          validationError.value = invalidResult
          interaction.focusResult()
          return
        }
      }
      // 狀態衝突或結果未確認時，只讀回一次，並等待使用者決定下一步。
      needsVerification.value = true
      closureMessage.value = error instanceof ApiError && error.code === 'alert_conflict'
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
    void interaction.focusHeading()
  })

  // 登出或離開功能後讓未完成的回應失效，並取消仍在傳輸的請求。
  onBeforeUnmount(() => {
    mounted = false
    readController?.abort()
    readController = null
    closeController?.abort()
  })

  return {
    page, offset, pageLoading, pageError, selectedId, selectedAlert, detailLoading,
    detailError, detailUnavailable, draft, validationError, closureMessage,
    closurePending, needsVerification, resultLength, liveTarget, liveReady,
    loadPage, loadAlert, refreshLive, reload, openAlert, returnToHistory, closeAlert,
  }
}
