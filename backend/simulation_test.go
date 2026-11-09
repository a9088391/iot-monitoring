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
	app.simulator = newSimulator(ctx, func(ctx context.Context, sample Measurement) (bool, error) {
		if sample.Sequence == 0 && (sample.DeviceID == "north-02" || sample.DeviceID == "north-03") {
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

	if !t.Run("authorization and strict HTTP inputs", func(t *testing.T) {
		testSimulationHTTP(t, f, passwords)
		expectSimulation(t, f, "north-01", simulationState{"stopped", "0", false})
		expectSimulationHistory(t, conn, "north-01", "")
	}) {
		return
	}

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
		for _, original := range []Measurement{first, south} {
			next := expectSimulationSample(t, observations, original.DeviceID, original.Temperature, true)
			elapsed := next.GeneratedAt.Sub(original.GeneratedAt)
			if next.Sequence != original.Sequence+1 || next.RunID != original.RunID ||
				elapsed < 5*time.Second || elapsed > 5500*time.Millisecond {
				t.Fatalf("report did not follow its own five-second cadence: elapsed=%s first=%+v next=%+v", elapsed, original, next)
			}
			expectLatestSimulationSample(t, f, next)
		}
		controlSimulation(t, f, "south-02", "stop", "1", simulationState{"stopped", "2", true})
		controlSimulation(t, f, "north-01", "stop", "2", simulationState{"stopped", "3", true})
	}) {
		return
	}

	if !t.Run("CAS concurrency and failed control rollback", func(t *testing.T) {
		secondSession := f.login(t, "hq", passwords["hq"])
		results := make(chan simulationHTTPResult, 2)
		start := make(chan struct{})
		for _, attempt := range []struct {
			action string
			cookie *http.Cookie
		}{{"normal", f.hq}, {"abnormal", secondSession}} {
			go func() {
				<-start
				results <- sendSimulation(t.Context(), f, "south-01", simulationBody(attempt.action, "0"), attempt.cookie, true)
			}()
		}
		close(start)
		outcomes := []simulationHTTPResult{waitForSimulation(t, results), waitForSimulation(t, results)}
		slices.SortFunc(outcomes, func(a, b simulationHTTPResult) int { return a.status - b.status })
		if outcomes[0].err != nil || outcomes[1].err != nil || outcomes[0].status != http.StatusOK ||
			outcomes[1].status != http.StatusConflict || outcomes[1].body != `{"error":"conflict"}` {
			t.Fatalf("expected one accepted control and one CAS conflict: %+v", outcomes)
		}
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

	if !t.Run("old pending first report and durable first expectation", func(t *testing.T) {
		pending := make(chan simulationHTTPResult, 1)
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

	if !t.Run("committed delivery survives request cancellation", func(t *testing.T) {
		request, cancel := context.WithCancel(t.Context())
		defer cancel()
		pending := make(chan simulationHTTPResult, 1)
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
	if before.API == "" || before.StartedAt.IsZero() || target.relations == 0 ||
		target.clusterID != before.ClusterID || !target.startedAt.After(before.StartedAt) {
		t.Fatalf("expected restart of seeded PostgreSQL cluster, previous start %s, current %s", before.StartedAt, target.startedAt)
	}
	t.Logf("PostgreSQL postmaster restarted: %s -> %s", before.StartedAt, target.startedAt)
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
	for _, prior := range []Measurement{before.Active, before.Unanswered} {
		prior.GeneratedAt = time.Now()
		prior.Sequence++
		f.submit(t, prior, false)
	}
	if got := f.snapshot(t); got != before.API {
		t.Fatal("restart or prior execution changed latest measurements or alerts")
	}
	controlSimulation(t, f, "north-01", "normal", "6", simulationState{"normal", "7", true})
	if newest := latestSimulationSample(t, f, "north-01"); newest == nil ||
		newest.Temperature != 23 || newest.Sequence <= before.Active.Sequence {
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

func testSimulationHTTP(t *testing.T, f *measurementFixture, passwords map[string]string) {
	t.Helper()
	path := simulationPath("north-01")
	normal := simulationBody("normal", "0")
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
		result.status != http.StatusForbidden || result.body != `{"error":"forbidden"}` {
		t.Fatalf("missing CSRF protection accepted: %+v", result)
	}
	f.request(t, http.MethodGet, path+"?role=headquarters", "", f.hq, http.StatusBadRequest)
	f.request(t, http.MethodGet, path, "{}", f.hq, http.StatusBadRequest)
	f.request(t, http.MethodPost, path+"?siteId=north", normal, f.hq, http.StatusBadRequest)
	f.request(t, http.MethodPost, path, strings.Repeat(" ", 16385), f.hq, http.StatusRequestEntityTooLarge)
	f.request(t, http.MethodPost, "/api/measurements", `{"deviceId":"north-01","temperature":99}`, f.hq, http.StatusNotFound)
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
	revoked := f.login(t, "hq", passwords["hq"])
	f.request(t, http.MethodPost, "/api/logout", "", revoked, http.StatusNoContent)
	expectSimulationError(t, f, "north-01", normal, revoked, 401, "unauthenticated")
}

func startupSimulationFixture(t *testing.T, conn *sql.Conn, state *os.Root) {
	t.Helper()
	if err := bootstrap(t.Context(), conn, state); err != nil {
		t.Fatal(err)
	}
	if err := stopSimulationsOnStartup(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
}

func simulationPath(id string) string {
	return "/api/devices/" + id + "/simulation"
}

func simulationBody(action, version string) string {
	return fmt.Sprintf(`{"action":%q,"version":%q}`, action, version)
}

func expectSimulation(t *testing.T, f *measurementFixture, id string, want simulationState) {
	t.Helper()
	body := f.get(t, simulationPath(id))
	expectSimulationHTTPState(t, simulationHTTPResult{status: http.StatusOK, body: string(body)}, want)
}

func controlSimulation(t *testing.T, f *measurementFixture, id, action, version string, want simulationState) {
	t.Helper()
	body, _ := f.request(t, http.MethodPost, simulationPath(id), simulationBody(action, version), f.hq, http.StatusOK)
	expectSimulationHTTPState(t, simulationHTTPResult{status: http.StatusOK, body: string(body)}, want)
}

func expectSimulationHTTPState(t *testing.T, result simulationHTTPResult, want simulationState) {
	t.Helper()
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if result.err != nil || result.status != http.StatusOK || result.body != string(expected) {
		t.Fatalf("unexpected simulation result: %+v, want %s", result, expected)
	}
}

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

func sendSimulation(ctx context.Context, f *measurementFixture, id, body string, cookie *http.Cookie, csrf bool) simulationHTTPResult {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+simulationPath(id), strings.NewReader(body))
	if err != nil {
		return simulationHTTPResult{err: err}
	}
	request.Host = f.app.host
	request.AddCookie(cookie)
	request.Header.Set("Content-Type", "application/json")
	if csrf {
		request.Header.Set("Origin", measurementTestOrigin)
		request.Header.Set("X-CSRF-Protection", "1")
	}
	response, err := f.server.Client().Do(request)
	if err != nil {
		return simulationHTTPResult{err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return simulationHTTPResult{status: response.StatusCode, body: string(data), err: err}
}

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

func expectSimulationSample(t *testing.T, input <-chan observedSimulationSample, id string, temperature float64, adopted bool) Measurement {
	t.Helper()
	got := waitForSimulation(t, input)
	if got.err != nil || got.adopted != adopted || got.sample.DeviceID != id ||
		got.sample.Source != SimulatorMeasurementSource || got.sample.Temperature != temperature {
		t.Fatalf("unexpected simulated report for %s: %+v", id, got)
	}
	return got.sample
}

func startFailingFirstReport(t *testing.T, f *measurementFixture, conn *sql.Conn, observed <-chan observedSimulationSample, id, action, version, next string) Measurement {
	t.Helper()
	simulationSQL(t, conn, "ALTER TABLE latest_measurements ADD CONSTRAINT test_first_report_failure CHECK (device_id <> '"+id+"')")
	controlSimulation(t, f, id, action, version, simulationState{action, next, true})
	got := waitForSimulation(t, observed)
	var pgErr *pgconn.PgError
	if got.sample.DeviceID != id || got.adopted || !errors.As(got.err, &pgErr) || pgErr.ConstraintName != "test_first_report_failure" {
		t.Fatalf("expected first measurement write failure for %s: %+v", id, got)
	}
	simulationSQL(t, conn, "ALTER TABLE latest_measurements DROP CONSTRAINT test_first_report_failure")
	return got.sample
}

func latestSimulationSample(t *testing.T, f *measurementFixture, id string) *measurementItem {
	t.Helper()
	var device deviceItem
	if err := json.Unmarshal(f.get(t, "/api/devices/"+id), &device); err != nil {
		t.Fatal(err)
	}
	return device.LatestMeasurement
}

func expectLatestSimulationSample(t *testing.T, f *measurementFixture, want Measurement) {
	t.Helper()
	got := latestSimulationSample(t, f, want.DeviceID)
	if got == nil || got.Temperature != want.Temperature || got.Sequence != want.Sequence || !got.GeneratedAt.Equal(want.GeneratedAt) {
		t.Fatalf("unexpected latest simulated sample: %+v, want %+v", got, want)
	}
}

func simulationSQL(t *testing.T, conn *sql.Conn, statement string) {
	t.Helper()
	if _, err := conn.ExecContext(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
}

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

func simulationRunID(t *testing.T, conn *sql.Conn, id string) int64 {
	t.Helper()
	var run int64
	if err := conn.QueryRowContext(t.Context(), "SELECT run_id FROM measurement_runs WHERE device_id = $1", id).Scan(&run); err != nil {
		t.Fatal(err)
	}
	return run
}

func expectSimulationHistory(t *testing.T, conn *sql.Conn, id, want string) {
	t.Helper()
	var actions string
	var actorsAndTimes bool
	if err := conn.QueryRowContext(t.Context(), `
		SELECT COALESCE(string_agg(action, ',' ORDER BY version), ''),
			COALESCE(bool_and(actor = 'hq' AND occurred_at_ns > 0), true)
		FROM simulation_history WHERE device_id = $1
	`, id).Scan(&actions, &actorsAndTimes); err != nil || actions != want || !actorsAndTimes {
		t.Fatalf("unexpected control history for %s: got %q, actors/times valid=%v, error=%v; want %q", id, actions, actorsAndTimes, err, want)
	}
}
