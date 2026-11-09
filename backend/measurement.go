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

func freshMeasurement(generated, now time.Time) bool {
	return !generated.After(now) && now.Sub(generated) <= measurementMaxAge
}

func validDeviceID(id string) bool {
	return len(id) > 0 && len(id) <= 64 && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}

func (a *application) measurementClock() time.Time {
	if a.measurementNow != nil {
		return a.measurementNow()
	}
	return time.Now()
}

// SubmitMeasurement 在入列前記錄抵達時間，只有完整提交才回傳採用成功。
func (a *application) SubmitMeasurement(ctx context.Context, sample Measurement) (adopted bool, err error) {
	received, generated := a.measurementClock().UTC(), sample.GeneratedAt.UTC()
	generatedNS, receivedNS := generated.UnixNano(), received.UnixNano()
	if sample.Source != SimulatorMeasurementSource || !validDeviceID(sample.DeviceID) ||
		sample.DataGeneration <= 0 || sample.RunID <= 0 || sample.Sequence < 0 ||
		math.IsNaN(sample.Temperature) || math.IsInf(sample.Temperature, 0) ||
		!freshMeasurement(generated, received) ||
		!time.Unix(0, generatedNS).Equal(generated) || !time.Unix(0, receivedNS).Equal(received) {
		return false, nil
	}

	err = a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin measurement transaction: %w", err)
		}
		defer tx.Rollback()

		// 依序鎖定該設備的執行、最新測量及未恢復告警，等待結束後才判斷採用期限。
		var allowed int
		err = tx.QueryRowContext(ctx, `
			SELECT 1 FROM measurement_runs
			WHERE device_id = $1 AND data_generation = $2 AND run_id = $3 FOR UPDATE
		`, sample.DeviceID, sample.DataGeneration, sample.RunID).Scan(&allowed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock allowed measurement run: %w", err)
		}

		var lastSequence, lastGenerated int64
		err = tx.QueryRowContext(ctx, `
			SELECT sequence, generated_at_ns FROM latest_measurements WHERE device_id = $1 FOR UPDATE
		`, sample.DeviceID).Scan(&lastSequence, &lastGenerated)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock latest measurement: %w", err)
		}
		if err == nil && (sample.Sequence <= lastSequence || generatedNS < lastGenerated) {
			return nil
		}

		var activeAlert int64
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM alerts
			WHERE device_id = $1 AND type = 'temperature' AND recovered_at_ns IS NULL FOR UPDATE
		`, sample.DeviceID).Scan(&activeAlert)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock active temperature alert: %w", err)
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
		`, sample.DeviceID, sample.Temperature, sample.Sequence, generatedNS, receivedNS); err != nil {
			return fmt.Errorf("write latest measurement: %w", err)
		}
		abnormal := sample.Temperature < 18 || sample.Temperature > 28
		var alertErr error
		switch {
		case abnormal && activeAlert == 0:
			_, alertErr = tx.ExecContext(ctx, `
				INSERT INTO alerts (device_id, type, temperature, detected_at_ns)
				VALUES ($1, 'temperature', $2, $3)
			`, sample.DeviceID, sample.Temperature, nowNS)
		case !abnormal && activeAlert != 0:
			// 系統時鐘往回調整時，恢復時間仍不能早於同筆告警的發現時間。
			_, alertErr = tx.ExecContext(ctx, `
				UPDATE alerts SET recovered_at_ns = GREATEST(detected_at_ns, $2) WHERE id = $1
			`, activeAlert, nowNS)
		}
		if alertErr != nil {
			return fmt.Errorf("write temperature alert: %w", alertErr)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit measurement transaction: %w", err)
		}
		adopted = true
		return nil
	})
	return adopted, err
}
