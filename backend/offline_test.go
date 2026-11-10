package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const offlineBeforeFile = "before-offline-restart.json"

type offlineTestFixture struct {
	*measurementFixture
	clock atomic.Int64
}

// newOfflineFixture 以可控時鐘驅動真實離線掃描和測量採用，透過 HTTP 查驗可見結果。
func newOfflineFixture(t *testing.T, conn *sql.Conn, at time.Time, atRestart bool) (*offlineTestFixture, <-chan observedSimulationSample) {
	t.Helper()
	f := &offlineTestFixture{}
	f.setTime(at)
	app := measurementTestApplication(conn)
	// 讓回報與掃描共用可跨工作執行緒讀取的可控時間。
	app.measurementNow = func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
	// 以重啟模式補判後才架起 HTTP，重現啟動時的可見順序。
	if atRestart {
		if err := app.scanOffline(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	}
	observed := make(chan observedSimulationSample, 12)
	// 沿用真正的採用入口，另以通道記下原始樣本與採用結果。
	app.simulator = newSimulator(t.Context(), func(ctx context.Context, sample Measurement) (bool, error) {
		adopted, err := app.SubmitMeasurement(ctx, sample)
		select {
		case observed <- observedSimulationSample{sample, adopted, err}:
		case <-ctx.Done():
		}
		return adopted, err
	})
	t.Cleanup(app.simulator.close)
	f.measurementFixture = serveMeasurementFixture(t, app)
	return f, observed
}

// setTime 以原子寫入調整測試時間，讓回報與掃描可安全共用時計。
func (f *offlineTestFixture) setTime(at time.Time) {
	f.clock.Store(at.UnixNano())
}

// scan 在指定時間執行一般離線巡檢，讓測試能精準比較逾時前後。
func (f *offlineTestFixture) scan(t *testing.T, at time.Time) {
	t.Helper()
	f.setTime(at)
	if err := f.app.scanOffline(t.Context(), false); err != nil {
		t.Fatal(err)
	}
}

type offlineRestartEvidence struct {
	ClusterID      int64
	StartedAt      time.Time
	Clock          time.Time
	ExistingID     string
	LastReport     Measurement
	Unanswered     Measurement
	FirstExpected  int64
	ExistingBefore json.RawMessage
}

// TestOfflineIntegration 以真實 PostgreSQL 驗證離線時間邊界、有效恢復、交易回滾及併發順序。
func TestOfflineIntegration(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	if target.relations != 0 {
		t.Fatalf("expected empty disposable database, found %d public relations", target.relations)
	}
	ctx, conn := t.Context(), target.conn
	state := openMeasurementTestState(t, true)
	started := time.Date(2026, 11, 9, 10, 0, 0, 123456789, time.UTC)
	passwords := seedLegacyMeasurementFixture(t, conn, state)
	// 以第 03 票的原始欄位約束重建既有資料庫，確認新增離線告警不改動舊溫度紀錄。
	simulationSQL(t, conn, createMeasurementSchema+createSimulationSchema)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO alerts (device_id, type, temperature, detected_at_ns, recovered_at_ns)
		VALUES ('north-01', 'temperature', 0, $1, $1)
	`, started.UnixNano()); err != nil {
		t.Fatal(err)
	}
	startupSimulationFixture(t, conn, state)
	f, observed := newOfflineFixture(t, conn, started, false)
	f.hq = f.login(t, "hq", passwords["hq"])
	f.scan(t, started)
	expectDeviceOnlineStatus(t, f, "north-01", "unreported", nil)
	if got := f.get(t, "/api/alerts/1"); !strings.Contains(string(got), `"temperature":0`) ||
		strings.Contains(string(got), `"deadlineAt"`) ||
		strings.Contains(string(got), `"detectedAtRestart"`) {
		t.Fatalf("legacy temperature wire shape changed: %s", got)
	}

	// 首筆寫入故意失敗，驗證停止後仍沿用初次期待，並精確比對 15 秒邊界。
	if !t.Run("first expectation, strict cutoff and field scope", func(t *testing.T) {
		startFailingFirstReport(t, f.measurementFixture, conn, observed, "north-01", "normal", "0", "1")
		expectDeviceOnlineStatus(t, f, "north-01", "awaiting", nil)
		deadline := started.Add(15 * time.Second)
		f.setTime(started.Add(time.Second))
		controlSimulation(t, f.measurementFixture, "north-01", "stop", "1", simulationState{"stopped", "2", true})
		for _, clock := range []time.Time{deadline.Add(-time.Nanosecond), deadline} {
			f.scan(t, clock)
			expectDeviceOnlineStatus(t, f, "north-01", "awaiting", nil)
			offlineAlerts(t, f, "north-01", 0)
		}
		// 恰好 15 秒仍等待首筆，晚一奈秒即判定離線。下一輪巡檢應沿用同一筆告警。
		detected := deadline.Add(time.Nanosecond)
		f.scan(t, detected)
		expectDeviceOnlineStatus(t, f, "north-01", "offline", nil)
		first := offlineAlerts(t, f, "north-01", 1)[0]
		expectOfflineAlert(t, first, deadline, detected, nil, false)
		f.scan(t, deadline.Add(time.Second))
		expectSameOfflineAlert(t, offlineAlerts(t, f, "north-01", 1)[0], first)
		verifyOfflineScopes(t, f, passwords, first.ID)

		// 以重新啟動後的有效首筆恢復原告警，核對期待起點未被覆寫。
		recovery := deadline.Add(2 * time.Second)
		f.setTime(recovery)
		controlSimulation(t, f.measurementFixture, "north-01", "normal", "2", simulationState{"normal", "3", true})
		report := expectSimulationSample(t, observed, "north-01", 23, true)
		expectDeviceOnlineStatus(t, f, "north-01", "online", &report)
		recovered := offlineAlerts(t, f, "north-01", 1)[0]
		expectOfflineAlert(t, recovered, deadline, detected, &recovery, false)
		if recovered.ID != first.ID || simulationFirstExpected(t, conn, "north-01") != started.UnixNano() {
			t.Fatal("recovery replaced an alert or reset the original expectation")
		}
		controlSimulation(t, f.measurementFixture, "north-01", "stop", "3", simulationState{"stopped", "4", true})
	}) {
		return
	}

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO measurement_runs (device_id, data_generation, run_id)
		VALUES ('north-02', 1, 10), ('north-03', 1, 10), ('south-01', 1, 10)
	`); err != nil {
		t.Fatal(err)
	}
	// 先保留一筆超標測量，再確認離線後仍保留原溫度與告警。
	if !t.Run("invalid arrivals cannot recover and abnormal recovery keeps temperature alert", func(t *testing.T) {
		at := started.Add(20 * time.Second)
		first := offlineSample("north-02", 0, at, 30)
		f.setTime(at)
		f.submit(t, first, true)
		deadline := at.Add(15 * time.Second)
		f.scan(t, deadline)
		expectDeviceOnlineStatus(t, f, "north-02", "online", &first)
		offlineAlerts(t, f, "north-02", 0)
		detected := deadline.Add(time.Nanosecond)
		f.scan(t, detected)
		expectDeviceOnlineStatus(t, f, "north-02", "offline", &first)
		active := offlineAlerts(t, f, "north-02", 1)[0]
		expectOfflineAlert(t, active, deadline, detected, nil, false)
		temperature := temperatureAlert(t, f, "north-02")

		recovery := deadline.Add(time.Second)
		f.setTime(recovery)
		before := f.snapshot(t)
		// 每個回呼只破壞一項採用條件，逐一確認無效回報不改變 API 快照。
		for _, edit := range []func(*Measurement){
			func(m *Measurement) { m.RunID-- },
			func(m *Measurement) { m.DataGeneration++ },
			func(m *Measurement) { m.Sequence = 0 },
			func(m *Measurement) { m.GeneratedAt = at.Add(-time.Nanosecond) },
			func(m *Measurement) { m.GeneratedAt = recovery.Add(time.Nanosecond) },
		} {
			sample := offlineSample("north-02", 1, recovery, 23)
			edit(&sample)
			f.submit(t, sample, false)
			if got := f.snapshot(t); got != before {
				t.Fatal("ignored arrival changed the offline device, alerts, or latest measurements")
			}
		}
		// 有效但仍超標的回報只恢復離線，原溫度告警保持不變。
		next := offlineSample("north-02", 1, recovery, 31)
		f.submit(t, next, true)
		expectDeviceOnlineStatus(t, f, "north-02", "online", &next)
		done := offlineAlerts(t, f, "north-02", 1)[0]
		expectOfflineAlert(t, done, deadline, detected, &recovery, false)
		if done.ID != active.ID {
			t.Fatal("accepted measurement did not recover the existing offline alert")
		}
		expectSameOfflineAlert(t, temperatureAlert(t, f, "north-02"), temperature)
	}) {
		return
	}

	// 晚到回報應補記離線及恢復，並與最新測量及溫度告警共同提交。
	if !t.Run("late arrival catches up without a scan and rolls back atomically", func(t *testing.T) {
		at := started.Add(40 * time.Second)
		f.setTime(at)
		first := offlineSample("north-03", 0, at, 23)
		f.submit(t, first, true)
		deadline := at.Add(15 * time.Second)
		late := deadline.Add(time.Nanosecond)
		f.setTime(late)
		before := f.snapshot(t)
		// 用真正的 PostgreSQL 限制讓離線寫入失敗，檢查沒有部分提交。
		simulationSQL(t, conn, `
			ALTER TABLE alerts ADD CONSTRAINT test_offline_catchup_failure
			CHECK (device_id <> 'north-03' OR type <> 'offline')
		`)
		next := offlineSample("north-03", 1, late, 30)
		adopted, err := f.app.SubmitMeasurement(t.Context(), next)
		var pgErr *pgconn.PgError
		if adopted ||
			!errors.As(err, &pgErr) ||
			pgErr.ConstraintName != "test_offline_catchup_failure" {
			t.Fatalf("expected real offline alert write failure, got adopted=%v error=%v", adopted, err)
		}
		if got := f.snapshot(t); got != before {
			t.Fatal("offline write failure left a partial measurement or temperature alert")
		}
		// 移除限制後原樣重送，核對補記的期限及恢復時間。
		simulationSQL(t, conn, "ALTER TABLE alerts DROP CONSTRAINT test_offline_catchup_failure")
		f.submit(t, next, true)
		expectDeviceOnlineStatus(t, f, "north-03", "online", &next)
		firstOffline := offlineAlerts(t, f, "north-03", 1)[0]
		expectOfflineAlert(t, firstOffline, deadline, late, &late, false)

		// 下一次離線使用最新回報起算並建立獨立告警，舊紀錄保持原樣。
		newDeadline := late.Add(15 * time.Second)
		detected := newDeadline.Add(time.Nanosecond)
		f.scan(t, detected)
		alerts := offlineAlerts(t, f, "north-03", 2)
		expectOfflineAlert(t, alerts[0], newDeadline, detected, nil, false)
		expectSameOfflineAlert(t, alerts[1], firstOffline)
		if alerts[0].ID == alerts[1].ID {
			t.Fatal("repeat offline episode reused the recovered alert ID")
		}
	}) {
		return
	}

	// 讓恰好於期限抵達的回報排在晚到回報前，確認掃描不會誤判離線。
	if !t.Run("earlier queued receipt wins over scanner and later report", func(t *testing.T) {
		at := started.Add(80 * time.Second)
		f.setTime(at)
		f.submit(t, offlineSample("south-01", 0, at, 23), true)
		deadline := at.Add(15 * time.Second)
		early := offlineSample("south-01", 1, deadline, 23)
		later := offlineSample("south-01", 2, deadline.Add(time.Second), 23)
		reports, scan := scanWithOfflinePending(t, f, target, later.GeneratedAt, early, later)
		assertMeasurementResult(t, waitForSimulation(t, reports[0]), true)
		assertMeasurementResult(t, waitForSimulation(t, reports[1]), true)
		if err := waitForSimulation(t, scan); err != nil {
			t.Fatal(err)
		}
		offlineAlerts(t, f, "south-01", 0)
		expectDeviceOnlineStatus(t, f, "south-01", "online", &later)
	}) {
		return
	}

	// 讓準時抵達的回報排隊超過新鮮度期限，驗證同一輪掃描續判並建立離線告警。
	if !t.Run("same scan settles an expired on-deadline report", func(t *testing.T) {
		at := started.Add(97 * time.Second)
		initial := offlineSample("south-01", 3, at, 23)
		f.setTime(at)
		f.submit(t, initial, true)
		deadline := at.Add(15 * time.Second)
		detected := deadline.Add(measurementMaxAge + time.Nanosecond)
		reports, scan := scanWithOfflinePending(t, f, target, detected, offlineSample(initial.DeviceID, 4, deadline, 31))
		assertMeasurementResult(t, waitForSimulation(t, reports[0]), false)
		if err := waitForSimulation(t, scan); err != nil {
			t.Fatal(err)
		}
		expectDeviceOnlineStatus(t, f, initial.DeviceID, "offline", &initial)
		expectOfflineAlert(t, offlineAlerts(t, f, initial.DeviceID, 1)[0], deadline, detected, nil, false)
	}) {
		return
	}

	var existing alertItem
	// 安排停止操作先取得應用層連線，驗證舊執行回報被拒後，掃描仍會完成。
	if !t.Run("control wins queued run while timeout still completes", func(t *testing.T) {
		at := started.Add(120 * time.Second)
		f.setTime(at)
		controlSimulation(t, f.measurementFixture, "south-02", "normal", "0", simulationState{"normal", "1", true})
		initial := expectSimulationSample(t, observed, "south-02", 23, true)
		// 以設備控制列的鎖讓停止請求先進入資料庫但暫時無法提交。
		rowLock, err := target.pool.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer rowLock.Rollback()
		var version int64
		if err := rowLock.QueryRowContext(t.Context(), `
			SELECT version FROM device_simulations WHERE device_id = 'south-02' FOR UPDATE
		`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		control := make(chan simulationHTTPResult, 1)
		// 背景送出真實 HTTP 停止請求，主測試才能在交易被擋住時安排回報。
		go func() {
			control <- sendSimulation(t.Context(), f.measurementFixture, "south-02", simulationBody("stop", "1"), f.hq, true)
		}()
		// 觀察到 PostgreSQL 鎖等待才登記舊執行的準時回報，接著把時鐘推過離線期限。
		waitForMeasurementRowLock(t, target.pool, target.pid)
		deadline := at.Add(15 * time.Second)
		f.setTime(deadline)
		pending := initial
		pending.Sequence++
		pending.GeneratedAt = deadline
		submitted := f.submitAsync(t.Context(), pending)
		waitForOfflinePending(t, f.app, "south-02", 1)
		detected := deadline.Add(time.Nanosecond)
		f.setTime(detected)
		scan := make(chan error, 1)
		// 另起掃描等待連線，釋放資料列後讓停止、舊回報與掃描各自結束。
		go func() { scan <- f.app.scanOffline(t.Context(), false) }()
		if err := rowLock.Rollback(); err != nil {
			t.Fatal(err)
		}
		expectSimulationHTTPState(t, waitForSimulation(t, control), simulationState{"stopped", "2", true})
		assertMeasurementResult(t, waitForSimulation(t, submitted), false)
		if err := waitForSimulation(t, scan); err != nil {
			t.Fatal(err)
		}
		existing = offlineAlerts(t, f, "south-02", 1)[0]
		expectOfflineAlert(t, existing, deadline, detected, nil, false)
		expectDeviceOnlineStatus(t, f, "south-02", "offline", &initial)
	}) {
		return
	}

	// 以已有回報、尚未首報及已離線三種狀態留下重啟前證據。
	end := started.Add(140 * time.Second)
	f.setTime(end)
	last := offlineSample("north-03", 2, end, 31)
	f.submit(t, last, true)
	expectDeviceOnlineStatus(t, f, "north-03", "online", &last)
	unanswered := startFailingFirstReport(t, f.measurementFixture, conn, observed, "south-03", "abnormal", "0", "1")
	expectDeviceOnlineStatus(t, f, "south-03", "awaiting", nil)
	f.app.simulator.close()
	before := offlineRestartEvidence{
		ClusterID: target.clusterID, StartedAt: target.startedAt, Clock: end.Add(16 * time.Second),
		ExistingID: existing.ID, LastReport: last, Unanswered: unanswered,
		FirstExpected: simulationFirstExpected(t, conn, "south-03"), ExistingBefore: f.get(t, "/api/alerts/"+existing.ID),
	}
	file, err := state.OpenFile(offlineBeforeFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := json.NewEncoder(file).Encode(before)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

// TestOfflineRestart 對照重啟前的證據，驗證同一資料庫保留既有告警，並依兩種基準補判。
func TestOfflineRestart(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	state := openMeasurementTestState(t, false)
	data, err := readPrivateFile(state, offlineBeforeFile)
	if err != nil {
		t.Fatal(err)
	}
	var before offlineRestartEvidence
	if err := json.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	// 確認是同一有資料的叢集，且 PostgreSQL 已有較新的啟動時間。
	if before.Clock.IsZero() ||
		before.StartedAt.IsZero() ||
		before.ExistingID == "" ||
		target.relations == 0 ||
		target.clusterID != before.ClusterID ||
		!target.startedAt.After(before.StartedAt) {
		t.Fatalf("expected restart of seeded PostgreSQL cluster, previous start %s, current %s", before.StartedAt, target.startedAt)
	}
	t.Logf("PostgreSQL postmaster restarted: %s -> %s", before.StartedAt, target.startedAt)
	// 在原有資料上執行啟動路徑，對照原告警與新補判的回報、首筆期待兩種期限。
	conn := target.conn
	startupSimulationFixture(t, conn, state)
	f, observed := newOfflineFixture(t, conn, before.Clock, true)
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}
	f.hq = f.login(t, "hq", passwords["hq"])
	if got := f.get(t, "/api/alerts/"+before.ExistingID); !slices.Equal(got, before.ExistingBefore) {
		t.Fatalf("startup replaced or rewrote the existing offline alert: %s", got)
	}
	offlineAlerts(t, f, "south-02", 1)
	expectDeviceOnlineStatus(t, f, "north-03", "offline", &before.LastReport)
	latest := offlineAlerts(t, f, "north-03", 3)[0]
	expectOfflineAlert(t, latest, before.LastReport.GeneratedAt.Add(15*time.Second), before.Clock, nil, true)
	expectDeviceOnlineStatus(t, f, "south-03", "offline", nil)
	first := offlineAlerts(t, f, "south-03", 1)[0]
	expectOfflineAlert(t, first, time.Unix(0, before.FirstExpected).Add(15*time.Second), before.Clock, nil, true)
	if simulationFirstExpected(t, conn, "south-03") != before.FirstExpected {
		t.Fatal("startup reset the unanswered first expectation")
	}
	// 重啟後模擬應停止且不產生新樣本，舊執行即使時間和順序有效也應被拒絕。
	expectSimulation(t, f.measurementFixture, "south-03", simulationState{"stopped", "2", false})
	expectSimulation(t, f.measurementFixture, "south-02", simulationState{"stopped", "2", false})
	select {
	case report := <-observed:
		t.Fatalf("startup generated a fake measurement: %+v", report)
	default:
	}
	snapshot := f.snapshot(t)
	for _, old := range []Measurement{before.LastReport, before.Unanswered} {
		old.GeneratedAt = before.Clock
		old.Sequence++
		f.submit(t, old, false)
	}
	f.scan(t, before.Clock.Add(time.Nanosecond))
	if got := f.snapshot(t); got != snapshot {
		t.Fatal("old run or repeated scan changed restarted state")
	}
}

// offlineSample 建立符合離線測試預載執行識別的樣本，讓案例只控制順序、時間及溫度。
func offlineSample(id string, sequence int64, generated time.Time, temperature float64) Measurement {
	return Measurement{Source: SimulatorMeasurementSource, DeviceID: id, DataGeneration: 1, RunID: 10,
		Sequence: sequence, GeneratedAt: generated, Temperature: temperature}
}

// expectDeviceOnlineStatus 以設備明細與清單核對相同在線狀態，並確認保留最後有效樣本。
func expectDeviceOnlineStatus(t *testing.T, f *offlineTestFixture, id, status string, sample *Measurement) {
	t.Helper()
	var detail deviceItem
	body := f.get(t, "/api/devices/"+id)
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.OnlineStatus != status {
		t.Fatalf("device %s status=%q, want %q", id, detail.OnlineStatus, status)
	}
	// 依是否曾採用有效回報核對最後測量。此夾具以產生時間安排抵達。
	if sample == nil {
		if detail.LatestMeasurement != nil {
			t.Fatalf("device without a valid report exposed a latest measurement: %s", body)
		}
	} else {
		expectLatestSimulationSample(t, f.measurementFixture, *sample)
		if !detail.LatestMeasurement.ReceivedAt.Equal(sample.GeneratedAt) {
			t.Fatalf("device %s lost prequeue receipt: %s", id, body)
		}
	}
	// 以清單回應中的同一設備明細確認兩個入口一致。
	if list := f.get(t, "/api/devices"); !strings.Contains(string(list), string(body)) {
		t.Fatalf("device list did not match online detail: %s", list)
	}
}

// deviceAlerts 從總部可讀的告警頁篩出設備及類型，沿用 API 的排序與欄位。
func deviceAlerts(t *testing.T, f *offlineTestFixture, id, kind string) []alertItem {
	t.Helper()
	var page alertPage
	if err := json.Unmarshal(f.get(t, "/api/alerts"), &page); err != nil {
		t.Fatal(err)
	}
	var result []alertItem
	for _, alert := range page.Items {
		if alert.DeviceID == id && alert.Type == kind {
			result = append(result, alert)
		}
	}
	return result
}

// offlineAlerts 核對指定設備的離線告警數，讓案例能辨認漏報或重複告警。
func offlineAlerts(t *testing.T, f *offlineTestFixture, id string, count int) []alertItem {
	t.Helper()
	alerts := deviceAlerts(t, f, id, "offline")
	if len(alerts) != count {
		t.Fatalf("device %s offline history count=%d, want %d: %+v", id, len(alerts), count, alerts)
	}
	return alerts
}

// temperatureAlert 取得唯一有溫度證據的未恢復告警，供離線前後比較。
func temperatureAlert(t *testing.T, f *offlineTestFixture, id string) alertItem {
	t.Helper()
	alerts := deviceAlerts(t, f, id, "temperature")
	if len(alerts) != 1 ||
		alerts[0].Status != "active" ||
		alerts[0].Temperature == nil {
		t.Fatalf("expected one active temperature alert for %s: %+v", id, alerts)
	}
	return alerts[0]
}

// expectOfflineAlert 分別核對離線期限、實際發現、恢復及重啟標記，確認欄位各自有明確意義。
func expectOfflineAlert(t *testing.T, item alertItem, deadline, detected time.Time, recovered *time.Time, atRestart bool) {
	t.Helper()
	wantStatus := "active"
	if recovered != nil {
		wantStatus = "recovered"
	}
	if item.Temperature != nil ||
		item.DeadlineAt == nil ||
		!item.DeadlineAt.Equal(deadline) ||
		!item.DetectedAt.Equal(detected) ||
		item.DetectedAtRestart == nil ||
		*item.DetectedAtRestart != atRestart ||
		item.Status != wantStatus ||
		(item.RecoveredAt == nil) != (recovered == nil) ||
		(recovered != nil &&
			!item.RecoveredAt.Equal(*recovered)) {
		data, _ := json.Marshal(item)
		t.Fatalf("unexpected offline alert: %s; want deadline=%s detected=%s recovered=%v atRestart=%t",
			data, deadline, detected, recovered, atRestart)
	}
}

// expectSameOfflineAlert 比較序列化內容，讓不同指標位置但相同的證據仍可相等。
func expectSameOfflineAlert(t *testing.T, got, want alertItem) {
	t.Helper()
	actual, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actual, expected) {
		t.Fatalf("unexpected alert changes: got %s want %s", actual, expected)
	}
}

// waitForOfflinePending 在鎖內觀察真實佇列，確認指定數目的回報已抵達才推進案例。
func waitForOfflinePending(t *testing.T, app *application, id string, want int) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		app.arrivals.mu.Lock()
		count := len(app.arrivals.devices[id])
		app.arrivals.mu.Unlock()
		if count == want {
			return
		}
		select {
		case <-timeout.C:
			t.Fatalf("timed out waiting for %d pending reports on %s, found %d", want, id, count)
		case <-ticker.C:
		}
	}
}

