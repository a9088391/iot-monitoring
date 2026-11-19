package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
	"time"
)

const automaticBeforeFile = "before-automatic-restart.json"

type automaticRestartEvidence struct {
	ClusterID     int64
	StartedAt     time.Time
	Generation    int64
	FirstExpected int64
	Active        Measurement
	Stopped       Measurement
}

// TestNormalSimulationTemperature 驗證正常樣本各有基準、每筆小幅變化，且長時間執行不越界。
func TestNormalSimulationTemperature(t *testing.T) {
	values := make(map[float64]bool)
	for _, id := range []string{"north-01", "north-02", "north-03", "south-01", "south-02", "south-03"} {
		values[normalSimulationTemperature(id, 0)] = true
		previous := normalSimulationTemperature(id, 0)
		for sequence := int64(1); sequence <= 100; sequence++ {
			value := normalSimulationTemperature(id, sequence)
			if !validNormalSimulationSample(value) || math.Abs(value*10-math.Round(value*10)) > 1e-9 ||
				math.Abs(math.Abs(value-previous)-0.1) > 1e-9 {
				t.Fatalf("normal report lacks bounded one-decimal gradual variation: id=%s sequence=%d previous=%v current=%v", id, sequence, previous, value)
			}
			previous = value
		}
		if value := normalSimulationTemperature(id, math.MaxInt64); !validNormalSimulationSample(value) {
			t.Fatalf("large sequence escaped normal range: %v", value)
		}
	}
	if len(values) != 6 {
		t.Fatalf("initial device baselines should be distinct, got %d values", len(values))
	}
}

// TestNormalSimulationTemperaturesRemainDistinctAcrossPhases 驗證停止造成的不同回報進度也不能讓六台正常值相撞。
func TestNormalSimulationTemperaturesRemainDistinctAcrossPhases(t *testing.T) {
	t.Run("different persisted sequences", func(t *testing.T) {
		left := normalSimulationTemperature("north-03", 1)
		right := normalSimulationTemperature("north-02", 5)
		if left == right {
			t.Fatalf("independent device phases collided: north-03 sequence 1 = north-02 sequence 5 = %v", left)
		}
	})
	t.Run("independent phases", func(t *testing.T) {
		type origin struct {
			id       string
			sequence int64
		}
		values := make(map[float64]origin)
		for _, id := range []string{"north-01", "north-02", "north-03", "south-01", "south-02", "south-03"} {
			for sequence := int64(0); sequence <= 100; sequence++ {
				value := normalSimulationTemperature(id, sequence)
				if previous, exists := values[value]; exists && previous.id != id {
					t.Fatalf("independent device phases collided at %v: %s sequence %d and %s sequence %d", value, previous.id, previous.sequence, id, sequence)
				}
				values[value] = origin{id, sequence}
			}
		}
	})
}

