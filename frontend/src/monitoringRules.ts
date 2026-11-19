// 正常區間含上下限，離線時仍只描述最後有效測量。
export function isAbnormal(temperature?: number): boolean {
  return temperature !== undefined && (temperature < 18 || temperature > 28)
}

// 只做送出前的非空白與 Unicode 碼位檢查；完整純文字驗證仍由伺服器負責。
export function isValidClosureResult(result: string): boolean {
  return /[^\p{White_Space}]/u.test(result) && [...result].length <= 2000
}
