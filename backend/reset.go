package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

const (
	resetDBHost       = "127.0.0.1"
	resetDBClient     = "127.0.0.2"
	resetDataDir      = "/var/lib/postgresql/18/docker"
	generationTable   = "iot_data_generation"
	initialGeneration = 1
)

const createDataGenerationSchema = `
	CREATE TABLE public.iot_data_generation (
		singleton boolean PRIMARY KEY CHECK (singleton),
		generation bigint NOT NULL CHECK (generation > 0)
	);
	INSERT INTO public.iot_data_generation (singleton, generation) VALUES (true, 1);
`

// 固定清除順序以先移除外鍵的依賴端。資料世代表另外更新，序列與其他資料表不重建。
var resetRelations = [...]struct {
	name, columns string
}{
	{"alert_closures", "actor:text,alert_id:bigint,closed_at_ns:bigint,result:text"},
	{"alerts", "deadline_at_ns:bigint,detected_at_ns:bigint,detected_at_restart:boolean,device_id:text,id:bigint,recovered_at_ns:bigint,temperature:double precision,type:text"},
	{"latest_measurements", "device_id:text,generated_at_ns:bigint,received_at_ns:bigint,sequence:bigint,temperature:double precision"},
	{"measurement_runs", "data_generation:bigint,device_id:text,run_id:bigint"},
	{"device_simulations", "device_id:text,first_expected_at_ns:bigint,mode:text,version:bigint"},
	{"simulation_history", "action:text,actor:text,device_id:text,occurred_at_ns:bigint,version:bigint"},
	{"sessions", "expires_at:timestamp with time zone,token_hash:bytea,username:text"},
	{"login_attempts", "expires_at:timestamp with time zone,key_hash:bytea,scope:text"},
	{"accounts", "password_hash:text,role:text,site_id:text,username:text"},
	{"devices", "id:text,name:text,site_id:text"},
	{"sites", "id:text,name:text"},
}

// validResetToken 要求兩段正整數皆為原樣十進位表示，拒絕缺漏或額外內容。
func validResetToken(token string) bool {
	cluster, generation, found := strings.Cut(token, ":")
	if !found {
		return false
	}
	for _, value := range []string{cluster, generation} {
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil ||
			number <= 0 ||
			strconv.FormatInt(number, 10) != value {
			return false
		}
	}
	return true
}

// hasDataGeneration 辨識舊版缺少世代表的狀態，查詢本身不建立結構。
func hasDataGeneration(ctx context.Context, tx *sql.Tx) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass('public.iot_data_generation') IS NOT NULL").Scan(&exists)
	return exists, err
}

// readDataGeneration 要求已存在的世代表保有有效的單例值，避免意外回到世代 1。
func readDataGeneration(ctx context.Context, tx *sql.Tx) (int64, error) {
	var generation int64
	err := tx.QueryRowContext(ctx, "SELECT generation FROM public.iot_data_generation WHERE singleton").Scan(&generation)
	if err == nil && generation <= 0 {
		return 0, errors.New("invalid IoT data generation")
	}
	return generation, err
}

// runReset 只從受保護的憑證連至資料庫容器的 TCP loopback，維護期間不接受服務的連線環境設定。
func runReset(ctx context.Context, expected string, stdout io.Writer) error {
	state, err := openProtectedState()
	if err != nil {
		return err
	}
	defer state.Close()
	passwords, err := demoPasswords(state, false)
	if err != nil {
		return operationFailure("read protected demo credentials for reset", err)
	}
	config, err := iotDatabaseConfig(state, resetDBHost)
	if err != nil {
		return err
	}
	// 排除外部 PG 環境注入的 session 選項，並限制連線只使用一個本機目的地。
	config.RuntimeParams = map[string]string{
		"application_name": "iot-monitoring-reset",
		"search_path":      "pg_catalog,public",
		"timezone":         "UTC",
	}
	config.Fallbacks = nil
	// 官方映像預設信任 127.0.0.1 來源。仍從同一個 loopback 連入，但用另一個來源命中密碼規則。
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(resetDBClient)}}
	config.DialFunc = dialer.DialContext
	config.RequireAuth = "scram-sha-256"
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return operationFailure("connect to PostgreSQL for reset", err)
	}
	defer conn.Close()
	// 唯讀預檢可在服務運作時執行。真正套用必須先排除同庫的服務與其他維護程序。
	if expected != "" {
		if err := lockIoTDatabase(ctx, conn); err != nil {
			return err
		}
	}
	token, err := resetDatabase(ctx, conn, passwords, expected)
	if err != nil || expected != "" {
		return err
	}
	// 只有完整通過唯讀檢查才輸出確認用的目標，讓外層命令能安全核對。
	_, err = fmt.Fprintln(stdout, token)
	return operationFailure("write reset target token", err)
}

