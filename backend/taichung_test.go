package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const taichungTestRow = `{"Id":"001","Device":"環境感測器一","Town":"西屯區","Landmark":"市政公園","Temp":"23.4","Hum":"65","PM2.5":"12.5","Time":"2026-11-19T10:44:47"}`

// TestTaichungParsing 確認外部字串資料的有限數值、臺灣時間、名稱與同設備最新一筆規則。
func TestTaichungParsing(t *testing.T) {
	for _, test := range []struct {
		name, body string
		count      int
		fail       bool
	}{
		{"valid", "[" + taichungTestRow + "]", 1, false},
		{"empty", "[]", 0, false},
		{"null array", "null", 0, true},
		{"object", "{}", 0, true},
		{"malformed", "[", 0, true},
		{"trailing JSON", "[]{}", 0, true},
		{"nonfinite temperature", "[" + strings.Replace(taichungTestRow, `"23.4"`, `"NaN"`, 1) + "]", 0, false},
		{"infinite temperature", "[" + strings.Replace(taichungTestRow, `"23.4"`, `"+Inf"`, 1) + "]", 0, false},
		{"overflow temperature", "[" + strings.Replace(taichungTestRow, `"23.4"`, `"1e999"`, 1) + "]", 0, false},
		{"numeric instead of source string", "[" + strings.Replace(taichungTestRow, `"23.4"`, `23.4`, 1) + "]", 0, false},
		{"missing identity", "[" + strings.Replace(taichungTestRow, `"001"`, `""`, 1) + "]", 0, false},
		{"ambiguous identity", "[" + strings.Replace(taichungTestRow, `"001"`, `"a,b"`, 1) + "]", 0, false},
		{"missing device", "[" + strings.Replace(taichungTestRow, `"環境感測器一"`, `""`, 1) + "]", 0, false},
		{"missing district", "[" + strings.Replace(taichungTestRow, `"西屯區"`, `""`, 1) + "]", 0, false},
		{"invalid calendar time", "[" + strings.Replace(taichungTestRow, "2026-11-19", "2026-02-30", 1) + "]", 0, false},
		{"date only", "[" + strings.Replace(taichungTestRow, "2026-11-19T10:44:47", "2026-11-19", 1) + "]", 0, false},
		{"invalid UTF8", "[" + strings.Replace(taichungTestRow, "市政公園", "\xff", 1) + "]", 0, true},
		{"invalid row beside valid", "[null,3," + taichungTestRow + "]", 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			items, err := parseTaichungSensors([]byte(test.body))
			if (err != nil) != test.fail || len(items) != test.count {
				t.Fatalf("got %d items, %v; want count=%d failure=%v", len(items), err, test.count, test.fail)
			}
		})
	}
	items, err := parseTaichungSensors([]byte("[" + taichungTestRow + "]"))
	if err != nil || items[0].Name != "西屯區 · 市政公園" || items[0].Device != "環境感測器一" ||
		items[0].MeasuredAt.Format(time.RFC3339) != "2026-11-19T02:44:47Z" || items[0].Temperature != 23.4 ||
		items[0].Humidity == nil || *items[0].Humidity != 65 || items[0].PM25 == nil || *items[0].PM25 != 12.5 {
		t.Fatalf("source fields lost their identity or timezone: %+v %v", items, err)
	}
	latest := strings.ReplaceAll(strings.Replace(taichungTestRow, "10:44:47", "11:44:47", 1), "市政公園", "新地點")
	for _, body := range []string{"[" + latest + "," + taichungTestRow + "]", "[" + taichungTestRow + "," + latest + "]"} {
		items, err = parseTaichungSensors([]byte(body))
		if err != nil || len(items) != 1 || items[0].Name != "西屯區 · 新地點" {
			t.Fatalf("did not retain latest whole observation: %+v %v", items, err)
		}
	}
	// 缺地標時使用官方設備名稱；無效選填數值保持 null，不能補成正常數值。
	for _, values := range [][2]string{{"NaN", "+Inf"}, {"-1", "-2"}, {"101", ""}} {
		body := strings.Replace(strings.Replace(strings.Replace(taichungTestRow, `"65"`, `"`+values[0]+`"`, 1), `"12.5"`, `"`+values[1]+`"`, 1), `"市政公園"`, `""`, 1)
		items, err = parseTaichungSensors([]byte("[" + body + "]"))
		if err != nil || len(items) != 1 || items[0].Name != "西屯區 · 環境感測器一" || items[0].Humidity != nil || items[0].PM25 != nil {
			t.Fatalf("optional invalid observations were fabricated: %+v %v", items, err)
		}
	}
	for _, raw := range []string{`null`, `false`, `42`, `{}`} {
		body := strings.Replace(strings.Replace(taichungTestRow, `"65"`, raw, 1), `"12.5"`, raw, 1)
		items, err = parseTaichungSensors([]byte("[" + body + "]"))
		if err != nil || len(items) != 1 || items[0].Humidity != nil || items[0].PM25 != nil {
			t.Fatalf("optional malformed values must not discard valid temperature: %+v %v", items, err)
		}
	}
}

