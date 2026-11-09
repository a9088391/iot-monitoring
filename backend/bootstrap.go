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

func bootstrap(ctx context.Context, conn *sql.Conn, state *os.Root) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return operationFailure("begin seed transaction", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, createSchema+createMeasurementSchema+createSimulationSchema); err != nil {
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

func readPrivateFile(state *os.Root, name string) ([]byte, error) {
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
	current, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if current.Mode() != 0o600 || !os.SameFile(info, current) {
		return nil, errors.New("protected file changed before opening")
	}
	const maxSize = 4096
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err == nil && len(data) > maxSize {
		return nil, errors.New("protected file exceeds size limit")
	}
	return data, err
}

func demoPasswords(state *os.Root, initialize bool) (map[string]string, error) {
	data, err := readPrivateFile(state, demoPasswordsName)
	var passwords map[string]string
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

func writeDemoPasswords(state *os.Root, passwords map[string]string) (err error) {
	name := ".demo-passwords-" + randomSecret()
	file, err := state.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, state.Remove(name))
	}()
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

func (e startupError) Error() string { return fmt.Sprintf("IoT startup: %s", e.operation) }
func (e startupError) Unwrap() error { return e.cause }

func operationFailure(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return startupError{operation: operation, cause: cause}
}
