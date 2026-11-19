package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
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
	WakeNS      string  `json:"wakeNs"`
	MonoLagNS   string  `json:"monoLagNs"`
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
func recordPerformanceLoad(ctx context.Context, app *application, state *os.Root, devices []performanceDevice, start time.Time) (result performanceRunResult, resultErr error) {
	// 證據或摘要錯誤也必須使完整性失敗，不讓缺少觀測的量測輪誤報完成。
	defer func() {
		result.Complete = resultErr == nil
		if resultErr != nil {
			result.Error = resultErr.Error()
		}
	}()
	diagnostic := &performanceDiagnosticResult{}
	result.Diagnostics = diagnostic
	var err error
	diagnostic.Start, err = performanceRuntime()
	if err != nil {
		return result, err
	}
	defer func() {
		var err error
		diagnostic.End, err = performanceRuntime()
		resultErr = errors.Join(resultErr, err)
	}()
	file, err := state.OpenFile(performanceEvents, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return result, errors.New("performance fixture: open event evidence")
	}
	started := struct {
		StartNS string `json:"startNs"`
	}{performanceNS(start)}
	if err := writePerformanceJSON(state, performanceStarted, started, false); err != nil {
		return result, errors.Join(err, file.Close())
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
		result.Events++
		if event.Outcome == "missed" && diagnostic.FirstMiss == nil {
			diagnostic.FirstMiss = &event
			snapshot, err := performanceRuntime()
			diagnostic.FirstMissObserved = &snapshot
			if err != nil {
				writeErr = errors.Join(writeErr, err)
				cancel()
			}
		}
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		writeErr = errors.Join(writeErr, errors.New("performance fixture: flush event evidence"))
	}
	return result, errors.Join(<-pacer, writeErr)
}

// waitPerformanceDeadline 不依賴一次 timer 喚醒；在原應用 CPU 配額內主動等待並讓其他工作進展。
func waitPerformanceDeadline(ctx context.Context, deadline time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		runtime.Gosched()
	}
}

