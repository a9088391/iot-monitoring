package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	SimulatorMeasurementSource = "simulator"
	measurementMaxAge          = 5 * time.Second
)

type Measurement struct {
	Source, DeviceID                string
	DataGeneration, RunID, Sequence int64
	GeneratedAt                     time.Time
	Temperature                     float64
}

type measurementItem struct {
	Temperature float64   `json:"temperature"`
	Sequence    int64     `json:"sequence"`
	GeneratedAt time.Time `json:"generatedAt"`
	ReceivedAt  time.Time `json:"receivedAt"`
}

// freshMeasurement 拒絕未來及超過五秒的測量，讓恰好五秒的資料仍可採用。
func freshMeasurement(generated, now time.Time) bool {
	return !generated.After(now) && now.Sub(generated) <= measurementMaxAge
}

// validDeviceID 檢查設備識別碼的長度與文字編碼，拒絕不能傳給 PostgreSQL 的 NUL。
func validDeviceID(id string) bool {
	return len(id) > 0 &&
		len(id) <= 64 &&
		utf8.ValidString(id) &&
		!strings.ContainsRune(id, 0)
}

// measurementClock 讓抵達、採用及離線使用同一時計，測試可控制邊界而不用等真實時間。
func (a *application) measurementClock() time.Time {
	if a.measurementNow != nil {
		return a.measurementNow()
	}
	return time.Now()
}

// SubmitMeasurement 在入列前記錄抵達時間，只有最新測量與相關告警完整提交才回傳採用成功。
func (a *application) SubmitMeasurement(ctx context.Context, sample Measurement) (bool, error) {
	adopted, _, err := a.submitMeasurement(ctx, sample)
	return adopted, err
}

