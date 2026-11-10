package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

// TestLocalOriginHost 驗證啟動時只接受瀏覽器會原樣送出的專用本機網址。
func TestLocalOriginHost(t *testing.T) {
	// 啟動設定必須與瀏覽器實際送出的 Host 及 Origin 完全一致。
	for _, test := range []struct {
		origin string
		host   string
	}{
		{"http://iot.localhost:1", "iot.localhost:1"},
		{"http://iot.localhost:18241", "iot.localhost:18241"},
		{"http://iot.localhost:65535", "iot.localhost:65535"},
		{"http://127.0.0.1:18241", ""},
		{"http://foreign.test:18241", ""},
		{"http://iot.localhost.:18241", ""},
		{"https://iot.localhost:18241", ""},
		{"http://iot.localhost", ""},
		{"http://iot.localhost:0", ""},
		{"http://iot.localhost:80", ""},
		{"http://iot.localhost:018241", ""},
		{"http://iot.localhost:65536", ""},
		{"http://iot.localhost:18241/", ""},
		{"http://user@iot.localhost:18241", ""},
		{"http://iot.localhost:invalid", ""},
	} {
		// 依網址逐一核對標準 Host 或啟動錯誤。
		t.Run(test.origin, func(t *testing.T) {
			host, err := localOriginHost(test.origin)
			if host != test.host || (err != nil) != (test.host == "") {
				t.Fatalf("got host %q, error %v, want host %q", host, err, test.host)
			}
		})
	}
}

// TestWatchdogProbeQueueTimeout 用虛擬時鐘證明排隊到期仍算未探測，避免繁忙時誤關服務。
func TestWatchdogProbeQueueTimeout(t *testing.T) {
	// 用 synctest 讓期限在無真實連線或等待的情況下到期。
	synctest.Test(t, func(t *testing.T) {
		// 另一筆查詢尚未釋放連線時，排隊逾時應標為尚未探測。
		a := application{db: &lockedDB{gate: make(chan struct{}, 1)}}
		a.db.gate <- struct{}{}
		probed, err := a.watchdogProbe(context.Background())
		if probed || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("probed = %v, err = %v, want an unstarted probe with deadline exceeded", probed, err)
		}
	})
}
