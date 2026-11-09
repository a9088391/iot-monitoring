package main

import (
	"context"
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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gin.SetMode(gin.ReleaseMode)
	if err := run(ctx); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	origin := os.Getenv("IOT_ORIGIN")
	host, err := localOriginHost(origin)
	if err != nil {
		return err
	}

	state, err := os.OpenRoot(stateDirectory)
	if err != nil {
		return operationFailure("open /state", err)
	}
	defer state.Close()
	info, err := state.Stat(".")
	if err != nil {
		return operationFailure("inspect /state", err)
	}
	if info.Mode() != os.ModeDir|0o700 {
		return errors.New("IoT startup: /state must have mode 0700")
	}

	passwordFile, err := readPrivateFile(state, "db-password")
	if err != nil {
		return operationFailure("read /state/db-password as a mode-0600 regular file", err)
	}
	password := strings.TrimSuffix(string(passwordFile), "\n")
	dbHost := os.Getenv("IOT_DB_HOST")
	if dbHost == "" || password == "" {
		return errors.New("IoT startup: IOT_DB_HOST and a nonempty database password are required")
	}
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("iot", password),
		Host:     net.JoinHostPort(dbHost, "5432"),
		Path:     "/iot",
		RawQuery: "sslmode=disable&application_name=iot-monitoring",
	}
	config, err := pgx.ParseConfig(dsn.String())
	if err != nil {
		return operationFailure("configure PostgreSQL connection", err)
	}
	config.RuntimeParams["timezone"] = "UTC"
	config.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		// 優先取消單筆查詢，瀏覽器中斷時可保留原本持鎖的連線。
		return &pgconn.CancelRequestContextWatcherHandler{
			Conn: conn, CancelRequestDelay: 20 * time.Millisecond, DeadlineDelay: time.Second,
		}
	}
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	defer db.Close()

	startup, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, err := db.Conn(startup)
	if err != nil {
		return operationFailure("connect to PostgreSQL", err)
	}
	defer conn.Close()
	var held bool
	if err := conn.QueryRowContext(startup, "SELECT pg_try_advisory_lock($1::bigint)", lockKey).Scan(&held); err != nil {
		return operationFailure("acquire PostgreSQL startup lock", err)
	}
	if !held {
		return errors.New("IoT startup: another instance holds the PostgreSQL startup lock")
	}
	if err := bootstrap(startup, conn, state); err != nil {
		return err
	}
	if err := stopSimulationsOnStartup(startup, conn); err != nil {
		return err
	}
	cancel()
	a := application{
		host: host, origin: origin,
		db:        &lockedDB{conn: conn, gate: make(chan struct{}, 1)},
		hashSlots: make(chan struct{}, 8),
	}
	a.simulator = newSimulator(ctx, a.SubmitMeasurement)
	defer a.simulator.close()
	return a.serve(ctx)
}

func localOriginHost(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err == nil {
		port, portErr := strconv.Atoi(u.Port())
		// 瀏覽器會省略預設埠 80，因此 Host 和 Origin 必須使用非預設埠。
		if portErr == nil && port > 0 && port <= 65535 && port != 80 &&
			u.Host == net.JoinHostPort("iot.localhost", strconv.Itoa(port)) &&
			origin == "http://"+u.Host {
			return u.Host, nil
		}
	}
	return "", errors.New("IoT startup: IOT_ORIGIN must be http://iot.localhost:<port> with a non-default port")
}

func (a *application) serve(ctx context.Context) error {
	server := &http.Server{
		Addr: ":8080", Handler: a.router(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 * 1024, ErrorLog: log.New(io.Discard, "", 0),
	}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			return operationFailure("serve HTTP on port 8080", err)
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := server.Shutdown(shutdown)
			cancel()
			if err != nil {
				return operationFailure("shut down HTTP server", errors.Join(err, server.Close()))
			}
			return nil
		case <-ticker.C:
			probed, err := a.watchdogProbe(ctx)
			if !probed || ctx.Err() != nil {
				continue
			}
			if err != nil {
				return operationFailure("lost PostgreSQL startup connection", errors.Join(err, server.Close()))
			}
			// 清理失敗只記錄操作，啟動鎖的連線仍由上方探測判斷。
			if err := a.pruneLoginAttempts(ctx); err != nil && ctx.Err() == nil {
				log.Print("could not prune expired login attempts")
			}
		}
	}
}