// TestTaichungSelection 驗證至多六個唯一設備、短清單，以及背景同步固定順序且不補入別台。
func TestTaichungSelection(t *testing.T) {
	for _, size := range []int{0, 1, 5, 6, 20} {
		items := make([]taichungSensor, size)
		for index := range items {
			items[index].ID = fmt.Sprint(index)
		}
		seenSets := make(map[string]bool)
		for attempt := 0; attempt < 20; attempt++ {
			got := selectTaichungSensors(items, nil)
			seen := make(map[string]bool)
			for _, item := range got {
				if seen[item.ID] {
					t.Fatalf("duplicate sample: %+v", got)
				}
				seen[item.ID] = true
			}
			if len(got) != min(size, 6) {
				t.Fatalf("got %d from %d rows", len(got), size)
			}
			seenSets[fmt.Sprint(got)] = true
		}
		if size == 20 && len(seenSets) < 2 {
			t.Fatal("sample never changed")
		}
		got := selectTaichungSensors(items, []string{"3", "missing", "1"})
		want := []string{}
		if size > 3 {
			want = append(want, "3")
		}
		if size > 1 {
			want = append(want, "1")
		}
		ids := []string{}
		for _, item := range got {
			ids = append(ids, item.ID)
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("fixed IDs got %v want %v", ids, want)
		}
		if got == nil {
			t.Fatal("empty selection must serialize as []")
		}
	}
}

type taichungTestTransport func(*http.Request) (*http.Response, error)

// RoundTrip 在真實 HTTP client 邊界提供受控回應，不允許正式服務新增可設定上游 URL。
func (f taichungTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type taichungWaitingContext struct {
	context.Context
	waiting chan<- struct{}
}

// Done 讓測試在等待者已到取消選擇點後才釋放上游，不依賴排程或睡眠。
func (c taichungWaitingContext) Done() <-chan struct{} {
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

// taichungTestResponse 建立與官方下載端點相同的非 JSON content type，避免錯誤依賴 MIME。
func taichungTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

// TestTaichungTransportLimits 確認固定來源、重新導向、本文上限、狀態碼與錯誤不會被誤當快照。
func TestTaichungTransportLimits(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"valid", 200, "[" + taichungTestRow + "]", false},
		{"empty valid snapshot", 200, "[]", false},
		{"failure", 500, "[]", true},
		{"oversized", 200, strings.Repeat(" ", taichungBodyLimit+1), true},
		{"malformed", 200, "not JSON", true},
		{"redirect", 302, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			service := taichungService{transport: taichungTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != taichungSourceURL || r.Method != http.MethodGet {
					t.Errorf("unexpected upstream %s %s", r.Method, r.URL)
				}
				if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 6*time.Second {
					t.Error("missing bounded upstream deadline")
				}
				response := taichungTestResponse(test.status, test.body)
				if test.status == 302 {
					response.Header.Set("Location", "http://127.0.0.1/private")
				}
				return response, nil
			})}
			_, _, err := service.snapshot(t.Context())
			if (err != nil) != test.wantError || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

// TestTaichungCacheConcurrentCancellationAndRecovery 以通道控制交錯，證明單次抓取、等待者取消與失敗後可恢復。
func TestTaichungCacheConcurrentCancellationAndRecovery(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	service := taichungService{transport: taichungTestTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return taichungTestResponse(200, "["+taichungTestRow+"]"), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	var group sync.WaitGroup
	group.Go(func() {
		if _, _, err := service.snapshot(t.Context()); err != nil {
			t.Error(err)
		}
	})
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	waiting := make(chan struct{}, 1)
	cancelled := make(chan error, 1)
	go func() {
		_, _, err := service.snapshot(taichungWaitingContext{ctx, waiting})
		cancelled <- err
	}()
	<-waiting
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation lost: %v", err)
	}
	waiting = make(chan struct{}, 20)
	for range 20 {
		group.Go(func() {
			if _, _, err := service.snapshot(taichungWaitingContext{t.Context(), waiting}); err != nil {
				t.Error(err)
			}
		})
	}
	for range 20 {
		<-waiting
	}
	close(release)
	group.Wait()
	_, fetched, err := service.snapshot(t.Context())
	if err != nil || fetched.IsZero() || calls.Load() != 1 {
		t.Fatalf("shared cache fetched %d times: %v", calls.Load(), err)
	}

	// 過期快取不冒充新資料；失敗保護結束後再次請求才能建立新快照。
	service.mu.Lock()
	service.fetchedAt = time.Now().Add(-taichungCacheLifetime)
	service.mu.Unlock()
	service.transport = taichungTestTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("upstream unavailable")
	})
	if _, _, err := service.snapshot(t.Context()); err == nil {
		t.Fatal("expired snapshot masked upstream failure")
	}
	if _, _, err := service.snapshot(t.Context()); err == nil || calls.Load() != 2 {
		t.Fatalf("failure retry protection missing, calls=%d err=%v", calls.Load(), err)
	}
	service.mu.Lock()
	service.retryAt = time.Now().Add(-time.Second)
	service.mu.Unlock()
	service.transport = taichungTestTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return taichungTestResponse(200, "[]"), nil })
	items, _, err := service.snapshot(t.Context())
	if err != nil || len(items) != 0 || calls.Load() != 3 {
		t.Fatalf("failed to recover: calls=%d items=%+v err=%v", calls.Load(), items, err)
	}
}

