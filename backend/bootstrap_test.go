package main

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// TestDemoPasswordsSurviveBootstrapRetry 驗證啟動中斷後仍可重用已發布的憑證，且檔案必須維持私密。
func TestDemoPasswordsSurviveBootstrapRetry(t *testing.T) {
	directory := t.TempDir()
	state, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	// 測試結束時關閉暫存目錄，避免留下開啟的檔案控制代碼。
	t.Cleanup(func() { state.Close() })
	if _, err := demoPasswords(state, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("existing database accepted a missing credential file")
	}

	// 首次初始化需為三個帳號發布各自的有效密碼，並限制檔案讀取權限。
	first, err := demoPasswords(state, true)
	if err != nil {
		t.Fatal(err)
	}
	hq, north, south := first["hq"], first["north"], first["south"]
	if len(first) != 3 ||
		!validSecret(hq) ||
		!validSecret(north) ||
		!validSecret(south) ||
		hq == north ||
		hq == south ||
		north == south {
		t.Fatal("expected distinct random credentials for the three demo accounts")
	}
	info, err := state.Stat(demoPasswordsName)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0o600 {
		t.Fatalf("got mode %v, want regular file with 0600", info.Mode())
	}
	// 模擬重新初始化及既有資料庫啟動，兩者都應重用相同密碼。
	for _, initialize := range []bool{true, false} {
		reused, err := demoPasswords(state, initialize)
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(reused, first) {
			t.Fatal("bootstrap replaced previously published credentials")
		}
	}
	// 讓其他使用者可讀後，即使仍是初始化也應拒絕。
	if err := os.Chmod(filepath.Join(directory, demoPasswordsName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := demoPasswords(state, true); err == nil {
		t.Fatal("bootstrap accepted credentials accessible by other users")
	}
}
