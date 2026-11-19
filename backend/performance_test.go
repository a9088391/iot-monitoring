package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	performanceReady        = "performance-ready.json"
	performanceCommand      = "performance-command.json"
	performanceStarted      = "performance-started.json"
	performanceClockRequest = "performance-clock-request.json"
	performanceClockReply   = "performance-clock-response.json"
	performanceEvents       = "performance-events.ndjson"
	performanceResult       = "performance-result.json"
)

var performanceNamespace = regexp.MustCompile(`^iot01_test_[0-9a-f]{16}$`)

type performanceDevice struct {
	ID     string `json:"id"`
	SiteID string `json:"siteId"`
}

type performanceReadyInfo struct {
	Namespace   string              `json:"namespace"`
	Devices     []performanceDevice `json:"devices"`
	GoVersion   string              `json:"goVersion"`
	IntervalNS  string              `json:"intervalNs"`
	WarmupSlots int                 `json:"warmupSlots"`
	FormalSlots int                 `json:"formalSlots"`
	TailSlots   int                 `json:"tailSlots"`
}

type performanceStartCommand struct {
	StartDelayMS int `json:"startDelayMs"`
}

type performanceClockMessage struct {
	ID       string `json:"id"`
	ServerNS string `json:"serverNs,omitempty"`
}

type performanceRunResult struct {
	Complete    bool                         `json:"complete"`
	Events      int                          `json:"events"`
	Error       string                       `json:"error,omitempty"`
	Diagnostics *performanceDiagnosticResult `json:"diagnostics,omitempty"`
}

// TestPerformanceStack 在隔離容器執行正式服務，負載經同一程序的測量入口進入。
func TestPerformanceStack(t *testing.T) {
	namespace, enabled := os.LookupEnv("IOT_PERFORMANCE_FIXTURE")
	if !enabled {
		t.Skip("requires the isolated performance stack")
	}
	if !performanceNamespace.MatchString(namespace) || os.Getenv("IOT_DB_HOST") != "db" {
		t.Fatal("invalid performance fixture namespace or database host")
	}
	// 防止 PostgreSQL 用戶端選項或 service 檔在本工作階段偽造隔離標記。
	for _, name := range []string{"PGOPTIONS", "PGSERVICE", "PGSERVICEFILE"} {
		if _, set := os.LookupEnv(name); set {
			t.Fatal("performance fixture: PostgreSQL client option overrides are forbidden")
		}
	}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gin.SetMode(gin.ReleaseMode)
	var workerErr error
	// 先查實際連線和狀態目錄，不能只依賴命令列的隔離旗標。
	check := func(startup context.Context, conn *sql.Conn, state *os.Root) error {
		return verifyPerformanceTarget(startup, conn, state, namespace)
	}
	// 使用既有啟動鎖的連線預載夾具，工作結束前保留狀態目錄和資料庫連線。
	setup := func(startup context.Context, app *application, state *os.Root) (func(), error) {
		devices, err := seedPerformanceDevices(startup, app.db.conn)
		if err != nil {
			return nil, err
		}
		if err := writePerformanceJSON(state, performanceReady, performanceReadyInfo{
			namespace, devices, runtime.Version(), strconv.FormatInt(int64(performanceInterval), 10),
			performanceWarmupSlots, performanceFormalSlots, performanceTailSlots,
		}, false); err != nil {
			return nil, err
		}
		worker, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		// 工作出錯時中止正式服務，讓外層測試可以判定量測未完成。
		go func() {
			err := servePerformanceFiles(worker, app, state, devices)
			if err != nil {
				stop()
			}
			done <- err
		}()
		// 清理會等候工作歸還檔案與測量，不在關閉後續寫證據。
		return func() {
			cancel()
			workerErr = <-done
		}, nil
	}
	serverErr := runApplication(ctx, check, setup)
	if err := errors.Join(serverErr, workerErr); err != nil {
		t.Fatal(err)
	}
}

