package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	ssePath       = "/api/events"
	sseBeforeFile = "before-sse-restart.json"
)

type testEventVersion struct {
	Viewer    string    `json:"viewer"`
	Version   string    `json:"version"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type testEventStream struct {
	reader *bufio.Reader
	cancel context.CancelFunc
}

// TestSSEIntegration 用真正的 PostgreSQL 與 HTTP 驗證已提交變更、場域版本和 session 生命週期。
func TestSSEIntegration(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	if target.relations != 0 {
		t.Fatalf("expected empty disposable database, found %d public relations", target.relations)
	}
	conn := target.conn
	state := openMeasurementTestState(t, true)
	if err := bootstrap(t.Context(), conn, state); err != nil {
		t.Fatal(err)
	}
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	f, _ := newOfflineFixture(t, conn, now, false)
	f.hq = f.login(t, "hq", passwords["hq"])
	north := f.login(t, "north", passwords["north"])
	south := f.login(t, "south", passwords["south"])
	testSSERequestValidation(t, f.measurementFixture, north)
	northView, otherNorth := testSSESessionView(t, f.measurementFixture, conn, north, passwords["north"])

	// 三個身分先訂閱再讀初始快照。南區不得查到北區設備或後續告警。
	allEvents := openSSE(t, f.measurementFixture, f.hq)
	northEvents := openSSE(t, f.measurementFixture, north)
	southEvents := openSSE(t, f.measurementFixture, south)
	beforeNorth, _ := f.request(t, http.MethodGet, "/api/devices/north-01", "", north, http.StatusOK)
	f.request(t, http.MethodGet, "/api/devices/north-01", "", south, http.StatusNotFound)
	if !strings.Contains(string(beforeNorth), `"onlineStatus":"unreported"`) {
		t.Fatalf("unexpected initial device: %s", beforeNorth)
	}

	// 透過測量入口採用超標回報，收到通知後用原本的查詢查驗資料與權限。
	simulationSQL(t, conn, `INSERT INTO measurement_runs (device_id, data_generation, run_id) VALUES ('north-01', 1, 10)`)
	sample := offlineSample("north-01", 1, now, 30)
	f.submit(t, sample, true)
	// 在開始消費串流前提交新測量，讓握手版本與隨後版本一起排入 HTTP 連線。
	expectVersion(t, allEvents, "0")
	northInitial := expectVersion(t, northEvents, "0")
	expectVersion(t, southEvents, "0")
	if northInitial.Viewer != northView || !northInitial.ExpiresAt.Equal(sseSessionExpiry(t, conn, north)) {
		t.Fatal("version did not match the authorized session metadata")
	}
	expectVersion(t, allEvents, "1")
	expectVersion(t, northEvents, "1")
	expectDeviceOnlineStatus(t, f, sample.DeviceID, "online", &sample)
	alert := temperatureAlert(t, f, sample.DeviceID)
	alertPath := "/api/alerts/" + alert.ID
	f.request(t, http.MethodGet, alertPath, "", south, http.StatusNotFound)

	// 重複、過期及在 COMMIT 才失敗的回報都不能改版本。重試原樣本應只產生一次恢復。
	f.submit(t, sample, false)
	f.setTime(now.Add(6 * time.Second))
	f.submit(t, offlineSample(sample.DeviceID, 2, now, 24), false)
	recovery := offlineSample(sample.DeviceID, 2, now.Add(6*time.Second), 24)
	dropFailure := failSSECommit(t, conn, "latest_measurements")
	adopted, err := f.app.SubmitMeasurement(t.Context(), recovery)
	if adopted ||
		err == nil ||
		!strings.Contains(err.Error(), "commit measurement transaction") {
		t.Fatalf("expected measurement commit failure, adopted=%v: %v", adopted, err)
	}
	expectDeviceOnlineStatus(t, f, sample.DeviceID, "online", &sample)
	dropFailure()
	f.submit(t, recovery, true)
	expectVersion(t, allEvents, "2")
	expectVersion(t, northEvents, "2")
	if recovered := readClosureAlert(t, f.get(t, alertPath)); recovered.ID != alert.ID || recovered.Status != "recovered" {
		t.Fatalf("notification preceded persisted temperature recovery: %+v", recovered)
	}

	// 結案失敗及越權不能通知。合法結案提交後，授權讀回立即看得到原處理結果。
	dropFailure = failSSECommit(t, conn, "alert_closures")
	closeBody := closureJSON(t, "已確認設備恢復")
	f.request(t, http.MethodPost, alertPath+"/close", closeBody, north, http.StatusServiceUnavailable)
	if item := readClosureAlert(t, f.get(t, alertPath)); item.Status != "recovered" {
		t.Fatalf("failed closure changed alert: %+v", item)
	}
	dropFailure()
	f.request(t, http.MethodPost, alertPath+"/close", closeBody, south, http.StatusNotFound)
	requestWithSSEView(t, f.measurementFixture, http.MethodPost, alertPath+"/close", closeBody, otherNorth, http.StatusUnauthorized, northView)
	if item := readClosureAlert(t, f.get(t, alertPath)); item.Status != "recovered" {
		t.Fatalf("mismatched session view closed alert: %+v", item)
	}
	requestWithSSEView(t, f.measurementFixture, http.MethodPost, alertPath+"/close", closeBody, north, http.StatusOK, northView)
	expectVersion(t, allEvents, "3")
	expectVersion(t, northEvents, "3")
	if item := readClosureAlert(t, f.get(t, alertPath)); item.Closure == nil || item.Closure.Result != "已確認設備恢復" {
		t.Fatalf("notification preceded persisted closure: %+v", item)
	}
	f.request(t, http.MethodPost, alertPath+"/close", closeBody, north, http.StatusConflict)

	// 只有實際提交的離線新增會通知，重複巡檢不推進版本。新有效測量恢復同一告警。
	overdue := recovery.GeneratedAt.Add(16 * time.Second)
	f.setTime(overdue)
	dropFailure = failSSECommit(t, conn, "alerts")
	if err := f.app.scanOffline(t.Context(), false); err == nil || !strings.Contains(err.Error(), "commit offline scan") {
		t.Fatalf("expected offline commit failure: %v", err)
	}
	offlineAlerts(t, f, sample.DeviceID, 0)
	dropFailure()
	f.scan(t, overdue)
	expectVersion(t, allEvents, "4")
	expectVersion(t, northEvents, "4")
	expectDeviceOnlineStatus(t, f, sample.DeviceID, "offline", &recovery)
	offline := offlineAlerts(t, f, sample.DeviceID, 1)[0]
	f.scan(t, overdue)
	f.setTime(overdue.Add(time.Second))
	online := offlineSample(sample.DeviceID, 3, overdue.Add(time.Second), 24)
	f.submit(t, online, true)
	expectVersion(t, allEvents, "5")
	expectVersion(t, northEvents, "5")
	expectDeviceOnlineStatus(t, f, sample.DeviceID, "online", &online)
	if recovered := offlineAlerts(t, f, sample.DeviceID, 1)[0]; recovered.ID != offline.ID || recovered.Status != "recovered" {
		t.Fatalf("notification preceded persisted offline recovery: %+v", recovered)
	}

	// 由總部控制南區，南區應收到自己的第一個版本，北區只會收到下一筆北區的版本。
	dropFailure = failSSECommit(t, conn, "simulation_history")
	expectSimulationError(t, f.measurementFixture, "south-01", simulationBody("stop", "0"), f.hq, http.StatusServiceUnavailable, "service_unavailable")
	expectSimulation(t, f.measurementFixture, "south-01", simulationState{Mode: "stopped", Version: "0"})
	dropFailure()
	f.request(t, http.MethodPost, simulationPath("south-01"), simulationBody("stop", "0"), south, http.StatusForbidden)
	f.request(t, http.MethodPost, simulationPath("south-01"), simulationBody("stop", "1"), f.hq, http.StatusConflict)
	controlSimulation(t, f.measurementFixture, "south-01", "stop", "0", simulationState{Mode: "stopped", Version: "1"})
	expectVersion(t, allEvents, "6")
	expectVersion(t, southEvents, "1")
	expectSimulation(t, f.measurementFixture, "south-01", simulationState{Mode: "stopped", Version: "1"})
	online.Sequence++
	f.submit(t, online, true)
	expectVersion(t, allEvents, "7")
	expectVersion(t, northEvents, "6")

	// 重連用當前範圍版本及新讀取，且撤銷只停止相同 Cookie 的訂閱。
	northEvents.cancel()
	northEvents = openSSE(t, f.measurementFixture, north)
	expectVersion(t, northEvents, "6")
	reconnected, _ := f.request(t, http.MethodGet, alertPath, "", north, http.StatusOK)
	if item := readClosureAlert(t, reconnected); item.Closure == nil || item.Closure.Actor != "north" {
		t.Fatalf("reconnect did not expose current authorized snapshot: %+v", item)
	}
	testSSESessionEnd(t, f.measurementFixture, conn, north, south, passwords["north"], northEvents, southEvents)
	// 其他 session 仍可接收變更，停止及重連從未改變它的範圍計數。
	controlSimulation(t, f.measurementFixture, "south-01", "stop", "1", simulationState{Mode: "stopped", Version: "2"})
	expectVersion(t, allEvents, "8")
	online.Sequence++
	testSSEBlockedFlush(t, f, allEvents, online)

	// 保留資料庫與 HTTP 結果的證據，讓下一個程序核對真正的重啟。
	evidence, err := json.Marshal(measurementRestartEvidence{
		ClusterID: target.clusterID, StartedAt: target.startedAt, API: f.snapshot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.WriteFile(sseBeforeFile, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSSERestart 驗證重啟後以新程序版本讀回持久快照，並讓串流跨越一般 HTTP 寫入期限。
func TestSSERestart(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	state := openMeasurementTestState(t, false)
	data, err := state.ReadFile(sseBeforeFile)
	if err != nil {
		t.Fatal(err)
	}
	var before measurementRestartEvidence
	if err := json.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	if target.clusterID != before.ClusterID || !target.startedAt.After(before.StartedAt) {
		t.Fatalf("expected a PostgreSQL restart: %s -> %s", before.StartedAt, target.startedAt)
	}
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}
	// 使用正式伺服器的寫入逾時設定，避免測試伺服器預設值掩蓋長連線錯誤。
	app := measurementTestApplication(target.conn)
	server := httptest.NewUnstartedServer(nil)
	server.Config = app.httpServer()
	server.Start()
	t.Cleanup(server.Close)
	server.Client().Timeout = 5 * time.Second
	f := &measurementFixture{app: app, server: server}
	f.hq = f.login(t, "hq", passwords["hq"])
	stream := openSSE(t, f, f.hq)
	expectVersion(t, stream, "0")
	if got := f.snapshot(t); got != before.API {
		t.Fatalf("restart changed persistent snapshot: got %s, want %s", got, before.API)
	}
	start := time.Now()
	for range 2 {
		line, err := stream.reader.ReadString('\n')
		if err != nil || line != "event: heartbeat\n" {
			t.Fatalf("expected SSE heartbeat, got %q: %v", line, err)
		}
		expectSSEHeartbeatData(t, stream)
	}
	if elapsed := time.Since(start); elapsed <= server.Config.WriteTimeout {
		t.Fatalf("heartbeats did not cross the production write timeout: %s", elapsed)
	}
	app.events.stop()
	expectSSEEnd(t, stream)
	f.request(t, http.MethodGet, ssePath, "", f.hq, http.StatusServiceUnavailable)
}

type blockedSSEWriter struct {
	http.ResponseWriter
	ctx      context.Context
	cleared  chan struct{}
	entered  chan time.Time
	deadline time.Time
	flushed  bool
	timedOut bool
}

// SetWriteDeadline 記錄正式串流的寫入期限，並通知測試首個訊框成功後已清除期限。
func (w *blockedSSEWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	if deadline.IsZero() && w.flushed {
		close(w.cleared)
	}
	return nil
}

// FlushError 讓握手訊框成功，下一次寫入則等待期限並回傳逾時。
func (w *blockedSSEWriter) FlushError() error {
	if !w.flushed {
		w.flushed = true
		return nil
	}
	w.entered <- w.deadline
	timer := time.NewTimer(time.Until(w.deadline))
	defer timer.Stop()
	select {
	case <-timer.C:
		w.timedOut = true
		return os.ErrDeadlineExceeded
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

// testSSEBlockedFlush 讓真實路由在寫入處逾時，並以其他 HTTP 連線查驗採用、更新和容量回收。
func testSSEBlockedFlush(t *testing.T, f *offlineTestFixture, healthy testEventStream, sample Measurement) {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, f.app.origin+ssePath, nil)
	request.AddCookie(f.hq)
	recorder := httptest.NewRecorder()
	blocked := &blockedSSEWriter{
		ResponseWriter: recorder, ctx: t.Context(),
		cleared: make(chan struct{}), entered: make(chan time.Time, 1),
	}
	exited := make(chan struct{})
	// 只替代寫入邊界，完整執行正式路由、Cookie 查詢、訂閱及離開時的清理。
	go func() {
		defer close(exited)
		f.server.Config.Handler.ServeHTTP(blocked, request)
	}()
	waitForSimulation(t, blocked.cleared)

	// 預留其餘槽位，讓真實 HTTP 在慢連線離開前後驗證容量差異。
	for range maxEventStreams - 2 {
		// 佔位訂閱不進入寫入迴圈，結束時由測試直接歸還。
		slot, _ := f.app.events.subscribe(nil, new("south"), func() {})
		if slot == nil {
			t.Fatal("unexpected live stream occupied a spare slot")
		}
		// 即使後續斷言失敗也要歸還佔位槽。
		t.Cleanup(func() { f.app.events.unsubscribe(slot) })
	}
	// 首筆提交觸發下一個訊框，必須重新設定期限，不能依賴握手時的期限。
	f.submit(t, sample, true)
	deadline := waitForSimulation(t, blocked.entered)
	if deadline.IsZero() {
		t.Fatal("later flush did not receive a fresh write deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > eventWriteLimit {
		t.Fatalf("later flush received an invalid write deadline: %s", remaining)
	}
	expectVersion(t, healthy, "9")
	expectDeviceOnlineStatus(t, f, sample.DeviceID, "online", &sample)

	// 慢連線仍卡在第二次 flush 時，再提交一筆回報並驗證讀回及容量上限。
	sample.Sequence++
	f.submit(t, sample, true)
	expectVersion(t, healthy, "10")
	expectDeviceOnlineStatus(t, f, sample.DeviceID, "online", &sample)
	f.request(t, http.MethodGet, ssePath, "", f.hq, http.StatusServiceUnavailable)
	select {
	case <-exited:
		t.Fatal("slow flush ended before the independent measurement and readback completed")
	default:
	}

	waitForSimulation(t, exited)
	if !blocked.timedOut || recorder.Code != http.StatusOK {
		t.Fatal("stream handler did not return after its write deadline")
	}
	reconnected := openSSE(t, f.measurementFixture, f.hq)
	expectVersion(t, reconnected, "10")
	expectDeviceOnlineStatus(t, f, sample.DeviceID, "online", &sample)
	reconnected.cancel()
}

// TestSSEBoundedMailbox 驗證慢訂閱只留一筆最新版本，健康訂閱及回收後的容量不受影響。
func TestSSEBoundedMailbox(t *testing.T) {
	var events liveEvents
	var streams []*eventStream
	// 填滿連線數上限，模擬第一條連線長時間無法讀取。
	for i := range maxEventStreams {
		var site *string
		if i == 2 {
			site = new("south")
		}
		// 此案例直接讀取佇列，取消動作由 HTTP 生命週期案例覆蓋。
		stream, _ := events.subscribe([]byte{byte(i)}, site, func() {})
		if stream == nil {
			t.Fatalf("stream %d unexpectedly exceeded limit", i)
		}
		streams = append(streams, stream)
	}
	// 已額滿時拒絕另一條訂閱，測試用訂閱無需取消回呼。
	if stream, _ := events.subscribe(nil, nil, func() {}); stream != nil {
		t.Fatal("subscription exceeded the connection bound")
	}
	consumed := make(chan struct{})
	done := make(chan struct{})
	// 發送端等健康訂閱每次確認，以精準比較版本，同時維持慢訂閱不讀取。
	go func() {
		defer close(done)
		for range 256 {
			events.publish("north")
			select {
			case <-consumed:
			case <-t.Context().Done():
				return
			}
		}
	}()
	for version := uint64(1); version <= 256; version++ {
		select {
		case got := <-streams[1].versions:
			if got != version {
				t.Fatalf("healthy stream got version %d, want %d", got, version)
			}
		case <-time.After(time.Second):
			t.Fatal("healthy stream did not receive version")
		}
		select {
		case got := <-streams[2].versions:
			t.Fatalf("south subscription received a north change: %d", got)
		default:
		}
		consumed <- struct{}{}
	}
	<-done
	select {
	case got := <-streams[0].versions:
		if got != 256 || len(streams[0].versions) != 0 {
			t.Fatalf("slow stream retained outdated versions: got %d with %d queued", got, len(streams[0].versions))
		}
	default:
		t.Fatal("slow stream lost the latest version")
	}
	events.unsubscribe(streams[0])
	// 南區未收到北區更新，重新訂閱也不得透過版本推知北區活動。
	if stream, version := events.subscribe(nil, new("south"), func() {}); stream == nil || version != 0 {
		t.Fatalf("released capacity missing or another site's changes disclosed: %d", version)
	}
}

// testSSERequestValidation 驗證訂閱沿用共用 Host、Cookie 和輸入規則，也拒絕跨 Origin。
func testSSERequestValidation(t *testing.T, f *measurementFixture, north *http.Cookie) {
	t.Helper()
	f.request(t, http.MethodGet, ssePath, "", nil, http.StatusUnauthorized)
	f.request(t, http.MethodGet, ssePath+"?site=south", "", north, http.StatusBadRequest)
	f.request(t, http.MethodGet, ssePath, "north", north, http.StatusBadRequest)
	for _, tc := range []struct{ host, origin string }{
		{"untrusted.localhost:8090", measurementTestOrigin},
		{f.app.host, "http://untrusted.localhost:8090"},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+ssePath, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = tc.host
		req.Header.Set("Origin", tc.origin)
		req.AddCookie(north)
		res, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("unexpected host/origin response: %d", res.StatusCode)
		}
	}
}

// testSSESessionEnd 確認登出、換發及到期各自終止既有連線，也停止舊 Cookie 後續讀取。
func testSSESessionEnd(t *testing.T, f *measurementFixture, conn *sql.Conn, north, south *http.Cookie, password string, northEvents, southEvents testEventStream) {
	t.Helper()
	secondTab := openSSE(t, f, south)
	expectVersion(t, secondTab, "1")
	f.request(t, http.MethodPost, "/api/logout", "", south, http.StatusNoContent)
	expectSSEEnd(t, southEvents)
	expectSSEEnd(t, secondTab)
	f.request(t, http.MethodGet, ssePath, "", south, http.StatusUnauthorized)
	f.request(t, http.MethodGet, "/api/devices", "", south, http.StatusUnauthorized)

	login, err := json.Marshal(map[string]string{"username": "north", "password": password})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := requestWithSSEView(t, f, http.MethodGet, "/api/session", "", north, http.StatusOK)
	oldView := expectSSESessionHeaders(t, conn, session, north)
	// INSERT 已經讀回期限但交易仍可能失敗。回滾後不得送出新 Cookie 或 session 標頭。
	dropFailure := failSSECommit(t, conn, "sessions")
	failed, _ := requestWithSSEView(t, f, http.MethodPost, "/api/login", string(login), north, http.StatusServiceUnavailable, oldView)
	if len(failed.Cookies()) != 0 ||
		failed.Header.Get(testSessionViewHeader) != "" ||
		failed.Header.Get(testSessionExpiryHeader) != "" {
		t.Fatal("failed session rotation sent new session metadata")
	}
	requestWithSSEView(t, f, http.MethodGet, "/api/session", "", north, http.StatusOK, oldView)
	dropFailure()
	rotated, _ := requestWithSSEView(t, f, http.MethodPost, "/api/login", string(login), north, http.StatusOK, oldView)
	cookies := rotated.Cookies()
	if len(cookies) != 1 ||
		cookies[0].Name != sessionCookie ||
		cookies[0].Value == north.Value {
		t.Fatal("login did not rotate session")
	}
	expectSSEEnd(t, northEvents)
	f.request(t, http.MethodGet, ssePath, "", north, http.StatusUnauthorized)
	// 在訂閱前寫入短絕對期限，驗證活動連線會自行關閉，不必再有業務變動。
	replacement := cookies[0]
	newView := expectSSESessionHeaders(t, conn, rotated, replacement)
	if newView == oldView {
		t.Fatal("session rotation retained the previous view")
	}
	digest := sessionHash(&http.Request{Header: http.Header{"Cookie": {replacement.String()}}})
	if _, err := conn.ExecContext(t.Context(), "UPDATE sessions SET expires_at = statement_timestamp() + interval '2 seconds' WHERE token_hash = $1", digest); err != nil {
		t.Fatal(err)
	}
	expiring := openSSE(t, f, replacement)
	event := expectVersion(t, expiring, "6")
	if event.Viewer != newView || !event.ExpiresAt.Equal(sseSessionExpiry(t, conn, replacement)) {
		t.Fatal("replacement stream metadata did not match the current session")
	}
	expectSSEEnd(t, expiring)
	f.request(t, http.MethodGet, ssePath, "", replacement, http.StatusUnauthorized)
	f.request(t, http.MethodGet, "/api/session", "", replacement, http.StatusUnauthorized)
}

// failSSECommit 讓指定資料表的測試交易到提交時才失敗，以區分寫入成功與提交成功。
func failSSECommit(t *testing.T, conn *sql.Conn, table string) func() {
	t.Helper()
	simulationSQL(t, conn, `CREATE OR REPLACE FUNCTION reject_sse_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'forced SSE commit failure'; END $$`)
	simulationSQL(t, conn, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER reject_sse_commit AFTER INSERT OR UPDATE ON %s
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_sse_commit()`, table))
	// 完成回滾案例後移除限制，讓同一筆變更可以正常重試。
	return func() {
		simulationSQL(t, conn, "DROP TRIGGER reject_sse_commit ON "+table)
	}
}

