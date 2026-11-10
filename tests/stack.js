import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { EventEmitter, once } from 'node:events'
import { chmodSync, copyFileSync, lstatSync, mkdirSync, mkdtempSync, readdirSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { request } from 'node:http'
import { createServer } from 'node:net'
import { join } from 'node:path'
import { Readable } from 'node:stream'
import { fileURLToPath } from 'node:url'
import { preparePerformanceConfig } from './performance-fixture.js'

const source = fileURLToPath(new URL('../', import.meta.url))

// 複製容器建置需要的來源，拒絕符號連結等特殊檔案以免跨出測試來源。
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

// 讓系統挑出可用的 IPv4 loopback 埠，關閉探測 socket 後交給 Compose 綁定。
async function localPort() {
  const server = createServer()
  // 等待實際開始監聽後再讀取埠號，並把綁定失敗交回呼叫端。
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  const { port } = server.address()
  // 關閉探測用 socket 後再交付埠號，並傳回關閉失敗。
  await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()))
  return port
}

// 只繼承工具執行所需的環境，並以同一個本機 socket 操作測試資源。
export async function createStack({ performance = false } = {}) {
  // 每套測試各自配置專案名稱與暫存根目錄，隔離資料卷和應用程式。
  const suite = mkdtempSync(join(realpathSync('/tmp'), 'iot01-suite-'))
  const project = `iot01_test_${randomBytes(8).toString('hex')}`
  const root = join(suite, project)
  const alias = join(suite, 'alias')
  let owned = false
  let database
  let application
  let performanceRuntime
  // 直接操作 Compose 的命令也須沿用啟動時的資源標籤，避免觸發重建。
  const env = { PATH: process.env.PATH, HOME: process.env.HOME, TMPDIR: root, IOT_PROJECT_ROOT: root }
  const logs = []

  // 用固定目錄和環境執行有期限的子程序，將敏感輸出留給測試比對。
  function run(command, args, phase, {
    input, expected = 0, timeout = 45_000, includeStderr = false, commandEnv = env,
  } = {}) {
    const result = spawnSync(command, args, {
      cwd: root, env: commandEnv, input, timeout, encoding: 'utf8', maxBuffer: 4 * 1024 * 1024,
    })
    // CLI 或 SQL 的輸出可能包含秘密，只回報階段與結束狀態。
    assert.equal(
      result.error?.code
        ?? result.signal
        ?? result.status,
      expected, phase
    )
    return includeStderr ? result.stdout + result.stderr : result.stdout
  }

  // 統一經過隔離的子程序環境呼叫 Docker，避免繼承外部設定。
  function docker(args, phase, options) {
    return run('docker', args, phase, options)
  }

  const config = join(root, 'compose.yaml')
  // 將 Compose 操作固定到本次專案名稱與設定檔，避免操作其他專案。
  const compose = (args, phase, options) =>
    docker(['compose', '-p', project, '-f', config, ...args], phase, options)

  // 只清理已取得歸屬的 Compose 資源；證據複製失敗時保留私有暫存原件。
  function cleanup({ retainSuite = false } = {}) {
    if (owned) {
      compose(['down', '--volumes', '--rmi', 'local', '--timeout', '10'], `清除測試專案 ${project}`)
      assertIsolated()
      owned = false
    }
    if (!retainSuite) rmSync(suite, { recursive: true, force: true })
  }

  // 依資源名稱與 Compose 標籤分別查找，避免碰撞，也能發現清理殘留。
  function assertIsolated() {
    const projectLabel = `label=com.docker.compose.project=${project}`
    for (const [kind, name] of [['container', project], ['volume', `${project}_data`], ['network', `${project}_default`]]) {
      const args = [kind, 'ls', '--quiet', ...(kind === 'container' ? ['--all'] : [])]
      for (const filter of [`name=${name}`, projectLabel]) {
        const output = docker([...args, '--filter', filter], '檢查專案資源')
        assert.ok(output.trim() === '', '測試專案名稱或資源已存在')
      }
    }
    // 映像以 Compose 預期的應用名稱查找，和容器、網路及資料卷分開檢查。
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
    // 只複製建置所需檔案；後端來源限定 Go 程式與模組鎖定資訊。
    for (const file of [
      '.dockerignore', 'compose.yaml', 'Dockerfile', 'bin/start', 'bin/reset',
      'frontend/index.html', 'frontend/package.json', 'frontend/package-lock.json',
      'frontend/tsconfig.json', 'frontend/vite.config.ts', 'frontend/src',
      ...readdirSync(join(source, 'backend'))
        // 只選 Go 原始碼及模組資訊，避免帶入其他本機檔案。
        .filter(file =>
          file.endsWith('.go')
          || file === 'go.mod'
          || file === 'go.sum')
        // 補上專案相對路徑，交給前面同一個複製流程。
        .map(file => `backend/${file}`),
    ]) copy(root, file)
    // 重設入口會直接執行啟動器，暫存複本也須保有執行權限。
    chmodSync(join(root, 'bin/start'), 0o700)
    if (performance) preparePerformanceConfig(root, project)

    // 驗證端點確實是本機 socket，再確認名稱無衝突，才取得清理權。
    const context = docker(['context', 'show'], '查詢 Docker context').trim()
    const endpoint = docker(['context', 'inspect', context, '--format', '{{.Endpoints.docker.Host}}'], '檢查 Docker context').trim()
    assert.ok(endpoint.startsWith('unix:///'), '測試需要本機 Unix socket Docker context')
    env.DOCKER_HOST = endpoint
    assertIsolated()
    owned = true
    env.COMPOSE_PROJECT_NAME = project
    env.IOT_PORT = String(await localPort())
  } catch (error) {
    // 準備階段失敗也走同一清理流程，是否回收 Docker 資源依歸屬旗標判定。
    cleanup()
    throw error
  }

  const origin = `http://iot.localhost:${env.IOT_PORT}`
  const loopbackOrigin = `http://127.0.0.1:${env.IOT_PORT}`
  const { host } = new URL(origin)
  const state = join(root, '.local')

  // 從隔離專案送出真實 HTTP 請求，保留 Cookie 與本文分段等傳輸差異。
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
    // 204 使用無本文回應並讀完底層串流，其他回應則保留可讀本文。
    const stream = response.statusCode === 204 ? null : Readable.toWeb(response)
    if (stream === null) response.resume()
    return new Response(stream, { status: response.statusCode, headers: responseHeaders })
  }

  // 取得單一健康容器，並驗證實際 Compose 標籤後才交付容器身分。
  function container(service) {
    const id = compose(['ps', '--quiet', service], '取得測試容器').trim()
    assert.ok(/^[a-f0-9]{12,64}$/.test(id), '測試容器必須唯一')
    // 一次取回標籤、掛載、連接埠及健康資訊，供歸屬與隔離檢查。
    const fields = '{{json .Config.Labels}}\n{{json .Mounts}}\n{{json .NetworkSettings.Ports}}\n{{.State.Health.Status}}'
    const [labels, mounts, ports, health] = docker(['inspect', '--format', fields, id], '檢查實際測試容器')
      .trim()
      .split('\n')
    const info = JSON.parse(labels)
    assert.ok(
      info['com.docker.compose.project'] === project
        && info['com.docker.compose.project.config_files'] === config
        && info['com.docker.compose.project.working_dir'] === root
        && info['com.docker.compose.service'] === service
        && health === 'healthy',
      '容器身分或健康狀態不符'
    )
    return { id, mounts: JSON.parse(mounts), ports: JSON.parse(ports) }
  }

  // 由正式入口啟動，核對容器歸屬、健康狀態、主機埠、掛載及資料卷後才保存容器識別。
  function start() {
    // 由符號連結啟動，並用下方的檢查確認實際 Compose 標籤與掛載皆使用實體路徑。
    const output = run('sh', [join(alias, 'bin/start')], '執行 bin/start', { timeout: 360_000, includeStderr: true })
    logs.push(output)
    assert.ok(output.includes(origin), '啟動顯示的網址須符合實際測試網址')
    // 只允許應用程式綁定 IPv4 loopback，資料庫不得發佈主機連接埠。
    const app = container('app')
    const db = container('db')
    assert.deepEqual(app.ports['8080/tcp'], [{ HostIp: '127.0.0.1', HostPort: env.IOT_PORT }])
    // 資料庫的宣告埠可以存在，但每筆都必須沒有對外綁定。
    assert.ok(Object.keys(app.ports).length === 1 && Object.values(db.ports ?? {}).every(binding => !binding?.length),
      '只能公開應用程式的 loopback 連接埠')
    // 應用程式只掛載這次測試的可寫狀態目錄。
    assert.ok(app.mounts.length === 1 && app.mounts.some(mount =>
      mount.Type === 'bind'
      && mount.Source === state
      && mount.Destination === '/state'
      && mount.RW), '應用程式狀態掛載不符')
    // 資料庫的憑證掛載必須來自同一目錄並保持唯讀。
    assert.ok(db.mounts.length === 2 && db.mounts.some(mount =>
      mount.Type === 'bind'
      && mount.Source === join(state, 'db-password')
      && mount.Destination === '/run/secrets/iot-db-password'
      && !mount.RW), '資料庫憑證掛載不符')
    const volume = `${project}_data`
    // PostgreSQL 只將資料寫入對應測試專案的資料卷。
    assert.ok(db.mounts.some(mount =>
      mount.Type === 'volume'
      && mount.Name === volume
      && mount.Destination === '/var/lib/postgresql'
      && mount.RW), '資料庫資料掛載不符')
    // 除了掛載名稱，也查核資料卷標籤，避免把同名資源當成本案資料。
    const volumeLabels = JSON.parse(docker(['volume', 'inspect', '--format', '{{json .Labels}}', volume], '檢查測試資料卷'))
    assert.ok(
      volumeLabels['com.docker.compose.project'] === project
        && volumeLabels['com.docker.compose.volume'] === 'data'
        && volumeLabels['io.iot-monitoring.project-root'] === root,
      '資料卷不屬於測試專案'
    )
    // 所有隔離檢查通過後才保存容器識別，讓 SQL 只能操作已驗證目標。
    application = app.id
    database = db.id
    if (performance) {
      performanceRuntime = {
        namespace: project,
        daemon: JSON.parse(docker(['version', '--format', '{{json .Server}}'], '記錄隔離 Docker 版本')),
        app: performanceQuota(app.id),
        db: performanceQuota(db.id),
      }
    }
  }

  // 驗證實際容器設定及執行中的 cgroup 上限，避免用宣告值充當量測環境。
  function performanceQuota(id) {
    const configured = docker(['container', 'inspect', '--format', '{{.HostConfig.NanoCpus}}|{{.HostConfig.Memory}}', id],
      '確認實際容器配額').trim()
    assert.equal(configured, '2000000000|2147483648', '單一容器上限須為 2 vCPU 與 2 GiB')
    const cgroup = docker(['exec', id, 'cat', '/sys/fs/cgroup/cpu.max', '/sys/fs/cgroup/memory.max'],
      '確認執行中容器配額').trim()
    const [quota, period, memory] = cgroup.split(/\s+/).map(Number)
    assert.equal(quota / period, 2, '實際 cgroup CPU 上限須為 2 vCPU')
    assert.equal(memory, 2147483648, '實際 cgroup 記憶體上限須為 2 GiB')
    return { id, configured, cgroup }
  }

  // 以已驗證的容器執行 SQL，任何 SQL 錯誤都須使子程序失敗。
  function sql(input) {
    assert.ok(database, '尚未驗證資料庫隔離')
    return docker(['exec', '-i', database, 'psql', '-XwqAt', '-v', 'ON_ERROR_STOP=1', '-U', 'iot', '-d', 'iot'],
      '執行隔離 PostgreSQL 查詢', { input }).trim()
  }

  // 只觀察原先通過隔離檢查的容器與資料卷，連停止狀態也可辨識。
  function resetResources() {
    assert.ok(
      owned
        && application
        && database,
      '尚未驗證重設資源隔離',
    )
    // 容器識別與啟動時間可分辨重啟、重建與從未停止。
    function instance(id) {
      const fields = '{{.Id}}|{{.State.Running}}|{{.State.StartedAt}}'
      const [actualId, running, startedAt] = docker(['container', 'inspect', '--format', fields, id], '檢查原重設容器')
        .trim()
        .split('|')
      return { id: actualId, running: running === 'true', startedAt }
    }
    const name = `${project}_data`
    const createdAt = docker(['volume', 'inspect', '--format', '{{.CreatedAt}}', name], '檢查原重設資料卷').trim()
    return { app: instance(application), db: instance(database), volume: { name, createdAt } }
  }

  // 只從隔離別名執行正式重設；錯誤端點固定為保留的無效 SSH 網域。
  function reset(input, expected = 0, { remoteTarget = false } = {}) {
    assert.ok(
      owned
        && application
        && database,
      '尚未驗證重設資源隔離',
    )
    const commandEnv = remoteTarget ? { ...env, DOCKER_HOST: 'ssh://docker.example.invalid' } : env
    const output = run('sh', [join(alias, 'bin/reset')], '執行 bin/reset', {
      input, expected, timeout: 120_000, includeStderr: true, commandEnv,
    })
    logs.push(output)
    return output
  }

  // 暫停已確認的第一個重設，只在已驗證容器即將收到實際 stop 時讓另一個指令進場。
  async function resetWithStopBarrier(whilePaused) {
    const app = resetResources().app
    assert.ok(app.running, '互斥驗證需要正在執行的本案應用')
    const directory = mkdtempSync(join(suite, 'reset-barrier-'))
    const realDocker = realpathSync(run('sh', ['-c', 'command -v docker'], '定位真實 Docker').trim())
    const marker = 'iot reset fixture before stop'
    // 只暫停特定 socket 與容器的 stop；控制管道中斷時不執行這次停止。
    writeFileSync(join(directory, 'docker'), `#!/bin/sh
if [ "$#" = 6 ] &&
    [ "$1" = --host ] &&
    [ "$2" = "$IOT_FIXTURE_SOCKET" ] &&
    [ "$3" = stop ] &&
    [ "$4" = --time ] &&
    [ "$5" = 10 ] &&
    [ "$6" = "$IOT_FIXTURE_APP" ]; then
    printf '%s\\n' '${marker}' >&2
    IFS= read -r proceed <&3 || exit 1
    [ "$proceed" = continue ] || exit 1
fi
exec "$IOT_FIXTURE_DOCKER" "$@"
`, { mode: 0o700 })
    const child = spawn('sh', [join(alias, 'bin/reset')], {
      cwd: root, timeout: 120_000, stdio: ['pipe', 'pipe', 'pipe', 'pipe'],
      env: {
        ...env, PATH: `${directory}:${env.PATH}`,
        IOT_FIXTURE_DOCKER: realDocker, IOT_FIXTURE_SOCKET: env.DOCKER_HOST, IOT_FIXTURE_APP: app.id,
      },
    })
    const barrier = new EventEmitter()
    const gate = child.stdio[3]
    let stdout = ''
    let stderr = ''
    let controlError
    // 保留控制管道錯誤供成功路徑檢查，仍等子程序結束後才回收隔離資源。
    for (const pipe of [child.stdin, gate]) {
      pipe.on('error', error => { controlError ??= error.code })
    }
    child.stdout.setEncoding('utf8').on('data', chunk => { stdout += chunk })
    child.stderr.setEncoding('utf8').on('data', chunk => {
      stderr += chunk
      if (stderr.includes(marker)) barrier.emit('ready')
    })
    const finished = new Promise(resolve => {
      child.once('error', error => resolve(error.code))
      child.once('close', (code, signal) => resolve(signal ?? code))
    })
    child.stdin.end(`RESET ${project}\n`)
    let status
    try {
      await Promise.race([
        once(barrier, 'ready', { signal: AbortSignal.timeout(30_000) }),
        finished.then(() => assert.fail('重設在到達停止屏障前結束')),
      ])
      await whilePaused()
      gate.end('continue\n')
    } finally {
      if (!gate.writableEnded) gate.end()
      status = await finished
      logs.push(stdout + stderr)
    }
    assert.equal(controlError ?? status, 0, '屏障放行後原指令應完成重設')
    return stdout + stderr
  }

  // 保留容器輸出供敏感值驗收，避免把日誌直接印進測試報告。
  function collectLogs() {
    logs.push(compose(['logs', '--no-color'], '讀取測試容器日誌', { includeStderr: true }))
  }

  // 啟動同專案的第二個應用程序，以指定診斷確認失敗來自 PostgreSQL 啟動鎖。
  function secondInstance() {
    assert.ok(application && database, '尚未驗證容器隔離')
    const output = compose(['run', '--rm', '--no-deps', 'app'], '第二實例須結束並回傳 1', {
      expected: 1, timeout: 60_000, includeStderr: true,
    })
    logs.push(output)
    assert.ok(output.includes('IoT startup: another instance holds the PostgreSQL startup lock'), '第二實例須由 PostgreSQL 啟動鎖拒絕')
  }

  // 保存日誌並確認停機後清除本地容器識別快取，讓重啟重新驗證容器歸屬與健康狀態。
  function stop() {
    collectLogs()
    compose(['stop', '--timeout', '10'], '停止測試專案')
    assert.ok(compose(['ps', '--quiet'], '確認測試專案已停止').trim() === '', '停機後不可有執行中容器')
    application = undefined
    database = undefined
  }

  return {
    origin, loopbackOrigin, state, send, start, sql, stop, reset, resetResources, resetWithStopBarrier,
    cleanup, secondInstance, collectLogs, logs,
    performanceRuntime: () => performanceRuntime,
  }
}
