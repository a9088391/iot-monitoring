export const onlineStatuses = { unreported: '尚未回報', awaiting: '等待首筆回報', online: '在線', offline: '離線' }
// 回報時間固定以臺灣時區顯示，避免受瀏覽器所在時區影響。
export const receiptTime = new Intl.DateTimeFormat('zh-TW', {
  timeZone: 'Asia/Taipei',
  dateStyle: 'short',
  timeStyle: 'medium',
  hourCycle: 'h23',
})
export const simulationModes = { stopped: '已停止', normal: '正常回報', abnormal: '異常回報' }
export const simulationActions = {
  normal: { label: '恢復正常', success: '已恢復正常回報。' },
  abnormal: { label: '開始異常回報', success: '已開始異常回報。' },
  stop: { label: '停止回報', success: '已停止模擬回報。' },
  replay: { label: '重複回報', success: '已重送最近一筆原始測量。' },
}
export const noReplayMessage = '尚無可重送的測量。'

// 整數補一位小數供展示，其他有效測量保留原精度。
export function formatTemperature(temperature: number): string {
  return Number.isInteger(temperature) ? temperature.toFixed(1) : String(temperature)
}

export const alertTypes = { temperature: '溫度異常', offline: '離線' }
export const alertStatuses = { active: '異常持續中', recovered: '已恢復、待結案', closed: '已結案' }
