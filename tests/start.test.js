import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'

const localEndpoint = 'unix:///tmp/iot-regression.sock'
const remoteEndpoint = 'ssh://docker.example.invalid'

// 透過符號連結執行真正的啟動腳本，將 Docker 呼叫限制在暫存目錄的替身。
function launch(t, overrides) {
  const suite = mkdtempSync('/tmp/iot01-start-')
  // 登記清除這次建立的測試目錄，避免案例失敗時留下憑證或替身。
  t.after(() => rmSync(suite, { recursive: true, force: true }))
  const root = join(suite, 'project')
  const alias = join(suite, 'alias')
  mkdirSync(root)
  symlinkSync(root, alias)
  mkdirSync(join(root, 'bin'))
  copyFileSync(new URL('../bin/start', import.meta.url), join(root, 'bin/start'))
  copyFileSync(new URL('../compose.yaml', import.meta.url), join(root, 'compose.yaml'))
  // 替身回傳可控制的端點及資源歸屬，並記錄命令以便確認拒絕時機。
  const log = join(root, 'docker.log')
  writeFileSync(log, '')
  writeFileSync(join(root, 'bin/docker'), `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_LOG"
if [ "$1" = --host ]; then shift 2; fi
case "$1 $2" in
  'context show') printf '%s\n' "\${DOCKER_CONTEXT:-local-test}" ;;
  'context inspect')
    case "$3" in
      remote-test) printf '%s\n' '${remoteEndpoint}' ;;
      *) printf '%s\n' '${localEndpoint}' ;;
    esac ;;
  'ps -a')
    case "$FOREIGN_RESOURCE:$*" in
      container:*'label=com.docker.compose.project=iot-monitoring'|named-container:*'name=^/iot-monitoring-(app|db)-1$')
        printf '%s\\n' "$FOREIGN_RESOURCE_ID" ;;
    esac ;;
  'container inspect')
    if [ "$FOREIGN_RESOURCE" = named-container ]; then
      printf '||\\n'
    else
      printf '/tmp/another-worktree|%s/compose.yaml|iot-monitoring\\n' "$(pwd -P)"
    fi ;;
  'volume ls'|'network ls'|'image ls')
    if [ "$FOREIGN_RESOURCE" = "$1" ]; then printf '%s\\n' "$FOREIGN_RESOURCE_ID"; fi ;;
  'volume inspect'|'network inspect'|'image inspect') printf '%s\\n' '/tmp/another-worktree' ;;
  'compose '*)
    printf 'compose directory: <%s>\\n' "$PWD" >> "$DOCKER_LOG"
    printf 'compose environment: <%s> <%s> <%s> <%s> <%s> <%s> <%s> <%s>\\n' \\
      "$DOCKER_HOST" "$DOCKER_CONTEXT" "$BUILDX_BUILDER" "$BUILDKIT_HOST" \\
      "$COMPOSE_COMPATIBILITY" "$COMPOSE_ENV_FILES" "$COMPOSE_REMOVE_ORPHANS" "$IOT_PROJECT_ROOT" >> "$DOCKER_LOG" ;;
esac
`, { mode: 0o700 })
  // 只提供測試 PATH 與案例指定環境，讓主機設定無法改變結果。
  const result = spawnSync('/bin/sh', [join(alias, 'bin/start')], {
    cwd: suite,
    env: { PATH: `${join(root, 'bin')}:/usr/bin:/bin`, DOCKER_LOG: log, ...overrides },
    encoding: 'utf8', timeout: 5000,
  })
  assert.ifError(result.error)
  assert.equal(result.signal, null)
  // 收集結束狀態、診斷及副作用證據，供各個防護案例共用。
  return {
    root: realpathSync(root),
    status: result.status,
    stderr: result.stderr,
    calls: readFileSync(log, 'utf8')
      .trim()
      .split('\n')
      .filter(Boolean),
    wroteLocal: existsSync(join(root, '.local')),
  }
}

// 同時核對拒絕診斷和副作用，避免錯把建置或寫入後的失敗判為防護成功。
function assertRejected({ status, stderr, calls, wroteLocal }, diagnostic) {
  // 只從呼叫紀錄挑出會建置或啟動資源的 Compose 命令。
  assert.deepEqual({ status, wroteLocal, buildOrUp: calls.filter(call => /\bcompose\b.*\b(build|up)\b/.test(call)) }, {
    status: 1, wroteLocal: false, buildOrUp: [],
  }, '啟動器須在建置、啟動或建立憑證前拒絕錯誤目標')
  assert.match(stderr, diagnostic)
}

// 以直接指定的 SSH 端點驗證啟動前的本機 Docker 限制。
test('啟動器拒絕 DOCKER_HOST 指向遠端 daemon', t => {
  assertRejected(launch(t, { DOCKER_HOST: remoteEndpoint }), /Docker/)
})