// verifyPerformanceTarget 在正式初始化前查證本次 PostgreSQL 實例和全新的狀態目錄。
func verifyPerformanceTarget(ctx context.Context, conn *sql.Conn, state *os.Root, namespace string) error {
	var allowed bool
	err := conn.QueryRowContext(ctx, `
		SELECT current_database() = 'iot'
			AND current_user = 'iot'
			AND session_user = 'iot'
			AND current_setting('data_directory') = $2
			AND COALESCE(current_setting('iot.performance_fixture', true) = $1, false)
			AND COALESCE(
				family(inet_server_addr()) = 4 AND family(inet_client_addr()) = 4
				AND inet_server_addr() <> inet_client_addr()
				AND NOT (inet_server_addr() <<= inet '127.0.0.0/8')
				AND NOT (inet_client_addr() <<= inet '127.0.0.0/8'),
				false
			)
			AND NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_class c
				JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'
			)
	`, namespace, resetDataDir).Scan(&allowed)
	if err != nil || !allowed {
		return errors.New("performance fixture: PostgreSQL identity, marker or empty schema mismatch")
	}
	return verifyPerformanceState(state)
}

// verifyPerformanceState 只允許啟動器的資料庫密碼與正在等待健康檢查的空維護鎖。
func verifyPerformanceState(state *os.Root) error {
	root, err := state.Open(".")
	if err != nil {
		return errors.New("performance fixture: inspect protected state")
	}
	entries, err := root.ReadDir(3)
	if errors.Join(err, root.Close()) != nil {
		return errors.New("performance fixture: read protected state directory")
	}
	hasPassword := false
	for _, entry := range entries {
		switch {
		case entry.Name() == "db-password" && entry.Type().IsRegular():
			hasPassword = true
		case entry.Name() == ".maintenance-lock" && entry.IsDir():
			lock, err := state.Open(entry.Name())
			if err != nil {
				return errors.New("performance fixture: inspect maintenance lock")
			}
			info, statErr := lock.Stat()
			_, readErr := lock.Readdirnames(1)
			if errors.Join(statErr, lock.Close()) != nil ||
				info.Mode() != os.ModeDir|0o700 ||
				!errors.Is(readErr, io.EOF) {
				return errors.New("performance fixture: expected an empty private maintenance lock")
			}
		default:
			return errors.New("performance fixture: expected a fresh protected state directory")
		}
	}
	if !hasPassword {
		return errors.New("performance fixture: missing database password file")
	}
	return nil
}