// pacePerformanceLoad 每次依單調時計的絕對時間發出，不因資料庫繁忙挪動後續時槽。
func pacePerformanceLoad(ctx context.Context, app *application, plans []performancePlan, events chan<- performanceEvent) error {
	active := make(chan struct{}, performanceInFlight)
	var workers sync.WaitGroup
	defer workers.Wait()
	for _, plan := range plans {
		if err := waitPerformanceDeadline(ctx, plan.at); err != nil {
			return err
		}
		event := performanceEvent{
			Phase: plan.phase, Slot: plan.slot, DeviceID: plan.sample.DeviceID, Sequence: plan.sample.Sequence,
			Temperature: plan.sample.Temperature, PlannedNS: performanceNS(plan.at),
		}
		// 在原逾期檢查點取一次時間，同一值同時決定漏槽與記錄證據，並非核心計時器的觸發時間。
		wake := time.Now()
		lag := wake.Sub(plan.at)
		event.WakeNS, event.MonoLagNS = performanceNS(wake), strconv.FormatInt(int64(lag), 10)
		if lag >= performanceInterval {
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
		lag, err := strconv.ParseInt(got.MonoLagNS, 10, 64)
		if err != nil || lag < int64(performanceInterval) || got.WakeNS == "" {
			t.Fatalf("missed slot lacks actual wake and monotonic delay: %+v", got)
		}
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

// TestPerformanceRecordDoesNotStartTrace 鎖定正式量測只保存輕量摘要，不建立會擾動排程的 trace。
func TestPerformanceRecordDoesNotStartTrace(t *testing.T) {
	if _, err := os.Stat("/sys/fs/cgroup/cpu.stat"); err != nil {
		t.Skip("requires the Linux cgroup performance fixture")
	}
	state, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	result, err := recordPerformanceLoad(t.Context(), nil, state, make([]performanceDevice, performanceDeviceCount), time.Now().Add(-time.Hour))
	if err != nil || !result.Complete || result.Events != 7698 {
		t.Fatalf("expired result=%+v err=%v", result, err)
	}
	if _, err := state.Stat("performance-scheduler.trace"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("qualification unexpectedly created runtime trace: %v", err)
	}
	d := result.Diagnostics
	if d == nil || d.FirstMiss == nil || d.FirstMissObserved == nil || d.FirstMiss.Slot != 0 || d.FirstMiss.SentNS != "" || d.FirstMiss.EntryNS != "" {
		t.Fatalf("missing first observed miss: %+v", d)
	}
	for _, snapshot := range []performanceRuntimeSnapshot{d.Start, *d.FirstMissObserved, d.End} {
		begin, beginErr := strconv.ParseInt(snapshot.BeginNS, 10, 64)
		end, endErr := strconv.ParseInt(snapshot.EndNS, 10, 64)
		if beginErr != nil || endErr != nil || end < begin || snapshot.GCCount == nil || snapshot.Error != "" ||
			!strings.Contains(snapshot.CPUStat, "usage_usec ") || (!strings.Contains(snapshot.CPUStatLocal, "throttled_usec ") && !strings.HasPrefix(snapshot.CPUStatLocal, "UNAVAILABLE: ")) {
			t.Fatalf("invalid bounded runtime snapshot: %+v", snapshot)
		}
	}
	data, err := state.ReadFile(performanceEvents)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 7698 {
		t.Fatalf("expired evidence count=%d", len(lines))
	}
	for _, line := range lines {
		var event performanceEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Outcome != "missed" || event.SentNS != "" || event.EntryNS != "" || event.ResultNS != "" {
			t.Fatalf("expired slot submitted: %+v", event)
		}
	}
}

// TestPerformanceDeadlineWaiter 直接驗證共用等待器不早返、取消優先，以及單 P 下同儕仍能取消等待。
func TestPerformanceDeadlineWaiter(t *testing.T) {
	t.Run("future-not-early", func(t *testing.T) {
		deadline := time.Now().Add(10 * time.Millisecond)
		if err := waitPerformanceDeadline(t.Context(), deadline); err != nil {
			t.Fatal(err)
		}
		if time.Now().Before(deadline) {
			t.Fatal("waiter returned before the absolute deadline")
		}
	})
	t.Run("pre-cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := waitPerformanceDeadline(ctx, time.Now().Add(-time.Hour)); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("peer-cancel-single-p", func(t *testing.T) {
		previous := runtime.GOMAXPROCS(1)
		defer runtime.GOMAXPROCS(previous)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { cancel(); close(done) }()
		if err := waitPerformanceDeadline(ctx, time.Now().Add(time.Hour)); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		<-done
	})
	t.Run("empty-pacer", func(t *testing.T) {
		if err := pacePerformanceLoad(t.Context(), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	})
}

// TestPerformanceWorkerKeepsDeadlineBoundary 即使 pacer 準時，worker 達到完整一槽仍不得送出或補送。
func TestPerformanceWorkerKeepsDeadlineBoundary(t *testing.T) {
	planned := time.Now()
	app := &application{measurementNow: func() time.Time { return planned.Add(performanceInterval) }}
	event := submitPerformanceSample(t.Context(), app, performancePlan{at: planned}, performanceEvent{})
	if event.Outcome != "missed" || event.Error != "submission worker late by at least one slot" ||
		event.SentNS != "" || event.EntryNS != "" || event.ResultNS != "" {
		t.Fatalf("deadline boundary submitted a measurement: %+v", event)
	}
}

// TestPerformanceRecordFailures 確認取消與既有證據檔錯誤仍回不完整，且不覆寫先前原件。
func TestPerformanceRecordFailures(t *testing.T) {
	if _, err := os.Stat("/sys/fs/cgroup/cpu.stat"); err != nil {
		t.Skip("requires the Linux cgroup performance fixture")
	}
	for _, scenario := range []string{"cancel", "existing-events"} {
		t.Run(scenario, func(t *testing.T) {
			state, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			ctx := t.Context()
			if scenario == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else if err := state.WriteFile(performanceEvents, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := recordPerformanceLoad(ctx, nil, state, make([]performanceDevice, performanceDeviceCount), time.Now().Add(time.Hour))
			if err == nil || result.Complete || result.Error == "" || result.Events != 0 {
				t.Fatalf("failure reported complete: %+v err=%v", result, err)
			}
			if scenario == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if scenario == "existing-events" {
				data, err := state.ReadFile(performanceEvents)
				if err != nil || string(data) != "keep" {
					t.Fatalf("existing evidence overwritten: %q err=%v", data, err)
				}
			}
			if result.Diagnostics == nil || result.Diagnostics.FirstMiss != nil {
				t.Fatalf("failure fabricated a missed slot: %+v", result.Diagnostics)
			}
		})
	}
}
