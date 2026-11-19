import { computed, ref } from 'vue'
import { ApiError, apiMessage, csrfHeaders, isAbort, pageSize } from './api'
import type { DeviceDto, DevicePageDto, SimulationDto, SimulationRequestDto } from './monitoringDtos'
import { noReplayMessage, simulationActions } from './monitoringPresentation'
import type { MonitoringSessionAccess } from './useMonitoringSession'

// 設備清單、詳情與模擬版本一起切換，寫入回讀只影響原設備。
export function useDeviceMonitoring(session: MonitoringSessionAccess) {
  const { identity, readApi, writeApi, expireSession } = session
  const devices = ref<DeviceDto[]>([])
  const total = ref(0)
  const offset = ref(0)
  const devicesLoading = ref(false)
  const devicesError = ref('')
  const selectedDeviceId = ref<string | null>(null)
  const selectedDevice = ref<DeviceDto | null>(null)
  const detailLoading = ref(false)
  const detailError = ref('')
  const detailCanRetry = ref(false)
  const simulation = ref<SimulationDto | null>(null)
  const simulationLoading = ref(false)
  const simulationPending = ref(false)
  const simulationReadError = ref('')
  const simulationActionError = ref('')
  const simulationFeedback = ref('')
  let pageController: AbortController | null = null
  let detailController: AbortController | null = null
  // 由後端回傳的筆數與偏移量推導分頁資訊，空清單也保有第一頁。
  const pageCount = computed(() => Math.max(1, Math.ceil(total.value / pageSize)))
  const currentPage = computed(() => Math.floor(offset.value / pageSize) + 1)
  const deviceRange = computed(() => total.value === 0
    ? '共 0 台設備'
    : `${offset.value + 1}–${Math.min(offset.value + devices.value.length, total.value)} / ${total.value} 台設備`)
  const selectedTemperature = computed(() => selectedDevice.value?.latestMeasurement?.temperature)

  // 設備或模擬狀態尚在讀取，或指令仍在處理時，暫停新的模擬操作。
  const simulationBusy = computed(() =>
    detailLoading.value
    || simulationLoading.value
    || simulationPending.value)

  const detailReady = computed(() =>
    !devicesLoading.value
    && !simulationBusy.value
    && detailController !== null)

  // 身分清除時連同清單與控制版本撤下，取消清單與詳情以阻擋晚到回應。
  function clear(): void {
    pageController?.abort()
    pageController = null
    clearDetail()
    devices.value = []
    total.value = 0
    offset.value = 0
    devicesLoading.value = false
    devicesError.value = ''
  }

  // 明確進頁可取代舊手動讀取；背景同步仍讓位給目前的手動查詢。
  async function loadDevices(
    nextOffset: number,
    generation = session.currentGeneration(),
    passive?: AbortController,
  ): Promise<boolean> {
    if (
      !identity.value
      || generation !== session.currentGeneration()
      || (passive && devicesLoading.value)
    ) return false
    const controller = passive ?? new AbortController()
    if (!passive) {
      pageController?.abort()
      pageController = controller
      devicesLoading.value = true
      devicesError.value = ''
    }
    try {
      const page = await readApi<DevicePageDto>(
        `/api/devices?limit=${pageSize}&offset=${nextOffset}`,
        generation,
        controller,
      )
      if (
        generation !== session.currentGeneration()
        || controller.signal.aborted
        || (!passive && pageController !== controller)
      ) return false
      devices.value = page.items
      total.value = page.total
      offset.value = page.offset
      devicesError.value = ''
      return true
    } catch (error) {
      // 只有目前身分的查詢可清除登入狀態或顯示查詢錯誤。
      if (
        generation !== session.currentGeneration()
        || controller.signal.aborted
        || (!passive && pageController !== controller)
        || isAbort(error)
      ) return false
      if (error instanceof ApiError && error.status === 401) expireSession(generation)
      else if (!passive) devicesError.value = apiMessage(error, '設備清單載入失敗，請重試。')
      return false
    } finally {
      // 被新進頁取代的查詢不能清掉新查詢的 loading。
      if (
        !passive
        && generation === session.currentGeneration()
        && pageController === controller
      ) {
        pageController = null
        devicesLoading.value = false
      }
    }
  }

  // 桌面清單與詳情同時可見，沿用同一輪取消範圍依序同步兩者。
  async function refreshLive(generation: number, controller: AbortController): Promise<boolean> {
    const listLoaded = await loadDevices(offset.value, generation, controller)
    if (!listLoaded || controller.signal.aborted) return false
    if (!selectedDeviceId.value) return true
    const deviceLoaded = await refreshDevice(controller)
    if (!deviceLoaded || controller.signal.aborted) return false
    if (identity.value?.role !== 'headquarters') return true
    return loadSimulation(controller)
  }

  // 更新目前詳情的設備資料，只接受仍屬於這次詳情的結果。
  async function refreshDevice(passive?: AbortController): Promise<boolean> {
    const id = selectedDeviceId.value
    const controller = detailController
    const generation = session.currentGeneration()
    if (
      !id
      || !controller
      || detailLoading.value
    ) return false
    if (!passive) {
      detailError.value = ''
      detailCanRetry.value = false
      detailLoading.value = true
    }
    try {
      const device = await readApi<DeviceDto>(`/api/devices/${encodeURIComponent(id)}`, generation, passive ?? controller)
      if (detailController !== controller || passive?.signal.aborted) return false
      selectedDevice.value = device
      // 手動讀回也更新同畫面的清單列，不等待下一次串流通知。
      devices.value = devices.value.map(item => item.id === device.id ? device : item)
      detailError.value = ''
      detailCanRetry.value = false
      return true
    } catch (error) {
      // 詳情已切換或請求取消時，略過這次查詢的錯誤。
      if (
        detailController !== controller
        || passive?.signal.aborted
        || isAbort(error)
      ) return false
      if (error instanceof ApiError && error.status === 401) expireSession(generation)
      else {
        // 權限遭拒或設備已不存在時，清除先前顯示的設備資料。
        const unavailable = error instanceof ApiError
          && (
            error.status === 403
            || error.status === 404
          )
        if (unavailable) selectedDevice.value = null
        if (!passive || unavailable) {
          detailError.value = apiMessage(error, '設備資料載入失敗，請重試。')
          detailCanRetry.value = !(error instanceof ApiError && error.status === 404)
        }
      }
      return false
    } finally {
      if (!passive && detailController === controller) detailLoading.value = false
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
  async function loadSimulation(passive?: AbortController): Promise<boolean> {
    const device = selectedDevice.value
    const controller = detailController
    const generation = session.currentGeneration()
    if (
      !device
      || !controller
      || identity.value?.role !== 'headquarters'
      || simulationLoading.value
    ) return false
    if (!passive) {
      simulationReadError.value = ''
      simulationLoading.value = true
    }
    try {
      const state = await readApi<SimulationDto>(simulationPath(device.id), generation, passive ?? controller)
      if (detailController !== controller || passive?.signal.aborted) return false
      simulation.value = state
      simulationReadError.value = ''
      return true
    } catch (error) {
      // 保留目前詳情的查詢錯誤供重試，登入失效則交回共用清除流程。
      if (
        detailController !== controller
        || passive?.signal.aborted
        || isAbort(error)
      ) return false
      if (error instanceof ApiError && error.status === 401) expireSession(generation)
      else if (!passive) simulationReadError.value = apiMessage(error, '模擬狀態載入失敗，請重試。')
      return false
    } finally {
      if (!passive && detailController === controller) simulationLoading.value = false
    }
  }

  // 以畫面目前的控制版本送出模擬指令，結果只更新原本的設備詳情。
  async function changeSimulation(action: keyof typeof simulationActions): Promise<void> {
    const device = selectedDevice.value
    const state = simulation.value
    const controller = detailController
    const generation = session.currentGeneration()
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

    // 合法控制優先保留目前設備，避免較早的換頁回應移走清單。
    pageController?.abort()
    // 開始新的手動操作時，先清除上一筆結果與錯誤提示。
    simulationPending.value = true
    simulationActionError.value = ''
    simulationFeedback.value = ''
    try {
      try {
        const input: SimulationRequestDto = { action, version: state.version }
        const updated = await writeApi<SimulationDto>(simulationPath(device.id), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...csrfHeaders },
          body: JSON.stringify(input),
        }, generation)
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
    // 較晚的設備選取優先，取消尚未完成的清單讀取以保留可見頁。
    pageController?.abort()
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
  function clearDetail(): void {
    detailController?.abort()
    detailController = null
    selectedDeviceId.value = null
    selectedDevice.value = null
    detailLoading.value = false
    detailError.value = ''
    detailCanRetry.value = false
    clearSimulation()
  }

  return {
    devices, total, offset, devicesLoading, devicesError, selectedDeviceId, selectedDevice,
    detailLoading, detailError, detailCanRetry, simulation, simulationLoading, simulationPending,
    simulationReadError, simulationActionError, simulationFeedback, pageCount, currentPage,
    deviceRange, selectedTemperature, simulationBusy, detailReady,
    loadDevices, refreshLive, refreshDevice, loadSimulation, changeSimulation, loadDevice,
    clearDetail, clear,
  }
}
