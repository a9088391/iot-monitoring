// API 請求與回應只列出各操作的傳輸欄位，不暴露持久化模型。
export type IdentityDto = {
  username: string
  role: 'headquarters' | 'site'
  siteId: string | null
}

export type DeviceDto = {
  id: string
  name: string
  siteId: string
  siteName: string
  onlineStatus: 'unreported' | 'awaiting' | 'online' | 'offline'
  latestMeasurement?: {
    temperature: number
    receivedAt: string
  }
}

export type SimulationDto = {
  mode: 'stopped' | 'normal' | 'abnormal'
  version: string
  canReplay: boolean
}

export type DevicePageDto = {
  items: DeviceDto[]
  total: number
  limit: number
  offset: number
}

// 官方觀測以整筆資料傳遞，名稱、數值與量測時間不可跨設備拼接。
export type TaichungSensorDto = {
  id: string
  device: string
  name: string
  district: string
  landmark: string
  temperature: number
  humidity: number | null
  pm25: number | null
  measuredAt: string
}

export type TaichungSensorPageDto = {
  items: TaichungSensorDto[]
  total: number
  fetchedAt: string
  sourceUrl: string
}

export type AlertDto = {
  id: string
  deviceId: string
  type: 'temperature' | 'offline'
  status: 'active' | 'recovered' | 'closed'
  temperature?: number
  detectedAt: string
  recoveredAt: string | null
  deadlineAt?: string
  detectedAtRestart?: boolean
  closure?: {
    actor: string
    closedAt: string
    result: string
  }
}

export type AlertPageDto = {
  items: AlertDto[]
  total: number
  limit: number
  offset: number
}

export type LoginRequestDto = {
  username: string
  password: string
}

export type SimulationRequestDto = {
  action: 'normal' | 'abnormal' | 'stop' | 'replay'
  version: string
}

export type CloseAlertRequestDto = {
  result: string
}