// submitMeasurement 另外回傳入口取得的抵達時間，讓隔離量測與資料庫使用同一時間基準。
func (a *application) submitMeasurement(ctx context.Context, sample Measurement) (adopted bool, received time.Time, err error) {
	// 先排除來源、欄位與時間表示無效的資料，避免占用設備佇列。
	generated := sample.GeneratedAt.UTC()
	generatedNS := generated.UnixNano()
	if sample.Source != SimulatorMeasurementSource ||
		!validDeviceID(sample.DeviceID) ||
		sample.DataGeneration <= 0 ||
		sample.RunID <= 0 ||
		sample.Sequence < 0 ||
		math.IsNaN(sample.Temperature) ||
		math.IsInf(sample.Temperature, 0) ||
		!time.Unix(0, generatedNS).Equal(generated) {
		return false, time.Time{}, nil
	}
	// 記錄抵達時間並等同設備的前一筆完成。逾時離開也會釋放這筆排隊紀錄。
	report, received := a.receiveMeasurement(sample.DeviceID, generated)
	if report == nil {
		return false, received, nil
	}
	defer a.arrivals.release(sample.DeviceID, report)
	ctx, cancel := context.WithTimeout(ctx, dbOperationTimeout)
	defer cancel()
	select {
	case <-report.ready:
	case <-ctx.Done():
		return false, received, ctx.Err()
	}

	// 在持鎖連線內確認採用資格並寫入，任一步失敗則回滾。
	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin measurement transaction: %w", err)
		}
		defer tx.Rollback()

		// 依序鎖定該設備的執行、最新測量及未恢復告警，等待結束後才判斷採用期限。
		var site string
		err = tx.QueryRowContext(ctx, `
			SELECT d.site_id FROM measurement_runs r JOIN devices d ON d.id = r.device_id
			WHERE r.device_id = $1 AND r.data_generation = $2 AND r.run_id = $3 FOR UPDATE OF r
		`, sample.DeviceID, sample.DataGeneration, sample.RunID).Scan(&site)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock allowed measurement run: %w", err)
		}

		// 核對順序與產生時間。離線基準優先用上次抵達時間，無有效回報時才用首筆期待。
		var lastSequence, lastGenerated int64
		var baseline sql.NullInt64
		err = tx.QueryRowContext(ctx, `
			SELECT sequence, generated_at_ns, received_at_ns FROM latest_measurements WHERE device_id = $1 FOR UPDATE
		`, sample.DeviceID).Scan(&lastSequence, &lastGenerated, &baseline)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock latest measurement: %w", err)
		}
		if err == nil &&
			(sample.Sequence <= lastSequence ||
				generatedNS < lastGenerated) {
			return nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.QueryRowContext(ctx, `
				SELECT (SELECT first_expected_at_ns FROM device_simulations WHERE device_id = $1)
			`, sample.DeviceID).Scan(&baseline); err != nil {
				return fmt.Errorf("read first expected measurement: %w", err)
			}
		}

		// 等告警列鎖到手後重查新鮮度，避免採用在等待期間過期的回報。
		temperatureAlert, offlineAlert, err := lockMeasurementAlerts(ctx, tx, sample.DeviceID)
		if err != nil {
			return err
		}
		now := a.measurementClock().UTC()
		nowNS := now.UnixNano()
		if !freshMeasurement(generated, now) || !time.Unix(0, nowNS).Equal(now) {
			return nil
		}

		// 最新溫度與其告警一起提交。告警寫入失敗時，測量可原樣重試。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO latest_measurements (device_id, temperature, sequence, generated_at_ns, received_at_ns)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (device_id) DO UPDATE SET
				temperature = excluded.temperature, sequence = excluded.sequence,
				generated_at_ns = excluded.generated_at_ns, received_at_ns = excluded.received_at_ns
		`, sample.DeviceID, sample.Temperature, sample.Sequence, generatedNS, received.UnixNano()); err != nil {
			return fmt.Errorf("write latest measurement: %w", err)
		}
		// 正常上下限皆包含。只有首次超標或恢復才寫溫度告警，持續超標沿用原告警。
		abnormal := sample.Temperature < 18 || sample.Temperature > 28
		var alertErr error
		switch {
		case abnormal && temperatureAlert == 0:
			_, alertErr = tx.ExecContext(ctx, `
				INSERT INTO alerts (device_id, type, temperature, detected_at_ns)
				VALUES ($1, 'temperature', $2, $3)
			`, sample.DeviceID, sample.Temperature, nowNS)
		case !abnormal && temperatureAlert != 0:
			// 系統時鐘往回調整時，恢復時間仍不能早於同筆告警的發現時間。
			_, alertErr = tx.ExecContext(ctx, `
				UPDATE alerts SET recovered_at_ns = GREATEST(detected_at_ns, $2) WHERE id = $1
			`, temperatureAlert, nowNS)
		}
		if alertErr != nil {
			return fmt.Errorf("write temperature alert: %w", alertErr)
		}
		// 獨立判斷離線恢復，再與最新測量及溫度告警一起提交。
		if err := recoverOffline(ctx, tx, sample.DeviceID, offlineAlert, baseline, received, now); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit measurement transaction: %w", err)
		}
		a.events.publish(site)
		adopted = true
		return nil
	})
	return adopted, received, err
}

// lockMeasurementAlerts 以固定順序鎖定兩類未恢復告警，讓採用交易能分別更新溫度和離線狀態。
func lockMeasurementAlerts(ctx context.Context, tx *sql.Tx, deviceID string) (temperature, offline int64, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, type FROM alerts
		WHERE device_id = $1 AND type IN ('temperature', 'offline') AND recovered_at_ns IS NULL
		ORDER BY type FOR UPDATE
	`, deviceID)
	if err != nil {
		return 0, 0, fmt.Errorf("lock active measurement alerts: %w", err)
	}
	defer rows.Close()
	// 按類型回傳未恢復告警的 ID，零值表示目前無該類告警。
	for rows.Next() {
		var id int64
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return 0, 0, fmt.Errorf("read active measurement alert: %w", err)
		}
		if kind == "temperature" {
			temperature = id
		} else {
			offline = id
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("read active measurement alerts: %w", err)
	}
	return temperature, offline, nil
}