// TestTaichungFetchCancellation 確認抓取擁有者取消後釋放槽位，下一次請求能重新抓取。
func TestTaichungFetchCancellation(t *testing.T) {
	started := make(chan struct{})
	service := taichungService{transport: taichungTestTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, _, err := service.snapshot(ctx); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("fetch cancellation lost: %v", err)
	}
	service.transport = taichungTestTransport(func(*http.Request) (*http.Response, error) { return taichungTestResponse(200, "[]"), nil })
	if _, _, err := service.snapshot(t.Context()); err != nil {
		t.Fatalf("cancelled fetch prevented recovery: %v", err)
	}
}

// TestTaichungOwnerCancellationDoesNotCancelHealthyWaiters 確認擁有者取消後，健康等待者共用一次替代抓取。
func TestTaichungOwnerCancellationDoesNotCancelHealthyWaiters(t *testing.T) {
	for _, count := range []int{1, 8} {
		t.Run(fmt.Sprintf("%d waiters", count), func(t *testing.T) {
			var calls atomic.Int32
			started, replacement, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			service := taichungService{transport: taichungTestTransport(func(r *http.Request) (*http.Response, error) {
				switch calls.Add(1) {
				case 1:
					close(started)
					<-r.Context().Done()
					return nil, r.Context().Err()
				case 2:
					close(replacement)
					select {
					case <-release:
						return taichungTestResponse(200, "["+taichungTestRow+"]"), nil
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				default:
					return nil, errors.New("unexpected extra replacement fetch")
				}
			})}
			ownerCtx, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			ownerDone := make(chan error, 1)
			go func() {
				_, _, err := service.snapshot(ownerCtx)
				ownerDone <- err
			}()
			<-started
			type result struct {
				items     []taichungSensor
				fetchedAt time.Time
				err       error
			}
			waiting, results := make(chan struct{}, count), make(chan result, count)
			for range count {
				go func() {
					items, fetchedAt, err := service.snapshot(taichungWaitingContext{t.Context(), waiting})
					results <- result{items, fetchedAt, err}
				}()
			}
			// 每位健康呼叫者都已等待同一抓取，才取消原擁有者。
			for range count {
				<-waiting
			}
			cancelOwner()
			if err := <-ownerDone; !errors.Is(err, context.Canceled) {
				t.Fatalf("owner lost its cancellation: %v", err)
			}
			select {
			case <-replacement:
				close(release)
			case got := <-results:
				t.Fatalf("healthy waiter returned before replacement: err=%v context=%v calls=%d", got.err, t.Context().Err(), calls.Load())
			}
			for range count {
				got := <-results
				if got.err != nil || len(got.items) != 1 || got.items[0].ID != "001" || got.fetchedAt.IsZero() {
					t.Fatalf("healthy waiter did not recover: %+v", got)
				}
			}
			items, fetchedAt, err := service.snapshot(t.Context())
			if err != nil || len(items) != 1 || fetchedAt.IsZero() || calls.Load() != 2 {
				t.Fatalf("replacement was not shared and cached: items=%+v fetchedAt=%v err=%v calls=%d", items, fetchedAt, err, calls.Load())
			}
		})
	}
}

// TestTaichungHTTPGuards 經真實路由檢查 Host、session、本文及查詢，拒絕時不得呼叫官方服務。
func TestTaichungHTTPGuards(t *testing.T) {
	app := &application{host: "iot.localhost:8090", origin: measurementTestOrigin}
	app.taichung.transport = taichungTestTransport(func(*http.Request) (*http.Response, error) {
		t.Error("rejected request reached upstream")
		return nil, errors.New("forbidden fetch")
	})
	server := app.router()
	for _, test := range []struct {
		path, body string
		status     int
	}{
		{"", "", 401}, {"?ids=001", "", 401}, {"?ids=1,2,3,4,5,6", "", 401},
		{"?ids=", "", 400}, {"?ids=1,1", "", 400}, {"?ids=1,,2", "", 400}, {"?ids=1,2,3,4,5,6,7", "", 400},
		{"?ids=1&ids=2", "", 400}, {"?url=http://localhost", "", 400}, {"?ids=%zz", "", 400}, {"?ids=a%20b", "", 400},
		{"", "{}", 400},
	} {
		request := httptest.NewRequest(http.MethodGet, measurementTestOrigin+"/api/taichung/sensors"+test.path, strings.NewReader(test.body))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != test.status || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s got %d %s", test.path, response.Code, response.Body)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://foreign.test/api/taichung/sensors", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatalf("Host protection missing: %d", response.Code)
	}
}

// TestTaichungIntegration 使用既有隔離 PostgreSQL seam 驗證場域登入也可讀同一官方資料，且不影響設備與告警。
func TestTaichungIntegration(t *testing.T) {
	target := openMeasurementTestDatabase(t)
	if target.relations != 0 {
		t.Fatal("expected empty disposable database")
	}
	state := openMeasurementTestState(t, true)
	passwords := seedLegacyMeasurementFixture(t, target.conn, state)
	if err := bootstrap(t.Context(), target.conn, state); err != nil {
		t.Fatal(err)
	}
	app := measurementTestApplication(target.conn)
	var calls atomic.Int32
	app.taichung.transport = taichungTestTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return taichungTestResponse(200, "["+taichungTestRow+"]"), nil
	})
	f := serveMeasurementFixture(t, app)
	f.hq = f.login(t, "hq", passwords["hq"])
	north := f.login(t, "north", passwords["north"])
	before := f.snapshot(t)
	var first taichungPage
	for _, cookie := range []*http.Cookie{f.hq, north} {
		body, _ := f.request(t, http.MethodGet, "/api/taichung/sensors", "", cookie, 200)
		var page taichungPage
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Total != 1 || page.Items[0].ID != "001" || page.SourceURL != taichungSourceURL || page.FetchedAt.IsZero() {
			t.Fatalf("invalid response: %s", body)
		}
		if first.Items != nil && !reflect.DeepEqual(first, page) {
			t.Fatal("public data differs by local site permission")
		}
		first = page
	}
	body, _ := f.request(t, http.MethodGet, "/api/taichung/sensors?ids=missing,001", "", north, 200)
	var page taichungPage
	if err := json.Unmarshal(body, &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != "001" {
		t.Fatalf("passive refresh substituted a device: %s %v", body, err)
	}
	if calls.Load() != 1 || before != f.snapshot(t) {
		t.Fatal("official read changed local state or missed shared cache")
	}
	app.taichung.mu.Lock()
	app.taichung.fetchedAt = time.Now().Add(-taichungCacheLifetime)
	app.taichung.mu.Unlock()
	app.taichung.transport = taichungTestTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("failed source") })
	f.request(t, http.MethodGet, "/api/taichung/sensors", "", north, 503)
	f.request(t, http.MethodGet, "/api/taichung/sensors", "", nil, 401)
	// 撤銷後即使持有原 Cookie 也必須先拒絕，不得取得共用快照。
	f.request(t, http.MethodPost, "/api/logout", "", north, 204)
	f.request(t, http.MethodGet, "/api/taichung/sensors", "", north, 401)
}