// 同時提供本機 host 與遠端 context，確認檢查的是實際選到的端點。
test('啟動器拒絕遠端具名 Docker context，即使 DOCKER_HOST 指向本機', t => {
  assertRejected(launch(t, { DOCKER_HOST: localEndpoint, DOCKER_CONTEXT: 'remote-test' }), /Docker/)
})

// 外部指定不同專案名稱時，須先拒絕以免碰到他案資源。
test('啟動器拒絕外部指定的 Compose 專案', t => {
  assertRejected(launch(t, { COMPOSE_PROJECT_NAME: 'unrelated-project' }), /Compose|COMPOSE_PROJECT_NAME/)
})

// 驗證測試專案名稱只能由特定暫存布局使用。
test('啟動器拒絕一般工作目錄使用測試專案名稱', t => {
  assertRejected(launch(t, { COMPOSE_PROJECT_NAME: 'iot01_test_0123456789abcdef' }), /Compose|COMPOSE_PROJECT_NAME/)
})

// 逐一驗證啟動器會查核各種可與其他工作目錄衝突的資源。
for (const [kind, id, description] of [
  ['container', 'b73c802dcab4', '同專案容器'],
  ['volume', 'iot-monitoring_data', '同名資料卷'],
  ['network', 'iot-monitoring_default', '同名網路'],
  ['image', 'iot-monitoring-app:latest', '同標籤應用映像'],
]) {
  // 以實際資源識別確認啟動器檢查歸屬，並在產生副作用前拒絕。
  test(`啟動器拒絕另一個工作目錄的${description}`, t => {
    const outcome = launch(t, { DOCKER_HOST: localEndpoint, FOREIGN_RESOURCE: kind, FOREIGN_RESOURCE_ID: id })
    assertRejected(outcome, /不屬於此工作目錄/)
    // 確認替身確實收到指定資源的檢查命令，避免只測到其他前置防護。
    assert.ok(outcome.calls.some(call => call.includes(`${kind} inspect`) && call.endsWith(` ${id}`)), '須確認衝突資源的歸屬')
  })
}

// 同名容器缺少標籤時仍須檢查並拒絕，避免接管無法辨認的資源。
test('啟動器拒絕沒有 Compose 標籤的同名容器', t => {
  const id = 'e72c802dcab4'
  const outcome = launch(t, { DOCKER_HOST: localEndpoint, FOREIGN_RESOURCE: 'named-container', FOREIGN_RESOURCE_ID: id })
  assertRejected(outcome, /Compose 容器不屬於此工作目錄/)
  // 核對被檢查的容器就是替身回報的同名資源。
  assert.ok(outcome.calls.some(call => call.includes('container inspect') && call.endsWith(` ${id}`)), '須確認同名容器的歸屬')
})

// 從符號連結啟動並混入外部設定，驗證實體工作目錄和命令仍由啟動器決定。
test('啟動器透過符號連結時仍使用實體工作目錄與固定的 Compose 設定', t => {
  const outcome = launch(t, {
    DOCKER_HOST: remoteEndpoint,
    DOCKER_CONTEXT: 'local-test',
    BUILDX_BUILDER: 'unrelated-builder',
    BUILDKIT_HOST: 'tcp://builder.example.invalid:1234',
    COMPOSE_COMPATIBILITY: '1',
    COMPOSE_ENV_FILES: 'unrelated.env',
    COMPOSE_REMOVE_ORPHANS: '1',
    IOT_PROJECT_ROOT: '/tmp/another-worktree',
  })
  // 先確認正常啟動且建立本機狀態，再核對實際執行的命令。
  assert.equal(outcome.status, 0, outcome.stderr)
  assert.ok(outcome.wroteLocal)
  // 只比對 daemon 操作；context 查詢和替身額外記錄不屬於這類操作。
  const daemonCalls = outcome.calls.filter(call => !call.startsWith('context ') && !call.startsWith('compose '))
  assert.ok(daemonCalls.every(call => call.startsWith(`--host ${localEndpoint} `)), '所有 daemon 操作須使用已驗證的端點')
  // 核對建置及啟動都以實體路徑和固定的專案設定執行。
  const directory = `compose directory: <${outcome.root}>`
  assert.deepEqual(outcome.calls.filter(call => call.startsWith('compose directory:')), [directory, directory])
  const compose = `--host ${localEndpoint} compose --env-file /dev/null -p iot-monitoring -f ${outcome.root}/compose.yaml`
  // 只挑出建置及啟動命令，比對實際執行的設定。
  assert.deepEqual(outcome.calls.filter(call => /\bcompose\b.*\b(build|up)\b/.test(call)), [
    `${compose} build --builder default`,
    `${compose} up --no-build --wait`,
  ])
  // 用替身回報的環境核對外部設定已排除，並保留經驗證的專案路徑。
  const environment = `compose environment: <> <> <> <> <> <> <> <${outcome.root}>`
  assert.deepEqual(outcome.calls.filter(call => call.startsWith('compose environment:')), [environment, environment])
})
