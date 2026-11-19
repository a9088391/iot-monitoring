package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type resetTestDatabase struct {
	conn      *sql.Conn
	clusterID string
	startedAt time.Time
}

const resetTestPassword = "iot-reset-fixture-password"

// openResetTestDatabase 只允許明確標記、用 TCP 連線且另有專用測試密碼的隔離資料庫。
func openResetTestDatabase(t *testing.T) resetTestDatabase {
	t.Helper()
	if os.Getenv("IOT_MEASUREMENT_TEST") != "1" {
		t.Skip("run bin/test-measurements reset for an isolated PostgreSQL instance")
	}
	config, err := pgx.ParseConfig("host=127.0.0.1 port=5432 user=iot dbname=iot sslmode=disable password=" + resetTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*config)
	// 只在測試目標確認後才由呼叫端建立夾具。
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 測試結束後歸還連線，避免延後同容器的資料庫重啟。
	t.Cleanup(func() { conn.Close() })
	target := resetTestDatabase{conn: conn}
	var database, user, marker string
	var local bool
	err = conn.QueryRowContext(t.Context(), `
		SELECT current_database(), current_user, current_setting('iot.reset_fixture', true),
			inet_server_addr() = '127.0.0.1'::inet, system_identifier::text, pg_postmaster_start_time()
		FROM pg_catalog.pg_control_system()
	`).Scan(&database, &user, &marker, &local, &target.clusterID, &target.startedAt)
	if err != nil ||
		database != "iot" ||
		user != "iot" ||
		marker != "ticket07" ||
		!local {
		t.Fatalf("not the isolated reset fixture: %v", err)
	}
	return target
}

// runResetCLI 呼叫容器內正式執行檔，刻意設定不適用於維護的服務環境。
func runResetCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/tmp/iot-reset-test-app", args...)
	cmd.Env = append(os.Environ(), "IOT_DB_HOST=not-the-reset-target.invalid", "IOT_ORIGIN=not-a-web-origin")
	out, err := cmd.Output()
	if ctx.Err() != nil {
		t.Fatal("reset CLI exceeded test deadline")
	}
	return string(out), err
}

type resetRestartEvidence struct {
	ClusterID string
	StartedAt time.Time
	Cookie    http.Cookie
	Stale     Measurement
	AlertID   string
}

const resetRestartFile = "reset-before-restart.json"

var resetEmptyTables = []string{
	"alert_closures", "alerts", "latest_measurements", "measurement_runs", "device_simulations",
	"simulation_history", "sessions", "login_attempts",
}

