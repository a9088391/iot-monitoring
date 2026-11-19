package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const (
	taichungSourceURL     = "https://newdatacenter.taichung.gov.tw/api/v1/no-auth/resource.download?rid=5a16b622-95ca-4e80-9d9f-e62a42962f4e"
	taichungBodyLimit     = 5 * 1024 * 1024
	taichungCacheLifetime = 10 * time.Minute
)

type taichungSensor struct {
	ID          string    `json:"id"`
	Device      string    `json:"device"`
	Name        string    `json:"name"`
	District    string    `json:"district"`
	Landmark    string    `json:"landmark"`
	Temperature float64   `json:"temperature"`
	Humidity    *float64  `json:"humidity"`
	PM25        *float64  `json:"pm25"`
	MeasuredAt  time.Time `json:"measuredAt"`
}

type taichungPage struct {
	Items     []taichungSensor `json:"items"`
	Total     int              `json:"total"`
	FetchedAt time.Time        `json:"fetchedAt"`
	SourceURL string           `json:"sourceUrl"`
}

type taichungSourceRow struct {
	ID       string          `json:"Id"`
	Device   string          `json:"Device"`
	Town     string          `json:"Town"`
	Landmark string          `json:"Landmark"`
	Temp     string          `json:"Temp"`
	Humidity json.RawMessage `json:"Hum"`
	PM25     json.RawMessage `json:"PM2.5"`
	Time     string          `json:"Time"`
}

// validTaichungID 保留官方字串識別，拒絕無法在單一逗號清單中明確表示的值。
func validTaichungID(id string) bool {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool { return r == ',' || unicode.IsSpace(r) || unicode.IsControl(r) })
}

// taichungNumber 只接受有限的官方數值，缺值或無效值由呼叫者依必填性處理。
func taichungNumber(raw string) *float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}

// taichungOptionalNumber 將缺漏或錯誤型別的選填欄位保留為 null，不丟棄同筆有效溫度。
func taichungOptionalNumber(raw json.RawMessage) *float64 {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return taichungNumber(value)
}

// parseTaichungSensors 將官方戶外觀測轉成獨立 DTO，不套用本機模擬的在線或告警規則。
func parseTaichungSensors(body []byte) ([]taichungSensor, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("Taichung source: invalid UTF-8")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("decode Taichung source: %w", err)
	}
	if rows == nil {
		return nil, errors.New("Taichung source: expected array")
	}
	items := make([]taichungSensor, 0, len(rows))
	positions := make(map[string]int)
	taipei := time.FixedZone("Asia/Taipei", 8*60*60)
	for _, raw := range rows {
		var row taichungSourceRow
		if err := json.Unmarshal(raw, &row); err != nil {
			continue
		}
		row.Device, row.Town, row.Landmark = strings.TrimSpace(row.Device), strings.TrimSpace(row.Town), strings.TrimSpace(row.Landmark)
		temperature := taichungNumber(row.Temp)
		// 來源未帶時區的 Time 是臺灣時間；輸出統一 UTC，避免執行環境改變解讀。
		measuredAt, err := time.ParseInLocation("2006-01-02T15:04:05", row.Time, taipei)
		if !validTaichungID(row.ID) || row.Device == "" || row.Town == "" || temperature == nil || err != nil {
			continue
		}
		humidity, pm25 := taichungOptionalNumber(row.Humidity), taichungOptionalNumber(row.PM25)
		if humidity != nil && (*humidity < 0 || *humidity > 100) {
			humidity = nil
		}
		if pm25 != nil && *pm25 < 0 {
			pm25 = nil
		}
		place := row.Landmark
		if place == "" {
			place = row.Device
		}
		item := taichungSensor{
			ID: row.ID, Device: row.Device, Name: row.Town + " · " + place, District: row.Town, Landmark: row.Landmark,
			Temperature: *temperature, Humidity: humidity, PM25: pm25, MeasuredAt: measuredAt.UTC(),
		}
		if index, exists := positions[row.ID]; exists {
			if item.MeasuredAt.After(items[index].MeasuredAt) {
				items[index] = item
			}
			continue
		}
		positions[row.ID] = len(items)
		items = append(items, item)
	}
	return items, nil
}

type taichungService struct {
	mu        sync.Mutex
	items     []taichungSensor
	fetchedAt time.Time
	retryAt   time.Time
	err       error
	inflight  chan struct{}
	transport http.RoundTripper
}

