package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const closureBeforeFile = "before-closure-restart.json"

type closureRestartEvidence struct {
	ClusterID int64
	StartedAt time.Time
	Clock     time.Time
	History   [sha256.Size]byte
}

// TestClosureIntegration 以真實 PostgreSQL 及 HTTP 驗證升級、兩類結案、授權、回滾與併發。
func TestClosureIntegration(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	if target.relations != 0 {
		t.Fatalf("expected empty disposable database, found %d public relations", target.relations)
	}
	state := openMeasurementTestState(t, true)
	conn := target.conn
	at := time.Date(2026, 11, 9, 10, 0, 0, 123456789, time.UTC)
	passwords := seedLegacyMeasurementFixture(t, conn, state)
	// 保留第 04 票的告警結構，補上目前控制入口需要的世代表後建立原有告警與 session。
	simulationSQL(t, conn, createMeasurementSchema+createSimulationSchema+addOfflineAlertColumns+createDataGenerationSchema)
	legacy, observed := newOfflineFixture(t, conn, at, false)
	legacy.hq = legacy.login(t, "hq", passwords["hq"])
	const northDevice, southDevice = "north-01", "south-01"
	controlSimulation(t, legacy.measurementFixture, northDevice, "abnormal", "0", simulationState{"abnormal", "1", true})
	expectSimulationSample(t, observed, northDevice, 30, true)
	recovery := at.Add(time.Second)
	legacy.setTime(recovery)
	controlSimulation(t, legacy.measurementFixture, northDevice, "normal", "1", simulationState{"normal", "2", true})
	expectSimulationSample(t, observed, northDevice, 23, true)
	controlSimulation(t, legacy.measurementFixture, northDevice, "stop", "2", simulationState{"stopped", "3", true})
	legacy.server.Close()
	legacy.app.simulator.close()

	// 第 05 票啟動後原告警及登入依舊有效，未結案回應維持原有欄位。
	startupSimulationFixture(t, conn, state)
	renewed := at.Add(2 * time.Second)
	f, observed := newOfflineFixture(t, conn, renewed, false)
	f.hq = legacy.hq
	north := f.login(t, "north", passwords["north"])
	south := f.login(t, "south", passwords["south"])
	const oldPath = "/api/alerts/1"
	original := f.get(t, oldPath)
	wantOriginal := fmt.Sprintf(`{"id":"1","deviceId":"north-01","type":"temperature","status":"recovered","temperature":30,"detectedAt":%q,"recoveredAt":%q}`,
		at.Format(time.RFC3339Nano), recovery.Format(time.RFC3339Nano))
	if string(original) != wantOriginal {
		t.Fatalf("startup changed the recovered alert or its JSON shape: %s", original)
	}

	// 新發生的異常不能結案，也不能阻止舊筆結案。兩筆資料各自維持證據。
	controlSimulation(t, f.measurementFixture, northDevice, "abnormal", "3", simulationState{"abnormal", "4", true})
	expectSimulationSample(t, observed, northDevice, 30, true)
	controlSimulation(t, f.measurementFixture, northDevice, "stop", "4", simulationState{"stopped", "5", true})
	newPath := "/api/alerts/2"
	newAlert := f.get(t, newPath)
	if got := readClosureAlert(t, newAlert); got.Status != "active" || got.DeviceID != northDevice {
		t.Fatalf("expected a newer active alert for the same device: %s", newAlert)
	}
	const note = `{"result":"已確認"}`
	expectClosureError(t, f, newPath, note, north, http.StatusConflict, "alert_conflict")

	// 失效登入與越權請求先後送出，後續成功結案也證明這些請求未更改狀態。
	t.Run("session and scope", func(t *testing.T) {
		expectClosureError(t, f, oldPath, note, nil, http.StatusUnauthorized, "unauthenticated")
		expired := f.login(t, "north", passwords["north"])
		hash := sha256.Sum256([]byte(expired.Value))
		// 只在隔離夾具調整到期時間，測試使用真實 Cookie 且不等待八小時。
		if _, err := conn.ExecContext(t.Context(), `UPDATE sessions SET expires_at = statement_timestamp() WHERE token_hash = $1`, hash[:]); err != nil {
			t.Fatal(err)
		}
		expectClosureError(t, f, oldPath, note, expired, http.StatusUnauthorized, "unauthenticated")
		revoked := f.login(t, "north", passwords["north"])
		f.request(t, http.MethodPost, "/api/logout", "", revoked, http.StatusNoContent)
		expectClosureError(t, f, oldPath, note, revoked, http.StatusUnauthorized, "unauthenticated")

		// 同一場域人員對越權、缺失與無效 ID 的回應完全一致。
		for _, path := range []string{oldPath, "/api/alerts/999999", "/api/alerts/invalid"} {
			expectClosureError(t, f, path, note, south, http.StatusNotFound, "not_found")
			body, _ := f.request(t, http.MethodGet, path, "", south, http.StatusNotFound)
			if string(body) != `{"error":"not_found"}` {
				t.Fatalf("unexpected scoped read error: %s", body)
			}
		}
	})
	t.Run("strict result and write guards", func(t *testing.T) {
		testClosureInputs(t, f, oldPath, north)
	})

	// 延遲觸發器在真正提交時模擬 PostgreSQL 容量不足，確認連已插入的資料也回滾。
	simulationSQL(t, conn, `
		CREATE FUNCTION fail_closure_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'isolated closure fixture' USING ERRCODE = '53100';
		END $$;
		CREATE CONSTRAINT TRIGGER fail_closure_commit AFTER INSERT ON alert_closures
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_closure_commit();
	`)
	expectClosureError(t, f, oldPath, note, north, http.StatusServiceUnavailable, "service_unavailable")
	if got := string(f.get(t, oldPath)); got != string(original) {
		t.Fatalf("rejected or rolled-back closure changed the original alert: %s", got)
	}
	simulationSQL(t, conn, "DROP TRIGGER fail_closure_commit ON alert_closures; DROP FUNCTION fail_closure_commit()")

	// 用含合法替代字元、表情、CRLF、LF 及 HTML 文字的 2,000 字元重試，保留原樣。
	const prefix, suffix = " <b>測試</b>�\r\n", "\n "
	long := prefix + strings.Repeat("😀", 2000-utf8.RuneCountInString(prefix+suffix)) + suffix
	body := closureJSON(t, long)
	body += strings.Repeat(" ", bodyLimit-len(body))
	closed, _ := f.request(t, http.MethodPost, oldPath+"/close", body, north, http.StatusOK)
	expectClosedAlert(t, original, closed, "north", long, renewed)
	expectClosureError(t, f, oldPath, note, f.hq, http.StatusConflict, "alert_conflict")
	if got := string(f.get(t, oldPath)); got != string(closed) {
		t.Fatal("second closure replaced the winning temperature result")
	}
	if got := string(f.get(t, newPath)); got != string(newAlert) {
		t.Fatalf("closing the previous alert changed the newer active alert: %s", got)
	}

	// 以真正的掃描及回報建立離線恢復，重現超標回報恢復在線但溫度告警仍活動。
	f.setTime(at.Add(3 * time.Second))
	controlSimulation(t, f.measurementFixture, southDevice, "normal", "0", simulationState{"normal", "1", true})
	expectSimulationSample(t, observed, southDevice, 23, true)
	controlSimulation(t, f.measurementFixture, southDevice, "stop", "1", simulationState{"stopped", "2", true})

	// 第三台設備在前一輪巡檢仍在線，期限到 +20 秒，只有競爭中的下一輪才會新增離線告警。
	const overdueDevice = "north-02"
	f.setTime(at.Add(5 * time.Second))
	controlSimulation(t, f.measurementFixture, overdueDevice, "normal", "0", simulationState{"normal", "1", true})
	last := expectSimulationSample(t, observed, overdueDevice, 23, true)
	controlSimulation(t, f.measurementFixture, overdueDevice, "stop", "1", simulationState{"stopped", "2", true})
	f.scan(t, at.Add(19*time.Second))
	offlineAlerts(t, f, overdueDevice, 0)
	expectDeviceOnlineStatus(t, f, overdueDevice, "online", &last)
	offlinePath := "/api/alerts/" + offlineAlerts(t, f, southDevice, 1)[0].ID
	expectClosureError(t, f, offlinePath, note, f.hq, http.StatusConflict, "alert_conflict")
	f.setTime(at.Add(20 * time.Second))
	controlSimulation(t, f.measurementFixture, southDevice, "abnormal", "2", simulationState{"abnormal", "3", true})
	report := expectSimulationSample(t, observed, southDevice, 30, true)
	detected := at.Add(21 * time.Second)
	f.setTime(detected)
	testClosureRace(t, target, f, offlinePath, north, south, report)

	// 從授權 API 核對巡檢確實寫入一筆新告警、保留最後回報並限制場域。
	detection := offlineAlerts(t, f, overdueDevice, 1)[0]
	expectOfflineAlert(t, detection, at.Add(20*time.Second), detected, nil, false)
	expectDeviceOnlineStatus(t, f, overdueDevice, "offline", &last)
	detectionPath := "/api/alerts/" + detection.ID
	own, _ := f.request(t, http.MethodGet, detectionPath, "", north, http.StatusOK)
	if string(own) != string(f.get(t, detectionPath)) {
		t.Fatal("headquarters and the device site saw different offline evidence")
	}
	f.request(t, http.MethodGet, detectionPath, "", south, http.StatusNotFound)

	// 大量分頁僅以 SQL 預載同時間的已恢復測試列，結案及狀態轉換仍由正式入口建立。
	simulationSQL(t, conn, `
		INSERT INTO alerts (device_id, type, temperature, detected_at_ns, recovered_at_ns)
		SELECT CASE WHEN n % 2 = 1 THEN 'north-03' ELSE 'south-03' END,
			'temperature', 30, 1, 2
		FROM generate_series(1, 105) AS n
	`)
	before := closureRestartEvidence{
		ClusterID: target.clusterID, StartedAt: target.startedAt,
		Clock: detected, History: verifyClosureHistory(t, f, north, south),
	}
	file, err := state.OpenFile(closureBeforeFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := json.NewEncoder(file).Encode(before)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

// TestClosureRestart 重新開啟正式啟動路徑，對照同一資料庫叢集重啟前後的歷程。
func TestClosureRestart(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	state := openMeasurementTestState(t, false)
	data, err := readPrivateFile(state, closureBeforeFile)
	if err != nil {
		t.Fatal(err)
	}
	var before closureRestartEvidence
	if err := json.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	if before.Clock.IsZero() ||
		before.StartedAt.IsZero() ||
		target.relations == 0 ||
		target.clusterID != before.ClusterID ||
		!target.startedAt.After(before.StartedAt) {
		t.Fatalf("expected restart of seeded PostgreSQL cluster, previous start %s, current %s", before.StartedAt, target.startedAt)
	}
	t.Logf("PostgreSQL postmaster restarted: %s -> %s", before.StartedAt, target.startedAt)

	// 以重啟的實際初始化及補判步驟啟動，再核對總部和場域可讀的完整分頁與明細。
	startupSimulationFixture(t, target.conn, state)
	f, _ := newOfflineFixture(t, target.conn, before.Clock, true)
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}
	f.hq = f.login(t, "hq", passwords["hq"])
	north := f.login(t, "north", passwords["north"])
	south := f.login(t, "south", passwords["south"])
	if got := verifyClosureHistory(t, f, north, south); got != before.History {
		t.Fatalf("restarted history differs: got %x want %x", got, before.History)
	}
}

// testClosureInputs 直接經 HTTP 傳原始位元組，避免測試端替無效字元或 JSON 結構修正。
func testClosureInputs(t *testing.T, f *offlineTestFixture, path string, cookie *http.Cookie) {
	t.Helper()
	for _, input := range []struct{ name, body string }{
		{"empty body", ""},
		{"missing", `{}`},
		{"wrong case", `{"Result":"a"}`},
		{"extra field", `{"result":"a","actor":"hq"}`},
		{"duplicate escaped key", `{"result":"a","res\u0075lt":"b"}`},
		{"number", `{"result":1}`},
		{"null", `{"result":null}`},
		{"array", `{"result":["a"]}`},
		{"trailing value", `{"result":"a"} {}`},
		{"empty result", `{"result":""}`},
		{"unicode whitespace", `{"result":" \u3000\u00a0\r\n "}`},
		{"unpaired high surrogate", `{"result":"\ud83dA"}`},
		{"unpaired low surrogate", `{"result":"\ude00"}`},
		{"raw invalid UTF-8", "{\"result\":\"\xff\"}"},
		{"nul", `{"result":"a\u0000b"}`},
		{"tab", `{"result":"a\tb"}`},
		{"standalone carriage return", `{"result":"a\rb"}`},
		{"C1 control", `{"result":"a\u0080b"}`},
		{"2001 code points", closureJSON(t, strings.Repeat("😀", 2001))},
	} {
		t.Run(input.name, func(t *testing.T) {
			expectClosureError(t, f, path, input.body, cookie, http.StatusBadRequest, "invalid_input")
		})
	}

	// 分別核對可辨識的 CSRF、格式和長度錯誤，包括無 Content-Length 的串流請求。
	const note = `{"result":"有效"}`
	data, _ := f.request(t, http.MethodPost, path+"/close?role=hq", note, cookie, http.StatusBadRequest)
	if string(data) != `{"error":"invalid_input"}` {
		t.Fatalf("unexpected query parameter error: %s", data)
	}
	for _, tc := range []struct {
		name, body, code string
		status           int
		configure        func(*http.Request)
	}{
		{"foreign origin", note, "forbidden", 403, func(r *http.Request) { r.Header.Set("Origin", "http://example.test") }},
		{"missing CSRF header", note, "forbidden", 403, func(r *http.Request) { r.Header.Del("X-CSRF-Protection") }},
		{"wrong content type", note, "unsupported_media_type", 415, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		{"encoded content", note, "unsupported_media_type", 415, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }},
		{"oversized known length", note + strings.Repeat(" ", bodyLimit), "body_too_large", 413, nil},
		{"oversized chunked body", note + strings.Repeat(" ", bodyLimit), "body_too_large", 413, func(r *http.Request) { r.ContentLength = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sendClosure(t.Context(), f.measurementFixture, path, tc.body, cookie, tc.configure)
			if got.err != nil ||
				got.status != tc.status ||
				got.body != fmt.Sprintf(`{"error":%q}`, tc.code) {
				t.Fatalf("wrong write guard response: %+v", got)
			}
		})
	}
}

// testClosureRace 讓兩個 HTTP 結案、停止、真實回報及離線巡檢同時面對同一條持鎖連線。
func testClosureRace(t *testing.T, target measurementTestDatabase, f *offlineTestFixture, path string, north, south *http.Cookie, initial Measurement) {
	t.Helper()
	const winnerInput = `{"result":"\ud83d\ude00"}`
	const loserInput = `{"result":"第二筆"}`
	original := f.get(t, path)
	expectClosureError(t, f, path, winnerInput, north, http.StatusNotFound, "not_found")
	// 先在 PostgreSQL 鎖住目標列，確定第一個 HTTP 請求真的進入了結案交易。
	rowLock, err := target.pool.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rowLock.Rollback()
	id, err := strconv.ParseInt(strings.TrimPrefix(path, "/api/alerts/"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	var locked int64
	if err := rowLock.QueryRowContext(t.Context(), "SELECT id FROM alerts WHERE id = $1 FOR UPDATE", id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	first := make(chan closureHTTPResult, 1)
	go func() { first <- sendClosure(t.Context(), f.measurementFixture, path, winnerInput, f.hq, nil) }()
	waitForMeasurementRowLock(t, target.pool, target.pid)

	// 用同一路由接收第二個真實 HTTP 結案，從伺服器端觀察共用連線的期限初始化。
	closeEntered := make(chan struct{}, 1)
	router := f.app.router()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r.WithContext(closureDeadlineProbe{r.Context(), closeEntered}))
	}))
	server.Client().Timeout = 5 * time.Second
	t.Cleanup(server.Close)
	competitor := &measurementFixture{app: f.app, server: server}
	second := make(chan closureHTTPResult, 1)
	go func() { second <- sendClosure(t.Context(), competitor, path, loserInput, south, nil) }()
	waitForSimulation(t, closeEntered)

	// 讓回報和掃描沿用同一較短期限，以便分辨各自的入口期限與共用連線的期限。
	operationContext, cancel := context.WithTimeout(t.Context(), dbOperationTimeout-time.Millisecond)
	defer cancel()
	pending := initial
	pending.Sequence++
	pending.GeneratedAt = time.Unix(0, f.clock.Load()).UTC()
	pending.Temperature = 31
	reportEntered := make(chan struct{}, 2)
	submitted := f.submitAsync(closureDeadlineProbe{operationContext, reportEntered}, pending)
	waitForSimulation(t, reportEntered) // 回報本身的處理期限。
	waitForSimulation(t, reportEntered) // 共用連線的等待期限。

	// 停止以服務 context 排隊。已啟動的計時工作使用自己先前取得的子 context。
	stopEntered := make(chan struct{}, 1)
	f.app.simulator.ctx = closureDeadlineProbe{f.app.simulator.ctx, stopEntered}
	control := make(chan simulationHTTPResult, 1)
	go func() {
		control <- sendSimulation(t.Context(), f.measurementFixture, initial.DeviceID, simulationBody("stop", "3"), f.hq, true)
	}()
	waitForSimulation(t, stopEntered)

	// 等掃描進入共用連線函式才放鎖。此時第一個結案仍占住連線，其他操作都已進入競爭。
	scanEntered := make(chan struct{}, 2)
	scan := make(chan error, 1)
	go func() { scan <- f.app.scanOffline(closureDeadlineProbe{operationContext, scanEntered}, false) }()
	waitForSimulation(t, scanEntered) // 巡檢整體期限。
	waitForSimulation(t, scanEntered) // 共用連線的等待期限。
	if err := rowLock.Rollback(); err != nil {
		t.Fatal(err)
	}

	// 第一筆保留精確的單字元，第二筆衝突，後續兩個角色讀回的結果相同。
	closed := waitForSimulation(t, first)
	if closed.err != nil || closed.status != http.StatusOK {
		t.Fatalf("first closure did not succeed: %+v", closed)
	}
	expectClosedAlert(t, original, []byte(closed.body), "hq", "😀", pending.GeneratedAt)
	loser := waitForSimulation(t, second)
	if loser.err != nil ||
		loser.status != http.StatusConflict ||
		loser.body != `{"error":"alert_conflict"}` {
		t.Fatalf("second closure did not conflict: %+v", loser)
	}
	sameSite, _ := f.request(t, http.MethodGet, path, "", south, http.StatusOK)
	if got := string(f.get(t, path)); got != closed.body || string(sameSite) != closed.body {
		t.Fatal("concurrent closures did not preserve the first committed result")
	}

	// 若停止先於回報，舊執行被拒；若回報先到連線，完整保存它並維持在線與溫度告警。
	expectSimulationHTTPState(t, waitForSimulation(t, control), simulationState{"stopped", "4", true})
	got := waitForSimulation(t, submitted)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.adopted {
		initial = pending
	}
	if err := waitForSimulation(t, scan); err != nil {
		t.Fatal(err)
	}
	expectDeviceOnlineStatus(t, f, initial.DeviceID, "online", &initial)
	temperatureAlert(t, f, initial.DeviceID)
	offlineAlerts(t, f, initial.DeviceID, 1)
}

