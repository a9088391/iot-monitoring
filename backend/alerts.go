package main

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

type alertItem struct {
	ID                string     `json:"id"`
	DeviceID          string     `json:"deviceId"`
	Type              string     `json:"type"`
	Status            string     `json:"status"`
	Temperature       *float64   `json:"temperature,omitempty"`
	DetectedAt        time.Time  `json:"detectedAt"`
	RecoveredAt       *time.Time `json:"recoveredAt"`
	DeadlineAt        *time.Time `json:"deadlineAt,omitempty"`
	DetectedAtRestart *bool      `json:"detectedAtRestart,omitempty"`
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
	WHERE ($1::text IS NULL OR d.site_id = $1)
`

const alertColumns = `
	SELECT a.id, a.device_id, a.type, a.temperature, a.detected_at_ns, a.recovered_at_ns,
		a.deadline_at_ns, a.detected_at_restart
`

// scanAlert 將資料列轉成 API 告警。只有離線告警有逾時證據，恢復時間則決定告警狀態。
func scanAlert(row rowScanner) (alertItem, error) {
	var item alertItem
	var id, detected int64
	var temperature sql.NullFloat64
	var recovered, deadline sql.NullInt64
	var restart bool
	if err := row.Scan(&id, &item.DeviceID, &item.Type, &temperature, &detected, &recovered, &deadline, &restart); err != nil {
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
