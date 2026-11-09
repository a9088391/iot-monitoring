import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { copyFileSync, lstatSync, mkdirSync, mkdtempSync, readdirSync, realpathSync, rmSync, symlinkSync } from 'node:fs'
import { request } from 'node:http'
import { createServer } from 'node:net'
import { join } from 'node:path'
import { Readable } from 'node:stream'
import { fileURLToPath } from 'node:url'

const source = fileURLToPath(new URL('../', import.meta.url))

function copy(root, relative) {
  const from = join(source, relative)
  const to = join(root, relative)
  const stat = lstatSync(from)
  if (stat.isDirectory()) {
    mkdirSync(to)
    for (const name of readdirSync(from)) copy(root, join(relative, name))
    return
  }
  assert.ok(stat.isFile(), '測試來源只能是一般檔案')
  copyFileSync(from, to)
}

async function localPort() {
  const server = createServer()
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  const { port } = server.address()
  await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()))
  return port
}

// 只繼承工具執行所需的環境，並以同一個本機 socket 操作測試資源。
export async function createStack() {
  const suite = mkdtempSync(join(realpathSync('/tmp'), 'iot01-suite-'))
  const project = `iot01_test_${randomBytes(8).toString('hex')}`
  const root = join(suite, project)
  const alias = join(suite, 'alias')
  let owned = false
  let database
  let application
  // 直接操作 Compose 的命令也須沿用啟動時的資源標籤，避免觸發重建。
  const env = { PATH: process.env.PATH, HOME: process.env.HOME, TMPDIR: root, IOT_PROJECT_ROOT: root }
  const logs = []

  function run(command, args, phase, { input, expected = 0, timeout = 45_000, includeStderr = false } = {}) {
    const result = spawnSync(command, args, {
      cwd: root, env, input, timeout, encoding: 'utf8', maxBuffer: 4 * 1024 * 1024,
    })
    // CLI 或 SQL 的輸出可能包含秘密，只回報階段與結束狀態。
    assert.equal(result.error?.code ?? result.signal ?? result.status, expected, phase)
    return includeStderr ? result.stdout + result.stderr : result.stdout
  }

  function docker(args, phase, options) {
    return run('docker', args, phase, options)
  }

  const config = join(root, 'compose.yaml')
  const compose = (args, phase, options) =>
    docker(['compose', '-p', project, '-f', config, ...args], phase, options)

  function cleanup() {
    if (owned) {
      compose(['down', '--volumes', '--rmi', 'local', '--timeout', '10'], `清除測試專案 ${project}`)
      assertIsolated()
      owned = false
    }
    rmSync(suite, { recursive: true, force: true })
  }

  function assertIsolated() {
    const projectLabel = `label=com.docker.compose.project=${project}`
    for (const [kind, name] of [['container', project], ['volume', `${project}_data`], ['network', `${project}_default`]]) {
      const args = [kind, 'ls', '--quiet', ...(kind === 'container' ? ['--all'] : [])]
      for (const filter of [`name=${name}`, projectLabel]) {
        const output = docker([...args, '--filter', filter], '檢查專案資源')
        assert.ok(output.trim() === '', '測試專案名稱或資源已存在')
      }
    }
    const image = docker(['image', 'ls', '--quiet', '--filter', `reference=${project}-app`], '檢查專案映像')
    assert.ok(image.trim() === '', '測試專案映像已存在')
  }

  try {
    // 啟動器只允許測試在與 Compose 專案同名的暫存子目錄覆寫專案名稱。
    mkdirSync(root)
    symlinkSync(root, alias)
    mkdirSync(join(root, 'bin'))
    mkdirSync(join(root, 'frontend'))
    mkdirSync(join(root, 'backend'))
    for (const file of [
      '.dockerignore', 'compose.yaml', 'Dockerfile', 'bin/start',
      'frontend/index.html', 'frontend/package.json', 'frontend/package-lock.json',
      'frontend/tsconfig.json', 'frontend/vite.config.ts', 'frontend/src',
      ...readdirSync(join(source, 'backend')).filter(file => file.endsWith('.go') || file === 'go.mod' || file === 'go.sum')
        .map(file => `backend/${file}`),
    ]) copy(root, file)

    const context = docker(['context', 'show'], '查詢 Docker context').trim()
    const endpoint = docker(['context', 'inspect', context, '--format', '{{.Endpoints.docker.Host}}'], '檢查 Docker context').trim()
    assert.ok(endpoint.startsWith('unix:///'), '測試需要本機 Unix socket Docker context')
    env.DOCKER_HOST = endpoint
    assertIsolated()
    owned = true
    env.COMPOSE_PROJECT_NAME = project
    env.IOT_PORT = String(await localPort())
  } catch (error) {
    cleanup()
    throw error
  }

  const origin = `http://iot.localhost:${env.IOT_PORT}`
  const loopbackOrigin = `http://127.0.0.1:${env.IOT_PORT}`
  const { host } = new URL(origin)
  const state = join(root, '.local')

  async function send(path, { method, headers, body, chunks = [] } = {}) {
    const response = await new Promise((resolve, reject) => {
      // Node 直連 IPv4 並送出正式 Host，重啟後也使用新的連線。
      const req = request(loopbackOrigin + path, {
        method, signal: AbortSignal.timeout(6000), headers: {
          Host: host, Connection: 'close',
          ...(body === undefined ? {} : { 'Content-Length': Buffer.byteLength(body) }),
          ...headers,
        },
      }, resolve)
      req.once('error', reject)
      // 保留未知總長度的分段請求，與有 Content-Length 的本文分開測試。
      for (const chunk of chunks) req.write(chunk)
      req.end(body)
    })
    // 個別加入標頭值，保留每筆 Set-Cookie 供登入與登出檢查。
    const responseHeaders = new Headers()
    for (const [name, values] of Object.entries(response.headersDistinct)) {
      for (const value of values) responseHeaders.append(name, value)
    }
    const stream = response.statusCode === 204 ? null : Readable.toWeb(response)
    if (stream === null) response.resume()
    return new Response(stream, { status: response.statusCode, headers: responseHeaders })
  }

  function container(service) {
    const id = compose(['ps', '--quiet', service], '取得測試容器').trim()
    assert.ok(/^[a-f0-9]{12,64}$/.test(id), '測試容器必須唯一')
    const fields = '{{json .Config.Labels}}\n{{json .Mounts}}\n{{json .NetworkSettings.Ports}}\n{{.State.Health.Status}}'
    const [labels, mounts, ports, health] = docker(['inspect', '--format', fields, id], '檢查實際測試容器').trim().split('\n')
    const info = JSON.parse(labels)
    assert.ok(info['com.docker.compose.project'] === project &&
      info['com.docker.compose.project.config_files'] === config &&
      info['com.docker.compose.project.working_dir'] === root &&
      info['com.docker.compose.service'] === service && health === 'healthy', '容器身分或健康狀態不符')
    return { id, mounts: JSON.parse(mounts), ports: JSON.parse(ports) }
  }

  function start() {
    // 由符號連結啟動，並用下方的檢查確認實際 Compose 標籤與掛載皆使用實體路徑。
    const output = run('sh', [join(alias, 'bin/start')], '執行 bin/start', { timeout: 360_000, includeStderr: true })
    logs.push(output)
    assert.ok(output.includes(origin), '啟動顯示的網址須符合實際測試網址')
    const app = container('app')
    const db = container('db')
    assert.deepEqual(app.ports['8080/tcp'], [{ HostIp: '127.0.0.1', HostPort: env.IOT_PORT }])
    assert.ok(Object.keys(app.ports).length === 1 && Object.values(db.ports ?? {}).every(binding => !binding?.length),
      '只能公開應用程式的 loopback 連接埠')
    assert.ok(app.mounts.length === 1 && app.mounts.some(mount =>
      mount.Type === 'bind' && mount.Source === state && mount.Destination === '/state' && mount.RW), '應用程式狀態掛載不符')
    assert.ok(db.mounts.length === 2 && db.mounts.some(mount =>
      mount.Type === 'bind' && mount.Source === join(state, 'db-password') &&
      mount.Destination === '/run/secrets/iot-db-password' && !mount.RW), '資料庫憑證掛載不符')
    const volume = `${project}_data`
    assert.ok(db.mounts.some(mount => mount.Type === 'volume' && mount.Name === volume &&
      mount.Destination === '/var/lib/postgresql' && mount.RW), '資料庫資料掛載不符')
    const volumeLabels = JSON.parse(docker(['volume', 'inspect', '--format', '{{json .Labels}}', volume], '檢查測試資料卷'))
    assert.ok(volumeLabels['com.docker.compose.project'] === project &&
      volumeLabels['com.docker.compose.volume'] === 'data' &&
      volumeLabels['io.iot-monitoring.project-root'] === root, '資料卷不屬於測試專案')
    application = app.id
    database = db.id
  }

  function sql(input) {
    assert.ok(database, '尚未驗證資料庫隔離')
    return docker(['exec', '-i', database, 'psql', '-XwqAt', '-v', 'ON_ERROR_STOP=1', '-U', 'iot', '-d', 'iot'],
      '執行隔離 PostgreSQL 查詢', { input }).trim()
  }

  function collectLogs() {
    logs.push(compose(['logs', '--no-color'], '讀取測試容器日誌', { includeStderr: true }))
  }

  function secondInstance() {
    assert.ok(application && database, '尚未驗證容器隔離')
    const output = compose(['run', '--rm', '--no-deps', 'app'], '第二實例須結束並回傳 1', {
      expected: 1, timeout: 60_000, includeStderr: true,
    })
    logs.push(output)
    assert.ok(output.includes('IoT startup: another instance holds the PostgreSQL startup lock'), '第二實例須由 PostgreSQL 啟動鎖拒絕')
  }

  function stop() {
    collectLogs()
    compose(['stop', '--timeout', '10'], '停止測試專案')
    assert.ok(compose(['ps', '--quiet'], '確認測試專案已停止').trim() === '', '停機後不可有執行中容器')
    application = undefined
    database = undefined
  }

  return { origin, loopbackOrigin, state, send, start, sql, stop, cleanup, secondInstance, collectLogs, logs }
}
