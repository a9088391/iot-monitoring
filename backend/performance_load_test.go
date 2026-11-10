package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	performanceDeviceCount = 100
	performanceInterval    = 50 * time.Millisecond
	performanceWarmupSlots = 1200
	performanceFormalSlots = 6000
	performanceTailSlots   = 500
	performanceInFlight    = 128
)

type performancePlan struct {
	phase  string
	slot   int
	at     time.Time
	sample Measurement
}

type performanceEvent struct {
	Phase       string  `json:"phase"`
	Slot        int     `json:"slot"`
	DeviceID    string  `json:"deviceId"`
	Sequence    int64   `json:"sequence"`
	Temperature float64 `json:"temperature"`
	PlannedNS   string  `json:"plannedNs"`
	SentNS      string  `json:"sentNs,omitempty"`
	EntryNS     string  `json:"entryNs,omitempty"`
	ResultNS    string  `json:"resultNs,omitempty"`
	Outcome     string  `json:"outcome"`
	Error       string  `json:"error,omitempty"`
}

// performanceSchedule 以絕對時槽維持開迴路，離線、恢復及無效回報都獨立於正式計數。
func performanceSchedule(devices []performanceDevice, start time.Time) []performancePlan {
	formalEnd := performanceWarmupSlots + performanceFormalSlots
	plans := make([]performancePlan, 0, formalEnd+performanceTailSlots+2)
	for slot := range formalEnd + performanceTailSlots {
		index := slot % performanceDeviceCount
		phase := "warmup"
		switch {
		case slot >= formalEnd:
			phase = "tail"
			// 固定只中斷北區第一台，其他設備維持原排程，最後一輪送恢復回報。
			if index == 0 {
				if slot < formalEnd+4*performanceDeviceCount {
					continue
				}
				phase = "recovery"
			}
		case slot >= performanceWarmupSlots:
			phase = "formal"
		}
		at := start.Add(time.Duration(slot) * performanceInterval)
		sequence := int64(slot/performanceDeviceCount + 1)
		plans = append(plans, performancePlan{phase, slot, at, performanceSample(devices[index], index, sequence, at)})
	}
	// 正式量測結束後重送南區最後一台的原樣回報，再送一筆超過五秒的回報。
	tailStart := start.Add(time.Duration(formalEnd) * performanceInterval)
	index := performanceDeviceCount - 1
	sequence := int64(formalEnd / performanceDeviceCount)
	original := performanceSample(devices[index], index, sequence, tailStart.Add(-performanceInterval))
	expiredAt := tailStart.Add(200 * time.Millisecond)
	expired := performanceSample(devices[index], index, sequence+1, expiredAt.Add(-6*time.Second))
	plans = append(plans,
		performancePlan{"invalid", -1, tailStart.Add(100 * time.Millisecond), original},
		performancePlan{"invalid", -2, expiredAt, expired},
	)
	// 將獨立的無效回報合併到絕對時間順序，不改原本有效時槽。
	slices.SortStableFunc(plans, func(a, b performancePlan) int { return a.at.Compare(b.at) })
	return plans
}

// performanceSample 為每台每筆產生可在 DOM 唯一識別的溫度，指定的兩台另跨越告警門檻。
func performanceSample(device performanceDevice, index int, sequence int64, generated time.Time) Measurement {
	base := int64(20)
	if (index == 0 || index == 50) &&
		(sequence == 16 ||
			sequence == 17 ||
			sequence == 20) {
		base = 30
	}
	return Measurement{
		Source: SimulatorMeasurementSource, DeviceID: device.ID, DataGeneration: initialGeneration, RunID: 1,
		Sequence: sequence, GeneratedAt: generated, Temperature: float64(base*100000+sequence*100+int64(index)+1) / 100000,
	}
}

