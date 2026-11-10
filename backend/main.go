package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	stateDirectory       = "/state"
	lockKey        int64 = 0x494f542d4d4f4e
)

// main 將終止訊號接到服務生命週期，讓 HTTP 與模擬工作走各自的關閉流程。
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 以發布模式啟動 Gin，失敗時記錄可公開的啟動錯誤。
	gin.SetMode(gin.ReleaseMode)
	if err := runCommand(ctx, os.Args[1:], os.Stdout); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

// runCommand 將維護指令與服務啟動分流，維護程序不啟動 HTTP 或模擬器。
func runCommand(ctx context.Context, args []string, stdout io.Writer) error {
	switch {
	case len(args) == 0:
		return run(ctx)
	case len(args) == 1 && args[0] == "reset-check":
		return runReset(ctx, "", stdout)
	case len(args) == 2 &&
		args[0] == "reset-apply" &&
		validResetToken(args[1]):
		return runReset(ctx, args[1], stdout)
	default:
		return errors.New("IoT: invalid command or reset target token")
	}
}

// openProtectedState 開啟正式狀態目錄，啟動與維護都先拒絕權限不符的憑證來源。
func openProtectedState() (*os.Root, error) {
	state, err := os.OpenRoot(stateDirectory)
	if err != nil {
		return nil, operationFailure("open /state", err)
	}
	info, err := state.Stat(".")
	if err != nil {
		state.Close()
		return nil, operationFailure("inspect /state", err)
	}
	if info.Mode() != os.ModeDir|0o700 {
		state.Close()
		return nil, errors.New("IoT startup: /state must have mode 0700")
	}
	return state, nil
}

// iotDatabaseConfig 從受保護的密碼檔組成連線設定，公開錯誤只帶操作名稱。
func iotDatabaseConfig(state *os.Root, host string) (*pgx.ConnConfig, error) {
	passwordFile, err := readPrivateFile(state, "db-password")
	if err != nil {
		return nil, operationFailure("read /state/db-password as a mode-0600 regular file", err)
	}
	password := strings.TrimSuffix(string(passwordFile), "\n")
	if host == "" || password == "" {
		return nil, errors.New("IoT startup: a database host and nonempty database password are required")
	}
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("iot", password),
		Host:     net.JoinHostPort(host, "5432"),
		Path:     "/iot",
		RawQuery: "sslmode=disable&application_name=iot-monitoring",
	}
	config, err := pgx.ParseConfig(dsn.String())
	if err != nil {
		return nil, operationFailure("configure PostgreSQL connection", err)
	}
	// 資料庫使用 UTC，取消請求時優先停止單筆查詢以保留持鎖連線。
	config.RuntimeParams["timezone"] = "UTC"
	config.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		// 優先取消單筆查詢，瀏覽器中斷時可保留原本持鎖的連線。
		return &pgconn.CancelRequestContextWatcherHandler{
			Conn: conn, CancelRequestDelay: 20 * time.Millisecond, DeadlineDelay: time.Second,
		}
	}
	return config, nil
}

// lockIoTDatabase 讓正式服務與重設共用同一個工作階段層級的排他鎖。
func lockIoTDatabase(ctx context.Context, conn *sql.Conn) error {
	var held bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1::bigint)", lockKey).Scan(&held); err != nil {
		return operationFailure("acquire PostgreSQL startup lock", err)
	}
	if !held {
		return errors.New("IoT startup: another instance holds the PostgreSQL startup lock")
	}
	return nil
}

// run 取得本案的 PostgreSQL 啟動鎖，完成初始化與重啟補判後才接受 HTTP 請求。
func run(ctx context.Context) error {
	return runApplication(ctx, nil, nil)
}