// scanWithOfflinePending 讓掃描先取得資料庫操作權，同時保留已抵達但尚未處理的回報。
func scanWithOfflinePending(t *testing.T, f *offlineTestFixture, target measurementTestDatabase, at time.Time, samples ...Measurement) ([]<-chan measurementResult, <-chan error) {
	t.Helper()
	first := samples[0]
	f.setTime(first.GeneratedAt)
	// 先占住設備的入列順序，讓真實回報在掃描取得資料庫連線前已抵達。
	blocker, _ := f.app.receiveMeasurement(first.DeviceID, first.GeneratedAt)
	if blocker == nil {
		t.Fatal("could not establish pending report fixture")
	}
	defer f.app.arrivals.release(first.DeviceID, blocker)
	reports := make([]<-chan measurementResult, 0, len(samples))
	for _, sample := range samples {
		f.setTime(sample.GeneratedAt)
		reports = append(reports, f.submitAsync(t.Context(), sample))
		waitForOfflinePending(t, f.app, first.DeviceID, len(reports)+1)
	}

	// 以告警表的 SHARE 鎖擋住掃描寫入，確認掃描已先取得連線才放行隊首。
	rowLock, err := target.pool.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rowLock.Rollback()
	if _, err := rowLock.ExecContext(t.Context(), "LOCK TABLE alerts IN SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	f.setTime(at)
	scan := make(chan error, 1)
	// 從背景啟動掃描，使主測試能觀察 PostgreSQL 的鎖等待。
	go func() { scan <- f.app.scanOffline(t.Context(), false) }()
	waitForMeasurementRowLock(t, target.pool, target.pid)
	return reports, scan
}

// verifyOfflineScopes 對照總部、兩場域及未登入讀取，驗證離線告警與設備遵守既有場域權限。
func verifyOfflineScopes(t *testing.T, f *offlineTestFixture, passwords map[string]string, id string) {
	t.Helper()
	// 先確認同場域與總部讀到相同證據，越權及未登入則各走預定回應。
	north := f.login(t, "north", passwords["north"])
	south := f.login(t, "south", passwords["south"])
	path := "/api/alerts/" + id
	detail := f.get(t, path)
	own, _ := f.request(t, http.MethodGet, path, "", north, http.StatusOK)
	if !slices.Equal(detail, own) || strings.Contains(string(detail), `"temperature"`) {
		t.Fatalf("offline detail is not scoped or includes a dummy temperature: %s", own)
	}
	f.request(t, http.MethodGet, path, "", south, http.StatusNotFound)
	f.request(t, http.MethodGet, path, "", nil, http.StatusUnauthorized)
	f.request(t, http.MethodGet, "/api/devices/north-01", "", south, http.StatusNotFound)
	// 明細受限時清單也不得暴露該筆告警。
	for _, actor := range []struct {
		session *http.Cookie
		visible bool
	}{{north, true}, {south, false}} {
		body, _ := f.request(t, http.MethodGet, "/api/alerts", "", actor.session, http.StatusOK)
		if strings.Contains(string(body), `"id":"`+id+`"`) != actor.visible {
			t.Fatalf("offline alert list did not enforce site scope: %s", body)
		}
	}
}