// TestResetIntegration 經正式 CLI 測試舊版相容、鎖與錯誤目標、憑證、回滾及有界清除。
func TestResetIntegration(t *testing.T) {
	target := openResetTestDatabase(t)
	conn := target.conn
	var empty bool
	if err := conn.QueryRowContext(t.Context(), "SELECT count(*) = 0 FROM pg_catalog.pg_class WHERE relnamespace = 'public'::regnamespace").Scan(&empty); err != nil || !empty {
		t.Fatalf("expected an empty reset fixture: %v", err)
	}
	state, err := os.OpenRoot("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	dbPassword := []byte(resetTestPassword + "\n")
	if err := state.WriteFile("db-password", dbPassword, 0o600); err != nil {
		t.Fatal(err)
	}
	passwords := seedLegacyMeasurementFixture(t, conn, state)
	simulationSQL(t, conn, createMeasurementSchema+createSimulationSchema+addOfflineAlertColumns+createAlertClosureSchema)
	token := target.clusterID + ":1"
	requireResetCheck(t, token)
	// 核准前不得建立世代表，正常啟動升級才補入世代 1。
	if err := conn.QueryRowContext(t.Context(), "SELECT to_regclass('public.iot_data_generation') IS NULL").Scan(&empty); err != nil || !empty {
		t.Fatalf("read-only preflight changed legacy schema: %v", err)
	}
	if err := bootstrap(t.Context(), conn, state); err != nil {
		t.Fatal(err)
	}

	// 以正式登入、控制、回報及結案填滿業務表，捕捉可和新世代碰撞的原始回報。
	now := time.Now().
		UTC().
		Truncate(time.Second)
	f, observed := newOfflineFixture(t, conn, now, false)
	f.hq = f.login(t, "hq", passwords["hq"])
	north := f.login(t, "north", passwords["north"])
	controlSimulation(t, f.measurementFixture, "north-01", "abnormal", "0", simulationState{Mode: "abnormal", Version: "1", CanReplay: true})
	stale := expectSimulationSample(t, observed, "north-01", 30, true)
	if stale.DataGeneration != 1 ||
		stale.RunID != 1 ||
		stale.Sequence != 0 {
		t.Fatalf("legacy startup did not retain the original measurement generation: %+v", stale)
	}
	alert := temperatureAlert(t, f, stale.DeviceID)
	recovered := stale
	recovered.Sequence++
	recovered.Temperature = 23
	f.submit(t, recovered, true)
	f.request(t, http.MethodPost, "/api/alerts/"+alert.ID+"/close", closureJSON(t, "確認設備恢復"), north, http.StatusOK)
	f.app.simulator.close()

	// 同名異 schema 與公開 schema 內的無關資料都必須留下，告警流水號也不可回捲。
	simulationSQL(t, conn, `
		CREATE TABLE public.reset_fixture_keep (id text PRIMARY KEY);
		INSERT INTO public.reset_fixture_keep VALUES ('keep');
		CREATE SCHEMA reset_fixture_other;
		CREATE TABLE reset_fixture_other.accounts (id text PRIMARY KEY);
		INSERT INTO reset_fixture_other.accounts VALUES ('same-name');
	`)
	before := resetSnapshot(t, conn)
	for _, table := range resetEmptyTables {
		if before["public."+table] == "[]" {
			t.Fatalf("reset rollback fixture did not populate %s", table)
		}
	}
	demoFile, err := state.ReadFile(demoPasswordsName)
	if err != nil {
		t.Fatal(err)
	}
	// 無確認、錯誤 token、運作中的應用鎖及錯誤實際目標都不得改動資料。
	requireResetCLIError(t, "invalid command", "reset-apply")
	requireResetCLIError(t, "invalid command", "reset-apply", token+"\n")
	requireResetCLIError(t, "confirmed target or generation changed", "reset-apply", "1:1")
	requireResetCLIError(t, "confirmed target or generation changed", "reset-apply", target.clusterID+":2")
	if err := lockIoTDatabase(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
	requireResetCheck(t, token)
	requireResetCLIError(t, "holds the PostgreSQL startup lock", "reset-apply", token)
	simulationSQL(t, conn, fmt.Sprintf("SELECT pg_advisory_unlock(%d)", lockKey))
	testResetWrongTarget(t, passwords)
	testResetCredentials(t, state, token, passwords, demoFile, dbPassword)
	assertResetSnapshot(t, before, resetSnapshot(t, conn))

	// 欄位不符、連鎖外鍵與世代溢位都不得啟動重設。
	testResetSchema(t, conn, target.clusterID)
	assertResetSnapshot(t, before, resetSnapshot(t, conn))

	// 非連鎖延期外鍵在提交才失敗，完整回滾清除、重建及世代輪替。
	simulationSQL(t, conn, `
		CREATE TABLE public.reset_fixture_restrict (alert_id bigint REFERENCES public.alerts(id) DEFERRABLE INITIALLY DEFERRED);
		INSERT INTO public.reset_fixture_restrict SELECT id FROM public.alerts;
	`)
	requireResetCheck(t, token)
	requireResetCLIError(t, "commit IoT reset transaction", "reset-apply", token)
	assertResetSnapshot(t, before, resetSnapshot(t, conn))
	f.request(t, http.MethodGet, "/api/session", "", f.hq, http.StatusOK)
	var retained int
	if err := conn.QueryRowContext(t.Context(), "SELECT count(*) FROM public.reset_fixture_restrict").Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("deferred failure changed unrelated rows: %v", err)
	}
	simulationSQL(t, conn, "DROP TABLE public.reset_fixture_restrict")

	// 套用成功才輪替世代、撤銷登入並重建帳號，外部憑證與無關資料維持原樣。
	output, err := runResetCLI(t, "reset-apply", token)
	if err != nil || output != "" {
		t.Fatalf("reset apply did not succeed silently: %v", err)
	}
	requireResetCheck(t, target.clusterID+":2")
	requireResetCLIError(t, "confirmed target or generation changed", "reset-apply", token)
	after := resetSnapshot(t, conn)
	for _, table := range resetEmptyTables {
		if after["public."+table] != "[]" {
			t.Fatalf("reset kept rows in %s", table)
		}
	}
	for _, table := range []string{"public.devices", "public.sites", "public.reset_fixture_keep", "reset_fixture_other.accounts", "public.alerts_id_seq"} {
		if before[table] != after[table] {
			t.Fatalf("reset changed protected data or rebuilt different seed rows in %s", table)
		}
	}
	if before["public.accounts"] == after["public.accounts"] {
		t.Fatal("reset did not replace the original password salts")
	}
	for name, expected := range map[string][]byte{demoPasswordsName: demoFile, "db-password": dbPassword} {
		got, err := state.ReadFile(name)
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("reset changed protected file %s: %v", name, err)
		}
	}
	f.request(t, http.MethodGet, "/api/session", "", f.hq, http.StatusUnauthorized)
	f.request(t, http.MethodGet, "/api/events", "", f.hq, http.StatusUnauthorized)
	evidence, err := json.Marshal(resetRestartEvidence{target.clusterID, target.startedAt, *f.hq, stale, alert.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.WriteFile(resetRestartFile, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestResetRestart 證明同一資料庫重啟後，新世代可收回報，原樣舊回報及舊登入已失效。
func TestResetRestart(t *testing.T) {
	target := openResetTestDatabase(t)
	state, err := os.OpenRoot("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	data, err := readPrivateFile(state, resetRestartFile)
	if err != nil {
		t.Fatal(err)
	}
	var before resetRestartEvidence
	if err := json.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	if target.clusterID != before.ClusterID || !target.startedAt.After(before.StartedAt) {
		t.Fatalf("expected restart of the same PostgreSQL volume: %s -> %s", before.StartedAt, target.startedAt)
	}
	startupSimulationFixture(t, target.conn, state)
	requireResetCheck(t, target.clusterID+":2")
	passwords, err := demoPasswords(state, false)
	if err != nil {
		t.Fatal(err)
	}
	app := measurementTestApplication(target.conn)
	// 凍結產生時間並捕捉模擬器首筆，讓舊回報先採用測試，排除重複與過期干擾。
	app.measurementNow = func() time.Time { return before.Stale.GeneratedAt }
	observed := make(chan Measurement, 1)
	// 先攔住夾具內的發送，核對識別後再透過正式採用入口提交。
	app.simulator = newSimulator(t.Context(), func(_ context.Context, sample Measurement) (bool, error) {
		select {
		case observed <- sample:
		default:
		}
		return false, nil
	})
	t.Cleanup(app.simulator.close)
	f := serveMeasurementFixture(t, app)
	f.request(t, http.MethodGet, "/api/session", "", &before.Cookie, http.StatusUnauthorized)
	f.request(t, http.MethodGet, "/api/events", "", &before.Cookie, http.StatusUnauthorized)
	f.hq = f.login(t, "hq", passwords["hq"])
	north := f.login(t, "north", passwords["north"])
	f.request(t, http.MethodGet, "/api/devices/south-01", "", north, http.StatusNotFound)
	expectSimulation(t, f, before.Stale.DeviceID, simulationState{Mode: "stopped", Version: "0"})
	f.request(t, http.MethodGet, "/api/alerts/"+before.AlertID, "", f.hq, http.StatusNotFound)

	// 同設備、執行、順序、產生時間與數值僅世代不同，舊回報不應影響新展示。
	controlSimulation(t, f, before.Stale.DeviceID, "abnormal", "0", simulationState{Mode: "abnormal", Version: "1", CanReplay: true})
	fresh := waitForSimulation(t, observed)
	withoutGeneration := fresh
	withoutGeneration.DataGeneration = before.Stale.DataGeneration
	if fresh.DataGeneration != 2 || withoutGeneration != before.Stale {
		t.Fatalf("test samples must match except for generation: old=%+v new=%+v", before.Stale, fresh)
	}
	f.submit(t, before.Stale, false)
	// 先讀回舊回報遭拒的結果，再送新回報，避免新資料掩蓋錯誤的狀態變更。
	var device deviceItem
	if err := json.Unmarshal(f.get(t, "/api/devices/"+fresh.DeviceID), &device); err != nil {
		t.Fatal(err)
	}
	if device.OnlineStatus != "awaiting" || device.LatestMeasurement != nil {
		t.Fatalf("old-generation sample changed the awaiting device: %+v", device)
	}
	if page := f.page(t, ""); page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("old-generation sample created an alert: %+v", page)
	}
	f.submit(t, fresh, true)
	expectLatestSimulationSample(t, f, fresh)
	page := f.page(t, "")
	if page.Total != 1 || page.Items[0].ID == before.AlertID {
		t.Fatal("fresh sample failed to create a new alert without reusing the previous identity")
	}
	// 重設後的正常啟動仍自動回報六台，但不可輪替資料世代或重新接納舊登入。
	app.simulator.close()
	if err := stopSimulationsOnStartup(t.Context(), target.conn); err != nil {
		t.Fatal(err)
	}
	if err := app.scanOffline(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	app.simulator = newSimulator(t.Context(), app.SubmitMeasurement)
	t.Cleanup(app.simulator.close)
	if err := app.startDemoSimulations(t.Context()); err != nil {
		t.Fatal(err)
	}
	app.simulator.close()
	var reporting int
	if err := target.conn.QueryRowContext(t.Context(), `
		SELECT count(*) FROM measurement_runs r JOIN latest_measurements m USING (device_id)
		WHERE r.data_generation = 2 AND m.temperature BETWEEN 18 AND 28
	`).Scan(&reporting); err != nil || reporting != 6 {
		t.Fatalf("reset restart did not automatically report all devices in generation 2: count=%d err=%v", reporting, err)
	}
	requireResetCheck(t, target.clusterID+":2")
	f.request(t, http.MethodGet, "/api/session", "", &before.Cookie, http.StatusUnauthorized)
}

// testResetCredentials 破壞夾具的保護權限或憑證內容，驗證預檢及套用都拒絕。
func testResetCredentials(t *testing.T, state *os.Root, token string, passwords map[string]string, demoFile, dbPassword []byte) {
	t.Helper()
	if err := state.Chmod(demoPasswordsName, 0o644); err != nil {
		t.Fatal(err)
	}
	requireResetCLIError(t, "read protected demo credentials", "reset-check")
	requireResetCLIError(t, "read protected demo credentials", "reset-apply", token)
	if err := state.Chmod(demoPasswordsName, 0o600); err != nil {
		t.Fatal(err)
	}
	wrongPasswords := maps.Clone(passwords)
	wrongPasswords["hq"] = randomSecret()
	wrongDemo, err := json.Marshal(wrongPasswords)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.WriteFile(demoPasswordsName, wrongDemo, 0o600); err != nil {
		t.Fatal(err)
	}
	requireResetCLIError(t, "protected demo credentials do not match", "reset-check")
	requireResetCLIError(t, "protected demo credentials do not match", "reset-apply", token)
	if err := state.WriteFile(demoPasswordsName, demoFile, 0o600); err != nil {
		t.Fatal(err)
	}
	// 使用錯誤的 TCP 驗證密碼，確認不會以其他環境憑證繼續。
	if err := state.WriteFile("db-password", []byte("invalid-fixture-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireResetCLIError(t, "connect to PostgreSQL for reset", "reset-check")
	requireResetCLIError(t, "connect to PostgreSQL for reset", "reset-apply", token)
	if err := state.WriteFile("db-password", dbPassword, 0o600); err != nil {
		t.Fatal(err)
	}
}

// testResetSchema 驗證錯誤欄位、可改動外部表的外鍵及世代溢位會被拒絕。
func testResetSchema(t *testing.T, conn *sql.Conn, clusterID string) {
	t.Helper()
	token := clusterID + ":1"
	simulationSQL(t, conn, "ALTER TABLE public.sites ADD COLUMN fixture_wrong text")
	requireResetCLIError(t, "unexpected schema layout", "reset-check")
	requireResetCLIError(t, "unexpected schema layout", "reset-apply", token)
	simulationSQL(t, conn, "ALTER TABLE public.sites DROP COLUMN fixture_wrong")
	simulationSQL(t, conn, `
		CREATE TABLE public.reset_fixture_cascade (device text REFERENCES public.devices(id) ON DELETE CASCADE);
		INSERT INTO public.reset_fixture_cascade VALUES ('north-01');
	`)
	requireResetCLIError(t, "missing or unsafe project tables", "reset-check")
	requireResetCLIError(t, "missing or unsafe project tables", "reset-apply", token)
	simulationSQL(t, conn, "DROP TABLE public.reset_fixture_cascade")
	testResetGenerationForeignKey(t, conn, token)
	simulationSQL(t, conn, "UPDATE public.iot_data_generation SET generation = 9223372036854775807")
	requireResetCLIError(t, "data generation exhausted", "reset-check")
	requireResetCLIError(t, "data generation exhausted", "reset-apply", clusterID+":9223372036854775807")
	simulationSQL(t, conn, "UPDATE public.iot_data_generation SET generation = 1")
}

// testResetGenerationForeignKey 驗證會改動外部資料的世代更新外鍵使預檢及套用都遭拒。
func testResetGenerationForeignKey(t *testing.T, conn *sql.Conn, token string) {
	t.Helper()
	simulationSQL(t, conn, `
		ALTER TABLE public.iot_data_generation
			ADD CONSTRAINT reset_fixture_generation_unique UNIQUE (generation);
		CREATE TABLE reset_fixture_other.generation_link (
			generation bigint NOT NULL REFERENCES public.iot_data_generation(generation) ON UPDATE CASCADE
		);
		INSERT INTO reset_fixture_other.generation_link VALUES (1);
	`)
	// 分開執行兩個 CLI 路徑，讓預檢未擋住時也能查出套用是否改動資料。
	t.Run("reject cascading generation update at preflight", func(t *testing.T) {
		requireResetCLIError(t, "missing or unsafe project tables", "reset-check")
	})
	// 即使已取得舊版的有效確認標記，套用仍須重新驗證外鍵。
	t.Run("reject cascading generation update at apply", func(t *testing.T) {
		requireResetCLIError(t, "missing or unsafe project tables", "reset-apply", token)
	})
	var owned, external int64
	err := conn.QueryRowContext(t.Context(), `
		SELECT g.generation, link.generation
		FROM public.iot_data_generation g CROSS JOIN reset_fixture_other.generation_link link
		WHERE g.singleton
	`).Scan(&owned, &external)
	if err != nil ||
		owned != 1 ||
		external != 1 {
		t.Fatalf("reset changed the original or external generation: owned=%d external=%d: %v", owned, external, err)
	}
	simulationSQL(t, conn, `
		DROP TABLE reset_fixture_other.generation_link;
		ALTER TABLE public.iot_data_generation DROP CONSTRAINT reset_fixture_generation_unique;
	`)
}

// requireResetCheck 核對成功前置檢查的 stdout 只包含一個確認用標記。
func requireResetCheck(t *testing.T, token string) {
	t.Helper()
	got, err := runResetCLI(t, "reset-check")
	if err != nil || got != token+"\n" {
		t.Fatalf("reset check returned %q: %v", got, err)
	}
}

// requireResetCLIError 確認正式 CLI 因指定理由退出，且沒有誤印確認標記。
func requireResetCLIError(t *testing.T, reason string, args ...string) {
	t.Helper()
	output, err := runResetCLI(t, args...)
	var exit *exec.ExitError
	if output != "" ||
		!errors.As(err, &exit) ||
		!strings.Contains(string(exit.Stderr), reason) {
		t.Fatalf("reset CLI did not reject with %q: %v", reason, err)
	}
}

// testResetWrongTarget 核對以相同角色連到另一個資料庫時仍會拒絕。
func testResetWrongTarget(t *testing.T, passwords map[string]string) {
	t.Helper()
	config, err := pgx.ParseConfig("host=127.0.0.1 port=5432 user=iot dbname=postgres sslmode=disable password=" + resetTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	// 使用相同的已驗證 loopback 來源，讓失敗原因只剩資料庫名稱。
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}}
	config.DialFunc = dialer.DialContext
	config.RequireAuth = "scram-sha-256"
	db := stdlib.OpenDB(*config)
	defer db.Close()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := resetDatabase(t.Context(), conn, passwords, ""); err == nil || err.Error() != "IoT reset: unexpected PostgreSQL target" {
		t.Fatalf("wrong PostgreSQL database was not rejected: %v", err)
	}
}

// resetSnapshot 記下業務資料、未受管理的資料和流水號，錯誤時避免輸出敏感欄位。
func resetSnapshot(t *testing.T, conn *sql.Conn) map[string]string {
	t.Helper()
	result := make(map[string]string)
	for _, table := range resetEmptyTables {
		result["public."+table] = ""
	}
	for _, name := range []string{"public.accounts", "public.devices", "public.sites", "public.iot_data_generation",
		"public.reset_fixture_keep", "reset_fixture_other.accounts"} {
		result[name] = ""
	}
	for name := range result {
		var data string
		err := conn.QueryRowContext(t.Context(), "SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)), '[]')::text FROM ONLY "+name+" t").Scan(&data)
		if err != nil {
			t.Fatalf("snapshot failed for %s: %v", name, err)
		}
		result[name] = data
	}
	// PostgreSQL 的序列沒有資料表的複合型別，直接比對目前值及是否已取用。
	var sequence string
	if err := conn.QueryRowContext(t.Context(), "SELECT last_value::text || ':' || is_called::text FROM public.alerts_id_seq").Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	result["public.alerts_id_seq"] = sequence
	return result
}

// assertResetSnapshot 在錯誤前後比對各表內容，省略憑證、雜湊或資料本文。
func assertResetSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	for table, want := range before {
		if after[table] != want {
			t.Fatalf("failed or unconfirmed reset changed %s", table)
		}
	}
}
