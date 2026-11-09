package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	measurementTestOrigin = "http://iot.localhost:8090"
	measurementTestState  = "/tmp/iot-measurement-state"
	measurementBeforeFile = "before-restart.json"
)

type measurementTestDatabase struct {
	pool      *sql.DB
	conn      *sql.Conn
	pid       int
	relations int
	clusterID int64
	startedAt time.Time
}

func openMeasurementTestDatabase(t *testing.T) measurementTestDatabase {
	t.Helper()
	if os.Getenv("IOT_MEASUREMENT_TEST") != "1" {
		t.Skip("run bin/test-measurements for a disposable PostgreSQL instance")
	}
	ctx := t.Context()
	config, err := pgx.ParseConfig("host=/var/run/postgresql port=5432 user=iot dbname=iot_measurement_test sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var database, user string
	var local bool
	target := measurementTestDatabase{pool: db, conn: conn}
	err = conn.QueryRowContext(ctx, `
		SELECT current_database(), current_user, inet_server_addr() IS NULL, pg_backend_pid(),
			(SELECT count(*) FROM pg_class WHERE relnamespace = 'public'::regnamespace),
			(SELECT system_identifier FROM pg_control_system()), pg_postmaster_start_time()
	`).Scan(&database, &user, &local, &target.pid, &target.relations, &target.clusterID, &target.startedAt)
	if err != nil || database != "iot_measurement_test" || user != "iot" || !local {
		t.Fatalf("expected disposable socket database, got database=%q user=%q socket=%v: %v",
			database, user, local, err)
	}
	return target
}

func openMeasurementTestState(t *testing.T, create bool) *os.Root {
	t.Helper()
	if create {
		if err := os.Mkdir(measurementTestState, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Lstat(measurementTestState)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != os.ModeDir|0o700 {
		t.Fatal("measurement fixture state must be a mode-0700 directory")
	}
	state, err := os.OpenRoot(measurementTestState)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return state
}

func newMeasurementFixture(t *testing.T, conn *sql.Conn) *measurementFixture {
	t.Helper()
	f := &measurementFixture{
		app: &application{
			host: "iot.localhost:8090", origin: measurementTestOrigin,
			db: &lockedDB{conn: conn, gate: make(chan struct{}, 1)}, hashSlots: make(chan struct{}, 8),
		},
	}
	f.server = httptest.NewServer(f.app.router())
	f.server.Client().Timeout = 5 * time.Second
	t.Cleanup(f.server.Close)
	return f
}

type measurementRestartEvidence struct {
	ClusterID int64
	StartedAt time.Time
	API       string
}

func TestMeasurementIntegration(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	if target.relations != 0 {
		t.Fatalf("expected empty disposable database, found %d public relations", target.relations)
	}
	ctx, conn := t.Context(), target.conn
	state := openMeasurementTestState(t, true)

	// 模擬從第 01 票升級，先建立舊版種子與有效 session，再確認新增資料表保留既有登入。
	passwords := seedLegacyMeasurementFixture(t, conn, state)
	f := newMeasurementFixture(t, conn)
	f.hq = f.login(t, "hq", passwords["hq"])
	if err := bootstrap(ctx, conn, state); err != nil {
		t.Fatal(err)
	}
	f.request(t, http.MethodGet, "/api/session", "", f.hq, http.StatusOK)
	empty := f.get(t, "/api/alerts")
	if string(empty) != `{"items":[],"total":0,"limit":50,"offset":0}` {
		t.Fatalf("unexpected empty alerts page: %s", empty)
	}
	beforeMeasurement := f.get(t, "/api/devices/north-01")
	if string(beforeMeasurement) != `{"id":"north-01","name":"北區設備 01","siteId":"north","siteName":"北區場域"}` {
		t.Fatalf("unexpected untouched device payload: %s", beforeMeasurement)
	}

	now := time.Date(2026, 11, 9, 10, 0, 0, 123456789, time.UTC)
	f.clock.Store(now.UnixNano())
	f.app.measurementNow = f.now
	sample := Measurement{
		Source: SimulatorMeasurementSource, DeviceID: "north-01", DataGeneration: 4, RunID: 12,
		Sequence: 0, GeneratedAt: now.Add(-5 * time.Second), Temperature: 30,
	}
	f.submit(t, sample, false)
	if !slices.Equal(beforeMeasurement, f.get(t, "/api/devices/north-01")) || !slices.Equal(empty, f.get(t, "/api/alerts")) {
		t.Fatal("unprovisioned run changed state")
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO measurement_runs (device_id, data_generation, run_id)
		VALUES ('north-01', 4, 12), ('south-01', 4, 13)
	`); err != nil {
		t.Fatal(err)
	}
	f.submit(t, sample, true)
	if got := f.get(t, "/api/devices/north-01"); !strings.Contains(string(got),
		`"latestMeasurement":{"temperature":30,"sequence":0,"generatedAt":"2026-11-09T09:59:55.123456789Z","receivedAt":"2026-11-09T10:00:00.123456789Z"}`) {
		t.Fatalf("exact boundary lost UTC/nanosecond evidence: %s", got)
	}
	firstPage := f.page(t, "")
	if firstPage.Total != 1 || len(firstPage.Items) != 1 {
		t.Fatalf("initial high measurement did not open one alert: %+v", firstPage)
	}
	first := firstPage.Items[0]
	if first.ID == "" || first.DeviceID != "north-01" || first.Type != "temperature" || first.Status != "active" ||
		first.Temperature != 30 || first.DetectedAt != "2026-11-09T10:00:00.123456789Z" || first.RecoveredAt != nil {
		t.Fatalf("unexpected initial high alert: %+v", first)
	}

	sample.Sequence++
	sample.GeneratedAt = now.Add(-time.Second)
	f.submit(t, sample, true)
	baseline := f.snapshot(t)
	if !t.Run("invalid samples preserve measurements and alerts", func(t *testing.T) {
		for _, invalid := range []struct {
			name string
			edit func(*Measurement)
		}{
			{"source", func(m *Measurement) { m.Source = "external" }},
			{"unknown device", func(m *Measurement) { m.DeviceID = "unknown" }},
			{"NUL device", func(m *Measurement) { m.DeviceID = "north-01\x00" }},
			{"non-UTF-8 device", func(m *Measurement) { m.DeviceID = "north-01\xff" }},
			{"stopped device", func(m *Measurement) { m.DeviceID = "north-02" }},
			{"run from another device", func(m *Measurement) { m.DeviceID = "south-01" }},
			{"stale run", func(m *Measurement) { m.RunID = 11 }},
			{"stale data generation", func(m *Measurement) { m.DataGeneration = 3 }},
			{"NaN", func(m *Measurement) { m.Temperature = math.NaN() }},
			{"positive infinity", func(m *Measurement) { m.Temperature = math.Inf(1) }},
			{"negative infinity", func(m *Measurement) { m.Temperature = math.Inf(-1) }},
			{"same sequence", func(m *Measurement) { m.Sequence = 1 }},
			{"older sequence", func(m *Measurement) { m.Sequence = 0 }},
			{"new sequence and generated a nanosecond earlier", func(m *Measurement) {
				m.GeneratedAt = m.GeneratedAt.Add(-time.Nanosecond)
			}},
			{"future at arrival", func(m *Measurement) { m.GeneratedAt = now.Add(time.Nanosecond) }},
			{"expired at arrival", func(m *Measurement) { m.GeneratedAt = now.Add(-5*time.Second - time.Nanosecond) }},
		} {
			t.Run(invalid.name, func(t *testing.T) {
				input := sample
				input.Sequence, input.Temperature = 2, 23
				invalid.edit(&input)
				f.submit(t, input, false)
				if got := f.snapshot(t); got != baseline {
					t.Fatalf("ignored sample changed device or alert state: %s", got)
				}
			})
		}
	}) {
		return
	}

	var recoveredFirst measurementAlertWire
	if !t.Run("inclusive thresholds and independent episodes", func(t *testing.T) {
		// 較新順序可使用相同生成時間，兩端正常值都不能建立告警。
		sample.Sequence, sample.Temperature = 2, 18
		f.clock.Add(int64(time.Nanosecond))
		f.submit(t, sample, true)
		recoveredFirst = f.alert(t, first.ID)
		if recoveredFirst.Status != "recovered" || recoveredFirst.Temperature != 30 ||
			recoveredFirst.RecoveredAt == nil || *recoveredFirst.RecoveredAt != "2026-11-09T10:00:00.12345679Z" {
			t.Fatalf("18°C did not recover first episode: %+v", recoveredFirst)
		}
		sample.Sequence, sample.Temperature = 3, 28
		f.submit(t, sample, true)
		if page := f.page(t, ""); page.Total != 1 || !slices.EqualFunc(page.Items, []measurementAlertWire{recoveredFirst}, sameMeasurementAlert) {
			t.Fatalf("28°C changed alert history: %+v", page)
		}
		sample.Sequence, sample.Temperature = 4, 17.99
		f.submit(t, sample, true)
		low := f.page(t, "").Items[0]
		if low.ID == first.ID || low.Status != "active" || low.Temperature != 17.99 || low.RecoveredAt != nil {
			t.Fatalf("low temperature did not open a new episode: %+v", low)
		}
		sample.Sequence, sample.Temperature = 5, 31
		f.submit(t, sample, true)
		if page := f.page(t, ""); page.Total != 2 || !sameMeasurementAlert(page.Items[0], low) {
			t.Fatalf("switching abnormal sides changed the ongoing episode: %+v", page)
		}
		sample.Sequence, sample.Temperature = 6, 20
		f.clock.Add(int64(time.Nanosecond))
		f.submit(t, sample, true)
		recoveredLow := f.alert(t, low.ID)
		if recoveredLow.Status != "recovered" || recoveredLow.RecoveredAt == nil {
			t.Fatalf("normal measurement did not recover the low episode: %+v", recoveredLow)
		}
		sample.Sequence, sample.Temperature = 7, 30
		f.submit(t, sample, true)
		page := f.page(t, "")
		if page.Total != 3 || page.Items[0].ID == low.ID || page.Items[0].ID == first.ID ||
			page.Items[0].Status != "active" || !sameMeasurementAlert(page.Items[1], recoveredLow) ||
			!sameMeasurementAlert(page.Items[2], recoveredFirst) {
			t.Fatalf("recurrence modified previous recovery or reused its ID: %+v", page)
		}
	}) {
		return
	}

	if !t.Run("receipt and adoption clocks", func(t *testing.T) {
		existing := f.snapshot(t)
		sample.Sequence, sample.Temperature = 8, 23

		// 即使後來時鐘使資料變得有效，抵達時的未來或過期資料仍不能採用。
		received := now.Add(20 * time.Second)
		future := sample
		future.GeneratedAt = received.Add(time.Nanosecond)
		f.delayed(t, future, received, received.Add(time.Second), false)
		expired := sample
		expired.GeneratedAt = received.Add(-5*time.Second - time.Nanosecond)
		f.delayed(t, expired, received, received.Add(-time.Nanosecond), false)
		sample.GeneratedAt = received.Add(-4 * time.Second)
		f.delayed(t, sample, received, received.Add(time.Second+time.Nanosecond), false)
		if got := f.snapshot(t); got != existing {
			t.Fatalf("ignored delayed samples changed state: %s", got)
		}

		// 先鎖住告警資料列，使到期發生於拿到應用層鎖之後。
		f.clock.Store(received.UnixNano())
		rowLock, err := target.pool.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer rowLock.Rollback()
		var locked int64
		if err := rowLock.QueryRowContext(t.Context(), `
			SELECT id FROM alerts WHERE device_id = 'north-01' AND recovered_at_ns IS NULL FOR UPDATE
		`).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		pending := f.submitAsync(t.Context(), sample)
		waitForMeasurementRowLock(t, target.pool, target.pid)
		f.clock.Store(received.Add(time.Second + time.Nanosecond).UnixNano())
		if err := rowLock.Rollback(); err != nil {
			t.Fatal(err)
		}
		assertMeasurementResult(t, <-pending, false)
		if got := f.snapshot(t); got != existing {
			t.Fatalf("sample expiring during a row lock changed state: %s", got)
		}

		f.delayed(t, sample, received, received.Add(time.Second), true)
		if got := f.get(t, "/api/devices/north-01"); !strings.Contains(string(got),
			`"latestMeasurement":{"temperature":23,"sequence":8,"generatedAt":"2026-11-09T10:00:16.123456789Z","receivedAt":"2026-11-09T10:00:20.123456789Z"}`) {
			t.Fatalf("adoption at the exact boundary lost the arrival timestamp: %s", got)
		}
		if latest := f.page(t, "").Items[0]; latest.Status != "recovered" ||
			latest.RecoveredAt == nil || *latest.RecoveredAt != "2026-11-09T10:00:21.123456789Z" {
			t.Fatalf("alert did not recover at adoption: %+v", latest)
		}
	}) {
		return
	}

	if !t.Run("alert write failure rolls back latest measurement", func(t *testing.T) {
		for _, fault := range []struct {
			check, expectedStatus string
			temperature           float64
		}{
			{"temperature <> 34", "active", 34},
			{"temperature <> 34 OR recovered_at_ns IS NULL", "recovered", 23},
		} {
			existing := f.snapshot(t)
			if _, err := conn.ExecContext(ctx, "ALTER TABLE alerts ADD CONSTRAINT measurement_test_reject CHECK ("+fault.check+")"); err != nil {
				t.Fatal(err)
			}
			sample.Sequence++
			sample.GeneratedAt, sample.Temperature = f.now(), fault.temperature
			adopted, err := f.app.SubmitMeasurement(t.Context(), sample)
			var pgErr *pgconn.PgError
			if adopted || !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "measurement_test_reject" {
				t.Fatalf("expected real alert %s write error with no adoption: adopted=%v error=%v", fault.expectedStatus, adopted, err)
			}
			if got := f.snapshot(t); got != existing {
				t.Fatalf("failed alert %s write left partial changes: %s", fault.expectedStatus, got)
			}
			if _, err := conn.ExecContext(ctx, "ALTER TABLE alerts DROP CONSTRAINT measurement_test_reject"); err != nil {
				t.Fatal(err)
			}
			f.submit(t, sample, true)
			if latest := f.page(t, "").Items[0]; latest.Status != fault.expectedStatus || latest.Temperature != 34 {
				t.Fatalf("retry did not commit alert state %s: %+v", fault.expectedStatus, latest)
			}
		}
	}) {
		return
	}

	if !t.Run("concurrent duplicate has one adoption", func(t *testing.T) {
		south := sample
		south.DeviceID, south.RunID, south.Sequence = "south-01", 13, 0
		south.Temperature = 30
		pending := make([]<-chan measurementResult, 4)
		for i := range pending {
			pending[i] = f.submitAsync(t.Context(), south)
		}
		adoptions := 0
		for _, result := range pending {
			got := <-result
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.adopted {
				adoptions++
			}
		}
		page := f.page(t, "")
		if adoptions != 1 || page.Total != 5 || page.Items[0].DeviceID != "south-01" || page.Items[0].Status != "active" {
			t.Fatalf("concurrent retries did not produce one adoption and one new alert: adoptions=%d page=%+v", adoptions, page)
		}
	}) {
		return
	}

	t.Run("authorized readback, pagination and bootstrap replay", func(t *testing.T) {
		verifyMeasurementReadback(t, f, passwords)
		persisted := f.snapshot(t)
		if err := bootstrap(ctx, conn, state); err != nil {
			t.Fatal(err)
		}
		if got := f.snapshot(t); got != persisted {
			t.Fatalf("bootstrap replay changed latest measurements, sessions or alerts: %s", got)
		}
		if !sameMeasurementAlert(f.alert(t, first.ID), recoveredFirst) {
			t.Fatal("older recovered alert changed after new episodes and bootstrap replay")
		}
		f.submit(t, sample, false)
		if got := f.snapshot(t); got != persisted {
			t.Fatal("duplicate of last committed measurement changed replayed state")
		}
		// 將已驗證的 API 結果留給重啟後的新程序，兩個程序只共用容器內的受保護檔案。
		file, err := state.OpenFile(measurementBeforeFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		before := measurementRestartEvidence{ClusterID: target.clusterID, StartedAt: target.startedAt, API: persisted}
		writeErr := json.NewEncoder(file).Encode(before)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMeasurementRestart(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	state := openMeasurementTestState(t, false)
	data, err := readPrivateFile(state, measurementBeforeFile)
	if err != nil {
		t.Fatal(err)
	}
	var before measurementRestartEvidence
	if err := json.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	if before.StartedAt.IsZero() || before.API == "" || target.clusterID != before.ClusterID ||
		target.relations == 0 || !target.startedAt.After(before.StartedAt) {
		t.Fatalf("expected restart of the seeded PostgreSQL cluster, previous start %s, current %s",
			before.StartedAt, target.startedAt)
	}
	t.Logf("PostgreSQL postmaster restarted: %s -> %s", before.StartedAt, target.startedAt)
	if err := bootstrap(t.Context(), target.conn, state); err != nil {
		t.Fatal(err)
	}
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}
	f := newMeasurementFixture(t, target.conn)
	f.hq = f.login(t, "hq", passwords["hq"])
	if got := f.snapshot(t); got != before.API {
		t.Fatalf("restart changed latest measurements or alert history: got %s, want %s", got, before.API)
	}
	verifyMeasurementReadback(t, f, passwords)
}

func seedLegacyMeasurementFixture(t *testing.T, conn *sql.Conn, state *os.Root) map[string]string {
	t.Helper()
	ctx := t.Context()
	if _, err := conn.ExecContext(ctx, createSchema+createSeedDevices); err != nil {
		t.Fatal(err)
	}
	passwords, err := demoPasswords(state, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range seedAccounts {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO accounts (username, role, site_id, password_hash) VALUES ($1, $2, $3, $4)
		`, account.username, account.role, account.siteID, hashPassword(passwords[account.username])); err != nil {
			t.Fatal(err)
		}
	}
	return passwords
}

type measurementFixture struct {
	app    *application
	server *httptest.Server
	hq     *http.Cookie
	clock  atomic.Int64
}

func (f *measurementFixture) now() time.Time {
	return time.Unix(0, f.clock.Load()).UTC()
}

type measurementResult struct {
	adopted bool
	err     error
}

func assertMeasurementResult(t *testing.T, got measurementResult, want bool) {
	t.Helper()
	if got.adopted != want || got.err != nil {
		t.Fatalf("measurement returned adopted=%v error=%v, want adopted=%v and no error", got.adopted, got.err, want)
	}
}

func (f *measurementFixture) submit(t *testing.T, sample Measurement, want bool) {
	t.Helper()
	adopted, err := f.app.SubmitMeasurement(t.Context(), sample)
	assertMeasurementResult(t, measurementResult{adopted, err}, want)
}

func (f *measurementFixture) submitAsync(ctx context.Context, sample Measurement) <-chan measurementResult {
	done := make(chan measurementResult, 1)
	go func() {
		adopted, err := f.app.SubmitMeasurement(ctx, sample)
		done <- measurementResult{adopted, err}
	}()
	return done
}

func (f *measurementFixture) delayed(t *testing.T, sample Measurement, received, released time.Time, want bool) {
	t.Helper()
	f.clock.Store(received.UnixNano())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{}, 1)
	f.app.measurementNow = func() time.Time {
		now := f.now()
		select {
		case entered <- struct{}{}:
		default:
		}
		return now
	}
	defer func() { f.app.measurementNow = f.now }()

	// 先占住應用層佇列，等入口取完抵達時間才推進時鐘。
	f.app.db.gate <- struct{}{}
	result := f.submitAsync(ctx, sample)
	select {
	case <-entered:
		f.clock.Store(released.UnixNano())
	case <-time.After(2 * time.Second):
		cancel()
		<-f.app.db.gate
		<-result
		t.Fatal("measurement did not capture receipt while waiting for the database gate")
	}
	<-f.app.db.gate
	assertMeasurementResult(t, <-result, want)
}

func waitForMeasurementRowLock(t *testing.T, db *sql.DB, pid int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')
		`, pid).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("measurement did not wait for the held PostgreSQL row lock")
		}
	}
}

type measurementAlertWire struct {
	ID          string
	DeviceID    string
	Type        string
	Status      string
	Temperature float64
	DetectedAt  string
	RecoveredAt *string
}

type measurementAlertPageWire struct {
	Items                []measurementAlertWire
	Total, Limit, Offset int
}

func sameMeasurementAlert(a, b measurementAlertWire) bool {
	if (a.RecoveredAt == nil) != (b.RecoveredAt == nil) ||
		(a.RecoveredAt != nil && *a.RecoveredAt != *b.RecoveredAt) {
		return false
	}
	a.RecoveredAt, b.RecoveredAt = nil, nil
	return a == b
}

func (f *measurementFixture) alert(t *testing.T, id string) measurementAlertWire {
	t.Helper()
	var alert measurementAlertWire
	if err := json.Unmarshal(f.get(t, "/api/alerts/"+id), &alert); err != nil {
		t.Fatal(err)
	}
	return alert
}

func (f *measurementFixture) page(t *testing.T, query string) measurementAlertPageWire {
	t.Helper()
	var page measurementAlertPageWire
	if err := json.Unmarshal(f.get(t, "/api/alerts"+query), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func (f *measurementFixture) snapshot(t *testing.T) string {
	t.Helper()
	return string(f.get(t, "/api/devices")) + "\n" + string(f.get(t, "/api/alerts"))
}

func (f *measurementFixture) get(t *testing.T, path string) []byte {
	t.Helper()
	body, _ := f.request(t, http.MethodGet, path, "", f.hq, http.StatusOK)
	return body
}

func (f *measurementFixture) login(t *testing.T, username, password string) *http.Cookie {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	_, cookies := f.request(t, http.MethodPost, "/api/login", string(body), nil, http.StatusOK)
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatal("login did not return the session cookie")
	}
	return cookies[0]
}

func (f *measurementFixture) request(t *testing.T, method, path, body string, cookie *http.Cookie, want int) ([]byte, []*http.Cookie) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = f.app.host
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if method == http.MethodPost {
		req.Header.Set("Origin", measurementTestOrigin)
		req.Header.Set("X-CSRF-Protection", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		if path == "/api/login" {
			t.Fatalf("login returned %d, want %d", response.StatusCode, want)
		}
		t.Fatalf("%s %s returned %d, want %d: %s", method, path, response.StatusCode, want, data)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("API response can be cached")
	}
	return data, response.Cookies()
}

func verifyMeasurementReadback(t *testing.T, f *measurementFixture, passwords map[string]string) {
	t.Helper()
	all := f.page(t, "")
	if all.Total != 5 || len(all.Items) != 5 || all.Limit != 50 || all.Offset != 0 {
		t.Fatalf("unexpected headquarters page: %+v", all)
	}
	if page := f.page(t, "?limit=2&offset=1"); page.Total != 5 || page.Limit != 2 || page.Offset != 1 ||
		!slices.EqualFunc(page.Items, all.Items[1:3], sameMeasurementAlert) {
		t.Fatalf("pagination did not preserve stable ordering and scoped total: %+v", page)
	}
	if page := f.page(t, "?offset=99"); page.Total != 5 || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("unexpected empty page after offset: %+v", page)
	}
	for _, alert := range all.Items {
		if !sameMeasurementAlert(f.alert(t, alert.ID), alert) {
			t.Fatalf("alert list and detail differ for id %s", alert.ID)
		}
	}

	for _, site := range []struct {
		username string
		visible  []measurementAlertWire
		hidden   string
	}{
		{"north", all.Items[1:], all.Items[0].ID},
		{"south", all.Items[:1], all.Items[1].ID},
	} {
		cookie := f.login(t, site.username, passwords[site.username])
		body, _ := f.request(t, http.MethodGet, "/api/alerts", "", cookie, http.StatusOK)
		var page measurementAlertPageWire
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != len(site.visible) || !slices.EqualFunc(page.Items, site.visible, sameMeasurementAlert) {
			t.Fatalf("%s got wrong site-scoped alert list: %s", site.username, body)
		}
		for _, alert := range site.visible {
			body, _ := f.request(t, http.MethodGet, "/api/alerts/"+alert.ID, "", cookie, http.StatusOK)
			var item measurementAlertWire
			if err := json.Unmarshal(body, &item); err != nil || !sameMeasurementAlert(item, alert) {
				t.Fatalf("%s got wrong site-scoped alert detail: %s", site.username, body)
			}
		}
		blocked, _ := f.request(t, http.MethodGet, "/api/alerts/"+site.hidden, "", cookie, http.StatusNotFound)
		missing, _ := f.request(t, http.MethodGet, "/api/alerts/9999999", "", cookie, http.StatusNotFound)
		if string(blocked) != `{"error":"not_found"}` || !slices.Equal(blocked, missing) {
			t.Fatalf("site scope revealed cross-site existence: %s versus %s", blocked, missing)
		}
		device := site.username + "-01"
		list, _ := f.request(t, http.MethodGet, "/api/devices", "", cookie, http.StatusOK)
		detail, _ := f.request(t, http.MethodGet, "/api/devices/"+device, "", cookie, http.StatusOK)
		if !strings.Contains(string(list), string(detail)) || !strings.Contains(string(detail), `"latestMeasurement"`) {
			t.Fatalf("device detail/list do not agree about latest measurement: %s / %s", detail, list)
		}
	}
	for _, request := range []struct {
		method, path, body string
		auth               bool
		status             int
		code               string
	}{
		{http.MethodGet, "/api/alerts", "", false, 401, "unauthenticated"},
		{http.MethodGet, "/api/alerts/" + all.Items[0].ID, "", false, 401, "unauthenticated"},
		{http.MethodGet, "/api/alerts?siteId=south", "", true, 400, "invalid_input"},
		{http.MethodGet, "/api/alerts?role=headquarters", "", true, 400, "invalid_input"},
		{http.MethodGet, "/api/alerts?limit=101", "", true, 400, "invalid_input"},
		{http.MethodGet, "/api/alerts?limit=1&limit=2", "", true, 400, "invalid_input"},
		{http.MethodGet, "/api/alerts/" + all.Items[0].ID + "?siteId=north", "", true, 400, "invalid_input"},
		{http.MethodGet, "/api/alerts", "{}", true, 400, "invalid_input"},
		{http.MethodGet, "/api/alerts/" + all.Items[0].ID, "{}", true, 400, "invalid_input"},
		{http.MethodPost, "/api/measurements", `{"deviceId":"north-01","temperature":90}`, true, 404, "not_found"},
	} {
		var cookie *http.Cookie
		if request.auth {
			cookie = f.hq
		}
		body, _ := f.request(t, request.method, request.path, request.body, cookie, request.status)
		if string(body) != `{"error":"`+request.code+`"}` {
			t.Fatalf("%s %s returned wrong error body: %s", request.method, request.path, body)
		}
	}
}
