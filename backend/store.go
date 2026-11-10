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
	ID                string           `json:"id"`
	Name              string           `json:"name"`
	SiteID            string           `json:"siteId"`
	SiteName          string           `json:"siteName"`
	LatestMeasurement *measurementItem `json:"latestMeasurement,omitempty"`
	OnlineStatus      string           `json:"onlineStatus"`
}

type devicePage struct {
	Items  []deviceItem `json:"items"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

const dbOperationTimeout = 4 * time.Second

// API 與測量操作使用原啟動鎖的連線，失去連線時不會換一條無鎖連線重試。
type lockedDB struct {
	conn *sql.Conn
	gate chan struct{}
}

// use 序列化同一條持鎖連線的操作，並對等候和查詢共用一個期限。
func (db *lockedDB) use(ctx context.Context, query func(context.Context, *sql.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, dbOperationTimeout)
	defer cancel()
	// 等到取得連線才執行，離開時歸還槽位讓下一項操作接手。
	select {
	case db.gate <- struct{}{}:
		defer func() { <-db.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return query(ctx, db.conn)
}

// ping 讓健康檢查使用實際服務的連線，反映持鎖連線能否查詢。
func (a *application) ping(ctx context.Context) error {
	return a.db.use(ctx, probeStartupConnection)
}

// probeStartupConnection 以不改動資料的最小查詢確認持鎖連線可用。
func probeStartupConnection(ctx context.Context, conn *sql.Conn) error {
	var probe int
	return conn.QueryRowContext(ctx, "SELECT 1").Scan(&probe)
}

// watchdogProbe 區分尚未取得連線與已開始探測，避免把繁忙誤判為持鎖連線失效。
func (a *application) watchdogProbe(ctx context.Context) (probed bool, err error) {
	err = a.db.use(ctx, func(wait context.Context, conn *sql.Conn) error {
		// 取得連線與排隊逾時可能同時發生，只有開始查詢才可判斷持鎖連線是否失效。
		if err := wait.Err(); err != nil {
			return err
		}
		probed = true
		// 已取得連線後使用獨立的探測期限，避免只剩下排隊期限的尾端。
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

// pruneLoginAttempts 在閒置時也清除逾期紀錄，避免保留已失效的登入識別雜湊。
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

// reserveLoginAttempts 以持鎖連線序列化配額檢查與寫入，避免併發請求取得同一個額度。
func (a *application) reserveLoginAttempts(ctx context.Context, username string, peer netip.Addr) (retry int, err error) {
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		// 以交易封裝清理及兩種配額。任一步資料庫操作失敗就回滾。
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
		// 即使達到某一限制，也要保存這次已成功保留的額度。
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit login quota transaction: %w", err)
		}
		return nil
	})
	return retry, err
}

// account 讀取資料庫中可信的帳號身分與密碼雜湊，供登入驗證使用。
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

// rotateSession 換發隨機 token，讓舊 session 撤銷與新 session 建立原子生效。
func (a *application) rotateSession(ctx context.Context, username string, previous []byte) (string, error) {
	// 產生新 token，只以其雜湊及固定期限寫入資料庫。
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

// revokeSession 移除指定 token 的雜湊，讓登出立即作用於後端的查詢。
func (a *application) revokeSession(ctx context.Context, hash []byte) error {
	return a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = $1", hash)
		return err
	})
}

const sessionIdentitySQL = `
	SELECT a.username, a.role, a.site_id
	FROM sessions s JOIN accounts a ON a.username = s.username
	WHERE s.token_hash = $1 AND s.expires_at > statement_timestamp()
`

// sessionIdentity 只從未達絕對期限的 session 取得帳號角色與場域。
func (a *application) sessionIdentity(ctx context.Context, hash []byte) (user identity, err error) {
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, sessionIdentitySQL, hash).Scan(&user.Username, &user.Role, &user.SiteID)
	})
	return user, err
}

const deviceSource = `
	FROM devices d JOIN sites s ON s.id = d.site_id
	LEFT JOIN latest_measurements m ON m.device_id = d.id
	LEFT JOIN device_simulations sim ON sim.device_id = d.id
	LEFT JOIN alerts offline ON offline.device_id = d.id AND offline.type = 'offline' AND offline.recovered_at_ns IS NULL
	WHERE ($1::text IS NULL OR d.site_id = $1)
`

const deviceColumns = `
	SELECT d.id, d.name, d.site_id, s.name, m.temperature, m.sequence, m.generated_at_ns, m.received_at_ns,
		CASE
			WHEN offline.id IS NOT NULL THEN 'offline'
			WHEN m.device_id IS NOT NULL THEN 'online'
			WHEN sim.first_expected_at_ns IS NOT NULL THEN 'awaiting'
			ELSE 'unreported'
		END
`

// scanDevice 將設備與選擇性測量資料轉成 API 結果，保留資料庫依告警和回報狀態得出的在線狀態。
func scanDevice(row rowScanner) (deviceItem, error) {
	var item deviceItem
	var temperature sql.NullFloat64
	var sequence, generated, received sql.NullInt64
	if err := row.Scan(&item.ID, &item.Name, &item.SiteID, &item.SiteName,
		&temperature, &sequence, &generated, &received, &item.OnlineStatus); err != nil {
		return deviceItem{}, err
	}
	// 只有曾採用測量才回傳最後溫度和時間，離線時也保留該筆測量。
	if generated.Valid {
		item.LatestMeasurement = &measurementItem{
			Temperature: temperature.Float64, Sequence: sequence.Int64,
			GeneratedAt: time.Unix(0, generated.Int64).UTC(),
			ReceivedAt:  time.Unix(0, received.Int64).UTC(),
		}
	}
	return item, nil
}

// listDevices 以相同場域範圍計數與分頁，並以設備 ID 排序讓分頁穩定。
func (a *application) listDevices(ctx context.Context, siteID *string, limit, offset int) (devicePage, error) {
	result := devicePage{Items: make([]deviceItem, 0, limit), Limit: limit, Offset: offset}
	err := a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		// 先計算使用者可見的總數，再依相同篩選讀取指定頁。
		if err := conn.QueryRowContext(ctx, "SELECT count(*) "+deviceSource, siteID).Scan(&result.Total); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, deviceColumns+deviceSource+` ORDER BY d.id LIMIT $2 OFFSET $3`, siteID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		// 逐筆轉換並回傳迭代錯誤，避免把不完整的頁面當成成功。
		for rows.Next() {
			item, err := scanDevice(rows)
			if err != nil {
				return err
			}
			result.Items = append(result.Items, item)
		}
		return rows.Err()
	})
	return result, err
}

// findDevice 以場域和設備識別碼一起查詢，使跨場域的設備與不存在的設備同樣查無資料。
func (a *application) findDevice(ctx context.Context, siteID *string, id string) (item deviceItem, err error) {
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		item, err = scanDevice(conn.QueryRowContext(ctx, deviceColumns+deviceSource+` AND d.id = $2`, siteID, id))
		return err
	})
	return item, err
}