// TestAutomaticSimulationIntegration 在無瀏覽器或登入時走正式啟動與採用入口，觀察六台設備的首筆及兩輪週期。
func TestAutomaticSimulationIntegration(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	if target.relations != 0 {
		t.Fatal("expected empty disposable database")
	}
	state := openMeasurementTestState(t, true)
	startupSimulationFixture(t, target.conn, state)
	app := measurementTestApplication(target.conn)
	observed := make(chan observedSimulationSample, 30)
	// 所有觀察樣本先經正式採用入口，測試不能以直接更新 latest_measurements 代替回報。
	app.simulator = newSimulator(t.Context(), func(ctx context.Context, sample Measurement) (bool, error) {
		adopted, err := app.SubmitMeasurement(ctx, sample)
		select {
		case observed <- observedSimulationSample{sample, adopted, err}:
		case <-ctx.Done():
		}
		return adopted, err
	})
	t.Cleanup(app.simulator.close)
	// 故意拒絕最後一台的啟動交易，前五台也不能部分提交或開啟計時器。
	simulationSQL(t, target.conn, `ALTER TABLE device_simulations ADD CONSTRAINT test_auto_start_failure CHECK (device_id <> 'south-03')`)
	if err := app.startDemoSimulations(t.Context()); err == nil {
		t.Fatal("startup accepted a failing transaction")
	}
	var count int
	if err := target.conn.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM measurement_runs) + (SELECT count(*) FROM device_simulations) + (SELECT count(*) FROM latest_measurements)`).Scan(&count); err != nil || count != 0 || len(app.simulator.devices) != 0 {
		t.Fatalf("failed startup partially committed or started workers: count=%d err=%v", count, err)
	}
	simulationSQL(t, target.conn, `ALTER TABLE device_simulations DROP CONSTRAINT test_auto_start_failure`)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := app.startDemoSimulations(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup: %v", err)
	}
	if err := app.scanOffline(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := app.startDemoSimulations(t.Context()); err != nil {
		t.Fatal(err)
	}
	latest := make(map[string]Measurement)
	initialValues := make(map[float64]bool)
	for round := 0; round < 3; round++ {
		seen := make(map[string]bool)
		for range 6 {
			got := waitForSimulation(t, observed)
			sample := got.sample
			if got.err != nil || !got.adopted || !validNormalSimulationSample(sample.Temperature) || seen[sample.DeviceID] {
				t.Fatalf("invalid or duplicate automatic report in round %d: %+v", round, got)
			}
			seen[sample.DeviceID] = true
			if round == 0 {
				if sample.Sequence != 0 {
					t.Fatalf("initial sequence: %+v", sample)
				}
				initialValues[sample.Temperature] = true
			} else {
				previous, exists := latest[sample.DeviceID]
				elapsed := sample.GeneratedAt.Sub(previous.GeneratedAt)
				if !exists || sample.RunID != previous.RunID || sample.Sequence != previous.Sequence+1 ||
					elapsed < 4500*time.Millisecond || elapsed > 5500*time.Millisecond || math.Abs(math.Abs(sample.Temperature-previous.Temperature)-0.1) > 1e-9 {
					t.Fatalf("device cadence or variation changed: previous=%+v current=%+v", previous, sample)
				}
			}
			latest[sample.DeviceID] = sample
		}
	}
	app.simulator.close()
	if len(latest) != 6 || len(initialValues) != 6 {
		t.Fatalf("missing device diversity: devices=%d values=%d", len(latest), len(initialValues))
	}
	if err := target.conn.QueryRowContext(t.Context(), `SELECT count(*) FROM simulation_history`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("automatic startup invented a human control record: count=%d err=%v", count, err)
	}
	// 真實 HTTP 仍需總部權限；停機前留一台異常、一台停止及其原始回報。
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}
	f := serveMeasurementFixture(t, app)
	f.hq = f.login(t, "hq", passwords["hq"])
	// 逐台讀回兩輪後的最後採用值，確認並行設備沒有覆蓋彼此的資料。
	for _, sample := range latest {
		expectLatestSimulationSample(t, f, sample)
	}
	// 用正式採用入口多收四筆正常回報，讓持久順序在重啟後仍不同步。
	phase := latest["north-02"]
	for range 4 {
		phase.Sequence++
		phase.GeneratedAt = time.Now().UTC()
		phase.Temperature = 24.5
		f.submit(t, phase, true)
	}
	latest[phase.DeviceID] = phase
	expectLatestSimulationSample(t, f, phase)
	// 重建服務生命週期以便在已觀察完兩輪後執行控制，避免關閉的 context 影響交易。
	app.simulator = newSimulator(t.Context(), app.SubmitMeasurement)
	t.Cleanup(app.simulator.close)
	controlSimulation(t, f, "north-01", "abnormal", "1", simulationState{"abnormal", "2", true})
	controlSimulation(t, f, "north-02", "stop", "1", simulationState{"stopped", "2", false})
	app.simulator.close()
	app.simulator.mu.Lock()
	active := *app.simulator.devices["north-01"].latest
	app.simulator.mu.Unlock()
	evidence := automaticRestartEvidence{ClusterID: target.clusterID, StartedAt: target.startedAt, Generation: active.DataGeneration, FirstExpected: simulationFirstExpected(t, target.conn, "north-02"), Active: active, Stopped: latest["north-02"]}
	file, err := state.OpenFile(automaticBeforeFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(json.NewEncoder(file).Encode(evidence), file.Close()); err != nil {
		t.Fatal(err)
	}
}

// TestAutomaticSimulationRestart 以同叢集重啟及可控時間驗證先補判離線，再以新執行恢復而不自動結案。
func TestAutomaticSimulationRestart(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	state := openMeasurementTestState(t, false)
	data, err := readPrivateFile(state, automaticBeforeFile)
	if err != nil {
		t.Fatal(err)
	}
	var before automaticRestartEvidence
	if err := json.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	if target.clusterID != before.ClusterID || !target.startedAt.After(before.StartedAt) {
		t.Fatal("expected a real restart of the same PostgreSQL cluster")
	}
	startupSimulationFixture(t, target.conn, state)
	app := measurementTestApplication(target.conn)
	var otherSequence int64
	if err := target.conn.QueryRowContext(t.Context(), `SELECT sequence FROM latest_measurements WHERE device_id = 'north-03'`).Scan(&otherSequence); err != nil || otherSequence == before.Stopped.Sequence {
		t.Fatalf("restart fixture lacks different persisted device phases: north-02=%d north-03=%d err=%v", before.Stopped.Sequence, otherSequence, err)
	}
	t.Logf("persisted phases before restart: north-02=%d north-03=%d", before.Stopped.Sequence, otherSequence)
	// 將服務時鐘推至上次有效回報一分鐘後，精確建立超過 15 秒的停機補判條件。
	now := before.Active.GeneratedAt.Add(time.Minute)
	app.measurementNow = func() time.Time { return now }
	if err := app.scanOffline(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	var offline int
	if err := target.conn.QueryRowContext(t.Context(), `SELECT count(*) FROM alerts WHERE type = 'offline' AND detected_at_restart AND recovered_at_ns IS NULL`).Scan(&offline); err != nil || offline != 6 {
		t.Fatalf("restart missed offline evidence: count=%d err=%v", offline, err)
	}
	app.simulator = newSimulator(t.Context(), app.SubmitMeasurement)
	t.Cleanup(app.simulator.close)
	// 第二台首筆失敗時保留第一台已提交的採用，不可開啟工作或抹除真實恢復證據。
	failed := errors.New("test first-report failure")
	app.simulator.submit = func(ctx context.Context, sample Measurement) (bool, error) {
		if sample.DeviceID == "north-01" {
			return app.SubmitMeasurement(ctx, sample)
		}
		return false, failed
	}
	if err := app.startDemoSimulations(t.Context()); !errors.Is(err, failed) || len(app.simulator.devices) != 0 {
		t.Fatalf("first report failure left workers or lost error: %v", err)
	}
	var partialSequence int64
	if err := target.conn.QueryRowContext(t.Context(), `SELECT sequence FROM latest_measurements WHERE device_id = 'north-01'`).Scan(&partialSequence); err != nil || partialSequence <= before.Active.Sequence {
		t.Fatalf("startup failure erased a committed first report: sequence=%d err=%v", partialSequence, err)
	}
	app.simulator.close()
	if err := stopSimulationsOnStartup(t.Context(), target.conn); err != nil {
		t.Fatal(err)
	}
	app.simulator = newSimulator(t.Context(), app.SubmitMeasurement)
	t.Cleanup(app.simulator.close)
	if err := app.startDemoSimulations(t.Context()); err != nil {
		t.Fatal(err)
	}
	app.simulator.close()
	for _, old := range []Measurement{before.Active, before.Stopped} {
		old.GeneratedAt = now
		old.Sequence += 100
		if adopted, err := app.SubmitMeasurement(t.Context(), old); err != nil || adopted {
			t.Fatalf("old execution revived after startup: adopted=%v err=%v", adopted, err)
		}
	}
	var generation int64
	var recovered, closures, history int
	if err := target.conn.QueryRowContext(t.Context(), `SELECT generation FROM public.iot_data_generation WHERE singleton`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := target.conn.QueryRowContext(t.Context(), `SELECT count(*) FROM alerts WHERE recovered_at_ns IS NOT NULL`).Scan(&recovered); err != nil {
		t.Fatal(err)
	}
	if err := target.conn.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM alert_closures), (SELECT count(*) FROM simulation_history)`).Scan(&closures, &history); err != nil {
		t.Fatal(err)
	}
	if generation != before.Generation || simulationFirstExpected(t, target.conn, "north-02") != before.FirstExpected || recovered != 7 || closures != 0 || history != 2 {
		t.Fatalf("restart lost history/generation or closed alerts: generation=%d recovered=%d closures=%d history=%d", generation, recovered, closures, history)
	}
	for _, old := range []Measurement{before.Active, before.Stopped} {
		var sequence, run int64
		var temperature float64
		if err := target.conn.QueryRowContext(t.Context(), `SELECT m.sequence, m.temperature, r.run_id FROM latest_measurements m JOIN measurement_runs r USING (device_id) WHERE device_id = $1`, old.DeviceID).Scan(&sequence, &temperature, &run); err != nil || sequence <= old.Sequence || run == old.RunID || !validNormalSimulationSample(temperature) {
			t.Fatalf("new startup did not recover normally: sequence=%d run=%d temperature=%v err=%v", sequence, run, temperature, err)
		}
	}
	// 首筆必須從同一個資料卷的各自順序延續，再經 HTTP 證明六台正常值仍不同。
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}
	f := serveMeasurementFixture(t, app)
	f.hq = f.login(t, "hq", passwords["hq"])
	values := make(map[float64]string)
	for _, id := range []string{"north-01", "north-02", "north-03", "south-01", "south-02", "south-03"} {
		sample := latestSimulationSample(t, f, id)
		if sample == nil || !validNormalSimulationSample(sample.Temperature) {
			t.Fatalf("missing normal restart report for %s: %+v", id, sample)
		}
		if previous, exists := values[sample.Temperature]; exists {
			t.Fatalf("restart reports collided at %v after different persisted phases: %s and %s", sample.Temperature, previous, id)
		}
		values[sample.Temperature] = id
		t.Logf("restart normal report: %s sequence=%d temperature=%v", id, sample.Sequence, sample.Temperature)
	}
}