// snapshot 共用成功快照並合併併發抓取；等待者可取消，失敗不以過期快照冒充新資料。
func (s *taichungService) snapshot(ctx context.Context) ([]taichungSensor, time.Time, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, time.Time{}, err
		}
		s.mu.Lock()
		if !s.fetchedAt.IsZero() && time.Since(s.fetchedAt) < taichungCacheLifetime {
			items, fetched := s.items, s.fetchedAt
			s.mu.Unlock()
			return items, fetched, nil
		}
		if time.Now().Before(s.retryAt) {
			err := s.err
			s.mu.Unlock()
			return nil, time.Time{}, err
		}
		if pending := s.inflight; pending != nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, time.Time{}, ctx.Err()
			case <-pending:
				// 原擁有者取消不代表此請求取消，重新檢查快取、失敗節流及唯一抓取槽位。
				continue
			}
		}
		s.inflight = make(chan struct{})
		s.mu.Unlock()

		items, err := s.fetch(ctx)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.err = err
		if err == nil {
			s.items, s.fetchedAt = items, time.Now().UTC()
			s.retryAt = time.Time{}
		} else if ctx.Err() == nil {
			// 短暫保護官方服務，避免故障時由每個瀏覽器輪流立即重試。
			s.retryAt = time.Now().Add(5 * time.Second)
		}
		close(s.inflight)
		s.inflight = nil
		if err != nil {
			return nil, time.Time{}, err
		}
		return s.items, s.fetchedAt, nil
	}
}

// fetch 僅能下載固定 HTTPS 資源，禁止重新導向，並限制整個傳輸及解碼的本文大小。
func (s *taichungService) fetch(ctx context.Context) ([]taichungSensor, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, taichungSourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("prepare Taichung request: %w", err)
	}
	client := &http.Client{
		Transport: s.transport, Timeout: 6 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch Taichung source: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Taichung source: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > taichungBodyLimit {
		return nil, errors.New("Taichung source: body too large")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, taichungBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read Taichung source: %w", err)
	}
	if len(body) > taichungBodyLimit {
		return nil, errors.New("Taichung source: body too large")
	}
	return parseTaichungSensors(body)
}

// selectTaichungSensors 在共用快照外建立回應切片，固定 ID 同步只保留仍存在的設備。
func selectTaichungSensors(items []taichungSensor, ids []string) []taichungSensor {
	selected := make([]taichungSensor, 0, 6)
	if len(ids) > 0 {
		byID := make(map[string]taichungSensor, len(items))
		for _, item := range items {
			byID[item.ID] = item
		}
		for _, id := range ids {
			if item, found := byID[id]; found {
				selected = append(selected, item)
			}
		}
		return selected
	}
	for _, index := range rand.Perm(len(items))[:min(6, len(items))] {
		selected = append(selected, items[index])
	}
	return selected
}

// parseTaichungIDs 限定單一 ids 查詢，避免重複、空值或不明參數改變抽樣語意。
func parseTaichungIDs(raw string) ([]string, bool) {
	query, err := url.ParseQuery(raw)
	if err != nil || len(query) > 1 {
		return nil, false
	}
	if len(query) == 0 {
		return nil, true
	}
	values := query["ids"]
	if len(values) != 1 {
		return nil, false
	}
	ids := strings.Split(values[0], ",")
	if len(ids) > 6 {
		return nil, false
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !validTaichungID(id) || seen[id] {
			return nil, false
		}
		seen[id] = true
	}
	return ids, true
}

// taichungSensors 先確認既有 session 才碰官方來源，公開觀測不改變本機場域授權或持久狀態。
func (a *application) taichungSensors(c *gin.Context) {
	if !emptyBody(c) {
		return
	}
	ids, ok := parseTaichungIDs(c.Request.URL.RawQuery)
	if !ok {
		reject(c, http.StatusBadRequest, "invalid_input")
		return
	}
	if _, ok := a.authenticated(c); !ok {
		return
	}
	items, fetchedAt, err := a.taichung.snapshot(c.Request.Context())
	if err != nil {
		unavailable(c)
		return
	}
	c.JSON(http.StatusOK, taichungPage{Items: selectTaichungSensors(items, ids), Total: len(items), FetchedAt: fetchedAt, SourceURL: taichungSourceURL})
}