// sseSessionExpiry 讀取測試 Cookie 的持久期限，核對訊框使用同一個絕對時間。
func sseSessionExpiry(t *testing.T, conn *sql.Conn, cookie *http.Cookie) time.Time {
	t.Helper()
	digest := sessionHash(&http.Request{Header: http.Header{"Cookie": {cookie.String()}}})
	var expiry time.Time
	if err := conn.QueryRowContext(t.Context(), "SELECT expires_at FROM sessions WHERE token_hash = $1", digest).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	return expiry.UTC()
}

// openSSE 使用獨立且有時限的 HTTP 請求保留連線，讓其他操作可以同時提交。
func openSSE(t *testing.T, f *measurementFixture, cookie *http.Cookie) testEventStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+ssePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = f.app.host
	req.Header.Set("Origin", f.app.origin)
	req.AddCookie(cookie)
	client := &http.Client{Transport: f.server.Client().Transport}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// 測試失敗或結束都先釋放串流，讓伺服器關閉可以完成。
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK ||
		res.Header.Get("Content-Type") != "text/event-stream" ||
		res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected SSE response: %d, %v", res.StatusCode, res.Header)
	}
	return testEventStream{bufio.NewReader(res.Body), cancel}
}

// expectVersion 只接受預期的下一個完整版本訊框，藉此偵測沒有授權或尚未提交的額外通知。
func expectVersion(t *testing.T, stream testEventStream, version string) (event testEventVersion) {
	t.Helper()
	for {
		line, err := stream.reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "event: heartbeat\n" {
			expectSSEHeartbeatData(t, stream)
			continue
		}
		if line != "event: version\n" {
			t.Fatalf("unexpected event line %q", line)
		}
		data, err := stream.reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		payload, ok := strings.CutPrefix(data, "data: ")
		if !ok {
			t.Fatalf("unexpected data line %q", data)
		}
		decoder := json.NewDecoder(strings.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Version != version ||
			!validSecret(event.Viewer) ||
			!event.ExpiresAt.After(time.Now()) {
			t.Fatalf("got version %q, want %q with an active session view and expiry", event.Version, version)
		}
		if end, err := stream.reader.ReadString('\n'); err != nil || end != "\n" {
			t.Fatalf("expected event terminator, got %q: %v", end, err)
		}
		return event
	}
}

// expectSSEEnd 要求伺服器結束連線，遇到排入的版本或測試端逾時都視為失敗。
func expectSSEEnd(t *testing.T, stream testEventStream) {
	t.Helper()
	if line, err := stream.reader.ReadString('\n'); !errors.Is(err, io.EOF) || line != "" {
		t.Fatalf("expected server EOF without another event, got %q: %v", line, err)
	}
}
