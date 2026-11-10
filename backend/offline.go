package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"
)

const offlineTimeout = 15 * time.Second

type pendingMeasurement struct {
	received time.Time
	ready    chan struct{}
	done     chan struct{}
}

type pendingMeasurements struct {
	mu      sync.Mutex
	devices map[string][]*pendingMeasurement
}

// receiveMeasurement 依同設備抵達順序入列，讓逾時掃描可看見尚未取得資料庫連線的回報。
func (a *application) receiveMeasurement(id string, generated time.Time) *pendingMeasurement {
	p := &a.arrivals
	p.mu.Lock()
	defer p.mu.Unlock()
	// 在鎖內取抵達時間及檢查新鮮度，使掃描看到同一界線下的時間與佇列。
	received := a.measurementClock().UTC()
	if !freshMeasurement(generated, received) || !time.Unix(0, received.UnixNano()).Equal(received) {
		return nil
	}
	if p.devices == nil {
		p.devices = make(map[string][]*pendingMeasurement)
	}
	// 每台設備只有隊首可以進資料庫，其他回報等前一筆離開。
	report := &pendingMeasurement{received: received, ready: make(chan struct{}), done: make(chan struct{})}
	if len(p.devices[id]) == 0 {
		close(report.ready)
	}
	p.devices[id] = append(p.devices[id], report)
	return report
}

// release 結束一筆排隊回報並通知掃描，只在移除隊首時放行下一筆以維持順序。
func (p *pendingMeasurements) release(id string, report *pendingMeasurement) {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer close(report.done)
	// 等待取消也可能從隊中離開，必須只移除該筆並保留其餘順序。
	queue := p.devices[id]
	index := slices.Index(queue, report)
	queue = slices.Delete(queue, index, index+1)
	if len(queue) == 0 {
		delete(p.devices, id)
		return
	}
	p.devices[id] = queue
	if index == 0 {
		close(queue[0].ready)
	}
}

type deferredOfflineCheck struct {
	deviceID string
	done     <-chan struct{}
}

// scanOffline 在共同期限內巡檢逾時設備，只為可能影響判定的期限內回報暫緩。
func (a *application) scanOffline(ctx context.Context, atRestart bool) error {
	ctx, cancel := context.WithTimeout(ctx, dbOperationTimeout)
	defer cancel()
	var targets []string
	for {
		var deferred []deferredOfflineCheck
		// 每一輪重新取得持鎖連線。第一輪掃全部，之後只重查暫緩的設備。
		err := a.db.use(ctx, func(ctx context.Context, conn *sql.Conn) error {
			var err error
			deferred, err = a.scanOfflineLocked(ctx, conn, atRestart, targets)
			return err
		})
		if err != nil || len(deferred) == 0 {
			return err
		}

		// 釋放資料庫連線後才等候回報；任一設備完成便續判，避免阻塞其他設備。
		cases := make([]reflect.SelectCase, 1, len(deferred)+1)
		cases[0] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())}
		targets = make([]string, 0, len(deferred))
		for _, check := range deferred {
			targets = append(targets, check.deviceID)
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(check.done)})
		}
		if chosen, _, _ := reflect.Select(cases); chosen == 0 {
			return ctx.Err()
		}
	}
}

