package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const demoPasswordsName = "demo-passwords.json"

var seedAccounts = [...]struct {
	username, role string
	siteID         *string
}{
	{"hq", "headquarters", nil},
	{"north", "site", new("north")},
	{"south", "site", new("south")},
}

const createSchema = `
	CREATE TABLE IF NOT EXISTS sites (
		id text PRIMARY KEY,
		name text NOT NULL
	);
	CREATE TABLE IF NOT EXISTS devices (
		id text PRIMARY KEY,
		name text NOT NULL,
		site_id text NOT NULL REFERENCES sites(id)
	);
	CREATE TABLE IF NOT EXISTS accounts (
		username text PRIMARY KEY,
		role text NOT NULL,
		site_id text REFERENCES sites(id),
		password_hash text NOT NULL,
		CHECK ((role = 'headquarters' AND site_id IS NULL) OR (role = 'site' AND site_id IS NOT NULL))
	);
	CREATE TABLE IF NOT EXISTS sessions (
		token_hash bytea PRIMARY KEY,
		username text NOT NULL REFERENCES accounts(username),
		expires_at timestamptz NOT NULL
	);
	CREATE TABLE IF NOT EXISTS login_attempts (
		scope text NOT NULL CHECK (scope IN ('source', 'account')),
		key_hash bytea NOT NULL CHECK (octet_length(key_hash) = 32),
		expires_at timestamptz NOT NULL
	);
	CREATE INDEX IF NOT EXISTS login_attempts_quota_idx ON login_attempts (scope, key_hash, expires_at);
	CREATE INDEX IF NOT EXISTS login_attempts_expiry_idx ON login_attempts (expires_at);
`

const createSeedDevices = `
	INSERT INTO sites (id, name) VALUES ('north', '北區場域'), ('south', '南區場域');
	INSERT INTO devices (id, name, site_id) VALUES
		('north-01', '北區設備 01', 'north'), ('north-02', '北區設備 02', 'north'), ('north-03', '北區設備 03', 'north'),
		('south-01', '南區設備 01', 'south'), ('south-02', '南區設備 02', 'south'), ('south-03', '南區設備 03', 'south');
`

const createMeasurementSchema = `
	CREATE TABLE IF NOT EXISTS measurement_runs (
		device_id text PRIMARY KEY REFERENCES devices(id),
		data_generation bigint NOT NULL CHECK (data_generation > 0),
		run_id bigint NOT NULL CHECK (run_id > 0)
	);
	CREATE TABLE IF NOT EXISTS latest_measurements (
		device_id text PRIMARY KEY REFERENCES devices(id),
		temperature double precision NOT NULL,
		sequence bigint NOT NULL CHECK (sequence >= 0),
		generated_at_ns bigint NOT NULL,
		received_at_ns bigint NOT NULL CHECK (received_at_ns >= generated_at_ns)
	);
	CREATE TABLE IF NOT EXISTS alerts (
		id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		device_id text NOT NULL REFERENCES devices(id),
		type text NOT NULL,
		temperature double precision NOT NULL,
		detected_at_ns bigint NOT NULL,
		recovered_at_ns bigint CHECK (recovered_at_ns >= detected_at_ns)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS alerts_active_device_type_idx
		ON alerts (device_id, type) WHERE recovered_at_ns IS NULL;
`

const createSimulationSchema = `
	CREATE TABLE IF NOT EXISTS device_simulations (
		device_id text PRIMARY KEY REFERENCES devices(id),
		mode text NOT NULL CHECK (mode IN ('stopped', 'normal', 'abnormal')),
		version bigint NOT NULL CHECK (version > 0),
		first_expected_at_ns bigint
	);
	CREATE TABLE IF NOT EXISTS simulation_history (
		device_id text NOT NULL REFERENCES devices(id),
		version bigint NOT NULL CHECK (version > 0),
		actor text NOT NULL REFERENCES accounts(username),
		action text NOT NULL CHECK (action IN ('normal', 'abnormal', 'stop', 'replay')),
		occurred_at_ns bigint NOT NULL,
		PRIMARY KEY (device_id, version)
	);
`

const addOfflineAlertColumns = `
	ALTER TABLE alerts
		ALTER COLUMN temperature DROP NOT NULL,
		ADD COLUMN IF NOT EXISTS deadline_at_ns bigint,
		ADD COLUMN IF NOT EXISTS detected_at_restart boolean NOT NULL DEFAULT false;
`

