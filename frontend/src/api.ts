import type { IdentityDto } from './monitoringDtos'
import { parseSessionExpiry } from './live'

export type SessionSnapshot = {
  identity: IdentityDto
  viewer: string
  expiresAt: number
}

export class ApiError extends Error {
  // 保留 HTTP 狀態和 API 錯誤代碼，讓各操作能分辨登入失效、拒絕和暫時故障。
  constructor(readonly status: number, readonly code: string) {
    super(code)
    this.name = 'ApiError'
  }
}

export const pageSize = 50
export const csrfHeaders = { 'X-CSRF-Protection': '1' }
const sessionViewHeader = 'X-IOT-Session-View'
const sessionExpiryHeader = 'X-IOT-Session-Expires-At'

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
export async function responseData<T>(response: Response): Promise<T> {
  if (!response.ok) {
    // 錯誤回應即使沒有可解析的 JSON，也保留原本的 HTTP 狀態。
    const body: unknown = await response.json().catch(() => null)
    throw new ApiError(response.status, errorCode(body))
  }
  // 例如登出成功時沒有回應本文，直接完成即可。
  if (response.status === 204) return undefined as T
  return await response.json() as T
}

// 以同一個成功回應建立身分與期限，避免第二次請求取到另一個分頁換發的 Cookie。
export async function sessionData(response: Response): Promise<SessionSnapshot> {
  const identity = await responseData<IdentityDto>(response)
  const viewer = response.headers.get(sessionViewHeader)
  const expiresAt = parseSessionExpiry(response.headers.get(sessionExpiryHeader))
  if (
    !viewer
    || !/^[A-Za-z0-9_-]{43}$/.test(viewer)
    || !Number.isFinite(expiresAt)
  ) throw new Error('Invalid session metadata')
  return { identity, viewer, expiresAt }
}

// viewer 只是原登入的比對值，實際授權仍由同源 Cookie 提供。
export function request(path: string, init: RequestInit, viewer?: string): Promise<Response> {
  const headers = new Headers(init.headers)
  if (viewer) headers.set(sessionViewHeader, viewer)
  return fetch(path, { ...init, headers, credentials: 'same-origin' })
}

// 將已取消或已被取代的查詢視為流程結束，避免畫面顯示載入失敗。
export function isAbort(error: unknown): boolean {
  return error instanceof Error && error.name === 'AbortError'
}

// 將可辨識的 API 錯誤轉為操作提示，其餘保留呼叫端提供的情境訊息。
export function apiMessage(error: unknown, fallback: string, notFound = '找不到這台設備，或您沒有權限查看。'): string {
  if (!(error instanceof ApiError)) return fallback
  if (error.status === 403 || error.code === 'forbidden') return '此操作未獲允許。'
  if (error.status === 404 || error.code === 'not_found') return notFound
  if (error.status === 429 || error.code === 'rate_limited') return '登入嘗試次數已達限制，請稍後再試。'
  if (error.status === 503 || error.code === 'service_unavailable') return '服務暫時無法使用，請稍後重試。'
  return fallback
}
