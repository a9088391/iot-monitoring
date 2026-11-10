package main

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

type alertItem struct {
	ID                string        `json:"id"`
	DeviceID          string        `json:"deviceId"`
	Type              string        `json:"type"`
	Status            string        `json:"status"`
	Temperature       *float64      `json:"temperature,omitempty"`
	DetectedAt        time.Time     `json:"detectedAt"`
	RecoveredAt       *time.Time    `json:"recoveredAt"`
	DeadlineAt        *time.Time    `json:"deadlineAt,omitempty"`
	DetectedAtRestart *bool         `json:"detectedAtRestart,omitempty"`
	Closure           *alertClosure `json:"closure,omitempty"`
}

type alertClosure struct {
	Actor    string    `json:"actor"`
	ClosedAt time.Time `json:"closedAt"`
	Result   string    `json:"result"`
}

type alertPage struct {
	Items  []alertItem `json:"items"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

type rowScanner interface {
	Scan(...any) error
}

const alertSource = `
	FROM alerts a JOIN devices d ON d.id = a.device_id
	LEFT JOIN alert_closures c ON c.alert_id = a.id
	WHERE ($1::text IS NULL OR d.site_id = $1)
`

const alertColumns = `
	SELECT a.id, a.device_id, a.type, a.temperature, a.detected_at_ns, a.recovered_at_ns,
		a.deadline_at_ns, a.detected_at_restart, c.actor, c.closed_at_ns, c.result
`

// parseAlertID 沿用告警明細的正整數 ID 格式，無效時以不存在的 ID 表示。
func parseAlertID(raw string) int64 {
	id, err := strconv.ParseUint(raw, 10, 63)
	if err != nil {
		return 0
	}
	return int64(id)
}

// scanAlert 保留告警的原始證據，依恢復與結案紀錄決定狀態。
func scanAlert(row rowScanner) (alertItem, error) {
	var item alertItem
	var id, detected int64
	var temperature sql.NullFloat64
	var recovered, deadline, closedAt sql.NullInt64
	var restart bool
	var actor, result sql.NullString
	if err := row.Scan(&id, &item.DeviceID, &item.Type, &temperature, &detected, &recovered,
		&deadline, &restart, &actor, &closedAt, &result); err != nil {
		return alertItem{}, err
	}
	// 以字串保留整數 ID 的完整精度，時間統一轉為 UTC。
	item.ID = strconv.FormatInt(id, 10)
	item.DetectedAt = time.Unix(0, detected).UTC()
	item.Status = "active"
	// 依欄位是否存在帶出證據，避免把無溫度或無期限的告警填成零值。
	if temperature.Valid {
		item.Temperature = &temperature.Float64
	}
	if deadline.Valid {
		item.DeadlineAt = new(time.Unix(0, deadline.Int64).UTC())
		item.DetectedAtRestart = &restart
	}
	if recovered.Valid {
		item.RecoveredAt = new(time.Unix(0, recovered.Int64).UTC())
		item.Status = "recovered"
	}
	if closedAt.Valid {
		item.Status = "closed"
		item.Closure = &alertClosure{actor.String, time.Unix(0, closedAt.Int64).UTC(), result.String}
	}
	return item, nil
}

// listAlerts 以同一場域篩選計數與清單，並以告警 ID 倒序提供穩定分頁。
func (a *application) listAlerts(ctx context.Context, siteID *string, limit, offset int) (alertPage, error) {
	result := alertPage{Items: make([]alertItem, 0, limit), Limit: limit, Offset: offset}
	err := a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		// 先以和分頁相同的範圍計算總數，再讀取指定頁。
		if err := conn.QueryRowContext(ctx, "SELECT count(*) "+alertSource, siteID).Scan(&result.Total); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, alertColumns+alertSource+`
			ORDER BY a.id DESC LIMIT $2 OFFSET $3`, siteID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		// 逐筆轉換並回傳迭代錯誤，避免把不完整的頁面當成成功。
		for rows.Next() {
			item, err := scanAlert(rows)
			if err != nil {
				return err
			}
			result.Items = append(result.Items, item)
		}
		return rows.Err()
	})
	return result, err
}

// findAlert 以識別碼和場域一起查詢，使跨場域的告警與不存在的告警同樣查無資料。
func (a *application) findAlert(ctx context.Context, siteID *string, id int64) (item alertItem, err error) {
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		item, err = scanAlert(conn.QueryRowContext(ctx, alertColumns+alertSource+" AND a.id = $2", siteID, id))
		return err
	})
	return item, err
}