// bootstrap 在單一資料庫交易中準備結構及展示資料，並核對重啟前留下的本機憑證。
func bootstrap(ctx context.Context, conn *sql.Conn, state *os.Root) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return operationFailure("begin seed transaction", err)
	}
	defer tx.Rollback()

	// 套用現有與新增的資料表後，依資料庫是否已有帳號選擇載入或初始化憑證。
	if _, err := tx.ExecContext(ctx, createSchema+createMeasurementSchema+createSimulationSchema+addOfflineAlertColumns); err != nil {
		return operationFailure("create IoT schema", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&count); err != nil {
		return operationFailure("inspect existing demo accounts", err)
	}
	passwords, err := demoPasswords(state, count == 0)
	if err != nil {
		return operationFailure("read or initialize protected demo credentials", err)
	}

	// 首次啟動才預載場域、設備及帳號，資料庫只保存密碼雜湊。
	if count == 0 {
		if _, err := tx.ExecContext(ctx, createSeedDevices); err != nil {
			return operationFailure("insert seed sites and devices", err)
		}
		for _, account := range seedAccounts {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO accounts (username, role, site_id, password_hash)
				VALUES ($1, $2, $3, $4)
			`, account.username, account.role, account.siteID, hashPassword(passwords[account.username])); err != nil {
				return operationFailure("insert demo account", err)
			}
		}
	} else {
		// 重啟時核對既有帳號的角色、場域及密碼，避免憑證檔與資料庫不一致仍啟動。
		if count != len(seedAccounts) {
			return errors.New("unexpected demo account count")
		}
		for _, account := range seedAccounts {
			var hash string
			err := tx.QueryRowContext(ctx, `
				SELECT password_hash FROM accounts
				WHERE username = $1 AND role = $2 AND site_id IS NOT DISTINCT FROM $3
			`, account.username, account.role, account.siteID).Scan(&hash)
			if err != nil {
				return operationFailure("inspect existing demo account", err)
			}
			if !matchesPassword(hash, passwords[account.username]) {
				return errors.New("protected demo credentials do not match the database")
			}
		}
	}
	// 停機期間到期的額度，在接受新請求前清除。
	if _, err := tx.ExecContext(ctx, pruneLoginAttemptsSQL); err != nil {
		return operationFailure("prune expired login attempts", err)
	}
	return operationFailure("commit seed transaction", tx.Commit())
}

// readPrivateFile 在受限的根目錄內讀取小型私密設定，只接受權限為 0600 的一般檔案。
func readPrivateFile(state *os.Root, name string) ([]byte, error) {
	// 先檢查路徑本身的檔案類型與權限，避免接受符號連結。
	info, err := state.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode() != 0o600 {
		return nil, errors.New("expected a regular file with mode 0600")
	}
	file, err := state.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// 再核對實際開啟的檔案，檢查兩次操作之間是否被替換。
	current, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if current.Mode() != 0o600 || !os.SameFile(info, current) {
		return nil, errors.New("protected file changed before opening")
	}
	// 多讀一個位元組以區分恰好到上限與超過上限。
	const maxSize = 4096
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err == nil && len(data) > maxSize {
		return nil, errors.New("protected file exceeds size limit")
	}
	return data, err
}

// demoPasswords 載入受保護的展示密碼。只有初始化且檔案缺失才產生新密碼，讓中斷後可重用原憑證。
func demoPasswords(state *os.Root, initialize bool) (map[string]string, error) {
	data, err := readPrivateFile(state, demoPasswordsName)
	var passwords map[string]string
	// 首次初始化為每個帳號產生獨立的隨機密碼。
	if errors.Is(err, os.ErrNotExist) && initialize {
		passwords = make(map[string]string, len(seedAccounts))
		for _, account := range seedAccounts {
			passwords[account.username] = randomSecret()
		}
		if err := writeDemoPasswords(state, passwords); err != nil {
			return nil, err
		}
	} else {
		if err != nil {
			return nil, err
		}
		// 核對保存的密碼涵蓋預載帳號且各不相同，避免以不完整設定繼續。
		if err := json.Unmarshal(data, &passwords); err != nil {
			return nil, err
		}
		if len(passwords) != len(seedAccounts) {
			return nil, errors.New("unexpected credential count")
		}
		seen := make(map[string]bool, len(seedAccounts))
		for _, account := range seedAccounts {
			password := passwords[account.username]
			if !validSecret(password) || seen[password] {
				return nil, errors.New("invalid or duplicate demo credentials")
			}
			seen[password] = true
		}
	}

	if initialize {
		// 先同步目錄再提交資料庫交易。中斷後可以重用已發布的密碼。
		directory, err := state.Open(".")
		if err != nil {
			return nil, err
		}
		syncErr := directory.Sync()
		if err := errors.Join(syncErr, directory.Close()); err != nil {
			return nil, err
		}
	}
	return passwords, nil
}

// writeDemoPasswords 先寫妥暫存檔再以同目錄的硬連結發布，避免讀到半成品或覆蓋既有憑證。
func writeDemoPasswords(state *os.Root, passwords map[string]string) (err error) {
	// 以獨占方式建立 0600 暫存檔，避免碰撞時改寫其他檔案。
	name := ".demo-passwords-" + randomSecret()
	file, err := state.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// 成功或失敗都清除暫存名稱，並保留清理錯誤。
	defer func() {
		err = errors.Join(err, state.Remove(name))
	}()
	// 先寫入並同步完整內容，再發布正式檔名。
	encodeErr := json.NewEncoder(file).Encode(passwords)
	if encodeErr != nil {
		return errors.Join(encodeErr, file.Close())
	}
	syncErr := file.Sync()
	if err := errors.Join(syncErr, file.Close()); err != nil {
		return err
	}
	// Link 在同一目錄原子發布完整檔案，既有密碼永遠不會被覆蓋。
	return state.Link(name, demoPasswordsName)
}

// 啟動錯誤公開操作名稱，保留原始原因供程式辨識，避免輸出連線或憑證內容。
type startupError struct {
	operation string
	cause     error
}

// Error 只公開啟動操作名稱，避免把底層的連線資訊或憑證寫入日誌。
func (e startupError) Error() string { return fmt.Sprintf("IoT startup: %s", e.operation) }

// Unwrap 保留底層錯誤供呼叫端用 errors.Is 或 errors.As 判斷。
func (e startupError) Unwrap() error { return e.cause }

// operationFailure 只包裝失敗操作，讓呼叫端能直接傳回提交等操作的結果。
func operationFailure(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return startupError{operation: operation, cause: cause}
}