// seedPerformanceDevices 只在新夾具內替換六台預載設備並授權固定的測量執行。
func seedPerformanceDevices(ctx context.Context, conn *sql.Conn) ([]performanceDevice, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, operationFailure("begin performance fixture setup", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM devices;
		INSERT INTO devices (id, name, site_id)
		SELECT d.id, d.id, s.id FROM sites s
		CROSS JOIN generate_series(1, 50) n
		CROSS JOIN LATERAL (SELECT s.id || '-' || lpad(n::text, 3, '0') AS id) d;
		INSERT INTO measurement_runs (device_id, data_generation, run_id)
		SELECT id, generation, 1 FROM devices CROSS JOIN public.iot_data_generation WHERE singleton;
	`); err != nil {
		return nil, operationFailure("seed performance devices and allowed runs", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, site_id FROM devices ORDER BY id COLLATE \"C\"")
	if err != nil {
		return nil, operationFailure("read performance roster", err)
	}
	defer rows.Close()
	devices := make([]performanceDevice, 0, performanceDeviceCount)
	for rows.Next() {
		var device performanceDevice
		if err := rows.Scan(&device.ID, &device.SiteID); err != nil {
			return nil, operationFailure("read performance device", err)
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return nil, operationFailure("complete performance roster", err)
	}
	if len(devices) != performanceDeviceCount {
		return nil, errors.New("performance fixture: expected 100 devices")
	}
	if err := tx.Commit(); err != nil {
		return nil, operationFailure("commit performance fixture setup", err)
	}
	return devices, nil
}

// servePerformanceFiles 在命令到來前後都提供同一伺服器時計，且只接受一次負載命令。
func servePerformanceFiles(ctx context.Context, app *application, state *os.Root, devices []performanceDevice) error {
	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var lastClockID string
	var running <-chan error
	// 即使讀取控制檔失敗，也先取消並等候負載寫完可用證據再關閉狀態目錄。
	defer func() {
		cancel()
		if running != nil {
			<-running
		}
	}()
	started := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-running:
			running = nil
			if err != nil {
				return err
			}
		case <-ticker.C:
			// 以新請求 ID 回傳一次即時伺服器時間，主機端用往返時間界定誤差。
			request, exists, err := readPerformanceJSON[performanceClockMessage](state, performanceClockRequest)
			if err != nil {
				return err
			}
			if exists {
				if request.ID == "" ||
					len(request.ID) > 128 ||
					request.ID == lastClockID {
					return errors.New("performance fixture: invalid clock request id")
				}
				// 主機等候相符的回覆才發布下一筆，先移除這筆請求再送出回覆。
				if err := state.Remove(performanceClockRequest); err != nil {
					return errors.New("performance fixture: consume clock request")
				}
				reply := performanceClockMessage{request.ID, performanceNS(app.measurementClock())}
				if err := writePerformanceJSON(state, performanceClockReply, reply, true); err != nil {
					return err
				}
				lastClockID = request.ID
			}
			if started {
				continue
			}
			command, exists, err := readPerformanceJSON[performanceStartCommand](state, performanceCommand)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			if command.StartDelayMS < 100 || command.StartDelayMS > 30_000 {
				return errors.New("performance fixture: invalid start delay")
			}
			start := app.measurementClock().Add(time.Duration(command.StartDelayMS) * time.Millisecond)
			done := make(chan error, 1)
			running, started = done, true
			// 負載排程獨立於時基回覆，長時間量測仍可重新校正。
			go func() {
				result, err := recordPerformanceLoad(ctx, app, state, devices, start)
				done <- errors.Join(err, writePerformanceJSON(state, performanceResult, result, false))
			}()
		}
	}
}

// readPerformanceJSON 只接收原子發布且權限受保護的量測控制檔。
func readPerformanceJSON[T any](state *os.Root, name string) (value T, exists bool, err error) {
	data, err := readPrivateFile(state, name)
	if errors.Is(err, os.ErrNotExist) {
		return value, false, nil
	}
	if err != nil {
		return value, false, fmt.Errorf("performance fixture: read %s", name)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, false, fmt.Errorf("performance fixture: decode %s", name)
	}
	return value, true, nil
}

// writePerformanceJSON 先關閉完整的私密檔案，再原子發布給主機讀取。
func writePerformanceJSON(state *os.Root, name string, value any, replace bool) error {
	temporary := name + ".tmp"
	file, err := state.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("performance fixture: open temporary %s", name)
	}
	defer state.Remove(temporary)
	if err := errors.Join(json.NewEncoder(file).Encode(value), file.Close()); err != nil {
		return fmt.Errorf("performance fixture: write %s", name)
	}
	if replace {
		err = state.Rename(temporary, name)
	} else {
		err = state.Link(temporary, name)
	}
	if err != nil {
		return fmt.Errorf("performance fixture: publish %s", name)
	}
	return nil
}

// performanceNS 將伺服器 Unix 奈秒編成字串，保留 JavaScript 整數精度以外的位元。
func performanceNS(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return strconv.FormatInt(at.UnixNano(), 10)
}

// TestPerformanceStateGuard 確認隔離工作只接受啟動器的密碼與真實空鎖目錄。
func TestPerformanceStateGuard(t *testing.T) {
	state, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.WriteFile("db-password", []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.Mkdir(".maintenance-lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyPerformanceState(state); err != nil {
		t.Fatal(err)
	}
	// 多出的狀態檔可能屬於上次執行，必須在碰觸資料庫前拒絕。
	if err := state.WriteFile(performanceCommand, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPerformanceState(state); err == nil {
		t.Fatal("accepted stale performance command")
	}
	if err := state.Remove(performanceCommand); err != nil {
		t.Fatal(err)
	}
	if err := state.Remove(".maintenance-lock"); err != nil {
		t.Fatal(err)
	}
	if err := state.Symlink(".", ".maintenance-lock"); err != nil {
		t.Fatal(err)
	}
	if err := verifyPerformanceState(state); err == nil {
		t.Fatal("accepted a symlink in the protected state")
	}
}