// resetDatabase 在一致的資料庫快照內核對目標。套用時由呼叫端持有啟動鎖，所有改動由同一交易提交。
func resetDatabase(ctx context.Context, conn *sql.Conn, passwords map[string]string, expected string) (string, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: expected == ""})
	if err != nil {
		return "", operationFailure("begin reset transaction", err)
	}
	defer tx.Rollback()

	// 確認實際連線對端、資料庫身分及 PostgreSQL 的掛載子目錄。
	var clusterID string
	var target bool
	err = tx.QueryRowContext(ctx, `
		SELECT system_identifier::text,
			current_database() = 'iot' AND current_user = 'iot' AND session_user = 'iot'
			AND COALESCE(inet_server_addr() = $1::inet AND inet_client_addr() = $2::inet, false)
			AND current_setting('data_directory') = $3
		FROM pg_catalog.pg_control_system()
	`, resetDBHost, resetDBClient, resetDataDir).Scan(&clusterID, &target)
	if err != nil {
		return "", operationFailure("inspect PostgreSQL reset target", err)
	}
	if !target {
		return "", errors.New("IoT reset: unexpected PostgreSQL target")
	}
	hasGeneration, err := hasDataGeneration(ctx, tx)
	if err != nil {
		return "", operationFailure("inspect reset data generation table", err)
	}
	if err := verifyResetSchema(ctx, tx, hasGeneration); err != nil {
		return "", err
	}
	generation := int64(initialGeneration)
	if hasGeneration {
		generation, err = readDataGeneration(ctx, tx)
		if err != nil {
			return "", operationFailure("read reset data generation", err)
		}
	}
	token := fmt.Sprintf("%s:%d", clusterID, generation)
	if !validResetToken(token) || (expected != "" && expected != token) {
		return "", errors.New("IoT reset: confirmed target or generation changed")
	}
	if generation == math.MaxInt64 {
		return "", errors.New("IoT reset: data generation exhausted")
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM public.accounts").Scan(&count); err != nil {
		return "", operationFailure("inspect reset demo accounts", err)
	}
	if err := verifyDemoAccounts(ctx, tx, count, passwords); err != nil {
		return "", err
	}
	if expected == "" {
		return token, operationFailure("finish read-only reset check", tx.Commit())
	}

	// 只有核對完成才建表或刪除。清除、重建與輪替世代任一失敗都會一起回滾。
	if !hasGeneration {
		if _, err := tx.ExecContext(ctx, createDataGenerationSchema); err != nil {
			return "", operationFailure("initialize reset data generation", err)
		}
	}
	var clearSQL strings.Builder
	for _, relation := range resetRelations {
		fmt.Fprintf(&clearSQL, "DELETE FROM ONLY public.%s;\n", relation.name)
	}
	if _, err := tx.ExecContext(ctx, clearSQL.String()); err != nil {
		return "", operationFailure("clear IoT demo data", err)
	}
	if err := seedDemoData(ctx, tx, passwords); err != nil {
		return "", err
	}
	var next int64
	err = tx.QueryRowContext(ctx, `
		UPDATE ONLY public.iot_data_generation SET generation = $1 WHERE singleton RETURNING generation
	`, generation+1).Scan(&next)
	if err != nil {
		return "", operationFailure("advance reset data generation", err)
	}
	if next != generation+1 {
		return "", errors.New("IoT reset: data generation did not advance")
	}
	return "", operationFailure("commit IoT reset transaction", tx.Commit())
}

// verifyResetSchema 只接受本案擁有且欄位相符的實體表，並拒絕可透過自訂觸發器、規則或外鍵改動其他資料表的配置。
func verifyResetSchema(ctx context.Context, tx *sql.Tx, hasGeneration bool) error {
	names := make([]string, 0, len(resetRelations)+1)
	expected := make(map[string]string, len(resetRelations)+1)
	for _, relation := range resetRelations {
		names = append(names, relation.name)
		expected[relation.name] = relation.columns
	}
	if hasGeneration {
		names = append(names, generationTable)
		expected[generationTable] = "generation:bigint,singleton:boolean"
	}
	// 清除可能觸發外部刪除動作，世代輪替另外可能觸發外部更新動作。
	rows, err := tx.QueryContext(ctx, `
		WITH owned AS (
			SELECT c.oid, c.relname
			FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public'
				AND c.relname = ANY($1::text[])
				AND c.relkind = 'r'
				AND NOT c.relispartition
				AND NOT c.relhasrules
				AND pg_catalog.pg_get_userbyid(c.relowner) = current_user
				AND NOT EXISTS (
					SELECT 1 FROM pg_catalog.pg_trigger t
					WHERE t.tgrelid = c.oid AND NOT t.tgisinternal AND t.tgenabled <> 'D'
				)
		)
		SELECT o.relname, string_agg(a.attname || ':' || pg_catalog.format_type(a.atttypid, a.atttypmod),
			',' ORDER BY a.attname::text COLLATE "C")
		FROM owned o JOIN pg_catalog.pg_attribute a ON a.attrelid = o.oid
		WHERE a.attnum > 0 AND NOT a.attisdropped AND NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_constraint fk
			WHERE fk.contype = 'f'
				AND fk.confrelid = o.oid
				AND fk.conrelid NOT IN (SELECT oid FROM owned)
				AND (
					fk.confdeltype IN ('c', 'n', 'd')
					OR (o.relname = $2 AND fk.confupdtype IN ('c', 'n', 'd'))
				)
		)
		GROUP BY o.relname
	`, names, generationTable)
	if err != nil {
		return operationFailure("inspect IoT reset schema", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, columns string
		if err := rows.Scan(&name, &columns); err != nil {
			return operationFailure("read IoT reset table layout", err)
		}
		if expected[name] != columns {
			return errors.New("IoT reset: unexpected schema layout")
		}
		delete(expected, name)
	}
	if err := rows.Err(); err != nil {
		return operationFailure("read IoT reset schema", err)
	}
	if len(expected) != 0 {
		return errors.New("IoT reset: missing or unsafe project tables")
	}
	return nil
}
