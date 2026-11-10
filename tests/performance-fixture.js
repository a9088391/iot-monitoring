import assert from 'node:assert/strict'
import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

// 只替換唯一的既有定錨，正式檔案改變時必須重新核對夾具。
function replace(text, before, after) {
  const offset = text.indexOf(before)
  assert.ok(offset >= 0 && offset === text.lastIndexOf(before), '效能夾具設定定錨不符')
  return text.slice(0, offset) + after + text.slice(offset + before.length)
}

// 只改隔離複本，沿用正式映像及 Vue 建置，並由 Go 入口另驗實際 PostgreSQL 標記。
export function preparePerformanceConfig(root, project) {
  assert.match(project, /^iot01_test_[0-9a-f]{16}$/)
  const composePath = join(root, 'compose.yaml')
  const dockerfilePath = join(root, 'Dockerfile')
  const cap = '    cpus: 2.0\n    mem_limit: 2147483648\n'
  const db = '    environment:\n      POSTGRES_DB: iot\n'
  const app = '    environment:\n      IOT_DB_HOST: db\n'
  const dbCommand = `    command: ["postgres", "-c", "iot.performance_fixture=${project}"]\n`
  const withDatabase = replace(readFileSync(composePath, 'utf8'), db, cap + dbCommand + db)
  writeFileSync(composePath, replace(withDatabase, app, cap + app + `      IOT_PERFORMANCE_FIXTURE: ${project}\n`))

  const build = 'RUN CGO_ENABLED=0 go build -trimpath -o /out/iot .\n'
  const entry = 'ENTRYPOINT ["/app/iot"]\n'
  const withTestBinary = replace(readFileSync(dockerfilePath, 'utf8'), build,
    'RUN CGO_ENABLED=0 go test -c -trimpath -o /out/iot .\n')
  writeFileSync(dockerfilePath, replace(withTestBinary, entry,
    'ENTRYPOINT ["/app/iot", "-test.run=^TestPerformanceStack$", "-test.timeout=12m"]\n'))
}
