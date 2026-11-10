package main

import (
	"context"
	"database/sql"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

var (
	errClosureUnauthenticated = errors.New("alert closure session is missing or expired")
	errClosureConflict        = errors.New("alert cannot be closed in its current state")
)

// parseClosureResult 只接受結案結果，檢查純文字的 Unicode 長度與換行，保留原字串。
func parseClosureResult(body []byte) (string, bool) {
	var input struct {
		Result string `json:"result"`
	}
	// 嚴格解碼拒絕重複欄位、非法 UTF-8 與未配對的 UTF-16 跳脫，不將其替換成 U+FFFD。
	if err := jsonv2.Unmarshal(body, &input, jsonv2.RejectUnknownMembers(true)); err != nil {
		return "", false
	}
	result := input.Result
	if strings.TrimSpace(result) == "" || utf8.RuneCountInString(result) > 2000 {
		return "", false
	}
	// 換行可用 LF 或 CRLF；單獨的 CR 及其他控制字元不可保存。
	for offset, char := range result {
		if char == '\n' ||
			(char == '\r' &&
				offset+1 < len(result) &&
				result[offset+1] == '\n') {
			continue
		}
		if unicode.IsControl(char) {
			return "", false
		}
	}
	return result, true
}

// closeAlert 在同一交易驗證目前身分、鎖定可見告警並新增唯讀處理紀錄。
func (a *application) closeAlert(ctx context.Context, hash []byte, id int64, result string) (item alertItem, err error) {
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin alert closure: %w", err)
		}
		defer tx.Rollback()

		// 交易持有 session 與帳號讀鎖，提交前不能撤銷身分或變更場域。
		var actor identity
		err = tx.QueryRowContext(ctx, sessionIdentitySQL+" FOR SHARE OF s, a", hash).
			Scan(&actor.Username, &actor.Role, &actor.SiteID)
		if errors.Is(err, sql.ErrNoRows) {
			return errClosureUnauthenticated
		}
		if err != nil {
			return fmt.Errorf("read alert closure session: %w", err)
		}

		// 只鎖該筆告警。設備重新異常時，舊告警仍依自身的恢復時間判定。
		item, err = scanAlert(tx.QueryRowContext(ctx, alertColumns+alertSource+" AND a.id = $2 FOR UPDATE OF a", actor.SiteID, id))
		if err != nil {
			return fmt.Errorf("lock scoped alert: %w", err)
		}
		if item.Status != "recovered" {
			return errClosureConflict
		}

		// 一筆告警只允許新增一次結果，即使有其他交易已先插入，也不能覆蓋。
		closedAt := a.measurementClock().UTC()
		var site string
		err = tx.QueryRowContext(ctx, `
			INSERT INTO alert_closures (alert_id, actor, closed_at_ns, result)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (alert_id) DO NOTHING
			RETURNING (SELECT site_id FROM devices WHERE id = $5)
		`, id, actor.Username, closedAt.UnixNano(), result, item.DeviceID).Scan(&site)
		if errors.Is(err, sql.ErrNoRows) {
			return errClosureConflict
		}
		if err != nil {
			return fmt.Errorf("insert alert closure: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit alert closure: %w", err)
		}
		// 只有提交成功才通知設備所屬場域，並在回應中帶出結案內容。
		item.Status = "closed"
		item.Closure = &alertClosure{actor.Username, closedAt, result}
		a.events.publish(site)
		return nil
	})
	return item, err
}

// alertClose 驗證結案請求，並映射交易結果及共用的 API 錯誤。
func (a *application) alertClose(c *gin.Context) {
	if !noQuery(c) {
		return
	}
	body, ok := readJSONBody(c)
	if !ok {
		return
	}
	result, ok := parseClosureResult(body)
	if !ok {
		reject(c, http.StatusBadRequest, "invalid_input")
		return
	}

	// 從 Cookie 取得 session 雜湊，角色與場域由交易內的查詢決定。
	hash := sessionHash(c.Request)
	if hash == nil {
		reject(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	item, err := a.closeAlert(c.Request.Context(), hash, parseAlertID(c.Param("id")), result)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, item)
	case errors.Is(err, errClosureUnauthenticated):
		reject(c, http.StatusUnauthorized, "unauthenticated")
	case errors.Is(err, sql.ErrNoRows):
		reject(c, http.StatusNotFound, "not_found")
	case errors.Is(err, errClosureConflict):
		reject(c, http.StatusConflict, "alert_conflict")
	default:
		unavailable(c)
	}
}
