import { computed, onUnmounted, ref } from 'vue'
import { ApiError, apiMessage, isAbort } from './api'
import type { TaichungSensorPageDto } from './monitoringDtos'
import type { MonitoringSessionAccess } from './useMonitoringSession'

// 官方抽樣與同步共用一個請求所有權，避免舊快照覆蓋較新的手動重抽。
export function useTaichungMonitoring(session: MonitoringSessionAccess) {
  const snapshot = ref<TaichungSensorPageDto | null>(null)
  const selectedId = ref<string | null>(null)
  const loading = ref(false)
  const error = ref('')
  const selected = computed(() => snapshot.value?.items.find(item => item.id === selectedId.value) ?? null)
  let controller: AbortController | null = null
  let timer: ReturnType<typeof setTimeout> | null = null
  let active = false

  // 離頁、登出與卸載立即撤銷請求和計時器，下一次登入不沿用資料。
  function clear(): void {
    active = false
    controller?.abort()
    controller = null
    if (timer !== null) clearTimeout(timer)
    timer = null
    snapshot.value = null
    selectedId.value = null
    loading.value = false
    error.value = ''
  }

  // 背景只同步目前抽樣；空清單不自行補抽，留待使用者明確重試。
  function schedule(): void {
    if (timer !== null) clearTimeout(timer)
    if (!active || !snapshot.value?.items.length) return
    timer = setTimeout(() => {
      timer = null
      void load(false)
    }, 60_000)
  }

  // 明確重抽可取消任何舊讀取；套用前同時核對請求與登入世代。
  async function load(resample = true, generation = session.currentGeneration()): Promise<void> {
    if (!session.identity.value || !session.isCurrentIdentity(generation)) return
    active = true
    if (timer !== null) clearTimeout(timer)
    timer = null
    controller?.abort()
    const request = new AbortController()
    controller = request
    loading.value = true
    const ids = snapshot.value?.items.map(item => item.id) ?? []
    const query = !resample && ids.length ? `?ids=${encodeURIComponent(ids.join(','))}` : ''
    try {
      const page = await session.readApi<TaichungSensorPageDto>(`/api/taichung/sensors${query}`, generation, request)
      if (controller !== request || request.signal.aborted || !session.isCurrentIdentity(generation)) return
      snapshot.value = page
      error.value = ''
      if (resample || (selectedId.value !== null && !page.items.some(item => item.id === selectedId.value))) {
        selectedId.value = page.items[0]?.id ?? null
      }
    } catch (cause) {
      // 保留最後快照供辨識，但來源錯誤必須可見且不可冒充本次更新成功。
      if (controller !== request || request.signal.aborted || !session.isCurrentIdentity(generation) || isAbort(cause)) return
      if (cause instanceof ApiError && cause.status === 401) session.expireSession(generation)
      else error.value = apiMessage(cause, '官方資料載入失敗，請重試。')
    } finally {
      if (controller === request && session.isCurrentIdentity(generation)) {
        controller = null
        loading.value = false
        schedule()
      }
    }
  }

  onUnmounted(clear)
  return { snapshot, selectedId, selected, loading, error, load, clear }
}