type closureDeadlineProbe struct {
	context.Context
	entered chan<- struct{}
}

// Deadline 觀察正式程式設定期限的時機，保留原本期限與取消。較短的父期限會被後續的 WithTimeout 沿用。
func (probe closureDeadlineProbe) Deadline() (time.Time, bool) {
	select {
	case probe.entered <- struct{}{}:
	default:
	}
	return probe.Context.Deadline()
}

// expectClosureError 核對一般 JSON 結案失敗的 HTTP 狀態及業務錯誤代碼。
func expectClosureError(t *testing.T, f *offlineTestFixture, path, input string, cookie *http.Cookie, status int, code string) {
	t.Helper()
	data, _ := f.request(t, http.MethodPost, path+"/close", input, cookie, status)
	if string(data) != fmt.Sprintf(`{"error":%q}`, code) {
		t.Fatalf("unexpected closure error: %s", data)
	}
}

// closureJSON 確保測試輸入透過 JSON 跳脫後仍包含預期的 Unicode 字元。
func closureJSON(t *testing.T, result string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"result": result})
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]string
	if err := json.Unmarshal(body, &input); err != nil || input["result"] != result {
		t.Fatal("serialized test request changed the supplied code points")
	}
	return string(body)
}

// readClosureAlert 解碼真實回應，讓後續檢查可以比對個別證據。
func readClosureAlert(t *testing.T, body []byte) alertItem {
	t.Helper()
	var item alertItem
	if err := json.Unmarshal(body, &item); err != nil {
		t.Fatal(err)
	}
	return item
}