// runApplication 保持正式啟動順序，測試可在任何資料寫入前核對隔離目標並準備夾具。
func runApplication(ctx context.Context, check func(context.Context, *sql.Conn, *os.Root) error, setup func(context.Context, *application, *os.Root) (func(), error)) error {
	origin := os.Getenv("IOT_ORIGIN")
	host, err := localOriginHost(origin)
	if err != nil {
		return err
	}
	state, err := openProtectedState()
	if err != nil {
		return err
	}
	defer state.Close()
	config, err := iotDatabaseConfig(state, os.Getenv("IOT_DB_HOST"))
	if err != nil {
		return err
	}
	// 只使用一條連線，讓啟動鎖與後續資料庫操作共用同一個 PostgreSQL 工作階段。
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	defer db.Close()

	// 限制初始化的總時間，並在整個服務生命週期保留取得鎖的連線。
	startup, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, err := db.Conn(startup)
	if err != nil {
		return operationFailure("connect to PostgreSQL", err)
	}
	defer conn.Close()
	// 先確認測試目標才取鎖，取鎖後再查一次，避免兩次啟動之間的狀態變更。
	if check != nil {
		if err := check(startup, conn, state); err != nil {
			return err
		}
	}
	if err := lockIoTDatabase(startup, conn); err != nil {
		return err
	}
	if check != nil {
		if err := check(startup, conn, state); err != nil {
			return err
		}
	}
	// 先準備資料並停用舊模擬執行，再依保留的計時基準補判離線。
	if err := bootstrap(startup, conn, state); err != nil {
		return err
	}
	if err := stopSimulationsOnStartup(startup, conn); err != nil {
		return err
	}
	a := application{
		host: host, origin: origin,
		db:        &lockedDB{conn: conn, gate: make(chan struct{}, 1)},
		hashSlots: make(chan struct{}, 8),
	}
	if err := a.scanOffline(startup, true); err != nil {
		return operationFailure("detect offline devices on startup", err)
	}
	// 服務結束時先停止測試工作，再釋放正式資料庫連線和狀態目錄。
	if setup != nil {
		cleanup, err := setup(startup, &a, state)
		if err != nil {
			return err
		}
		defer cleanup()
	}
	// 解除啟動期限，以服務生命週期管理後續模擬。
	cancel()
	a.simulator = newSimulator(ctx, a.SubmitMeasurement)
	defer a.simulator.close()
	return a.serve(ctx)
}

// localOriginHost 只接受瀏覽器會原樣送出的專用本機 Origin，供 Host 與寫入檢查共用。
func localOriginHost(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err == nil {
		port, portErr := strconv.Atoi(u.Port())
		// 瀏覽器會省略預設埠 80，因此 Host 和 Origin 必須使用非預設埠。
		if portErr == nil &&
			port > 0 &&
			port <= 65535 &&
			port != 80 &&
			u.Host == net.JoinHostPort("iot.localhost", strconv.Itoa(port)) &&
			origin == "http://"+u.Host {
			return u.Host, nil
		}
	}
	return "", errors.New("IoT startup: IOT_ORIGIN must be http://iot.localhost:<port> with a non-default port")
}

// httpServer 為 HTTP 讀寫與標頭設上限，避免請求無限佔用本機服務資源。
func (a *application) httpServer() *http.Server {
	return &http.Server{
		Addr: ":8080", Handler: a.router(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 * 1024, ErrorLog: log.New(io.Discard, "", 0),
	}
}

// serve 管理 HTTP 生命週期與背景巡檢。持鎖連線確認失效時停止服務，避免在失去排他保障後繼續。
func (a *application) serve(ctx context.Context) error {
	server := a.httpServer()
	// 以緩衝通道回收 HTTP 結果，主迴圈同時負責背景巡檢及關閉。
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			return operationFailure("serve HTTP on port 8080", err)
		case <-ctx.Done():
			a.events.stop()
			// 使用獨立期限讓既有請求收尾，逾時則強制關閉。
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := server.Shutdown(shutdown)
			cancel()
			if err != nil {
				return operationFailure("shut down HTTP server", errors.Join(err, server.Close()))
			}
			return nil
		case <-ticker.C:
			// 先確認真正執行探測，再區分排隊未能探測與持鎖連線失效。
			probed, err := a.watchdogProbe(ctx)
			if !probed || ctx.Err() != nil {
				continue
			}
			if err != nil {
				return operationFailure("lost PostgreSQL startup connection", errors.Join(err, server.Close()))
			}
			// 連線有效才補判逾時設備，巡檢錯誤只記錄操作名稱。
			if err := a.scanOffline(ctx, false); err != nil && ctx.Err() == nil {
				log.Print("could not detect offline devices")
			}
			// 清理失敗只記錄操作，啟動鎖的連線仍由上方探測判斷。
			if err := a.pruneLoginAttempts(ctx); err != nil && ctx.Err() == nil {
				log.Print("could not prune expired login attempts")
			}
		}
	}
}