// recordPerformanceLoad 將每個結果立即寫入證據檔，排程從不等待採用或主機讀取。
func recordPerformanceLoad(ctx context.Context, app *application, state *os.Root, devices []performanceDevice, start time.Time) (int, error) {
	file, err := state.OpenFile(performanceEvents, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, errors.New("performance fixture: open event evidence")
	}
	started := struct {
		StartNS string `json:"startNs"`
	}{performanceNS(start)}
	if err := writePerformanceJSON(state, performanceStarted, started, false); err != nil {
		return 0, errors.Join(err, file.Close())
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	plans := performanceSchedule(devices, start)
	events := make(chan performanceEvent, len(plans))
	pacer := make(chan error, 1)
	// 緩衝以完整且固定的計畫筆數為上限，個別結果不會阻塞排程。
	go func() {
		pacer <- pacePerformanceLoad(ctx, app, plans, events)
		close(events)
	}()
	count := 0
	var writeErr error
	encoder := json.NewEncoder(file)
	for event := range events {
		if writeErr != nil {
			continue
		}
		if err := encoder.Encode(event); err != nil {
			writeErr = errors.New("performance fixture: write event evidence")
			cancel()
			continue
		}
		count++
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		writeErr = errors.Join(writeErr, errors.New("performance fixture: flush event evidence"))
	}
	return count, errors.Join(<-pacer, writeErr)
}

// pacePerformanceLoad 每次依單調時計的絕對時間發出，不因資料庫繁忙挪動後續時槽。
func pacePerformanceLoad(ctx context.Context, app *application, plans []performancePlan, events chan<- performanceEvent) error {
	active := make(chan struct{}, performanceInFlight)
	var workers sync.WaitGroup
	defer workers.Wait()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for _, plan := range plans {
		timer.Reset(time.Until(plan.at))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		event := performanceEvent{
			Phase: plan.phase, Slot: plan.slot, DeviceID: plan.sample.DeviceID, Sequence: plan.sample.Sequence,
			Temperature: plan.sample.Temperature, PlannedNS: performanceNS(plan.at),
		}
		if time.Since(plan.at) >= performanceInterval {
			event.Outcome, event.Error = "missed", "scheduler late by at least one slot"
			events <- event
			continue
		}
		select {
		case active <- struct{}{}:
			// 在背景工作實際呼叫入口時取送出時間，前一筆完成與否不影響下一時槽。
			workers.Go(func() {
				// 完成後歸還槽位，包含被拒絕或資料庫逾時的回報。
				defer func() { <-active }()
				events <- submitPerformanceSample(ctx, app, plan, event)
			})
		default:
			event.Outcome, event.Error = "missed", "in-flight generator capacity exhausted"
			events <- event
		}
	}
	return nil
}

// submitPerformanceSample 記錄同一入口提供的抵達時間，以及採用、拒絕或錯誤結果。
func submitPerformanceSample(ctx context.Context, app *application, plan performancePlan, event performanceEvent) performanceEvent {
	sent := app.measurementClock()
	if sent.Sub(plan.at) >= performanceInterval {
		event.Outcome, event.Error = "missed", "submission worker late by at least one slot"
		return event
	}
	event.SentNS = performanceNS(sent)
	adopted, received, err := app.submitMeasurement(ctx, plan.sample)
	event.EntryNS = performanceNS(received)
	event.ResultNS = performanceNS(app.measurementClock())
	switch {
	case err != nil:
		event.Outcome, event.Error = "error", err.Error()
	case adopted:
		event.Outcome = "adopted"
	default:
		event.Outcome = "rejected"
	}
	return event
}

// TestPerformanceSchedule 鎖定公開的量測時槽與例外數量，避免校正或故障案例混入正式吞吐量。
func TestPerformanceSchedule(t *testing.T) {
	devices := make([]performanceDevice, performanceDeviceCount)
	for i := range devices {
		devices[i].ID = fmt.Sprintf("fixture-%d", i)
	}
	start := time.Unix(1000, 0)
	count := make(map[string]int)
	temperatures := make(map[float64]struct{})
	var previous time.Time
	for _, plan := range performanceSchedule(devices, start) {
		count[plan.phase]++
		if plan.at.Before(previous) {
			t.Fatal("performance plan is not chronologically ordered")
		}
		previous = plan.at
		if plan.phase == "invalid" {
			continue
		}
		if !plan.at.Equal(start.Add(time.Duration(plan.slot) * 50 * time.Millisecond)) {
			t.Fatalf("non-absolute schedule at slot %d", plan.slot)
		}
		if _, exists := temperatures[plan.sample.Temperature]; exists {
			t.Fatalf("ambiguous visible temperature at slot %d", plan.slot)
		}
		temperatures[plan.sample.Temperature] = struct{}{}
	}
	if count["warmup"] != 1200 ||
		count["formal"] != 6000 ||
		count["tail"] != 495 ||
		count["recovery"] != 1 ||
		count["invalid"] != 2 {
		t.Fatalf("unexpected schedule counts: %v", count)
	}
}

// TestPerformancePacerKeepsMisses 證明逾期時槽留有失敗證據，且不補送到原服務。
func TestPerformancePacerKeepsMisses(t *testing.T) {
	events := make(chan performanceEvent, 1)
	plans := []performancePlan{{phase: "formal", slot: 1200, at: time.Now().Add(-time.Second)}}
	if err := pacePerformanceLoad(t.Context(), nil, plans, events); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-events:
		if got.Outcome != "missed" ||
			got.Slot != 1200 ||
			got.PlannedNS == "" ||
			got.SentNS != "" ||
			got.EntryNS != "" {
			t.Fatalf("unexpected missed slot evidence: %+v", got)
		}
	default:
		t.Fatal("missed slot was not recorded")
	}
}
