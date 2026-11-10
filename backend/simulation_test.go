package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const simulationBeforeFile = "before-simulation-restart.json"

type observedSimulationSample struct {
	sample  Measurement
	adopted bool
	err     error
}

type simulationRestartEvidence struct {
	ClusterID     int64
	StartedAt     time.Time
	API           string
	Active        Measurement
	Unanswered    Measurement
	FirstExpected int64
}

// TestSimulationIntegration 經真實 HTTP、測量入口及 PostgreSQL 驗證模擬控制、重送與競爭結果。
func TestSimulationIntegration(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	if target.relations != 0 {
		t.Fatalf("expected empty disposable database, found %d public relations", target.relations)
	}
	ctx, conn := t.Context(), target.conn
	state := openMeasurementTestState(t, true)
	startupSimulationFixture(t, conn, state)
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}

	app := measurementTestApplication(conn)
	held := make(chan Measurement, 1)
	release := make(chan struct{}, 1)
	observations := make(chan observedSimulationSample, 24)
	// 攔住指定設備的首筆送出，讓停止或取消能在提交後、樣本採用前發生。
	app.simulator = newSimulator(ctx, func(ctx context.Context, sample Measurement) (bool, error) {
		if sample.Sequence == 0 &&
			(sample.DeviceID == "north-02" ||
				sample.DeviceID == "north-03") {
			select {
			case held <- sample:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			select {
			case <-release:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		// 通過測量入口後回報原始樣本及結果，讓測試看得到遭拒的送出。
		adopted, err := app.SubmitMeasurement(ctx, sample)
		select {
		case observations <- observedSimulationSample{sample, adopted, err}:
		case <-ctx.Done():
		}
		return adopted, err
	})
	t.Cleanup(app.simulator.close)
	f := serveMeasurementFixture(t, app)
	f.hq = f.login(t, "hq", passwords["hq"])
	expectSimulation(t, f, "north-01", simulationState{"stopped", "0", false})

	// 權限或輸入錯誤不得變更控制狀態，也不得新增歷程。
	if !t.Run("authorization and strict HTTP inputs", func(t *testing.T) {
		testSimulationHTTP(t, f, passwords)
		expectSimulation(t, f, "north-01", simulationState{"stopped", "0", false})
		expectSimulationHistory(t, conn, "north-01", "")
	}) {
		return
	}

	// 驗證開始後立即送出指定溫度，且重送不改變任一設備的週期。
	if !t.Run("immediate temperature, independent cadence and exact replay", func(t *testing.T) {
		controlSimulation(t, f, "north-01", "normal", "0", simulationState{"normal", "1", true})
		first := expectSimulationSample(t, observations, "north-01", 23, true)
		expectLatestSimulationSample(t, f, first)

		// 刻意錯開兩台設備的開始時點，並在兩次回報之間重送第一台的原始回報。
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		<-timer.C
		controlSimulation(t, f, "south-02", "abnormal", "0", simulationState{"abnormal", "1", true})
		south := expectSimulationSample(t, observations, "south-02", 30, true)
		before := f.snapshot(t)
		controlSimulation(t, f, "north-01", "replay", "1", simulationState{"normal", "2", true})
		if replay := expectSimulationSample(t, observations, "north-01", 23, false); replay != first {
			t.Fatalf("replay changed original sample: got %+v, want %+v", replay, first)
		}
		if got := f.snapshot(t); got != before {
			t.Fatal("replay changed latest measurements or alerts")
		}
		// 用各設備的第一筆為基準驗證下一筆時間、順序和執行識別。
		for _, original := range []Measurement{first, south} {
			next := expectSimulationSample(t, observations, original.DeviceID, original.Temperature, true)
			elapsed := next.GeneratedAt.Sub(original.GeneratedAt)
			if next.Sequence != original.Sequence+1 ||
				next.RunID != original.RunID ||
				elapsed < 5*time.Second ||
				elapsed > 5500*time.Millisecond {
				t.Fatalf("report did not follow its own five-second cadence: elapsed=%s first=%+v next=%+v", elapsed, original, next)
			}
			expectLatestSimulationSample(t, f, next)
		}
		controlSimulation(t, f, "south-02", "stop", "1", simulationState{"stopped", "2", true})
		controlSimulation(t, f, "north-01", "stop", "2", simulationState{"stopped", "3", true})
	}) {
		return
	}

	// 讓兩個總部 session 競爭同一版本，並驗證歷程寫入失敗會回滾控制。
	if !t.Run("CAS concurrency and failed control rollback", func(t *testing.T) {
		secondSession := f.login(t, "hq", passwords["hq"])
		results := make(chan simulationHTTPResult, 2)
		start := make(chan struct{})
		for _, attempt := range []struct {
			action string
			cookie *http.Cookie
		}{{"normal", f.hq}, {"abnormal", secondSession}} {
			// 背景請求等同一個放行訊號，使兩次操作都以相同版本起跑。
			go func() {
				<-start
				results <- sendSimulation(t.Context(), f, "south-01", simulationBody(attempt.action, "0"), attempt.cookie, true)
			}()
		}
		close(start)
		outcomes := []simulationHTTPResult{waitForSimulation(t, results), waitForSimulation(t, results)}
		// 按狀態碼排列兩個結果，使核對不受請求完成順序影響。
		slices.SortFunc(outcomes, func(a, b simulationHTTPResult) int { return a.status - b.status })
		if outcomes[0].err != nil ||
			outcomes[1].err != nil ||
			outcomes[0].status != http.StatusOK ||
			outcomes[1].status != http.StatusConflict ||
			outcomes[1].body != `{"error":"conflict"}` {
			t.Fatalf("expected one accepted control and one CAS conflict: %+v", outcomes)
		}
		// 由成功的回應判斷應採用的溫度，對照實際樣本及唯一一筆歷程。
		var winner simulationState
		if err := json.Unmarshal([]byte(outcomes[0].body), &winner); err != nil {
			t.Fatal(err)
		}
		temperature := 23.0
		if winner.Mode == "abnormal" {
			temperature = 30
		} else if winner.Mode != "normal" {
			t.Fatalf("unexpected winning mode: %+v", winner)
		}
		raw := expectSimulationSample(t, observations, "south-01", temperature, true)
		expectLatestSimulationSample(t, f, raw)
		expectSimulation(t, f, "south-01", simulationState{winner.Mode, "1", true})
		expectSimulationHistory(t, conn, "south-01", winner.Mode)

		// 讓停止操作在寫歷程時失敗，核對原模式、執行及測量均保留。
		simulationSQL(t, conn, `
			ALTER TABLE simulation_history ADD CONSTRAINT test_control_rollback
			CHECK (device_id <> 'south-01' OR action <> 'stop')
		`)
		before := f.snapshot(t)
		expectSimulationError(t, f, "south-01", simulationBody("stop", "1"), f.hq, 503, "service_unavailable")
		expectSimulation(t, f, "south-01", simulationState{winner.Mode, "1", true})
		expectSimulationHistory(t, conn, "south-01", winner.Mode)
		if run := simulationRunID(t, conn, "south-01"); run != raw.RunID {
			t.Fatalf("failed control changed active run: %d", run)
		}
		if got := f.snapshot(t); got != before {
			t.Fatal("failed control changed latest measurements or alerts")
		}
		simulationSQL(t, conn, "ALTER TABLE simulation_history DROP CONSTRAINT test_control_rollback")
		controlSimulation(t, f, "south-01", "stop", "1", simulationState{"stopped", "2", true})
		// 等過下一個回報時點，確認停止後連會被測量入口拒絕的新回報也沒有產生。
		timer := time.NewTimer(simulationInterval + 250*time.Millisecond)
		defer timer.Stop()
		select {
		case unexpected := <-observations:
			t.Fatalf("stopped simulator generated a report: %+v", unexpected)
		case <-timer.C:
		}
		// 停止後仍可原樣重送，但原執行已無採用資格，舊控制版本也應被拒絕。
		controlSimulation(t, f, "south-01", "replay", "2", simulationState{"stopped", "3", true})
		if replay := expectSimulationSample(t, observations, "south-01", temperature, false); replay != raw {
			t.Fatalf("stopping changed original sample: got %+v, want %+v", replay, raw)
		}
		expectSimulationError(t, f, "south-01", simulationBody("replay", "2"), f.hq, 409, "conflict")
		expectSimulationHistory(t, conn, "south-01", winner.Mode+",stop,replay")
		if got := f.snapshot(t); got != before {
			t.Fatal("stop/replay changed latest measurements or alerts")
		}
	}) {
		return
	}

	// 暫停首筆送出並先停止執行，驗證舊樣本無法採用且首筆期待不變。
	if !t.Run("old pending first report and durable first expectation", func(t *testing.T) {
		pending := make(chan simulationHTTPResult, 1)
		// 背景開始操作，讓測試可在送出首筆被攔住時呼叫停止。
		go func() {
			pending <- sendSimulation(t.Context(), f, "north-02", simulationBody("normal", "0"), f.hq, true)
		}()
		first := waitForSimulation(t, held)
		if first.DeviceID != "north-02" || first.Temperature != 23 {
			t.Fatalf("unexpected pending first sample: %+v", first)
		}
		expectedAt := simulationFirstExpected(t, conn, "north-02")
		before := f.snapshot(t)
		controlSimulation(t, f, "north-02", "stop", "1", simulationState{"stopped", "2", true})
		release <- struct{}{}
		expectSimulationHTTPState(t, waitForSimulation(t, pending), simulationState{"normal", "1", true})
		if rejected := expectSimulationSample(t, observations, "north-02", 23, false); rejected != first {
			t.Fatalf("old pending sample changed before delivery: %+v", rejected)
		}
		if f.snapshot(t) != before || simulationFirstExpected(t, conn, "north-02") != expectedAt {
			t.Fatal("stopping or processing an old run changed first expectation or measurements")
		}

		// 重新開始後故意拒絕首筆，再確認重送同一原始樣本可以採用。
		unanswered := startFailingFirstReport(t, f, conn, observations, "north-02", "abnormal", "2", "3")
		if f.snapshot(t) != before || simulationFirstExpected(t, conn, "north-02") != expectedAt {
			t.Fatal("new run with a failed first report lost expectation or partially wrote measurements")
		}
		controlSimulation(t, f, "north-02", "replay", "3", simulationState{"abnormal", "4", true})
		if replay := expectSimulationSample(t, observations, "north-02", 30, true); replay != unanswered {
			t.Fatalf("replay did not forward the rejected original: %+v", replay)
		}
		controlSimulation(t, f, "north-02", "stop", "4", simulationState{"stopped", "5", true})
		expectSimulationHistory(t, conn, "north-02", "normal,stop,abnormal,replay,stop")
	}) {
		return
	}

	// 在控制已提交但首筆尚未送出時取消請求，驗證服務仍完成已提交的送出。
	if !t.Run("committed delivery survives request cancellation", func(t *testing.T) {
		request, cancel := context.WithCancel(t.Context())
		defer cancel()
		pending := make(chan simulationHTTPResult, 1)
		// 用可取消的背景請求送出開始控制，讓主測試掌握取消時點。
		go func() {
			pending <- sendSimulation(request, f, "north-03", simulationBody("normal", "0"), f.hq, true)
		}()
		first := waitForSimulation(t, held)
		cancel()
		if result := waitForSimulation(t, pending); !errors.Is(result.err, context.Canceled) {
			t.Fatalf("expected a canceled HTTP request, got %+v", result)
		}
		release <- struct{}{}
		if delivered := expectSimulationSample(t, observations, "north-03", 23, true); delivered != first {
			t.Fatalf("canceled HTTP client changed accepted delivery: %+v", delivered)
		}
		expectLatestSimulationSample(t, f, first)
		expectSimulation(t, f, "north-03", simulationState{"normal", "1", true})
		controlSimulation(t, f, "north-03", "stop", "1", simulationState{"stopped", "2", true})
	}) {
		return
	}

	// 切換模式要換執行識別並延續順序，舊執行即使送來更新的樣本也不生效。
	controlSimulation(t, f, "north-01", "normal", "3", simulationState{"normal", "4", true})
	old := expectSimulationSample(t, observations, "north-01", 23, true)
	controlSimulation(t, f, "north-01", "abnormal", "4", simulationState{"abnormal", "5", true})
	current := expectSimulationSample(t, observations, "north-01", 30, true)
	if old.RunID == current.RunID || old.Sequence >= current.Sequence {
		t.Fatalf("switch reused an execution identity or sequence: previous=%+v current=%+v", old, current)
	}
	beforeSwitchProbe := f.snapshot(t)
	old.GeneratedAt, old.Sequence = time.Now(), current.Sequence+1
	f.submit(t, old, false)
	if got := f.snapshot(t); got != beforeSwitchProbe {
		t.Fatal("old execution changed data after switch")
	}
	expectSimulationHistory(t, conn, "north-01", "normal,replay,stop,normal,abnormal")

	// 結束模擬程序時仍保有一台尚無有效首筆的設備，供下個程序驗證重啟。
	unanswered := startFailingFirstReport(t, f, conn, observations, "south-03", "normal", "0", "1")
	app.simulator.close()
	if got := latestSimulationSample(t, f, "south-03"); got != nil {
		t.Fatalf("unexpected valid measurement before restart: %+v", got)
	}
	evidence := simulationRestartEvidence{
		ClusterID: target.clusterID, StartedAt: target.startedAt, API: f.snapshot(t),
		Active: current, Unanswered: unanswered, FirstExpected: simulationFirstExpected(t, conn, "south-03"),
	}
	file, err := state.OpenFile(simulationBeforeFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := json.NewEncoder(file).Encode(evidence)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

// TestSimulationRestart 對照重啟前資料，確認舊執行失效、回報順序延續及首筆期待保留。
func TestSimulationRestart(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	state := openMeasurementTestState(t, false)
	data, err := readPrivateFile(state, simulationBeforeFile)
	if err != nil {
		t.Fatal(err)
	}
	var before simulationRestartEvidence
	if err := json.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	// 確認同一已有資料的叢集真的重啟，再開始比對。
	if before.API == "" ||
		before.StartedAt.IsZero() ||
		target.relations == 0 ||
		target.clusterID != before.ClusterID ||
		!target.startedAt.After(before.StartedAt) {
		t.Fatalf("expected restart of seeded PostgreSQL cluster, previous start %s, current %s", before.StartedAt, target.startedAt)
	}
	t.Logf("PostgreSQL postmaster restarted: %s -> %s", before.StartedAt, target.startedAt)
	// 沿用啟動步驟及新模擬器，檢查模式停止、版本更新和無原始樣本可重送。
	ctx, conn := t.Context(), target.conn
	startupSimulationFixture(t, conn, state)
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}
	app := measurementTestApplication(conn)
	app.simulator = newSimulator(ctx, app.SubmitMeasurement)
	t.Cleanup(app.simulator.close)
	f := serveMeasurementFixture(t, app)
	f.hq = f.login(t, "hq", passwords["hq"])
	expectSimulation(t, f, "north-01", simulationState{"stopped", "6", false})
	expectSimulation(t, f, "south-03", simulationState{"stopped", "2", false})
	expectSimulation(t, f, "north-02", simulationState{"stopped", "5", false})
	expectSimulationError(t, f, "north-01", simulationBody("normal", "5"), f.hq, 409, "conflict")
	expectSimulationError(t, f, "north-01", simulationBody("replay", "6"), f.hq, 409, "no_measurement")
	expectSimulationHistory(t, conn, "north-01", "normal,replay,stop,normal,abnormal")
	expectSimulationHistory(t, conn, "south-03", "normal")
	if simulationFirstExpected(t, conn, "south-03") != before.FirstExpected {
		t.Fatal("restart reset the unanswered first expectation")
	}
	// 更新舊樣本的時間與順序後重送，確認失效的是執行資格本身。
	for _, prior := range []Measurement{before.Active, before.Unanswered} {
		prior.GeneratedAt = time.Now()
		prior.Sequence++
		f.submit(t, prior, false)
	}
	if got := f.snapshot(t); got != before.API {
		t.Fatal("restart or prior execution changed latest measurements or alerts")
	}
	// 重新啟動設備應換執行識別、接續順序，並保留未首報設備的原始期待。
	controlSimulation(t, f, "north-01", "normal", "6", simulationState{"normal", "7", true})
	if newest := latestSimulationSample(t, f, "north-01"); newest == nil ||
		newest.Temperature != 23 ||
		newest.Sequence <= before.Active.Sequence {
		t.Fatalf("new execution did not continue measurement order: %+v", newest)
	}
	if run := simulationRunID(t, conn, "north-01"); run == before.Active.RunID {
		t.Fatalf("new execution reused prior run identity: %d", run)
	}
	controlSimulation(t, f, "south-03", "abnormal", "2", simulationState{"abnormal", "3", true})
	if simulationFirstExpected(t, conn, "south-03") != before.FirstExpected {
		t.Fatal("new start after restart reset the unanswered first expectation")
	}
}

// testSimulationHTTP 送出不同身分及錯誤輸入，驗證只有有效總部 session 能依嚴格契約操作。
func testSimulationHTTP(t *testing.T, f *measurementFixture, passwords map[string]string) {
	t.Helper()
	path := simulationPath("north-01")
	normal := simulationBody("normal", "0")
	// 同場域與跨場域帳號都沒有控制權，找不到設備也不能洩漏查詢結果。
	for _, username := range []string{"north", "south"} {
		site := f.login(t, username, passwords[username])
		f.request(t, http.MethodGet, path, "", site, http.StatusForbidden)
		expectSimulationError(t, f, "north-01", normal, site, 403, "forbidden")
		expectSimulationError(t, f, "missing", normal, site, 403, "forbidden")
	}
	f.request(t, http.MethodGet, path, "", nil, http.StatusUnauthorized)
	expectSimulationError(t, f, "north-01", normal, nil, 401, "unauthenticated")
	f.request(t, http.MethodGet, simulationPath("missing"), "", f.hq, http.StatusNotFound)
	expectSimulationError(t, f, "missing", normal, f.hq, 404, "not_found")
	expectSimulationError(t, f, "north-01", simulationBody("replay", "0"), f.hq, 409, "no_measurement")
	expectSimulationError(t, f, "north-01", simulationBody("stop", "1"), f.hq, 409, "conflict")
	if result := sendSimulation(t.Context(), f, "north-01", normal, f.hq, false); result.err != nil ||
		result.status != http.StatusForbidden ||
		result.body != `{"error":"forbidden"}` {
		t.Fatalf("missing CSRF protection accepted: %+v", result)
	}
	f.request(t, http.MethodGet, path+"?role=headquarters", "", f.hq, http.StatusBadRequest)
	f.request(t, http.MethodGet, path, "{}", f.hq, http.StatusBadRequest)
	f.request(t, http.MethodPost, path+"?siteId=north", normal, f.hq, http.StatusBadRequest)
	f.request(t, http.MethodPost, path, strings.Repeat(" ", 16385), f.hq, http.StatusRequestEntityTooLarge)
	f.request(t, http.MethodPost, "/api/measurements", `{"deviceId":"north-01","temperature":99}`, f.hq, http.StatusNotFound)
	// 逐一核對非正規版本、未知操作、重複欄位及非法編碼。
	for _, invalid := range []string{
		`{}`, `{"action":"normal","version":0}`, `{"action":"normal","version":"00"}`,
		`{"action":"normal","version":"+0"}`, `{"action":"normal","version":"-0"}`,
		`{"action":"normal","version":"9223372036854775808"}`, `{"action":"unknown","version":"0"}`,
		`{"action":"normal","version":"0","temperature":27}`, `{"action":"normal","action":"stop","version":"0"}`,
		`{"action":"normal","version":"0","version":"1"}`, normal + `{}`,
		"{\"action\":\"normal\xff\",\"version\":\"0\"}",
	} {
		expectSimulationError(t, f, "north-01", invalid, f.hq, 400, "invalid_input")
	}
	// 正式登出後重用舊 Cookie，確認控制交易會重新驗證 session。
	revoked := f.login(t, "hq", passwords["hq"])
	f.request(t, http.MethodPost, "/api/logout", "", revoked, http.StatusNoContent)
	expectSimulationError(t, f, "north-01", normal, revoked, 401, "unauthenticated")
}

// startupSimulationFixture 重用正式初始化與停用舊執行步驟，讓整合測試採同一重啟流程。
func startupSimulationFixture(t *testing.T, conn *sql.Conn, state *os.Root) {
	t.Helper()
	if err := bootstrap(t.Context(), conn, state); err != nil {
		t.Fatal(err)
	}
	if err := stopSimulationsOnStartup(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
}

// simulationPath 組成正式的設備控制路徑，供讀取及控制請求共用。
func simulationPath(id string) string {
	return "/api/devices/" + id + "/simulation"
}

// simulationBody 產生控制 API 要求的 JSON，保留版本為字串。
func simulationBody(action, version string) string {
	return fmt.Sprintf(`{"action":%q,"version":%q}`, action, version)
}

// expectSimulation 從 HTTP 讀回控制狀態，確認寫入後或重啟時的可見結果。
func expectSimulation(t *testing.T, f *measurementFixture, id string, want simulationState) {
	t.Helper()
	body := f.get(t, simulationPath(id))
	expectSimulationHTTPState(t, simulationHTTPResult{status: http.StatusOK, body: string(body)}, want)
}

// controlSimulation 以總部身分執行真實控制，並核對成功回應包含正確版本。
func controlSimulation(t *testing.T, f *measurementFixture, id, action, version string, want simulationState) {
	t.Helper()
	body, _ := f.request(t, http.MethodPost, simulationPath(id), simulationBody(action, version), f.hq, http.StatusOK)
	expectSimulationHTTPState(t, simulationHTTPResult{status: http.StatusOK, body: string(body)}, want)
}

// expectSimulationHTTPState 比對完整成功回應，避免漏掉模式、版本或重送資格。
func expectSimulationHTTPState(t *testing.T, result simulationHTTPResult, want simulationState) {
	t.Helper()
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if result.err != nil ||
		result.status != http.StatusOK ||
		result.body != string(expected) {
		t.Fatalf("unexpected simulation result: %+v, want %s", result, expected)
	}
}

// expectSimulationError 同時確認 HTTP 狀態與錯誤碼，區分控制拒絕原因。
func expectSimulationError(t *testing.T, f *measurementFixture, id, input string, cookie *http.Cookie, status int, code string) {
	t.Helper()
	body, _ := f.request(t, http.MethodPost, simulationPath(id), input, cookie, status)
	if string(body) != fmt.Sprintf(`{"error":%q}`, code) {
		t.Fatalf("unexpected simulation error: %s", body)
	}
}

type simulationHTTPResult struct {
	status int
	body   string
	err    error
}

// sendSimulation 保留 HTTP 回應及傳輸錯誤，讓併發案例能檢查衝突、CSRF 或取消。
func sendSimulation(ctx context.Context, f *measurementFixture, id, body string, cookie *http.Cookie, csrf bool) simulationHTTPResult {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+simulationPath(id), strings.NewReader(body))
	if err != nil {
		return simulationHTTPResult{err: err}
	}
	// 重現正式 Host，依案例選擇是否加入防護標頭。
	request.Host = f.app.host
	request.AddCookie(cookie)
	request.Header.Set("Content-Type", "application/json")
	if csrf {
		request.Header.Set("Origin", measurementTestOrigin)
		request.Header.Set("X-CSRF-Protection", "1")
	}
	// 保留傳輸錯誤及回應資料，讓案例決定如何判讀。
	response, err := f.server.Client().Do(request)
	if err != nil {
		return simulationHTTPResult{err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return simulationHTTPResult{status: response.StatusCode, body: string(data), err: err}
}

// waitForSimulation 在有界期限內等候觀察結果，避免背景工作失敗時讓測試卡住。
func waitForSimulation[T any](t *testing.T, input <-chan T) T {
	t.Helper()
	timer := time.NewTimer(7 * time.Second)
	defer timer.Stop()
	select {
	case value := <-input:
		return value
	case <-timer.C:
		t.Fatal("timed out waiting for simulated report or control response")
		var zero T
		return zero
	}
}

// expectSimulationSample 檢查真正產生的樣本與採用結果，區分有送出與有寫入。
func expectSimulationSample(t *testing.T, input <-chan observedSimulationSample, id string, temperature float64, adopted bool) Measurement {
	t.Helper()
	got := waitForSimulation(t, input)
	if got.err != nil ||
		got.adopted != adopted ||
		got.sample.DeviceID != id ||
		got.sample.Source != SimulatorMeasurementSource ||
		got.sample.Temperature != temperature {
		t.Fatalf("unexpected simulated report for %s: %+v", id, got)
	}
	return got.sample
}

// startFailingFirstReport 以資料庫限制故意拒絕首筆回報，留下已啟動但未有有效測量的設備。
func startFailingFirstReport(t *testing.T, f *measurementFixture, conn *sql.Conn, observed <-chan observedSimulationSample, id, action, version, next string) Measurement {
	t.Helper()
	simulationSQL(t, conn, "ALTER TABLE latest_measurements ADD CONSTRAINT test_first_report_failure CHECK (device_id <> '"+id+"')")
	controlSimulation(t, f, id, action, version, simulationState{action, next, true})
	// 等待採用結果後核對指定資料庫限制，排除其他錯誤被當成預期失敗。
	got := waitForSimulation(t, observed)
	var pgErr *pgconn.PgError
	if got.sample.DeviceID != id ||
		got.adopted ||
		!errors.As(got.err, &pgErr) ||
		pgErr.ConstraintName != "test_first_report_failure" {
		t.Fatalf("expected first measurement write failure for %s: %+v", id, got)
	}
	simulationSQL(t, conn, "ALTER TABLE latest_measurements DROP CONSTRAINT test_first_report_failure")
	return got.sample
}

// latestSimulationSample 從設備明細讀出最後採用的樣本，保留尚無樣本的情況。
func latestSimulationSample(t *testing.T, f *measurementFixture, id string) *measurementItem {
	t.Helper()
	var device deviceItem
	if err := json.Unmarshal(f.get(t, "/api/devices/"+id), &device); err != nil {
		t.Fatal(err)
	}
	return device.LatestMeasurement
}

// expectLatestSimulationSample 確認 API 保存的是指定樣本的值、順序和產生時間。
func expectLatestSimulationSample(t *testing.T, f *measurementFixture, want Measurement) {
	t.Helper()
	got := latestSimulationSample(t, f, want.DeviceID)
	if got == nil ||
		got.Temperature != want.Temperature ||
		got.Sequence != want.Sequence ||
		!got.GeneratedAt.Equal(want.GeneratedAt) {
		t.Fatalf("unexpected latest simulated sample: %+v, want %+v", got, want)
	}
}

// simulationSQL 在測試庫執行夾具設定或還原，資料庫錯誤立即使案例失敗。
func simulationSQL(t *testing.T, conn *sql.Conn, statement string) {
	t.Helper()
	if _, err := conn.ExecContext(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
}

// simulationFirstExpected 直接取得持久化的首筆期待起點，供停止及重啟前後比較。
func simulationFirstExpected(t *testing.T, conn *sql.Conn, id string) int64 {
	t.Helper()
	var expected int64
	if err := conn.QueryRowContext(t.Context(), `
		SELECT first_expected_at_ns FROM device_simulations WHERE device_id = $1
	`, id).Scan(&expected); err != nil || expected <= 0 {
		t.Fatalf("expected a durable first-report baseline for %s: got %d, error=%v", id, expected, err)
	}
	return expected
}

// simulationRunID 讀取目前允許的執行識別，確認失敗或重啟沒有錯誤換用執行。
func simulationRunID(t *testing.T, conn *sql.Conn, id string) int64 {
	t.Helper()
	var run int64
	if err := conn.QueryRowContext(t.Context(), "SELECT run_id FROM measurement_runs WHERE device_id = $1", id).Scan(&run); err != nil {
		t.Fatal(err)
	}
	return run
}

// expectSimulationHistory 依版本核對操作順序、操作者及時間，確認控制交易完整保存。
func expectSimulationHistory(t *testing.T, conn *sql.Conn, id, want string) {
	t.Helper()
	var actions string
	var actorsAndTimes bool
	if err := conn.QueryRowContext(t.Context(), `
		SELECT COALESCE(string_agg(action, ',' ORDER BY version), ''),
			COALESCE(bool_and(actor = 'hq' AND occurred_at_ns > 0), true)
		FROM simulation_history WHERE device_id = $1
	`, id).Scan(&actions, &actorsAndTimes); err != nil ||
		actions != want ||
		!actorsAndTimes {
		t.Fatalf("unexpected control history for %s: got %q, actors/times valid=%v, error=%v; want %q", id, actions, actorsAndTimes, err, want)
	}
}
