package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/metrics"
	"time"
)

type performanceRuntimeSnapshot struct {
	BeginNS      string  `json:"beginNs"`
	EndNS        string  `json:"endNs"`
	GCCount      *uint64 `json:"gcCount,omitempty"`
	CPUStat      string  `json:"cpuStat"`
	CPUStatLocal string  `json:"cpuStatLocal"`
	Error        string  `json:"error,omitempty"`
}

type performanceDiagnosticResult struct {
	Start             performanceRuntimeSnapshot  `json:"start"`
	FirstMiss         *performanceEvent           `json:"firstMiss,omitempty"`
	FirstMissObserved *performanceRuntimeSnapshot `json:"firstMissObserved,omitempty"`
	End               performanceRuntimeSnapshot  `json:"end"`
}

// performanceRuntime 只讀已完成 GC 次數；cgroup 自身限制與含 ancestor 的實際節流分開保留。
// 起訖界定非原子取樣窗，首漏槽摘要是 encoder 收到事件後的值，累積正差值不是該槽因果。
func performanceRuntime() (snapshot performanceRuntimeSnapshot, resultErr error) {
	snapshot.BeginNS = performanceNS(time.Now())
	samples := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindUint64 {
		resultErr = errors.New("performance fixture: completed GC cycle metric unavailable or wrong kind")
	} else {
		count := samples[0].Value.Uint64()
		snapshot.GCCount = &count
	}
	cpu, err := os.ReadFile("/sys/fs/cgroup/cpu.stat")
	snapshot.CPUStat = string(cpu)
	if err != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("performance fixture: read own CPU quota statistics: %w", err))
	}
	local, err := os.ReadFile("/sys/fs/cgroup/cpu.stat.local")
	snapshot.CPUStatLocal = string(local)
	if errors.Is(err, os.ErrNotExist) {
		// 舊 kernel 未提供這項補充計數時明示能力缺失，不把自身 quota 零值當成 ancestor 沒有節流。
		snapshot.CPUStatLocal = "UNAVAILABLE: " + err.Error()
	} else if err != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("performance fixture: read local runqueue throttling statistics: %w", err))
	}
	snapshot.EndNS = performanceNS(time.Now())
	if resultErr != nil {
		snapshot.Error = resultErr.Error()
	}
	return
}
