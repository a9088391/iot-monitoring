package main

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

type alertItem struct {
	ID          string     `json:"id"`
	DeviceID    string     `json:"deviceId"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	Temperature float64    `json:"temperature"`
	DetectedAt  time.Time  `json:"detectedAt"`
	RecoveredAt *time.Time `json:"recoveredAt"`
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

const alertColumns = "SELECT a.id, a.device_id, a.type, a.temperature, a.detected_at_ns, a.recovered_at_ns "

func scanAlert(row rowScanner) (alertItem, error) {
	var item alertItem
	var id, detected int64
	var recovered sql.NullInt64
	if err := row.Scan(&id, &item.DeviceID, &item.Type, &item.Temperature, &detected, &recovered); err != nil {
		return alertItem{}, err
	}
	item.ID = strconv.FormatInt(id, 10)
	item.DetectedAt = time.Unix(0, detected).UTC()
	item.Status = "active"
	if recovered.Valid {
		item.RecoveredAt = new(time.Unix(0, recovered.Int64).UTC())
		item.Status = "recovered"
	}
	return item, nil
}

func (a *application) listAlerts(ctx context.Context, siteID *string, limit, offset int) (alertPage, error) {
	result := alertPage{Items: make([]alertItem, 0, limit), Limit: limit, Offset: offset}
	err := a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, "SELECT count(*) "+alertSource, siteID).Scan(&result.Total); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, alertColumns+alertSource+`
			ORDER BY a.id DESC LIMIT $2 OFFSET $3`, siteID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
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

func (a *application) findAlert(ctx context.Context, siteID *string, id int64) (item alertItem, err error) {
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		item, err = scanAlert(conn.QueryRowContext(ctx, alertColumns+alertSource+" AND a.id = $2", siteID, id))
		return err
	})
	return item, err
}