// scanOfflineLocked 持有資料庫操作權時，依抵達快照一次寫入可判定的離線告警，並傳回需要續判的設備。
func (a *application) scanOfflineLocked(ctx context.Context, conn *sql.Conn, atRestart bool, targets []string) ([]deferredOfflineCheck, error) {
	p := &a.arrivals
	p.mu.Lock()
	// 同時擷取判定時間及各設備最早的抵達時間。依隊首完成信號重新評估，保留實際處理順序。
	now := a.measurementClock().UTC()
	ids := make([]string, 0, len(p.devices))
	receipts := make([]int64, 0, len(p.devices))
	settlements := make(map[string]<-chan struct{}, len(p.devices))
	for id, queue := range p.devices {
		earliest := queue[0].received.UnixNano()
		for _, report := range queue[1:] {
			earliest = min(earliest, report.received.UnixNano())
		}
		ids, receipts = append(ids, id), append(receipts, earliest)
		settlements[id] = queue[0].done
	}
	p.mu.Unlock()
	nowNS := now.UnixNano()
	if !time.Unix(0, nowNS).Equal(now) {
		return nil, errors.New("offline scan clock outside supported range")
	}

	// 先收集實際新增告警的場域，確認交易提交後才能通知訂閱端。
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin offline scan: %w", err)
	}
	defer tx.Rollback()
	// 期限內已抵達的回報先處理；期限後才抵達的回報不會遮蔽逾時。
	rows, err := tx.QueryContext(ctx, `
		WITH deadlines AS (
			SELECT d.id, COALESCE(m.received_at_ns, s.first_expected_at_ns)::numeric + $3::bigint AS deadline,
				p.receipt
			FROM devices d
			LEFT JOIN latest_measurements m ON m.device_id = d.id
			LEFT JOIN device_simulations s ON s.device_id = d.id
			LEFT JOIN unnest($4::text[], $5::bigint[]) AS p(id, receipt) ON p.id = d.id
			LEFT JOIN alerts a ON a.device_id = d.id AND a.type = 'offline' AND a.recovered_at_ns IS NULL
			WHERE a.id IS NULL AND ($6::text[] IS NULL OR d.id = ANY($6))
		), overdue AS (
			SELECT id, deadline, receipt FROM deadlines WHERE deadline < $1::bigint
		), detected AS (
			INSERT INTO alerts (device_id, type, detected_at_ns, deadline_at_ns, detected_at_restart)
			SELECT id, 'offline', $1, deadline::bigint, $2 FROM overdue
			WHERE receipt IS NULL OR receipt > deadline
			ORDER BY id
			ON CONFLICT (device_id, type) WHERE recovered_at_ns IS NULL DO NOTHING
			RETURNING device_id
		)
		SELECT id, NULL::text FROM overdue WHERE receipt <= deadline
		UNION ALL
		SELECT NULL::text, d.site_id FROM detected JOIN devices d ON d.id = detected.device_id GROUP BY d.site_id
	`, nowNS, atRestart, offlineTimeout.Nanoseconds(), ids, receipts, targets)
	if err != nil {
		return nil, fmt.Errorf("persist offline detections: %w", err)
	}
	defer rows.Close()
	// 將期限內已有回報的設備交給外層續判，避免等待時仍占用資料庫連線。
	var deferred []deferredOfflineCheck
	var sites []string
	for rows.Next() {
		var id, site sql.NullString
		if err := rows.Scan(&id, &site); err != nil {
			return nil, fmt.Errorf("read offline scan result: %w", err)
		}
		if site.Valid {
			sites = append(sites, site.String)
		} else {
			deferred = append(deferred, deferredOfflineCheck{deviceID: id.String, done: settlements[id.String]})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("complete offline scan: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit offline scan: %w", err)
	}
	a.events.publish(sites...)
	return deferred, nil
}

// recoverOffline 隨有效測量恢復離線告警，也補記背景掃描前已經逾時的區間。
func recoverOffline(ctx context.Context, tx *sql.Tx, id string, active int64, baseline sql.NullInt64, received, adopted time.Time) error {
	var err error
	// 有持續離線告警時恢復同一筆。系統時間調整也不能使恢復早於發現。
	if active != 0 {
		_, err = tx.ExecContext(ctx, `
			UPDATE alerts SET recovered_at_ns = GREATEST(detected_at_ns, $2) WHERE id = $1
		`, active, adopted.UnixNano())
	} else if baseline.Valid {
		deadline := time.Unix(0, baseline.Int64).Add(offlineTimeout)
		if received.After(deadline) {
			// 採用晚到回報時才發現逾時，因此同次記錄發現與恢復，期限獨立保留。
			discovered := max(received.UnixNano(), adopted.UnixNano())
			_, err = tx.ExecContext(ctx, `
				INSERT INTO alerts (device_id, type, detected_at_ns, deadline_at_ns, recovered_at_ns)
				VALUES ($1, 'offline', $2, $3, $2)
			`, id, discovered, deadline.UnixNano())
		}
	}
	if err != nil {
		return fmt.Errorf("recover offline device: %w", err)
	}
	return nil
}
