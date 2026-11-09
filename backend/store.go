package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

type identity struct {
	Username string  `json:"username"`
	Role     string  `json:"role"`
	SiteID   *string `json:"siteId"`
}

type deviceItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SiteID   string `json:"siteId"`
	SiteName string `json:"siteName"`
}

type devicePage struct {
	Items  []deviceItem `json:"items"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

const dbOperationTimeout = 4 * time.Second

// 所有 API 查詢使用原啟動鎖的連線，失去連線時不會換一條無鎖連線重試。
type lockedDB struct {
	conn *sql.Conn
	gate chan struct{}
}

func (db *lockedDB) use(ctx context.Context, query func(context.Context, *sql.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, dbOperationTimeout)
	defer cancel()
	select {
	case db.gate <- struct{}{}:
		defer func() { <-db.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return query(ctx, db.conn)
}

func (a *application) ping(ctx context.Context) error {
	return a.db.use(ctx, probeStartupConnection)
}

func probeStartupConnection(ctx context.Context, conn *sql.Conn) error {
	var probe int
	return conn.QueryRowContext(ctx, "SELECT 1").Scan(&probe)
}

func (a *application) watchdogProbe(ctx context.Context) (probed bool, err error) {
	err = a.db.use(ctx, func(wait context.Context, conn *sql.Conn) error {
		// 取得連線與排隊逾時可能同時發生，只有開始查詢才可判斷持鎖連線是否失效。
		if err := wait.Err(); err != nil {
			return err
		}
		probed = true
		probe, cancel := context.WithTimeout(ctx, dbOperationTimeout)
		defer cancel()
		return probeStartupConnection(probe, conn)
	})
	return probed, err
}

const (
	loginWindowSeconds    = 60
	maxLoginKeys          = 4096
	pruneLoginAttemptsSQL = "DELETE FROM login_attempts WHERE expires_at <= statement_timestamp()"
)

// 閒置時也要清除逾期紀錄，避免保留已失效的登入識別雜湊。
func (a *application) pruneLoginAttempts(ctx context.Context) error {
	return a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, pruneLoginAttemptsSQL)
		return err
	})
}

const reserveLoginAttemptSQL = `
	WITH quota AS (
		SELECT CASE
			WHEN count(*) >= $3::integer
				THEN ceil(extract(epoch FROM min(expires_at) - transaction_timestamp()))::integer
			WHEN count(*) = 0 AND (
				SELECT count(DISTINCT key_hash)
				FROM login_attempts
				WHERE scope = $1 AND expires_at > transaction_timestamp()
			) >= $4::integer THEN $5::integer
			ELSE 0
		END AS retry_after
		FROM login_attempts
		WHERE scope = $1 AND key_hash = $2 AND expires_at > transaction_timestamp()
	), reserved AS (
		INSERT INTO login_attempts (scope, key_hash, expires_at)
		SELECT $1, $2, transaction_timestamp() + $5::integer * interval '1 second'
		FROM quota WHERE retry_after = 0
	)
	SELECT retry_after FROM quota
`

// 以持鎖連線序列化配額檢查與寫入，避免併發請求取得同一個額度。
func (a *application) reserveLoginAttempts(ctx context.Context, username string, peer netip.Addr) (retry int, err error) {
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin login quota transaction: %w", err)
		}
		defer tx.Rollback()

		// 清理、額度判斷和新增使用同一交易時間。到期的瞬間即可再次嘗試。
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM login_attempts WHERE expires_at <= transaction_timestamp()
		`); err != nil {
			return fmt.Errorf("prune expired login attempts: %w", err)
		}
		// 來源已滿即停止。帳號已滿仍提交這次來源額度，資料庫錯誤則回滾。
		for _, quota := range []struct {
			scope string
			key   [sha256.Size]byte
			limit int
		}{
			{"source", sha256.Sum256(peer.AsSlice()), 30},
			{"account", sha256.Sum256([]byte(username)), 5},
		} {
			err := tx.QueryRowContext(ctx, reserveLoginAttemptSQL, quota.scope, quota.key[:],
				quota.limit, maxLoginKeys, loginWindowSeconds).Scan(&retry)
			if err != nil {
				return fmt.Errorf("reserve %s login quota: %w", quota.scope, err)
			}
			if retry > 0 {
				break
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit login quota transaction: %w", err)
		}
		return nil
	})
	return retry, err
}

func (a *application) account(ctx context.Context, username string) (user identity, hash string, err error) {
	// PostgreSQL 的文字不能包含 NUL，這種名稱仍走同一筆未知帳號查詢。
	if strings.ContainsRune(username, 0) {
		username = ""
	}
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `
			SELECT username, role, site_id, password_hash FROM accounts WHERE username = $1
		`, username).Scan(&user.Username, &user.Role, &user.SiteID, &hash)
	})
	return user, hash, err
}

func (a *application) rotateSession(ctx context.Context, username string, previous []byte) (string, error) {
	token := randomSecret()
	hash := sha256.Sum256([]byte(token))
	err := a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		// 撤銷舊 session 和建立新 session 在同一筆交易內生效。
		_, err := conn.ExecContext(ctx, `
			WITH removed AS (
				DELETE FROM sessions WHERE token_hash = $1 OR expires_at <= statement_timestamp()
			)
			INSERT INTO sessions (token_hash, username, expires_at)
			VALUES ($2, $3, statement_timestamp() + $4::integer * interval '1 second')
		`, previous, hash[:], username, int(sessionLifetime/time.Second))
		return err
	})
	return token, err
}

func (a *application) revokeSession(ctx context.Context, hash []byte) error {
	return a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = $1", hash)
		return err
	})
}

func (a *application) sessionIdentity(ctx context.Context, hash []byte) (user identity, err error) {
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `
			SELECT a.username, a.role, a.site_id
			FROM sessions s JOIN accounts a ON a.username = s.username
			WHERE s.token_hash = $1 AND s.expires_at > statement_timestamp()
		`, hash).Scan(&user.Username, &user.Role, &user.SiteID)
	})
	return user, err
}

const deviceSource = `
	FROM devices d JOIN sites s ON s.id = d.site_id
	WHERE ($1::text IS NULL OR d.site_id = $1)
`

func (a *application) listDevices(ctx context.Context, siteID *string, limit, offset int) (devicePage, error) {
	result := devicePage{Items: make([]deviceItem, 0, limit), Limit: limit, Offset: offset}
	err := a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, "SELECT count(*) "+deviceSource, siteID).Scan(&result.Total); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, `
			SELECT d.id, d.name, d.site_id, s.name
		`+deviceSource+` ORDER BY d.id LIMIT $2 OFFSET $3`, siteID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item deviceItem
			if err := rows.Scan(&item.ID, &item.Name, &item.SiteID, &item.SiteName); err != nil {
				return err
			}
			result.Items = append(result.Items, item)
		}
		return rows.Err()
	})
	return result, err
}

func (a *application) findDevice(ctx context.Context, siteID *string, id string) (item deviceItem, err error) {
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `
			SELECT d.id, d.name, d.site_id, s.name
		`+deviceSource+` AND d.id = $2`, siteID, id).Scan(&item.ID, &item.Name, &item.SiteID, &item.SiteName)
	})
	return item, err
}