// expectClosedAlert 核對新增的唯讀欄位、UTC 時間格式及原有證據。
func expectClosedAlert(t *testing.T, before, after []byte, actor, result string, closedAt time.Time) {
	t.Helper()
	got := readClosureAlert(t, after)
	if got.Status != "closed" ||
		got.Closure == nil ||
		got.Closure.Actor != actor ||
		got.Closure.Result != result ||
		!got.Closure.ClosedAt.Equal(closedAt) ||
		!strings.Contains(string(after), fmt.Sprintf(`"closedAt":%q`, closedAt.Format(time.RFC3339Nano))) {
		t.Fatalf("close response has wrong status, actor, time or result for alert %s", got.ID)
	}
	want := readClosureAlert(t, before)
	got.Status, got.Closure = "recovered", nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closing changed existing evidence for alert %s", got.ID)
	}
}

type closureHTTPResult struct {
	status int
	body   string
	err    error
}

// sendClosure 經實際網路送出結案，保留傳輸錯誤以便背景請求及個別標頭案例檢查。
func sendClosure(ctx context.Context, f *measurementFixture, path, body string, cookie *http.Cookie, configure func(*http.Request)) closureHTTPResult {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+path+"/close", strings.NewReader(body))
	if err != nil {
		return closureHTTPResult{err: err}
	}
	r.Host = f.app.host
	r.AddCookie(cookie)
	r.Header.Set("Origin", measurementTestOrigin)
	r.Header.Set("X-CSRF-Protection", "1")
	r.Header.Set("Content-Type", "application/json")
	if configure != nil {
		configure(r)
	}
	response, err := f.server.Client().Do(r)
	if err != nil {
		return closureHTTPResult{err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if response.Header.Get("Cache-Control") != "no-store" {
		err = errors.Join(err, errors.New("API response can be cached"))
	}
	return closureHTTPResult{status: response.StatusCode, body: string(data), err: err}
}

// verifyClosureHistory 核對固定筆數、兩種分頁及場域範圍，並保存完整回應的摘要供重啟比對。
func verifyClosureHistory(t *testing.T, f *offlineTestFixture, north, south *http.Cookie) [sha256.Size]byte {
	t.Helper()
	var evidence strings.Builder
	for _, scope := range []struct {
		cookie *http.Cookie
		site   string
		total  int
		closed int
	}{
		{f.hq, "", 111, 2},
		{north, "north-", 57, 1},
		{south, "south-", 54, 1},
	} {
		// 比對預設第一頁、後續偏移頁及 100 筆頁面，且每個角色只看到自己可查閱的結案。
		var previousID int64
		var ids []string
		var closed int
		for offset := 0; offset < scope.total; offset += 50 {
			query := ""
			if offset != 0 {
				query = "?offset=" + strconv.Itoa(offset)
			}
			page := closureHistoryPage(t, f, scope.cookie, query, scope.total, 50, offset, &evidence)
			for _, item := range page.Items {
				id, err := strconv.ParseInt(item.ID, 10, 64)
				if err != nil ||
					id <= 0 ||
					(previousID != 0 && id >= previousID) ||
					!strings.HasPrefix(item.DeviceID, scope.site) {
					t.Fatalf("history out of order, overlapping or out of scope at alert %s", item.ID)
				}
				previousID = id
				ids = append(ids, item.ID)
				if item.Closure != nil {
					closed++
					detail, _ := f.request(t, http.MethodGet, "/api/alerts/"+item.ID, "", scope.cookie, http.StatusOK)
					if !reflect.DeepEqual(readClosureAlert(t, detail), item) {
						t.Fatalf("list and detail disagree for closed alert %s", item.ID)
					}
					evidence.Write(detail)
				}
			}
		}
		if len(ids) != scope.total || closed != scope.closed {
			t.Fatalf("history scope %q returned %d items and %d closures", scope.site, len(ids), closed)
		}
		// 相同資料用上限 100 分頁應得到同一順序及相同分頁邊界。
		for offset := 0; offset < scope.total; offset += 100 {
			query := fmt.Sprintf("?limit=100&offset=%d", offset)
			page := closureHistoryPage(t, f, scope.cookie, query, scope.total, 100, offset, &evidence)
			for index, item := range page.Items {
				if item.ID != ids[offset+index] {
					t.Fatalf("history order changed at offset %d", offset+index)
				}
			}
		}
	}
	for _, query := range []string{"?limit=101", "?limit=0", "?offset=-1", "?offset=0&offset=1"} {
		data, _ := f.request(t, http.MethodGet, "/api/alerts"+query, "", f.hq, http.StatusBadRequest)
		if string(data) != `{"error":"invalid_input"}` {
			t.Fatalf("unexpected page parameter error: %s", data)
		}
	}
	return sha256.Sum256([]byte(evidence.String()))
}

// closureHistoryPage 核對分頁中繼資料與實際資料筆數，將原始回應納入跨重啟比對。
func closureHistoryPage(t *testing.T, f *offlineTestFixture, cookie *http.Cookie, query string, total, limit, offset int, evidence *strings.Builder) alertPage {
	t.Helper()
	data, _ := f.request(t, http.MethodGet, "/api/alerts"+query, "", cookie, http.StatusOK)
	var page alertPage
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != total ||
		page.Limit != limit ||
		page.Offset != offset ||
		len(page.Items) != min(limit, total-offset) {
		t.Fatalf("bad page %s: total=%d limit=%d offset=%d items=%d", query, page.Total, page.Limit, page.Offset, len(page.Items))
	}
	evidence.Write(data)
	return page
}
